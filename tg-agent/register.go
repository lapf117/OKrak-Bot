package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// User registration. config "registration" decides how strangers get access:
//   - off:      the old behaviour, the owner adds friends by ID;
//   - approval: a stranger applies, admins approve or reject (default);
//   - open:     a stranger becomes a friend as soon as they register.
// Registered users are friends, so friend_tools limits what they can do.

const (
	regCooldown     = 24 * time.Hour // wait after a rejection before applying again
	maxPendingRegs  = 100
	maxRegNoteRunes = 200
)

type regRequest struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username,omitempty"`
	Note     string `json:"note,omitempty"`
	Created  int64  `json:"created"`
	Status   string `json:"status"` // pending | rejected
	Updated  int64  `json:"updated,omitempty"`
}

var regMu sync.Mutex

// registrationMode reads the mode; an unset value means approval.
func registrationMode(c Config) string {
	if c.Registration == "off" || c.Registration == "open" {
		return c.Registration
	}
	return "approval"
}

func regPath() string { return filepath.Join(baseDir, "registrations.json") }

func loadRegsLocked() []regRequest {
	raw, err := os.ReadFile(regPath())
	if err != nil {
		return nil
	}
	var list []regRequest
	if json.Unmarshal(raw, &list) != nil {
		logf("注册记录文件损坏，已忽略")
		return nil
	}
	return list
}

func saveRegsLocked(list []regRequest) error {
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(regPath(), data, 0600)
}

