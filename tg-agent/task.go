package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A reply longer than this is summarised in chat and attached as a .md file.
const longReplyRunes = 12000

type taskOptions struct {
	Origin string // shown in the panel, e.g. "#3" for scheduled runs
	Quiet  bool   // do not message the user when the chat is busy
}

type lastTask struct {
	Text   string
	Images []string
}

var lastTasks sync.Map // chatID -> lastTask
var lastUsage sync.Map // chatID -> int64 tokens of the most recent task

func lastUsageTokens(chatID int64) int64 {
	if v, ok := lastUsage.Load(chatID); ok {
		return v.(int64)
	}
	return 0
}

func runUserTask(chatID, uid int64, text string, images []string) {
	runUserTaskOpts(chatID, uid, text, images, taskOptions{})
}

// runUserTaskOpts runs one agent task with a live progress panel. It returns
// false when the task could not start (busy chat, shutdown or no permission).
func runUserTaskOpts(chatID, uid int64, text string, images []string, opts taskOptions) bool {
	if chatID <= 0 || chatID != uid || !allowed(uid) {
		return false
	}
	lang := langFor(uid)
	ctx, cleanup, ok := beginTaskWithOrigin(chatID, opts.Origin)
	if !ok {
		if !opts.Quiet && telegramContext.Err() == nil {
			sendMessage(chatID, tr(lang, "task.busy"), nil)
		}
		return false
	}
	defer cleanup()
	c := executionConfig(ctx)
	ctx = withTaskBudget(withLang(ctx, lang), c)
	lastTasks.Store(chatID, lastTask{Text: text, Images: images})

	show := displayFor(uid, c)
	panel := newTaskPanel(lang, show, opts.Origin)
	panel.stats = func() taskStats { return statsOf(ctx) }
	// The live panel carries no buttons: to stop, the user sends /stop.
	msgID := sendHTML(ctx, chatID, panel.render(), "", nil)

	var renderMu sync.Mutex
	var lastEdit time.Time
	var lastBody string
	refresh := func(force bool) {
		// Never let a slow or retrying Telegram edit hold up the agent: when
		// another edit is in flight, skip; the next event or tick catches up.
		if !renderMu.TryLock() {
			return
		}
		defer renderMu.Unlock()
		// Telegram allows roughly one edit per second per chat; tool results
		// (force) may refresh a little sooner than background ticks.
		minGap := 1200 * time.Millisecond
		if force {
			minGap = 800 * time.Millisecond
		}
		if msgID == 0 || ctx.Err() != nil || time.Since(lastEdit) < minGap {
			return
		}
		body := panel.render()
		if body == lastBody {
			return
		}
		if editHTML(ctx, chatID, msgID, body, "", nil) {
			lastBody = body
		}
		lastEdit = time.Now()
	}
	tickerDone := make(chan struct{})
	go func() {
		t := time.NewTicker(4 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-tickerDone:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if c.Typing {
					sendTyping(ctx, chatID)
				}
				refresh(false) // keeps the elapsed time moving during long commands
			}
		}
	}()
	onStep := func(ev StepEvent) {
		panel.apply(ev)
		if !ev.Final {
			refresh(ev.Done || ev.Note != "" || ev.Reasoning != "")
		}
	}

	start := time.Now()
	out, err := runAgentContext(ctx, chatID, uid, text, images, 0, onStep)
	dur := time.Since(start)
	close(tickerDone)
	renderMu.Lock() // wait for an in-flight edit before the final one
	renderMu.Unlock()

	stats := statsOf(ctx)
	if tokens := stats.Tokens(); tokens > 0 {
		lastUsage.Store(chatID, tokens)
	}
	recordTaskUsage(stats.Tools, stats.Prompt, stats.Completion)
	trace := panel.snapshot()
	finalCtx, cancelFinal := context.WithTimeout(telegramContext, 30*time.Second)
	defer cancelFinal()

	// Final answers carry no buttons: /new, /continue and /menu stay available as commands.
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		reason := tr(lang, "task.stopped")
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = tr(lang, "task.timeout")
		}
		writeRunLog(chatID, redactExecutionSecrets(ctx, text), trace, reason, dur, stats)
		if !editHTML(finalCtx, chatID, msgID, panel.renderFinal("■", reason), "", nil) {
			sendUI(chatID, "<b>■ "+esc(reason)+"</b>", nil)
		}
		return true
	}
	if err != nil {
		msg := redactExecutionSecrets(ctx, err.Error())
		writeRunLog(chatID, redactExecutionSecrets(ctx, text), trace, "执行失败："+msg, dur, stats)
		editHTML(finalCtx, chatID, msgID, panel.renderFinal("✗", tr(lang, "task.failed")), "", nil)
		sendHTML(finalCtx, chatID, failureHTML(lang, msg), "", nil)
		return true
	}

	out = stripEmojiMarkdown(strings.TrimSpace(redactExecutionSecrets(ctx, out)))
	if strings.TrimSpace(out) == "" {
		out = tr(lang, "task.no_output")
	}
	writeRunLog(chatID, redactExecutionSecrets(ctx, text), trace, out, dur, stats)
	finalPanel := func() { editHTML(finalCtx, chatID, msgID, panel.renderFinal("✓", tr(lang, "task.done")), "", nil) }
	if show["keep"] {
		finalPanel() // collapse first so the panel never looks stuck while the answer is prepared
	}

	footer := ""
	if hasFooter(show) {
		f := footerData{Elapsed: dur, Stats: stats, Model: c.Model}
		if show["balance"] {
			f.Balance = footerBalance(finalCtx, c)
		}
		footer = renderFooter(lang, show, f)
	}
	answer := out
	var attachment []byte
	if len([]rune(out)) > longReplyRunes {
		attachment = []byte(out)
		answer = cutAtParagraph(out, 3000) + "\n\n" + tr(lang, "task.see_attachment")
	}
	answerID := sendAnswer(finalCtx, chatID, answer, footer)
	switch {
	case show["keep"]:
	case answerID != 0:
		deleteMessage(finalCtx, chatID, msgID) // "keep" off: only the answer stays
	default:
		finalPanel() // the answer did not arrive; keep the panel as the record
	}
	if attachment != nil {
		sendMedia(chatID, "sendDocument", "document", attachment, "reply-"+time.Now().Format("20060102-150405")+".md", tr(lang, "task.attachment_caption"))
	}
	if isAdmin(uid) && c.Backend != "ssh" {
		go sendMediaInText(chatID, out, sentFiles(ctx))
	}
	return true
}

