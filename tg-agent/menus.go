package main

import (
	"fmt"
	"strings"
)

// mainMenu is deliberately minimal: a regular user only clears the
// conversation and reads help. Model and reasoning choices, and the backend
// settings, are shown to admins only.
func mainMenu(uid int64) map[string]interface{} {
	lang := langFor(uid)
	if !isAdmin(uid) {
		return keyboard([][]Btn{{btn(tr(lang, "btn.new"), "act:new"), btn(tr(lang, "btn.help"), "act:help")}})
	}
	return keyboard([][]Btn{
		{btn(tr(lang, "btn.new"), "act:new")},
		{btn(tr(lang, "btn.model"), "nav:model"), btn(tr(lang, "btn.effort"), "nav:effort")},
		{btn(tr(lang, "btn.settings"), "nav:settings"), btn(tr(lang, "btn.help"), "act:help")},
	})
}

func useMenu(uid int64) map[string]interface{} { return mainMenu(uid) }

func backKB(lang, target string) map[string]interface{} {
	return keyboard([][]Btn{{btn(tr(lang, "btn.back"), target)}})
}

func adminMenu(uid int64) map[string]interface{} {
	if !isAdmin(uid) {
		return mainMenu(uid)
	}
	lang := langFor(uid)
	steps := tr(lang, "btn.steps_off")
	if stepsDetailed(uid, configSnapshot()) {
		steps = tr(lang, "btn.steps_on")
	}
	return keyboard([][]Btn{
		{btn(tr(lang, "btn.model"), "nav:model"), btn(tr(lang, "btn.mode"), "nav:execution")},
		{btn(tr(lang, "btn.workspace"), "nav:workspace"), btn(tr(lang, "btn.effort"), "nav:effort")},
		{btn(tr(lang, "btn.cron"), "act:cron"), btn(tr(lang, "btn.quick"), "act:quick")},
		{btn(steps, "act:steps"), btn(tr(lang, "btn.advanced"), "nav:advanced")},
		{btn(tr(lang, "btn.back"), "nav:root")},
	})
}

func advancedMenu(uid int64) map[string]interface{} {
	if !isAdmin(uid) {
		return mainMenu(uid)
	}
	lang := langFor(uid)
	return keyboard([][]Btn{
		{btn(tr(lang, "btn.provider"), "nav:provider"), btn(tr(lang, "btn.system"), "nav:system")},
		{btn(tr(lang, "btn.persona"), "nav:persona"), btn(tr(lang, "btn.users"), "nav:users")},
		{btn(tr(lang, "btn.health"), "act:health"), btn(tr(lang, "btn.log"), "act:log")},
		{btn(tr(lang, "btn.config"), "act:config"), btn(tr(lang, "btn.back"), "nav:settings")},
	})
}

// menuText is the HTML header of the main panel.
func menuText(uid int64) string {
	lang := langFor(uid)
	if !allowed(uid) {
		return "<b>" + esc(tr(lang, "app.name")) + "</b>\n" + esc(tr(lang, "menu.unauthorized"))
	}
	c := configSnapshot()
	state := tr(lang, "menu.idle")
	if elapsed, origin, ok := runningTask(uid); ok {
		state = tr(lang, "menu.running", clock(elapsed))
		if origin != "" {
			state += " · " + origin
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b> · %s\n\n", esc(tr(lang, "app.name")), esc(state))
	fmt.Fprintf(&b, "%s <code>%s</code>\n", esc(tr(lang, "menu.model")), esc(c.Model))
	fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "menu.effort")), esc(effortLabel(lang, c.ReasoningEffort)))
	// The execution mode is a backend concern; only admins see it.
	if isAdmin(uid) {
		fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "menu.mode")), esc(modeLabel(lang, approvalMode(c))))
	}
	b.WriteString(esc(tr(lang, "menu.turns", historyTurns(uid))))
	if tok := lastUsageTokens(uid); tok > 0 {
		fmt.Fprintf(&b, " · %s tokens", esc(humanTokens(tok)))
	}
	b.WriteString("\n\n<i>" + esc(tr(lang, "menu.hint")) + "</i>")
	return b.String()
}

func helpBody(uid int64) string {
	lang := langFor(uid)
	var b strings.Builder
	b.WriteString("<b>" + esc(tr(lang, "app.name")) + "</b>\n" + esc(tr(lang, "help.intro")) + "\n")
	section := func(title string, lines ...string) {
		b.WriteString("\n<b>" + esc(title) + "</b>\n")
		for _, l := range lines {
			cmd, desc, _ := strings.Cut(l, "|")
			b.WriteString("<code>" + esc(cmd) + "</code> " + esc(desc) + "\n")
		}
	}
	section(tr(lang, "help.basic"),
		"/menu|"+tr(lang, "help.menu"), "/new|"+tr(lang, "help.new"), "/stop|"+tr(lang, "help.stop"),
		"/status|"+tr(lang, "help.status"), "/continue|"+tr(lang, "help.continue"))
	section(tr(lang, "help.more"),
		"/memory|"+tr(lang, "help.memory"), "/skills|"+tr(lang, "help.skills"), "/export|"+tr(lang, "help.export"),
		"/lang zh|en|"+tr(lang, "help.lang"), "/steps on|off|"+tr(lang, "help.steps"), "/cmd|"+tr(lang, "help.cmd"))
	if isAdmin(uid) {
		section(tr(lang, "help.admin"),
			"/settings|"+tr(lang, "help.settings"), "/mode|"+tr(lang, "help.mode"), "/cron|"+tr(lang, "help.cron"),
			"/runs|"+tr(lang, "help.runs"), "/file|"+tr(lang, "help.file"), "/health|"+tr(lang, "help.health"))
	}
	b.WriteString("\n<i>" + esc(tr(lang, "help.tip")) + "</i>")
	return b.String()
}

func sendHelp(chatID, uid int64) { sendUI(chatID, helpBody(uid), mainMenu(uid)) }

func sendMenu(chatID, uid int64) { sendUI(chatID, menuText(uid), mainMenu(uid)) }
