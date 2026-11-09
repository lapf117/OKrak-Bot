package main

import (
	"fmt"
	"strings"
)

// handleMessage is the entry point for every private message.
func handleMessage(m Message) {
	chatID, uid := m.Chat.ID, m.From.ID
	if (m.Chat.Type != "" && m.Chat.Type != "private") || chatID <= 0 || chatID != uid {
		sendMessage(chatID, tr(langZH, "err.private_only")+"\n"+tr(langEN, "err.private_only"), nil)
		return
	}
	t := strings.TrimSpace(m.Text)
	if t == "" {
		t = strings.TrimSpace(m.Caption)
	}
	hasMedia := m.hasMedia()
	if t == "" && !hasMedia {
		return
	}
	touchUser(m.From)
	lang := langFor(uid)

	t = normalizeBotCommand(t)
	low := strings.ToLower(t)
	if low == "/whoami" || low == "/id" {
		sendMessage(chatID, tr(lang, "whoami", uid, chatID, roleNameLang(uid, lang)), nil)
		return
	}
	if !allowed(uid) {
		handleGuest(m, t, low) // banned notice, or the sign-up page
		return
	}
	// After an admin taps an input button in the console, the next text is the value.
	if !hasMedia && handlePendingInput(chatID, uid, t, low) {
		return
	}
	switch low {
	case "/start", "/menu", "menu", "菜单":
		sendMenu(chatID, uid)
		return
	case "/help", "help", "帮助":
		sendHelp(chatID, uid)
		return
	}
	if strings.HasPrefix(low, "/") && handleCommand(chatID, uid, t, low) {
		return
	}
	// Check before downloading media so a busy chat does not waste bandwidth.
	if isBusy(chatID) {
		sendMessage(chatID, tr(lang, "task.busy"), nil)
		return
	}
	var images []string
	if hasMedia {
		info := storeIncoming(m)
		if info.Err != nil {
			sendMessage(chatID, tr(lang, "err.download", info.Err.Error()), nil)
			return
		}
		t, images = describeMedia(lang, info, t)
	}
	if strings.TrimSpace(t) == "" {
		return
	}
	runUserTask(chatID, uid, t, images)
}

// describeMedia turns an uploaded file into task text (and image inputs).
func describeMedia(lang string, info mediaInfo, t string) (string, []string) {
	if info.Path == "" {
		return t, nil
	}
	switch info.Kind {
	case "image":
		if t == "" {
			t = tr(lang, "media.image_default")
		}
		return t, []string{info.Path}
	case "audio":
		if txt, err := transcribe(info.Path); err == nil && strings.TrimSpace(txt) != "" {
			return strings.TrimSpace(fmt.Sprintf("[用户发来语音，转文字如下 / voice transcript]\n%s\n%s", txt, t)), nil
		}
		return fmt.Sprintf("[用户发来语音/音频，已保存到 %s]（未配置语音识别，可用工具自行处理，如 ffmpeg 看时长或转码）\n%s", info.Path, t), nil
	case "video":
		return fmt.Sprintf("[用户发来视频，已保存到 %s]（可用 ffmpeg 抽帧、抽音频、看时长）\n%s", info.Path, t), nil
	}
	return fmt.Sprintf("[用户发来文件，已保存到 %s]\n%s", info.Path, t), nil
}

// looksLikePath reports text such as "/etc/nginx 怎么配置" that starts with a
// slash but is a question about a path rather than a bot command.
func looksLikePath(cmd string) bool {
	return strings.Count(cmd, "/") > 1 || strings.Contains(cmd, ".")
}
