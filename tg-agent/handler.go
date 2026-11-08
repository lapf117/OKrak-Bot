package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	uiMu   sync.Mutex
	seenMu sync.Mutex
	seen   = map[int64]SeenUser{}
)

type SeenUser struct {
	Name string
	Last int64
}

func touchUser(uid int64, name string) {
	seenMu.Lock()
	seen[uid] = SeenUser{Name: name, Last: time.Now().Unix()}
	seenMu.Unlock()
}

func userName(uid int64) string {
	seenMu.Lock()
	defer seenMu.Unlock()
	if s, ok := seen[uid]; ok && s.Name != "" {
		return s.Name
	}
	return "未知"
}

func lastActive(uid int64) string {
	seenMu.Lock()
	defer seenMu.Unlock()
	if s, ok := seen[uid]; ok && s.Last > 0 {
		return time.Unix(s.Last, 0).Format("01-02 15:04")
	}
	return "从未"
}

// ---------------- 媒体入库 ----------------

var unsafeName = regexp.MustCompile(`[^\w.\-]`)

type mediaInfo struct {
	Path string
	Kind string // image / video / audio / file
}

func storeIncoming(m Message) mediaInfo {
	var fileID, name, kind string
	switch {
	case len(m.Photo) > 0:
		fileID = m.Photo[len(m.Photo)-1].FileID
		name = fmt.Sprintf("photo_%d.jpg", time.Now().Unix())
		kind = "image"
	case m.Video != nil:
		fileID = m.Video.FileID
		name = m.Video.FileName
		kind = "video"
		if name == "" {
			name = fmt.Sprintf("video_%d.mp4", time.Now().Unix())
		}
	case m.VideoNote != nil:
		fileID = m.VideoNote.FileID
		name = fmt.Sprintf("note_%d.mp4", time.Now().Unix())
		kind = "video"
	case m.Animation != nil:
		fileID = m.Animation.FileID
		name = m.Animation.FileName
		kind = "video"
		if name == "" {
			name = fmt.Sprintf("anim_%d.mp4", time.Now().Unix())
		}
	case m.Voice != nil:
		fileID = m.Voice.FileID
		name = fmt.Sprintf("voice_%d.ogg", time.Now().Unix())
		kind = "audio"
	case m.Audio != nil:
		fileID = m.Audio.FileID
		name = m.Audio.FileName
		kind = "audio"
		if name == "" {
			name = fmt.Sprintf("audio_%d.mp3", time.Now().Unix())
		}
	case m.Document != nil:
		fileID = m.Document.FileID
		name = m.Document.FileName
		kind = "file"
		if name == "" {
			name = fmt.Sprintf("file_%d.bin", time.Now().Unix())
		}
		if strings.HasPrefix(m.Document.MimeType, "image/") {
			kind = "image"
		} else if strings.HasPrefix(m.Document.MimeType, "audio/") {
			kind = "audio"
		} else if strings.HasPrefix(m.Document.MimeType, "video/") {
			kind = "video"
		}
	}
	if fileID == "" {
		return mediaInfo{}
	}
	name = unsafeName.ReplaceAllString(filepath.Base(name), "_")
	name = fmt.Sprintf("%d_%d_%s", m.From.ID, time.Now().UnixNano(), name)
	fp := tgFilePath(fileID)
	data, err := downloadTGFile(fp)
	if err != nil {
		logf("下载失败: %v", err)
		return mediaInfo{}
	}
	os.MkdirAll(uploadsDir, 0700)
	full := filepathJoin(uploadsDir, name)
	if err := os.WriteFile(full, data, 0600); err != nil {
		logf("保存失败: %v", err)
		return mediaInfo{}
	}
	logf("收到 %s %s (%d B)", kind, full, len(data))
	return mediaInfo{Path: full, Kind: kind}
}

// transcribe 调 ASR 把语音转文字（需在配置里填 asr）
func transcribe(path string) (string, error) {
	cfg := configSnapshot()
	if cfg.ASR.BaseURL == "" || cfg.ASR.APIKey == "" {
		return "", fmt.Errorf("未配置语音识别")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	fw.Write(data)
	w.WriteField("model", cfg.ASR.Model)
	w.Close()

	url := strings.TrimRight(cfg.ASR.BaseURL, "/") + "/audio/transcriptions"
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+cfg.ASR.APIKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("ASR HTTP %d: %s", resp.StatusCode, short(string(raw), 120))
	}
	var r struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	return r.Text, nil
}

func filepathJoin(a, b string) string {
	if strings.HasSuffix(a, "/") {
		return a + b
	}
	return a + "/" + b
}

// ---------------- 消息入口 ----------------

