package main

import (
	"strings"
	"testing"
)

func TestMarkdownToHTMLCoversCommonBlocks(t *testing.T) {
	md := "# 标题 Title\n\n**粗体** and *italic* with `a<b>` and [link](https://example.com/?a=1&b=2)\n\n- item one\n- [x] done\n1. first\n\n> quoted **text**\n\n```go\nfmt.Println(\"<hi>\")\n```\n\n| 名称 | value |\n| --- | --- |\n| 中文 | 1 |"
	got := markdownToHTML(md)
	for _, want := range []string{
		"<b>标题 Title</b>",
		"<b>粗体</b>",
		"<i>italic</i>",
		"<code>a&lt;b&gt;</code>",
		`<a href="https://example.com/?a=1&amp;b=2">link</a>`,
		"• item one",
		"[x] done",
		"1. first",
		"<blockquote>quoted <b>text</b></blockquote>",
		`<pre><code class="language-go">fmt.Println("&lt;hi&gt;")</code></pre>`,
		"名称 │ value",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !balancedHTML(got) {
		t.Fatal("rendered HTML is not balanced")
	}
}

func TestMarkdownInlineFallsBackWhenUnbalanced(t *testing.T) {
	got := renderInline("**a *b** c* <tag>")
	if !balancedHTML(got) {
		t.Fatalf("unbalanced inline output: %s", got)
	}
	if !strings.Contains(got, "&lt;tag&gt;") {
		t.Fatalf("raw HTML was not escaped: %s", got)
	}
	if got := renderInline("snake_case_name and 2*3*4"); strings.Contains(got, "<i>") {
		t.Fatalf("identifiers or arithmetic became italic: %s", got)
	}
}

func TestMarkdownChunksRespectLimitAndKeepContent(t *testing.T) {
	var b strings.Builder
	b.WriteString("Intro paragraph\n\n```\n")
	for i := 0; i < 400; i++ {
		b.WriteString("line <" + strings.Repeat("x", 20) + ">\n")
	}
	b.WriteString("```\n\nOutro")
	chunks := markdownChunks(b.String(), 1000)
	if len(chunks) < 3 {
		t.Fatalf("expected the long code block to be split, got %d chunks", len(chunks))
	}
	lines := 0
	for _, c := range chunks {
		if c.HTML != "" && utf16Len(c.HTML) > 1000 {
			t.Fatalf("chunk exceeds limit: %d", utf16Len(c.HTML))
		}
		if c.HTML != "" && !balancedHTML(c.HTML) {
			t.Fatalf("chunk is not balanced: %s", c.HTML)
		}
		lines += strings.Count(c.Plain, "line <")
	}
	if lines != 400 {
		t.Fatalf("lost code lines: %d", lines)
	}
	if !strings.Contains(chunks[0].HTML, "Intro") || !strings.Contains(chunks[len(chunks)-1].HTML, "Outro") {
		t.Fatal("surrounding paragraphs lost")
	}
}

func TestSplitTelegramTextPrefersLineBreaks(t *testing.T) {
	text := strings.Repeat("a", 60) + "\n" + strings.Repeat("b", 60)
	chunks := splitTelegramText(text, 100)
	if len(chunks) != 2 || chunks[0] != strings.Repeat("a", 60)+"\n" {
		t.Fatalf("chunks=%q", chunks)
	}
}

func TestCutAtParagraphClosesCodeFence(t *testing.T) {
	text := "intro\n\n```\n" + strings.Repeat("code line\n", 200) + "```"
	got := cutAtParagraph(text, 300)
	if strings.Count(got, "```")%2 != 0 {
		t.Fatalf("unterminated fence in preview: %s", got)
	}
}

func TestArgsAfterToleratesRepeatedSpaces(t *testing.T) {
	cases := map[string]string{
		"add  review   检查  改动": "检查  改动",
		"add review":           "",
		"\tadd\treview\tx y":   "x y",
	}
	for in, want := range cases {
		if got := argsAfter(in, 2); got != want {
			t.Errorf("argsAfter(%q)=%q want %q", in, got, want)
		}
	}
}

func TestIntArgClampsInsteadOfResetting(t *testing.T) {
	args := map[string]interface{}{"limit": 5000.0, "offset": -3.0}
	if got := intArg(args, "limit", 200, 1, 2000); got != 2000 {
		t.Fatalf("limit=%d", got)
	}
	if got := intArg(args, "offset", 1, 1, 100); got != 1 {
		t.Fatalf("offset=%d", got)
	}
	if got := intArg(args, "missing", 7, 1, 10); got != 7 {
		t.Fatalf("default=%d", got)
	}
}

func TestLooksLikePath(t *testing.T) {
	for _, s := range []string{"/etc/nginx", "/var/log/syslog", "/tmp/a.txt", "/main.go"} {
		if !looksLikePath(s) {
			t.Errorf("%s should be treated as a path", s)
		}
	}
	for _, s := range []string{"/menu", "/unknown", "/stop"} {
		if looksLikePath(s) {
			t.Errorf("%s should be treated as a command", s)
		}
	}
}

// TestUIHasNoEmoji guards the plain-text style: no pictographic emoji in the
// main user-visible surfaces.
func TestUIHasNoEmoji(t *testing.T) {
	hasEmoji := func(s string) (rune, bool) {
		for _, r := range s {
			if r == 0xFE0F || (r >= 0x1F000 && r <= 0x1FFFF) ||
				r == 0x2705 || r == 0x274C || r == 0x26A0 || r == 0x2699 || r == 0x23F0 ||
				r == 0x23F3 || r == 0x23F9 || r == 0x1F6E1 {
				return r, true
			}
		}
		return 0, false
	}
	for key, v := range messages {
		for _, s := range v {
			if r, ok := hasEmoji(s); ok {
				t.Errorf("message %q contains emoji %q: %s", key, string(r), s)
			}
		}
	}
	c := runtimeTestConfig(t)
	uid := c.OwnerID
	panel := newProgressPanel(langZH, true, "cron #1")
	panel.apply(StepEvent{Tool: "bash", Detail: "ls", Intent: "看看"})
	panel.apply(StepEvent{Tool: "bash", Done: true, Output: "a\nb"})
	for name, s := range map[string]string{
		"menu": menuText(uid), "help": helpBody(uid), "mode": executionSettingsText(langZH),
		"panel": panel.render(), "final": panel.renderFinal("done"), "memory": memoryPanel(uid, langZH),
	} {
		if r, ok := hasEmoji(s); ok {
			t.Errorf("%s surface contains emoji %q:\n%s", name, string(r), s)
		}
	}
}
