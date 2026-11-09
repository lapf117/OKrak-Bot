package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// sentTo returns the messages sent to one chat.
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

func lastToast(r *tgRecorder) string {
	calls := r.snapshot()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == "answerCallbackQuery" {
			text, _ := calls[i].Payload["text"].(string)
			return text
		}
	}
	return ""
}

func op(t *testing.T, name string, args ...string) string {
	t.Helper()
	text, err := execAdminOp(context.Background(), langZH, name, args)
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return text
}

func opErr(name string, args ...string) error {
	_, err := execAdminOp(context.Background(), langZH, name, args)
	return err
}

func TestTelegramKeepsOnlyUserMenusAndSettings(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	owner, friend := c.OwnerID, c.OwnerID+1
	cfgMu.Lock()
	cfg.Friends = []int64{friend}
	cfg.Provider = []Provider{{Name: "p", BaseURL: "https://api.example.com", Models: []string{"m1", "m2"}}}
	cfg.ProviderName, cfg.Model = "p", "m1"
	cfgMu.Unlock()

	user, admin := kbString(mainMenu(friend)), kbString(mainMenu(owner))
	for _, want := range []string{"act:new", "nav:display", "act:help", "act:notice", "act:about"} {
		if !strings.Contains(user, want) || !strings.Contains(admin, want) {
			t.Errorf("menus lack %s", want)
		}
	}
	for _, menu := range []string{user, admin} {
		for _, gone := range []string{"adm:", "nav:skills", "nav:advanced", "act:cron", "act:quick", "nav:persona", "nav:system", "nav:workspace", "nav:users", "nav:provider"} {
			if strings.Contains(menu, gone) {
				t.Errorf("menu still offers %s", gone)
			}
		}
	}
	settings := kbString(adminMenu(owner))
	if strings.Count(settings, "callback_data") != 4 || !strings.Contains(settings, "nav:model") || !strings.Contains(settings, "nav:execution") || !strings.Contains(settings, "nav:effort") {
		t.Fatalf("settings should hold only model, mode, reasoning and back: %s", settings)
	}
	if help := helpBody(owner); strings.Contains(help, "/admin") || !strings.Contains(help, " admin") || !strings.Contains(help, "/settings") {
		t.Fatalf("admin help should point to the terminal: %s", help)
	}
	if help := helpBody(friend); strings.Contains(help, "admin") || strings.Contains(help, "/settings") {
		t.Fatalf("user help mentions admin tools: %s", help)
	}
	var names []string
	for _, cmd := range commandList(true, langZH) {
		names = append(names, cmd["command"])
	}
	if strings.Join(names, " ") != "menu new stop show notice about help settings usage" {
		t.Fatalf("admin command list: %v", names)
	}

	// Management commands answer with the terminal command for admins only.
	for _, cmd := range []string{"/admin", "/provider", "/ws", "/cron", "/skills", "/announce", "/health", "/file"} {
		handleCommand(owner, owner, cmd, cmd)
		if text, _ := lastTextTo(rec, owner); !strings.Contains(text, "服务器终端") || !strings.Contains(text, " admin") {
			t.Errorf("%s did not point to the terminal: %s", cmd, text)
		}
		handleCommand(friend, friend, cmd, cmd)
		if text, _ := lastTextTo(rec, friend); !strings.Contains(text, "未知命令") {
			t.Errorf("%s answered a friend with %q", cmd, text)
		}
	}
	handleCommand(owner, owner, "/cmd add x y", "/cmd add x y")
	if len(configSnapshot().QuickCommands) != 0 {
		t.Fatal("shortcuts can still be created from Telegram")
	}
	// Buttons from older admin panels only point to the terminal.
	pressButton(User{ID: owner}, 9, "adm:root")
	if toast := lastToast(rec); !strings.Contains(toast, "tg-agent admin") {
		t.Fatalf("legacy button toast: %q", toast)
	}
	pressButton(User{ID: owner}, 9, "set:ban:"+"123")
	if containsID(configSnapshot().Banned, 123) {
		t.Fatal("legacy user button still changes users")
	}
	// The three settings still work from Telegram.
	pressButton(User{ID: owner}, 9, "nav:model")
	edit, ok := rec.find("editMessageText", "选择模型")
	if !ok || strings.Contains(markupOf(edit), "nav:provider") {
		t.Fatal("model picker missing or links to providers")
	}
	pressButton(User{ID: owner}, 9, settingCallback("model", "m2"))
	if configSnapshot().Model != "m2" {
		t.Fatal("model switch failed")
	}
}

