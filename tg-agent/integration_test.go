package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func integrationToolCall(id, name string, args interface{}) ToolCall {
	var call ToolCall
	call.ID = id
	call.Type = "function"
	call.Function.Name = name
	data, _ := json.Marshal(args)
	call.Function.Arguments = string(data)
	return call
}
func integrationReply(w http.ResponseWriter, m LLMMessage) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"message": m, "finish_reason": "stop"}}})
}
func TestIntegrationAgentExecutesOrderedToolsAndRestoresConversation(t *testing.T) {
	c := runtimeTestConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		switch calls.Add(1) {
		case 1:
			integrationReply(w, LLMMessage{Role: "assistant", ToolCalls: []ToolCall{
				integrationToolCall("write", "write_file", map[string]interface{}{"path": "example.txt", "content": "before\n"}),
				integrationToolCall("edit", "edit_file", map[string]interface{}{"path": "example.txt", "old_string": "before", "new_string": "after"}),
				integrationToolCall("read", "read_file", map[string]interface{}{"path": "example.txt"}),
			}})
		case 2:
			var tools []LLMMessage
			for _, m := range request.Messages {
				if m.Role == "tool" {
					tools = append(tools, m)
				}
			}
			if len(tools) != 3 || tools[0].ToolCallID != "write" || tools[1].ToolCallID != "edit" || tools[2].ToolCallID != "read" || !strings.Contains(textOf(tools[2]), "after") {
				t.Errorf("tool chain not preserved: %+v", tools)
			}
			integrationReply(w, textMsg("assistant", "修改完成，文件内容已读取验证。"))
		default:
			integrationReply(w, textMsg("assistant", "已恢复此前对话。"))
		}
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mock"}}}
	c.ProviderName = "mock"
	c.Model = "mock"
	ctx := WithExecutionConfig(context.Background(), c)
	out, err := runAgentContext(ctx, 1001, 1001, "创建并编辑 example.txt，再读取验证", nil, 0, nil)
	if err != nil || !strings.Contains(out, "修改完成") {
		t.Fatalf("out=%s err=%v", out, err)
	}
	data, err := os.ReadFile(filepath.Join(c.Workspace, "example.txt"))
	if err != nil || string(data) != "after\n" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	sessMu.Lock()
	delete(sessions, 1001)
	sessMu.Unlock()
	restored := getHistory(1001)
	if len(restored) != 6 || restored[0].Role != "user" || restored[5].Role != "assistant" {
		t.Fatalf("restored chain=%+v", restored)
	}
}
func TestIntegrationBudgetEndsWithSummaryWithoutExtraTools(t *testing.T) {
	c := runtimeTestConfig(t)
	c.MaxSteps = 1
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		if calls.Add(1) == 1 {
			integrationReply(w, LLMMessage{Role: "assistant", ToolCalls: []ToolCall{integrationToolCall("read", "list_dir", map[string]interface{}{"path": "."})}})
			return
		}
		if len(request.Tools) != 0 {
			t.Error("budget summary still contains executable tools")
		}
		integrationReply(w, textMsg("assistant", "已查看目录，剩余修改尚未执行。"))
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mock"}}}
	c.ProviderName = "mock"
	c.Model = "mock"
	out, err := runAgentContext(WithExecutionConfig(context.Background(), c), 1001, 1001, "检查目录再完成修改", nil, 0, nil)
	if err != nil || calls.Load() != 2 || !strings.Contains(out, "达到本次执行上限") {
		t.Fatalf("out=%s err=%v calls=%d", out, err, calls.Load())
	}
}
func TestIntegrationInterruptedToolBatchCanResume(t *testing.T) {
	c := runtimeTestConfig(t)
	ctx, cancel := context.WithCancel(WithExecutionConfig(context.Background(), c))
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		integrationReply(w, LLMMessage{Role: "assistant", ToolCalls: []ToolCall{
			integrationToolCall("one", "list_dir", map[string]interface{}{"path": "."}),
			integrationToolCall("two", "list_dir", map[string]interface{}{"path": "."}),
		}})
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mock"}}}
	c.ProviderName = "mock"
	c.Model = "mock"
	ctx = WithExecutionConfig(ctx, c)
	_, err := runAgentContext(ctx, 1001, 1001, "检查两个目录", nil, 0, func(e StepEvent) {
		if e.Done && !e.Final {
			cancel()
		}
	})
	if err == nil {
		t.Fatal("cancelled task returned success")
	}
	history := getHistory(1001)
	results := map[string]bool{}
	for _, m := range history {
		if m.Role == "tool" {
			results[m.ToolCallID] = true
		}
	}
	if !results["one"] || !results["two"] {
		t.Fatalf("interrupted batch missing tool results: %+v", history)
	}
	if len(history) == 0 || !strings.Contains(textOf(history[len(history)-1]), "任务中断") {
		t.Fatal("interruption was not recorded")
	}
}
func TestIntegrationLLMRetryableFailureRetries(t *testing.T) {
	c := runtimeTestConfig(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
			return
		}
		integrationReply(w, textMsg("assistant", "ok"))
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mock"}}}
	c.ProviderName = "mock"
	c.Model = "mock"
	out, err := chatContext(WithExecutionConfig(context.Background(), c), []LLMMessage{textMsg("user", "hello")}, nil)
	if err != nil || requests.Load() != 2 || textOf(out.Choices[0].Message) != "ok" {
		t.Fatalf("retry requests=%d err=%v", requests.Load(), err)
	}
}

func TestIntegrationProjectInstructionsRespectFriendPaths(t *testing.T) {
	c := runtimeTestConfig(t)
	friendID := c.OwnerID + 1
	cfgMu.Lock()
	cfg.Friends = []int64{friendID}
	cfgMu.Unlock()
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("PRIVATE_SENTINEL_DO_NOT_LOAD"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(c.Workspace, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(WithExecutionConfig(context.Background(), c), identityContextKey, executionIdentityValue{ChatID: friendID, UserID: friendID})
	if got := projectInstructionsContext(ctx); strings.Contains(got, "PRIVATE_SENTINEL") {
		t.Fatal("automatic project instructions escaped friend workspace")
	}
}
