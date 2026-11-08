package main

import "strings"

// The bot never shows emoji. Model text is cleaned before display: emoji that
// carry meaning become plain symbols (✅ → ✓, ➡️ → →), the rest is removed.
// Code blocks and inline code are left untouched.

// emojiSymbols maps meaningful emoji to text symbols.
var emojiSymbols = map[rune]string{
	0x2705: "✓", 0x2714: "✓", 0x2611: "✓", 0x1F197: "OK",
	0x274C: "✗", 0x274E: "✗", 0x2716: "✗", 0x1F6AB: "✗",
	0x2B50: "★", 0x1F31F: "★",
	0x2757: "!", 0x2755: "!", 0x203C: "!!", 0x2753: "?", 0x2754: "?", 0x2049: "!?",
	0x2795: "+", 0x2796: "-", 0x2797: "÷",
	0x27A1: "→", 0x2B05: "←", 0x2B06: "↑", 0x2B07: "↓",
	0x1F449: "→", 0x1F448: "←", 0x1F446: "↑", 0x1F447: "↓",
	0x25B6: "▸", 0x25C0: "◂", 0x23E9: "»", 0x23EA: "«",
	0x25AA: "•", 0x25AB: "◦", 0x25FB: "□", 0x25FC: "■", 0x25FD: "□", 0x25FE: "■",
	0x2B1B: "■", 0x2B1C: "□", 0x26AA: "○", 0x26AB: "●",
	0x1F534: "●", 0x1F535: "●", 0x1F7E0: "●", 0x1F7E1: "●", 0x1F7E2: "●", 0x1F7E3: "●", 0x1F7E4: "●",
	0x1F7E5: "■", 0x1F7E6: "■", 0x1F7E7: "■", 0x1F7E8: "■", 0x1F7E9: "■", 0x1F7EA: "■", 0x1F7EB: "■",
	0x1F536: "◆", 0x1F537: "◆", 0x1F538: "◆", 0x1F539: "◆",
	0x1F51F: "10",
	// Text symbols that only turn into emoji with U+FE0F: keep the symbol.
	0x00A9: "©", 0x00AE: "®", 0x2122: "™", 0x2194: "↔", 0x2195: "↕", 0x2196: "↖", 0x2197: "↗",
	0x2198: "↘", 0x2199: "↙", 0x21A9: "↩", 0x21AA: "↪", 0x2605: "★", 0x2606: "☆",
}

// emojiRanges are code points with emoji presentation (or pictographs that
// clients draw as emoji). Plain symbols such as ✓ ✗ ★ → • ● ◆ are not listed.
var emojiRanges = [][2]rune{ // sorted
	{0x2139, 0x2139}, {0x231A, 0x231B}, {0x2328, 0x2328}, {0x23CF, 0x23CF}, {0x23E9, 0x23F3},
	{0x23F8, 0x23FA}, {0x24C2, 0x24C2},
	{0x2600, 0x2604}, {0x260E, 0x260E}, {0x2614, 0x2615}, {0x2618, 0x2618}, {0x261D, 0x261D},
	{0x2620, 0x2620}, {0x2622, 0x2623}, {0x2626, 0x2626}, {0x262A, 0x262A}, {0x262E, 0x262F},
	{0x2638, 0x263A}, {0x2640, 0x2640}, {0x2642, 0x2642}, {0x2648, 0x2653}, {0x265F, 0x265F},
	{0x2668, 0x2668}, {0x267B, 0x267B}, {0x267E, 0x267F}, {0x2692, 0x2697}, {0x2699, 0x2699},
	{0x269B, 0x269C}, {0x26A0, 0x26A1}, {0x26A7, 0x26A7}, {0x26B0, 0x26B1}, {0x26BD, 0x26BE},
	{0x26C4, 0x26C5}, {0x26C8, 0x26C8}, {0x26CE, 0x26CF}, {0x26D1, 0x26D1}, {0x26D3, 0x26D4},
	{0x26E9, 0x26EA}, {0x26F0, 0x26F5}, {0x26F7, 0x26FA}, {0x26FD, 0x26FD},
	{0x2702, 0x2702}, {0x2708, 0x270D}, {0x270F, 0x270F}, {0x2712, 0x2712}, {0x271D, 0x271D},
	{0x2721, 0x2721}, {0x2728, 0x2728}, {0x2733, 0x2734}, {0x2744, 0x2744}, {0x2747, 0x2747},
	{0x2763, 0x2764}, {0x27B0, 0x27B0}, {0x27BF, 0x27BF}, {0x2934, 0x2935}, {0x2B55, 0x2B55},
	{0x3030, 0x3030}, {0x303D, 0x303D}, {0x3297, 0x3297}, {0x3299, 0x3299},
	{0x1F000, 0x1F0FF}, {0x1F170, 0x1F171}, {0x1F17E, 0x1F17F}, {0x1F18E, 0x1F18E},
	{0x1F191, 0x1F19A}, {0x1F1E6, 0x1F1FF}, {0x1F201, 0x1F202}, {0x1F21A, 0x1F21A},
	{0x1F22F, 0x1F22F}, {0x1F232, 0x1F23A}, {0x1F250, 0x1F251},
	{0x1F300, 0x1F64F}, {0x1F680, 0x1F6FF}, {0x1F7E0, 0x1F7EB}, {0x1F7F0, 0x1F7F0},
	{0x1F90C, 0x1F9FF}, {0x1FA70, 0x1FAFF},
	{0xE0020, 0xE007F}, // tag sequences of subdivision flags
}

