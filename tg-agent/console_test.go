package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func markupOf(call recordedCall) string {
	raw, _ := json.Marshal(call.Payload["reply_markup"])
	return string(raw)
}

func kbString(kb interface{}) string {
	raw, _ := json.Marshal(kb)
	return string(raw)
}

func privateMsg(u User, text string) Message {
	return Message{MessageID: 1, From: u, Chat: Chat{ID: u.ID, Type: "private"}, Text: text}
}

func pressButton(u User, msgID int64, data string) {
	handleCallback(CallbackQuery{ID: "cb", From: u, Message: &Message{MessageID: msgID, Chat: Chat{ID: u.ID, Type: "private"}}, Data: data})
}

// sentTo returns the texts of messages sent to one chat.
func (r *tgRecorder) sentTo(chatID int64) []recordedCall {
	var out []recordedCall
	for _, call := range r.snapshot() {
		if call.Method != "sendMessage" {
			continue
		}
		if id, _ := call.Payload["chat_id"].(float64); int64(id) == chatID {
			out = append(out, call)
		}
	}
	return out
}

func lastTextTo(r *tgRecorder, chatID int64) (string, string) {
	calls := r.sentTo(chatID)
	if len(calls) == 0 {
		return "", ""
	}
	last := calls[len(calls)-1]
	text, _ := last.Payload["text"].(string)
	return text, markupOf(last)
}

func TestMenusAreConcise(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	owner, friend := c.OwnerID, c.OwnerID+1
	cfgMu.Lock()
	cfg.Friends = []int64{friend}
	cfg.Provider = []Provider{{Name: "p", BaseURL: "https://api.example.com", Models: []string{"m1", "m2"}}}
	cfg.ProviderName, cfg.Model = "p", "m1"
	cfgMu.Unlock()

	user := kbString(mainMenu(friend))
	for _, want := range []string{"act:new", "nav:display", "act:help", "act:notice", "act:about"} {
		if !strings.Contains(user, want) {
			t.Errorf("user menu lacks %s: %s", want, user)
		}
	}
	admin := kbString(mainMenu(owner))
	for _, want := range []string{"act:usage", "nav:model", "nav:effort", "nav:settings", "act:notice", "act:about"} {
		if !strings.Contains(admin, want) {
			t.Errorf("admin menu lacks %s: %s", want, admin)
		}
	}
	for _, menu := range []string{user, admin} {
		for _, gone := range []string{"nav:skills", "nav:advanced", "act:cron", "act:quick", "nav:persona", "nav:system", "nav:workspace"} {
			if strings.Contains(menu, gone) {
				t.Errorf("menu still offers %s: %s", gone, menu)
			}
		}
	}
	settings := kbString(adminMenu(owner))
	for _, want := range []string{"nav:model", "nav:execution", "nav:effort", "nav:root"} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings lacks %s", want)
		}
	}
	if strings.Count(settings, "callback_data") != 4 {
		t.Fatalf("settings should hold only model, mode, reasoning and back: %s", settings)
	}
	if text := settingsText(owner); !strings.Contains(text, "m1") || !strings.Contains(text, "执行模式") || !strings.Contains(text, "推理") {
		t.Fatalf("settings text: %s", text)
	}
	help := helpBody(friend)
	for _, gone := range []string{"/skills", "/cron", "/steps", "/cmd", "/settings", "/admin"} {
		if strings.Contains(help, gone) {
			t.Errorf("user help lists %s", gone)
		}
	}
	if !strings.Contains(help, "/notice") || !strings.Contains(help, "/about") || !strings.Contains(helpBody(owner), "/admin") {
		t.Fatal("help lacks the new entries")
	}
	var names []string
	for _, cmd := range commandList(false, langZH) {
		names = append(names, cmd["command"])
	}
	if strings.Join(names, " ") != "menu new stop show notice about help" {
		t.Fatalf("default command list: %v", names)
	}
	if got := len(commandList(true, langZH)); got != len(names)+3 {
		t.Fatalf("admin command list size %d", got)
	}

	// The model picker no longer links to providers; skills are admin-only.
	pressButton(User{ID: owner}, 9, "nav:model")
	edit, ok := rec.find("editMessageText", "选择模型")
	if !ok || strings.Contains(markupOf(edit), "nav:provider") {
		t.Fatalf("model picker: %v %s", ok, markupOf(edit))
	}
	handleCommand(friend, friend, "/skills", "/skills")
	if text, _ := lastTextTo(rec, friend); !strings.Contains(text, "管理员") {
		t.Fatalf("friend reached /skills: %q", text)
	}
}

