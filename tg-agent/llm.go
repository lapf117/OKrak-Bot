package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Part struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type LLMMessage struct {
	Role             string      `json:"role"`
	ReasoningContent string      `json:"reasoning_content,omitempty"`
	Content          interface{} `json:"content,omitempty"`
	ToolCalls        []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID       string      `json:"tool_call_id,omitempty"`
	Name             string      `json:"name,omitempty"`
}

func textMsg(role, text string) LLMMessage { return LLMMessage{Role: role, Content: text} }

func textOf(m LLMMessage) string {
	switch v := m.Content.(type) {
	case string:
		return v
	case []Part:
		var b strings.Builder
		for _, p := range v {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	case []interface{}:
		var b strings.Builder
		for _, p := range v {
			if obj, ok := p.(map[string]interface{}); ok && obj["type"] == "text" {
				if t, ok := obj["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

type ToolSchema struct {
	Type     string `json:"type"`
	Function struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		Parameters  interface{} `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model           string       `json:"model"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
	Messages        []LLMMessage `json:"messages"`
	Tools           []ToolSchema `json:"tools,omitempty"`
	Temperature     *float64     `json:"temperature,omitempty"`
	Stream          bool         `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message      LLMMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

var llmClient = &http.Client{Timeout: 300 * time.Second}

// Model API failures are retried silently in the background: the user only
// sees the task keep running. llmRetryBase is the first back-off step; it
// doubles per attempt up to llmRetryCap. Tests shorten both.
var (
	llmRetryBase = time.Second
	llmRetryCap  = 30 * time.Second
)

const defaultLLMRetries = 6

// llmRetries is the number of extra attempts after the first request.
func llmRetries(c Config) int {
	if c.LLMRetries < 0 {
		return 0
	}
	if c.LLMRetries == 0 {
		return defaultLLMRetries
	}
	return c.LLMRetries
}

// retryDelay is the wait before retry number n (1-based): exponential
// back-off with a little jitter, or the server's Retry-After when given.
func retryDelay(n int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, 60*time.Second)
	}
	d := llmRetryBase
	for i := 1; i < n && d < llmRetryCap; i++ {
		d *= 2
	}
	d = min(d, llmRetryCap)
	if jitter := int64(d / 4); jitter > 0 {
		d += time.Duration(rand.Int64N(jitter))
	}
	return d
}

// llmAttemptError describes one failed request and whether retrying can help.
type llmAttemptError struct {
	err        error
	retryable  bool
	retryAfter time.Duration
	status     int    // HTTP status of a non-2xx answer
	body       string // its (redacted, shortened) body
}

// permanentModelError reports provider errors that a retry cannot fix.
func permanentModelError(text string) bool {
	low := strings.ToLower(text)
	for _, marker := range []string{"invalid", "auth", "api key", "api_key", "permission", "forbidden", "not found", "not_found", "unsupported", "context length", "context_length", "maximum context", "too long", "content_filter", "content filter", "insufficient", "balance", "quota"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusConflict || code == http.StatusTooEarly || code == http.StatusTooManyRequests || code >= 500
}

func chat(messages []LLMMessage, schemas []ToolSchema) (*chatResponse, error) {
	return chatContext(WithExecutionConfig(context.Background(), configSnapshot()), messages, schemas)
}

// chatContext calls an OpenAI-compatible Chat Completions endpoint with the
// task's frozen provider settings. Transport failures, 408/409/425/429/5xx,
// gateway garbage and transient provider errors are retried silently with
// exponential back-off; definitive errors (bad request, auth) fail at once.
func chatContext(ctx context.Context, messages []LLMMessage, schemas []ToolSchema) (*chatResponse, error) {
	c := executionConfig(ctx)
	p := providerFor(c)
	if p == nil {
		return nil, fmt.Errorf("没有配置模型服务")
	}
	model := c.Model
	if model == "" {
		model = p.DefaultModel
	}
	if model == "" && len(p.Models) > 0 {
		model = p.Models[0]
	}
	if model == "" {
		return nil, fmt.Errorf("未配置模型名称")
	}
	// Only send a reasoning_effort the provider/model accepts (see effort.go).
	effort := effectiveEffort(c)
	encode := func(effort string) ([]byte, error) {
		bodyReq := chatRequest{Model: model, ReasoningEffort: effort, Messages: messages, Tools: schemas}
		// Most reasoning providers reject a custom temperature. An empty effort keeps the compatible temperature option.
		if effort == "" {
			bodyReq.Temperature = &c.Temperature
		}
		return json.Marshal(bodyReq)
	}
	body, err := encode(effort)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	client := llmClient
	if c.Proxy != "" {
		proxyURL, err := url.Parse(c.Proxy)
		if err != nil {
			return nil, fmt.Errorf("无效模型代理")
		}
		clone := *llmClient
		if transport, ok := http.DefaultTransport.(*http.Transport); ok {
			t := transport.Clone()
			t.Proxy = http.ProxyURL(proxyURL)
			clone.Transport = t
			defer t.CloseIdleConnections()
		}
		client = &clone
	}
	retries := llmRetries(c)
	var last *llmAttemptError
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			delay := retryDelay(attempt, last.retryAfter)
			logf("模型请求失败，%s 后静默重试（%d/%d）: %s", delay.Round(100*time.Millisecond), attempt, retries, short(last.err.Error(), 200))
			recordRetry(ctx)
			if err := waitContext(ctx, delay); err != nil {
				return nil, err
			}
		}
		if err := checkExecutionAccess(ctx); err != nil {
			return nil, err
		}
		cr, failure := chatAttempt(ctx, client, endpoint, p.APIKey, body)
		if failure == nil {
			return cr, nil
		}
		if err := checkExecutionAccess(ctx); err != nil {
			return nil, err
		}
		if effort != "" && rejectsEffort(failure.status, failure.body) {
			// The API does not take reasoning_effort: drop it and resend at once.
			logf("服务商拒绝 reasoning_effort=%s（%s），已停用该参数并重发", effort, c.Model)
			effortRejected.Store(effortKey(c), true)
			effort = ""
			if body, err = encode(""); err != nil {
				return nil, err
			}
			attempt--
			continue
		}
		if !failure.retryable {
			return nil, failure.err
		}
		last = failure
	}
	return nil, fmt.Errorf("模型请求已自动重试 %d 次仍失败: %v", retries, last.err)
}

// chatAttempt performs one request and classifies any failure.
func chatAttempt(ctx context.Context, client *http.Client, endpoint, apiKey string, body []byte) (*chatResponse, *llmAttemptError) {
	retry := func(err error) *llmAttemptError { return &llmAttemptError{err: err, retryable: true} }
	fail := func(err error) *llmAttemptError { return &llmAttemptError{err: err} }
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fail(fmt.Errorf("无效模型地址"))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fail(ctx.Err())
		}
		return nil, retry(fmt.Errorf("模型连接失败: %s", redactExecutionSecrets(ctx, err.Error())))
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	resp.Body.Close()
	if ctx.Err() != nil {
		return nil, fail(ctx.Err())
	}
	if readErr != nil {
		return nil, retry(fmt.Errorf("读取模型响应失败: %s", redactExecutionSecrets(ctx, readErr.Error())))
	}
	if len(raw) > 8<<20 {
		return nil, fail(fmt.Errorf("模型响应超过 8 MiB"))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text := short(redactExecutionSecrets(ctx, string(raw)), 500)
		failure := &llmAttemptError{err: fmt.Errorf("模型 HTTP %d: %s", resp.StatusCode, text), status: resp.StatusCode, body: text}
		if retryableStatus(resp.StatusCode) {
			failure.retryable = true
			if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
				failure.retryAfter = time.Duration(seconds) * time.Second
			}
		}
		return nil, failure
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		// Gateways sometimes answer 200 with an HTML error page.
		return nil, retry(fmt.Errorf("模型返回无效 JSON: %v", err))
	}
	if cr.Error != nil {
		failure := fmt.Errorf("模型报错: %s", short(redactExecutionSecrets(ctx, cr.Error.Message), 500))
		if permanentModelError(cr.Error.Type + " " + cr.Error.Message) {
			return nil, fail(failure)
		}
		return nil, retry(failure)
	}
	if len(cr.Choices) == 0 {
		return nil, retry(fmt.Errorf("模型没有返回 choices"))
	}
	recordUsage(ctx, cr.Usage.PromptTokens, cr.Usage.CompletionTokens)
	if cr.Choices[0].FinishReason == "length" && len(cr.Choices[0].Message.ToolCalls) > 0 {
		return nil, fail(fmt.Errorf("模型输出被截断，工具参数不完整；请提高服务端输出限额"))
	}
	if cr.Choices[0].FinishReason == "length" {
		cr.Choices[0].Message.Content = textOf(cr.Choices[0].Message) + "\n\n" + tr(ctxLang(ctx), "agent.truncated")
	}
	return &cr, nil
}
