package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------- run logs ----------------

type runEntry struct {
	name string
	t    time.Time
	size int64
}

func recentRuns(n int) []runEntry {
	entries, err := os.ReadDir(filepath.Join(baseDir, "runs"))
	if err != nil {
		return nil
	}
	var items []runEntry
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		items = append(items, runEntry{e.Name(), info.ModTime(), info.Size()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].t.After(items[j].t) })
	if n >= 0 && len(items) > n {
		items = items[:n]
	}
	return items
}

func listRunsHTML(lang string, n int) string {
	items := recentRuns(n)
	if len(items) == 0 {
		return esc(tr(lang, "runs.empty"))
	}
	var b strings.Builder
	b.WriteString("<b>" + esc(tr(lang, "runs.title", len(items))) + "</b>\n\n")
	for i, it := range items {
		fmt.Fprintf(&b, "%d. %s · %s KB\n", i+1, it.t.Format("01-02 15:04"), fmt.Sprintf("%.1f", float64(it.size)/1024))
	}
	b.WriteString("\n<i>" + esc(tr(lang, "runs.hint")) + "</i>")
	return b.String()
}

func listRuns(n int) string { return stripTags(listRunsHTML(langZH, n)) }

func runsKeyboard(lang string, n int) interface{} {
	var row []Btn
	for i, it := range recentRuns(n) {
		row = append(row, btn(fmt.Sprintf("%d", i+1), "runlog:"+it.name))
	}
	rows := [][]Btn{}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []Btn{btn(tr(lang, "btn.refresh"), "act:runs"), btn(tr(lang, "btn.back"), "adm:root")})
	return keyboard(rows)
}

// ---------------- health ----------------

func healthCheck(lang string) string {
	c := configSnapshot()
	var b strings.Builder
	b.WriteString("<b>" + esc(tr(lang, "health.title")) + "</b>\n\n")
	line := func(ok bool, label, detail string) {
		icon := ""
		if !ok {
			icon = ""
		}
		fmt.Fprintf(&b, "%s <b>%s</b> %s\n", icon, esc(label), esc(detail))
	}
	start := time.Now()
	resp, err := chat([]LLMMessage{textMsg("user", "Reply with one word: OK")}, nil)
	if err != nil {
		line(false, tr(lang, "health.model"), short(err.Error(), 120))
	} else {
		line(true, tr(lang, "health.model"), fmt.Sprintf("%dms · %s", time.Since(start).Milliseconds(), oneLine(textOf(resp.Choices[0].Message), 20)))
	}
	start = time.Now()
	out, err := runShell("echo backend_ok; pwd", 20*time.Second)
	if err != nil || !strings.Contains(out, "backend_ok") {
		line(false, tr(lang, "health.backend"), short(fmt.Sprint(err, " ", out), 120))
	} else {
		line(true, tr(lang, "health.backend"), fmt.Sprintf("%dms · %s", time.Since(start).Milliseconds(), oneLine(strings.TrimPrefix(strings.TrimSpace(out), "backend_ok"), 60)))
	}
	if res, err := runShell("df -h . | tail -1 | awk '{print $5\" disk\"}'; free -m 2>/dev/null | awk 'NR==2{print $3\"M/\"$2\"M mem\"}'", 20*time.Second); err == nil {
		fmt.Fprintf(&b, "%s\n", esc(strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(res), "\n", " · ")), " ")))
	}
	sk, _ := loadSkillIndex()
	fmt.Fprintf(&b, "\n%s %d · %s %d · %s %d\n", esc(tr(lang, "health.tools")), len(allTools()), esc(tr(lang, "health.skills")), len(sk), esc(tr(lang, "health.jobs")), len(cronSnapshot()))
	fmt.Fprintf(&b, "<code>%s</code> · %s %s · v%s", esc(c.Model), esc(tr(lang, "effort.title")), esc(effortLabel(lang, effectiveEffort(c))), version)
	return b.String()
}

func tailLog(n int) string {
	data, err := os.ReadFile(filepath.Join(baseDir, "agent.log"))
	if err != nil {
		return "-"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func mustSkills() []Skill {
	s, _ := loadSkillIndex()
	return s
}

func configOverview(lang string) string {
	c := configSnapshot()
	rows := [][2]string{
		{"owner", fmt.Sprintf("%d %s", c.OwnerID, userName(c.OwnerID))},
		{"model", c.Model},
		{"provider", fmt.Sprintf("%s (%d)", c.ProviderName, len(c.Provider))},
		{"workspace", c.Workspace},
		{"backend", c.Backend},
		{"mode", approvalMode(c)},
		{"max_steps", fmt.Sprint(c.MaxSteps)},
		{"temperature", fmt.Sprintf("%.1f", c.Temperature)},
		{"max_history", fmt.Sprint(c.MaxHistory)},
		{"language", c.Language},
		{"timezone", scheduleLocation(c).String()},
		{"skills_dir", c.SkillsDir},
		{"persona", onOff(lang, strings.TrimSpace(c.Persona) != "")},
		{"system_prompt", onOff(lang, strings.TrimSpace(c.SystemPrompt) != "")},
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%-13s %s\n", r[0], r[1])
	}
	return "<b>" + esc(tr(lang, "config.title")) + "</b>\n<pre>" + esc(b.String()) + "</pre>"
}

func onOff(lang string, v bool) string {
	if v {
		return tr(lang, "config.set")
	}
	return tr(lang, "config.unset")
}

// ---------------- export ----------------

// exportConversation sends the current conversation as a Markdown file.
func exportConversation(chatID, uid int64) {
	lang := langFor(uid)
	history := getHistory(chatID)
	if len(history) == 0 {
		sendMessage(chatID, tr(lang, "export.empty"), nil)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n_%s_\n\n", tr(lang, "export.title"), time.Now().Format("2006-01-02 15:04"))
	for _, m := range history {
		switch m.Role {
		case "user":
			b.WriteString("## " + tr(lang, "export.user") + "\n\n" + textOf(m) + "\n\n")
		case "assistant":
			if t := strings.TrimSpace(textOf(m)); t != "" {
				b.WriteString("## " + tr(lang, "export.assistant") + "\n\n" + t + "\n\n")
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "- %s `%s`\n", toolLabel(lang, tc.Function.Name), oneLine(summarizeArgs(tc.Function.Name, tc.Function.Arguments), 120))
			}
			if len(m.ToolCalls) > 0 {
				b.WriteString("\n")
			}
		}
	}
	name := "conversation-" + time.Now().Format("20060102-150405") + ".md"
	if !sendMedia(chatID, "sendDocument", "document", []byte(redactSecrets(b.String())), name, tr(lang, "export.caption")) {
		sendMessage(chatID, tr(lang, "file.send_failed"), nil)
	}
}