func pendingRegistrations() []regRequest {
	regMu.Lock()
	defer regMu.Unlock()
	var out []regRequest
	for _, r := range loadRegsLocked() {
		if r.Status == "pending" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}

func regDisplay(r regRequest) string {
	name := strings.TrimSpace(r.Name)
	if r.Username != "" {
		name = strings.TrimSpace(name + " @" + r.Username)
	}
	if name == "" {
		name = "?"
	}
	return name
}

// Outcomes of applyRegistration.
const (
	regApproved = "approved" // open mode: access granted now
	regPending  = "pending"  // waiting for an admin
	regAlready  = "already"  // a request is already waiting
	regCooling  = "cooldown" // rejected recently
	regClosed   = "off"      // registration disabled
	regBanned   = "banned"
	regAllowed  = "allowed" // already a user
	regFull     = "full"    // too many pending requests
)

// applyRegistration records a stranger's request according to the mode.
func applyRegistration(u User, note string) (string, regRequest) {
	note = short(strings.TrimSpace(redactSecrets(note)), maxRegNoteRunes)
	req := regRequest{ID: u.ID, Name: short(strings.TrimSpace(u.FirstName), 64), Username: u.Username, Note: note, Created: time.Now().Unix(), Status: "pending"}
	switch {
	case allowed(u.ID):
		return regAllowed, req
	case isBanned(u.ID):
		return regBanned, req
	}
	mode := registrationMode(configSnapshot())
	if mode == "off" {
		return regClosed, req
	}
	regMu.Lock()
	defer regMu.Unlock()
	list := loadRegsLocked()
	pending := 0
	var keep []regRequest
	for _, r := range list {
		if r.ID == u.ID {
			switch {
			case r.Status == "pending" && mode != "open":
				return regAlready, r
			case r.Status == "rejected" && mode != "open" && time.Since(time.Unix(r.Updated, 0)) < regCooldown:
				return regCooling, r
			}
			continue // replaced below
		}
		if r.Status == "pending" {
			pending++
		}
		keep = append(keep, r)
	}
	if mode == "open" {
		uiMu.Lock()
		err := updateConfig(func(c *Config) {
			if !containsID(c.Banned, u.ID) {
				c.Friends = toggleID(c.Friends, u.ID, true)
			}
		})
		uiMu.Unlock()
		if err != nil {
			logf("开放注册保存失败: %v", err)
			return regClosed, req
		}
		_ = saveRegsLocked(keep)
		return regApproved, req
	}
	if pending >= maxPendingRegs {
		return regFull, req
	}
	if err := saveRegsLocked(append(keep, req)); err != nil {
		logf("保存注册申请失败: %v", err)
		return regFull, req
	}
	return regPending, req
}

// handleGuest answers every message from someone who is not a user yet.
func handleGuest(m Message, text, low string) {
	chatID, uid := m.Chat.ID, m.From.ID
	lang := langFor(uid)
	if isBanned(uid) {
		sendMessage(chatID, tr(lang, "err.banned"), nil)
		return
	}
	if registrationMode(configSnapshot()) == "off" {
		sendMessage(chatID, tr(lang, "err.unauthorized", uid), nil)
		return
	}
	if fields := strings.Fields(low); len(fields) > 0 && (fields[0] == "/register" || fields[0] == "/signup" || fields[0] == "/注册") {
		submitRegistration(chatID, m.From, argsAfter(text, 1))
		return
	}
	sendUI(chatID, guestWelcome(uid), guestKeyboard(uid))
}

// guestWelcome introduces the bot and the registration state of the visitor.
func guestWelcome(uid int64) string {
	lang := langFor(uid)
	var b strings.Builder
	b.WriteString("<b>▍" + esc(appName(lang)) + "</b>\n")
	b.WriteString(esc(tr(lang, "reg.intro")) + "\n\n")
	regMu.Lock()
	var mine *regRequest
	for _, r := range loadRegsLocked() {
		if r.ID == uid {
			r := r
			mine = &r
		}
	}
	regMu.Unlock()
	switch {
	case mine != nil && mine.Status == "pending":
		b.WriteString(esc(tr(lang, "reg.waiting")))
	case mine != nil && mine.Status == "rejected" && time.Since(time.Unix(mine.Updated, 0)) < regCooldown:
		b.WriteString(esc(tr(lang, "reg.cooling", humanDuration(regCooldown-time.Since(time.Unix(mine.Updated, 0)).Round(time.Minute)))))
	case registrationMode(configSnapshot()) == "open":
		b.WriteString(esc(tr(lang, "reg.open_hint")))
	default:
		b.WriteString(esc(tr(lang, "reg.apply_hint")))
	}
	b.WriteString("\n\n<i>" + esc(tr(lang, "reg.your_id", uid)) + "</i>")
	return b.String()
}

func guestKeyboard(uid int64) interface{} {
	lang := langFor(uid)
	for _, r := range pendingRegistrations() {
		if r.ID == uid {
			return nil
		}
	}
	label := tr(lang, "reg.btn_apply")
	if registrationMode(configSnapshot()) == "open" {
		label = tr(lang, "reg.btn_join")
	}
	return keyboard([][]Btn{{btn(label, "reg:apply")}})
}

// submitRegistration applies and tells the visitor (and admins) what happened.
func submitRegistration(chatID int64, u User, note string) {
	lang := langFor(u.ID)
	outcome, req := applyRegistration(u, note)
	switch outcome {
	case regApproved:
		sendUI(chatID, "<b>"+esc(tr(lang, "reg.welcome", appName(lang)))+"</b>\n\n"+menuText(u.ID), mainMenu(u.ID))
		notifyAdmins(func(adminLang string) string {
			return "<b>▍" + esc(tr(adminLang, "reg.joined_title")) + "</b>\n" + regSummaryHTML(adminLang, req)
		}, nil)
	case regPending:
		sendMessage(chatID, tr(lang, "reg.submitted"), nil)
		notifyAdmins(func(adminLang string) string {
			return "<b>▍" + esc(tr(adminLang, "reg.request_title")) + "</b>\n" + regSummaryHTML(adminLang, req)
		}, func(adminLang string) interface{} { return regDecisionKeyboard(adminLang, req.ID) })
	case regAlready:
		sendMessage(chatID, tr(lang, "reg.waiting"), nil)
	case regCooling:
		sendMessage(chatID, tr(lang, "reg.cooling", humanDuration(regCooldown-time.Since(time.Unix(req.Updated, 0)).Round(time.Minute))), nil)
	case regAllowed:
		sendUI(chatID, menuText(u.ID), mainMenu(u.ID))
	case regBanned:
		sendMessage(chatID, tr(lang, "err.banned"), nil)
	case regFull:
		sendMessage(chatID, tr(lang, "reg.full"), nil)
	default:
		sendMessage(chatID, tr(lang, "err.unauthorized", u.ID), nil)
	}
}

func regSummaryHTML(lang string, r regRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>\n", esc(tr(lang, "users.name")), esc(regDisplay(r)))
	fmt.Fprintf(&b, "ID <code>%d</code>\n", r.ID)
	if r.Note != "" {
		fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "reg.note")), esc(r.Note))
	}
	fmt.Fprintf(&b, "<i>%s</i>", esc(time.Unix(r.Created, 0).Format("2006-01-02 15:04")))
	return b.String()
}

func regDecisionKeyboard(lang string, id int64) interface{} {
	sid := strconv.FormatInt(id, 10)
	return keyboard([][]Btn{{btn("✓ "+tr(lang, "reg.approve"), "reg:ok:"+sid), btn("✗ "+tr(lang, "reg.reject"), "reg:no:"+sid), btn(tr(lang, "users.set_ban"), "reg:ban:"+sid)}})
}

// notifyAdmins sends a message to the owner and every admin in their language.
func notifyAdmins(text func(lang string) string, markup func(lang string) interface{}) {
	c := configSnapshot()
	ids := append([]int64{c.OwnerID}, c.Admins...)
	seenIDs := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seenIDs[id] || containsID(c.Banned, id) {
			continue
		}
		seenIDs[id] = true
		lang := langFor(id)
		var kb interface{}
		if markup != nil {
			kb = markup(lang)
		}
		sendUI(id, text(lang), kb)
	}
}

