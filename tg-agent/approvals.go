package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

type pendingApproval struct {
	ChatID, UserID int64
	Decision       chan bool
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
func authorizeTool(ctx context.Context, name, argsJSON string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, uid, _ := executionIdentity(ctx)
	if uid == 0 || !allowed(uid) {
		return fmt.Errorf("当前用户无工具执行权限")
	}
	c := executionConfig(ctx)
	read := toolReadOnly(name, argsJSON)
	if !isAdmin(uid) && (c.FriendTools != "all") && !read {
		return fmt.Errorf("朋友角色只允许只读工具")
	}
	if approvalMode(c) == "read-only" && !read {
		return fmt.Errorf("只读模式禁止工具 %s", name)
	}
	if read || approvalMode(c) == "auto" {
		return nil
	}
	if err := requestToolApproval(ctx, name, argsJSON); err != nil {
		return err
	}
	if err := checkExecutionAccess(ctx); err != nil {
		return err
	}
	if !isAdmin(uid) && c.FriendTools != "all" && !read {
		return fmt.Errorf("用户角色已变更，只允许只读工具")
	}
	return nil
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
	p := &pendingApproval{ChatID: chatID, UserID: uid, Decision: make(chan bool, 1)}
	approvalState.Lock()
	approvalState.pending[id] = p
	approvalState.Unlock()
	defer func() { approvalState.Lock(); delete(approvalState.pending, id); approvalState.Unlock() }()
	c := executionConfig(ctx)
	digest := sha256.Sum256([]byte(name + "\n" + c.Workspace + "\n" + c.Backend + "\n" + argsJSON))
	message := fmt.Sprintf("等待执行确认\n\n工具：%s\n工作区：%s (%s)\n参数摘要：%x\n\n%s\n\n本次批准只对以上参数生效（已配置密钥脱敏）。", name, c.Workspace, c.Backend, digest[:8], redactExecutionSecrets(ctx, argsJSON))
	msgID := sendMessageContext(ctx, chatID, message, keyboard([][]Btn{{btn("批准一次", "approve:"+id), btn("拒绝", "reject:"+id)}}))
	if msgID == 0 {
		return fmt.Errorf("无法发送审批消息，本次执行已取消")
	}
	seconds := c.ApprovalTimeoutSeconds
	if seconds <= 0 {
		seconds = 120
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	var err error
	var status string
	select {
	case yes := <-p.Decision:
		if yes {
			status = "已批准本次执行。"
		} else {
			status = "已拒绝本次执行。"
			err = fmt.Errorf("用户拒绝执行 %s", name)
		}
	case <-ctx.Done():
		status = "任务已停止，本次审批失效。"
		err = ctx.Err()
	case <-timer.C:
		status = "审批超时，本次执行已取消。"
		err = fmt.Errorf("审批等待超时")
	}
	statusCtx, statusCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer statusCancel()
	editMessageContext(statusCtx, chatID, msgID, status, nil)
	return err
}

func handleApprovalCallback(cb CallbackQuery) bool {
	if !strings.HasPrefix(cb.Data, "approve:") && !strings.HasPrefix(cb.Data, "reject:") {
		return false
	}
	callbackCtx, callbackCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer callbackCancel()
	defer tgCallContext(callbackCtx, "answerCallbackQuery", map[string]interface{}{"callback_query_id": cb.ID})
	if cb.Message == nil {
		return true
	}
	parts := strings.SplitN(cb.Data, ":", 2)
	approvalState.Lock()
	p, ok := approvalState.pending[parts[1]]
	if ok && p.ChatID == cb.Message.Chat.ID && p.UserID == cb.From.ID && allowed(cb.From.ID) {
		delete(approvalState.pending, parts[1])
		select {
		case p.Decision <- parts[0] == "approve":
		default:
		}
	}
	approvalState.Unlock()
	return true
}

func executionSettingsText() string {
	c := configSnapshot()
	label := map[string]string{"manual": "人工确认", "auto": "自动执行", "read-only": "只读"}[approvalMode(c)]
	return fmt.Sprintf("执行设置\n\n当前模式：%s\n\n人工确认：命令与文件修改前显示一次性审批。\n自动执行：按任务授权连续执行，直到完成或达到限制。\n只读：禁止任意 shell、文件修改和 Git 写入。\n\n任务和子任务继承同一权限；外部提示词无法修改模式。\n命令超时 %d 秒 · 任务超时 %d 秒\n\n切换：/mode manual | auto | read-only", label, c.CommandTimeoutSeconds, c.TaskTimeoutSeconds)
}
func executionSettingsMenu() interface{} {
	return keyboard([][]Btn{{btn("人工确认", "mode:manual"), btn("自动执行", "mode:auto")}, {btn("只读", "mode:read-only")}, {btn("返回设置", "nav:admin")}})
}
func handleExecutionCommand(chatID, uid int64, rest string) bool {
	if !isAdmin(uid) {
		sendMessage(chatID, "只有管理员能更改执行模式。", nil)
		return true
	}
	mode := strings.TrimSpace(rest)
	if mode != "" {
		if mode != "manual" && mode != "auto" && mode != "read-only" {
			sendMessage(chatID, "用法：/mode manual | auto | read-only", nil)
			return true
		}
		if err := updateConfig(func(c *Config) { c.ApprovalMode = mode }); err != nil {
			sendMessage(chatID, "保存失败："+err.Error(), nil)
			return true
		}
	}
	sendMessage(chatID, executionSettingsText(), executionSettingsMenu())
	return true
}
func handleExecutionCallback(cb CallbackQuery) bool {
	if cb.Data != "nav:execution" && !strings.HasPrefix(cb.Data, "mode:") {
		return false
	}
	if cb.Message == nil {
		return true
	}
	if !isAdmin(cb.From.ID) || !allowed(cb.From.ID) {
		answerCallback(cb.ID)
		return true
	}
	if strings.HasPrefix(cb.Data, "mode:") {
		mode := strings.TrimPrefix(cb.Data, "mode:")
		if mode != "manual" && mode != "auto" && mode != "read-only" {
			return true
		}
		if err := updateConfig(func(c *Config) { c.ApprovalMode = mode }); err != nil {
			sendMessage(cb.Message.Chat.ID, "保存失败："+err.Error(), nil)
			return true
		}
	}
	answerCallback(cb.ID)
	editMessage(cb.Message.Chat.ID, cb.Message.MessageID, executionSettingsText(), executionSettingsMenu())
	return true
}
