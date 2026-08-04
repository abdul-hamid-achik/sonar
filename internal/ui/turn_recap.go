package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

// maxTurnRecapCells bounds the scannable one-line digest under an assistant
// turn. Keep it short so the recap reads as chrome, not a second paragraph.
const maxTurnRecapCells = 72

// minTurnRecapCells rejects digests that are too short to be useful
// (e.g. Spanish "¡Claro!" or "Ready when you are.").
const minTurnRecapCells = 24

// buildTurnRecap derives a single-line, human-readable digest from settled
// assistant prose. It is pure (no I/O) so render and memo keys stay cheap.
//
// Recaps only pay off when the answer is long enough that a one-line digest
// actually compresses scrollback. Short single-paragraph replies (and
// truncated copies of the first line the user just read) are suppressed.
func buildTurnRecap(content string) string {
	content = sanitizeTerminalMultiline(content)
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	full := strings.Join(strings.Fields(content), " ")
	// Short answers fit on screen above any digest — never restate them.
	if lipgloss.Width(full) < 200 {
		return ""
	}

	// Prefer the first non-fence, non-heading paragraph.
	plain, compressed := firstProseParagraph(content)
	plain = strings.Join(strings.Fields(plain), " ")
	if plain == "" {
		return ""
	}
	// Prefer a useful first sentence when it is a real compression of a long
	// first paragraph. Short greetings ("¡Claro!") never become the digest.
	if cut := firstSentenceEnd(plain); cut > 0 && cut+1 < len(plain) {
		candidate := strings.TrimSpace(plain[:cut+1])
		if lipgloss.Width(candidate) >= minTurnRecapCells &&
			lipgloss.Width(plain) >= lipgloss.Width(candidate)+40 {
			plain = candidate
			compressed = true
		}
	}
	if lipgloss.Width(plain) < minTurnRecapCells {
		return ""
	}
	// Require multi-section compression (later paragraphs, fences, lists…) —
	// not "echo the first line of a short reply".
	if !compressed {
		return ""
	}
	// Cap to display budget after sentence selection.
	if lipgloss.Width(plain) > maxTurnRecapCells {
		plain = truncateDisplayWithGlyphProfile(plain, maxTurnRecapCells, GlyphUnicode)
		plain = strings.TrimSpace(strings.TrimSuffix(plain, "…"))
		plain = strings.TrimSpace(strings.TrimSuffix(plain, "~"))
		if lipgloss.Width(plain) < minTurnRecapCells {
			return ""
		}
	}
	// Final guard: drop digests that restatement almost the entire visible
	// answer (truncated first line of a short reply). A true first-sentence
	// digest of a long multi-section answer remains — remaining body is large.
	digest := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(plain, "…"), "~"))
	if digest != "" && strings.HasPrefix(full, digest) {
		restW := lipgloss.Width(full) - lipgloss.Width(digest)
		if restW < 100 {
			return ""
		}
	}
	return plain
}

func firstSentenceEnd(s string) int {
	// Prefer . ! ? followed by space or end — ignore decimals like 3.14.
	// Do not treat Spanish opening ¡ ¿ as terminators (they are not ASCII).
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '.' && c != '!' && c != '?' {
			continue
		}
		if i+1 >= len(s) {
			return i
		}
		next, _ := utf8.DecodeRuneInString(s[i+1:])
		if unicode.IsSpace(next) {
			// Skip version-like "v2.0" / "3.14".
			if c == '.' && i > 0 && unicode.IsDigit(rune(s[i-1])) {
				continue
			}
			return i
		}
	}
	return -1
}

// firstProseParagraph extracts the first non-fence, non-heading paragraph and
// reports whether the digest omits answer content (fenced code, later
// paragraphs, or a width-capped tail) — i.e. whether it actually compresses.
func firstProseParagraph(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	lines := strings.Split(s, "\n")
	dropped := false
	inFence := false
	stoppedAt := -1
	for index, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			if trim != "" {
				dropped = true
			}
			continue
		}
		if strings.HasPrefix(trim, "#") {
			trim = strings.TrimLeft(trim, "#")
			trim = strings.TrimSpace(trim)
		}
		// Drop pure list markers for a prose-first digest.
		for _, prefix := range []string{"- ", "* ", "+ "} {
			if strings.HasPrefix(trim, prefix) {
				trim = strings.TrimSpace(trim[len(prefix):])
				break
			}
		}
		if trim == "" {
			if b.Len() > 0 {
				stoppedAt = index // first paragraph only
				break
			}
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		// Strip simple emphasis markers without a full markdown parse.
		trim = strings.NewReplacer("**", "", "__", "", "*", "", "`", "").Replace(trim)
		b.WriteString(trim)
		// Stop after a reasonably long first paragraph.
		if lipgloss.Width(b.String()) >= maxTurnRecapCells {
			stoppedAt = index + 1
			break
		}
	}
	if stoppedAt >= 0 && !dropped {
		for _, line := range lines[stoppedAt:] {
			if strings.TrimSpace(line) != "" {
				dropped = true
				break
			}
		}
	}
	return b.String(), dropped
}

// isTurnRecapSemanticRow reports whether a painted transcript row is a turn
// digest chrome line (not primary answer content). Used to keep search and
// selection budgets focused on the answer body.
func isTurnRecapSemanticRow(semantic string) bool {
	plain := strings.TrimSpace(semantic)
	if strings.HasPrefix(plain, "✳ recap:") || strings.HasPrefix(plain, "* recap:") {
		return true
	}
	// Compact marker form: "✳ <digest>" (not list items — those are shorter
	// bullets and usually lack the dim recap styling path).
	if strings.HasPrefix(plain, "✳ ") && lipgloss.Width(plain) <= maxTurnRecapCells+4 {
		return true
	}
	return false
}

// formatTurnRecapLine paints the digest with a quiet marker. Width is the flex
// content budget. Returns "" when the digest is too short to show.
func formatTurnRecapLine(recap string, width int, isDark bool, themeID string, profile GlyphProfile) string {
	recap = strings.TrimSpace(recap)
	if recap == "" || width < 12 {
		return ""
	}
	if lipgloss.Width(recap) < minTurnRecapCells {
		return ""
	}
	marker := "✳ "
	if resolveGlyphProfile(profile) == GlyphASCII {
		marker = "* "
	}
	budget := max(1, width-lipgloss.Width(marker))
	body := truncateDisplayWithGlyphProfile(recap, budget, profile)
	if lipgloss.Width(body) < min(minTurnRecapCells, budget) {
		return ""
	}
	palette := newSemanticPalette(isDark, themeID)
	style := lipgloss.NewStyle().Foreground(palette.Dim)
	return style.Render(marker + body)
}
