package main

import (
	"fmt"
	"strings"
	"sync"
)

// uiMu serializes interactive configuration changes from commands and buttons.
var uiMu sync.Mutex

// handleCommand returns true when the text was handled as a command.
func handleCommand(chatID, uid int64, t, low string) bool {
	fields := strings.Fields(t)
	if len(fields) == 0 {
		return false
	}
	cmd := strings.ToLower(fields[0])
	rest := argsAfter(t, 1)
	if commandMutatesConfig(cmd, rest) {
		uiMu.Lock()
		defer uiMu.Unlock()
	}
	lang := langFor(uid)

	switch cmd {
	case "/new", "/reset":
		if isBusy(chatID) {
			sendMessage(chatID, tr(lang, "task.stop_first"), nil)
			return true
		}
		resetHistory(chatID)
		sendUI(chatID, esc(tr(lang, "chat.reset"))+"\n\n"+menuText(uid), mainMenu(uid))
	case "/stop":
		if stopTask(chatID) {
			sendMessage(chatID, tr(lang, "task.stop_requested"), nil)
		} else {
			sendMessage(chatID, tr(lang, "task.none"), nil)
		}
	case "/status":
		sendMenu(chatID, uid)
	case "/lang", "/language":
		handleLangCommand(chatID, uid, rest)
	case "/steps", "/verbose":
		handleStepsCommand(chatID, uid, rest)
	case "/show", "/display", "/footer", "/显示":
		handleShowCommand(chatID, uid, rest)
	case "/memory", "/mem":
		return handleMemoryCommand(chatID, uid, rest)
	case "/export":
		exportConversation(chatID, uid)
	case "/usage", "/balance":
		handleUsageCommand(chatID, uid)
	case "/think":
		if !isAdmin(uid) {
			sendMessage(chatID, tr(lang, "err.admin_only"), nil)
			return true
		}
		if th := getLastThink(chatID); th != "" {
			sendRich(telegramContext, chatID, "**"+tr(lang, "think.title")+"**\n\n"+th, nil)
		} else {
			sendMessage(chatID, tr(lang, "think.none"), nil)
		}
	case "/continue", "/retry":
		retryLastTask(chatID, uid)
	case "/cmd":
		return handleQuickCmd(chatID, uid, rest)
	case "/mode", "/approval", "/execution":
		return handleExecutionCommand(chatID, uid, rest)
	case "/settings":
		if !isAdmin(uid) {
			sendMessage(chatID, tr(lang, "err.admin_only"), nil)
			return true
		}
		sendUI(chatID, settingsText(uid), adminMenu(uid))
	case "/model", "/effort":
		return handleAdminCommand(chatID, uid, cmd, rest)
	case "/notice", "/notices", "/公告":
		text, kb := noticesPanel(uid)
		sendUI(chatID, text, kb)
	case "/about", "/support", "/支持":
		text, kb := aboutPanel(uid)
		sendUI(chatID, text, kb)
	case "/register", "/signup", "/注册":
		sendMessage(chatID, tr(lang, "reg.already_user"), nil)
	case "/admin", "/console", "/backend", "/后台", "/announce", "/发布公告", "/provider", "/persona", "/system",
		"/friend", "/ban", "/ws", "/workspace", "/cron", "/schedule", "/skills", "/health", "/runs", "/img", "/file":
		// Management lives in the server terminal: tg-agent admin.
		if isAdmin(uid) {
			sendUI(chatID, terminalHint(lang), nil)
		} else {
			sendMessage(chatID, tr(lang, "err.unknown_command"), nil)
		}
	default:
		if looksLikePath(cmd) {
			return false // e.g. "/etc/hosts 是什么" is a task, not a command
		}
		sendMessage(chatID, tr(lang, "err.unknown_command"), nil)
	}
	return true
}

func commandMutatesConfig(command, rest string) bool {
	switch command {
	case "/model", "/effort", "/mode", "/approval", "/execution":
		return rest != ""
	}
	return false
}