func TestRegistrationIsReviewedInTerminal(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	owner := c.OwnerID
	ann := User{ID: 5005, FirstName: "Ann", Username: "ann"}

	handleMessage(privateMsg(ann, "/start"))
	if text, kb := lastTextTo(rec, ann.ID); !strings.Contains(text, "云端 AI Agent") || !strings.Contains(kb, "reg:apply") {
		t.Fatalf("welcome: %s %s", text, kb)
	}
	pressButton(ann, 2, "reg:apply")
	notice, noticeKB := lastTextTo(rec, owner)
	if !strings.Contains(notice, "新的注册申请") || !strings.Contains(notice, "reg approve 5005") || noticeKB != "null" {
		t.Fatalf("admin notification should carry the terminal command and no buttons: %s %s", notice, noticeKB)
	}
	before := len(rec.sentTo(owner))
	handleMessage(privateMsg(ann, "/register 再试一次"))
	if text, _ := lastTextTo(rec, ann.ID); !strings.Contains(text, "等待") || len(rec.sentTo(owner)) != before {
		t.Fatalf("duplicate application: %q", text)
	}
	if list := op(t, "reg.list"); !strings.Contains(list, "Ann @ann (5005)") || !strings.Contains(list, "需审核") {
		t.Fatalf("reg list: %s", list)
	}
	if ids := op(t, "reg.ids"); ids != "5005" {
		t.Fatalf("reg ids: %q", ids)
	}
	if out := op(t, "reg.approve", "5005"); !strings.Contains(out, "已通过") || !isFriend(ann.ID) {
		t.Fatalf("approve: %s", out)
	}
	if text, kb := lastTextTo(rec, ann.ID); !strings.Contains(text, "注册已通过") || !strings.Contains(kb, "act:new") {
		t.Fatalf("applicant not welcomed: %s", text)
	}
	if err := opErr("reg.approve", "5005"); err == nil || !strings.Contains(err.Error(), "已经处理") {
		t.Fatalf("second decision: %v", err)
	}

	bob := User{ID: 6006, FirstName: "Bob"}
	submitRegistration(bob.ID, bob, "想试试")
	op(t, "reg.reject", "6006")
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

	op(t, "reg.mode", "开放")
	cat := User{ID: 7007, FirstName: "Cat"}
	handleMessage(privateMsg(cat, "/register"))
	if !isFriend(cat.ID) {
		t.Fatal("open registration did not grant access")
	}
	op(t, "reg.mode", "off")
	handleMessage(privateMsg(User{ID: 8008}, "/start"))
	if text, kb := lastTextTo(rec, 8008); !strings.Contains(text, "未授权") || strings.Contains(kb, "reg:apply") {
		t.Fatalf("off mode: %s %s", text, kb)
	}
	if err := opErr("reg.mode", "maybe"); err == nil {
		t.Fatal("bad mode accepted")
	}
	op(t, "reg.mode", "approval")
	op(t, "users.add", "ban", "9009")
	handleMessage(privateMsg(User{ID: 9009}, "/register"))
	if text, _ := lastTextTo(rec, 9009); !strings.Contains(text, "封禁") {
		t.Fatalf("banned user could apply: %s", text)
	}
}

