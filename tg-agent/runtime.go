package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type runtimeKey int

const (
	configContextKey runtimeKey = iota
	identityContextKey
	budgetContextKey
	langContextKey
)

type executionIdentityValue struct {
	ChatID, UserID int64
	Depth          int
}

// executionBudget is shared by a task and all of its sub-agents.
type executionBudget struct {
	Steps            int64
	Children         int64
	ToolCalls        int64
	PromptTokens     int64
	CompletionTokens int64
	ModelCalls       int64 // successful model API responses
	Commands         int64 // shell commands run by the bash tool
	Retries          int64 // silent model API retries
	approveAll       int32 // set when the user approves every action of this task
}

// taskStats is a snapshot of one task's counters for the panel, footer and run log.
type taskStats struct {
	Calls, Tools, Commands, Retries int64
	Prompt, Completion              int64
}

func (s taskStats) Tokens() int64 { return s.Prompt + s.Completion }

// statsOf reads the shared counters of the running task (zero without a budget).
func statsOf(ctx context.Context) taskStats {
	b := budgetOf(ctx)
	if b == nil {
		return taskStats{}
	}
	return taskStats{
		Calls:      atomic.LoadInt64(&b.ModelCalls),
		Tools:      atomic.LoadInt64(&b.ToolCalls),
		Commands:   atomic.LoadInt64(&b.Commands),
		Retries:    atomic.LoadInt64(&b.Retries),
		Prompt:     atomic.LoadInt64(&b.PromptTokens),
		Completion: atomic.LoadInt64(&b.CompletionTokens),
	}
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

func executionUID(ctx context.Context) int64 { _, uid, _ := executionIdentity(ctx); return uid }

func withLang(ctx context.Context, lang string) context.Context {
	return context.WithValue(ctx, langContextKey, lang)
}

// ctxLang is the reply/UI language of the running task (Chinese by default).
func ctxLang(ctx context.Context) string {
	if l, ok := ctx.Value(langContextKey).(string); ok && l == langEN {
		return langEN
	}
	return langZH
}

func budgetOf(ctx context.Context) *executionBudget {
	b, _ := ctx.Value(budgetContextKey).(*executionBudget)
	return b
}

// withTaskBudget attaches a fresh budget and per-task tool state once.
func withTaskBudget(ctx context.Context, c Config) context.Context {
	if budgetOf(ctx) == nil {
		ctx = context.WithValue(ctx, budgetContextKey, &executionBudget{Steps: int64(maxPositive(c.MaxSteps, 25))})
	}
	return withTodoContext(ctx)
}

func consumeStep(ctx context.Context) error {
	budget := budgetOf(ctx)
	if budget == nil {
		return nil
	}
	if atomic.AddInt64(&budget.Steps, -1) < 0 {
		return fmt.Errorf("已达到本次任务的模型调用预算")
	}
	return ctx.Err()
}

func recordUsage(ctx context.Context, prompt, completion int) {
	if b := budgetOf(ctx); b != nil {
		atomic.AddInt64(&b.ModelCalls, 1)
		atomic.AddInt64(&b.PromptTokens, int64(prompt))
		atomic.AddInt64(&b.CompletionTokens, int64(completion))
	}
}

func recordRetry(ctx context.Context) {
	if b := budgetOf(ctx); b != nil {
		atomic.AddInt64(&b.Retries, 1)
	}
}

func recordCommand(ctx context.Context) {
	if b := budgetOf(ctx); b != nil {
		atomic.AddInt64(&b.Commands, 1)
	}
}

// taskUsage reports tool calls and total tokens for the footer and run log.
func taskUsage(ctx context.Context) (tools, tokens int64) {
	t, p, c := taskUsageDetailed(ctx)
	return t, p + c
}

// taskUsageDetailed also splits prompt and completion tokens.
func taskUsageDetailed(ctx context.Context) (tools, prompt, completion int64) {
	if b := budgetOf(ctx); b != nil {
		return atomic.LoadInt64(&b.ToolCalls), atomic.LoadInt64(&b.PromptTokens), atomic.LoadInt64(&b.CompletionTokens)
	}
	return 0, 0, 0
}

// ---------------- running tasks ----------------

type liveTask struct {
	cancel  context.CancelFunc
	started time.Time
	origin  string
}

var taskRegistry = struct {
	sync.Mutex
	tasks map[int64]*liveTask
}{tasks: make(map[int64]*liveTask)}

func beginTask(chatID int64) (context.Context, func(), bool) {
	return beginTaskWithOrigin(chatID, "")
}

// beginTaskWithOrigin admits one task per private chat with a frozen config.
func beginTaskWithOrigin(chatID int64, origin string) (context.Context, func(), bool) {
	taskRegistry.Lock()
	defer taskRegistry.Unlock()
	if telegramContext.Err() != nil {
		return nil, nil, false
	}
	if _, exists := taskRegistry.tasks[chatID]; exists {
		return nil, nil, false
	}
	c := configSnapshot()
	seconds := maxPositive(c.TaskTimeoutSeconds, 1800)
	ctx, cancel := context.WithTimeout(WithExecutionConfig(telegramContext, c), time.Duration(seconds)*time.Second)
	entry := &liveTask{cancel: cancel, started: time.Now(), origin: origin}
	taskRegistry.tasks[chatID] = entry
	var once sync.Once
	finish := func() {
		once.Do(func() {
			cancel()
			taskRegistry.Lock()
			if taskRegistry.tasks[chatID] == entry {
				delete(taskRegistry.tasks, chatID)
			}
			taskRegistry.Unlock()
		})
	}
	return ctx, finish, true
}

func stopTask(chatID int64) bool {
	taskRegistry.Lock()
	entry, ok := taskRegistry.tasks[chatID]
	taskRegistry.Unlock()
	if ok {
		entry.cancel()
	}
	return ok
}

func stopAllTasks() {
	taskRegistry.Lock()
	defer taskRegistry.Unlock()
	for _, entry := range taskRegistry.tasks {
		entry.cancel()
	}
}

func isBusy(chatID int64) bool {
	taskRegistry.Lock()
	defer taskRegistry.Unlock()
	_, ok := taskRegistry.tasks[chatID]
	return ok
}

// runningTask returns how long the chat's task has been running.
func runningTask(chatID int64) (time.Duration, string, bool) {
	taskRegistry.Lock()
	defer taskRegistry.Unlock()
	if entry, ok := taskRegistry.tasks[chatID]; ok {
		return time.Since(entry.started), entry.origin, true
	}
	return 0, "", false
}

// ---------------- sub-agents ----------------

func spawnAgentContext(ctx context.Context, task string) (string, error) {
	chatID, uid, depth := executionIdentity(ctx)
	if uid == 0 {
		return "", fmt.Errorf("子任务缺少用户身份")
	}
	limit := maxPositive(executionConfig(ctx).MaxSubagents, 3)
	budget := budgetOf(ctx)
	if budget == nil {
		return "", fmt.Errorf("子任务缺少共享预算")
	}
	if atomic.AddInt64(&budget.Children, 1) > int64(limit) {
		return "", fmt.Errorf("子智能体总数达到上限 %d", limit)
	}
	if depth >= 3 {
		return "", fmt.Errorf("子智能体层级达到上限")
	}
	// A child gets its own plan; files, undo history and budget stay shared.
	return runAgentContext(withPlanContext(ctx), chatID, uid, task, nil, depth+1, nil)
}

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
	return redactWith(redactSecrets(s), secretsOf(executionConfig(ctx)))
}
