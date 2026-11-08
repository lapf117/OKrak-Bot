package main

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Telegram accepts 4096 UTF-16 units per message. Chunks stay below that with
// room for small footers; HTML length is measured conservatively (tags count).
const telegramChunk = 3900

// richChunk is one Telegram message. When HTML is empty the chunk is sent as
// plain text. Plain is also the fallback if Telegram rejects the markup.
type richChunk struct {
	HTML  string
	Plain string
}

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
var attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

func esc(s string) string { return htmlEscaper.Replace(s) }

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xffff {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// splitTelegramText splits plain text by UTF-16 units, preferring line breaks
// so that messages do not cut words or code lines in half.
func splitTelegramText(text string, limit int) []string {
	if limit < 2 {
		limit = 2
	}
	if text == "" {
		return []string{""}
	}
	var chunks []string
	for utf16Len(text) > limit {
		cut, units, lastBreak := len(text), 0, -1
		for index, r := range text {
			width := 1
			if r > 0xffff {
				width = 2
			}
			if units+width > limit {
				cut = index
				break
			}
			units += width
			if r == '\n' {
				lastBreak = index + 1
			}
		}
		// Only break on a newline when it keeps the chunk reasonably full.
		if lastBreak > 0 && utf16Len(text[:lastBreak]) >= limit/2 {
			cut = lastBreak
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return append(chunks, text)
}

func truncateTelegramText(text string, limit int) string { return splitTelegramText(text, limit)[0] }

// ---------------- Markdown → Telegram HTML ----------------

var (
	headingRe   = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$`)
	hrRe        = regexp.MustCompile(`^\s{0,3}([-*_])(\s*[-*_]){2,}\s*$`)
	bulletRe    = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	orderedRe   = regexp.MustCompile(`^(\s*)(\d{1,4})[.)]\s+(.*)$`)
	tableSepRe  = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	codeSpanRe  = regexp.MustCompile("`([^`\n]+)`")
	linkRe      = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
	boldRe      = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	boldUnderRe = regexp.MustCompile(`(^|[^\w])__([^_\n]+?)__($|[^\w])`)
	strikeRe    = regexp.MustCompile(`~~([^~\n]+?)~~`)
	italicRe    = regexp.MustCompile(`(^|[^\w*])\*([^*\s][^*\n]*?)\*($|[^\w*])`)
	tagRe       = regexp.MustCompile(`</?([a-z]+)(?:\s[^>]*)?>`)
	codeLangRe  = regexp.MustCompile(`^[A-Za-z0-9_+#.-]{1,24}$`)
)

// renderInline converts inline Markdown on a single line. The result is
// verified to be balanced; otherwise the escaped source is returned.
func renderInline(text string) string {
	var spans []string
	protected := codeSpanRe.ReplaceAllStringFunc(text, func(m string) string {
		spans = append(spans, "<code>"+esc(m[1:len(m)-1])+"</code>")
		return "\x00" + strconv.Itoa(len(spans)-1) + "\x00"
	})
	out := esc(protected)
	out = linkRe.ReplaceAllStringFunc(out, func(m string) string {
		sub := linkRe.FindStringSubmatch(m)
		href := strings.ReplaceAll(sub[2], `"`, "&quot;")
		return `<a href="` + href + `">` + sub[1] + "</a>"
	})
	out = boldRe.ReplaceAllString(out, "<b>$1</b>")
	out = boldUnderRe.ReplaceAllString(out, "$1<b>$2</b>$3")
	out = strikeRe.ReplaceAllString(out, "<s>$1</s>")
	// Applied twice: adjacent matches share their boundary character.
	out = italicRe.ReplaceAllString(out, "$1<i>$2</i>$3")
	out = italicRe.ReplaceAllString(out, "$1<i>$2</i>$3")
	for i, span := range spans {
		out = strings.Replace(out, "\x00"+strconv.Itoa(i)+"\x00", span, 1)
	}
	if !balancedHTML(out) || strings.ContainsRune(out, 0) {
		return esc(strings.ReplaceAll(text, "\x00", ""))
	}
	return out
}

// balancedHTML checks that the tags we generate nest properly.
func balancedHTML(s string) bool {
	var stack []string
	for _, m := range tagRe.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if strings.HasPrefix(m[0], "</") {
			if len(stack) == 0 || stack[len(stack)-1] != name {
				return false
			}
			stack = stack[:len(stack)-1]
		} else {
			stack = append(stack, name)
		}
	}
	return len(stack) == 0
}

// bulletMarks are the list glyphs per nesting level.
var bulletMarks = []string{"•", "◦", "‣"}

func renderListLine(line string) (string, bool) {
	if m := bulletRe.FindStringSubmatch(line); m != nil {
		level := len(strings.ReplaceAll(m[1], "\t", "  ")) / 2
		indent := strings.Repeat("   ", level)
		body := m[2]
		markText := bulletMarks[min(level, len(bulletMarks)-1)] + " "
		switch {
		case strings.HasPrefix(body, "[ ] "):
			markText, body = "○ ", body[4:]
		case strings.HasPrefix(body, "[x] "), strings.HasPrefix(body, "[X] "):
			markText, body = "✓ ", body[4:]
		}
		return indent + markText + renderInline(body), true
	}
	if m := orderedRe.FindStringSubmatch(line); m != nil {
		indent := strings.Repeat("   ", len(strings.ReplaceAll(m[1], "\t", "  "))/2)
		return indent + "<b>" + m[2] + ".</b> " + renderInline(m[3]), true
	}
	return "", false
}

// headingHTML gives headings a section bar: level 1–2 "▍Title", deeper "▸ Title".
func headingHTML(level int, text string) string {
	if level <= 2 {
		return "<b>▍" + renderInline(text) + "</b>"
	}
	return "<b>▸ " + renderInline(text) + "</b>"
}

// markdownChunks renders Markdown into Telegram-sized HTML messages. Blocks
// are never split across messages unless a single block is too large.
func markdownChunks(md string, limit int) []richChunk {
	if limit <= 0 {
		limit = telegramChunk
	}
	blocks := markdownBlocks(md, limit)
	var out []richChunk
	var cur richChunk
	flush := func() {
		if cur.HTML != "" || cur.Plain != "" {
			out = append(out, cur)
		}
		cur = richChunk{}
	}
	for _, b := range blocks {
		if b.HTML == "" {
			// Oversized plain fragment: its own message without markup.
			flush()
			out = append(out, b)
			continue
		}
		if cur.HTML == "" {
			cur = b
			continue
		}
		joined := cur.HTML + "\n\n" + b.HTML
		if utf16Len(joined) > limit {
			flush()
			cur = b
			continue
		}
		cur.HTML = joined
		cur.Plain += "\n\n" + b.Plain
	}
	flush()
	if len(out) == 0 {
		out = []richChunk{{HTML: "…", Plain: "…"}}
	}
	return out
}

// markdownToHTML renders the whole text without size limits (used in tests and short UI).
func markdownToHTML(md string) string {
	var parts []string
	for _, b := range markdownBlocks(md, 1<<30) {
		parts = append(parts, b.HTML)
	}
	return strings.Join(parts, "\n\n")
}

func markdownBlocks(md string, limit int) []richChunk {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var blocks []richChunk
	var paraHTML, paraPlain []string
	flushPara := func() {
		if len(paraHTML) > 0 {
			blocks = append(blocks, sizedBlock(strings.Join(paraHTML, "\n"), strings.Join(paraPlain, "\n"), limit)...)
		}
		paraHTML, paraPlain = nil, nil
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			flushPara()
			fence := trimmed[:3]
			lang := strings.TrimSpace(strings.TrimLeft(trimmed, fence[:1]))
			var body []string
			for i++; i < len(lines); i++ {
				if strings.HasPrefix(strings.TrimSpace(lines[i]), fence) {
					break
				}
				body = append(body, lines[i])
			}
			blocks = append(blocks, codeBlocks(lang, body, limit)...)
		case trimmed == "":
			flushPara()
		case strings.HasPrefix(trimmed, "|") && i+1 < len(lines) && tableSepRe.MatchString(lines[i+1]):
			flushPara()
			rows := [][]string{splitTableRow(trimmed)}
			for i += 2; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				rows = append(rows, splitTableRow(strings.TrimSpace(lines[i])))
			}
			i--
			blocks = append(blocks, codeBlocks("", strings.Split(renderTable(rows), "\n"), limit)...)
		case headingRe.MatchString(line):
			flushPara()
			m := headingRe.FindStringSubmatch(line)
			blocks = append(blocks, sizedBlock(headingHTML(len(m[1]), m[2]), line, limit)...)
		case hrRe.MatchString(line):
			flushPara()
			blocks = append(blocks, richChunk{HTML: "┈┈┈┈┈┈┈┈┈┈┈┈", Plain: "┈┈┈┈┈┈┈┈┈┈┈┈"})
		case strings.HasPrefix(trimmed, ">"):
			flushPara()
			var quote, raw []string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">"); i++ {
				text := strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")
				quote = append(quote, renderInline(strings.TrimSpace(text)))
				raw = append(raw, lines[i])
			}
			i--
			// Long quotes start collapsed so they do not dominate the answer.
			open := "<blockquote>"
			if len(quote) > 6 {
				open = "<blockquote expandable>"
			}
			blocks = append(blocks, sizedBlock(open+strings.Join(quote, "\n")+"</blockquote>", strings.Join(raw, "\n"), limit)...)
		default:
			rendered, ok := renderListLine(line)
			if !ok {
				rendered = renderInline(strings.TrimRight(line, " \t"))
			}
			paraHTML = append(paraHTML, rendered)
			paraPlain = append(paraPlain, line)
		}
	}
	flushPara()
	return blocks
}

