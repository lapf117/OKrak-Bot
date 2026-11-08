package main

import (
	"regexp"
	"strings"
	"sync"
)

// Reasoning effort choices follow what the current provider and model accept:
//  1. providers[].efforts in config.json, when set;
//  2. a built-in table keyed by the model name;
//  3. for unknown models, the common OpenAI values.
// A value the model does not list is not sent, and when the API rejects the
// reasoning_effort parameter it is dropped silently and not sent again.

type effortSource int

const (
	effortFromConfig effortSource = iota
	effortFromModel
	effortUnknown
)

var commonEfforts = []string{"minimal", "low", "medium", "high"}

type modelEffortRule struct {
	match   *regexp.Regexp
	efforts []string // nil: the model takes no reasoning_effort
}

// Rules are checked in order against the bare model name ("openai/gpt-5:online" → "gpt-5").
var modelEffortRules = []modelEffortRule{
	{regexp.MustCompile(`^deepseek`), nil}, // thinking is chosen by model: deepseek-reasoner
	{regexp.MustCompile(`codex-max`), []string{"low", "medium", "high", "xhigh"}},
	{regexp.MustCompile(`^gpt-5\.([2-9]|\d{2,})`), []string{"low", "medium", "high", "xhigh"}},
	{regexp.MustCompile(`^gpt-5\.1`), []string{"low", "medium", "high"}},
	{regexp.MustCompile(`^gpt-5`), []string{"minimal", "low", "medium", "high"}},
	{regexp.MustCompile(`^o[1-9](-|$)`), []string{"low", "medium", "high"}},
	{regexp.MustCompile(`^grok-3-mini`), []string{"low", "high"}},
	{regexp.MustCompile(`^gemini-(2\.5|[3-9])`), []string{"low", "medium", "high"}},
	{regexp.MustCompile(`^(gpt-4|gpt-3\.5|chatgpt-|qwen|qwq|glm|kimi|moonshot|yi-|ernie|hunyuan|llama|mistral|grok-[2-4]|gemini-(1|2\.0))`), nil},
}

// modelBase strips a gateway prefix and variant suffix from a model id.
func modelBase(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.Index(m, ":"); i >= 0 {
		m = m[:i]
	}
	return m
}

// effortOptions lists the reasoning_effort values to offer (without "off").
func effortOptions(c Config) ([]string, effortSource) {
	if p := providerFor(c); p != nil && len(p.Efforts) > 0 {
		var out []string
		for _, e := range p.Efforts {
			e = strings.ToLower(strings.TrimSpace(e))
			if e != "" && e != "off" && validEffort(e) && !containsString(out, e) {
				out = append(out, e)
			}
		}
		return out, effortFromConfig
	}
	m := modelBase(c.Model)
	for _, r := range modelEffortRules {
		if r.match.MatchString(m) {
			return r.efforts, effortFromModel
		}
	}
	return commonEfforts, effortUnknown
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// effortRejected remembers provider/model pairs whose API refused reasoning_effort.
var effortRejected sync.Map

func effortKey(c Config) string {
	if p := providerFor(c); p != nil {
		return p.Name + "|" + p.BaseURL + "|" + c.Model
	}
	return "|" + c.Model
}

func effortWasRejected(c Config) bool {
	_, ok := effortRejected.Load(effortKey(c))
	return ok
}

// effectiveEffort is the value actually sent: empty when it is off, when the
// model does not list it, or when the provider has rejected the parameter.
func effectiveEffort(c Config) string {
	e := c.ReasoningEffort
	if e == "" || effortWasRejected(c) {
		return ""
	}
	opts, src := effortOptions(c)
	if src == effortUnknown || containsString(opts, e) {
		return e
	}
	return ""
}

// rejectsEffort recognises an API error about the reasoning_effort parameter.
func rejectsEffort(status int, body string) bool {
	if status != 400 && status != 422 {
		return false
	}
	low := strings.ToLower(body)
	if !strings.Contains(low, "reasoning") {
		return false
	}
	for _, marker := range []string{"reasoning_effort", "unsupported", "not support", "unrecognized", "unknown", "invalid", "extra", "not allowed", "not permitted"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// effortPanel is the text and keyboard of the reasoning effort menu.
func effortPanel(lang string, c Config) (string, map[string]interface{}) {
	opts, src := effortOptions(c)
	cur := c.ReasoningEffort
	if cur == "" {
		cur = "off"
	}
	var b strings.Builder
	b.WriteString("<b>▍" + esc(tr(lang, "effort.title")) + "</b>\n")
	b.WriteString(esc(tr(lang, "menu.model")) + " <code>" + esc(c.Model) + "</code>\n\n")
	switch {
	case src == effortFromConfig:
		b.WriteString(esc(tr(lang, "effort.src_config")))
	case src == effortFromModel && len(opts) == 0:
		b.WriteString(esc(tr(lang, "effort.src_none")))
		if strings.HasPrefix(modelBase(c.Model), "deepseek") {
			b.WriteString("\n" + esc(tr(lang, "effort.deepseek")))
		}
	case src == effortFromModel:
		b.WriteString(esc(tr(lang, "effort.src_model")))
	default:
		b.WriteString(esc(tr(lang, "effort.src_unknown")))
	}
	if effortWasRejected(c) {
		b.WriteString("\n\n<i>" + esc(tr(lang, "effort.rejected")) + "</i>")
	} else if c.ReasoningEffort != "" && effectiveEffort(c) == "" {
		b.WriteString("\n\n<i>" + esc(tr(lang, "effort.inactive", c.ReasoningEffort)) + "</i>")
	}
	rows := [][]Btn{{btn(mark(cur, "off"), "set:effort:off")}}
	var row []Btn
	for _, lv := range opts {
		row = append(row, btn(mark(cur, lv), "set:effort:"+lv))
		if len(row) == 3 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []Btn{btn(tr(lang, "btn.back"), "nav:settings")})
	return b.String(), keyboard(rows)
}

// effortUsage lists the accepted values for /effort.
func effortUsage(lang string, c Config) string {
	opts, _ := effortOptions(c)
	return tr(lang, "effort.usage", strings.Join(append([]string{"off"}, opts...), " | "))
}
