package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tgRecorder replaces the Telegram client and records every Bot API call.
type recordedCall struct {
	Method  string
	Payload map[string]interface{}
}

type tgRecorder struct {
	mu    sync.Mutex
	calls []recordedCall
}

func recordTelegram(t *testing.T) *tgRecorder {
	t.Helper()
	rec := &tgRecorder{}
	previous := httpClient
	httpClient = &http.Client{Transport: uiRoundTripper(func(r *http.Request) (*http.Response, error) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		rec.mu.Lock()
		rec.calls = append(rec.calls, recordedCall{Method: method, Payload: payload})
		id := len(rec.calls)
		rec.mu.Unlock()
		body := `{"ok":true,"result":{"message_id":` + itoa(id) + `}}`
		if method == "editMessageText" || method == "deleteMessage" || method == "sendChatAction" {
			body = `{"ok":true,"result":true}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { httpClient = previous })
	return rec
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (r *tgRecorder) snapshot() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedCall(nil), r.calls...)
}

// lastText returns the text of the latest call of a method whose text contains marker.
func (r *tgRecorder) find(method, marker string) (recordedCall, bool) {
	calls := r.snapshot()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == method {
			if text, _ := calls[i].Payload["text"].(string); strings.Contains(text, marker) {
				return calls[i], true
			}
		}
	}
	return recordedCall{}, false
}

func allDisplay(on bool) display {
	d := display{}
	for _, it := range displayItems {
		d[it.Key] = on
	}
	return d
}

func TestFooterFollowsSwitches(t *testing.T) {
	f := footerData{Elapsed: 83 * time.Second, Stats: taskStats{Calls: 6, Tools: 8, Commands: 3, Prompt: 10100, Completion: 2200}, Balance: "¥23.45", Model: "deepseek-chat"}
	full := renderFooter(langZH, allDisplay(true), f)
	for _, want := range []string{"耗时 <b>1m23s</b>", "调用 <b>6</b>", "工具 <b>8</b>", "命令 <b>3</b>", "Token <b>12.3k</b>", "余额 <b>¥23.45</b>", "<code>deepseek-chat</code>"} {
		if !strings.Contains(full, want) {
			t.Errorf("footer lacks %q: %s", want, full)
		}
	}
	if !balancedHTML(full) {
		t.Fatalf("unbalanced footer: %s", full)
	}
	d := allDisplay(true)
	d["tokens"], d["balance"] = false, false
	if got := renderFooter(langZH, d, f); strings.Contains(got, "Token") || strings.Contains(got, "余额") {
		t.Fatalf("hidden items still shown: %s", got)
	}
	if got := renderFooter(langZH, allDisplay(false), f); got != "" {
		t.Fatalf("footer should be empty when everything is off: %s", got)
	}
	if got := renderFooter(langEN, allDisplay(true), footerData{Elapsed: time.Second}); strings.Contains(got, "Calls") || !strings.Contains(got, "Time") {
		t.Fatalf("zero counters must be omitted: %s", got)
	}
}

func TestShowCommandChangesDisplay(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	uid := c.OwnerID
	friend := uid + 7
	cfgMu.Lock()
	cfg.Friends = []int64{friend}
	cfgMu.Unlock()

	// Balance is hidden by default; everything else but the model name is shown.
	if d := displayFor(uid, configSnapshot()); !d["tokens"] || d["balance"] || !d["think"] || d["model"] {
		t.Fatalf("unexpected defaults: %v", d)
	}
	handleShowCommand(uid, uid, "token off")
	if displayFor(uid, configSnapshot())["tokens"] {
		t.Fatal("/show token off did not hide tokens")
	}
	handleShowCommand(uid, uid, "耗时 余额 关 模型 开")
	d := displayFor(uid, configSnapshot())
	if d["time"] || d["balance"] || !d["model"] {
		t.Fatalf("mixed switches not applied: %v", d)
	}
	handleShowCommand(uid, uid, "思考") // no state: toggle
	if displayFor(uid, configSnapshot())["think"] {
		t.Fatal("toggle did not switch thinking off")
	}
	handleShowCommand(uid, uid, "output:on")
	if !stepsDetailed(uid, configSnapshot()) {
		t.Fatal("output preview should map to the /steps preference")
	}
	handleShowCommand(uid, uid, "简洁")
	d = displayFor(uid, configSnapshot())
	if !d["time"] || !d["tokens"] || d["calls"] || d["tools"] || d["balance"] || d["output"] || !d["keep"] {
		t.Fatalf("minimal preset wrong: %v", d)
	}
	handleShowCommand(uid, uid, "pure")
	if d := displayFor(uid, configSnapshot()); hasFooter(d) || d["keep"] || d["think"] {
		t.Fatalf("pure preset left something on: %v", d)
	}
	handleShowCommand(uid, uid, "默认")
	if d := displayFor(uid, configSnapshot()); !d["tokens"] || !d["think"] || d["output"] || d["model"] {
		t.Fatalf("reset did not restore defaults: %v", d)
	}
	if _, ok := rec.find("sendMessage", "显示设置"); !ok {
		t.Fatal("display panel was not sent")
	}

	before := displayFor(uid, configSnapshot())
	handleShowCommand(uid, uid, "nonsense off")
	if _, ok := rec.find("sendMessage", "未知项"); !ok {
		t.Fatal("unknown item not reported")
	}
	if after := displayFor(uid, configSnapshot()); len(after) != len(before) || after["tokens"] != before["tokens"] {
		t.Fatal("unknown item changed settings")
	}

	// Friends never see the provider balance and cannot switch it on.
	if displayFor(friend, configSnapshot())["balance"] {
		t.Fatal("friend sees balance")
	}
	handleShowCommand(friend, friend, "balance on")
	if displayFor(friend, configSnapshot())["balance"] {
		t.Fatal("friend enabled balance")
	}
	for _, it := range visibleDisplayItems(friend, "") {
		if it.Key == "balance" {
			t.Fatal("balance switch offered to a friend")
		}
	}

	// Config defaults apply when the user has not chosen.
	cfgMu.Lock()
	cfg.Display = map[string]bool{"calls": false, "output": true}
	cfgMu.Unlock()
	if d := displayFor(uid, configSnapshot()); d["calls"] || !d["output"] {
		t.Fatalf("config defaults ignored: %v", d)
	}
	bad := configSnapshot()
	bad.Display = map[string]bool{"bogus": true}
	if err := validateConfig(bad); err == nil {
		t.Fatal("unknown display key accepted")
	}
}

func TestDisplayCallbackTogglesAndPresets(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	uid := c.OwnerID
	if toast := handleDisplayCallback(uid, 5, uid, "show:t:tokens"); !strings.Contains(toast, "关") {
		t.Fatalf("toast=%q", toast)
	}
	if displayFor(uid, configSnapshot())["tokens"] {
		t.Fatal("button did not toggle tokens")
	}
	edit, ok := rec.find("editMessageText", "显示设置")
	if !ok {
		t.Fatal("panel not refreshed")
	}
	raw, _ := json.Marshal(edit.Payload["reply_markup"])
	if !strings.Contains(string(raw), "○ Token") || !strings.Contains(string(raw), "● 耗时") || !strings.Contains(string(raw), "show:p:pure") {
		t.Fatalf("keyboard does not reflect state: %s", raw)
	}
	handleDisplayCallback(uid, 5, uid, "show:p:pure")
	if hasFooter(displayFor(uid, configSnapshot())) {
		t.Fatal("pure preset button failed")
	}
	if toast := handleDisplayCallback(uid, 5, uid, "show:t:bogus"); toast != "" {
		t.Fatalf("unknown switch handled: %q", toast)
	}
	if text := displayPanelText(uid); !balancedHTML(text) || !strings.Contains(text, "统计已全部隐藏") {
		t.Fatalf("panel text: %s", text)
	}
}

func TestStripEmojiKeepsSymbolsAndCode(t *testing.T) {
	cases := map[string]string{
		"✅ 完成":            "✓ 完成",
		"🚀 部署完成":          "部署完成",
		"注意 ⚠️ 风险":        "注意 风险",
		"家庭 👨‍👩‍👧 成员":     "家庭 成员",
		"1️⃣ 第一步":         "1 第一步",
		"中国 🇨🇳":           "中国",
		"➡️ 下一步":          "→ 下一步",
		"好的😀。":            "好的。",
		"❌ 失败 ❤️":         "✗ 失败",
		"保持 ✓ ✗ → • ★ 符号": "保持 ✓ ✗ → • ★ 符号",
		"## 🎯 目标":         "## 目标",
		"- 🔥 热门":          "- 热门",
	}
	for in, want := range cases {
		if got := stripEmoji(in); got != want {
			t.Errorf("stripEmoji(%q)=%q want %q", in, got, want)
		}
	}
	md := "标题 🎉\n```\necho 🚀\n```\n`🚀` 外面🚀"
	want := "标题\n```\necho 🚀\n```\n`🚀` 外面"
	if got := stripEmojiMarkdown(md); got != want {
		t.Fatalf("markdown cleaning touched code:\n%q\nwant\n%q", got, want)
	}
}

func fastRetries(t *testing.T) {
	t.Helper()
	base, limit := llmRetryBase, llmRetryCap
	llmRetryBase, llmRetryCap = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { llmRetryBase, llmRetryCap = base, limit })
}

func TestLLMRetriesTransientFailuresSilently(t *testing.T) {
	c := runtimeTestConfig(t)
	fastRetries(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			io.WriteString(w, `{"error":{"message":"server overloaded, please retry"}}`)
		case 2:
			io.WriteString(w, `<html>bad gateway</html>`)
		case 3:
			w.WriteHeader(http.StatusBadGateway)
		case 4:
			io.WriteString(w, `{"choices":[]}`)
		default:
			integrationReply(w, textMsg("assistant", "ok"))
		}
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mock"}}}
	c.ProviderName, c.Model = "mock", "mock"
	ctx := withTaskBudget(WithExecutionConfig(t.Context(), c), c)
	out, err := chatContext(ctx, []LLMMessage{textMsg("user", "hi")}, nil)
	if err != nil || textOf(out.Choices[0].Message) != "ok" {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if s := statsOf(ctx); s.Retries != 4 || s.Calls != 1 || requests.Load() != 5 {
		t.Fatalf("stats=%+v requests=%d", s, requests.Load())
	}
}

func TestLLMDoesNotRetryPermanentErrors(t *testing.T) {
	c := runtimeTestConfig(t)
	fastRetries(t)
	for name, handler := range map[string]http.HandlerFunc{
		"bad request": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"maximum context length exceeded"}}`)
		},
		"invalid key in 200": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"error":{"message":"Invalid API key","type":"authentication_error"}}`)
		},
	} {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			handler(w, r)
		}))
		c.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mock"}}}
		c.ProviderName, c.Model = "mock", "mock"
		if _, err := chatContext(WithExecutionConfig(t.Context(), c), []LLMMessage{textMsg("user", "hi")}, nil); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if requests.Load() != 1 {
			t.Errorf("%s: permanent error retried %d times", name, requests.Load())
		}
		server.Close()
	}

	// When every attempt fails the error says how often it was retried.
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mock"}}}
	c.LLMRetries = 2
	_, err := chatContext(WithExecutionConfig(t.Context(), c), []LLMMessage{textMsg("user", "hi")}, nil)
	if err == nil || !strings.Contains(err.Error(), "2 次") || requests.Load() != 3 {
		t.Fatalf("err=%v requests=%d", err, requests.Load())
	}
}

func TestRetryDelayBacksOffWithinCap(t *testing.T) {
	if d := retryDelay(1, 3*time.Second); d != 3*time.Second {
		t.Fatalf("Retry-After ignored: %s", d)
	}
	if d := retryDelay(1, 5*time.Minute); d != time.Minute {
		t.Fatalf("Retry-After not capped: %s", d)
	}
	prev := time.Duration(0)
	for n := 1; n <= 8; n++ {
		d := retryDelay(n, 0)
		if d > llmRetryCap+llmRetryCap/4 {
			t.Fatalf("delay %d exceeds cap: %s", n, d)
		}
		if n <= 4 && d < prev {
			t.Fatalf("delay %d shrank: %s < %s", n, d, prev)
		}
		prev = d
	}
}

// TestTaskAnswerHasNoButtonsAndShowsStatistics runs one full task against a
// mocked model and Telegram: the answer carries no keyboard, emoji are gone,
// the statistics line follows the user's switches and the thinking appears
// in the panel.
func TestTaskAnswerHasNoButtonsAndShowsStatistics(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	var chats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/balance" {
			io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"23.45"}]}`)
			return
		}
		if chats.Add(1)%2 == 1 {
			integrationReply(w, LLMMessage{Role: "assistant", Content: "先确认环境。", ReasoningContent: "用户想确认 shell 可用，先 echo 一下。", ToolCalls: []ToolCall{
				integrationToolCall("c1", "bash", map[string]interface{}{"command": "echo hi", "intent": "确认 shell 可用"}),
			}})
			return
		}
		integrationReply(w, textMsg("assistant", "## 结果\n✅ 命令执行成功 🚀"))
	}))
	defer server.Close()
	cfgMu.Lock()
	cfg.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mock"}}}
	cfg.ProviderName, cfg.Model = "mock", "mock"
	cfgMu.Unlock()
	uid := c.OwnerID
	if err := setDisplay(uid, map[string]bool{"balance": true}); err != nil { // off by default
		t.Fatal(err)
	}

	if !runUserTaskOpts(uid, uid, "检查 shell", nil, taskOptions{}) {
		t.Fatal("task did not start")
	}
	answer, ok := rec.find("sendMessage", "结果")
	if !ok {
		t.Fatalf("answer not sent: %+v", rec.snapshot())
	}
	text := answer.Payload["text"].(string)
	if _, has := answer.Payload["reply_markup"]; has {
		t.Fatalf("answer still has buttons: %v", answer.Payload["reply_markup"])
	}
	for _, want := range []string{"<b>▍结果</b>", "✓ 命令执行成功", "耗时 <b>", "调用 <b>2</b>", "工具 <b>1</b>", "命令 <b>1</b>", "余额 <b>¥23.45</b>"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer lacks %q:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "✅🚀") {
		t.Errorf("answer still contains emoji:\n%s", text)
	}
	final, ok := rec.find("editMessageText", "已完成")
	if !ok {
		t.Fatal("panel not finalised")
	}
	if panel := final.Payload["text"].(string); !strings.Contains(panel, "执行命令 ×1") || !strings.Contains(panel, "先 echo 一下") {
		t.Errorf("final panel lacks steps or thinking:\n%s", panel)
	}
	for _, call := range rec.snapshot() {
		if _, has := call.Payload["reply_markup"]; has && call.Method != "setMyCommands" {
			t.Errorf("%s carried a keyboard during the task: %v", call.Method, call.Payload["reply_markup"])
		}
	}

	// Pure display: only the answer, no statistics, and the panel is removed.
	if err := applyDisplayPreset(uid, "pure"); err != nil {
		t.Fatal(err)
	}
	before := len(rec.snapshot())
	runUserTaskOpts(uid, uid, "再检查一次", nil, taskOptions{})
	var answerText string
	deleted := false
	for _, call := range rec.snapshot()[before:] {
		if text, _ := call.Payload["text"].(string); call.Method == "sendMessage" && strings.Contains(text, "结果") {
			answerText = text
		}
		deleted = deleted || call.Method == "deleteMessage"
	}
	if answerText == "" || strings.Contains(answerText, "耗时") || strings.Contains(answerText, "Token") {
		t.Fatalf("pure answer still has statistics: %q", answerText)
	}
	if !deleted {
		t.Fatal("panel was not removed with keep off")
	}
}

