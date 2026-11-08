package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// StepEvent reports agent progress to the Telegram panel.
//   - Thought: one per model response; Note is the narration that accompanies
//     a batch of tool calls, Reasoning the provider's reasoning_content.
//   - Tool start: Tool/Detail/Intent set, Done=false.
//   - Tool end: Done=true with Output (Failed when the tool returned an error).
//   - Final: the agent finished.
type StepEvent struct {
	Step                    int
	Tool, Detail, Output    string
	Intent, Note, Reasoning string
	Plan                    []planItem
	Done, Final, Failed     bool
	Thought                 bool
}

type stepFn func(ev StepEvent)

func runAgent(chatID, uid int64, userText string, imagePaths []string, depth int, onStep stepFn) (string, error) {
	c := configSnapshot()
	ctx, cancel := context.WithTimeout(WithExecutionConfig(telegramContext, c), time.Duration(maxPositive(c.TaskTimeoutSeconds, 1800))*time.Second)
	defer cancel()
	return runAgentContext(ctx, chatID, uid, userText, imagePaths, depth, onStep)
}

func runAgentContext(ctx context.Context, chatID, uid int64, userText string, imagePaths []string, depth int, onStep stepFn) (answer string, taskErr error) {
	if depth > 3 {
		return "", fmt.Errorf("子智能体层级过深")
	}
	if !allowed(uid) || chatID <= 0 || chatID != uid {
		return "", fmt.Errorf("只允许已授权用户在私聊会话执行任务")
	}
	if strings.TrimSpace(userText) == "" && len(imagePaths) == 0 {
		return "", fmt.Errorf("任务不能为空")
	}
	c := executionConfig(ctx)
	lang := ctxLang(ctx)
	ctx = WithExecutionConfig(ctx, c)
	ctx = context.WithValue(ctx, identityContextKey, executionIdentityValue{ChatID: chatID, UserID: uid, Depth: depth})
	ctx = withTaskBudget(ctx, c)
	emit := func(ev StepEvent) {
		if onStep != nil {
			onStep(ev)
		}
	}

	var history []LLMMessage
	if depth == 0 {
		history = maybeCompressContext(ctx, getHistory(chatID))
	}
	var content interface{} = userText
	if len(imagePaths) > 0 {
		parts := []Part{{Type: "text", Text: userText}}
		for _, p := range imagePaths {
			part, err := imagePart(p)
			if err != nil {
				return "", err
			}
			parts = append(parts, part)
		}
		content = parts
	}
	msgs := []LLMMessage{textMsg("system", buildSystemPromptContext(ctx, uid))}
	if skillText := autoSkillContext(ctx, userText); skillText != "" {
		msgs[0].Content = textOf(msgs[0]) + "\n" + tr(lang, "prompt.auto_skills") + "\n" + short(skillText, 18000)
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, LLMMessage{Role: "user", Content: content})
	defer func() {
		if depth != 0 {
			return
		}
		saved := completeToolChain(msgs[1:])
		if taskErr != nil {
			saved = append(saved, textMsg("assistant", tr(lang, "agent.interrupted")+short(redactExecutionSecrets(ctx, taskErr.Error()), 400)))
		} else {
			// Backups only serve this task's undo; remove them so they do not
			// clutter the workspace or get committed by "git add".
			cleanupTaskBackups(ctx)
		}
		setHistoryLimit(chatID, saved, c.MaxHistory)
	}()

	schemas := toolSchemasContext(ctx, uid)
	maxSteps := maxPositive(c.MaxSteps, 25)
	var finalText string
	budgetReached := false
	for step := 0; step < maxSteps; step++ {
		if err := checkExecutionAccess(ctx); err != nil {
			return "", err
		}
		if err := consumeStep(ctx); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			budgetReached = true
			break
		}
		resp, err := chatContext(ctx, msgs, schemas)
		if err != nil {
			return "", err
		}
		msg := resp.Choices[0].Message
		msg.Role = "assistant"
		if text, ok := msg.Content.(string); ok {
			msg.Content = redactExecutionSecrets(ctx, text)
		}
		msg.ReasoningContent = redactExecutionSecrets(ctx, msg.ReasoningContent)
		if len(msg.ToolCalls) > 16 {
			msg.ToolCalls = msg.ToolCalls[:16]
		}
		text := strings.TrimSpace(textOf(msg))
		if text != "" {
			setLastThink(chatID, short(redactExecutionSecrets(ctx, text), 4000))
		}
		ids := map[string]bool{}
		for i := range msg.ToolCalls {
			tc := &msg.ToolCalls[i]
			if tc.ID == "" || ids[tc.ID] {
				tc.ID = fmt.Sprintf("call_%d_%d", step, i)
			}
			ids[tc.ID] = true
		}
		msgs = append(msgs, msg)
		if len(msg.ToolCalls) == 0 {
			if msg.ReasoningContent != "" {
				emit(StepEvent{Step: step + 1, Thought: true, Reasoning: tailRunes(msg.ReasoningContent, 4000)})
			}
			finalText = text
			break
		}
		emit(StepEvent{Step: step + 1, Thought: true, Note: oneLine(text, 300), Reasoning: tailRunes(msg.ReasoningContent, 4000)})
		for _, tc := range msg.ToolCalls {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			name := canonicalToolName(tc.Function.Name)
			detail := redactExecutionSecrets(ctx, summarizeArgs(name, tc.Function.Arguments))
			intent := redactExecutionSecrets(ctx, toolIntent(tc.Function.Arguments))
			emit(StepEvent{Step: step + 1, Tool: name, Detail: detail, Intent: intent})
			if b := budgetOf(ctx); b != nil {
				atomic.AddInt64(&b.ToolCalls, 1)
			}
			if name == "bash" {
				recordCommand(ctx)
			}
			result, toolErr := runToolContext(ctx, tc.Function.Name, tc.Function.Arguments)
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if toolErr != nil {
				if result != "" {
					result += "\n"
				}
				result += "执行出错: " + toolErr.Error()
			}
			if result == "" {
				result = "（无输出）"
			}
			result = short(redactExecutionSecrets(ctx, result), maxPositive(c.MaxToolOutput, 12000))
			ev := StepEvent{Step: step + 1, Tool: name, Detail: detail, Intent: intent, Output: result, Done: true, Failed: toolErr != nil}
			if name == "plan" {
				ev.Plan = planSnapshot(ctx)
			}
			emit(ev)
			msgs = append(msgs, LLMMessage{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
		if step == maxSteps-1 {
			budgetReached = true
		}
	}
	if budgetReached {
		// One tool-free final summary is reserved so hitting a limit is never reported as completion.
		summaryMsgs := append(append([]LLMMessage(nil), msgs...), textMsg("user", tr(lang, "agent.budget_prompt")))
		if depth == 0 && ctx.Err() == nil {
			if r, err := chatContext(ctx, summaryMsgs, nil); err == nil {
				finalText = strings.TrimSpace(textOf(r.Choices[0].Message))
			}
		}
		if finalText == "" {
			finalText = tr(lang, "agent.budget_fallback")
		}
		finalText += "\n\n" + tr(lang, "agent.budget_note")
		msgs = append(msgs, textMsg("assistant", finalText))
	}
	if err := checkExecutionAccess(ctx); err != nil {
		return "", err
	}
	if finalText == "" {
		finalText = tr(lang, "agent.empty")
	}
	emit(StepEvent{Done: true, Final: true})
	return redactExecutionSecrets(ctx, finalText), nil
}

func maybeCompress(history []LLMMessage) []LLMMessage {
	return maybeCompressContext(WithExecutionConfig(context.Background(), configSnapshot()), history)
}

// maybeCompressContext replaces a long history with a hand-off summary.
func maybeCompressContext(ctx context.Context, history []LLMMessage) []LLMMessage {
	total := 0
	for _, m := range history {
		b, _ := json.Marshal(m)
		total += len(b)
	}
	if total < 60000 || len(history) < 6 {
		return history
	}
	if err := consumeStep(ctx); err != nil {
		return history
	}
	var b strings.Builder
	for _, m := range history {
		t := textOf(m)
		if len(m.ToolCalls) > 0 {
			raw, _ := json.Marshal(m.ToolCalls)
			t += "\n工具调用：" + string(raw)
		}
		b.WriteString("[" + m.Role + "] " + short(t, 1600) + "\n")
	}
	lang := ctxLang(ctx)
	response, err := chatContext(ctx, []LLMMessage{textMsg("system", tr(lang, "agent.compress_prompt")), textMsg("user", short(b.String(), 50000))}, nil)
	if err != nil {
		return history
	}
	summary := strings.TrimSpace(textOf(response.Choices[0].Message))
	if summary == "" {
		return history
	}
	return []LLMMessage{textMsg("user", tr(lang, "agent.compress_header")+"\n"+summary), textMsg("assistant", tr(lang, "agent.compress_ack"))}
}

func imagePart(path string) (Part, error) {
	f, err := os.Open(path)
	if err != nil {
		return Part{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (12<<20)+1))
	if err != nil {
		return Part{}, err
	}
	if len(data) > 12<<20 {
		return Part{}, fmt.Errorf("图片超过 12 MiB")
	}
	mime := "image/jpeg"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		mime = "image/png"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	}
	p := Part{Type: "image_url"}
	p.ImageURL = &struct {
		URL string `json:"url"`
	}{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)}
	return p, nil
}