func TestTerminalOperations(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	owner := c.OwnerID
	cronMu.Lock()
	cronJobs, cronSeq = nil, 0
	cronMu.Unlock()

	// users
	op(t, "users.add", "friend", "2002")
	op(t, "users.add", "管理员", "3003")
	if !isFriend(2002) || !isAdmin(3003) {
		t.Fatal("users.add failed")
	}
	if list := op(t, "users.list"); !strings.Contains(list, "2002") || !strings.Contains(list, "3003") {
		t.Fatalf("users list: %s", list)
	}
	if _, ok := rec.find("setMyCommands", ""); !ok {
		t.Fatal("new admin did not get the admin command menu")
	}
	op(t, "users.del", "admin", "3003")
	if isAdmin(3003) || opErr("users.add", "friend", "abc") == nil || opErr("users.add", "admin", "1001") == nil {
		t.Fatal("users validation")
	}

	// name, author, support
	op(t, "brand.set", "name", "AI Agent 云端")
	op(t, "brand.set", "作者", "大肥鱼")
	op(t, "brand.set", "text", "觉得好用可以请作者喝杯咖啡。")
	if err := opErr("brand.set", "link", "ftp://example.com"); err == nil {
		t.Fatal("invalid link accepted")
	}
	op(t, "brand.set", "link", "https://example.com/sponsor")
	text, kb := aboutPanel(2002)
	if !strings.Contains(text, "AI Agent 云端") || !strings.Contains(text, "作者 大肥鱼") || !strings.Contains(kbString(kb), "https://example.com/sponsor") || !strings.Contains(menuText(owner), "AI Agent 云端") {
		t.Fatalf("brand not applied: %s", text)
	}
	op(t, "brand.set", "name", "-")
	if configSnapshot().BotName != "" {
		t.Fatal("- did not clear the name")
	}

	// providers
	op(t, "provider.add", "ds", "https://api.deepseek.com", "env:DS_KEY", "deepseek-chat,deepseek-reasoner")
	op(t, "provider.use", "ds")
	if cs := configSnapshot(); cs.ProviderName != "ds" || cs.Model != "deepseek-chat" {
		t.Fatalf("provider.use: %s %s", cs.ProviderName, cs.Model)
	}
	if raw, _ := os.ReadFile(configPath()); !strings.Contains(string(raw), "env:DS_KEY") {
		t.Fatal("secret reference not kept in the saved config")
	}
	if opErr("provider.del", "ds") == nil {
		t.Fatal("deleted the active provider")
	}

	// workspace
	dir := t.TempDir()
	op(t, "workspace.add", dir)
	op(t, "workspace.use", "1")
	if configSnapshot().Workspace != dir {
		t.Fatal("workspace.use by number failed")
	}
	if opErr("workspace.use", filepath.Join(dir, "missing")) == nil || opErr("workspace.add", "relative") == nil {
		t.Fatal("workspace validation")
	}
	op(t, "workspace.del", "1")

	// prompts
	op(t, "prompt.set", "system", "只用中文回答。\n保持简洁。")
	op(t, "prompt.set", "人设", "专业、友好")
	if cs := configSnapshot(); cs.SystemPrompt != "只用中文回答。\n保持简洁。" || cs.Persona != "专业、友好" {
		t.Fatal("prompt.set failed")
	}
	if shown := op(t, "prompt.show", "system"); !strings.Contains(shown, "保持简洁") {
		t.Fatalf("prompt.show: %s", shown)
	}
	op(t, "prompt.set", "persona", "-")
	if configSnapshot().Persona != "" {
		t.Fatal("persona not cleared")
	}

	// shortcuts and schedules
	op(t, "cmd.add", "review", "检查未提交的改动")
	if _, ok := findQuick("review"); !ok {
		t.Fatal("cmd.add failed")
	}
	op(t, "cmd.del", "review")
	created := op(t, "cron.add", "daily", "09:00", "汇总服务器状态")
	if !strings.Contains(created, "#1") || len(cronSnapshot()) != 1 || cronSnapshot()[0].UserID != owner {
		t.Fatalf("cron.add: %s", created)
	}
	op(t, "cron.pause", "1")
	if list := op(t, "cron.list"); !strings.Contains(list, "暂停") || !strings.Contains(list, "汇总服务器状态") {
		t.Fatalf("cron list: %s", list)
	}
	op(t, "cron.resume", "1")
	op(t, "cron.del", "1")
	if len(cronSnapshot()) != 0 || opErr("cron.add", "sometimes", "x") == nil {
		t.Fatal("cron handling")
	}

	// announcements are pushed to every user, not to banned ones
	cfgMu.Lock()
	cfg.Banned = []int64{2002}
	cfgMu.Unlock()
	if n := op(t, "notice.recipients"); n != "1" {
		t.Fatalf("recipients: %s", n)
	}
	cfgMu.Lock()
	cfg.Banned = nil
	cfgMu.Unlock()
	out := op(t, "notice.publish", "## 维护通知\n今晚 **22:00** 升级")
	if !strings.Contains(out, "送达 2/2") {
		t.Fatalf("publish: %s", out)
	}
	for _, id := range []int64{owner, 2002} {
		if text, _ := lastTextTo(rec, id); !strings.Contains(text, "维护通知") || !strings.Contains(text, "<b>22:00</b>") {
			t.Errorf("user %d did not get the announcement: %s", id, text)
		}
	}
	pressButton(User{ID: 2002}, 5, "act:notice")
	if _, ok := rec.find("editMessageText", "维护通知"); !ok {
		t.Fatal("users cannot read announcements")
	}
	if list := op(t, "notice.list"); !strings.Contains(list, "#1") || !strings.Contains(list, "送达 2/2") {
		t.Fatalf("notice list: %s", list)
	}
	op(t, "notice.del", "1")
	if len(listNotices()) != 0 || opErr("notice.del", "9") == nil {
		t.Fatal("notice.del")
	}

	// diagnostics and overview
	if s := op(t, "status"); !strings.Contains(s, "用户 2 人") || !strings.Contains(s, "注册方式") {
		t.Fatalf("status: %s", s)
	}
	if s := op(t, "config"); !strings.Contains(s, "workspace") || strings.Contains(s, "<pre>") {
		t.Fatalf("config: %s", s)
	}
	os.MkdirAll(filepath.Join(baseDir, "runs"), 0700)
	os.WriteFile(filepath.Join(baseDir, "runs", "20261109-101010.000000000-1001.md"), []byte("# 执行记录\n\n## 任务\n\n检查服务"), 0600)
	if s := op(t, "runs.list"); !strings.Contains(s, "1.") {
		t.Fatalf("runs: %s", s)
	}
	if s := op(t, "runs.show", "1"); !strings.Contains(s, "检查服务") {
		t.Fatalf("runs.show: %s", s)
	}
	if _, err := callAdmin(context.Background(), langZH, "cron.run", []string{"1"}); err == nil || !strings.Contains(err.Error(), "机器人正在运行") {
		t.Fatalf("cron.run without a bot: %v", err)
	}
}

