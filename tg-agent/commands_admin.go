package main

import (
	"fmt"
	"strings"
)

// handleAdminCommand covers the Telegram settings commands /model and /effort.
func handleAdminCommand(chatID, uid int64, cmd, rest string) bool {
	lang := langFor(uid)
	c := configSnapshot()
	if cmd == "/model" && rest == "" {
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s\n", tr(lang, "menu.model"), c.Model)
		if p := currentProvider(); p != nil && len(p.Models) > 0 {
			b.WriteString("\n" + tr(lang, "model.available") + "\n")
			for _, m := range p.Models {
				b.WriteString("• " + m + "\n")
			}
		}
		b.WriteString("\n" + tr(lang, "model.usage"))
		sendMessage(chatID, b.String(), nil)
		return true
	}
	if !isAdmin(uid) {
		sendMessage(chatID, tr(lang, "err.admin_only"), nil)
		return true
	}
	switch cmd {
	case "/model":
		p := currentProvider()
		if p == nil {
			sendMessage(chatID, tr(lang, "provider.none"), nil)
			return true
		}
		for _, m := range p.Models {
			if m == rest {
				if err := updateConfig(func(next *Config) { next.Model = rest }); err != nil {
					sendMessage(chatID, tr(lang, "err.save", redactSecrets(err.Error())), nil)
					return true
				}
				sendMessage(chatID, tr(lang, "model.switched", rest), nil)
				return true
			}
		}
		sendMessage(chatID, tr(lang, "model.unknown", strings.Join(p.Models, ", ")), nil)
	case "/effort":
		if rest == "" {
			text, kb := effortPanel(lang, c)
			sendUI(chatID, text, kb)
			return true
		}
		level := strings.ToLower(strings.TrimSpace(rest))
		if level == "off" || level == "default" {
			level = ""
		}
		if !validEffort(level) {
			sendMessage(chatID, effortUsage(lang, c), nil)
			return true
		}
		// Known providers/models only take the values they list.
		if opts, src := effortOptions(c); level != "" && src != effortUnknown && !containsString(opts, level) {
			sendMessage(chatID, tr(lang, "effort.unsupported", c.Model, strings.Join(append([]string{"off"}, opts...), " | ")), nil)
			return true
		}
		if err := updateConfig(func(next *Config) { next.ReasoningEffort = level }); err != nil {
			sendMessage(chatID, tr(lang, "err.save", redactSecrets(err.Error())), nil)
			return true
		}
		sendMessage(chatID, tr(lang, "effort.updated", effortLabel(lang, level)), nil)
	}
	return true
}

func effortLabel(lang, effort string) string {
	if effort == "" {
		return tr(lang, "effort.default")
	}
	return effort
}

func selectProvider(c *Config, p Provider) {
	c.ProviderName = p.Name
	if len(p.Models) > 0 {
		c.Model = p.DefaultModel
		if c.Model == "" {
			c.Model = p.Models[0]
		}
	}
}
