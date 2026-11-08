package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// The plan is the agent's self-made task list. It is shown live in the
// Telegram progress panel and belongs to one agent (sub-agents get their own).

const (
	planPending    = "pending"
	planInProgress = "in_progress"
	planCompleted  = "completed"
)

type planItem struct {
	ID     int
	Text   string
	Status string
}

type planState struct {
	mu    sync.Mutex
	items []planItem
}

type planStateKey struct{}

func withPlanContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, planStateKey{}, &planState{})
}

func taskPlan(ctx context.Context) *planState {
	if p, ok := ctx.Value(planStateKey{}).(*planState); ok {
		return p
	}
	return &planState{}
}

func planSnapshot(ctx context.Context) []planItem {
	p := taskPlan(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]planItem(nil), p.items...)
}

func (p *planState) find(id int) *planItem {
	for i := range p.items {
		if p.items[i].ID == id {
			return &p.items[i]
		}
	}
	return nil
}

func (p *planState) add(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("text 不能为空")
	}
	if len(p.items) >= 30 {
		return fmt.Errorf("计划最多 30 步")
	}
	next := 1
	if n := len(p.items); n > 0 {
		next = p.items[n-1].ID + 1
	}
	p.items = append(p.items, planItem{ID: next, Text: short(text, 300), Status: planPending})
	return nil
}

// target resolves the step an action refers to: explicit id, otherwise the
// current in-progress step, otherwise the first pending one.
func (p *planState) target(args map[string]interface{}) (*planItem, error) {
	if id := intArg(args, "id", 0, 0, 1000); id > 0 {
		if item := p.find(id); item != nil {
			return item, nil
		}
		return nil, fmt.Errorf("步骤编号 %d 不存在", id)
	}
	for _, status := range []string{planInProgress, planPending} {
		for i := range p.items {
			if p.items[i].Status == status {
				return &p.items[i], nil
			}
		}
	}
	return nil, fmt.Errorf("没有可操作的步骤，请提供 id")
}

func toolPlanContext(ctx context.Context, args map[string]interface{}) (string, error) {
	p := taskPlan(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	action, text := strArg(args, "action"), strArg(args, "text")
	switch action {
	case "set":
		raw, _ := args["items"].([]interface{})
		if len(raw) == 0 {
			return "", fmt.Errorf("set 需要 items 数组")
		}
		p.items = nil
		for _, v := range raw {
			if s, ok := v.(string); ok {
				if err := p.add(s); err != nil {
					return "", err
				}
			}
		}
	case "add":
		if err := p.add(text); err != nil {
			return "", err
		}
	case "start", "done", "update", "remove":
		item, err := p.target(args)
		if err != nil {
			return "", err
		}
		switch action {
		case "start":
			item.Status = planInProgress
		case "done":
			item.Status = planCompleted
		case "update":
			if strings.TrimSpace(text) == "" {
				return "", fmt.Errorf("update 需要 text")
			}
			item.Text = short(strings.TrimSpace(text), 300)
		case "remove":
			id := item.ID
			kept := p.items[:0]
			for _, it := range p.items {
				if it.ID != id {
					kept = append(kept, it)
				}
			}
			p.items = kept
		}
	case "clear":
		p.items = nil
	case "list", "":
	default:
		return "", fmt.Errorf("未知计划操作 %q，可用 set/add/start/done/update/remove/list/clear", action)
	}
	return renderPlanText(p.items), nil
}

// toolTodoContext is the v2.0 name of the plan tool (kept for compatibility).
func toolTodoContext(ctx context.Context, args map[string]interface{}) (string, error) {
	return toolPlanContext(ctx, args)
}

func planProgress(items []planItem) (done, total int) {
	for _, it := range items {
		if it.Status == planCompleted {
			done++
		}
	}
	return done, len(items)
}

func renderPlanText(items []planItem) string {
	if len(items) == 0 {
		return "（计划为空）"
	}
	done, total := planProgress(items)
	var b strings.Builder
	fmt.Fprintf(&b, "计划 %d/%d\n", done, total)
	for _, it := range items {
		markText := "[ ]"
		switch it.Status {
		case planCompleted:
			markText = "[x]"
		case planInProgress:
			markText = "[>]"
		}
		fmt.Fprintf(&b, "%s %d. %s\n", markText, it.ID, it.Text)
	}
	return b.String()
}

// todoSnapshotContext lists unfinished steps (used in summaries).
func todoSnapshotContext(ctx context.Context) string {
	var b strings.Builder
	for _, it := range planSnapshot(ctx) {
		if it.Status != planCompleted {
			fmt.Fprintf(&b, "- %s\n", it.Text)
		}
	}
	return b.String()
}
