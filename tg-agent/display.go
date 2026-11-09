package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Display switches decide what each user sees around a reply: the live
// progress panel (thinking, tool steps, output previews, whether the panel
// stays after the answer) and the statistics line under the answer (time,
// model calls, tools, commands, tokens, balance, model). Resolution order:
// the user's own choice, then config.json "display", then the default.

const (
	displayGroupPanel  = "panel"
	displayGroupFooter = "footer"
)

type displayItem struct {
	Key       string
	Group     string
	Default   bool
	AdminOnly bool     // balance belongs to the owner's provider account
	Aliases   []string // accepted by /show besides the key
}

var displayItems = []displayItem{
	{Key: "think", Group: displayGroupPanel, Default: true, Aliases: []string{"thinking", "reasoning", "思考", "思考过程"}},
	{Key: "trace", Group: displayGroupPanel, Default: true, Aliases: []string{"steps", "process", "步骤", "过程", "工具步骤"}},
	{Key: "output", Group: displayGroupPanel, Default: false, Aliases: []string{"outputs", "preview", "输出", "预览", "输出预览"}},
	{Key: "keep", Group: displayGroupPanel, Default: true, Aliases: []string{"panel", "保留", "面板", "保留面板"}},
	{Key: "time", Group: displayGroupFooter, Default: true, Aliases: []string{"duration", "耗时", "时间"}},
	{Key: "calls", Group: displayGroupFooter, Default: true, Aliases: []string{"call", "api", "requests", "调用", "模型调用"}},
	{Key: "tools", Group: displayGroupFooter, Default: true, Aliases: []string{"tool", "工具", "工具调用"}},
	{Key: "cmds", Group: displayGroupFooter, Default: true, Aliases: []string{"cmd", "commands", "command", "命令"}},
	{Key: "tokens", Group: displayGroupFooter, Default: true, Aliases: []string{"token", "用量"}},
	{Key: "balance", Group: displayGroupFooter, Default: false, AdminOnly: true, Aliases: []string{"余额"}},
	{Key: "model", Group: displayGroupFooter, Default: false, Aliases: []string{"模型", "模型名"}},
}

func findDisplayItem(key string) *displayItem {
	for i := range displayItems {
		if displayItems[i].Key == key {
			return &displayItems[i]
		}
	}
	return nil
}

func displayKeys() []string {
	keys := make([]string, 0, len(displayItems))
	for _, it := range displayItems {
		keys = append(keys, it.Key)
	}
	return keys
}

// resolveDisplayKey maps a key or alias (any case) to the item key.
func resolveDisplayKey(word string) string {
	word = strings.ToLower(strings.TrimSpace(word))
	for _, it := range displayItems {
		if word == it.Key {
			return it.Key
		}
		for _, a := range it.Aliases {
			if word == strings.ToLower(a) {
				return it.Key
			}
		}
	}
	return ""
}

// visibleDisplayItems lists the switches a user may change, optionally by group.
func visibleDisplayItems(uid int64, group string) []displayItem {
	admin := isAdmin(uid)
	var out []displayItem
	for _, it := range displayItems {
		if (group == "" || it.Group == group) && (admin || !it.AdminOnly) {
			out = append(out, it)
		}
	}
	return out
}

// display is the resolved set of switches for one task.
type display map[string]bool

func displayFor(uid int64, c Config) display {
	p := getPrefs(uid)
	admin := isAdmin(uid)
	d := display{}
	for _, it := range displayItems {
		v := it.Default
		if cv, ok := c.Display[it.Key]; ok {
			v = cv
		}
		if uv, ok := p.Show[it.Key]; ok {
			v = uv
		}
		if it.AdminOnly && !admin {
			v = false
		}
		d[it.Key] = v
	}
	// Output previews keep the legacy /steps preference and show_steps default.
	d["output"] = stepsDetailed(uid, c)
	return d
}

// setDisplay stores explicit choices for the given switches.
func setDisplay(uid int64, changes map[string]bool) error {
	return updatePrefs(uid, func(p *UserPrefs) {
		for key, on := range changes {
			if key == "output" {
				p.Steps = map[bool]string{true: "on", false: "off"}[on]
				continue
			}
			if p.Show == nil {
				p.Show = map[string]bool{}
			}
			p.Show[key] = on
		}
	})
}

// resetDisplay drops every display choice of the user.
func resetDisplay(uid int64) error {
	return updatePrefs(uid, func(p *UserPrefs) { p.Show, p.Steps = nil, "" })
}

// Presets: full shows everything, minimal keeps time and tokens, pure leaves
// only the answer (no statistics, panel removed once the answer is sent).
var displayPresetAliases = map[string]string{
	"full": "full", "all": "full", "全部": "full", "全部显示": "full", "全开": "full",
	"minimal": "minimal", "simple": "minimal", "简洁": "minimal", "精简": "minimal",
	"pure": "pure", "clean": "pure", "none": "pure", "纯净": "pure", "全部隐藏": "pure", "全关": "pure",
	"reset": "reset", "default": "reset", "默认": "reset", "恢复默认": "reset",
}

