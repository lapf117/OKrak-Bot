package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
			if obj, ok := p.(map[string]interface{}); ok {
				if obj["type"] == "text" {
					if t, ok := obj["text"].(string); ok {
						b.WriteString(t)
					}
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

func chat(messages []LLMMessage, schemas []ToolSchema) (*chatResponse, error) {
	return chatContext(WithExecutionConfig(context.Background(), configSnapshot()), messages, schemas)
}
func chatContext(ctx context.Context, messages []LLMMessage, schemas []ToolSchema) (*chatResponse, error) {
	c := executionConfig(ctx)
	var p *Provider
	for i := range c.Provider {
		if c.Provider[i].Name == c.ProviderName {
			p = &c.Provider[i]
			break
		}
	}
	if p == nil && len(c.Provider) > 0 {
		p = &c.Provider[0]
	}
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
	bodyReq := chatRequest{Model: model, ReasoningEffort: c.ReasoningEffort, Messages: messages, Tools: schemas, Stream: false}
	// Most reasoning providers reject a custom temperature. An empty effort keeps the compatible temperature option.
	if c.ReasoningEffort == "" {
		bodyReq.Temperature = &c.Temperature
	}
	body, err := json.Marshal(bodyReq)
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
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := checkExecutionAccess(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("无效模型地址")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("模型连接失败: %s", redactExecutionSecrets(ctx, err.Error()))
			if attempt < 2 {
				if err = waitContext(ctx, time.Duration(attempt+1)*time.Second); err != nil {
					return nil, err
				}
			}
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
		resp.Body.Close()
		if err := checkExecutionAccess(ctx); err != nil {
			return nil, err
		}
		if readErr != nil {
			return nil, fmt.Errorf("读取模型响应失败: %s", redactExecutionSecrets(ctx, readErr.Error()))
		}
		if len(raw) > 8<<20 {
			return nil, fmt.Errorf("模型响应超过 8 MiB")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("模型 HTTP %d: %s", resp.StatusCode, short(redactExecutionSecrets(ctx, string(raw)), 500))
			if resp.StatusCode != 429 && resp.StatusCode < 500 {
				return nil, lastErr
			}
			if attempt < 2 {
				delay := time.Duration(attempt+1) * 2 * time.Second
				if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
					if seconds > 30 {
						seconds = 30
					}
					delay = time.Duration(seconds) * time.Second
				}
				if err := waitContext(ctx, delay); err != nil {
					return nil, err
				}
			}
			continue
		}
		var cr chatResponse
		if err := json.Unmarshal(raw, &cr); err != nil {
			return nil, fmt.Errorf("模型返回无效 JSON: %v", err)
		}
		if cr.Error != nil {
			return nil, fmt.Errorf("模型报错: %s", short(redactExecutionSecrets(ctx, cr.Error.Message), 500))
		}
		if len(cr.Choices) == 0 {
			return nil, fmt.Errorf("模型没有返回 choices")
		}
		if cr.Choices[0].FinishReason == "length" && len(cr.Choices[0].Message.ToolCalls) > 0 {
			return nil, fmt.Errorf("模型输出被截断，工具参数不完整；请提高服务端输出限额")
		}
		if cr.Choices[0].FinishReason == "length" {
			cr.Choices[0].Message.Content = textOf(cr.Choices[0].Message) + "\n\n模型输出达到服务端长度限制，本次回答可能不完整。"
		}
		return &cr, nil
	}
	return nil, fmt.Errorf("模型请求重试失败: %v", lastErr)
}
func waitContext(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