func handleMessage(m Message) {
	chatID := m.Chat.ID
	uid := m.From.ID
	if (m.Chat.Type != "" && m.Chat.Type != "private") || chatID <= 0 || chatID != uid {
		sendMessage(chatID, "请在与机器人的私聊中使用 Agent。", nil)
		return
	}
	t := strings.TrimSpace(m.Text)
	if t == "" {
		t = strings.TrimSpace(m.Caption)
	}
	hasMedia := len(m.Photo) > 0 || m.Video != nil || m.Document != nil || m.Voice != nil || m.Audio != nil || m.Animation != nil || m.VideoNote != nil
	if t == "" && !hasMedia {
		return
	}
	touchUser(uid, displayName(m.From))

	t = normalizeBotCommand(t)
	low := strings.ToLower(t)
	if low == "/whoami" || low == "/id" {
		sendMessage(chatID, fmt.Sprintf("user_id: %d\nchat_id: %d\n身份: %s", uid, chatID, roleName(uid)), nil)
		return
	}
	if !allowed(uid) {
		if isBanned(uid) {
			sendMessage(chatID, "你已被封禁。", nil)
		} else {
			sendMessage(chatID, fmt.Sprintf("未授权。你的 id: %d\n请让主人把你加为朋友。", uid), nil)
		}
		return
	}

	if low == "/start" || low == "/help" || low == "/menu" || low == "help" || low == "menu" {
		sendHelp(chatID, uid)
		return
	}

	// 命令
	if strings.HasPrefix(low, "/") {
		if handleCommand(chatID, uid, t, low) {
			return
		}
	}

	// 媒体
	var images []string
	if hasMedia {
		info := storeIncoming(m)
		if info.Path != "" {
			switch info.Kind {
			case "image":
				images = append(images, info.Path)
				if t == "" {
					t = "看看这张图片，说说内容。"
				}
			case "audio":
				if txt, err := transcribe(info.Path); err == nil && strings.TrimSpace(txt) != "" {
					t = fmt.Sprintf("[用户发来语音，转文字如下]\n%s\n%s", txt, t)
					if strings.TrimSpace(t) == "" {
						t = txt
					}
				} else {
					t = fmt.Sprintf("[用户发来语音/音频，已保存到 %s]（未配置语音识别，可用工具自行处理，如 ffmpeg 看时长或转码）\n%s", info.Path, t)
				}
			case "video":
				t = fmt.Sprintf("[用户发来视频，已保存到 %s]（可用 ffmpeg 抽帧、抽音频、看时长）\n%s", info.Path, t)
			default:
				t = fmt.Sprintf("[用户发来文件，已保存到 %s]\n%s", info.Path, t)
			}
		}
	}
	if strings.TrimSpace(t) == "" {
		return
	}
	runUserTask(chatID, uid, t, images)
}