// sizedBlock keeps one HTML block when it fits; otherwise falls back to
// plain-text pieces so content is never dropped.
func sizedBlock(htmlText, plain string, limit int) []richChunk {
	if utf16Len(htmlText) <= limit {
		return []richChunk{{HTML: htmlText, Plain: plain}}
	}
	var out []richChunk
	for _, piece := range splitTelegramText(plain, limit) {
		out = append(out, richChunk{Plain: piece})
	}
	return out
}

func codeBlocks(lang string, lines []string, limit int) []richChunk {
	open, closeTag := "<pre>", "</pre>"
	if codeLangRe.MatchString(lang) {
		open, closeTag = `<pre><code class="language-`+attrEscaper.Replace(lang)+`">`, "</code></pre>"
	}
	budget := limit - utf16Len(open+closeTag) - 8
	if budget < 64 {
		budget = 64
	}
	var out []richChunk
	var body, plain []string
	size := 0
	flush := func() {
		if len(body) == 0 {
			return
		}
		out = append(out, richChunk{HTML: open + strings.Join(body, "\n") + closeTag, Plain: strings.Join(plain, "\n")})
		body, plain, size = nil, nil, 0
	}
	for _, line := range lines {
		for _, piece := range splitEscapedLine(line, budget) {
			escaped := esc(piece)
			n := utf16Len(escaped) + 1
			if size+n > budget {
				flush()
			}
			body = append(body, escaped)
			plain = append(plain, piece)
			size += n
		}
	}
	flush()
	if len(out) == 0 {
		out = append(out, richChunk{HTML: open + closeTag, Plain: ""})
	}
	return out
}

