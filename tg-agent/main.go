package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var configFile string
var version = "2.1.0"

// botCommands is the Telegram command menu, registered in both languages.
var botCommands = []struct{ name, zh, en string }{
	{"menu", "打开任务面板", "Open the task panel"},
	{"new", "新建对话", "Start a new conversation"},
	{"stop", "停止当前任务", "Stop the running task"},
	{"status", "运行状态", "Show status"},
	{"memory", "长期备忘", "Long-term memory"},
	{"skills", "技能中心", "Skills"},
	{"lang", "切换语言 中文/English", "Switch language"},
	{"settings", "设置与自动化", "Settings and automation"},
	{"help", "使用帮助", "Help"},
}

func registerCommands() {
	for _, variant := range []struct{ code, lang string }{{"", langEN}, {"zh", langZH}} {
		var cmds []map[string]string
		for _, c := range botCommands {
			desc := c.en
			if variant.lang == langZH {
				desc = c.zh
			}
			cmds = append(cmds, map[string]string{"command": c.name, "description": desc})
		}
		payload := map[string]interface{}{"commands": cmds}
		if variant.code != "" {
			payload["language_code"] = variant.code
		}
		tgCall("setMyCommands", payload)
	}
	tgCall("setChatMenuButton", map[string]interface{}{"menu_button": map[string]interface{}{"type": "commands"}})
}

func main() { os.Exit(runMain()) }

func runMain() int {
	check := flag.Bool("check", false, "只验证配置，不连接 Telegram 或模型服务 / validate config only")
	showVersion := flag.Bool("version", false, "显示版本 / print version")
	flag.StringVar(&configFile, "config", "", "配置文件路径，默认查找当前目录或程序目录的 config.json")
	flag.Parse()
	if *showVersion {
		fmt.Println("tg-agent " + version)
		return 0
	}
	if flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "未知参数；使用 --help 查看帮助")
		return 2
	}
	if configFile != "" {
		absolute, err := filepath.Abs(configFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		configFile = absolute
		baseDir = filepath.Dir(absolute)
	} else {
		if exe, err := os.Executable(); err == nil {
			baseDir = filepath.Dir(exe)
		}
		if wd, err := os.Getwd(); err == nil && fileExists(filepath.Join(wd, "config.json")) {
			baseDir = wd
		}
	}
	if err := loadConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败: %v\n配置路径: %s\n", err, configPath())
		return 1
	}
	if err := mustConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *check {
		fmt.Println("配置验证通过。未连接外部服务。")
		return 0
	}
	openLog()
	defer func() {
		if logFile != nil {
			logFile.Close()
		}
	}()
	uploadsDir = filepath.Join(baseDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0700); err != nil {
		logf("创建上传目录失败: %v", err)
		return 1
	}
	var cancel context.CancelFunc
	telegramContext, cancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() { <-telegramContext.Done(); stopAllTasks() }()
	c := configSnapshot()
	logf("启动 tg-agent %s | 主人 %d | 模型 %s | 后端 %s | 模式 %s", version, c.OwnerID, c.Model, c.Backend, approvalMode(c))
	if sk, err := loadSkillIndex(); err == nil {
		logf("发现技能 %d 个", len(sk))
	}
	var me struct {
		Username string `json:"username"`
		ID       int64  `json:"id"`
	}
	raw := tgCall("getMe", map[string]interface{}{})
	if raw == nil {
		if telegramContext.Err() == nil {
			logf("连接 Telegram 失败，请检查 token 与网络")
			return 1
		}
		return 0
	}
	if err := json.Unmarshal(raw, &me); err != nil || me.ID == 0 {
		logf("Telegram 返回无效机器人信息")
		return 1
	}
	logf("@%s 已上线", me.Username)
	registerCommands()
	loadCron()
	startCron()
	pollUpdates(newUpdateDispatcher(16, 8, handleMessage, handleCallback))
	logf("已退出")
	return 0
}

// pollUpdates long-polls Telegram until shutdown, then drains handlers.
func pollUpdates(dispatcher *updateDispatcher) {
	offset := loadUpdateOffset()
	for telegramContext.Err() == nil {
		params := map[string]interface{}{"timeout": 50, "allowed_updates": []string{"message", "callback_query"}}
		if offset > 0 {
			params["offset"] = offset
		}
		raw := tgCall("getUpdates", params)
		if raw == nil {
			if waitContext(telegramContext, 3*time.Second) != nil {
				break
			}
			continue
		}
		var updates []Update
		if err := json.Unmarshal(raw, &updates); err != nil {
			logf("Telegram 更新解析失败: %v", err)
			if waitContext(telegramContext, time.Second) != nil {
				break
			}
			continue
		}
		for _, u := range updates {
			if telegramContext.Err() != nil {
				break
			}
			offset = u.UpdateID + 1
			if !dispatcher.dispatch(u) {
				dispatcher.reject(u)
			}
			saveUpdateOffset(offset)
		}
	}
	stopAllTasks()
	drained := make(chan struct{})
	go func() { dispatcher.wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		logf("等待任务退出超时")
	}
}

func loadUpdateOffset() int64 {
	data, err := os.ReadFile(filepath.Join(baseDir, "updates.offset"))
	if err != nil {
		return 0
	}
	value, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return max(value, 0)
}

func saveUpdateOffset(offset int64) {
	path := filepath.Join(baseDir, "updates.offset")
	if err := os.WriteFile(path+".tmp", []byte(strconv.FormatInt(offset, 10)), 0600); err == nil {
		if err := os.Rename(path+".tmp", path); err != nil {
			logf("保存更新游标失败: %v", err)
		}
	}
}