func applyDisplayPreset(uid int64, preset string) error {
	if preset == "reset" {
		return resetDisplay(uid)
	}
	changes := map[string]bool{}
	for _, it := range visibleDisplayItems(uid, "") {
		switch preset {
		case "full":
			changes[it.Key] = true
		case "pure":
			changes[it.Key] = false
		case "minimal":
			changes[it.Key] = it.Key == "think" || it.Key == "trace" || it.Key == "keep" || it.Key == "time" || it.Key == "tokens"
		}
	}
	return setDisplay(uid, changes)
}

func displayOnWord(w string) (on, ok bool) {
	switch w {
	case "on", "1", "true", "yes", "show", "开", "开启", "打开", "显示":
		return true, true
	case "off", "0", "false", "no", "hide", "关", "关闭", "隐藏":
		return false, true
	}
	return false, false
}

// handleShowCommand: /show opens the panel; /show tokens off, /show 余额 关,
// /show time on tokens off, /show tokens (toggle), /show 简洁 | pure | reset.
func handleShowCommand(chatID, uid int64, rest string) {
	lang := langFor(uid)
	words := strings.Fields(strings.ToLower(strings.NewReplacer("=", " ", ":", " ", "：", " ", ",", " ", "，", " ").Replace(rest)))
	if len(words) == 0 {
		sendUI(chatID, displayPanelText(uid), displayKeyboard(uid))
		return
	}
	if len(words) == 1 {
		if preset, ok := displayPresetAliases[words[0]]; ok {
			if err := applyDisplayPreset(uid, preset); err != nil {
				sendMessage(chatID, tr(lang, "err.save", err.Error()), nil)
				return
			}
			sendUI(chatID, "<b>"+esc(tr(lang, "show.preset."+preset))+"</b>\n\n"+displayPanelText(uid), displayKeyboard(uid))
			return
		}
	}
	current := displayFor(uid, configSnapshot())
	changes := map[string]bool{}
	var pending []string
	for _, w := range words {
		if on, ok := displayOnWord(w); ok {
			if len(pending) == 0 {
				sendMessage(chatID, tr(lang, "show.usage"), nil)
				return
			}
			for _, key := range pending {
				changes[key] = on
			}
			pending = nil
			continue
		}
		if w == "all" || w == "全部" {
			for _, it := range visibleDisplayItems(uid, "") {
				pending = append(pending, it.Key)
			}
			continue
		}
		key := resolveDisplayKey(w)
		if key == "" {
			sendMessage(chatID, tr(lang, "show.unknown", w)+"\n"+tr(lang, "show.usage"), nil)
			return
		}
		if findDisplayItem(key).AdminOnly && !isAdmin(uid) {
			sendMessage(chatID, tr(lang, "err.admin_only"), nil)
			return
		}
		pending = append(pending, key)
	}
	for _, key := range pending { // no on/off given: toggle
		changes[key] = !current[key]
	}
	if err := setDisplay(uid, changes); err != nil {
		sendMessage(chatID, tr(lang, "err.save", err.Error()), nil)
		return
	}
	sendUI(chatID, "<b>"+esc(tr(lang, "show.updated"))+"</b>\n\n"+displayPanelText(uid), displayKeyboard(uid))
}

// handleDisplayCallback serves the display panel buttons. It returns the toast.
func handleDisplayCallback(chatID, msgID, uid int64, data string) string {
	lang := langFor(uid)
	toast := ""
	switch {
	case data == "nav:display":
	case strings.HasPrefix(data, "show:t:"):
		key := strings.TrimPrefix(data, "show:t:")
		it := findDisplayItem(key)
		if it == nil || (it.AdminOnly && !isAdmin(uid)) {
			return ""
		}
		on := !displayFor(uid, configSnapshot())[key]
		if err := setDisplay(uid, map[string]bool{key: on}); err != nil {
			return tr(lang, "err.save", err.Error())
		}
		state := tr(lang, "show.off")
		if on {
			state = tr(lang, "show.on")
		}
		toast = tr(lang, "show.item."+key) + " · " + state
	case strings.HasPrefix(data, "show:p:"):
		preset := strings.TrimPrefix(data, "show:p:")
		if _, ok := map[string]bool{"full": true, "minimal": true, "pure": true, "reset": true}[preset]; !ok {
			return ""
		}
		if err := applyDisplayPreset(uid, preset); err != nil {
			return tr(lang, "err.save", err.Error())
		}
		toast = tr(lang, "show.preset."+preset)
	default:
		return ""
	}
	editUI(chatID, msgID, displayPanelText(uid), displayKeyboard(uid))
	return toast
}