// summarizeArgs picks the most telling argument for a one-line display.
func summarizeArgs(tool, argsJSON string) string {
	var args map[string]interface{}
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return short(argsJSON, 100)
	}
	for _, key := range []string{"command", "path", "url", "query", "pattern", "name", "task", "when", "text"} {
		if value, ok := args[key].(string); ok && value != "" {
			s := oneLine(value, 100)
			if action, ok := args["action"].(string); ok && action != "" {
				s = action + " · " + s
			}
			return s
		}
	}
	if action, ok := args["action"].(string); ok {
		return action
	}
	delete(args, "intent")
	if len(args) == 0 {
		return ""
	}
	raw, _ := json.Marshal(args)
	return short(string(raw), 100)
}

// toolIntent extracts the model's one-line purpose for a tool call.
func toolIntent(argsJSON string) string {
	var args struct {
		Intent string `json:"intent"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return ""
	}
	return oneLine(args.Intent, 160)
}

// Legacy direct spawning cannot supply an owner identity implicitly.
func spawnAgent(task string) (string, error) {
	return "", fmt.Errorf("子智能体必须通过当前任务的工具上下文启动")
}

// completeToolChain repairs interrupted tool batches before they can be replayed to a provider.
func completeToolChain(messages []LLMMessage) []LLMMessage {
	out := make([]LLMMessage, 0, len(messages))
	pending := []ToolCall{}
	seenIDs := map[string]bool{}
	flush := func() {
		for _, tc := range pending {
			if !seenIDs[tc.ID] {
				out = append(out, LLMMessage{Role: "tool", ToolCallID: tc.ID, Content: "该调用因任务中断未完成，不应推断成功。"})
			}
		}
		pending = nil
		seenIDs = map[string]bool{}
	}
	for _, m := range messages {
		if m.Role != "tool" {
			flush()
		} else {
			valid := false
			for _, tc := range pending {
				if tc.ID == m.ToolCallID {
					valid = true
					break
				}
			}
			if !valid || seenIDs[m.ToolCallID] {
				continue
			}
			seenIDs[m.ToolCallID] = true
		}
		out = append(out, m)
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			pending = m.ToolCalls
		}
	}
	flush()
	return out
}
