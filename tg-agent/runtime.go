package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type runtimeKey int

const (
	configContextKey runtimeKey = iota
	identityContextKey
	budgetContextKey
)

type executionIdentityValue struct {
	ChatID, UserID int64
	Depth          int
}
type executionBudget struct {
	Steps    int64
	Children int64
}

// WithExecutionConfig freezes connection, workspace and limits for an entire task.
func WithExecutionConfig(ctx context.Context, c Config) context.Context {
	return context.WithValue(ctx, configContextKey, cloneConfig(c))
}
func executionConfig(ctx context.Context) Config {
	if c, ok := ctx.Value(configContextKey).(Config); ok {
		return c
	}
	return configSnapshot()
}
func executionIdentity(ctx context.Context) (chatID, uid int64, depth int) {
	if v, ok := ctx.Value(identityContextKey).(executionIdentityValue); ok {
		return v.ChatID, v.UserID, v.Depth
	}
	return 0, 0, 0
}

var taskRegistry = struct {
	sync.Mutex
	tasks map[int64]context.CancelFunc
}{tasks: make(map[int64]context.CancelFunc)}

func beginTask(chatID int64) (context.Context, func(), bool) {
	taskRegistry.Lock()
	defer taskRegistry.Unlock()
	if telegramContext.Err() != nil {
		return nil, nil, false
	}
	if _, exists := taskRegistry.tasks[chatID]; exists {
		return nil, nil, false
	}
	c := configSnapshot()
	seconds := c.TaskTimeoutSeconds
	if seconds <= 0 {
		seconds = 1800
	}
	ctx, cancel := context.WithTimeout(WithExecutionConfig(telegramContext, c), time.Duration(seconds)*time.Second)
	taskRegistry.tasks[chatID] = cancel
	var once sync.Once
	finish := func() {
		once.Do(func() { cancel(); taskRegistry.Lock(); delete(taskRegistry.tasks, chatID); taskRegistry.Unlock() })
	}
	return ctx, finish, true
}
func stopTask(chatID int64) bool {
	taskRegistry.Lock()
	cancel, ok := taskRegistry.tasks[chatID]
	taskRegistry.Unlock()
	if ok {
		cancel()
	}
	// Keep old handlers compatible during migration.
	if f, exists := stopFlags.Load(chatID); exists {
		atomic.StoreInt32(f.(*int32), 1)
		ok = true
	}
	return ok
}
func stopAllTasks() {
	taskRegistry.Lock()
	defer taskRegistry.Unlock()
	for _, cancel := range taskRegistry.tasks {
		cancel()
	}
}

func consumeStep(ctx context.Context) error {
	budget, _ := ctx.Value(budgetContextKey).(*executionBudget)
	if budget == nil {
		return nil
	}
	if atomic.AddInt64(&budget.Steps, -1) < 0 {
		return fmt.Errorf("已达到本次任务的模型调用预算")
	}
	return ctx.Err()
}

func spawnAgentContext(ctx context.Context, task string) (string, error) {
	chatID, uid, depth := executionIdentity(ctx)
	if uid == 0 {
		return "", fmt.Errorf("子任务缺少用户身份")
	}
	c := executionConfig(ctx)
	limit := c.MaxSubagents
	if limit <= 0 {
		limit = 3
	}
	budget, ok := ctx.Value(budgetContextKey).(*executionBudget)
	if !ok {
		return "", fmt.Errorf("子任务缺少共享预算")
	}
	if atomic.AddInt64(&budget.Children, 1) > int64(limit) {
		return "", fmt.Errorf("子智能体总数达到上限 %d", limit)
	}
	if depth >= 3 {
		return "", fmt.Errorf("子智能体层级达到上限")
	}
	return runAgentContext(ctx, chatID, uid, task, nil, depth+1, nil)
}

func executionUID(ctx context.Context) int64 { _, uid, _ := executionIdentity(ctx); return uid }

// Current membership is checked independently from the captured execution mode.
func checkExecutionAccess(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if uid := executionUID(ctx); uid != 0 && !allowed(uid) {
		return fmt.Errorf("当前用户的任务权限已被撤销")
	}
	return nil
}

// A running task must also redact the credentials it captured before a settings change.
func redactExecutionSecrets(ctx context.Context, s string) string {
	s = redactSecrets(s)
	c := executionConfig(ctx)
	secrets := []string{c.Token, c.ASR.APIKey}
	for _, p := range c.Provider {
		secrets = append(secrets, p.APIKey)
	}
	if c.Proxy != "" {
		if u, err := url.Parse(c.Proxy); err == nil && u.User != nil {
			if pass, ok := u.User.Password(); ok {
				secrets = append(secrets, pass)
			}
		}
	}
	for _, secret := range secrets {
		if len(secret) >= 6 {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	return s
}
