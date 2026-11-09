package main

import (
	"fmt"
	"strings"
)

// mainMenu is deliberately minimal. Everyone gets a new chat, display
// settings, help, announcements and the author page; admins also see usage,
// model, reasoning and settings. Everything else lives in the admin console.
func mainMenu(uid int64) map[string]interface{} {
	lang := langFor(uid)
	extra := []Btn{btn(tr(lang, "btn.notice"), "act:notice"), btn(tr(lang, "btn.about"), "act:about")}
	if !isAdmin(uid) {
		return keyboard([][]Btn{
			{btn(tr(lang, "btn.new"), "act:new"), btn(tr(lang, "btn.display"), "nav:display"), btn(tr(lang, "btn.help"), "act:help")},
			extra,
		})
	}
	return keyboard([][]Btn{
		{btn(tr(lang, "btn.new"), "act:new"), btn(tr(lang, "btn.usage"), "act:usage")},
		{btn(tr(lang, "btn.model"), "nav:model"), btn(tr(lang, "btn.effort"), "nav:effort")},
		{btn(tr(lang, "btn.display"), "nav:display"), btn(tr(lang, "btn.settings"), "nav:settings"), btn(tr(lang, "btn.help"), "act:help")},
		extra,
	})
}

func useMenu(uid int64) map[string]interface{} { return mainMenu(uid) }

func backKB(lang, target string) map[string]interface{} {
	return keyboard([][]Btn{{btn(tr(lang, "btn.back"), target)}})
}

// adminMenu is the settings page: only model, execution mode and reasoning.
// Everything else is in the admin console (/admin).
func adminMenu(uid int64) map[string]interface{} {
	if !isAdmin(uid) {
		return mainMenu(uid)
	}
	lang := langFor(uid)
	return keyboard([][]Btn{
		{btn(tr(lang, "btn.model"), "nav:model"), btn(tr(lang, "btn.mode"), "nav:execution"), btn(tr(lang, "btn.effort"), "nav:effort")},
		{btn(tr(lang, "btn.back"), "nav:root")},
	})
}

// settingsText shows the three settings with their current values.
func settingsText(uid int64) string {
	lang := langFor(uid)
	c := configSnapshot()
	var b strings.Builder
	b.WriteString("<b>▍" + esc(tr(lang, "settings.title")) + "</b>\n")
	fmt.Fprintf(&b, "%s <code>%s</code>\n", esc(tr(lang, "menu.model")), esc(c.Model))
	fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "btn.mode")), esc(modeLabel(lang, approvalMode(c))))
	fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "menu.effort")), esc(effortLabel(lang, effectiveEffort(c))))
	b.WriteString("\n<i>" + esc(tr(lang, "settings.hint")) + "</i>")
	return b.String()
}

// menuText is the HTML header of the main panel.
func menuText(uid int64) string {
	lang := langFor(uid)
	if !allowed(uid) {
		return "<b>" + esc(appName(lang)) + "</b>\n" + esc(tr(lang, "menu.unauthorized"))
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
	fmt.Fprintf(&b, "<b>▍%s</b> · %s\n\n", esc(appName(lang)), esc(state))
	fmt.Fprintf(&b, "%s <code>%s</code>\n", esc(tr(lang, "menu.model")), esc(c.Model))
	fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "menu.effort")), esc(effortLabel(lang, effectiveEffort(c))))
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
	b.WriteString("<b>▍" + esc(appName(lang)) + "</b>\n" + esc(tr(lang, "help.intro")) + "\n")
	section := func(title string, lines ...string) {
		b.WriteString("\n<b>" + esc(title) + "</b>\n")
		for _, l := range lines {
			cmd, desc, _ := strings.Cut(l, "|")
			b.WriteString("<code>" + esc(cmd) + "</code> " + esc(desc) + "\n")
		}
	}
	section(tr(lang, "help.basic"),
		"/menu|"+tr(lang, "help.menu"), "/new|"+tr(lang, "help.new"), "/stop|"+tr(lang, "help.stop"),
		"/continue|"+tr(lang, "help.continue"), "/show|"+tr(lang, "help.show"))
	section(tr(lang, "help.more"),
		"/notice|"+tr(lang, "help.notice"), "/about|"+tr(lang, "help.about"),
		"/memory|"+tr(lang, "help.memory"), "/lang zh|en|"+tr(lang, "help.lang"))
	if isAdmin(uid) {
		section(tr(lang, "help.admin"),
			"/settings|"+tr(lang, "help.settings"), "/usage|"+tr(lang, "help.usage"))
		b.WriteString("<i>" + esc(tr(lang, "help.terminal")) + "</i>\n<code>" + esc(adminCommandLine()) + "</code>\n")
	}
	b.WriteString("\n<i>" + esc(tr(lang, "help.tip")) + "</i>")
	return b.String()
}

func sendHelp(chatID, uid int64) { sendUI(chatID, helpBody(uid), mainMenu(uid)) }

func sendMenu(chatID, uid int64) { sendUI(chatID, menuText(uid), mainMenu(uid)) }