// handleCommand 返回 true 表示已处理
func handleCommand(chatID, uid int64, t, low string) bool {
	fields := strings.Fields(t)
	if len(fields) == 0 {
		return false
	}
	cmd := low
	if len(fields) > 0 {
		cmd = strings.ToLower(fields[0])
	}
	rest := strings.TrimSpace(t[len(fields[0]):])
	if commandMutatesConfig(cmd, rest) {
		uiMu.Lock()
		defer uiMu.Unlock()
	}
	cfg := configSnapshot()

	switch cmd {
	case "/new", "/reset":
		if _, running := busy.Load(chatID); running {
			sendMessage(chatID, "请先停止当前任务，再新建对话。", nil)
			return true
		}
		resetHistory(chatID)
		sendMessage(chatID, "已开新对话。\n\n"+menuText(uid), mainMenu(uid))
		return true
	case "/stop":
		if stopTask(chatID) {
			sendMessage(chatID, "已请求停止。", nil)
		} else {
			sendMessage(chatID, "没有正在跑的任务。", nil)
		}
		return true
	case "/status":
		sendMessage(chatID, menuText(uid), mainMenu(uid))
		return true
	case "/think":
		if !isAdmin(uid) {
			sendMessage(chatID, "只有管理员能查看任务记录。", nil)
			return true
		}
		th := getLastThink(chatID)
		if th == "" {
			sendMessage(chatID, "还没有思考记录。", nil)
		} else {
			sendMessage(chatID, "最近一次思考：\n\n"+th, nil)
		}
		return true
	case "/model":
		if rest == "" {
			var b strings.Builder
			b.WriteString(fmt.Sprintf("当前模型: %s\n\n", cfg.Model))
			p := currentProvider()
			if p != nil {
				b.WriteString("可用:\n")
				for _, m := range p.Models {
					b.WriteString("  " + m + "\n")
				}
			}
			b.WriteString("\n切换: /model <名字>")
			sendMessage(chatID, b.String(), nil)
			return true
		}
		if !isAdmin(uid) {
			sendMessage(chatID, "只有管理员能改模型。", nil)
			return true
		}
		p := currentProvider()
		if p == nil {
			sendMessage(chatID, "没有配置服务商。", nil)
			return true
		}
		for _, m := range p.Models {
			if m == rest {
				cfg.Model = rest
				if !persistUIConfig(chatID, cfg) {
					return true
				}
				sendMessage(chatID, "模型已切换: "+rest, nil)
				return true
			}
		}
		sendMessage(chatID, "没有这个模型。可用: "+strings.Join(p.Models, ", "), nil)
		return true
	case "/provider":
		return handleProvider(chatID, uid, rest)
	case "/persona":
		if !isAdmin(uid) {
			sendMessage(chatID, "只有管理员能改人设。", nil)
			return true
		}
		if rest == "" {
			cur := cfg.Persona
			if strings.TrimSpace(cur) == "" {
				cur = "（未设置，使用内置助手人格）"
			}
			sendMessage(chatID, "当前人设：\n\n"+short(cur, 3200)+"\n\n设置: /persona <文本>\n清除: /persona clear", nil)
			return true
		}
		if strings.EqualFold(rest, "clear") {
			cfg.Persona = ""
			if !persistUIConfig(chatID, cfg) {
				return true
			}
			sendMessage(chatID, "人设已清除。", nil)
			return true
		}
		cfg.Persona = rest
		if !persistUIConfig(chatID, cfg) {
			return true
		}
		sendMessage(chatID, "人设已更新，下一条消息生效。", nil)
		return true
	case "/system":
		if !isAdmin(uid) {
			sendMessage(chatID, "只有管理员能改系统提示词。", nil)
			return true
		}
		if rest == "" {
			cur := cfg.SystemPrompt
			if strings.TrimSpace(cur) == "" {
				cur = "（未设置）"
			}
			sendMessage(chatID, "当前系统提示词：\n\n"+short(cur, 3200)+"\n\n设置: /system <文本>\n清除: /system clear", nil)
			return true
		}
		if strings.EqualFold(rest, "clear") {
			cfg.SystemPrompt = ""
			if !persistUIConfig(chatID, cfg) {
				return true
			}
			sendMessage(chatID, "已清除。", nil)
			return true
		}
		cfg.SystemPrompt = rest
		if !persistUIConfig(chatID, cfg) {
			return true
		}
		sendMessage(chatID, "系统提示词已更新，下一条消息生效。", nil)
		return true
	case "/skills":
		return handleSkillsCommand(chatID, uid, rest)
	case "/mode", "/approval", "/execution":
		return handleExecutionCommand(chatID, uid, rest)
	case "/settings":
		if isAdmin(uid) {
			sendMessage(chatID, "设置\n\n模型、执行和自动化统一在这里管理。", adminMenu(uid))
		}
		return true
	case "/effort":
		if !isAdmin(uid) {
			sendMessage(chatID, "只有管理员能改推理档位。", nil)
			return true
		}
		if rest == "" {
			sendMessage(chatID, "当前档位: "+cfg.ReasoningEffort+"\n用法: /effort low|medium|high|max", nil)
			return true
		}
		switch rest {
		case "low", "medium", "high", "max":
			cfg.ReasoningEffort = rest
			if !persistUIConfig(chatID, cfg) {
				return true
			}
			sendMessage(chatID, "推理档位已更新。", nil)
		default:
			sendMessage(chatID, "可选: low / medium / high / max", nil)
		}
		return true
	case "/admin":
		return handleList(chatID, uid, rest, "admin")
	case "/friend":
		return handleList(chatID, uid, rest, "friend")
	case "/ban":
		return handleList(chatID, uid, rest, "ban")
	case "/ws", "/workspace":
		if !isAdmin(uid) {
			sendMessage(chatID, "只有管理员能改工作区。", nil)
			return true
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			var b strings.Builder
			b.WriteString("工作区\n\n")
			b.WriteString("当前: " + cfg.Workspace + "\n\n")
			if len(cfg.Workspaces) == 0 {
				b.WriteString("常用列表为空\n")
			} else {
				b.WriteString("常用:\n")
				for i, w := range cfg.Workspaces {
					mark := " "
					if w == cfg.Workspace {
						mark = "●"
					}
					b.WriteString(fmt.Sprintf("%s %d. %s\n", mark, i+1, w))
				}
			}
			b.WriteString("\n切换: /ws <路径或编号>\n记住: /ws add <路径>\n忘掉: /ws del <编号>")
			sendMessage(chatID, b.String(), nil)
			return true
		}
		switch f[0] {
		case "add":
			if len(f) < 2 {
				sendMessage(chatID, "用法: /ws add /root/project", nil)
				return true
			}
			path := strings.TrimSpace(rest[3:])
			for _, w := range cfg.Workspaces {
				if w == path {
					sendMessage(chatID, "已经在列表里。", nil)
					return true
				}
			}
			cfg.Workspaces = append(cfg.Workspaces, path)
			if !persistUIConfig(chatID, cfg) {
				return true
			}
			sendMessage(chatID, "已记住: "+path, nil)
			return true
		case "del":
			if len(f) < 2 {
				sendMessage(chatID, "用法: /ws del <编号>", nil)
				return true
			}
			idx, err := strconv.Atoi(f[1])
			if err != nil || idx < 1 || idx > len(cfg.Workspaces) {
				sendMessage(chatID, "编号不对。", nil)
				return true
			}
			removed := cfg.Workspaces[idx-1]
			cfg.Workspaces = append(cfg.Workspaces[:idx-1], cfg.Workspaces[idx:]...)
			if !persistUIConfig(chatID, cfg) {
				return true
			}
			sendMessage(chatID, "已移除: "+removed, nil)
			return true
		}
		// 切换
		target := strings.TrimSpace(rest)
		if idx, err := strconv.Atoi(target); err == nil && idx >= 1 && idx <= len(cfg.Workspaces) {
			target = cfg.Workspaces[idx-1]
		}
		if out, err := runShell("test -d "+shellQuote(target)+" && echo OK", 15*time.Second); err != nil || !strings.Contains(out, "OK") {
			sendMessage(chatID, "目录不存在: "+target, nil)
			return true
		}
		cfg.Workspace = target
		if !persistUIConfig(chatID, cfg) {
			return true
		}
		resetHistory(chatID)
		sendMessage(chatID, "工作区已切换: "+target+"\n对话已重置。", nil)
		return true
	case "/health":
		if !isAdmin(uid) {
			sendMessage(chatID, "只有管理员能运行自检。", nil)
			return true
		}
		sendMessage(chatID, healthCheck(), nil)
		return true
	case "/runs":
		if !isAdmin(uid) {
			sendMessage(chatID, "只有管理员能查看执行记录。", nil)
			return true
		}
		sendMessage(chatID, listRuns(10), nil)
		return true
	case "/continue", "/retry":
		if v, ok := lastTasks.Load(chatID); ok {
			task := v.(string)
			sendMessage(chatID, "重跑上一个任务："+short(task, 60), nil)
			go runUserTask(chatID, uid, task, nil)
			return true
		}
		sendMessage(chatID, "还没有可以继续的任务。", nil)
		return true
	case "/cmd":
		return handleQuickCmd(chatID, uid, rest)
	case "/cron":
		return handleCron(chatID, uid, rest)
	case "/img", "/file":
		return handleGetFile(chatID, uid, cmd, rest)
	}
	sendMessage(chatID, "未知命令。发送 /menu 打开面板，或直接描述任务。", nil)
	return true
}

