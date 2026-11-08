package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	decisionReject     = "reject"
	decisionApprove    = "approve"
	decisionApproveAll = "approveall"
)

type pendingApproval struct {
	ChatID, UserID int64
	Decision       chan string
}

var approvalState = struct {
	sync.Mutex
	pending map[string]*pendingApproval
}{pending: make(map[string]*pendingApproval)}

func approvalMode(c Config) string {
	switch c.ApprovalMode {
	case "auto", "read-only":
		return c.ApprovalMode
	default:
		return "manual"
	}
}

func isApprovalCallback(data string) bool {
	return strings.HasPrefix(data, decisionApprove+":") || strings.HasPrefix(data, decisionApproveAll+":") || strings.HasPrefix(data, decisionReject+":")
}

// authorizeTool is the single server-side gate for every tool call.
func authorizeTool(ctx context.Context, name, argsJSON string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, uid, _ := executionIdentity(ctx)
	if uid == 0 || !allowed(uid) {
		return fmt.Errorf("当前用户无工具执行权限")
	}
	if t, ok := findTool(name); ok && t.AdminOnly && !isAdmin(uid) {
		return fmt.Errorf("只有管理员可以使用 %s", t.Name)
	}
	c := executionConfig(ctx)
	switch classifyTool(name, argsJSON) {
	case accessRead:
		return nil
	case accessSelf:
		if approvalMode(c) == "read-only" {
			return fmt.Errorf("只读模式禁止修改 %s", name)
		}
		return nil
	}
	if !isAdmin(uid) && c.FriendTools != "all" {
		return fmt.Errorf("朋友角色只允许只读工具")
	}
	if approvalMode(c) == "read-only" {
		return fmt.Errorf("只读模式禁止工具 %s", name)
	}
	if approvalMode(c) == "auto" {
		return nil
	}
	if b := budgetOf(ctx); b != nil && atomic.LoadInt32(&b.approveAll) == 1 {
		return checkExecutionAccess(ctx)
	}
	if err := requestToolApproval(ctx, name, argsJSON); err != nil {
		return err
	}
	if err := checkExecutionAccess(ctx); err != nil {
		return err
	}
	if !isAdmin(uid) && c.FriendTools != "all" {
		return fmt.Errorf("用户角色已变更，只允许只读工具")
	}
	return nil
}

// approvalCard renders the full arguments so the user sees exactly what runs.
func approvalCard(lang, name, argsJSON string, c Config, digest []byte, visibleArgs string) (string, string) {
	label := toolLabel(lang, name)
	intent := toolIntent(argsJSON)
	pretty := visibleArgs
	var buf bytes.Buffer
	// Indentation must never push the card over Telegram's limit: the user
	// has to see every argument, so fall back to the compact form.
	if json.Indent(&buf, []byte(visibleArgs), "", "  ") == nil && utf16Len(buf.String()) <= 3000 {
		pretty = buf.String()
	}
	var h, p strings.Builder
	fmt.Fprintf(&h, "<b>%s</b>\n\n<b>%s</b> · <code>%s</code>\n", esc(tr(lang, "approval.title")), esc(label), esc(name))
	fmt.Fprintf(&p, "%s\n\n%s · %s\n", tr(lang, "approval.title"), label, name)
	if intent != "" {
		fmt.Fprintf(&h, "↳ <i>%s</i>\n", esc(intent))
		fmt.Fprintf(&p, "↳ %s\n", intent)
	}
	fmt.Fprintf(&h, "<code>%s</code> (%s)\n\n<pre><code class=\"language-json\">%s</code></pre>\n", esc(c.Workspace), esc(c.Backend), esc(pretty))
	fmt.Fprintf(&p, "%s (%s)\n\n%s\n", c.Workspace, c.Backend, pretty)
	note := tr(lang, "approval.note", fmt.Sprintf("%x", digest[:8]))
	fmt.Fprintf(&h, "<i>%s</i>", esc(note))
	p.WriteString(note)
	return h.String(), p.String()
}