func TestTerminalTalksToRunningBot(t *testing.T) {
	runtimeTestConfig(t)
	recordTelegram(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if botRunning() {
		t.Fatal("no bot should be running yet")
	}
	stop := startAdminSocket(ctx)
	if !botRunning() {
		t.Fatal("socket not reachable")
	}
	if info, err := os.Stat(adminSocketPath()); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %v %v", info, err)
	}
	if out, err := callAdmin(ctx, langZH, "brand.set", []string{"name", "云端助手"}); err != nil || !strings.Contains(out, "已保存") {
		t.Fatalf("socket call: %q %v", out, err)
	}
	if configSnapshot().BotName != "云端助手" {
		t.Fatal("operation did not run in the bot")
	}
	if _, err := callAdmin(ctx, langZH, "users.add", []string{"friend", "x"}); err == nil {
		t.Fatal("errors are not passed back")
	}
	// A second instance does not steal the socket.
	stop2 := startAdminSocket(ctx)
	stop2()
	if !botRunning() {
		t.Fatal("second instance removed the live socket")
	}
	stop()
	if botRunning() {
		t.Fatal("socket left behind")
	}
	// Without the bot the terminal edits the files itself.
	if _, err := callAdmin(ctx, langZH, "brand.set", []string{"name", "-"}); err != nil || configSnapshot().BotName != "" {
		t.Fatalf("local fallback: %v", err)
	}
}