func handleProvider(chatID, uid int64, rest string) bool {
	cfg := configSnapshot()
	if !isAdmin(uid) {
		sendMessage(chatID, "只有管理员能管理服务商。", nil)
		return true
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		var b strings.Builder
		b.WriteString("已配置的服务商：\n\n")
		for _, p := range cfg.Provider {
			mark := " "
			if p.Name == cfg.ProviderName {
				mark = "●"
			}
			b.WriteString(fmt.Sprintf("%s %s  %s  (%d 个模型)\n", mark, p.Name, p.BaseURL, len(p.Models)))
		}
		b.WriteString("\n切换: /provider <名字>\n")
		b.WriteString("添加: /provider add <名字> <base_url> <api_key> <模型1,模型2>")
		sendMessage(chatID, b.String(), nil)
		return true
	}
	if f[0] == "add" && len(f) >= 5 {
		name := f[1]
		models := strings.Split(f[4], ",")
		for i := range models {
			models[i] = strings.TrimSpace(models[i])
		}
		setProviderIn(&cfg, Provider{
			Name:         name,
			BaseURL:      f[2],
			APIKey:       f[3],
			Models:       models,
			DefaultModel: models[0],
		})
		if !persistUIConfig(chatID, cfg) {
			return true
		}
		sendMessage(chatID, fmt.Sprintf("已添加服务商 %s，模型 %d 个。\n用 /provider %s 切过去。", name, len(models), name), nil)
		return true
	}
	if f[0] == "del" && len(f) >= 2 {
		var keep []Provider
		for _, p := range cfg.Provider {
			if p.Name != f[1] {
				keep = append(keep, p)
			}
		}
		cfg.Provider = keep
		if !persistUIConfig(chatID, cfg) {
			return true
		}
		sendMessage(chatID, "已删除 "+f[1], nil)
		return true
	}
	for _, p := range cfg.Provider {
		if p.Name == f[0] {
			cfg.ProviderName = p.Name
			if len(p.Models) > 0 {
				cfg.Model = p.DefaultModel
				if cfg.Model == "" {
					cfg.Model = p.Models[0]
				}
			}
			if !persistUIConfig(chatID, cfg) {
				return true
			}
			sendMessage(chatID, fmt.Sprintf("已切换到 %s，模型 %s", p.Name, cfg.Model), nil)
			return true
		}
	}
	sendMessage(chatID, "没有这个服务商。", nil)
	return true
}

func handleList(chatID, uid int64, rest, kind string) bool {
	cfg := configSnapshot()
	if !isAdmin(uid) {
		sendMessage(chatID, "只有管理员能操作。", nil)
		return true
	}
	f := strings.Fields(rest)
	var list *[]int64
	title := ""
	switch kind {
	case "admin":
		list, title = &cfg.Admins, "管理员"
	case "friend":
		list, title = &cfg.Friends, "朋友"
	case "ban":
		list, title = &cfg.Banned, "封禁"
	}
	if len(f) == 0 {
		if len(*list) == 0 {
			sendMessage(chatID, title+"名单为空。", nil)
			return true
		}
		var b strings.Builder
		b.WriteString(title + "：\n\n")
		for _, id := range *list {
			b.WriteString(fmt.Sprintf("  %d  %s  最后 %s\n", id, userName(id), lastActive(id)))
		}
		sendMessage(chatID, b.String(), nil)
		return true
	}
	if len(f) < 2 {
		sendMessage(chatID, "用法: /"+kind+" add|rem <id>", nil)
		return true
	}
	id, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || id <= 0 || id == cfg.OwnerID {
		sendMessage(chatID, "请填写有效用户 ID；主人身份不可更改。", nil)
		return true
	}
	switch f[0] {
	case "add":
		for _, x := range *list {
			if x == id {
				sendMessage(chatID, "已经在名单里。", nil)
				return true
			}
		}
		*list = append(*list, id)
	case "rem", "del":
		var keep []int64
		for _, x := range *list {
			if x != id {
				keep = append(keep, x)
			}
		}
		*list = keep
	default:
		sendMessage(chatID, "只支持 add / rem", nil)
		return true
	}
	if !persistUIConfig(chatID, cfg) {
		return true
	}
	sendMessage(chatID, fmt.Sprintf("%s %d 已更新。", title, id), nil)
	return true
}

func handleQuickCmd(chatID, uid int64, rest string) bool {
	cfg := configSnapshot()
	f := strings.Fields(rest)
	if len(f) == 0 {
		sendMessage(chatID, "快捷指令\n\n"+quickList(), nil)
		return true
	}
	if f[0] == "add" {
		if !isAdmin(uid) {
			sendMessage(chatID, "只有管理员能加。", nil)
			return true
		}
		if len(f) < 3 {
			sendMessage(chatID, "用法: /cmd add <名字> <提示词>", nil)
			return true
		}
		name := f[1]
		prompt := strings.TrimSpace(rest[len("add "+name):])
		var keep []QuickCmd
		for _, q := range cfg.QuickCommands {
			if q.Name != name {
				keep = append(keep, q)
			}
		}
		cfg.QuickCommands = append(keep, QuickCmd{Name: name, Prompt: prompt})
		if !persistUIConfig(chatID, cfg) {
			return true
		}
		sendMessage(chatID, fmt.Sprintf("已添加指令 %s，用 /cmd %s 触发。", name, name), nil)
		return true
	}
	if f[0] == "del" && len(f) >= 2 {
		if !isAdmin(uid) {
			return true
		}
		var keep []QuickCmd
		for _, q := range cfg.QuickCommands {
			if q.Name != f[1] {
				keep = append(keep, q)
			}
		}
		cfg.QuickCommands = keep
		if !persistUIConfig(chatID, cfg) {
			return true
		}
		sendMessage(chatID, "已删除 "+f[1], nil)
		return true
	}
	if q, ok := findQuick(f[0]); ok {
		go runUserTask(chatID, uid, q.Prompt, nil)
		return true
	}
	sendMessage(chatID, "没有这个指令。\n\n"+quickList(), nil)
	return true
}

