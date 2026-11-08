package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// toolLabels gives each tool a short bilingual label for panels. No emoji:
// the UI stays plain text.
var toolLabels = map[string][2]string{
	"bash":            {"执行命令", "Run command"},
	"read_file":       {"读取文件", "Read file"},
	"write_file":      {"写入文件", "Write file"},
	"edit_file":       {"编辑文件", "Edit file"},
	"glob":            {"查找文件", "Find files"},
	"grep":            {"搜索内容", "Search code"},
	"list_dir":        {"列出目录", "List folder"},
	"repo_map":        {"项目地图", "Map project"},
	"git":             {"Git", "Git"},
	"undo":            {"撤销改动", "Undo edits"},
	"plan":            {"更新计划", "Update plan"},
	"web_search":      {"联网搜索", "Web search"},
	"web_fetch":       {"读取网页", "Fetch page"},
	"net_check":       {"网络检测", "Network check"},
	"memory":          {"备忘", "Memory"},
	"schedule":        {"定时任务", "Schedule"},
	"send_file":       {"发送文件", "Send file"},
	"list_skills":     {"列出技能", "List skills"},
	"load_skill":      {"加载技能", "Load skill"},
	"discover_skills": {"查找技能", "Find skills"},
	"install_skill":   {"安装技能", "Install skill"},
	"spawn_agent":     {"子任务", "Sub-agent"},
}

func toolLabel(lang, name string) string {
	m, ok := toolLabels[canonicalToolName(name)]
	if !ok {
		return name
	}
	if lang == langEN {
		return m[1]
	}
	return m[0]
}

type progressAction struct {
	Tool, Detail, Intent, Output string
	Done, Failed                 bool
}

// progressPanel is the live status message of one task.
type progressPanel struct {
	mu       sync.Mutex
	lang     string
	detailed bool
	origin   string
	started  time.Time
	actions  []progressAction
	note     string
	plan     []planItem
	counts   map[string]int
}

func newProgressPanel(lang string, detailed bool, origin string) *progressPanel {
	return &progressPanel{lang: lang, detailed: detailed, origin: origin, started: time.Now(), counts: map[string]int{}}
}

func (p *progressPanel) apply(ev StepEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case ev.Final:
	case ev.Note != "":
		p.note = ev.Note
	case !ev.Done:
		p.actions = append(p.actions, progressAction{Tool: ev.Tool, Detail: ev.Detail, Intent: ev.Intent})
		p.counts[ev.Tool]++
	default:
		for i := len(p.actions) - 1; i >= 0; i-- {
			if a := &p.actions[i]; a.Tool == ev.Tool && !a.Done {
				a.Done, a.Failed, a.Output = true, ev.Failed, ev.Output
				break
			}
		}
		if ev.Plan != nil {
			p.plan = ev.Plan
		}
	}
}

func (p *progressPanel) snapshot() []progressAction {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]progressAction(nil), p.actions...)
}

func (p *progressPanel) toolCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.actions)
}

func (p *progressPanel) header(state string) string {
	h := "<b>" + esc(state) + "</b>"
	if p.origin != "" {
		h = esc(p.origin) + " · " + h
	}
	return h + " · " + clock(time.Since(p.started))
}

// planMark is the status prefix for a plan step.
func planMark(status string) string {
	switch status {
	case planCompleted:
		return "[x]"
	case planInProgress:
		return "[>]"
	}
	return "[ ]"
}

func renderPlanHTML(lang string, plan []planItem, limit int) string {
	if len(plan) == 0 {
		return ""
	}
	done, total := planProgress(plan)
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b> %d/%d\n", esc(tr(lang, "progress.plan")), done, total)
	for i, it := range plan {
		if i >= limit {
			fmt.Fprintf(&b, "   … +%d\n", len(plan)-limit)
			break
		}
		text := esc(short(it.Text, 80))
		if it.Status == planInProgress {
			text = "<b>" + text + "</b>"
		}
		fmt.Fprintf(&b, "%s %s\n", planMark(it.Status), text)
	}
	return b.String()
}

// actionMark marks a tool step: pending, done, or failed.
func actionMark(done, failed bool) string {
	switch {
	case !done:
		return "·"
	case failed:
		return "!"
	}
	return "✓"
}

// render builds the live panel. It shrinks progressively to fit Telegram.
func (p *progressPanel) render() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, opts := range []struct {
		actions, planLimit int
		outputs            bool
	}{{6, 10, p.detailed}, {4, 8, false}, {2, 5, false}} {
		if s := p.renderWith(opts.actions, opts.planLimit, opts.outputs); utf16Len(s) <= telegramChunk {
			return s
		}
	}
	return p.header(tr(p.lang, "progress.running"))
}

func (p *progressPanel) renderWith(maxActions, planLimit int, outputs bool) string {
	var b strings.Builder
	b.WriteString(p.header(tr(p.lang, "progress.running")) + "\n")
	if p.note != "" {
		b.WriteString("<i>" + esc(short(p.note, 240)) + "</i>\n")
	}
	if plan := renderPlanHTML(p.lang, p.plan, planLimit); plan != "" {
		b.WriteString("\n" + plan)
	}
	if len(p.actions) == 0 {
		b.WriteString("\n<i>" + esc(tr(p.lang, "progress.thinking")) + "</i>")
		return b.String()
	}
	fmt.Fprintf(&b, "\n<b>%s</b> (%d)\n", esc(tr(p.lang, "progress.steps")), len(p.actions))
	start := max(0, len(p.actions)-maxActions)
	if start > 0 {
		b.WriteString("<i>" + esc(tr(p.lang, "progress.earlier", start)) + "</i>\n")
	}
	for _, a := range p.actions[start:] {
		line := actionMark(a.Done, a.Failed) + " " + esc(toolLabel(p.lang, a.Tool))
		if a.Detail != "" {
			line += " <code>" + esc(short(a.Detail, 60)) + "</code>"
		}
		b.WriteString(line + "\n")
		if a.Intent != "" {
			b.WriteString("    ↳ <i>" + esc(short(a.Intent, 120)) + "</i>\n")
		}
		if outputs && a.Done && strings.TrimSpace(a.Output) != "" {
			b.WriteString("<blockquote expandable>" + esc(previewText(a.Output, 6, 300)) + "</blockquote>\n")
		}
	}
	if last := p.actions[len(p.actions)-1]; last.Done {
		b.WriteString("<i>" + esc(tr(p.lang, "progress.thinking")) + "</i>")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderFinal collapses the panel once the task ends: state, plan and a
// compact histogram of the tools used.
func (p *progressPanel) renderFinal(state string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	b.WriteString(p.header(state))
	if plan := renderPlanHTML(p.lang, p.plan, 10); plan != "" {
		b.WriteString("\n\n" + strings.TrimRight(plan, "\n"))
	}
	if len(p.counts) > 0 {
		names := make([]string, 0, len(p.counts))
		for name := range p.counts {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool {
			if p.counts[names[i]] != p.counts[names[j]] {
				return p.counts[names[i]] > p.counts[names[j]]
			}
			return names[i] < names[j]
		})
		var parts []string
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%s×%d", esc(toolLabel(p.lang, name)), p.counts[name]))
		}
		b.WriteString("\n\n" + strings.Join(parts, " · "))
	}
	return b.String()
}