// displayPanelText explains the switches and previews the statistics line.
func displayPanelText(uid int64) string {
	lang := langFor(uid)
	c := configSnapshot()
	d := displayFor(uid, c)
	var b strings.Builder
	b.WriteString("<b>▍" + esc(tr(lang, "show.title")) + "</b>\n")
	b.WriteString("<i>" + esc(tr(lang, "show.hint")) + "</i>\n\n")
	for _, group := range []string{displayGroupPanel, displayGroupFooter} {
		b.WriteString("<b>" + esc(tr(lang, "show.group."+group)) + "</b>\n")
		for _, it := range visibleDisplayItems(uid, group) {
			fmt.Fprintf(&b, "%s %s <i>%s</i>\n", switchMark(d[it.Key]), esc(tr(lang, "show.item."+it.Key)), esc(tr(lang, "show.desc."+it.Key)))
		}
		b.WriteString("\n")
	}
	sample := footerData{
		Elapsed: 83 * time.Second,
		Stats:   taskStats{Calls: 6, Tools: 8, Commands: 3, Prompt: 10100, Completion: 2200},
		Balance: "¥23.45",
		Model:   c.Model,
	}
	b.WriteString("<b>" + esc(tr(lang, "show.preview")) + "</b>\n")
	if footer := renderFooter(lang, d, sample); footer != "" {
		b.WriteString(footer + "\n\n")
	} else {
		b.WriteString("<i>" + esc(tr(lang, "show.preview_none")) + "</i>\n\n")
	}
	b.WriteString("<i>" + esc(tr(lang, "show.tip")) + "</i>")
	return b.String()
}

func switchMark(on bool) string {
	if on {
		return "●"
	}
	return "○"
}

func displayKeyboard(uid int64) map[string]interface{} {
	lang := langFor(uid)
	d := displayFor(uid, configSnapshot())
	var rows [][]Btn
	for _, group := range []string{displayGroupPanel, displayGroupFooter} {
		perRow := 2
		if group == displayGroupFooter {
			perRow = 3
		}
		var row []Btn
		for _, it := range visibleDisplayItems(uid, group) {
			row = append(row, btn(switchMark(d[it.Key])+" "+tr(lang, "show.item."+it.Key), "show:t:"+it.Key))
			if len(row) == perRow {
				rows = append(rows, row)
				row = nil
			}
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	rows = append(rows,
		[]Btn{btn(tr(lang, "show.btn.full"), "show:p:full"), btn(tr(lang, "show.btn.minimal"), "show:p:minimal"), btn(tr(lang, "show.btn.pure"), "show:p:pure")},
		[]Btn{btn(tr(lang, "show.btn.reset"), "show:p:reset"), btn(tr(lang, "btn.back"), "nav:root")},
	)
	return keyboard(rows)
}

// ---------------- statistics line ----------------

type footerData struct {
	Elapsed time.Duration
	Stats   taskStats
	Balance string
	Model   string
}

// renderFooter builds the italic statistics line shown under an answer.
// Zero counters are omitted; an empty string means nothing to show.
func renderFooter(lang string, d display, f footerData) string {
	var parts []string
	add := func(key, value string) {
		parts = append(parts, esc(tr(lang, "footer."+key))+" <b>"+esc(value)+"</b>")
	}
	if d["time"] && f.Elapsed > 0 {
		add("time", humanDuration(f.Elapsed))
	}
	if d["calls"] && f.Stats.Calls > 0 {
		add("calls", fmt.Sprint(f.Stats.Calls))
	}
	if d["tools"] && f.Stats.Tools > 0 {
		add("tools", fmt.Sprint(f.Stats.Tools))
	}
	if d["cmds"] && f.Stats.Commands > 0 {
		add("cmds", fmt.Sprint(f.Stats.Commands))
	}
	if d["tokens"] && f.Stats.Tokens() > 0 {
		add("tokens", humanTokens(f.Stats.Tokens()))
	}
	if d["balance"] && f.Balance != "" {
		add("balance", f.Balance)
	}
	if d["model"] && f.Model != "" {
		parts = append(parts, "<code>"+esc(f.Model)+"</code>")
	}
	if len(parts) == 0 {
		return ""
	}
	return "<i>" + strings.Join(parts, " · ") + "</i>"
}

// ---------------- balance for the statistics line ----------------

// Providers without a balance endpoint are not asked again for a while, so
// the statistics line never slows replies down.
var balanceFailures = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

func footerBalance(ctx context.Context, c Config) string {
	p := providerFor(c)
	if p == nil {
		return ""
	}
	key := p.BaseURL
	balanceFailures.Lock()
	until, failed := balanceFailures.until[key]
	balanceFailures.Unlock()
	if failed && time.Now().Before(until) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := providerBalance(ctx, c)
	if err != nil {
		balanceFailures.Lock()
		balanceFailures.until[key] = time.Now().Add(10 * time.Minute)
		balanceFailures.Unlock()
		return ""
	}
	return balanceShort(r)
}

// balanceShort renders balances compactly: ¥23.45, $1.20 or "12.00 EUR".
func balanceShort(r *balanceResponse) string {
	var parts []string
	for _, b := range r.BalanceInfos {
		value := strings.TrimSpace(b.TotalBalance)
		if value == "" {
			continue
		}
		switch strings.ToUpper(b.Currency) {
		case "CNY", "RMB":
			parts = append(parts, "¥"+value)
		case "USD":
			parts = append(parts, "$"+value)
		case "":
			parts = append(parts, value)
		default:
			parts = append(parts, value+" "+b.Currency)
		}
	}
	return strings.Join(parts, " / ")
}