// splitEscapedLine splits a raw line so that each escaped piece fits budget.
func splitEscapedLine(line string, budget int) []string {
	if utf16Len(esc(line)) <= budget {
		return []string{line}
	}
	var out []string
	var b strings.Builder
	size := 0
	for _, r := range line {
		n := utf16Len(esc(string(r)))
		if size+n > budget {
			out = append(out, b.String())
			b.Reset()
			size = 0
		}
		b.WriteRune(r)
		size += n
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func splitTableRow(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	cells := strings.Split(row, "|")
	for i := range cells {
		cell := strings.TrimSpace(cells[i])
		cell = strings.NewReplacer("**", "", "__", "", "`", "").Replace(cell)
		cells[i] = cell
	}
	return cells
}

// displayWidth approximates terminal cell width: CJK and emoji count twice.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r == 0 || unicode.Is(unicode.Mn, r):
		case r >= 0x1100 && (r <= 0x115f || (r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe30 && r <= 0xfe4f) || (r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6) || r >= 0x1f300):
			w += 2
		default:
			w++
		}
	}
	return w
}

func renderTable(rows [][]string) string {
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	widths := make([]int, cols)
	for _, r := range rows {
		for i, cell := range r {
			if w := displayWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}
	var b strings.Builder
	for ri, r := range rows {
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			b.WriteString(cell)
			if i < cols-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)))
				b.WriteString(" │ ")
			}
		}
		b.WriteString("\n")
		if ri == 0 && len(rows) > 1 {
			for i := 0; i < cols; i++ {
				b.WriteString(strings.Repeat("─", widths[i]))
				if i < cols-1 {
					b.WriteString("─┼─")
				}
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// stripTags converts our generated HTML back into readable plain text.
func stripTags(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&amp;", "&").Replace(s)
}

// previewText trims tool output for the live panel: first lines, bounded runes.
func previewText(s string, maxLines, maxRunes int) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "�"))
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], "…")
	}
	s = strings.Join(lines, "\n")
	if utf8.RuneCountInString(s) > maxRunes {
		s = short(s, maxRunes)
	}
	return s
}
