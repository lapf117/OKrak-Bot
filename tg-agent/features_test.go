package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func taskContext(c Config, uid int64) context.Context {
	ctx := WithExecutionConfig(context.Background(), c)
	ctx = context.WithValue(ctx, identityContextKey, executionIdentityValue{ChatID: uid, UserID: uid})
	return withTaskBudget(ctx, c)
}

func TestPlanToolLifecycleAndChildIsolation(t *testing.T) {
	ctx := toolTestContext(t)
	out, err := runPlan(ctx, `{"action":"set","items":["读代码","修复","测试"],"intent":"列出计划"}`)
	if err != nil || !strings.Contains(out, "0/3") {
		t.Fatalf("set: %q %v", out, err)
	}
	if _, err := runPlan(ctx, `{"action":"start"}`); err != nil {
		t.Fatal(err)
	}
	plan := planSnapshot(ctx)
	if plan[0].Status != planInProgress {
		t.Fatalf("start without id should pick the first pending step: %+v", plan)
	}
	out, _ = runPlan(ctx, `{"action":"done"}`)
	if !strings.Contains(out, "1/3") || !strings.Contains(out, "[x] 1. 读代码") {
		t.Fatalf("done without id should complete the current step: %s", out)
	}
	child := withPlanContext(ctx)
	if _, err := runPlan(child, `{"action":"set","items":["child step"]}`); err != nil {
		t.Fatal(err)
	}
	if got := planSnapshot(ctx); len(got) != 3 {
		t.Fatalf("sub-agent plan replaced the parent plan: %+v", got)
	}
	if _, err := runPlan(ctx, `{"action":"remove","id":2}`); err != nil {
		t.Fatal(err)
	}
	if got := planSnapshot(ctx); len(got) != 2 || got[1].Text != "测试" {
		t.Fatalf("remove: %+v", got)
	}
}

func runPlan(ctx context.Context, args string) (string, error) {
	var m map[string]interface{}
	_ = json.Unmarshal([]byte(args), &m)
	delete(m, "intent")
	return toolPlanContext(ctx, m)
}

