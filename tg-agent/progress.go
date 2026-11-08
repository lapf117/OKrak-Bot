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
	Started                      time.Time
	Took                         time.Duration
}

// progressPanel is the live status message of one task.
type progressPanel struct {
	mu      sync.Mutex
	lang    string
	show    display
	origin  string
	started time.Time
	actions []progressAction
	note    string // the model's narration for the current batch of tools
	thought string // the latest reasoning text, when the provider returns it
	plan    []planItem
	counts  map[string]int
	stats   func() taskStats // live counters of the task, optional
	frame   int
}

// newProgressPanel creates a panel with default switches; detailed adds
// tool output previews.
func newProgressPanel(lang string, detailed bool, origin string) *progressPanel {
	d := display{}
	for _, it := range displayItems {
		d[it.Key] = it.Default && !it.AdminOnly
	}
	d["output"] = detailed
	return newTaskPanel(lang, d, origin)
}

func newTaskPanel(lang string, d display, origin string) *progressPanel {
	return &progressPanel{lang: lang, show: d, origin: origin, started: time.Now(), counts: map[string]int{}}
}

func (p *progressPanel) apply(ev StepEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case ev.Final:
	case ev.Thought:
		p.note = ev.Note
		if ev.Reasoning != "" {
			p.thought = ev.Reasoning
		}
	case ev.Note != "":
		p.note = ev.Note
	case !ev.Done:
		p.actions = append(p.actions, progressAction{Tool: ev.Tool, Detail: ev.Detail, Intent: ev.Intent, Started: time.Now()})
		p.counts[ev.Tool]++
	default:
		for i := len(p.actions) - 1; i >= 0; i-- {
			if a := &p.actions[i]; a.Tool == ev.Tool && !a.Done {
				a.Done, a.Failed, a.Output = true, ev.Failed, ev.Output
				a.Took = time.Since(a.Started)
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

// spinner frames: plain geometric shapes, never emoji.
var spinnerFrames = []string{"◐", "◓", "◑", "◒"}

func (p *progressPanel) header(mark, state string) string {
	h := "<b>" + mark + " " + esc(state) + "</b> · " + clock(time.Since(p.started))
	if p.origin != "" {
		h = esc(p.origin) + " · " + h
	}
	return h
}

// liveState names what the task is doing right now.
func (p *progressPanel) liveState() string {
	if n := len(p.actions); n > 0 && !p.actions[n-1].Done {
		return tr(p.lang, "progress.running")
	}
	return tr(p.lang, "progress.thinking")
}

// section starts a titled block: ▍Title  extra
func section(title, extra string) string {
	s := "<b>▍" + esc(title) + "</b>"
	if extra != "" {
		s += "  " + extra
	}
	return s + "\n"
}

// planMark is the status prefix for a plan step.
func planMark(status string) string {
	switch status {
	case planCompleted:
		return "✓"
	case planInProgress:
		return "▸"
	}
	return "○"
}

func renderPlanHTML(lang string, plan []planItem, limit int) string {
	if len(plan) == 0 {
		return ""
	}
	done, total := planProgress(plan)
	var b strings.Builder
	b.WriteString(section(tr(lang, "progress.plan"), fmt.Sprintf("%d/%d", done, total)))
	for i, it := range plan {
		if i >= limit {
			fmt.Fprintf(&b, "   … +%d\n", len(plan)-limit)
			break
		}
		text := esc(stripEmoji(short(it.Text, 80)))
		if it.Status == planInProgress {
			text = "<b>" + text + "</b>"
		}
		fmt.Fprintf(&b, "%s %s\n", planMark(it.Status), text)
	}
	return b.String()
}

// actionMark marks a tool step: running, done, or failed.
func actionMark(done, failed bool) string {
	switch {
	case !done:
		return "◌"
	case failed:
		return "✗"
	}
	return "✓"
}

// stepDuration renders how long one tool step took.
func stepDuration(d time.Duration) string {
	if d < 100*time.Millisecond {
		return "<0.1s"
	}
	return humanDuration(d)
}

// tailRunes keeps the last n runes, marking the cut.
func tailRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return "…" + strings.TrimLeft(string(r[len(r)-n:]), " \n")
}

// liveStats is the counters line under the header, following the footer switches.
func (p *progressPanel) liveStats() string {
	if p.stats == nil {
		return ""
	}
	d := display{"calls": p.show["calls"], "tools": p.show["tools"], "cmds": p.show["cmds"], "tokens": p.show["tokens"]}
	return renderFooter(p.lang, d, footerData{Stats: p.stats()})
}

type panelLayout struct {
	actions, planLimit, thoughtRunes int
	outputs                          bool
}

// render builds the live panel. It shrinks progressively to fit Telegram.
func (p *progressPanel) render() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frame++
	for _, l := range []panelLayout{{6, 10, 600, p.show["output"]}, {4, 8, 300, false}, {2, 5, 0, false}} {
		if s := p.renderWith(l); utf16Len(s) <= telegramChunk {
			return s
		}
	}
	return p.header(spinnerFrames[p.frame%len(spinnerFrames)], p.liveState())
}

func (p *progressPanel) renderWith(l panelLayout) string {
	var b strings.Builder
	b.WriteString(p.header(spinnerFrames[p.frame%len(spinnerFrames)], p.liveState()) + "\n")
	if stats := p.liveStats(); stats != "" {
		b.WriteString(stats + "\n")
	}
	if plan := renderPlanHTML(p.lang, p.plan, l.planLimit); plan != "" {
		b.WriteString("\n" + plan)
	}
	if p.show["think"] {
		if thinking := p.renderThinking(l.thoughtRunes, true); thinking != "" {
			b.WriteString("\n" + thinking)
		}
	}
	if len(p.actions) > 0 {
		b.WriteString("\n" + p.renderActions(l.actions, l.outputs))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderThinking shows the reasoning (collapsed) and the narration line.
func (p *progressPanel) renderThinking(thoughtRunes int, withNote bool) string {
	var body strings.Builder
	if thought := strings.TrimSpace(stripEmoji(p.thought)); thought != "" && thoughtRunes > 0 {
		body.WriteString("<blockquote expandable>" + esc(tailRunes(thought, thoughtRunes)) + "</blockquote>\n")
	}
	if note := strings.TrimSpace(stripEmoji(p.note)); note != "" && withNote {
		body.WriteString("<i>" + esc(short(note, 240)) + "</i>\n")
	}
	if body.Len() == 0 {
		return ""
	}
	return section(tr(p.lang, "progress.thought"), "") + body.String()
}

func (p *progressPanel) renderActions(maxActions int, outputs bool) string {
	var b strings.Builder
	b.WriteString(section(tr(p.lang, "progress.steps"), esc(tr(p.lang, "progress.count", len(p.actions)))))
	if !p.show["trace"] {
		last := p.actions[len(p.actions)-1]
		fmt.Fprintf(&b, "%s %s\n", actionMark(last.Done, last.Failed), esc(toolLabel(p.lang, last.Tool)))
		return b.String()
	}
	start := max(0, len(p.actions)-maxActions)
	if start > 0 {
		b.WriteString("<i>" + esc(tr(p.lang, "progress.earlier", start)) + "</i>\n")
	}
	for i, a := range p.actions[start:] {
		line := fmt.Sprintf("%s <b>%02d</b> %s", actionMark(a.Done, a.Failed), start+i+1, esc(toolLabel(p.lang, a.Tool)))
		if a.Detail != "" {
			line += " <code>" + esc(short(a.Detail, 60)) + "</code>"
		}
		if a.Done {
			line += " <i>" + esc(stepDuration(a.Took)) + "</i>"
		}
		b.WriteString(line + "\n")
		if intent := strings.TrimSpace(stripEmoji(a.Intent)); intent != "" {
			b.WriteString("   ╰ <i>" + esc(short(intent, 120)) + "</i>\n")
		}
		if outputs && a.Done && strings.TrimSpace(a.Output) != "" {
			b.WriteString("<blockquote expandable>" + esc(stripEmoji(previewText(a.Output, 6, 300))) + "</blockquote>\n")
		}
	}
	return b.String()
}

// renderFinal collapses the panel once the task ends: state, plan, a
// histogram of the tools used, the step list and the last reasoning, both
// collapsed so the chat stays tidy.
func (p *progressPanel) renderFinal(mark, state string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range []panelLayout{{40, 10, 1500, false}, {15, 8, 500, false}, {0, 5, 0, false}} {
		if s := p.renderFinalWith(mark, state, l); utf16Len(s) <= telegramChunk {
			return s
		}
	}
	return p.header(mark, state)
}

func (p *progressPanel) renderFinalWith(mark, state string, l panelLayout) string {
	var b strings.Builder
	b.WriteString(p.header(mark, state) + "\n")
	if plan := renderPlanHTML(p.lang, p.plan, l.planLimit); plan != "" {
		b.WriteString("\n" + plan)
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
			parts = append(parts, fmt.Sprintf("%s ×%d", esc(toolLabel(p.lang, name)), p.counts[name]))
		}
		b.WriteString("\n" + section(tr(p.lang, "progress.steps"), esc(tr(p.lang, "progress.count", len(p.actions)))))
		b.WriteString(strings.Join(parts, " · ") + "\n")
		if p.show["trace"] && l.actions > 0 {
			var list []string
			start := max(0, len(p.actions)-l.actions)
			if start > 0 {
				list = append(list, esc(tr(p.lang, "progress.earlier", start)))
			}
			for i, a := range p.actions[start:] {
				line := fmt.Sprintf("%02d %s %s", start+i+1, actionMark(a.Done, a.Failed), esc(toolLabel(p.lang, a.Tool)))
				if a.Detail != "" {
					line += " · " + esc(short(a.Detail, 48))
				}
				if a.Done {
					line += " · " + esc(stepDuration(a.Took))
				}
				list = append(list, line)
			}
			b.WriteString("<blockquote expandable>" + strings.Join(list, "\n") + "</blockquote>\n")
		}
	}
	if p.show["think"] {
		if thinking := p.renderThinking(l.thoughtRunes, false); thinking != "" {
			b.WriteString("\n" + thinking)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