// terminalHint tells admins where management moved: the server terminal.
func terminalHint(lang string) string {
	return "<b>▍" + esc(tr(lang, "terminal.title")) + "</b>\n" + esc(tr(lang, "terminal.body")) + "\n\n<code>" + esc(adminCommandLine()) + "</code>"
}

func handleLangCommand(chatID, uid int64, rest string) {
	choice := strings.ToLower(strings.TrimSpace(rest))
	switch choice {
	case "":
		// Toggle between the two languages.
		choice = langZH
		if langFor(uid) == langZH {
			choice = langEN
		}
	case "zh", "cn", "中文":
		choice = langZH
	case "en", "english", "英文":
		choice = langEN
	case "auto":
		choice = ""
	default:
		sendMessage(chatID, tr(langFor(uid), "lang.usage"), nil)
		return
	}
	if err := updatePrefs(uid, func(p *UserPrefs) { p.Lang = choice }); err != nil {
		sendMessage(chatID, tr(langFor(uid), "err.save", err.Error()), nil)
		return
	}
	sendUI(chatID, esc(tr(langFor(uid), "lang.changed")), mainMenu(uid))
}

func handleStepsCommand(chatID, uid int64, rest string) {
	lang := langFor(uid)
	choice := strings.ToLower(strings.TrimSpace(rest))
	switch choice {
	case "":
		choice = "on"
		if stepsDetailed(uid, configSnapshot()) {
			choice = "off"
		}
	case "on", "off":
	case "default", "auto":
		choice = ""
	default:
		sendMessage(chatID, tr(lang, "steps.usage"), nil)
		return
	}
	if err := updatePrefs(uid, func(p *UserPrefs) { p.Steps = choice }); err != nil {
		sendMessage(chatID, tr(lang, "err.save", err.Error()), nil)
		return
	}
	key := "steps.off"
	if stepsDetailed(uid, configSnapshot()) {
		key = "steps.on"
	}
	sendMessage(chatID, tr(lang, key), nil)
}

func retryLastTask(chatID, uid int64) {
	lang := langFor(uid)
	v, ok := lastTasks.Load(chatID)
	if !ok {
		sendMessage(chatID, tr(lang, "task.no_last"), nil)
		return
	}
	task := v.(lastTask)
	if isBusy(chatID) {
		sendMessage(chatID, tr(lang, "task.busy"), nil)
		return
	}
	sendMessage(chatID, tr(lang, "task.rerun", oneLine(task.Text, 60)), nil)
	go runUserTask(chatID, uid, task.Text, task.Images)
}

// ---------------- quick commands ----------------

type QuickCmd struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

func findQuick(name string) (QuickCmd, bool) {
	for _, q := range configSnapshot().QuickCommands {
		if q.Name == name {
			return q, true
		}
	}
	return QuickCmd{}, false
}

func quickList(lang string) string {
	c := configSnapshot()
	if len(c.QuickCommands) == 0 {
		return tr(lang, "quick.empty")
	}
	var b strings.Builder
	for _, q := range c.QuickCommands {
		fmt.Fprintf(&b, "• /cmd %s — %s\n", q.Name, oneLine(q.Prompt, 70))
	}
	return b.String()
}

// handleQuickCmd runs a shortcut: /cmd <name> [extra]. Shortcuts are
// created and removed in the server terminal.
func handleQuickCmd(chatID, uid int64, rest string) bool {
	lang := langFor(uid)
	f := strings.Fields(rest)
	if len(f) == 0 {
		sendMessage(chatID, tr(lang, "quick.title")+"\n\n"+quickList(lang), nil)
		return true
	}
	if f[0] == "add" || f[0] == "del" {
		if isAdmin(uid) {
			sendUI(chatID, terminalHint(lang), nil)
		} else {
			sendMessage(chatID, tr(lang, "err.admin_only"), nil)
		}
		return true
	}
	if q, ok := findQuick(f[0]); ok {
		task := q.Prompt
		if extra := argsAfter(rest, 1); extra != "" {
			task += "\n\n" + extra
		}
		go runUserTask(chatID, uid, task, nil)
		return true
	}
	sendMessage(chatID, tr(lang, "quick.unknown")+"\n\n"+quickList(lang), nil)
	return true
}
