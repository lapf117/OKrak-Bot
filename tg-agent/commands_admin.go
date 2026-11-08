package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// effortChoices are offered as buttons; /effort also accepts other lowercase
// provider-specific values. "off" omits reasoning_effort from requests.
var effortChoices = []string{"off", "minimal", "low", "medium", "high"}

// handleAdminCommand covers model, provider, prompt, people and workspace settings.
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
				c.Model = rest
				if persistUIConfig(chatID, c) {
					sendMessage(chatID, tr(lang, "model.switched", rest), nil)
				}
				return true
			}
		}
		sendMessage(chatID, tr(lang, "model.unknown", strings.Join(p.Models, ", ")), nil)
	case "/provider":
		handleProvider(chatID, uid, rest)
	case "/persona", "/system":
		field, key := &c.Persona, "persona"
		if cmd == "/system" {
			field, key = &c.SystemPrompt, "system"
		}
		switch {
		case rest == "":
			cur := *field
			if strings.TrimSpace(cur) == "" {
				cur = tr(lang, "prompt.unset")
			}
			sendMessage(chatID, tr(lang, key+".title")+"\n\n"+short(cur, 3200)+"\n\n"+tr(lang, key+".usage"), nil)
			return true
		case strings.EqualFold(rest, "clear"):
			*field = ""
		default:
			*field = rest
		}
		if persistUIConfig(chatID, c) {
			sendMessage(chatID, tr(lang, key+".updated"), nil)
		}
	case "/effort":
		if rest == "" {
			sendMessage(chatID, tr(lang, "effort.current", effortLabel(lang, c.ReasoningEffort))+"\n"+tr(lang, "effort.usage"), nil)
			return true
		}
		level := strings.ToLower(rest)
		if level == "off" || level == "default" || level == "none" {
			level = ""
		}
		if !validEffort(level) {
			sendMessage(chatID, tr(lang, "effort.usage"), nil)
			return true
		}
		c.ReasoningEffort = level
		if persistUIConfig(chatID, c) {
			sendMessage(chatID, tr(lang, "effort.updated", effortLabel(lang, level)), nil)
		}
	case "/admin", "/friend", "/ban":
		handleList(chatID, uid, rest, strings.TrimPrefix(cmd, "/"))
	case "/ws", "/workspace":
		handleWorkspace(chatID, uid, rest)
	}
	return true
}

func effortLabel(lang, effort string) string {
	if effort == "" {
		return tr(lang, "effort.default")
	}
	return effort
}

