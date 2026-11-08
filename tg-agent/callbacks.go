package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func settingCallback(kind, value string) string {
	sum := sha256.Sum256([]byte(value))
	return "set:" + kind + ":" + hex.EncodeToString(sum[:8])
}

var runNameRe = regexp.MustCompile(`^[0-9.\-]+\.md$`)

// handleCallback routes inline keyboard presses.
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
	if !allowed(uid) || msgID == 0 {
		answerCallback(cb.ID)
		return
	}
	if strings.HasPrefix(data, "set:") || strings.HasPrefix(data, "unset:") {
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
	case data == "act:retry":
		retryLastTask(chatID, uid)
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
			editUI(chatID, msgID, "<b>"+esc(tr(lang, "settings.title"))+"</b>\n\n"+esc(tr(lang, "settings.hint")), adminMenu(uid))
		}
	case data == "nav:skills":
		editUI(chatID, msgID, skillsPanel(lang), backKB(lang, "nav:root"))

	// ---------- admin panels ----------
	case data == "nav:admin", data == "nav:settings":
		if needAdmin() {
			editUI(chatID, msgID, "<b>"+esc(tr(lang, "settings.title"))+"</b>\n\n"+esc(tr(lang, "settings.hint")), adminMenu(uid))
		}
	case data == "nav:advanced":
		if needAdmin() {
			editUI(chatID, msgID, "<b>"+esc(tr(lang, "settings.advanced"))+"</b>", advancedMenu(uid))
		}
	case data == "nav:workspace":
		if needAdmin() {
			editUI(chatID, msgID, "<b>"+esc(tr(lang, "ws.title"))+"</b>\n\n<code>"+esc(c.Workspace)+"</code>\n\n<i>"+esc(tr(lang, "ws.usage"))+"</i>", backKB(lang, "nav:settings"))
		}
	case data == "act:health":
		if needAdmin() {
			editUI(chatID, msgID, esc(tr(lang, "health.running")), nil)
			editUI(chatID, msgID, healthCheck(lang), keyboard([][]Btn{{btn(tr(lang, "btn.refresh"), "act:health"), btn(tr(lang, "btn.back"), "nav:advanced")}}))
		}
	case data == "act:runs":
		if needAdmin() {
			editUI(chatID, msgID, listRunsHTML(lang, 10), runsKeyboard(lang, 5))
		}
	case strings.HasPrefix(data, "runlog:"):
		if needAdmin() {
			name := strings.TrimPrefix(data, "runlog:")
			if !runNameRe.MatchString(name) {
				return
			}
			path := filepath.Join(baseDir, "runs", name)
			if raw, err := os.ReadFile(path); err == nil {
				sendMedia(chatID, "sendDocument", "document", raw, name, "")
			} else {
				toast = tr(lang, "file.error", err.Error())
			}
		}
	case data == "act:cron":
		if needAdmin() {
			editUI(chatID, msgID, cronPanel(lang), cronKeyboard(lang))
		}
	case strings.HasPrefix(data, "cron:"):
		if !needAdmin() {
			return
		}
		parts := strings.Split(data, ":")
		if len(parts) != 3 {
			return
		}
		id, _ := strconv.Atoi(parts[2])
		var err error
		switch parts[1] {
		case "pause":
			err = setCronEnabled(id, 0, false)
		case "resume":
			err = setCronEnabled(id, 0, true)
		case "del":
			err = deleteCronJob(id, 0)
		case "run":
			err = runCronNow(id)
		}
		if err != nil {
			toast = err.Error()
		} else {
			toast = tr(lang, "cron.updated", id)
		}
		editUI(chatID, msgID, cronPanel(lang), cronKeyboard(lang))
	case data == "act:quick":
		if needAdmin() {
			editUI(chatID, msgID, "<b>"+esc(tr(lang, "quick.title"))+"</b>\n\n"+esc(quickList(lang))+"\n<i>"+esc(tr(lang, "quick.help"))+"</i>", backKB(lang, "nav:settings"))
		}
	case data == "act:config":
		if needAdmin() {
			editUI(chatID, msgID, configOverview(lang), backKB(lang, "nav:advanced"))
		}
	case data == "act:log":
		if needAdmin() {
			editUI(chatID, msgID, "<b>"+esc(tr(lang, "log.title"))+"</b>\n<pre>"+esc(short(tailLog(20), 3200))+"</pre>", keyboard([][]Btn{{btn(tr(lang, "btn.refresh"), "act:log"), btn(tr(lang, "btn.back"), "nav:advanced")}}))
		}
	case data == "nav:effort":
		if !needAdmin() {
			return
		}
		var rows [][]Btn
		for _, lv := range effortChoices {
			cur := c.ReasoningEffort
			if cur == "" {
				cur = "off"
			}
			rows = append(rows, []Btn{btn(mark(cur, lv), "set:effort:"+lv)})
		}
		rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:settings")})
		editUI(chatID, msgID, "<b>"+esc(tr(lang, "effort.title"))+"</b>\n\n"+esc(tr(lang, "effort.hint")), keyboard(rows))
	case strings.HasPrefix(data, "set:effort:"):
		if !needAdmin() {
			return
		}
		level := strings.TrimPrefix(data, "set:effort:")
		valid := false
		for _, lv := range effortChoices {
			valid = valid || lv == level
		}
		if !valid {
			return
		}
		if level == "off" {
			level = ""
		}
		c.ReasoningEffort = level
		if !persistUIConfig(chatID, c) {
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
		rows = append(rows, []Btn{btn(tr(lang, "btn.provider"), "nav:provider"), btn(tr(lang, "btn.back"), "nav:settings")})
		editUI(chatID, msgID, "<b>"+esc(tr(lang, "model.pick"))+"</b>", keyboard(rows))
	case strings.HasPrefix(data, "set:model:"):
		if !needAdmin() {
			return
		}
		if p := currentProvider(); p != nil {
			for _, m := range p.Models {
				if data == settingCallback("model", m) {
					c.Model = m
					if persistUIConfig(chatID, c) {
						toast = tr(lang, "model.switched", m)
						editUI(chatID, msgID, menuText(uid), mainMenu(uid))
					}
					return
				}
			}
		}
	case data == "nav:provider":
		if !needAdmin() {
			return
		}
		var rows [][]Btn
		for _, p := range c.Provider {
			rows = append(rows, []Btn{btn(mark(c.ProviderName, short(p.Name, 40)), settingCallback("provider", p.Name))})
		}
		rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:advanced")})
		editUI(chatID, msgID, "<b>"+esc(tr(lang, "provider.pick"))+"</b>\n\n<i>"+esc(tr(lang, "provider.usage"))+"</i>", keyboard(rows))
	case strings.HasPrefix(data, "set:provider:"):
		if !needAdmin() {
			return
		}
		for _, p := range c.Provider {
			if data == settingCallback("provider", p.Name) {
				selectProvider(&c, p)
				if persistUIConfig(chatID, c) {
					toast = tr(lang, "provider.switched", p.Name, c.Model)
					editUI(chatID, msgID, menuText(uid), mainMenu(uid))
				}
				return
			}
		}
	case data == "nav:system", data == "nav:persona":
		if !needAdmin() {
			return
		}
		key, cur := "system", c.SystemPrompt
		if data == "nav:persona" {
			key, cur = "persona", c.Persona
		}
		if strings.TrimSpace(cur) == "" {
			cur = tr(lang, "prompt.unset")
		}
		editUI(chatID, msgID, "<b>"+esc(tr(lang, key+".title"))+"</b>\n\n"+esc(short(cur, 3000))+"\n\n<i>"+esc(tr(lang, key+".usage"))+"</i>", backKB(lang, "nav:advanced"))
	case data == "nav:users":
		if !needAdmin() {
			return
		}
		var rows [][]Btn
		addRow := func(id int64, tag string) {
			rows = append(rows, []Btn{btn(fmt.Sprintf("%s %d %s", tag, id, short(userName(id), 10)), "user:"+strconv.FormatInt(id, 10))})
		}
		addRow(c.OwnerID, "")
		for _, a := range c.Admins {
			addRow(a, "")
		}
		for _, f := range c.Friends {
			addRow(f, "")
		}
		rows = append(rows, []Btn{btn(tr(lang, "users.all"), "nav:allusers"), btn(tr(lang, "users.bans"), "nav:bans")})
		rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:advanced")})
		editUI(chatID, msgID, "<b>"+esc(tr(lang, "users.title"))+"</b>\n\n"+esc(tr(lang, "users.summary", len(c.Admins), len(c.Friends), len(c.Banned))), keyboard(rows))
	case data == "nav:allusers":
		if !needAdmin() {
			return
		}
		type pair struct {
			id int64
			s  SeenUser
		}
		seenMu.Lock()
		var list []pair
		for id, s := range seen {
			list = append(list, pair{id, s})
		}
		seenMu.Unlock()
		sort.Slice(list, func(i, j int) bool { return list[i].s.Last > list[j].s.Last })
		var rows [][]Btn
		for i, p := range list {
			if i >= 12 {
				break
			}
			rows = append(rows, []Btn{btn(fmt.Sprintf("%s %d %s", roleIcon(p.id), p.id, short(p.s.Name, 10)), "user:"+strconv.FormatInt(p.id, 10))})
		}
		rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:users")})
		editUI(chatID, msgID, esc(tr(lang, "users.visitors", len(list))), keyboard(rows))
	case data == "nav:bans":
		if !needAdmin() {
			return
		}
		var rows [][]Btn
		for _, id := range c.Banned {
			rows = append(rows, []Btn{btn(fmt.Sprintf("%d %s", id, short(userName(id), 8)), "unset:ban:"+strconv.FormatInt(id, 10))})
		}
		rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:users")})
		text := tr(lang, "users.bans_hint")
		if len(c.Banned) == 0 {
			text = tr(lang, "users.bans_empty")
		}
		editUI(chatID, msgID, esc(text), keyboard(rows))
	case strings.HasPrefix(data, "user:"):
		if !needAdmin() {
			return
		}
		id, _ := strconv.ParseInt(strings.TrimPrefix(data, "user:"), 10, 64)
		info := fmt.Sprintf("<b>%d</b>\n\n%s %s\n%s %s\n%s %s", id, esc(tr(lang, "users.name")), esc(userName(id)), esc(tr(lang, "menu.role")), esc(roleNameLang(id, lang)), esc(tr(lang, "users.last")), esc(lastActive(id)))
		var rows [][]Btn
		if isOwner(id) {
			info += "\n\n<i>" + esc(tr(lang, "users.owner_fixed")) + "</i>"
		} else {
			sid := strconv.FormatInt(id, 10)
			toggle := func(is bool, kind string) {
				if is {
					rows = append(rows, []Btn{btn(tr(lang, "users.unset_"+kind), "unset:"+kind+":"+sid)})
				} else {
					rows = append(rows, []Btn{btn(tr(lang, "users.set_"+kind), "set:"+kind+":"+sid)})
				}
			}
			toggle(isAdmin(id), "admin")
			toggle(isFriend(id), "friend")
			toggle(isBanned(id), "ban")
		}
		rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:users")})
		editUI(chatID, msgID, info, keyboard(rows))
	case strings.HasPrefix(data, "set:") || strings.HasPrefix(data, "unset:"):
		if !needAdmin() {
			return
		}
		parts := strings.SplitN(data, ":", 3)
		if len(parts) != 3 {
			return
		}
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || id <= 0 {
			return
		}
		if id == c.OwnerID {
			toast = tr(lang, "users.owner_fixed")
			return
		}
		add := parts[0] == "set"
		switch parts[1] {
		case "admin":
			c.Admins = toggleID(c.Admins, id, add)
		case "friend":
			c.Friends = toggleID(c.Friends, id, add)
		case "ban":
			c.Banned = toggleID(c.Banned, id, add)
		default:
			return
		}
		if persistUIConfig(chatID, c) {
			toast = tr(lang, "toast.saved")
			editUI(chatID, msgID, esc(tr(lang, "list.updated", tr(lang, "list."+parts[1]), id)), backKB(lang, "nav:users"))
		}
	}
}

func roleIcon(id int64) string {
	switch {
	case isOwner(id):
		return ""
	case isBanned(id):
		return ""
	case isAdmin(id):
		return ""
	case isFriend(id):
		return ""
	}
	return "·"
}
