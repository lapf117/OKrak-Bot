package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runtimeTestConfig(t *testing.T) Config {
	t.Helper()
	cfgMu.Lock()
	old := cloneConfig(cfg)
	oldBase := baseDir
	baseDir = t.TempDir()
	cfg = Config{OwnerID: 1001, Token: "test-token-only", Workspace: baseDir, SkillsDir: baseDir + "/skills", Backend: "local", ApprovalMode: "auto", MaxSteps: 8, MaxHistory: 3, MaxSubagents: 2, TaskTimeoutSeconds: 30, CommandTimeoutSeconds: 5, ApprovalTimeoutSeconds: 5, MaxToolOutput: 12000, FriendTools: "read"}
	c := cloneConfig(cfg)
	cfgMu.Unlock()
	t.Cleanup(func() {
		stopAllTasks()
		cfgMu.Lock()
		cfg = old
		baseDir = oldBase
		cfgMu.Unlock()
		sessMu.Lock()
		sessions = map[int64]*Session{}
		lastThink = map[int64]string{}
		sessMu.Unlock()
	})
	return c
}
func TestRuntimeHistoryKeepsCompleteTurns(t *testing.T) {
	msgs := []LLMMessage{textMsg("user", "first"), {Role: "assistant", ToolCalls: []ToolCall{{ID: "a"}}}, {Role: "tool", ToolCallID: "a", Content: "done"}, textMsg("assistant", "first done"), textMsg("user", "second"), textMsg("assistant", "second done")}
	trimmed := trimHistoryLimit(msgs, 1)
	if len(trimmed) != 2 || textOf(trimmed[0]) != "second" {
		t.Fatalf("trimmed=%+v", trimmed)
	}
}
func TestRuntimePersistsPrivateHistory(t *testing.T) {
	runtimeTestConfig(t)
	setHistory(1001, []LLMMessage{textMsg("user", "remember"), textMsg("assistant", "ok")})
	info, err := os.Stat(sessionPath(1001))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	sessMu.Lock()
	delete(sessions, 1001)
	sessMu.Unlock()
	if history := getHistory(1001); len(history) != 2 || textOf(history[0]) != "remember" {
		t.Fatalf("persisted history=%+v", history)
	}
	resetHistory(1001)
	if _, err = os.Stat(sessionPath(1001)); !os.IsNotExist(err) {
		t.Fatal("reset did not remove history")
	}
}
func TestRuntimeStopsAndRejectsDuplicateTask(t *testing.T) {
	runtimeTestConfig(t)
	ctx, finish, ok := beginTask(1001)
	if !ok {
		t.Fatal("begin failed")
	}
	defer finish()
	if _, _, duplicate := beginTask(1001); duplicate {
		t.Fatal("duplicate task admitted")
	}
	if !stopTask(1001) {
		t.Fatal("stop failed")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel context")
	}
}
func TestRuntimeReadonlyRejectsDirectShell(t *testing.T) {
	c := runtimeTestConfig(t)
	c.ApprovalMode = "read-only"
	ctx := context.WithValue(WithExecutionConfig(context.Background(), c), identityContextKey, executionIdentityValue{ChatID: 1001, UserID: 1001})
	if err := authorizeTool(ctx, "bash", `{"command":"echo forbidden"}`); err == nil {
		t.Fatal("readonly shell admitted")
	}
	if _, err := runToolContext(ctx, "write_file", `{"path":"forbidden.txt","content":"bad"}`); err == nil {
		t.Fatal("direct dispatch bypassed read-only policy")
	}
	if _, err := os.Stat(c.Workspace + "/forbidden.txt"); !os.IsNotExist(err) {
		t.Fatal("forbidden file exists")
	}
}
func TestRuntimeChildCannotAcquireOwnerIdentity(t *testing.T) {
	c := runtimeTestConfig(t)
	ctx := WithExecutionConfig(context.Background(), c)
	if _, err := spawnAgentContext(ctx, "do something"); err == nil {
		t.Fatal("identity-free subagent admitted")
	}
	ctx = context.WithValue(ctx, identityContextKey, executionIdentityValue{ChatID: 1001, UserID: 1001, Depth: 3})
	ctx = context.WithValue(ctx, budgetContextKey, &executionBudget{Steps: 10})
	if _, err := spawnAgentContext(ctx, "do something"); err == nil {
		t.Fatal("depth limit bypassed")
	}
}
func TestRuntimeHTTPErrorAndCancellation(t *testing.T) {
	c := runtimeTestConfig(t)
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid API key"}}`)
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "test", BaseURL: server.URL, Models: []string{"test"}}}
	c.ProviderName = "test"
	c.Model = "test"
	ctx := WithExecutionConfig(context.Background(), c)
	if _, err := chatContext(ctx, []LLMMessage{textMsg("user", "hello")}, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err=%v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("non-retryable 401 retried %d times", got)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := chatContext(canceled, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("cancelled request reached server %d", got)
	}
}