func isEmojiRune(r rune) bool {
	if r < 0x2100 {
		return false
	}
	for _, rg := range emojiRanges {
		if r < rg[0] {
			return false
		}
		if r <= rg[1] {
			return true
		}
	}
	return false
}

// emojiJoiner reports the invisible parts of emoji sequences: variation
// selectors, the keycap mark and skin-tone modifiers.
func emojiJoiner(r rune) bool {
	return r == 0xFE0F || r == 0xFE0E || r == 0x20E3 || (r >= 0x1F3FB && r <= 0x1F3FF)
}

// stripEmoji replaces or removes emoji in plain text.
func stripEmoji(s string) string { return cleanEmoji(s, false) }

// stripEmojiMarkdown cleans Markdown outside fenced code and inline code.
func stripEmojiMarkdown(md string) string {
	if md == "" {
		return md
	}
	lines := strings.Split(md, "\n")
	fence := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:3]
			continue
		}
		lines[i] = cleanEmoji(line, true)
	}
	return strings.Join(lines, "\n")
}

func cleanEmoji(s string, skipInlineCode bool) string {
	if s == "" {
		return s
	}
	in := []rune(s)
	out := make([]rune, 0, len(in))
	inCode := false
	removed := false // the previous rune was dropped; joiners after it go too
	for i := 0; i < len(in); i++ {
		r := in[i]
		if skipInlineCode && r == '`' {
			inCode = !inCode
		}
		if inCode || r == '`' {
			out = append(out, r)
			removed = false
			continue
		}
		next := rune(0)
		if i+1 < len(in) {
			next = in[i+1]
		}
		if sym, ok := emojiSymbols[r]; ok {
			out = append(out, []rune(sym)...)
			removed = false
			continue
		}
		if emojiJoiner(r) || (r == 0x200D && removed) {
			removed = true
			continue
		}
		// A text-style symbol followed by U+FE0F was meant as emoji.
		if isEmojiRune(r) || (next == 0xFE0F && r > 0x7F) {
			removed = true
			// Drop the space the emoji leaves behind: "✨ Done" → "Done".
			atStart := len(out) == 0 || out[len(out)-1] == ' ' || out[len(out)-1] == '\n' || out[len(out)-1] == '\t'
			j := i + 1
			for j < len(in) && (emojiJoiner(in[j]) || in[j] == 0x200D || (in[j-1] == 0x200D && isEmojiRune(in[j]))) {
				j++
			}
			i = j - 1
			if j < len(in) && in[j] == ' ' && atStart {
				i = j
			} else if (j >= len(in) || in[j] == '\n') && len(out) > 0 && out[len(out)-1] == ' ' {
				out = out[:len(out)-1]
			}
			continue
		}
		removed = false
		out = append(out, r)
	}
	return string(out)
}