func TestConsoleIsAdminOnly(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	owner, friend := c.OwnerID, c.OwnerID+1
	cfgMu.Lock()
	cfg.Friends = []int64{friend}
	cfgMu.Unlock()
	handleCommand(owner, owner, "/admin", "/admin")
	text, kb := lastTextTo(rec, owner)
	if !strings.Contains(text, "管理后台") {
		t.Fatalf("console text: %s", text)
	}
	for _, want := range []string{"nav:users", "adm:reg", "adm:notice", "adm:brand", "nav:provider", "nav:system", "nav:persona", "act:cron", "act:quick", "nav:skills", "act:health"} {
		if !strings.Contains(kb, want) {
			t.Errorf("console lacks %s", want)
		}
	}
	handleCommand(friend, friend, "/admin", "/admin")
	if text, _ := lastTextTo(rec, friend); !strings.Contains(text, "管理员") {
		t.Fatalf("friend opened console: %s", text)
	}
	pressButton(User{ID: friend}, 3, "adm:brand")
	if _, ok := rec.find("editMessageText", "名称与作者"); ok {
		t.Fatal("friend opened an admin panel")
	}
	// /admin add <id> still edits the admin list.
	handleCommand(owner, owner, "/admin add 4242", "/admin add 4242")
	if !isAdmin(4242) {
		t.Fatal("/admin add no longer works")
	}
}