func requestToolApproval(ctx context.Context, name, argsJSON string) error {
	if len(argsJSON) > 2200 {
		return fmt.Errorf("人工确认参数超过 2200 字节，无法在审批卡片完整展示；请拆分为较小的精确编辑或命令")
	}
	chatID, uid, _ := executionIdentity(ctx)
	if chatID == 0 {
		return fmt.Errorf("人工确认需要有效聊天会话")
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(nonce[:])
	p := &pendingApproval{ChatID: chatID, UserID: uid, Decision: make(chan string, 1)}
	approvalState.Lock()
	approvalState.pending[id] = p
	approvalState.Unlock()
	defer func() { approvalState.Lock(); delete(approvalState.pending, id); approvalState.Unlock() }()

	c := executionConfig(ctx)
	lang := ctxLang(ctx)
	digest := sha256.Sum256([]byte(name + "\n" + c.Workspace + "\n" + c.Backend + "\n" + argsJSON))
	htmlText, plain := approvalCard(lang, name, argsJSON, c, digest[:], redactExecutionSecrets(ctx, argsJSON))
	markup := keyboard([][]Btn{
		{btn(tr(lang, "approval.once"), decisionApprove+":"+id), btn(tr(lang, "approval.reject"), decisionReject+":"+id)},
		{btn(tr(lang, "approval.all"), decisionApproveAll+":"+id)},
	})
	msgID := sendHTML(ctx, chatID, htmlText, plain, markup)
	if msgID == 0 {
		return fmt.Errorf("无法发送审批消息，本次执行已取消")
	}
	timer := time.NewTimer(time.Duration(maxPositive(c.ApprovalTimeoutSeconds, 120)) * time.Second)
	defer timer.Stop()
	var err error
	var status string
	select {
	case decision := <-p.Decision:
		switch decision {
		case decisionApprove:
			status = tr(lang, "approval.approved")
		case decisionApproveAll:
			status = tr(lang, "approval.approved_all")
			if b := budgetOf(ctx); b != nil {
				atomic.StoreInt32(&b.approveAll, 1)
			}
		default:
			status = tr(lang, "approval.rejected")
			err = fmt.Errorf("用户拒绝执行 %s", name)
		}
	case <-ctx.Done():
		status = tr(lang, "approval.stopped")
		err = ctx.Err()
	case <-timer.C:
		status = tr(lang, "approval.timeout")
		err = fmt.Errorf("审批等待超时")
	}
	statusCtx, statusCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer statusCancel()
	summary := fmt.Sprintf("%s · %s", status, toolLabel(lang, name))
	if detail := summarizeArgs(name, argsJSON); detail != "" {
		summary += " · " + oneLine(redactExecutionSecrets(ctx, detail), 120)
	}
	editMessageContext(statusCtx, chatID, msgID, summary, nil)
	return err
}

func handleApprovalCallback(cb CallbackQuery) bool {
	if !isApprovalCallback(cb.Data) {
		return false
	}
	lang := langFor(cb.From.ID)
	if cb.Message == nil {
		answerCallback(cb.ID)
		return true
	}
	parts := strings.SplitN(cb.Data, ":", 2)
	toast := tr(lang, "approval.expired")
	approvalState.Lock()
	p, ok := approvalState.pending[parts[1]]
	if ok && p.ChatID == cb.Message.Chat.ID && p.UserID == cb.From.ID && allowed(cb.From.ID) {
		delete(approvalState.pending, parts[1])
		select {
		case p.Decision <- parts[0]:
			toast = map[string]string{decisionApprove: tr(lang, "approval.approved"), decisionApproveAll: tr(lang, "approval.approved_all"), decisionReject: tr(lang, "approval.rejected")}[parts[0]]
		default:
		}
	}
	approvalState.Unlock()
	answerCallbackText(cb.ID, toast)
	return true
}

// ---------------- execution mode settings ----------------

func modeLabel(lang, mode string) string { return tr(lang, "mode."+mode) }

func executionSettingsText(lang string) string {
	c := configSnapshot()
	return tr(lang, "mode.panel", esc(modeLabel(lang, approvalMode(c))), c.CommandTimeoutSeconds, c.TaskTimeoutSeconds)
}

func executionSettingsMenu(lang string) interface{} {
	cur := approvalMode(configSnapshot())
	label := func(mode string) string {
		if mode == cur {
			return "● " + modeLabel(lang, mode)
		}
		return modeLabel(lang, mode)
	}
	return keyboard([][]Btn{
		{btn(label("manual"), "mode:manual"), btn(label("auto"), "mode:auto")},
		{btn(label("read-only"), "mode:read-only")},
		{btn(tr(lang, "btn.back"), "nav:settings")},
	})
}

func validMode(mode string) bool { return mode == "manual" || mode == "auto" || mode == "read-only" }

func handleExecutionCommand(chatID, uid int64, rest string) bool {
	lang := langFor(uid)
	if !isAdmin(uid) {
		sendMessage(chatID, tr(lang, "err.admin_only"), nil)
		return true
	}
	if mode := strings.TrimSpace(rest); mode != "" {
		if !validMode(mode) {
			sendMessage(chatID, tr(lang, "mode.usage"), nil)
			return true
		}
		if err := updateConfig(func(c *Config) { c.ApprovalMode = mode }); err != nil {
			sendMessage(chatID, tr(lang, "err.save", err.Error()), nil)
			return true
		}
	}
	sendUI(chatID, executionSettingsText(lang), executionSettingsMenu(lang))
	return true
}

func handleExecutionCallback(cb CallbackQuery) bool {
	if cb.Data != "nav:execution" && !strings.HasPrefix(cb.Data, "mode:") {
		return false
	}
	lang := langFor(cb.From.ID)
	if cb.Message == nil || !isAdmin(cb.From.ID) || !allowed(cb.From.ID) {
		answerCallback(cb.ID)
		return true
	}
	if mode := strings.TrimPrefix(cb.Data, "mode:"); mode != cb.Data {
		if !validMode(mode) {
			answerCallback(cb.ID)
			return true
		}
		if err := updateConfig(func(c *Config) { c.ApprovalMode = mode }); err != nil {
			answerCallbackText(cb.ID, tr(lang, "err.save", err.Error()))
			return true
		}
		answerCallbackText(cb.ID, tr(lang, "toast.saved"))
	} else {
		answerCallback(cb.ID)
	}
	editUI(cb.Message.Chat.ID, cb.Message.MessageID, executionSettingsText(lang), executionSettingsMenu(lang))
	return true
}
