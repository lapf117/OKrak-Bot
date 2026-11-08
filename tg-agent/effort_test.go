package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func effortConfig(model string, efforts ...string) Config {
	return Config{ProviderName: "p", Model: model, Provider: []Provider{{Name: "p", BaseURL: "https://api.example.com", Models: []string{model}, Efforts: efforts}}}
}

func TestEffortOptionsFollowProviderAndModel(t *testing.T) {
	cases := []struct {
		model  string
		want   string
		source effortSource
	}{
		{"deepseek-chat", "", effortFromModel},
		{"deepseek-reasoner", "", effortFromModel},
		{"gpt-5-mini", "minimal low medium high", effortFromModel},
		{"openai/gpt-5.1:online", "low medium high", effortFromModel},
		{"gpt-5.2", "low medium high xhigh", effortFromModel},
		{"o4-mini", "low medium high", effortFromModel},
		{"grok-3-mini", "low high", effortFromModel},
		{"gemini-2.5-pro", "low medium high", effortFromModel},
		{"gpt-4o", "", effortFromModel},
		{"qwen-max", "", effortFromModel},
		{"some-new-model", "minimal low medium high", effortUnknown},
	}
	for _, tc := range cases {
		opts, src := effortOptions(effortConfig(tc.model))
		if strings.Join(opts, " ") != tc.want || src != tc.source {
			t.Errorf("%s: got %v (%d), want %q (%d)", tc.model, opts, src, tc.want, tc.source)
		}
	}
	// providers[].efforts wins over the built-in table.
	opts, src := effortOptions(effortConfig("deepseek-chat", "High", "max", "off", "max"))
	if strings.Join(opts, " ") != "high max" || src != effortFromConfig {
		t.Fatalf("config efforts: %v %d", opts, src)
	}
}

func TestEffectiveEffortOnlySendsSupportedValues(t *testing.T) {
	for _, tc := range []struct{ model, set, want string }{
		{"deepseek-chat", "high", ""},
		{"gpt-5", "minimal", "minimal"},
		{"gpt-5.1", "minimal", ""},
		{"o3", "high", "high"},
		{"unknown-x", "xhigh", "xhigh"},
		{"gpt-5", "", ""},
	} {
		c := effortConfig(tc.model)
		c.ReasoningEffort = tc.set
		if got := effectiveEffort(c); got != tc.want {
			t.Errorf("%s/%s: got %q want %q", tc.model, tc.set, got, tc.want)
		}
	}
	bad := effortConfig("x", "Hi!")
	bad.OwnerID, bad.Workspace, bad.Backend, bad.ApprovalMode, bad.FriendTools = 1, "/tmp", "local", "manual", "read"
	bad.MaxSteps, bad.MaxHistory, bad.CommandTimeoutSeconds, bad.TaskTimeoutSeconds, bad.ApprovalTimeoutSeconds, bad.MaxToolOutput, bad.MaxSubagents = 1, 1, 1, 1, 1, 1024, 1
	if err := validateConfig(bad); err == nil || !strings.Contains(err.Error(), "efforts") {
		t.Fatalf("invalid efforts accepted: %v", err)
	}
}

// The request carries reasoning_effort only when supported, and a provider
// that rejects the parameter gets the request again without it.
func TestChatDropsRejectedEffortSilently(t *testing.T) {
	c := runtimeTestConfig(t)
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		effort, _ := req["reasoning_effort"].(string)
		mu.Lock()
		sent = append(sent, effort)
		mu.Unlock()
		if effort != "" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unrecognized request argument supplied: reasoning_effort"}}`)
			return
		}
		integrationReply(w, textMsg("assistant", "ok"))
	}))
	defer server.Close()
	c.Provider = []Provider{{Name: "mock", BaseURL: server.URL, Models: []string{"mystery-model", "deepseek-chat"}}}
	c.ProviderName, c.Model, c.ReasoningEffort = "mock", "mystery-model", "high"
	ctx := withTaskBudget(WithExecutionConfig(t.Context(), c), c)
	for i := 0; i < 2; i++ {
		if out, err := chatContext(ctx, []LLMMessage{textMsg("user", "hi")}, nil); err != nil || textOf(out.Choices[0].Message) != "ok" {
			t.Fatalf("call %d: out=%v err=%v", i, out, err)
		}
	}
	if strings.Join(sent, ",") != "high,," {
		t.Fatalf("efforts sent: %q", sent)
	}
	if !effortWasRejected(c) || effectiveEffort(c) != "" {
		t.Fatal("rejection not remembered")
	}

	// A model known not to take the parameter never receives it.
	sent = nil
	c.Model = "deepseek-chat"
	if _, err := chatContext(WithExecutionConfig(t.Context(), c), []LLMMessage{textMsg("user", "hi")}, nil); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != "" {
		t.Fatalf("deepseek got reasoning_effort: %q", sent)
	}
}

func TestEffortMenuListsModelChoices(t *testing.T) {
	c := runtimeTestConfig(t)
	rec := recordTelegram(t)
	uid := c.OwnerID
	set := func(model, effort string) {
		cfgMu.Lock()
		cfg.Provider = []Provider{{Name: "p", BaseURL: "https://api.example.com", Models: []string{"deepseek-chat", "gpt-5"}}}
		cfg.ProviderName, cfg.Model, cfg.ReasoningEffort = "p", model, effort
		cfgMu.Unlock()
	}
	buttons := func(kb map[string]interface{}) string {
		raw, _ := json.Marshal(kb)
		return string(raw)
	}

	set("deepseek-chat", "")
	text, kb := effortPanel(langZH, configSnapshot())
	if !strings.Contains(text, "不支持") || !strings.Contains(text, "deepseek-reasoner") || strings.Contains(buttons(kb), "set:effort:high") {
		t.Fatalf("deepseek menu:\n%s\n%s", text, buttons(kb))
	}
	set("gpt-5", "high")
	text, kb = effortPanel(langZH, configSnapshot())
	for _, lv := range []string{"off", "minimal", "low", "medium", "high"} {
		if !strings.Contains(buttons(kb), "set:effort:"+lv) {
			t.Errorf("gpt-5 menu lacks %s: %s", lv, buttons(kb))
		}
	}
	if !strings.Contains(buttons(kb), "● high") || !balancedHTML(text) {
		t.Fatalf("current level not marked: %s", buttons(kb))
	}

	// /effort rejects levels the model does not list and accepts listed ones.
	handleAdminCommand(uid, uid, "/effort", "xhigh")
	if configSnapshot().ReasoningEffort != "high" {
		t.Fatal("unsupported level saved")
	}
	if _, ok := rec.find("sendMessage", "不支持这个档位"); !ok {
		t.Fatal("unsupported level not explained")
	}
	handleAdminCommand(uid, uid, "/effort", "minimal")
	if configSnapshot().ReasoningEffort != "minimal" {
		t.Fatal("supported level not saved")
	}
	// Switching to a model without effort keeps the setting but stops sending it.
	set("deepseek-chat", "minimal")
	if effectiveEffort(configSnapshot()) != "" || !strings.Contains(menuText(uid), "默认（不发送）") {
		t.Fatalf("menu shows an effort deepseek does not receive:\n%s", menuText(uid))
	}
}