func handleCron(chatID, uid int64, rest string) bool {
	if !isAdmin(uid) {
		sendMessage(chatID, "只有管理员能用定时任务。", nil)
		return true
	}
	f := strings.Fields(rest)
	if len(f) == 0 || f[0] == "list" {
		sendMessage(chatID, "定时任务\n\n"+listCron()+
			"\n添加: /cron add <间隔> <任务>\n间隔写法: 1m / 10m / 2h / 1d（至少 1 分钟）\n删除: /cron del <编号>", nil)
		return true
	}
	if f[0] == "add" && len(f) >= 3 {
		sec, err := parseEvery(f[1])
		if err != nil {
			sendMessage(chatID, "间隔须在 1 分钟至 366 天之间，例如 30m / 2h / 1d", nil)
			return true
		}
		prompt := strings.TrimSpace(rest[len("add "+f[1]):])
		id := addCronFor(chatID, uid, sec, prompt)
		if id == 0 {
			sendMessage(chatID, "任务创建失败：间隔至少 1 分钟，任务不能为空。", nil)
			return true
		}
		sendMessage(chatID, fmt.Sprintf("定时任务 #%d 已创建：每 %s 执行「%s」", id, humanEvery(sec), short(prompt, 50)), nil)
		return true
	}
	if f[0] == "del" && len(f) >= 2 {
		id, err := strconv.Atoi(f[1])
		if err == nil && delCron(id) {
			sendMessage(chatID, fmt.Sprintf("已删除 #%d", id), nil)
		} else {
			sendMessage(chatID, "没有这个编号。", nil)
		}
		return true
	}
	sendMessage(chatID, "用法: /cron add <间隔> <任务> | /cron list | /cron del <id>", nil)
	return true
}

func parseEvery(s string) (int64, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid interval")
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid interval")
	}
	var multiplier int64
	switch s[len(s)-1] {
	case 's':
		multiplier = 1
	case 'm':
		multiplier = 60
	case 'h':
		multiplier = 3600
	case 'd':
		multiplier = 86400
	default:
		return 0, fmt.Errorf("invalid interval")
	}
	const maxInterval int64 = 366 * 86400
	if n > maxInterval/multiplier {
		return 0, fmt.Errorf("interval exceeds one year")
	}
	seconds := n * multiplier
	if seconds < 60 {
		return 0, fmt.Errorf("interval must be at least 60 seconds")
	}
	return seconds, nil
}

func handleGetFile(chatID, uid int64, cmd, rest string) bool {
	if strings.TrimSpace(rest) == "" {
		sendMessage(chatID, "用法: "+cmd+" <绝对路径>", nil)
		return true
	}
	if !isAdmin(uid) {
		sendMessage(chatID, "只有管理员能取回文件。", nil)
		return true
	}
	data, err := readDeliverable(rest)
	if err != nil {
		sendMessage(chatID, "取不到: "+err.Error(), nil)
		return true
	}
	if !sendByExt(chatID, rest, data) {
		sendMessage(chatID, "发送失败。", nil)
	}
	return true
}

func helpBody(uid int64) string {
	body := "大肥鱼 Agent\n\n直接描述任务，我会规划、执行并验证结果。\n支持图片、文件、语音和视频。\n\n/menu 打开面板 · /new 新对话 · /stop 停止任务\n/continue 继续上次任务 · /skills 管理技能"
	if isAdmin(uid) {
		body += "\n\n/settings 模型、执行模式、工作区和自动化\n/mode 审核与自动执行 · /runs 执行记录\n/file <路径> 取回文件"
	}
	return body
}

func sendHelp(chatID, uid int64) {
	sendMessage(chatID, helpBody(uid), mainMenu(uid))
}

// ---------------- 回调 ----------------