func TestTerminalPicksUpOutsideConfigChanges(t *testing.T) {
	runtimeTestConfig(t)
	recordTelegram(t)
	op(t, "brand.set", "author", "甲")
	// Someone edits config.json by hand (or another process saves it).
	raw, _ := os.ReadFile(configPath())
	edited := strings.Replace(string(raw), `"author": "甲"`, `"author": "乙"`, 1)
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(configPath(), []byte(edited+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	op(t, "brand.set", "name", "新名称")
	if c := configSnapshot(); c.Author != "乙" || c.BotName != "新名称" {
		t.Fatalf("outside edit lost: author=%q name=%q", c.Author, c.BotName)
	}
	// A broken file is never overwritten.
	os.WriteFile(configPath(), []byte("{broken"), 0600)
	if err := opErr("brand.set", "name", "x"); err == nil {
		t.Fatal("saved over a broken config file")
	}
	if raw, _ := os.ReadFile(configPath()); string(raw) != "{broken" {
		t.Fatal("broken file was replaced")
	}
}

// TestTerminalMenuWalkthrough drives the interactive menu with typed input.
func TestTerminalMenuWalkthrough(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	cronMu.Lock()
	cronJobs, cronSeq = nil, 0
	cronMu.Unlock()
	script := strings.Join([]string{
		"4", "1", "AI Agent 云端", "0", // name
		"1", "3", "777", "0", // add a user
		"3", "1", "## 维护通知", "今晚升级", ".", "y", "0", // publish
		"2", "4", "3", "0", // registration open
		"9", "1", "daily 09:00", "检查服务", "0", // schedule
		"7", "1", "只用中文回答。", ".", "0", // system prompt
		"10", "1", "review", "检查改动", "0", // shortcut
		"5", "0", "6", "0", "8", "0", "11", "0", "13", "", "14", "15", "", "99", "abc",
		"0",
	}, "\n") + "\n"
	var out bytes.Buffer
	call := func(name string, args ...string) (string, error) {
		return callAdmin(context.Background(), langZH, name, args)
	}
	newAdminUI(strings.NewReader(script), &out, langZH, call, func() bool { return false }).run()
	shown := out.String()
	cs := configSnapshot()
	if cs.BotName != "AI Agent 云端" || !isFriend(777) || registrationMode(cs) != "open" || cs.SystemPrompt != "只用中文回答。" {
		t.Fatalf("menu actions not applied: %+v", cs)
	}
	if _, ok := findQuick("review"); !ok || len(cronSnapshot()) != 1 || len(listNotices()) != 1 {
		t.Fatal("shortcut, schedule or announcement missing")
	}
	if text, _ := lastTextTo(rec, c.OwnerID); !strings.Contains(text, "维护通知") {
		t.Fatal("announcement not pushed")
	}
	for _, want := range []string{"终端管理", "机器人未运行", "用户管理", "注册审核", "发布公告", "名称与作者", "预览", "送达", "请输入列表中的编号", "已退出"} {
		if !strings.Contains(shown, want) {
			t.Errorf("menu output lacks %q", want)
		}
	}
	for _, raw := range []string{"cli.", "btn.", "brand.edit", "reg.mode"} {
		if strings.Contains(shown, raw) {
			t.Errorf("untranslated key %q in menu output", raw)
		}
	}
	for _, r := range shown {
		if r >= 0x1F000 && r <= 0x1FFFF {
			t.Fatalf("emoji in terminal output: %q", string(r))
		}
	}
}

func TestRunAdminCLICommands(t *testing.T) {
	runtimeTestConfig(t)
	rec := recordTelegram(t)
	var stdout, stderr bytes.Buffer
	if code := runAdminCLI([]string{"help"}, strings.NewReader(""), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "admin reg") {
		t.Fatalf("help: %d %s", code, stdout.String())
	}
	stdout.Reset()
	if code := runAdminCLI([]string{"brand", "set", "author", "大肥鱼"}, strings.NewReader(""), &stdout, &stderr); code != 0 || configSnapshot().Author != "大肥鱼" {
		t.Fatalf("brand set: %d %s %s", code, stdout.String(), stderr.String())
	}
	if code := runAdminCLI([]string{"bogus"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("unknown command exit %d", code)
	}
	stderr.Reset()
	if code := runAdminCLI([]string{"users", "add", "friend"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "用法") {
		t.Fatalf("usage error: %d %s", code, stderr.String())
	}
	stdout.Reset()
	if code := runAdminCLI([]string{"notice", "publish", "-"}, strings.NewReader("来自标准输入的公告\n"), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "已发布") {
		t.Fatalf("publish from stdin: %d %s %s", code, stdout.String(), stderr.String())
	}
	if _, ok := rec.find("sendMessage", "来自标准输入的公告"); !ok {
		t.Fatal("announcement not sent")
	}
	if telegramContext.Err() != nil || !logToStdout.Load() {
		t.Fatal("runAdminCLI did not restore the process state")
	}
}