func handleProvider(chatID, uid int64, rest string) bool {
	lang := langFor(uid)
	c := configSnapshot()
	f := strings.Fields(rest)
	if len(f) == 0 {
		var b strings.Builder
		b.WriteString(tr(lang, "provider.title") + "\n\n")
		for _, p := range c.Provider {
			markText := "  "
			if p.Name == c.ProviderName {
				markText = "● "
			}
			fmt.Fprintf(&b, "%s%s  %s  (%d)\n", markText, p.Name, p.BaseURL, len(p.Models))
		}
		b.WriteString("\n" + tr(lang, "provider.usage"))
		sendMessage(chatID, b.String(), nil)
		return true
	}
	switch {
	case f[0] == "add" && len(f) >= 5:
		var models []string
		for _, m := range strings.Split(f[4], ",") {
			if m = strings.TrimSpace(m); m != "" {
				models = append(models, m)
			}
		}
		if len(models) == 0 {
			sendMessage(chatID, tr(lang, "provider.usage"), nil)
			return true
		}
		setProviderIn(&c, Provider{Name: f[1], BaseURL: f[2], APIKey: f[3], Models: models, DefaultModel: models[0]})
		if persistUIConfig(chatID, c) {
			sendMessage(chatID, tr(lang, "provider.added", f[1], len(models), f[1]), nil)
		}
		return true
	case f[0] == "del" && len(f) >= 2:
		if f[1] == c.ProviderName {
			sendMessage(chatID, tr(lang, "provider.del_current"), nil)
			return true
		}
		var keep []Provider
		for _, p := range c.Provider {
			if p.Name != f[1] {
				keep = append(keep, p)
			}
		}
		c.Provider = keep
		if persistUIConfig(chatID, c) {
			sendMessage(chatID, tr(lang, "provider.deleted", f[1]), nil)
		}
		return true
	}
	for _, p := range c.Provider {
		if p.Name == f[0] {
			selectProvider(&c, p)
			if persistUIConfig(chatID, c) {
				sendMessage(chatID, tr(lang, "provider.switched", p.Name, c.Model), nil)
			}
			return true
		}
	}
	sendMessage(chatID, tr(lang, "provider.unknown"), nil)
	return true
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

func handleList(chatID, uid int64, rest, kind string) bool {
	lang := langFor(uid)
	c := configSnapshot()
	var list *[]int64
	switch kind {
	case "admin":
		list = &c.Admins
	case "friend":
		list = &c.Friends
	default:
		kind, list = "ban", &c.Banned
	}
	title := tr(lang, "list."+kind)
	f := strings.Fields(rest)
	if len(f) == 0 {
		if len(*list) == 0 {
			sendMessage(chatID, tr(lang, "list.empty", title), nil)
			return true
		}
		var b strings.Builder
		b.WriteString(title + "\n\n")
		for _, id := range *list {
			fmt.Fprintf(&b, "• %d  %s  %s %s\n", id, userName(id), tr(lang, "users.last"), lastActive(id))
		}
		sendMessage(chatID, b.String(), nil)
		return true
	}
	if len(f) < 2 {
		sendMessage(chatID, tr(lang, "list.usage", kind), nil)
		return true
	}
	id, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || id <= 0 || id == c.OwnerID {
		sendMessage(chatID, tr(lang, "list.bad_id"), nil)
		return true
	}
	switch f[0] {
	case "add":
		if containsID(*list, id) {
			sendMessage(chatID, tr(lang, "list.exists"), nil)
			return true
		}
		*list = append(*list, id)
	case "rem", "del":
		*list = toggleID(*list, id, false)
	default:
		sendMessage(chatID, tr(lang, "list.usage", kind), nil)
		return true
	}
	if persistUIConfig(chatID, c) {
		sendMessage(chatID, tr(lang, "list.updated", title, id), nil)
	}
	return true
}

func handleWorkspace(chatID, uid int64, rest string) {
	lang := langFor(uid)
	c := configSnapshot()
	f := strings.Fields(rest)
	if len(f) == 0 {
		var b strings.Builder
		b.WriteString(tr(lang, "ws.title") + "\n\n" + tr(lang, "ws.current") + " " + c.Workspace + "\n\n")
		if len(c.Workspaces) == 0 {
			b.WriteString(tr(lang, "ws.empty") + "\n")
		} else {
			for i, w := range c.Workspaces {
				markText := "  "
				if w == c.Workspace {
					markText = "● "
				}
				fmt.Fprintf(&b, "%s%d. %s\n", markText, i+1, w)
			}
		}
		b.WriteString("\n" + tr(lang, "ws.usage"))
		sendMessage(chatID, b.String(), nil)
		return
	}
	switch f[0] {
	case "add":
		path := argsAfter(rest, 1)
		if path == "" || !strings.HasPrefix(path, "/") {
			sendMessage(chatID, tr(lang, "ws.usage"), nil)
			return
		}
		for _, w := range c.Workspaces {
			if w == path {
				sendMessage(chatID, tr(lang, "ws.exists"), nil)
				return
			}
		}
		c.Workspaces = append(c.Workspaces, path)
		if persistUIConfig(chatID, c) {
			sendMessage(chatID, tr(lang, "ws.added", path), nil)
		}
		return
	case "del":
		idx, err := strconv.Atoi(argsAfter(rest, 1))
		if err != nil || idx < 1 || idx > len(c.Workspaces) {
			sendMessage(chatID, tr(lang, "ws.bad_index"), nil)
			return
		}
		removed := c.Workspaces[idx-1]
		c.Workspaces = append(c.Workspaces[:idx-1], c.Workspaces[idx:]...)
		if persistUIConfig(chatID, c) {
			sendMessage(chatID, tr(lang, "ws.removed", removed), nil)
		}
		return
	}
	if isBusy(chatID) {
		sendMessage(chatID, tr(lang, "task.stop_first"), nil)
		return
	}
	target := strings.TrimSpace(rest)
	if idx, err := strconv.Atoi(target); err == nil && idx >= 1 && idx <= len(c.Workspaces) {
		target = c.Workspaces[idx-1]
	}
	if !strings.HasPrefix(target, "/") {
		sendMessage(chatID, tr(lang, "ws.absolute"), nil)
		return
	}
	if out, err := runShell("test -d "+shellQuote(target)+" && echo OK", 15*time.Second); err != nil || !strings.Contains(out, "OK") {
		sendMessage(chatID, tr(lang, "ws.missing", target), nil)
		return
	}
	c.Workspace = target
	if persistUIConfig(chatID, c) {
		resetHistory(chatID)
		sendMessage(chatID, tr(lang, "ws.switched", target), nil)
	}
}
