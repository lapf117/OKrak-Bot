package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
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
	case "/cron", "/schedule":
		return handleCron(chatID, uid, rest)
	case "/skills":
		return handleSkillsCommand(chatID, uid, rest)
	case "/mode", "/approval", "/execution":
		return handleExecutionCommand(chatID, uid, rest)
	case "/settings":
		if !isAdmin(uid) {
			sendMessage(chatID, tr(lang, "err.admin_only"), nil)
			return true
		}
		sendUI(chatID, "<b>"+esc(tr(lang, "settings.title"))+"</b>\n\n"+esc(tr(lang, "settings.hint")), adminMenu(uid))
	case "/health":
		if !isAdmin(uid) {
			sendMessage(chatID, tr(lang, "err.admin_only"), nil)
			return true
		}
		sendUI(chatID, healthCheck(lang), nil)
	case "/runs":
		if !isAdmin(uid) {
			sendMessage(chatID, tr(lang, "err.admin_only"), nil)
			return true
		}
		sendUI(chatID, listRunsHTML(lang, 10), runsKeyboard(lang, 5))
	case "/img", "/file":
		return handleGetFile(chatID, uid, cmd, rest)
	case "/model", "/provider", "/persona", "/system", "/effort", "/admin", "/friend", "/ban", "/ws", "/workspace":
		return handleAdminCommand(chatID, uid, cmd, rest)
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
		sendMessage(chatID, tr(langFor(chatID), "err.save", redactSecrets(err.Error())), nil)
		return false
	}
	return true
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

func handleGetFile(chatID, uid int64, cmd, rest string) bool {
	lang := langFor(uid)
	path := strings.TrimSpace(rest)
	if path == "" {
		sendMessage(chatID, tr(lang, "file.usage", cmd), nil)
		return true
	}
	if !isAdmin(uid) {
		sendMessage(chatID, tr(lang, "err.admin_only"), nil)
		return true
	}
	if !strings.HasPrefix(path, "/") {
		path = absPath(path) // relative to the workspace
	}
	data, err := readDeliverable(path)
	if err != nil {
		sendMessage(chatID, tr(lang, "file.error", err.Error()), nil)
		return true
	}
	if !sendByExt(chatID, path, data) {
		sendMessage(chatID, tr(lang, "file.send_failed"), nil)
	}
	return true
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

func handleQuickCmd(chatID, uid int64, rest string) bool {
	lang := langFor(uid)
	c := configSnapshot()
	f := strings.Fields(rest)
	if len(f) == 0 {
		sendMessage(chatID, tr(lang, "quick.title")+"\n\n"+quickList(lang)+"\n"+tr(lang, "quick.help"), nil)
		return true
	}
	switch f[0] {
	case "add":
		if !isAdmin(uid) {
			sendMessage(chatID, tr(lang, "err.admin_only"), nil)
			return true
		}
		prompt := argsAfter(rest, 2)
		if len(f) < 3 || prompt == "" {
			sendMessage(chatID, tr(lang, "quick.usage"), nil)
			return true
		}
		name := f[1]
		var keep []QuickCmd
		for _, q := range c.QuickCommands {
			if q.Name != name {
				keep = append(keep, q)
			}
		}
		c.QuickCommands = append(keep, QuickCmd{Name: name, Prompt: prompt})
		if persistUIConfig(chatID, c) {
			sendMessage(chatID, tr(lang, "quick.added", name, name), nil)
		}
		return true
	case "del":
		if !isAdmin(uid) {
			sendMessage(chatID, tr(lang, "err.admin_only"), nil)
			return true
		}
		if len(f) < 2 {
			sendMessage(chatID, tr(lang, "quick.usage"), nil)
			return true
		}
		var keep []QuickCmd
		for _, q := range c.QuickCommands {
			if q.Name != f[1] {
				keep = append(keep, q)
			}
		}
		c.QuickCommands = keep
		if persistUIConfig(chatID, c) {
			sendMessage(chatID, tr(lang, "quick.deleted", f[1]), nil)
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

// ---------------- /cron ----------------

func cronUsage(lang string) string { return tr(lang, "cron.usage") }

func handleCron(chatID, uid int64, rest string) bool {
	lang := langFor(uid)
	if !isAdmin(uid) {
		sendMessage(chatID, tr(lang, "err.admin_only"), nil)
		return true
	}
	f := strings.Fields(rest)
	if len(f) == 0 || f[0] == "list" {
		sendUI(chatID, cronPanel(lang), cronKeyboard(lang))
		return true
	}
	switch f[0] {
	case "add":
		spec, used, err := parseScheduleFields(f[1:], time.Now(), scheduleLocation(configSnapshot()))
		if err != nil {
			sendMessage(chatID, err.Error()+"\n\n"+cronUsage(lang), nil)
			return true
		}
		prompt := argsAfter(rest, 1+used)
		job, err := addCronJob(chatID, uid, spec, prompt, "user")
		if err != nil {
			sendMessage(chatID, err.Error(), nil)
			return true
		}
		sendMessage(chatID, tr(lang, "cron.created", job.ID, spec.describe(lang), formatRunTime(job.NextRun), oneLine(prompt, 60)), nil)
	case "del", "rm", "pause", "resume", "run":
		if len(f) < 2 {
			sendMessage(chatID, cronUsage(lang), nil)
			return true
		}
		id, convErr := strconv.Atoi(strings.TrimPrefix(f[1], "#"))
		if convErr != nil {
			sendMessage(chatID, cronUsage(lang), nil)
			return true
		}
		var err error
		switch f[0] {
		case "del", "rm":
			err = deleteCronJob(id, 0)
		case "pause":
			err = setCronEnabled(id, 0, false)
		case "resume":
			err = setCronEnabled(id, 0, true)
		case "run":
			err = runCronNow(id)
		}
		if err != nil {
			sendMessage(chatID, err.Error(), nil)
			return true
		}
		sendMessage(chatID, tr(lang, "cron.updated", id), nil)
	default:
		sendMessage(chatID, cronUsage(lang), nil)
	}
	return true
}

func cronPanel(lang string) string {
	list := listCronText(lang, 0)
	if list == "" {
		list = tr(lang, "cron.empty")
	}
	return "<b>" + esc(tr(lang, "cron.title")) + "</b>\n\n" + esc(list) + "\n<i>" + esc(cronUsage(lang)) + "</i>"
}

func cronKeyboard(lang string) interface{} {
	var rows [][]Btn
	for i, j := range cronSnapshot() {
		if i >= 8 {
			break
		}
		id := strconv.Itoa(j.ID)
		row := []Btn{btn("#"+id, "cron:run:"+id)}
		switch {
		case j.Done:
		case j.Enabled:
			row = append(row, btn("#"+id, "cron:pause:"+id))
		default:
			row = append(row, btn(tr(lang, "cron.resume")+" #"+id, "cron:resume:"+id))
		}
		rows = append(rows, append(row, btn("#"+id, "cron:del:"+id)))
	}
	rows = append(rows, []Btn{btn(tr(lang, "btn.refresh"), "act:cron"), btn(tr(lang, "btn.back"), "nav:root")})
	return keyboard(rows)
}
