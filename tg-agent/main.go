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
	"sync"
	"syscall"
	"time"
)

var configFile string
var version = "2.0.0"

func registerCommands() {
	cmds := []map[string]string{
		{"command": "menu", "description": "打开任务面板"},
		{"command": "new", "description": "新建对话"},
		{"command": "stop", "description": "停止当前任务"},
		{"command": "skills", "description": "技能中心"},
		{"command": "settings", "description": "设置与自动化"},
		{"command": "help", "description": "使用帮助"},
	}
	tgCall("setMyCommands", map[string]interface{}{"commands": cmds})
	tgCall("setChatMenuButton", map[string]interface{}{"menu_button": map[string]interface{}{"type": "commands"}})
}

func main() { os.Exit(runMain()) }

func runMain() int {
	check := flag.Bool("check", false, "只验证配置，不连接 Telegram 或模型服务")
	showVersion := flag.Bool("version", false, "显示版本")
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
	logf("启动 tg-agent %s | 主人 %d | 模型 %s | 后端 %s", version, c.OwnerID, c.Model, c.Backend)
	if sk, err := loadSkillIndex(); err == nil {
		logf("发现技能 %d 个", len(sk))
	}
	var me struct {
		Username string `json:"username"`
		ID       int64  `json:"id"`
	}
	if raw := tgCall("getMe", map[string]interface{}{}); raw != nil {
		if err := json.Unmarshal(raw, &me); err != nil || me.ID == 0 {
			logf("Telegram 返回无效机器人信息")
			return 1
		}
		logf("@%s 已上线", me.Username)
	} else {
		if telegramContext.Err() == nil {
			logf("连接 Telegram 失败，请检查 token 与网络")
			return 1
		}
		return 0
	}
	registerCommands()
	loadCron()
	startCron()
	offset := loadUpdateOffset()
	dispatcher := newUpdateDispatcher(16, 8, handleMessage, handleCallback)
	for telegramContext.Err() == nil {
		params := map[string]interface{}{"timeout": 50, "allowed_updates": []string{"message", "callback_query"}}
		if offset > 0 {
			params["offset"] = offset
		}
		raw := tgCall("getUpdates", params)
		if raw == nil {
			if !waitTelegram(3 * time.Second) {
				break
			}
			continue
		}
		var updates []Update
		if err := json.Unmarshal(raw, &updates); err != nil {
			logf("Telegram 更新解析失败: %v", err)
			if !waitTelegram(time.Second) {
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
	logf("已退出")
	return 0
}

func waitTelegram(delay time.Duration) bool {
	select {
	case <-telegramContext.Done():
		return false
	case <-time.After(delay):
		return true
	}
}
func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
func loadUpdateOffset() int64 {
	data, err := os.ReadFile(filepath.Join(baseDir, "updates.offset"))
	if err != nil {
		return 0
	}
	value, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if value < 0 {
		return 0
	}
	return value
}
func saveUpdateOffset(offset int64) {
	path := filepath.Join(baseDir, "updates.offset")
	if err := os.WriteFile(path+".tmp", []byte(strconv.FormatInt(offset, 10)), 0600); err == nil {
		if err := os.Rename(path+".tmp", path); err != nil {
			logf("保存更新游标失败: %v", err)
		}
	}
}

// Ordinary work can wait for approvals for minutes. Its admission never blocks
// polling, and a separate bounded lane keeps approvals and stop controls usable.
type updateDispatcher struct {
	ordinary   chan struct{}
	control    chan struct{}
	notices    chan struct{}
	handlers   sync.WaitGroup
	onMessage  func(Message)
	onCallback func(CallbackQuery)
}

func newUpdateDispatcher(ordinaryLimit, controlLimit int, onMessage func(Message), onCallback func(CallbackQuery)) *updateDispatcher {
	if ordinaryLimit < 1 {
		ordinaryLimit = 1
	}
	if controlLimit < 1 {
		controlLimit = 1
	}
	return &updateDispatcher{ordinary: make(chan struct{}, ordinaryLimit), control: make(chan struct{}, controlLimit), notices: make(chan struct{}, 4), onMessage: onMessage, onCallback: onCallback}
}

func (d *updateDispatcher) launch(lane chan struct{}, work func()) bool {
	select {
	case lane <- struct{}{}:
		d.handlers.Add(1)
		go func() { defer d.handlers.Done(); defer func() { <-lane }(); work() }()
		return true
	default:
		return false
	}
}

func privateControlAllowed(chat Chat, uid int64) bool {
	return chat.ID > 0 && chat.ID == uid && (chat.Type == "" || chat.Type == "private") && allowed(uid)
}

func stopMessage(m Message) bool {
	text := m.Text
	if strings.TrimSpace(text) == "" {
		text = m.Caption
	}
	fields := strings.Fields(normalizeBotCommand(strings.TrimSpace(text)))
	return len(fields) > 0 && strings.EqualFold(fields[0], "/stop")
}

func (d *updateDispatcher) dispatch(update Update) bool {
	if update.CallbackQuery != nil {
		callback := *update.CallbackQuery
		lane := d.ordinary
		if callback.Message != nil && privateControlAllowed(callback.Message.Chat, callback.From.ID) {
			if callback.Data == "act:stop" {
				// Cancellation is local and must work even if all network handlers are busy.
				stopTask(callback.Message.Chat.ID)
				lane = d.control
			} else if strings.HasPrefix(callback.Data, "approve:") || strings.HasPrefix(callback.Data, "reject:") {
				lane = d.control
			}
		}
		return d.launch(lane, func() { d.onCallback(callback) })
	}
	if update.Message != nil {
		message := *update.Message
		lane := d.ordinary
		if stopMessage(message) && privateControlAllowed(message.Chat, message.From.ID) {
			stopTask(message.Chat.ID)
			lane = d.control
		}
		return d.launch(lane, func() { d.onMessage(message) })
	}
	return true
}

func (d *updateDispatcher) reject(update Update) {
	// Overload notifications are bounded too; a slow Telegram connection cannot
	// allocate one goroutine per rejected update or stall the polling loop.
	d.launch(d.notices, func() {
		ctx, cancel := context.WithTimeout(telegramContext, 3*time.Second)
		defer cancel()
		if update.CallbackQuery != nil {
			tgCallContext(ctx, "answerCallbackQuery", map[string]interface{}{"callback_query_id": update.CallbackQuery.ID, "text": "系统繁忙，请稍后重试；停止请求已优先处理。"})
		} else if update.Message != nil {
			m := update.Message
			if privateControlAllowed(m.Chat, m.From.ID) {
				sendMessageContext(ctx, m.Chat.ID, "当前并发任务已满，请稍后重试。/stop 仍可停止当前任务。", nil)
			}
		}
	})
}

func (d *updateDispatcher) wait() { d.handlers.Wait() }