func handleCallback(cb CallbackQuery) {
	if cb.Message == nil || cb.Message.Chat.ID <= 0 || cb.Message.Chat.ID != cb.From.ID || (cb.Message.Chat.Type != "" && cb.Message.Chat.Type != "private") {
		answerCallback(cb.ID)
		return
	}
	if handleApprovalCallback(cb) {
		return
	}
	if cb.Data == "nav:execution" || strings.HasPrefix(cb.Data, "mode:") {
		uiMu.Lock()
		handled := handleExecutionCallback(cb)
		uiMu.Unlock()
		if handled {
			return
		}
	}
	answerCallback(cb.ID)
	if cb.Message == nil || cb.Message.Chat.ID == 0 || cb.Message.MessageID == 0 {
		return
	}
	if strings.HasPrefix(cb.Data, "set:") || strings.HasPrefix(cb.Data, "unset:") {
		uiMu.Lock()
		defer uiMu.Unlock()
	}
	cfg := configSnapshot()
	chatID := cb.Message.Chat.ID
	msgID := cb.Message.MessageID
	uid := cb.From.ID
	if !allowed(uid) {
		return
	}
	data := cb.Data
	admin := isAdmin(uid)

	needAdmin := func() bool {
		if !admin {
			sendMessage(chatID, "只有管理员能进管理面板。", nil)
			return false
		}
		return true
	}

	switch {
	// ---------- 导航 ----------
	case data == "nav:root", data == "nav:use":
		editMessage(chatID, msgID, menuText(uid), mainMenu(uid))
	case data == "nav:admin", data == "nav:settings":
		if needAdmin() {
			editMessage(chatID, msgID, "设置\n\n选择需要调整的项目。", adminMenu(uid))
		}
	case data == "nav:advanced":
		if needAdmin() {
			editMessage(chatID, msgID, "高级设置", advancedMenu(uid))
		}
	case data == "nav:execution":
		if needAdmin() {
			editMessage(chatID, msgID, executionSettingsText(), executionSettingsMenu())
		}
	case data == "nav:workspace":
		if needAdmin() {
			editMessage(chatID, msgID, "工作区\n\n当前: "+cfg.Workspace+"\n切换: /ws <绝对路径或编号>\n收藏: /ws add <绝对路径>", keyboard([][]Btn{{btn("返回", "nav:settings")}}))
		}
	case data == "act:health":
		if !needAdmin() {
			return
		}
		editMessage(chatID, msgID, "检查中…", nil)
		editMessage(chatID, msgID, healthCheck(), keyboard([][]Btn{{btn("重测", "act:health"), btn("返回", "nav:root")}}))
	case data == "act:runs":
		if !needAdmin() {
			return
		}
		editMessage(chatID, msgID, listRuns(10), keyboard([][]Btn{{btn("刷新", "act:runs"), btn("返回", "nav:root")}}))
	case data == "act:cron":
		if !needAdmin() {
			return
		}
		editMessage(chatID, msgID, "定时任务\n\n"+listCron()+
			"\n添加: /cron add 30m <任务>\n删除: /cron del <编号>",
			keyboard([][]Btn{{btn("刷新", "act:cron"), btn("返回", "nav:root")}}))
	case data == "act:quick":
		if !needAdmin() {
			return
		}
		editMessage(chatID, msgID, "快捷指令\n\n"+quickList()+
			"\n添加: /cmd add <名字> <提示词>\n使用: /cmd <名字>",
			keyboard([][]Btn{{btn("返回", "nav:root")}}))
	case data == "act:help":
		editMessage(chatID, msgID, helpBody(uid), mainMenu(uid))
	case data == "nav:effort":
		if !needAdmin() {
			return
		}
		var rows [][]Btn
		for _, lv := range []string{"low", "medium", "high", "max"} {
			rows = append(rows, []Btn{btn(mark(cfg.ReasoningEffort, lv), "set:effort:"+lv)})
		}
		rows = append(rows, []Btn{btn("返回", "nav:root")})
		editMessage(chatID, msgID, "选择推理档位（越高思考越久）：", keyboard(rows))
	case strings.HasPrefix(data, "set:effort:"):
		if !needAdmin() {
			return
		}
		level := strings.TrimPrefix(data, "set:effort:")
		if level != "low" && level != "medium" && level != "high" && level != "max" {
			return
		}
		cfg.ReasoningEffort = level
		if !persistUIConfig(chatID, cfg) {
			return
		}
		editMessage(chatID, msgID, "推理档位已切换: "+cfg.ReasoningEffort+"\n\n"+menuText(uid), mainMenu(uid))

	// ---------- 使用面板 ----------
	case data == "act:new":
		if _, running := busy.Load(chatID); running {
			sendMessage(chatID, "请先停止当前任务，再新建对话。", nil)
			return
		}
		resetHistory(chatID)
		editMessage(chatID, msgID, "已开新对话。\n\n"+menuText(uid), useMenu(uid))
	case data == "act:stop":
		stopTask(chatID)
		editMessage(chatID, msgID, "已请求停止。\n\n"+menuText(uid), useMenu(uid))
	case data == "act:status":
		editMessage(chatID, msgID, menuText(uid), useMenu(uid))
	case data == "act:think":
		if !needAdmin() {
			return
		}
		th := getLastThink(chatID)
		if th == "" {
			th = "还没有思考记录。"
		}
		editMessage(chatID, msgID, "最近一次思考\n\n"+short(th, 3400),
			keyboard([][]Btn{{btn("返回", "nav:use")}}))
	case data == "act:filehint":
		editMessage(chatID, msgID,
			"取文件\n\n/img <绝对路径>   取图片\n/file <绝对路径>  取任意文件\n\n"+
				"也可以直接让 agent 生成图片或文件，回复里出现路径会自动发给你。",
			keyboard([][]Btn{{btn("返回", "nav:use")}}))
	case data == "nav:skills":
		sk, _ := loadSkillIndex()
		if len(sk) == 0 {
			editMessage(chatID, msgID, "技能目录为空: "+cfg.SkillsDir,
				keyboard([][]Btn{{btn("返回", "nav:use")}}))
			return
		}
		var b strings.Builder
		for _, s := range sk {
			b.WriteString(fmt.Sprintf("- %s: %s\n", s.Name, s.Desc))
		}
		editMessage(chatID, msgID, "技能（"+strconv.Itoa(len(sk))+" 个）\n\n"+short(b.String(), 2900)+"\n\n自动按任务匹配本地技能。\n/skills refresh 重新扫描\n/skills install <名称> 安装已配置技能",
			keyboard([][]Btn{{btn("返回", "nav:use")}}))

	// ---------- 管理面板 ----------
	case data == "act:config":
		if needAdmin() {
			editMessage(chatID, msgID, configOverview(),
				keyboard([][]Btn{{btn("返回", "nav:admin")}}))
		}
	case data == "act:log":
		if needAdmin() {
			editMessage(chatID, msgID, "运行日志\n\n"+short(tailLog(20), 3400),
				keyboard([][]Btn{{btn("刷新", "act:log"), btn("返回", "nav:admin")}}))
		}
	case data == "nav:model":
		if !needAdmin() {
			return
		}
		p := currentProvider()
		if p == nil {
			return
		}
		var rows [][]Btn
		for _, m := range p.Models {
			rows = append(rows, []Btn{btn(mark(cfg.Model, short(m, 40)), settingCallback("model", m))})
		}
		rows = append(rows, []Btn{btn("返回", "nav:admin")})
		editMessage(chatID, msgID, "选择模型：", keyboard(rows))
	case strings.HasPrefix(data, "set:model:"):
		if !needAdmin() {
			return
		}
		model := ""
		p := currentProvider()
		valid := false
		if p != nil {
			for _, m := range p.Models {
				if data == settingCallback("model", m) || data == "set:model:"+m {
					valid = true
					model = m
				}
			}
		}
		if !valid {
			return
		}
		cfg.Model = model
		if !persistUIConfig(chatID, cfg) {
			return
		}
		editMessage(chatID, msgID, "模型已切换: "+cfg.Model+"\n\n"+menuText(uid),
			keyboard([][]Btn{{btn("返回", "nav:admin")}}))
	case data == "nav:provider":
		if !needAdmin() {
			return
		}
		var rows [][]Btn
		for _, p := range cfg.Provider {
			rows = append(rows, []Btn{btn(mark(cfg.ProviderName, short(p.Name, 40)), settingCallback("provider", p.Name))})
		}
		rows = append(rows, []Btn{btn("添加用 /provider add", "nav:provider")})
		rows = append(rows, []Btn{btn("返回", "nav:admin")})
		editMessage(chatID, msgID, "选择服务商：", keyboard(rows))
	case strings.HasPrefix(data, "set:provider:"):
		if !needAdmin() {
			return
		}
		for _, p := range cfg.Provider {
			if data == settingCallback("provider", p.Name) || data == "set:provider:"+p.Name {
				cfg.ProviderName = p.Name
				if len(p.Models) > 0 {
					cfg.Model = p.DefaultModel
					if cfg.Model == "" {
						cfg.Model = p.Models[0]
					}
				}
			}
		}
		if !persistUIConfig(chatID, cfg) {
			return
		}
		editMessage(chatID, msgID, "已切换服务商: "+cfg.ProviderName+"\n\n"+menuText(uid),
			keyboard([][]Btn{{btn("返回", "nav:admin")}}))
	case data == "nav:system":
		if !needAdmin() {
			return
		}
		cur := cfg.SystemPrompt
		if strings.TrimSpace(cur) == "" {
			cur = "（未设置）"
		}
		editMessage(chatID, msgID, "系统提示词\n\n"+short(cur, 3000)+"\n\n设置: /system <文本>\n清除: /system clear",
			keyboard([][]Btn{{btn("返回", "nav:admin")}}))
	case data == "nav:persona":
		if !needAdmin() {
			return
		}
		cur := cfg.Persona
		if strings.TrimSpace(cur) == "" {
			cur = "（未设置，使用内置助手人格）"
		}
		editMessage(chatID, msgID, "人设\n\n"+short(cur, 3000)+"\n\n设置: /persona <文本>\n清除: /persona clear",
			keyboard([][]Btn{{btn("返回", "nav:admin")}}))
	case data == "nav:users":
		if !needAdmin() {
			return
		}
		var rows [][]Btn
		addRow := func(id int64, tag string) {
			rows = append(rows, []Btn{btn(fmt.Sprintf("%s %d %s", tag, id, short(userName(id), 10)),
				"user:"+strconv.FormatInt(id, 10))})
		}
		if cfg.OwnerID != 0 {
			addRow(cfg.OwnerID, "[主人]")
		}
		for _, a := range cfg.Admins {
			addRow(a, "[管理]")
		}
		for _, f := range cfg.Friends {
			addRow(f, "[朋友]")
		}
		rows = append(rows, []Btn{btn("所有来访用户", "nav:allusers")})
		rows = append(rows, []Btn{btn("返回", "nav:admin")})
		editMessage(chatID, msgID,
			fmt.Sprintf("用户管理\n\n主人 1 位（唯一）\n管理员 %d\n朋友 %d\n封禁 %d\n\n点某人操作。",
				len(cfg.Admins), len(cfg.Friends), len(cfg.Banned)),
			keyboard(rows))
	case data == "nav:allusers":
		if !needAdmin() {
			return
		}
		seenMu.Lock()
		type pair struct {
			id int64
			s  SeenUser
		}
		var list []pair
		for id, s := range seen {
			list = append(list, pair{id, s})
		}
		seenMu.Unlock()
		sort.Slice(list, func(i, j int) bool { return list[i].s.Last > list[j].s.Last })
		var rows [][]Btn
		n := len(list)
		if n > 12 {
			n = 12
		}
		for _, p := range list[:n] {
			tag := " "
			switch {
			case isOwner(p.id):
				tag = "[主人]"
			case isBanned(p.id):
				tag = "[封禁]"
			case isAdmin(p.id):
				tag = "[管理]"
			case isFriend(p.id):
				tag = "[朋友]"
			}
			rows = append(rows, []Btn{btn(fmt.Sprintf("%s %d %s", tag, p.id, short(p.s.Name, 10)),
				"user:"+strconv.FormatInt(p.id, 10))})
		}
		rows = append(rows, []Btn{btn("返回", "nav:users")})
		editMessage(chatID, msgID, fmt.Sprintf("所有来访用户（%d 人）", len(list)), keyboard(rows))
	case data == "nav:bans":
		if !needAdmin() {
			return
		}
		if len(cfg.Banned) == 0 {
			editMessage(chatID, msgID, "封禁名单为空。\n\n用 /ban add <id> 添加。",
				keyboard([][]Btn{{btn("返回", "nav:admin")}}))
			return
		}
		var rows [][]Btn
		for _, id := range cfg.Banned {
			rows = append(rows, []Btn{btn(fmt.Sprintf("解除 %d %s", id, short(userName(id), 8)),
				"unset:ban:"+strconv.FormatInt(id, 10))})
		}
		rows = append(rows, []Btn{btn("返回", "nav:admin")})
		editMessage(chatID, msgID, "封禁名单\n\n点一下解除。", keyboard(rows))
	case strings.HasPrefix(data, "user:"):
		if !needAdmin() {
			return
		}
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "user:"), 10, 64)
		info := fmt.Sprintf("用户 %d\n\n名字     %s\n身份     %s\n最后活跃 %s\n",
			id, userName(id), roleName(id), lastActive(id))
		rows := [][]Btn{}
		if isOwner(id) {
			info += "\n主人身份不可更改。"
		} else {
			if !isAdmin(id) {
				rows = append(rows, []Btn{btn("设为管理员", fmt.Sprintf("set:admin:%d", id))})
			} else {
				rows = append(rows, []Btn{btn("取消管理员", fmt.Sprintf("unset:admin:%d", id))})
			}
			if !isFriend(id) {
				rows = append(rows, []Btn{btn("加为朋友", fmt.Sprintf("set:friend:%d", id))})
			} else {
				rows = append(rows, []Btn{btn("移出朋友", fmt.Sprintf("unset:friend:%d", id))})
			}
			if !isBanned(id) {
				rows = append(rows, []Btn{btn("封禁", fmt.Sprintf("set:ban:%d", id))})
			} else {
				rows = append(rows, []Btn{btn("解除封禁", fmt.Sprintf("unset:ban:%d", id))})
			}
		}
		rows = append(rows, []Btn{btn("返回", "nav:users")})
		editMessage(chatID, msgID, info, keyboard(rows))
	case strings.HasPrefix(data, "set:admin:") || strings.HasPrefix(data, "unset:admin:") ||
		strings.HasPrefix(data, "set:friend:") || strings.HasPrefix(data, "unset:friend:") ||
		strings.HasPrefix(data, "set:ban:") || strings.HasPrefix(data, "unset:ban:"):
		if !needAdmin() {
			return
		}
		parts := strings.SplitN(data, ":", 3)
		if len(parts) != 3 {
			return
		}
		op, kind := parts[0], parts[1]
		id, _ := strconv.ParseInt(parts[2], 10, 64)
		add := op == "set"
		if id == cfg.OwnerID {
			editMessage(chatID, msgID, "主人身份固定，不能改。",
				keyboard([][]Btn{{btn("返回", "nav:users")}}))
			return
		}
		switch kind {
		case "admin":
			cfg.Admins = toggleID(cfg.Admins, id, add)
		case "friend":
			cfg.Friends = toggleID(cfg.Friends, id, add)
		case "ban":
			cfg.Banned = toggleID(cfg.Banned, id, add)
		}
		if !persistUIConfig(chatID, cfg) {
			return
		}
		editMessage(chatID, msgID, fmt.Sprintf("%s %d 已更新。", kind, id),
			keyboard([][]Btn{{btn("返回", "nav:users")}}))
	}
}

