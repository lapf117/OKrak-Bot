package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://api.telegram.org/bot"

var (
	httpClient      = &http.Client{Timeout: 120 * time.Second}
	stopFlags       sync.Map // compatibility: legacy stop handlers
	lastTasks       sync.Map // chatID -> string
	busy            sync.Map // chatID -> bool
	uploadsDir      string
	telegramContext = context.Background()
)

// ---------------- 基础 API ----------------

type tgResp struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

func tgCall(method string, payload interface{}) json.RawMessage {
	return tgCallContext(telegramContext, method, payload)
}

func tgCallContext(ctx context.Context, method string, payload interface{}) json.RawMessage {
	cfg := configSnapshot()
	body, _ := json.Marshal(payload)
	url := apiBase + cfg.Token + "/" + method
	var lastErr string
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			lastErr = err.Error()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = redactSecrets(err.Error())
			if ctx.Err() != nil {
				return nil
			}
			time.Sleep(time.Second)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		var r tgResp
		if json.Unmarshal(raw, &r) == nil && r.OK {
			return r.Result
		}
		lastErr = redactSecrets(string(raw))
		if strings.Contains(lastErr, "message is not modified") {
			return nil
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			logf("TG 鉴权失败: %s", lastErr)
			return nil
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
			return nil
		}
		wait := time.Second
		if resp.StatusCode == 429 {
			var rate struct {
				Parameters struct {
					RetryAfter int `json:"retry_after"`
				} `json:"parameters"`
			}
			if json.Unmarshal(raw, &rate) == nil && rate.Parameters.RetryAfter > 0 {
				wait = time.Duration(rate.Parameters.RetryAfter) * time.Second
				if wait > 30*time.Second {
					wait = 30 * time.Second
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
	logf("TG %s 失败: %s", method, short(lastErr, 200))
	return nil
}

func sendMessage(chatID int64, text string, markup interface{}) int64 {
	return sendMessageContext(telegramContext, chatID, text, markup)
}

func sendMessageContext(ctx context.Context, chatID int64, text string, markup interface{}) int64 {
	text = redactSecrets(text)
	if strings.TrimSpace(text) == "" {
		text = "（空）"
	}
	chunks := splitTelegramText(text, 4000)
	var firstID int64
	for i, chunk := range chunks {
		if ctx.Err() != nil {
			break
		}
		p := map[string]interface{}{"chat_id": chatID, "text": chunk, "disable_web_page_preview": true}
		if markup != nil && i == len(chunks)-1 {
			p["reply_markup"] = markup
		}
		var m struct {
			MessageID int64 `json:"message_id"`
		}
		_ = json.Unmarshal(tgCallContext(ctx, "sendMessage", p), &m)
		if i == 0 {
			firstID = m.MessageID
		}
	}
	return firstID
}

func editMessage(chatID, msgID int64, text string, markup interface{}) bool {
	return editMessageContext(telegramContext, chatID, msgID, text, markup)
}

func editMessageContext(ctx context.Context, chatID, msgID int64, text string, markup interface{}) bool {
	text = redactSecrets(text)
	p := map[string]interface{}{
		"chat_id":                  chatID,
		"message_id":               msgID,
		"text":                     truncateTelegramText(text, 4000),
		"disable_web_page_preview": true,
	}
	if markup != nil {
		p["reply_markup"] = markup
	}
	return len(tgCallContext(ctx, "editMessageText", p)) > 0
}

func answerCallback(id string) {
	tgCall("answerCallbackQuery", map[string]interface{}{"callback_query_id": id})
}

func sendTyping(chatID int64) {
	tgCall("sendChatAction", map[string]interface{}{"chat_id": chatID, "action": "typing"})
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---------------- 媒体 ----------------

func tgFilePath(fileID string) string {
	raw := tgCall("getFile", map[string]interface{}{"file_id": fileID})
	if raw == nil {
		return ""
	}
	var r struct {
		FilePath string `json:"file_path"`
	}
	json.Unmarshal(raw, &r)
	return r.FilePath
}

func downloadTGFile(p string) ([]byte, error) {
	cfg := configSnapshot()
	if p == "" {
		return nil, fmt.Errorf("没有 file_path")
	}
	url := "https://api.telegram.org/file/bot" + cfg.Token + "/" + p
	req, err := http.NewRequestWithContext(telegramContext, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024+1))
	if len(data) > 20*1024*1024 {
		return nil, fmt.Errorf("下载文件超过 20MB")
	}
	return data, err
}

func sendMedia(chatID int64, endpoint, field string, data []byte, name, caption string) bool {
	cfg := configSnapshot()
	if len(data) == 0 {
		return false
	}
	if len(data) > 48*1024*1024 {
		sendMessage(chatID, fmt.Sprintf("文件太大（%.1fMB），上限 50MB", float64(len(data))/1048576), nil)
		return false
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if caption != "" {
		w.WriteField("caption", caption)
	}
	fw, err := w.CreateFormFile(field, name)
	if err != nil {
		return false
	}
	fw.Write(data)
	w.Close()

	req, err := http.NewRequestWithContext(telegramContext, "POST", apiBase+cfg.Token+"/"+endpoint, &buf)
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := httpClient.Do(req)
	if err != nil {
		logf("%s 失败: %s", endpoint, redactSecrets(err.Error()))
		return false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r tgResp
	json.Unmarshal(raw, &r)
	if !r.OK {
		logf("%s 拒绝: %s", endpoint, r.Description)
	}
	return r.OK
}

var imgExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".bmp": true}
var vidExt = map[string]bool{".mp4": true, ".mov": true, ".mkv": true, ".webm": true}

func sendByExt(chatID int64, path string, data []byte) bool {
	ext := strings.ToLower(filepath.Ext(path))
	name := filepath.Base(path)
	switch {
	case imgExt[ext]:
		return sendMedia(chatID, "sendPhoto", "photo", data, name, name)
	case vidExt[ext]:
		return sendMedia(chatID, "sendVideo", "video", data, name, name)
	default:
		return sendMedia(chatID, "sendDocument", "document", data, name, name)
	}
}

// ---------------- 类型 ----------------

type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type PhotoSize struct {
	FileID string `json:"file_id"`
}

type Video struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
}

type Voice struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type"`
	Duration int    `json:"duration"`
}

type Audio struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	Duration int    `json:"duration"`
}

type Message struct {
	MessageID int64       `json:"message_id"`
	From      User        `json:"from"`
	Chat      Chat        `json:"chat"`
	Text      string      `json:"text"`
	Caption   string      `json:"caption"`
	Photo     []PhotoSize `json:"photo"`
	Video     *Video      `json:"video"`
	Document  *Document   `json:"document"`
	Voice     *Voice      `json:"voice"`
	Audio     *Audio      `json:"audio"`
	Animation *Document   `json:"animation"`
	VideoNote *Video      `json:"video_note"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

func displayName(u User) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return u.FirstName
}

// ---------------- 键盘 ----------------

type Btn map[string]string

func keyboard(rows [][]Btn) map[string]interface{} {
	return map[string]interface{}{"inline_keyboard": rows}
}

func btn(text, data string) Btn { return Btn{"text": text, "callback_data": data} }

func mark(cur, val string) string {
	if cur == val {
		return "● " + val
	}
	return val
}

func mainMenu(uid int64) map[string]interface{} {
	rows := [][]Btn{
		{btn("新对话", "act:new"), btn("停止任务", "act:stop")},
		{btn("技能", "nav:skills"), btn("帮助", "act:help")},
	}
	if isAdmin(uid) {
		rows = append(rows, []Btn{btn("执行记录", "act:runs"), btn("设置", "nav:settings")})
	}
	return keyboard(rows)
}

func useMenu(uid int64) map[string]interface{} { return mainMenu(uid) }

func adminMenu(uid int64) map[string]interface{} {
	if !isAdmin(uid) {
		return mainMenu(uid)
	}
	return keyboard([][]Btn{
		{btn("模型", "nav:model"), btn("执行模式", "nav:execution")},
		{btn("工作区", "nav:workspace"), btn("推理档位", "nav:effort")},
		{btn("定时任务", "act:cron"), btn("快捷指令", "act:quick")},
		{btn("高级设置", "nav:advanced"), btn("返回", "nav:root")},
	})
}

func advancedMenu(uid int64) map[string]interface{} {
	if !isAdmin(uid) {
		return mainMenu(uid)
	}
	return keyboard([][]Btn{
		{btn("服务商", "nav:provider"), btn("系统提示词", "nav:system")},
		{btn("人设", "nav:persona"), btn("用户管理", "nav:users")},
		{btn("自检", "act:health"), btn("运行日志", "act:log")},
		{btn("配置总览", "act:config"), btn("返回", "nav:settings")},
	})
}

func menuText(uid int64) string {
	if !allowed(uid) {
		return "大肥鱼 Agent\n请先联系主人授权。"
	}
	cfg := configSnapshot()
	state := "就绪"
	return fmt.Sprintf("大肥鱼 Agent · %s\n模型 %s\n\n直接发送任务，或选择下面的入口。", state, cfg.Model)
}

// ---------------- 执行任务 ----------------

type traceItem struct {
	Step   int
	Tool   string
	Detail string
	Output string
	Done   bool
}

func runUserTask(chatID, uid int64, text string, images []string) {
	if chatID <= 0 || chatID != uid || !allowed(uid) {
		return
	}
	ctx, cleanup, ok := beginTask(chatID)
	if !ok {
		sendMessage(chatID, "上一个任务还在跑，先 /stop 或等它结束。", nil)
		return
	}
	defer cleanup()
	cfg := executionConfig(ctx)
	busy.Store(chatID, true)
	defer busy.Delete(chatID)

	lastTasks.Store(chatID, text)
	msgID := sendMessageContext(ctx, chatID, "正在处理任务…", keyboard([][]Btn{{btn("停止任务", "act:stop")}}))

	stop := make(chan struct{})
	if cfg.Typing {
		go func() {
			t := time.NewTicker(4 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					tgCallContext(ctx, "sendChatAction", map[string]interface{}{"chat_id": chatID, "action": "typing"})
				}
			}
		}()
	}
	defer close(stop)

	var (
		mu       sync.Mutex
		trace    []traceItem
		lastEdit time.Time
		lastBody string
	)

	render := func(force bool) {
		if !cfg.ShowSteps {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if !force && time.Since(lastEdit) < 1800*time.Millisecond {
			return
		}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("执行中 · 第 %d 步\n", len(trace)))
		b.WriteString(strings.Repeat("─", 16) + "\n")
		start := 0
		if len(trace) > 4 {
			start = len(trace) - 4
			if start > 0 {
				b.WriteString(fmt.Sprintf("（省略前面 %d 步）\n", start))
			}
		}
		for _, t := range trace[start:] {
			if t.Done {
				b.WriteString(fmt.Sprintf("✓ %s  %s\n", t.Tool, t.Detail))
				if strings.TrimSpace(t.Output) != "" {
					for _, line := range strings.Split(short(t.Output, 420), "\n") {
						b.WriteString("   " + line + "\n")
					}
				}
			} else {
				b.WriteString(fmt.Sprintf("▶ %s  %s\n", t.Tool, t.Detail))
			}
		}
		body := truncRunes(b.String(), 3800)
		if body == lastBody {
			return
		}
		lastBody = body
		editMessageContext(ctx, chatID, msgID, redactExecutionSecrets(ctx, body), keyboard([][]Btn{{btn("停止任务", "act:stop")}}))
		lastEdit = time.Now()
	}

	onStep := func(ev StepEvent) {
		if ev.Final {
			return
		}
		mu.Lock()
		if ev.Done {
			if n := len(trace); n > 0 && trace[n-1].Step == ev.Step && !trace[n-1].Done {
				trace[n-1].Output = ev.Output
				trace[n-1].Done = true
			}
		} else {
			trace = append(trace, traceItem{Step: ev.Step, Tool: ev.Tool, Detail: ev.Detail})
		}
		mu.Unlock()
		render(ev.Done)
	}

	start := time.Now()
	out, err := runAgentContext(ctx, chatID, uid, text, images, 0, onStep)
	dur := time.Since(start)

	mu.Lock()
	steps := len(trace)
	traceCopy := append([]traceItem(nil), trace...)
	mu.Unlock()

	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		writeRunLog(chatID, redactExecutionSecrets(ctx, text), traceCopy, "任务已停止。", dur)
		editMessage(chatID, msgID, "任务已停止。", mainMenu(uid))
		return
	}
	if err != nil {
		writeRunLog(chatID, redactExecutionSecrets(ctx, text), traceCopy, "执行失败："+redactExecutionSecrets(ctx, err.Error()), dur)
		editMessage(chatID, msgID, "执行失败："+redactExecutionSecrets(ctx, err.Error()), mainMenu(uid))
		return
	}
	out = strings.TrimSpace(redactExecutionSecrets(ctx, out))
	if out == "" {
		out = "（没有输出）"
	}
	runPath := writeRunLog(chatID, redactExecutionSecrets(ctx, text), traceCopy, out, dur)

	footer := fmt.Sprintf("\n\n%d 步 · %.1fs", steps, dur.Seconds())
	if runPath != "" {
		footer += " · 记录已保存"
	}
	full := out + footer

	chunks := splitTelegramText(full, 4000)
	if !editMessage(chatID, msgID, chunks[0], mainMenu(uid)) && msgID == 0 {
		sendMessage(chatID, chunks[0], mainMenu(uid))
	}
	for _, chunk := range chunks[1:] {
		sendMessage(chatID, chunk, nil)
	}
	if isAdmin(uid) {
		go sendMediaInText(chatID, out)
	}
}

// writeRunLog 把完整执行过程写成 markdown
func writeRunLog(chatID int64, task string, trace []traceItem, final string, dur time.Duration) string {
	dir := filepath.Join(baseDir, "runs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ""
	}
	p := filepath.Join(dir, fmt.Sprintf("%s-%d.md", time.Now().Format("20060102-150405.000000000"), chatID))
	var b strings.Builder
	b.WriteString("# 执行记录\n\n")
	b.WriteString(fmt.Sprintf("- 时间：%s\n", time.Now().Format("2006-01-02 15:04:05")))
	b.WriteString(fmt.Sprintf("- 耗时：%.1fs\n", dur.Seconds()))
	b.WriteString(fmt.Sprintf("- 步数：%d\n\n", len(trace)))
	b.WriteString("## 任务\n\n" + task + "\n\n")
	for _, t := range trace {
		b.WriteString(fmt.Sprintf("## 第 %d 步 · %s\n\n%s\n\n", t.Step, t.Tool, t.Detail))
		if strings.TrimSpace(t.Output) != "" {
			b.WriteString("```\n" + t.Output + "\n```\n\n")
		}
	}
	b.WriteString("## 最终输出\n\n" + final + "\n")
	if err := os.WriteFile(p, []byte(redactSecrets(b.String())), 0600); err != nil {
		return ""
	}
	return p
}

var mediaRe = regexp.MustCompile(`(?i)(/[A-Za-z0-9._/\-]+\.(?:png|jpe?g|gif|webp|bmp|mp4|mov|mkv|webm))`)

// sendMediaInText 扫描回复里的本地媒体路径并发给用户
func sendMediaInText(chatID int64, text string) {
	seen := map[string]bool{}
	n := 0
	for _, m := range mediaRe.FindAllString(text, -1) {
		if seen[m] || n >= 3 {
			continue
		}
		seen[m] = true
		data, err := readDeliverable(m)
		if err != nil || len(data) == 0 {
			continue
		}
		if sendByExt(chatID, m, data) {
			n++
		}
	}
}

// Telegram counts UTF-16 code units; astral characters consume two units.
func splitTelegramText(text string, limit int) []string {
	if limit < 2 {
		limit = 2
	}
	if text == "" {
		return []string{""}
	}
	var chunks []string
	start, units := 0, 0
	for index, r := range text {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit {
			chunks = append(chunks, text[start:index])
			start = index
			units = 0
		}
		units += width
	}
	return append(chunks, text[start:])
}

func truncateTelegramText(text string, limit int) string { return splitTelegramText(text, limit)[0] }

// Resolve symlinks before checking roots so a generated path cannot expose host files.
func readDeliverable(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("请使用绝对路径")
	}
	full, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	full, err = filepath.Abs(full)
	if err != nil {
		return nil, err
	}
	cfg := configSnapshot()
	trusted := false
	for _, root := range []string{cfg.Workspace, uploadsDir, filepath.Join(baseDir, "runs")} {
		if root == "" {
			continue
		}
		realRoot, e := filepath.EvalSymlinks(root)
		if e != nil {
			continue
		}
		rel, e := filepath.Rel(realRoot, full)
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			trusted = true
			break
		}
	}
	if !trusted {
		return nil, fmt.Errorf("只允许发送工作区、上传目录或执行记录中的文件")
	}
	for _, part := range strings.Split(filepath.ToSlash(full), "/") {
		lower := strings.ToLower(part)
		if lower == ".ssh" || lower == ".aws" || lower == ".gnupg" || lower == ".config" || lower == ".git" || lower == "config.json" || lower == "agent.log" || lower == ".env" || strings.HasPrefix(lower, ".env.") || strings.HasSuffix(lower, ".key") || strings.HasSuffix(lower, ".pem") || lower == "credentials" {
			return nil, fmt.Errorf("该路径包含私密配置，不能通过聊天发送")
		}
	}
	configFull, _ := filepath.Abs(configPath())
	if full == configFull {
		return nil, fmt.Errorf("不能发送服务配置")
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("只支持普通文件")
	}
	if info.Size() > 48*1024*1024 {
		return nil, fmt.Errorf("文件超过发送上限 48MB")
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 48*1024*1024+1))
	if len(data) > 48*1024*1024 {
		return nil, fmt.Errorf("文件超过发送上限 48MB")
	}
	return data, err
}
