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

	panel := newProgressPanel(lang, stepsDetailed(uid, c), opts.Origin)
	// The live panel carries no buttons: to stop, the user sends /stop.
	msgID := sendHTML(ctx, chatID, panel.render(), "", nil)

	var renderMu sync.Mutex
	var lastEdit time.Time
	var lastBody string
	refresh := func(force bool) {
		renderMu.Lock()
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
			refresh(ev.Done || ev.Note != "")
		}
	}

	start := time.Now()
	out, err := runAgentContext(ctx, chatID, uid, text, images, 0, onStep)
	dur := time.Since(start)
	close(tickerDone)
	renderMu.Lock() // wait for an in-flight edit before the final one
	renderMu.Unlock()

	tools, prompt, completion := taskUsageDetailed(ctx)
	tokens := prompt + completion
	if tokens > 0 {
		lastUsage.Store(chatID, tokens)
	}
	recordTaskUsage(tools, prompt, completion)
	trace := panel.snapshot()
	finalCtx, cancelFinal := context.WithTimeout(telegramContext, 30*time.Second)
	defer cancelFinal()
	menuKB := answerKeyboard(lang, uid, "")

	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		reason := tr(lang, "task.stopped")
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = tr(lang, "task.timeout")
		}
		writeRunLog(chatID, redactExecutionSecrets(ctx, text), trace, reason, dur, tools, tokens)
		if !editHTML(finalCtx, chatID, msgID, panel.renderFinal(reason), "", menuKB) {
			sendUI(chatID, esc(reason), menuKB)
		}
		return true
	}
	if err != nil {
		msg := redactExecutionSecrets(ctx, err.Error())
		writeRunLog(chatID, redactExecutionSecrets(ctx, text), trace, "执行失败："+msg, dur, tools, tokens)
		editHTML(finalCtx, chatID, msgID, panel.renderFinal(tr(lang, "task.failed")), "", nil)
		sendHTML(finalCtx, chatID, "<b>"+esc(tr(lang, "task.failed"))+"</b>\n<pre>"+esc(short(msg, 1500))+"</pre>", "", menuKB)
		return true
	}

	out = strings.TrimSpace(redactExecutionSecrets(ctx, out))
	if out == "" {
		out = tr(lang, "task.no_output")
	}
	runPath := writeRunLog(chatID, redactExecutionSecrets(ctx, text), trace, out, dur, tools, tokens)
	editHTML(finalCtx, chatID, msgID, panel.renderFinal(tr(lang, "task.done")), "", nil)

	footer := humanDuration(dur)
	if tools > 0 {
		footer += " · " + fmt.Sprint(tools) + " tools"
	}
	if tokens > 0 {
		footer += " · " + humanTokens(tokens) + " tokens"
	}
	answer := out
	var attachment []byte
	if len([]rune(out)) > longReplyRunes {
		attachment = []byte(out)
		answer = cutAtParagraph(out, 3000) + "\n\n" + tr(lang, "task.see_attachment")
	}
	sendRich(finalCtx, chatID, answer+"\n\n*"+footer+"*", answerKeyboard(lang, uid, filepath.Base(runPath)))
	if attachment != nil {
		sendMedia(chatID, "sendDocument", "document", attachment, "reply-"+time.Now().Format("20060102-150405")+".md", tr(lang, "task.attachment_caption"))
	}
	if isAdmin(uid) && c.Backend != "ssh" {
		go sendMediaInText(chatID, out, sentFiles(ctx))
	}
	return true
}

// answerKeyboard is attached to final answers: new chat, retry, menu and (for
// admins) the run log of this task.
func answerKeyboard(lang string, uid int64, runName string) interface{} {
	row := []Btn{btn(tr(lang, "btn.new"), "act:new"), btn(tr(lang, "btn.retry"), "act:retry"), btn(tr(lang, "btn.menu"), "nav:root")}
	rows := [][]Btn{row}
	if runName != "" && runName != "." && isAdmin(uid) && len("runlog:"+runName) <= 64 {
		rows = append(rows, []Btn{btn(tr(lang, "btn.runlog"), "runlog:"+runName)})
	}
	return keyboard(rows)
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
func writeRunLog(chatID int64, task string, trace []progressAction, final string, dur time.Duration, tools, tokens int64) string {
	dir := filepath.Join(baseDir, "runs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ""
	}
	now := time.Now()
	p := filepath.Join(dir, fmt.Sprintf("%s-%d.md", now.Format("20060102-150405.000000000"), chatID))
	var b strings.Builder
	b.WriteString("# 执行记录 / Run log\n\n")
	fmt.Fprintf(&b, "- 时间：%s\n- 耗时：%s\n- 工具调用：%d\n- Tokens：%d\n\n", now.Format("2006-01-02 15:04:05"), humanDuration(dur), tools, tokens)
	b.WriteString("## 任务\n\n" + task + "\n\n")
	for i, t := range trace {
		fmt.Fprintf(&b, "## %d. %s · `%s`\n\n", i+1, toolLabel(langZH, t.Tool), t.Tool)
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