func tailLog(n int) string {
	data, err := os.ReadFile(filepath.Join(baseDir, "agent.log"))
	if err != nil {
		return "（没有日志）"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func adminSummary() string {
	cfg := configSnapshot()
	p := currentProvider()
	pname := ""
	if p != nil {
		pname = p.Name
	}
	return fmt.Sprintf("模型   %s\n服务   %s\n管理员 %d\n朋友   %d\n封禁   %d\n技能   %d 个",
		cfg.Model, pname, len(cfg.Admins), len(cfg.Friends), len(cfg.Banned), len(mustSkills()))
}

func mustSkills() []Skill {
	s, _ := loadSkillIndex()
	return s
}

func configOverview() string {
	cfg := configSnapshot()
	var b strings.Builder
	b.WriteString("配置总览\n\n")
	b.WriteString(fmt.Sprintf("主人     %d %s\n", cfg.OwnerID, userName(cfg.OwnerID)))
	b.WriteString(fmt.Sprintf("模型     %s\n", cfg.Model))
	b.WriteString(fmt.Sprintf("服务商   %s（%d 个）\n", cfg.ProviderName, len(cfg.Provider)))
	b.WriteString(fmt.Sprintf("工作区   %s\n", cfg.Workspace))
	b.WriteString(fmt.Sprintf("后端     %s\n", cfg.Backend))
	b.WriteString(fmt.Sprintf("最大步数 %d\n", cfg.MaxSteps))
	b.WriteString(fmt.Sprintf("温度     %.1f\n", cfg.Temperature))
	b.WriteString(fmt.Sprintf("历史     %d 轮\n", cfg.MaxHistory))
	b.WriteString(fmt.Sprintf("技能目录 %s\n", cfg.SkillsDir))
	b.WriteString(fmt.Sprintf("\n人设     %s\n", onOff(strings.TrimSpace(cfg.Persona) != "")))
	b.WriteString(fmt.Sprintf("系统词   %s\n", onOff(strings.TrimSpace(cfg.SystemPrompt) != "")))
	return b.String()
}

func onOff(v bool) string {
	if v {
		return "已设置"
	}
	return "未设置"
}

func toggleID(list []int64, id int64, add bool) []int64 {
	if add {
		for _, x := range list {
			if x == id {
				return list
			}
		}
		return append(list, id)
	}
	var keep []int64
	for _, x := range list {
		if x != id {
			keep = append(keep, x)
		}
	}
	return keep
}

func normalizeBotCommand(text string) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}
	end := strings.IndexAny(text, " \t\r\n")
	if end < 0 {
		end = len(text)
	}
	command := text[:end]
	if at := strings.IndexByte(command, '@'); at >= 0 {
		command = command[:at]
	}
	return command + text[end:]
}

func commandMutatesConfig(command, rest string) bool {
	switch command {
	case "/model", "/provider", "/persona", "/system", "/admin", "/friend", "/ban", "/ws", "/workspace", "/effort", "/mode", "/approval", "/execution":
		return rest != ""
	case "/cmd":
		fields := strings.Fields(rest)
		return len(fields) > 0 && (fields[0] == "add" || fields[0] == "del")
	}
	return false
}

func persistUIConfig(chatID int64, next Config) bool {
	if err := saveConfig(next); err != nil {
		sendMessage(chatID, "保存配置失败："+redactSecrets(err.Error()), nil)
		return false
	}
	return true
}

func settingCallback(kind, value string) string {
	sum := sha256.Sum256([]byte(value))
	return "set:" + kind + ":" + hex.EncodeToString(sum[:8])
}
