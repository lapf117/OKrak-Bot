package main

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The admin console (/admin) is the bot's backend: users and registrations,
// announcements, name and author, providers, prompts, workspace, schedules,
// shortcuts, skills and diagnostics. None of it appears in user menus.

// ---------------- name, author and support ----------------

// appName is the bot's display name: bot_name from config, else the built-in name.
func appName(lang string) string {
	if name := strings.TrimSpace(configSnapshot().BotName); name != "" {
		return name
	}
	return tr(lang, "app.name")
}

func validLinkURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https":
		return u.Host != ""
	case "tg":
		return u.Opaque != "" || u.Host != ""
	}
	return false
}

// aboutPanel is the "Support the author" page shown to everyone.
func aboutPanel(uid int64) (string, map[string]interface{}) {
	lang := langFor(uid)
	c := configSnapshot()
	var b strings.Builder
	b.WriteString("<b>▍" + esc(tr(lang, "about.title")) + "</b>\n")
	head := "<b>" + esc(appName(lang)) + "</b>"
	if author := strings.TrimSpace(c.Author); author != "" {
		head += " · " + esc(tr(lang, "about.author", author))
	}
	b.WriteString(head + "\n\n")
	if text := strings.TrimSpace(c.SupportText); text != "" {
		b.WriteString(markdownToHTML(text))
	} else {
		b.WriteString(esc(tr(lang, "about.thanks")))
	}
	if isAdmin(uid) && strings.TrimSpace(c.Author) == "" && strings.TrimSpace(c.SupportText) == "" {
		b.WriteString("\n\n<i>" + esc(tr(lang, "about.admin_hint")) + "</i>")
	}
	var rows [][]Btn
	if link := strings.TrimSpace(c.SupportURL); link != "" && validLinkURL(link) {
		rows = append(rows, []Btn{{"text": tr(lang, "about.open"), "url": link}})
	}
	rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:root")})
	return b.String(), keyboard(rows)
}

// ---------------- typed input after a backend button ----------------

// An admin taps e.g. "Edit name" and then simply sends the new value. The
// request expires after a few minutes; any command cancels it.
const adminInputTTL = 5 * time.Minute

type adminInput struct {
	Kind    string // announce | name | author | support | link
	Expires time.Time
}

var adminInputs = struct {
	sync.Mutex
	m map[int64]adminInput
}{m: map[int64]adminInput{}}

func expectAdminInput(uid int64, kind string) {
	adminInputs.Lock()
	defer adminInputs.Unlock()
	adminInputs.m[uid] = adminInput{Kind: kind, Expires: time.Now().Add(adminInputTTL)}
}

// pendingAdminInput returns the awaited input kind, dropping expired ones.
func pendingAdminInput(uid int64) (string, bool) {
	adminInputs.Lock()
	defer adminInputs.Unlock()
	in, ok := adminInputs.m[uid]
	if !ok {
		return "", false
	}
	if time.Now().After(in.Expires) {
		delete(adminInputs.m, uid)
		return "", false
	}
	return in.Kind, true
}

func cancelAdminInput(uid int64) bool {
	adminInputs.Lock()
	defer adminInputs.Unlock()
	_, ok := adminInputs.m[uid]
	delete(adminInputs.m, uid)
	return ok
}

// handlePendingInput consumes a message an admin sends after tapping an
// input button. It returns false when the message should be handled normally.
func handlePendingInput(chatID, uid int64, text, low string) bool {
	kind, ok := pendingAdminInput(uid)
	if !ok || !isAdmin(uid) {
		return false
	}
	lang := langFor(uid)
	if low == "/cancel" || low == "取消" {
		cancelAdminInput(uid)
		sendMessage(chatID, tr(lang, "input.cancelled"), nil)
		return true
	}
	if strings.HasPrefix(low, "/") {
		cancelAdminInput(uid) // another command: forget the request and run it
		return false
	}
	cancelAdminInput(uid)
	switch kind {
	case "announce":
		showNoticeDraft(chatID, uid, text)
	case "name", "author", "support", "link":
		applyBrandInput(chatID, uid, kind, text)
	}
	return true
}

// applyBrandInput stores a new name, author, support text or link; "-" clears it.
func applyBrandInput(chatID, uid int64, kind, value string) {
	lang := langFor(uid)
	value = strings.TrimSpace(value)
	if value == "-" || value == "清空" || strings.EqualFold(value, "clear") {
		value = ""
	}
	uiMu.Lock()
	err := updateConfig(func(c *Config) {
		switch kind {
		case "name":
			c.BotName = value
		case "author":
			c.Author = value
		case "support":
			c.SupportText = value
		case "link":
			c.SupportURL = value
		}
	})
	uiMu.Unlock()
	if err != nil {
		sendMessage(chatID, tr(lang, "err.save", err.Error()), nil)
		return
	}
	text, kb := brandPanel(lang)
	sendUI(chatID, "<b>"+esc(tr(lang, "toast.saved"))+"</b>\n\n"+text, kb)
}

// ---------------- console panels ----------------

func registrationLabel(lang, mode string) string { return tr(lang, "reg.mode."+mode) }