func TestTelegramCallsRetrySilently(t *testing.T) {
	runtimeTestConfig(t)
	previousClient, previousBase := httpClient, tgRetryBase
	t.Cleanup(func() { httpClient, tgRetryBase = previousClient, previousBase })
	tgRetryBase = time.Millisecond
	var requests atomic.Int32
	httpClient = &http.Client{Transport: uiRoundTripper(func(r *http.Request) (*http.Response, error) {
		n := requests.Add(1)
		switch n {
		case 1:
			return nil, io.ErrUnexpectedEOF
		case 2, 3:
			return &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader(`{"ok":false,"description":"Bad Gateway"}`)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":9}}`)), Header: make(http.Header)}, nil
	})}
	if id := sendMessage(1, "hello", nil); id != 9 || requests.Load() != 4 {
		t.Fatalf("id=%d requests=%d", id, requests.Load())
	}
	requests.Store(0)
	httpClient = &http.Client{Transport: uiRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"ok":false,"description":"Bad Request: chat not found"}`)), Header: make(http.Header)}, nil
	})}
	if _, err := tgDo(t.Context(), "sendMessage", map[string]interface{}{"chat_id": 1, "text": "x"}); err == nil || requests.Load() != 1 {
		t.Fatalf("definitive 400 retried: err=%v requests=%d", err, requests.Load())
	}
}