type runtimeRoundTrip func(*http.Request) (*http.Response, error)

func (f runtimeRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRuntimeApprovalIsOneTimeAndBoundToPrincipal(t *testing.T) {
	c := runtimeTestConfig(t)
	c.ApprovalMode = "manual"
	old := httpClient
	t.Cleanup(func() { httpClient = old })
	sent := make(chan string, 1)
	httpClient = &http.Client{Transport: runtimeRoundTrip(func(r *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var p struct {
				ReplyMarkup struct {
					InlineKeyboard [][]Btn `json:"inline_keyboard"`
				} `json:"reply_markup"`
			}
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatal(err)
			}
			select {
			case sent <- strings.TrimPrefix(p.ReplyMarkup.InlineKeyboard[0][0]["callback_data"], "approve:"):
			default:
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":7}}`))}, nil
	})}
	ctx, cancel := context.WithCancel(context.WithValue(WithExecutionConfig(context.Background(), c), identityContextKey, executionIdentityValue{ChatID: 1001, UserID: 1001}))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- authorizeTool(ctx, "bash", `{"command":"echo approved"}`) }()
	var id string
	select {
	case id = <-sent:
	case <-time.After(time.Second):
		t.Fatal("approval did not arrive")
	}
	// A different principal cannot consume or approve the pending capability.
	handleApprovalCallback(CallbackQuery{Data: "approve:" + id, From: User{ID: 9999}, Message: &Message{Chat: Chat{ID: 1001}}})
	approvalState.Lock()
	_, pending := approvalState.pending[id]
	approvalState.Unlock()
	if !pending {
		t.Fatal("wrong principal consumed approval")
	}
	handleApprovalCallback(CallbackQuery{Data: "approve:" + id, From: User{ID: 1001}, Message: &Message{Chat: Chat{ID: 1001}}})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("correct approval did not resume")
	}
	approvalState.Lock()
	_, pending = approvalState.pending[id]
	approvalState.Unlock()
	if pending {
		t.Fatal("one-time approval remained reusable")
	}
	if err := requestToolApproval(ctx, "write_file", strings.Repeat("x", 2201)); err == nil {
		t.Fatal("oversized review silently truncated")
	}
}

func TestRuntimeCapturedCredentialsRemainRedacted(t *testing.T) {
	c := runtimeTestConfig(t)
	c.Provider = []Provider{{Name: "test", APIKey: "captured-secret-value"}}
	ctx := WithExecutionConfig(context.Background(), c)
	cfgMu.Lock()
	cfg.Provider = []Provider{{Name: "test", APIKey: "current-secret-value"}}
	cfgMu.Unlock()
	got := redactExecutionSecrets(ctx, "captured-secret-value current-secret-value")
	if strings.Contains(got, "secret-value") {
		t.Fatalf("snapshot credential leaked: %s", got)
	}
}

func TestRuntimeRevokedPrincipalCannotCallModel(t *testing.T) {
	c := runtimeTestConfig(t)
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "test", BaseURL: server.URL, Models: []string{"test"}}}
	c.ProviderName = "test"
	c.Model = "test"
	ctx := context.WithValue(WithExecutionConfig(context.Background(), c), identityContextKey, executionIdentityValue{ChatID: 1001, UserID: 1001})
	cfgMu.Lock()
	cfg.Banned = []int64{1001}
	cfgMu.Unlock()
	if _, err := chatContext(ctx, []LLMMessage{textMsg("user", "hello")}, nil); err == nil {
		t.Fatal("revoked principal called model")
	}
	if atomic.LoadInt32(&requests) != 0 {
		t.Fatal("revoked request reached model")
	}
}

func TestRuntimeInterruptedHistoryRepairsToolBatch(t *testing.T) {
	messages := []LLMMessage{textMsg("user", "work"), {Role: "assistant", ToolCalls: []ToolCall{{ID: "first"}, {ID: "second"}}}, {Role: "tool", ToolCallID: "first", Content: "done"}}
	got := completeToolChain(messages)
	if len(got) != 4 || got[3].Role != "tool" || got[3].ToolCallID != "second" {
		t.Fatalf("tool chain not repaired: %+v", got)
	}
	if !strings.Contains(textOf(got[3]), "未完成") {
		t.Fatal("interrupted action incorrectly marked successful")
	}
}

func TestRuntimeShutdownRejectsLateTask(t *testing.T) {
	runtimeTestConfig(t)
	previous := telegramContext
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	telegramContext = ctx
	t.Cleanup(func() { telegramContext = previous })
	if _, _, ok := beginTask(1001); ok {
		t.Fatal("task started after shutdown")
	}
}
