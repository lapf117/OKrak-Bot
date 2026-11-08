package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// toolMeta gives each tool an icon and a short bilingual label for panels.
var toolMeta = map[string]struct{ icon, zh, en string }{
	"bash":            {"🖥", "执行命令", "Run command"},
	"read_file":       {"📖", "读取文件", "Read file"},
	"write_file":      {"📝", "写入文件", "Write file"},
	"edit_file":       {"✏️", "编辑文件", "Edit file"},
	"glob":            {"🗂", "查找文件", "Find files"},
	"grep":            {"🔍", "搜索内容", "Search code"},
	"list_dir":        {"📂", "列出目录", "List folder"},
	"repo_map":        {"🗺", "项目地图", "Map project"},
	"git":             {"🌿", "Git", "Git"},
	"undo":            {"↩️", "撤销改动", "Undo edits"},
	"plan":            {"📋", "更新计划", "Update plan"},
	"web_search":      {"🔎", "联网搜索", "Web search"},
	"web_fetch":       {"🌐", "读取网页", "Fetch page"},
	"net_check":       {"📡", "网络检测", "Network check"},
	"memory":          {"🧠", "备忘", "Memory"},
	"schedule":        {"⏰", "定时任务", "Schedule"},
	"send_file":       {"📤", "发送文件", "Send file"},
	"list_skills":     {"🧩", "列出技能", "List skills"},
	"load_skill":      {"🧩", "加载技能", "Load skill"},
	"discover_skills": {"🧩", "查找技能", "Find skills"},
	"install_skill":   {"🧩", "安装技能", "Install skill"},
	"spawn_agent":     {"🤖", "子任务", "Sub-agent"},
}

func toolLabel(lang, name string) (string, string) {
	m, ok := toolMeta[canonicalToolName(name)]
	if !ok {
		return "🔧", name
	}
	if lang == langEN {
		return m.icon, m.en
	}
	return m.icon, m.zh
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

func (p *progressPanel) header(icon, state string) string {
	h := icon + " <b>" + esc(state) + "</b>"
	if p.origin != "" {
		h = esc(p.origin) + " · " + h
	}
	return h + " · " + clock(time.Since(p.started))
}

func renderPlanHTML(lang string, plan []planItem, limit int) string {
	if len(plan) == 0 {
		return ""
	}
	done, total := planProgress(plan)
	var b strings.Builder
	fmt.Fprintf(&b, "📋 <b>%s</b> %d/%d\n", esc(tr(lang, "progress.plan")), done, total)
	for i, it := range plan {
		if i >= limit {
			fmt.Fprintf(&b, "   … +%d\n", len(plan)-limit)
			break
		}
		switch it.Status {
		case planCompleted:
			fmt.Fprintf(&b, "✅ %s\n", esc(short(it.Text, 80)))
		case planInProgress:
			fmt.Fprintf(&b, "🔄 <b>%s</b>\n", esc(short(it.Text, 80)))
		default:
			fmt.Fprintf(&b, "⬜ %s\n", esc(short(it.Text, 80)))
		}
	}
	return b.String()
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
	return p.header("⚙️", tr(p.lang, "progress.running"))
}

func (p *progressPanel) renderWith(maxActions, planLimit int, outputs bool) string {
	var b strings.Builder
	b.WriteString(p.header("⚙️", tr(p.lang, "progress.running")) + "\n")
	if p.note != "" {
		b.WriteString("💭 <i>" + esc(short(p.note, 240)) + "</i>\n")
	}
	if plan := renderPlanHTML(p.lang, p.plan, planLimit); plan != "" {
		b.WriteString("\n" + plan)
	}
	if len(p.actions) == 0 {
		b.WriteString("\n⏳ <i>" + esc(tr(p.lang, "progress.thinking")) + "</i>")
		return b.String()
	}
	fmt.Fprintf(&b, "\n🛠 <b>%s</b> (%d)\n", esc(tr(p.lang, "progress.steps")), len(p.actions))
	start := max(0, len(p.actions)-maxActions)
	if start > 0 {
		b.WriteString("<i>" + esc(tr(p.lang, "progress.earlier", start)) + "</i>\n")
	}
	for _, a := range p.actions[start:] {
		status := "⏳"
		if a.Done {
			status = "✅"
			if a.Failed {
				status = "⚠️"
			}
		}
		icon, label := toolLabel(p.lang, a.Tool)
		line := status + " " + icon + " " + esc(label)
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
		b.WriteString("⏳ <i>" + esc(tr(p.lang, "progress.thinking")) + "</i>")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderFinal collapses the panel once the task ends: state, plan and a
// compact histogram of the tools used.
func (p *progressPanel) renderFinal(icon, state string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	b.WriteString(p.header(icon, state))
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
			icon, label := toolLabel(p.lang, name)
			parts = append(parts, fmt.Sprintf("%s %s×%d", icon, esc(label), p.counts[name]))
		}
		b.WriteString("\n\n🛠 " + strings.Join(parts, " · "))
	}
	return b.String()
}
