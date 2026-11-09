package main

import (
	"net/url"
	"strings"
)

// Name, author and the "Support the author" page. The values are set in the
// server terminal (tg-agent admin → 名称与作者) or in config.json.

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
		b.WriteString("\n\n<i>" + esc(tr(lang, "about.admin_hint")) + "</i>\n<code>" + esc(adminCommandLine()) + "</code>")
	}
	var rows [][]Btn
	if link := strings.TrimSpace(c.SupportURL); link != "" && validLinkURL(link) {
		rows = append(rows, []Btn{{"text": tr(lang, "about.open"), "url": link}})
	}
	rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:root")})
	return b.String(), keyboard(rows)
}