// hasFooter reports whether any statistics switch is on.
func hasFooter(d display) bool {
	for _, it := range displayItems {
		if it.Group == displayGroupFooter && d[it.Key] {
			return true
		}
	}
	return false
}

// failureHTML is the message sent when a task ends with an error: a short
// title, the error collapsed, and a hint to run the task again.
func failureHTML(lang, msg string) string {
	return "<b>✗ " + esc(tr(lang, "task.failed")) + "</b>\n<blockquote expandable>" + esc(short(msg, 1500)) + "</blockquote>\n<i>" + esc(tr(lang, "task.failed_hint")) + "</i>"
}

// cutAtParagraph returns roughly the first n runes, ending at a paragraph or line.
func cutAtParagraph(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	head := string(r[:n])
	if i := strings.LastIndex(head, "\n\n"); i > len(head)/2 {
		head = head[:i]
	} else if i := strings.LastIndex(head, "\n"); i > len(head)/2 {
		head = head[:i]
	}
	// Close an unterminated code fence so the preview renders cleanly.
	if strings.Count(head, "```")%2 == 1 {
		head += "\n```"
	}
	return strings.TrimRight(head, "\n") + "\n…"
}

// writeRunLog stores the full run as Markdown under runs/.
func writeRunLog(chatID int64, task string, trace []progressAction, final string, dur time.Duration, stats taskStats) string {
	dir := filepath.Join(baseDir, "runs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ""
	}
	now := time.Now()
	p := filepath.Join(dir, fmt.Sprintf("%s-%d.md", now.Format("20060102-150405.000000000"), chatID))
	var b strings.Builder
	b.WriteString("# 执行记录 / Run log\n\n")
	fmt.Fprintf(&b, "- 时间：%s\n- 耗时：%s\n- 模型调用：%d\n- 工具调用：%d\n- 命令：%d\n- 静默重试：%d\n- Tokens：%d（输入 %d / 输出 %d）\n\n", now.Format("2006-01-02 15:04:05"), humanDuration(dur), stats.Calls, stats.Tools, stats.Commands, stats.Retries, stats.Tokens(), stats.Prompt, stats.Completion)
	b.WriteString("## 任务\n\n" + task + "\n\n")
	for i, t := range trace {
		fmt.Fprintf(&b, "## %d. %s · `%s`", i+1, toolLabel(langZH, t.Tool), t.Tool)
		if t.Done {
			fmt.Fprintf(&b, " · %s", stepDuration(t.Took))
		}
		b.WriteString("\n\n")
		if t.Intent != "" {
			b.WriteString("> " + t.Intent + "\n\n")
		}
		if t.Detail != "" {
			b.WriteString(t.Detail + "\n\n")
		}
		if strings.TrimSpace(t.Output) != "" {
			b.WriteString("```\n" + strings.ReplaceAll(t.Output, "```", "ˋˋˋ") + "\n```\n\n")
		}
	}
	b.WriteString("## 最终输出\n\n" + final + "\n")
	if err := os.WriteFile(p, []byte(redactSecrets(b.String())), 0600); err != nil {
		return ""
	}
	return p
}