// decideRegistration approves, rejects or bans an applicant. It returns a
// toast and whether the request was still open.
func decideRegistration(adminUID, id int64, decision string) (string, bool) {
	lang := langFor(adminUID)
	regMu.Lock()
	list := loadRegsLocked()
	idx := -1
	for i, r := range list {
		if r.ID == id && r.Status == "pending" {
			idx = i
		}
	}
	if idx < 0 {
		regMu.Unlock()
		return tr(lang, "reg.handled"), false
	}
	req := list[idx]
	if decision == "no" {
		list[idx].Status, list[idx].Updated = "rejected", time.Now().Unix()
	} else {
		list = append(list[:idx], list[idx+1:]...)
	}
	saveErr := saveRegsLocked(list)
	regMu.Unlock()
	if saveErr != nil {
		return tr(lang, "err.save", saveErr.Error()), false
	}
	switch decision {
	case "ok", "ban":
		uiMu.Lock()
		err := updateConfig(func(c *Config) {
			if decision == "ok" {
				c.Friends = toggleID(c.Friends, id, true)
				c.Banned = toggleID(c.Banned, id, false)
			} else {
				c.Friends = toggleID(c.Friends, id, false)
				c.Banned = toggleID(c.Banned, id, true)
			}
		})
		uiMu.Unlock()
		if err != nil {
			return tr(lang, "err.save", err.Error()), false
		}
	}
	userLang := langFor(id)
	switch decision {
	case "ok":
		sendUI(id, "<b>"+esc(tr(userLang, "reg.approved_user", appName(userLang)))+"</b>\n\n"+menuText(id), mainMenu(id))
		return tr(lang, "reg.done_ok", regDisplay(req)), true
	case "no":
		sendMessage(id, tr(userLang, "reg.rejected_user"), nil)
		return tr(lang, "reg.done_no", regDisplay(req)), true
	}
	return tr(lang, "reg.done_ban", regDisplay(req)), true
}

// registrationPanel lists pending applications and the registration mode.
func registrationPanel(lang string) (string, map[string]interface{}) {
	mode := registrationMode(configSnapshot())
	pending := pendingRegistrations()
	var b strings.Builder
	b.WriteString("<b>▍" + esc(tr(lang, "btn.registrations")) + "</b>\n")
	fmt.Fprintf(&b, "%s %s\n", esc(tr(lang, "console.registration")), esc(registrationLabel(lang, mode)))
	b.WriteString("<i>" + esc(tr(lang, "reg.mode_hint."+mode)) + "</i>\n\n")
	if len(pending) == 0 {
		b.WriteString(esc(tr(lang, "reg.none")))
	}
	var rows [][]Btn
	for i, r := range pending {
		if i >= 8 {
			fmt.Fprintf(&b, "… +%d\n", len(pending)-i)
			break
		}
		fmt.Fprintf(&b, "%d. <b>%s</b> <code>%d</code> · %s", i+1, esc(regDisplay(r)), r.ID, esc(time.Unix(r.Created, 0).Format("01-02 15:04")))
		if r.Note != "" {
			b.WriteString(" · " + esc(short(r.Note, 60)))
		}
		b.WriteString("\n")
		sid := strconv.FormatInt(r.ID, 10)
		rows = append(rows, []Btn{btn(fmt.Sprintf("✓ %d. %s", i+1, short(regDisplay(r), 14)), "regp:ok:"+sid), btn("✗ "+tr(lang, "reg.reject"), "regp:no:"+sid)})
	}
	var modeRow []Btn
	for _, m := range []string{"off", "approval", "open"} {
		label := registrationLabel(lang, m)
		if m == mode {
			label = "● " + label
		}
		modeRow = append(modeRow, btn(label, "regp:mode:"+m))
	}
	rows = append(rows, modeRow, []Btn{btn(tr(lang, "btn.refresh"), "adm:reg"), btn(tr(lang, "btn.back"), "adm:root")})
	return strings.TrimRight(b.String(), "\n"), keyboard(rows)
}

// handleRegistrationCallback serves admin buttons: "reg:<ok|no|ban>:<id>" on
// notifications (collapsed to the result afterwards) and "regp:..." on the
// review list (re-rendered in place), plus "regp:mode:<mode>".
func handleRegistrationCallback(chatID, msgID, uid int64, data string) string {
	lang := langFor(uid)
	parts := strings.Split(data, ":")
	if len(parts) != 3 {
		return ""
	}
	fromPanel := parts[0] == "regp"
	refreshPanel := func() {
		text, kb := registrationPanel(lang)
		editUI(chatID, msgID, text, kb)
	}
	if parts[1] == "mode" {
		mode := parts[2]
		if !fromPanel || (mode != "off" && mode != "approval" && mode != "open") {
			return ""
		}
		uiMu.Lock()
		err := updateConfig(func(c *Config) { c.Registration = mode })
		uiMu.Unlock()
		if err != nil {
			return tr(lang, "err.save", err.Error())
		}
		refreshPanel()
		return tr(lang, "console.registration") + " " + registrationLabel(lang, mode)
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || id <= 0 || (parts[1] != "ok" && parts[1] != "no" && parts[1] != "ban") {
		return ""
	}
	toast, _ := decideRegistration(uid, id, parts[1])
	if fromPanel {
		refreshPanel()
	} else {
		editUI(chatID, msgID, "<b>"+esc(toast)+"</b>", nil)
	}
	return toast
}