func userCount(c Config) int {
	ids := map[int64]bool{c.OwnerID: true}
	for _, list := range [][]int64{c.Admins, c.Friends} {
		for _, id := range list {
			if !containsID(c.Banned, id) {
				ids[id] = true
			}
		}
	}
	return len(ids)
}

func consoleText(lang string) string {
	c := configSnapshot()
	var b strings.Builder
	b.WriteString("<b>▍" + esc(tr(lang, "console.title")) + "</b> · " + esc(appName(lang)) + "\n")
	fmt.Fprintf(&b, "%s\n", esc(tr(lang, "console.summary", userCount(c), len(pendingRegistrations()), len(listNotices()))))
	fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "console.registration")), esc(registrationLabel(lang, registrationMode(c))))
	fmt.Fprintf(&b, "%s <code>%s</code> · %s\n", esc(tr(lang, "menu.model")), esc(c.Model), esc(modeLabel(lang, approvalMode(c))))
	b.WriteString("\n<i>" + esc(tr(lang, "console.hint")) + "</i>\n")
	b.WriteString("<i>" + esc(tr(lang, "console.commands")) + "</i>")
	return b.String()
}

func consoleMenu(lang string) map[string]interface{} {
	reg := tr(lang, "btn.registrations")
	if n := len(pendingRegistrations()); n > 0 {
		reg += fmt.Sprintf(" · %d", n)
	}
	return keyboard([][]Btn{
		{btn(tr(lang, "btn.users"), "nav:users"), btn(reg, "adm:reg")},
		{btn(tr(lang, "btn.announce"), "adm:notice"), btn(tr(lang, "btn.brand"), "adm:brand")},
		{btn(tr(lang, "btn.provider"), "nav:provider"), btn(tr(lang, "btn.workspace"), "nav:workspace")},
		{btn(tr(lang, "btn.system"), "nav:system"), btn(tr(lang, "btn.persona"), "nav:persona")},
		{btn(tr(lang, "btn.cron"), "act:cron"), btn(tr(lang, "btn.quick"), "act:quick"), btn(tr(lang, "btn.skills"), "nav:skills")},
		{btn(tr(lang, "btn.health"), "act:health"), btn(tr(lang, "btn.log"), "act:log"), btn(tr(lang, "btn.config"), "act:config")},
		{btn(tr(lang, "btn.back"), "nav:root")},
	})
}

func sendConsole(chatID, uid int64) {
	lang := langFor(uid)
	if !isAdmin(uid) {
		sendMessage(chatID, tr(lang, "err.admin_only"), nil)
		return
	}
	sendUI(chatID, consoleText(lang), consoleMenu(lang))
}

func brandPanel(lang string) (string, map[string]interface{}) {
	c := configSnapshot()
	value := func(v string, n int) string {
		if strings.TrimSpace(v) == "" {
			return "<i>" + esc(tr(lang, "config.unset")) + "</i>"
		}
		return esc(short(oneLine(v, n), n))
	}
	name := value(c.BotName, 64)
	if strings.TrimSpace(c.BotName) == "" {
		name = esc(tr(lang, "app.name")) + " <i>" + esc(tr(lang, "brand.default")) + "</i>"
	}
	var b strings.Builder
	b.WriteString("<b>▍" + esc(tr(lang, "btn.brand")) + "</b>\n")
	fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "brand.name")), name)
	fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "brand.author")), value(c.Author, 64))
	fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "brand.support")), value(c.SupportText, 80))
	fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "brand.link")), value(c.SupportURL, 80))
	b.WriteString("\n<i>" + esc(tr(lang, "brand.hint")) + "</i>")
	return b.String(), keyboard([][]Btn{
		{btn(tr(lang, "brand.edit_name"), "adm:in:name"), btn(tr(lang, "brand.edit_author"), "adm:in:author")},
		{btn(tr(lang, "brand.edit_support"), "adm:in:support"), btn(tr(lang, "brand.edit_link"), "adm:in:link")},
		{btn(tr(lang, "btn.back"), "adm:root")},
	})
}

// handleConsoleCallback serves adm:* buttons. It returns the toast text.
func handleConsoleCallback(chatID, msgID, uid int64, data string) string {
	lang := langFor(uid)
	switch {
	case data == "adm:root" || data == "nav:advanced":
		editUI(chatID, msgID, consoleText(lang), consoleMenu(lang))
	case data == "adm:brand":
		text, kb := brandPanel(lang)
		editUI(chatID, msgID, text, kb)
	case strings.HasPrefix(data, "adm:in:"):
		kind := strings.TrimPrefix(data, "adm:in:")
		if !map[string]bool{"announce": true, "name": true, "author": true, "support": true, "link": true}[kind] {
			return ""
		}
		expectAdminInput(uid, kind)
		sendUI(chatID, "<b>▍"+esc(tr(lang, "input.title."+kind))+"</b>\n"+esc(tr(lang, "input.prompt."+kind))+"\n\n<i>"+esc(tr(lang, "input.cancel_hint"))+"</i>", nil)
		return tr(lang, "input.waiting")
	case data == "adm:notice":
		text, kb := noticeAdminPanel(lang)
		editUI(chatID, msgID, text, kb)
	case data == "adm:reg":
		text, kb := registrationPanel(lang)
		editUI(chatID, msgID, text, kb)
	}
	return ""
}
