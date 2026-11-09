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
var version = "2.4.0"

// botCommands is the short command menu every user sees.
var botCommands = []struct{ name, zh, en string }{
	{"menu", "打开主菜单", "Open the main menu"},
	{"new", "新建对话", "Start a new conversation"},
	{"stop", "停止当前任务", "Stop the running task"},
	{"show", "显示设置", "Display settings"},
	{"notice", "查看公告", "Announcements"},
	{"about", "支持作者", "Support the author"},
	{"help", "使用帮助", "Help"},
}

// adminCommands are added to the menu only in the owner's and admins' chats.
// Everything else is managed in the server terminal (tg-agent admin).
var adminCommands = []struct{ name, zh, en string }{
	{"settings", "设置：模型、执行模式、推理", "Settings: model, mode, reasoning"},
	{"usage", "用量与余额", "Usage and balance"},
}

func commandList(admin bool, lang string) []map[string]string {
	list := botCommands
	if admin {
		list = append(list[:len(list):len(list)], adminCommands...)
	}
	var cmds []map[string]string
	for _, c := range list {
		desc := c.en
		if lang == langZH {
			desc = c.zh
		}
		cmds = append(cmds, map[string]string{"command": c.name, "description": desc})
	}
	return cmds
}

// commandVariants registers English as the default and Chinese for zh clients.
var commandVariants = []struct{ code, lang string }{{"", langEN}, {"zh", langZH}}

func registerCommands() {
	for _, variant := range commandVariants {
		payload := map[string]interface{}{"commands": commandList(false, variant.lang)}
		if variant.code != "" {
			payload["language_code"] = variant.code
		}
		tgCall("setMyCommands", payload)
	}
	tgCall("setChatMenuButton", map[string]interface{}{"menu_button": map[string]interface{}{"type": "commands"}})
	c := configSnapshot()
	for _, id := range append([]int64{c.OwnerID}, c.Admins...) {
		if id > 0 {
			registerChatCommands(id, true)
		}
	}
}

// registerChatCommands gives one admin chat the admin menu, or removes it so
// the chat falls back to the default list.
func registerChatCommands(chatID int64, admin bool) {
	scope := map[string]interface{}{"type": "chat", "chat_id": chatID}
	for _, variant := range commandVariants {
		payload := map[string]interface{}{"scope": scope}
		if variant.code != "" {
			payload["language_code"] = variant.code
		}
		if admin {
			payload["commands"] = commandList(true, variant.lang)
			tgCall("setMyCommands", payload)
		} else {
			tgCall("deleteMyCommands", payload)
		}
	}
}

func main() { os.Exit(runMain()) }

func runMain() int {
	check := flag.Bool("check", false, "只验证配置，不连接 Telegram 或模型服务 / validate config only")
	showVersion := flag.Bool("version", false, "显示版本 / print version")
	flag.StringVar(&configFile, "config", "", "配置文件路径，默认查找当前目录或程序目录的 config.json")
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintln(out, "用法 / usage:")
		fmt.Fprintln(out, "  tg-agent [--config config.json]            启动机器人 / run the bot")
		fmt.Fprintln(out, "  tg-agent [--config config.json] admin      服务器终端管理 / terminal admin")
		fmt.Fprintln(out, "  tg-agent [--config config.json] admin help 管理命令说明 / admin commands")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("tg-agent " + version)
		return 0
	}
	adminMode := flag.NArg() > 0 && isAdminSubcommand(flag.Arg(0))
	if flag.NArg() > 0 && !adminMode {
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
	if adminMode {
		loadCron()
		return runAdminCLI(flag.Args()[1:], os.Stdin, os.Stdout, os.Stderr)
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
	stopAdmin := startAdminSocket(telegramContext) // tg-agent admin talks to the live bot here
	defer stopAdmin()
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