func TestRegistrationApprovalFlow(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	owner := c.OwnerID
	ann := User{ID: 5005, FirstName: "Ann", Username: "ann"}

	handleMessage(privateMsg(ann, "/start"))
	text, kb := lastTextTo(rec, ann.ID)
	if !strings.Contains(text, "云端 AI Agent") || !strings.Contains(kb, "reg:apply") {
		t.Fatalf("welcome: %s %s", text, kb)
	}
	pressButton(ann, 2, "reg:apply")
	if len(pendingRegistrations()) != 1 || allowed(ann.ID) {
		t.Fatal("application not pending")
	}
	notice, noticeKB := lastTextTo(rec, owner)
	if !strings.Contains(notice, "新的注册申请") || !strings.Contains(notice, "@ann") || !strings.Contains(noticeKB, "reg:ok:5005") {
		t.Fatalf("admin not notified: %s %s", notice, noticeKB)
	}
	before := len(rec.sentTo(owner))
	handleMessage(privateMsg(ann, "/register 再试一次"))
	if text, _ := lastTextTo(rec, ann.ID); !strings.Contains(text, "等待") || len(rec.sentTo(owner)) != before {
		t.Fatalf("duplicate application: %q", text)
	}

	// A friend cannot decide; the owner approves.
	friend := owner + 1
	cfgMu.Lock()
	cfg.Friends = []int64{friend}
	cfgMu.Unlock()
	pressButton(User{ID: friend}, 4, "reg:ok:5005")
	if allowed(ann.ID) {
		t.Fatal("friend approved a registration")
	}
	pressButton(User{ID: owner}, 4, "reg:ok:5005")
	if !isFriend(ann.ID) || len(pendingRegistrations()) != 0 {
		t.Fatal("approval did not grant access")
	}
	if text, kb := lastTextTo(rec, ann.ID); !strings.Contains(text, "注册已通过") || !strings.Contains(kb, "act:new") {
		t.Fatalf("applicant not welcomed: %s", text)
	}
	if edit, ok := rec.find("editMessageText", "已通过"); !ok || strings.Contains(markupOf(edit), "reg:ok") {
		t.Fatal("notification not collapsed after the decision")
	}
	pressButton(User{ID: owner}, 4, "reg:ok:5005")
	if edit, ok := rec.find("editMessageText", "已经处理"); !ok || edit.Payload["text"] == "" {
		t.Fatal("second decision not reported as handled")
	}

	// Rejection with cooldown.
	bob := User{ID: 6006, FirstName: "Bob"}
	submitRegistration(bob.ID, bob, "想试试")
	pressButton(User{ID: owner}, 7, "regp:no:6006")
	if allowed(bob.ID) {
		t.Fatal("rejected user has access")
	}
	if text, _ := lastTextTo(rec, bob.ID); !strings.Contains(text, "未通过") {
		t.Fatalf("rejection not told: %s", text)
	}
	submitRegistration(bob.ID, bob, "")
	if text, _ := lastTextTo(rec, bob.ID); !strings.Contains(text, "后可重新申请") {
		t.Fatalf("cooldown not enforced: %s", text)
	}

	// Open mode grants access at once; off mode keeps the old message.
	pressButton(User{ID: owner}, 8, "regp:mode:open")
	if registrationMode(configSnapshot()) != "open" {
		t.Fatal("mode button failed")
	}
	cat := User{ID: 7007, FirstName: "Cat"}
	handleMessage(privateMsg(cat, "/register"))
	if !isFriend(cat.ID) {
		t.Fatal("open registration did not grant access")
	}
	if text, _ := lastTextTo(rec, owner); !strings.Contains(text, "新用户已注册") {
		t.Fatalf("admins not told about the new user: %s", text)
	}
	pressButton(User{ID: owner}, 8, "regp:mode:off")
	dan := User{ID: 8008, FirstName: "Dan"}
	handleMessage(privateMsg(dan, "/start"))
	if text, kb := lastTextTo(rec, dan.ID); !strings.Contains(text, "未授权") || strings.Contains(kb, "reg:apply") {
		t.Fatalf("off mode: %s %s", text, kb)
	}
	cfgMu.Lock()
	cfg.Registration, cfg.Banned = "approval", []int64{9009}
	cfgMu.Unlock()
	handleMessage(privateMsg(User{ID: 9009}, "/register"))
	if text, _ := lastTextTo(rec, 9009); !strings.Contains(text, "封禁") {
		t.Fatalf("banned user could apply: %s", text)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAnnouncementPreviewPublishAndView(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	owner, f1, f2, banned := c.OwnerID, c.OwnerID+1, c.OwnerID+2, c.OwnerID+3
	cfgMu.Lock()
	cfg.Friends, cfg.Banned = []int64{f1, f2, banned}, []int64{banned}
	cfgMu.Unlock()

	pressButton(User{ID: owner}, 3, "adm:in:announce")
	handleMessage(privateMsg(User{ID: owner}, "## 维护通知\n今晚 **22:00** 升级"))
	preview, kb := lastTextTo(rec, owner)
	if !strings.Contains(preview, "预览") || !strings.Contains(preview, "<b>22:00</b>") || !strings.Contains(kb, "ntc:pub") {
		t.Fatalf("preview: %s %s", preview, kb)
	}
	if len(rec.sentTo(f1)) != 0 {
		t.Fatal("announcement pushed before confirmation")
	}
	pressButton(User{ID: f1}, 4, "ntc:pub")
	if len(listNotices()) != 0 {
		t.Fatal("friend published an announcement")
	}
	pressButton(User{ID: owner}, 4, "ntc:pub")
	waitFor(t, func() bool { n := listNotices(); return len(n) == 1 && n[0].Total > 0 })
	n := listNotices()[0]
	if n.Sent != 3 || n.Total != 3 {
		t.Fatalf("delivery %d/%d, want owner and two friends", n.Sent, n.Total)
	}
	for _, id := range []int64{owner, f1, f2} {
		if text, _ := lastTextTo(rec, id); !strings.Contains(text, "维护通知") || !strings.Contains(text, "▍公告") {
			t.Errorf("user %d did not get the announcement: %s", id, text)
		}
	}
	if len(rec.sentTo(banned)) != 0 {
		t.Fatal("banned user received the announcement")
	}
	waitFor(t, func() bool { _, ok := rec.find("editMessageText", "送达 3/3"); return ok })

	pressButton(User{ID: f1}, 5, "act:notice")
	if edit, ok := rec.find("editMessageText", "维护通知"); !ok || !strings.Contains(markupOf(edit), "nav:root") {
		t.Fatal("users cannot read announcements")
	}
	handleCommand(f1, f1, "/announce 偷偷发", "/announce")
	if text, _ := lastTextTo(rec, f1); !strings.Contains(text, "管理员") {
		t.Fatalf("friend could announce: %s", text)
	}

	// Cancelling a draft publishes nothing; deleting removes it.
	handleCommand(owner, owner, "/announce 草稿", "/announce")
	pressButton(User{ID: owner}, 6, "ntc:cancel")
	pressButton(User{ID: owner}, 6, "ntc:pub")
	if len(listNotices()) != 1 {
		t.Fatal("cancelled draft was published")
	}
	pressButton(User{ID: owner}, 7, "ntc:del:1")
	if len(listNotices()) != 0 {
		t.Fatal("delete failed")
	}
}

func TestBrandInputAndAboutPanel(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	owner := c.OwnerID
	me := User{ID: owner}

	text, kbMap := aboutPanel(owner)
	if kb := kbString(kbMap); !strings.Contains(text, "感谢使用") || !strings.Contains(text, "/admin") || strings.Contains(kb, `"url"`) {
		t.Fatalf("default about page: %s %s", text, kb)
	}
	pressButton(me, 3, "adm:in:name")
	handleMessage(privateMsg(me, "AI Agent 云端"))
	if configSnapshot().BotName != "AI Agent 云端" || !strings.Contains(menuText(owner), "AI Agent 云端") || !strings.Contains(helpBody(owner), "AI Agent 云端") {
		t.Fatal("name not applied")
	}
	pressButton(me, 3, "adm:in:author")
	handleMessage(privateMsg(me, "大肥鱼"))
	pressButton(me, 3, "adm:in:support")
	handleMessage(privateMsg(me, "觉得好用可以请作者喝杯咖啡。"))
	pressButton(me, 3, "adm:in:link")
	handleMessage(privateMsg(me, "ftp://example.com"))
	if configSnapshot().SupportURL != "" {
		t.Fatal("invalid link saved")
	}
	if text, _ := lastTextTo(rec, owner); !strings.Contains(text, "support_url") {
		t.Fatalf("invalid link not explained: %s", text)
	}
	pressButton(me, 3, "adm:in:link")
	handleMessage(privateMsg(me, "https://example.com/sponsor"))
	text, kbMap = aboutPanel(owner + 1)
	if kb := kbString(kbMap); !strings.Contains(text, "作者 大肥鱼") || !strings.Contains(text, "请作者喝杯咖啡") || !strings.Contains(kb, "https://example.com/sponsor") {
		t.Fatalf("about page: %s %s", text, kbString(kbMap))
	}

	// "-" restores the default; a command cancels a pending input.
	pressButton(me, 3, "adm:in:name")
	handleMessage(privateMsg(me, "-"))
	if configSnapshot().BotName != "" || !strings.Contains(menuText(owner), tr(langZH, "app.name")) {
		t.Fatal("name not cleared")
	}
	pressButton(me, 3, "adm:in:name")
	handleMessage(privateMsg(me, "/help"))
	if _, pending := pendingAdminInput(owner); pending || configSnapshot().BotName != "" {
		t.Fatal("command did not cancel the input")
	}
	pressButton(me, 3, "adm:in:author")
	handleCommand(owner, owner, "/cancel", "/cancel")
	if _, pending := pendingAdminInput(owner); pending {
		t.Fatal("/cancel failed")
	}
	if bad := configSnapshot(); validateConfig(func() Config { bad.Registration = "maybe"; return bad }()) == nil {
		t.Fatal("invalid registration mode accepted")
	}
}
