package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func settingCallback(kind, value string) string {
	sum := sha256.Sum256([]byte(value))
	return "set:" + kind + ":" + hex.EncodeToString(sum[:8])
}

// movedToTerminal reports buttons of earlier versions whose functions now
// live in the server terminal (tg-agent admin): users, registrations,
// announcements, name, providers, prompts, workspace, schedules, shortcuts,
// skills, health, logs, config and run logs.
func movedToTerminal(data string) bool {
	for _, prefix := range []string{"adm:", "reg:", "regp:", "ntc:", "user:", "cron:", "runlog:", "set:provider:", "set:admin:", "set:friend:", "set:ban:", "unset:"} {
		if strings.HasPrefix(data, prefix) {
			return true
		}
	}
	switch data {
	case "nav:advanced", "nav:users", "nav:allusers", "nav:bans", "nav:provider", "nav:system", "nav:persona",
		"nav:workspace", "nav:skills", "act:cron", "act:quick", "act:health", "act:log", "act:config", "act:runs":
		return true
	}
	return false
}

// handleCallback routes inline keyboard presses. Telegram only carries the
// user-facing menus and the admin settings (model, execution mode,
// reasoning); everything else is managed in the server terminal.
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
	touchUser(cb.From)
	chatID, msgID, uid, data := cb.Message.Chat.ID, cb.Message.MessageID, cb.From.ID, cb.Data
	lang := langFor(uid)
	if data == "reg:apply" { // a visitor applies from the sign-up page
		answerCallback(cb.ID)
		submitRegistration(chatID, cb.From, "")
		// Drop the button from the welcome message whatever the outcome.
		if allowed(uid) {
			editUI(chatID, msgID, "<b>▍"+esc(appName(lang))+"</b>\n"+esc(tr(lang, "reg.intro")), nil)
		} else {
			editUI(chatID, msgID, guestWelcome(uid), nil)
		}
		return
	}
	if !allowed(uid) || msgID == 0 {
		answerCallback(cb.ID)
		return
	}
	if strings.HasPrefix(data, "set:") {
		uiMu.Lock()
		defer uiMu.Unlock()
	}
	admin := isAdmin(uid)
	toast := ""
	defer func() { answerCallbackText(cb.ID, toast) }()
	needAdmin := func() bool {
		if !admin {
			toast = tr(lang, "err.admin_only")
		}
		return admin
	}
	c := configSnapshot()

	switch {
	// ---------- everyone ----------
	case data == "nav:root", data == "nav:use", data == "act:status":
		editUI(chatID, msgID, menuText(uid), mainMenu(uid))
	case data == "act:help":
		editUI(chatID, msgID, helpBody(uid), mainMenu(uid))
	case data == "act:new":
		if isBusy(chatID) {
			toast = tr(lang, "task.stop_first")
			return
		}
		resetHistory(chatID)
		toast = tr(lang, "chat.reset")
		sendUI(chatID, esc(tr(lang, "chat.reset"))+"\n\n"+menuText(uid), mainMenu(uid))
	case data == "act:stop":
		// The dispatcher already cancelled the task; the panel updates itself.
		toast = tr(lang, "task.stopping")
	case data == "act:retry": // buttons on answers sent by earlier versions
		retryLastTask(chatID, uid)
	case data == "nav:display", strings.HasPrefix(data, "show:"):
		toast = handleDisplayCallback(chatID, msgID, uid, data)
	case data == "act:notice":
		text, kb := noticesPanel(uid)
		editUI(chatID, msgID, text, kb)
	case data == "act:about":
		text, kb := aboutPanel(uid)
		editUI(chatID, msgID, text, kb)
	case data == "act:lang":
		next := langEN
		if lang == langEN {
			next = langZH
		}
		if err := updatePrefs(uid, func(p *UserPrefs) { p.Lang = next }); err != nil {
			toast = tr(lang, "err.save", err.Error())
			return
		}
		toast = tr(next, "lang.changed")
		editUI(chatID, msgID, menuText(uid), mainMenu(uid))
	case data == "act:usage":
		editUI(chatID, msgID, usagePanel(uid, lang), usageMenu(lang, admin))
	case data == "act:memory":
		editUI(chatID, msgID, memoryPanel(uid, lang), memoryMenu(lang))
	case data == "memory:clear":
		if err := clearMemory(uid); err != nil {
			toast = tr(lang, "err.save", err.Error())
			return
		}
		toast = tr(lang, "memory.cleared")
		editUI(chatID, msgID, memoryPanel(uid, lang), memoryMenu(lang))
	case data == "act:steps":
		next := "on"
		if stepsDetailed(uid, c) {
			next = "off"
		}
		_ = updatePrefs(uid, func(p *UserPrefs) { p.Steps = next })
		toast = tr(lang, "steps."+next)
		if admin {
			editUI(chatID, msgID, settingsText(uid), adminMenu(uid))
		}

	// ---------- admin settings: model, execution mode, reasoning ----------
	case data == "nav:admin", data == "nav:settings":
		if needAdmin() {
			editUI(chatID, msgID, settingsText(uid), adminMenu(uid))
		}
	case data == "nav:effort":
		if !needAdmin() {
			return
		}
		text, kb := effortPanel(lang, c)
		editUI(chatID, msgID, text, kb)
	case strings.HasPrefix(data, "set:effort:"):
		if !needAdmin() {
			return
		}
		level := strings.TrimPrefix(data, "set:effort:")
		if opts, _ := effortOptions(c); level != "off" && !containsString(opts, level) {
			toast = tr(lang, "effort.unsupported", c.Model, strings.Join(append([]string{"off"}, opts...), " | "))
			return
		}
		if level == "off" {
			level = ""
		}
		if err := updateConfig(func(next *Config) { next.ReasoningEffort = level }); err != nil {
			toast = tr(lang, "err.save", redactSecrets(err.Error()))
			return
		}
		toast = tr(lang, "effort.updated", effortLabel(lang, level))
		editUI(chatID, msgID, menuText(uid), mainMenu(uid))
	case data == "nav:model":
		if !needAdmin() {
			return
		}
		p := currentProvider()
		if p == nil {
			toast = tr(lang, "provider.none")
			return
		}
		var rows [][]Btn
		for _, m := range p.Models {
			rows = append(rows, []Btn{btn(mark(c.Model, short(m, 40)), settingCallback("model", m))})
		}
		rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:settings")})
		editUI(chatID, msgID, "<b>"+esc(tr(lang, "model.pick"))+"</b>", keyboard(rows))
	case strings.HasPrefix(data, "set:model:"):
		if !needAdmin() {
			return
		}
		if p := currentProvider(); p != nil {
			for _, m := range p.Models {
				if data == settingCallback("model", m) {
					if err := updateConfig(func(next *Config) { next.Model = m }); err != nil {
						toast = tr(lang, "err.save", redactSecrets(err.Error()))
						return
					}
					toast = tr(lang, "model.switched", m)
					editUI(chatID, msgID, menuText(uid), mainMenu(uid))
					return
				}
			}
		}

	// ---------- moved to the server terminal ----------
	case movedToTerminal(data):
		if needAdmin() {
			toast = tr(lang, "terminal.moved")
		}
	}
}