func TestToolSchemasRequireIntentAndAliasTodo(t *testing.T) {
	c := runtimeTestConfig(t)
	for _, s := range toolSchemasContext(WithExecutionConfig(context.Background(), c), c.OwnerID) {
		params := s.Function.Parameters.(map[string]interface{})
		props := params["properties"].(map[string]interface{})
		required := params["required"].([]string)
		if _, ok := props["intent"]; !ok || !containsString(required, "intent") {
			t.Fatalf("%s lacks a required intent parameter", s.Function.Name)
		}
	}
	if canonicalToolName("todo") != "plan" || !toolReadOnly("todo", `{"action":"add"}`) {
		t.Fatal("legacy todo alias broken")
	}
	ctx := taskContext(c, c.OwnerID)
	if out, err := runToolContext(ctx, "todo", `{"action":"add","text":"legacy","intent":"兼容旧名"}`); err != nil || !strings.Contains(out, "legacy") {
		t.Fatalf("alias dispatch: %q %v", out, err)
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestToolAccessClassification(t *testing.T) {
	cases := []struct {
		name, args string
		want       toolAccess
	}{
		{"memory", `{"action":"list"}`, accessRead},
		{"memory", `{"action":"add","text":"x"}`, accessSelf},
		{"schedule", `{"action":"list"}`, accessRead},
		{"schedule", `{"action":"add"}`, accessWrite},
		{"git", `{"action":"show"}`, accessRead},
		{"git", `{"action":"push"}`, accessWrite},
		{"send_file", `{"path":"a"}`, accessRead},
		{"bash", `{"command":"ls"}`, accessWrite},
	}
	for _, tc := range cases {
		if got := classifyTool(tc.name, tc.args); got != tc.want {
			t.Errorf("%s %s = %v, want %v", tc.name, tc.args, got, tc.want)
		}
	}
}

func TestMemoryToolPermissions(t *testing.T) {
	c := runtimeTestConfig(t)
	friend := c.OwnerID + 7
	cfgMu.Lock()
	cfg.Friends = []int64{friend}
	cfgMu.Unlock()
	ctx := taskContext(c, friend)
	if _, err := runToolContext(ctx, "memory", `{"action":"add","text":"喜欢简短回答","intent":"记住偏好"}`); err != nil {
		t.Fatalf("friend could not save own memory: %v", err)
	}
	if !strings.Contains(memoryPrompt(friend), "喜欢简短回答") {
		t.Fatal("memory not injected into prompt")
	}
	if strings.Contains(memoryPrompt(c.OwnerID), "喜欢简短回答") {
		t.Fatal("memory leaked to another user")
	}
	c.ApprovalMode = "read-only"
	ro := taskContext(c, friend)
	if _, err := runToolContext(ro, "memory", `{"action":"add","text":"blocked"}`); err == nil {
		t.Fatal("read-only mode allowed a memory write")
	}
	if out, err := runToolContext(ro, "memory", `{"action":"list"}`); err != nil || !strings.Contains(out, "#1") {
		t.Fatalf("read-only list: %q %v", out, err)
	}
	if ok, err := deleteMemory(friend, 1); !ok || err != nil {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if _, err := os.Stat(memoryPath(friend)); !os.IsNotExist(err) {
		t.Fatal("empty memory file should be removed")
	}
}

func TestAdminOnlyToolsAndApproveAll(t *testing.T) {
	c := runtimeTestConfig(t)
	friend := c.OwnerID + 9
	c.FriendTools = "all"
	cfgMu.Lock()
	cfg.Friends = []int64{friend}
	cfgMu.Unlock()
	if err := authorizeTool(taskContext(c, friend), "schedule", `{"action":"add"}`); err == nil {
		t.Fatal("non-admin created a schedule")
	}
	for _, s := range toolSchemasContext(WithExecutionConfig(context.Background(), c), friend) {
		if s.Function.Name == "schedule" || s.Function.Name == "send_file" {
			t.Fatalf("admin-only tool %s exposed to a friend", s.Function.Name)
		}
	}
	c.ApprovalMode = "manual"
	ctx := taskContext(c, c.OwnerID)
	atomic.StoreInt32(&budgetOf(ctx).approveAll, 1)
	// No Telegram transport exists in tests: reaching approval would fail.
	if err := authorizeTool(ctx, "bash", `{"command":"true"}`); err != nil {
		t.Fatalf("approve-all did not skip the card: %v", err)
	}
}

func TestScheduleGrammar(t *testing.T) {
	loc := time.FixedZone("T", 8*3600)
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, loc) // Friday
	cases := []struct {
		in   string
		kind string
		used int
	}{
		{"30m 检查", cronEvery, 1},
		{"every 1h30m 检查", cronEvery, 2},
		{"daily 09:00 早报", cronDaily, 2},
		{"每天 9：30 早报", cronDaily, 2},
		{"weekdays 18:00 下班提醒", cronWeekdays, 2},
		{"in 45m 提醒", cronOnce, 2},
		{"at 09:00 开会", cronOnce, 2},
		{"at 2026-12-01 09:00 续费", cronOnce, 3},
		{"at 2026-12-01T09:00 续费", cronOnce, 2},
	}
	for _, tc := range cases {
		spec, used, err := parseScheduleFields(strings.Fields(tc.in), now, loc)
		if err != nil || spec.Kind != tc.kind || used != tc.used {
			t.Errorf("%q => %+v used=%d err=%v", tc.in, spec, used, err)
		}
	}
	if spec, _, _ := parseScheduleFields([]string{"at", "09:00"}, now, loc); spec.Once.Day() != 7 {
		t.Errorf("past clock time should roll to tomorrow: %v", spec.Once)
	}
	for _, bad := range []string{"10s x", "at 2020-01-01 09:00 x", "daily 25:00 x", "every", "soon x"} {
		if _, _, err := parseScheduleFields(strings.Fields(bad), now, loc); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	weekday := CronJob{Kind: cronWeekdays, At: "09:00"}
	next, _ := nextRunAfter(weekday, now, loc)
	if got := time.Unix(next, 0).In(loc); got.Weekday() != time.Monday || got.Hour() != 9 {
		t.Errorf("weekdays from Friday 10:00 should be Monday 09:00, got %v", got)
	}
	daily := CronJob{Kind: cronDaily, At: "09:00"}
	next, _ = nextRunAfter(daily, now, loc)
	if got := time.Unix(next, 0).In(loc); got.Day() != 7 || got.Hour() != 9 {
		t.Errorf("daily: %v", got)
	}
}

func resetCronForTest(t *testing.T) {
	t.Helper()
	cronMu.Lock()
	prevJobs, prevSeq := cronJobs, cronSeq
	cronJobs, cronSeq = nil, 0
	cronMu.Unlock()
	t.Cleanup(func() {
		cronMu.Lock()
		cronJobs, cronSeq = prevJobs, prevSeq
		cronMu.Unlock()
	})
}

func TestCronDueJobsPostponeWhenBusyAndFinishOnce(t *testing.T) {
	c := runtimeTestConfig(t)
	resetCronForTest(t)
	once, err := addCronJob(c.OwnerID, c.OwnerID, scheduleSpec{Kind: cronOnce, Once: time.Now().Add(time.Hour)}, "一次性提醒", "agent")
	if err != nil {
		t.Fatal(err)
	}
	every, _ := addCronJob(c.OwnerID+1, c.OwnerID, scheduleSpec{Kind: cronEvery, Every: 600}, "定期检查", "user")
	future := time.Now().Add(2 * time.Hour)

	_, finish, ok := beginTask(c.OwnerID)
	if !ok {
		t.Fatal("fixture task did not start")
	}
	due := dueCronJobs(future)
	if len(due) != 1 || due[0].ID != every.ID {
		t.Fatalf("busy chat should be postponed, due=%+v", due)
	}
	finish()
	for _, j := range cronSnapshot() {
		if j.ID == once.ID && (j.Done || j.NextRun != future.Unix()+60) {
			t.Fatalf("busy one-shot job not postponed: %+v", j)
		}
	}
	due = dueCronJobs(future.Add(2 * time.Minute))
	if len(due) != 1 || due[0].ID != once.ID {
		t.Fatalf("one-shot job did not fire after the chat became free: %+v", due)
	}
	for _, j := range cronSnapshot() {
		if j.ID == once.ID && (!j.Done || j.Enabled) {
			t.Fatalf("one-shot job should be finished: %+v", j)
		}
	}
	if err := setCronEnabled(once.ID, 0, true); err == nil {
		t.Fatal("finished one-shot job was resumed")
	}
	if _, err := os.Stat(cronPath()); err != nil {
		t.Fatal("schedule state was not persisted")
	}
}

func TestScheduleToolCreatesListsAndCancels(t *testing.T) {
	c := runtimeTestConfig(t)
	resetCronForTest(t)
	ctx := taskContext(c, c.OwnerID)
	out, err := toolScheduleContext(ctx, map[string]interface{}{"action": "add", "when": "daily 08:30", "task": "汇总服务器状态"})
	if err != nil || !strings.Contains(out, "#1") {
		t.Fatalf("add: %q %v", out, err)
	}
	if _, err := toolScheduleContext(ctx, map[string]interface{}{"action": "add", "when": "daily 08:30 extra words", "task": "x"}); err == nil {
		t.Fatal("trailing words in a rule were accepted")
	}
	list, _ := toolScheduleContext(ctx, map[string]interface{}{"action": "list"})
	if !strings.Contains(list, "汇总服务器状态") || !strings.Contains(list, "自动创建") {
		t.Fatalf("list: %s", list)
	}
	other := taskContext(c, c.OwnerID)
	other = context.WithValue(other, identityContextKey, executionIdentityValue{ChatID: c.OwnerID + 5, UserID: c.OwnerID})
	if _, err := toolScheduleContext(other, map[string]interface{}{"action": "cancel", "id": 1.0}); err == nil {
		t.Fatal("a job from another chat was cancelled")
	}
	if _, err := toolScheduleContext(ctx, map[string]interface{}{"action": "cancel", "id": 1.0}); err != nil {
		t.Fatal(err)
	}
	if len(cronSnapshot()) != 0 {
		t.Fatal("cancel did not remove the job")
	}
}

func TestSuccessfulTaskRemovesBackupsAndGitAddSkipsThem(t *testing.T) {
	c := runtimeTestConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			integrationReply(w, LLMMessage{Role: "assistant", ToolCalls: []ToolCall{
				integrationToolCall("w1", "write_file", map[string]interface{}{"path": "a.txt", "content": "one\n", "intent": "创建文件"}),
				integrationToolCall("w2", "write_file", map[string]interface{}{"path": "a.txt", "content": "two\n", "intent": "覆盖文件"}),
			}})
			return
		}
		integrationReply(w, textMsg("assistant", "完成"))
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mock"}}}
	c.ProviderName, c.Model = "mock", "mock"
	if _, err := runAgentContext(WithExecutionConfig(context.Background(), c), c.OwnerID, c.OwnerID, "写文件", nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(c.Workspace, "*.agentbak.*"))
	if len(matches) != 0 {
		t.Fatalf("backups left behind: %v", matches)
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(repo, "keep.txt"), []byte("x"), 0600)
	os.WriteFile(filepath.Join(repo, "keep.txt.agentbak.abc"), []byte("x"), 0600)
	gitCtx := WithExecutionConfig(context.Background(), Config{Workspace: repo, Backend: "local", MaxToolOutput: 4096, CommandTimeoutSeconds: 10})
	out, err := toolGitContext(gitCtx, map[string]interface{}{"action": "add"})
	if err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	staged, _ := exec.Command("git", "-C", repo, "diff", "--cached", "--name-only").Output()
	if strings.Contains(string(staged), "agentbak") || !strings.Contains(string(staged), "keep.txt") {
		t.Fatalf("staged files: %s", staged)
	}
}

func TestAgentReportsNotesAndIntents(t *testing.T) {
	c := runtimeTestConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			integrationReply(w, LLMMessage{Role: "assistant", Content: "先看看目录结构。", ToolCalls: []ToolCall{
				integrationToolCall("p", "plan", map[string]interface{}{"action": "set", "items": []string{"查看目录"}, "intent": "制定计划"}),
				integrationToolCall("l", "list_dir", map[string]interface{}{"path": ".", "intent": "确认工作区内容"}),
			}})
			return
		}
		integrationReply(w, textMsg("assistant", "目录已检查。"))
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mock"}}}
	c.ProviderName, c.Model = "mock", "mock"
	var mu sync.Mutex
	var events []StepEvent
	_, err := runAgentContext(WithExecutionConfig(context.Background(), c), c.OwnerID, c.OwnerID, "看看目录", nil, 0, func(ev StepEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	var note, intent, plan bool
	for _, ev := range events {
		note = note || ev.Note == "先看看目录结构。"
		intent = intent || (!ev.Done && ev.Tool == "list_dir" && ev.Intent == "确认工作区内容")
		plan = plan || (ev.Done && len(ev.Plan) == 1)
	}
	if !note || !intent || !plan {
		t.Fatalf("note=%v intent=%v plan=%v events=%+v", note, intent, plan, events)
	}
	panel := newProgressPanel(langZH, true, "")
	for _, ev := range events {
		panel.apply(ev)
	}
	html := panel.render()
	for _, want := range []string{"确认工作区内容", "查看目录", "列出目录", "先看看目录结构"} {
		if !strings.Contains(html, want) {
			t.Errorf("panel lacks %q:\n%s", want, html)
		}
	}
	if strings.ContainsAny(html, "📂🤖🔧✅") {
		t.Errorf("panel still contains emoji:\n%s", html)
	}
	if final := panel.renderFinal("已完成"); !strings.Contains(final, "×1") {
		t.Errorf("final summary lacks tool histogram: %s", final)
	}
}

func TestProgressPanelStaysWithinTelegramLimit(t *testing.T) {
	panel := newProgressPanel(langEN, true, "⏰ #1")
	for i := 0; i < 200; i++ {
		panel.apply(StepEvent{Tool: "bash", Detail: strings.Repeat("d", 200), Intent: strings.Repeat("i", 200)})
		panel.apply(StepEvent{Tool: "bash", Done: true, Output: strings.Repeat("<out>\n", 400)})
	}
	if html := panel.render(); utf16Len(html) > telegramChunk || !balancedHTML(html) {
		t.Fatalf("panel too large or unbalanced: %d", utf16Len(html))
	}
}

func TestWebSearchResultParsing(t *testing.T) {
	page := `<div class="result results_links"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F&amp;rut=abc">The <b>Go</b> &amp; docs</a>
<a class="result__snippet" href="x">Learn <b>Go</b> today</a></div>
<div class="result result--ad"><a class="result__a" href="https://duckduckgo.com/y.js?ad_domain=x">Ad</a></div>
<div class="result"><a class="result__a" href="https://example.com/">Example</a></div>`
	got := parseSearchResults(page, 10)
	if len(got) != 2 {
		t.Fatalf("results=%+v", got)
	}
	if got[0].URL != "https://go.dev/doc/" || got[0].Title != "The Go & docs" || got[0].Snippet != "Learn Go today" {
		t.Fatalf("first=%+v", got[0])
	}
	if got[1].URL != "https://example.com/" {
		t.Fatalf("second=%+v", got[1])
	}
	text := htmlToText("<html><script>var x=1</script><p>Hello&nbsp;  world</p><p>Second</p></html>")
	if strings.Contains(text, "var x") || !strings.Contains(text, "Hello world\n") || !strings.Contains(text, "Second") {
		t.Fatalf("htmlToText=%q", text)
	}
}

func TestLanguagePreferenceOrder(t *testing.T) {
	c := runtimeTestConfig(t)
	uid := c.OwnerID
	touchUser(User{ID: uid, FirstName: "x", LanguageCode: "en-US"})
	if langFor(uid) != langEN {
		t.Fatal("Telegram language code ignored")
	}
	if err := updatePrefs(uid, func(p *UserPrefs) { p.Lang = langZH }); err != nil {
		t.Fatal(err)
	}
	if langFor(uid) != langZH {
		t.Fatal("explicit preference ignored")
	}
	prefsState.Lock()
	prefsState.data = nil // force a reload from disk
	prefsState.Unlock()
	if langFor(uid) != langZH {
		t.Fatal("preference not persisted")
	}
	_ = updatePrefs(uid, func(p *UserPrefs) { *p = UserPrefs{} })
	seenMu.Lock()
	delete(seen, uid)
	seenMu.Unlock()
	if langFor(uid) != langZH {
		t.Fatal("default language should be Chinese")
	}
}

var trCallRe = regexp.MustCompile(`tr\([^,()]+(?:\([^)]*\))?,\s*"([a-z_.\-]+)"`)
var verbRe = regexp.MustCompile(`%[-+# 0]*\d*(?:\.\d+)?[a-zA-Z]`)

func TestTranslationsCompleteAndConsistent(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, m := range trCallRe.FindAllStringSubmatch(string(src), -1) {
			if strings.HasSuffix(m[1], ".") || strings.HasSuffix(m[1], "_") {
				continue // dynamic key prefix such as "mode."+mode
			}
			if _, ok := messages[m[1]]; !ok {
				t.Errorf("%s: missing translation %q", f, m[1])
			}
		}
	}
	dynamic := []string{"role.owner", "role.admin", "role.friend", "role.guest", "mode.manual", "mode.auto", "mode.read-only", "steps.on", "steps.off",
		"persona.title", "persona.usage", "persona.updated", "system.title", "system.usage", "system.updated", "list.admin", "list.friend", "list.ban",
		"users.set_admin", "users.unset_admin", "users.set_friend", "users.unset_friend", "users.set_ban", "users.unset_ban"}
	for _, k := range dynamic {
		if _, ok := messages[k]; !ok {
			t.Errorf("missing dynamic translation %q", k)
		}
	}
	for k, v := range messages {
		if v[0] == "" || v[1] == "" {
			t.Errorf("%s lacks a language", k)
		}
		if a, b := verbRe.FindAllString(v[0], -1), verbRe.FindAllString(v[1], -1); strings.Join(a, ",") != strings.Join(b, ",") {
			t.Errorf("%s format verbs differ: %v vs %v", k, a, b)
		}
	}
}

func TestEditHTMLTreatsNotModifiedAsSuccessAndFallsBack(t *testing.T) {
	runtimeTestConfig(t)
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	var mu sync.Mutex
	var modes []string
	httpClient = &http.Client{Transport: uiRoundTripper(func(r *http.Request) (*http.Response, error) {
		var p map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		mode, _ := p["parse_mode"].(string)
		modes = append(modes, mode)
		mu.Unlock()
		body := `{"ok":true,"result":{"message_id":5}}`
		status := 200
		if strings.HasSuffix(r.URL.Path, "/editMessageText") {
			body, status = `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`, 400
		} else if mode == "HTML" {
			body, status = `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`, 400
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: ioNopCloser(body)}, nil
	})}
	if !editHTML(context.Background(), 1, 2, "<b>x</b>", "", nil) {
		t.Fatal("not-modified edit reported as failure")
	}
	if id := sendHTML(context.Background(), 1, "<b>broken", "plain text", nil); id != 5 {
		t.Fatalf("parse error did not fall back to plain text, id=%d", id)
	}
	if len(modes) != 3 || modes[1] != "HTML" || modes[2] != "" {
		t.Fatalf("modes=%v", modes)
	}
}

func ioNopCloser(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }
