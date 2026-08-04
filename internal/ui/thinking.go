package ui

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

func reasoningReceiptDigest(content string) [sha256.Size]byte {
	return sha256.Sum256([]byte(content))
}

// processStreamChunk processes a streaming chunk, extracting <think>...</think> tags.
// It handles tag boundaries that may be split across chunks.
func processStreamChunk(chunk string, inThinking bool, searchBuf string) (mainText, thinkText string, outInThinking bool, outSearchBuf string) {
	combined := searchBuf + chunk
	outInThinking = inThinking

	var mainBuf, thinkBuf strings.Builder

	for len(combined) > 0 {
		if outInThinking {
			idx := strings.Index(combined, "</think>")
			if idx >= 0 {
				thinkBuf.WriteString(combined[:idx])
				combined = combined[idx+len("</think>"):]
				outInThinking = false
				continue
			}
			partial := hasPartialTagSuffix(combined, "</think>")
			if partial > 0 {
				thinkBuf.WriteString(combined[:len(combined)-partial])
				outSearchBuf = combined[len(combined)-partial:]
				return mainBuf.String(), thinkBuf.String(), outInThinking, outSearchBuf
			}
			thinkBuf.WriteString(combined)
			combined = ""
		} else {
			idx := strings.Index(combined, "<think>")
			if idx >= 0 {
				mainBuf.WriteString(combined[:idx])
				combined = combined[idx+len("<think>"):]
				outInThinking = true
				continue
			}
			partial := hasPartialTagSuffix(combined, "<think>")
			if partial > 0 {
				mainBuf.WriteString(combined[:len(combined)-partial])
				outSearchBuf = combined[len(combined)-partial:]
				return mainBuf.String(), thinkBuf.String(), outInThinking, outSearchBuf
			}
			mainBuf.WriteString(combined)
			combined = ""
		}
	}

	return mainBuf.String(), thinkBuf.String(), outInThinking, outSearchBuf
}

// hasPartialTagSuffix returns the length of the longest suffix of s
// that is a proper prefix of tag (not the full tag).
func hasPartialTagSuffix(s, tag string) int {
	maxCheck := len(tag) - 1
	if maxCheck > len(s) {
		maxCheck = len(s)
	}
	for i := maxCheck; i > 0; i-- {
		if strings.HasSuffix(s, tag[:i]) {
			return i
		}
	}
	return 0
}

// renderThinkingBox renders a collapsible thinking content box.
func (m *Model) renderThinkingBox(content string, collapsed bool) string {
	content = strings.Trim(sanitizeTerminalMultiline(content), "\r\n")
	if strings.TrimSpace(content) == "" {
		return ""
	}

	// The caller applies content-grid IndentBlock (accent+pad = 3 cells). Bound
	// this box to ContentWidth so the painted line stays within LineWidth.
	width := max(4, m.chatContentWidth())
	inner := max(1, width-2) // left rail plus one separating space
	glyphs := glyphSet(m.glyphProfile)
	direction := glyphs.Collapsed
	if !collapsed {
		direction = glyphs.Expanded
	}
	// Zero-style: collapsed reasoning leads with a size cue instead of dumping
	// the whole chain of thought into the transcript.
	detail := ""
	if collapsed {
		if n := nonEmptyLineCount(content); n > 1 {
			detail = fmt.Sprintf("%d lines", n)
		}
	}
	header := thinkingHeaderWithDetail(direction, detail, inner)

	bar := m.styles.ThinkingBorder.Render(glyphs.Vertical)
	var b strings.Builder
	b.WriteString(bar)
	b.WriteByte(' ')
	b.WriteString(m.styles.ThinkingHeader.Render(header))

	// Keep the settled receipt intentionally quiet. The global help surface owns
	// the disclosure shortcut; repeating it on every assistant turn makes the
	// transcript read like control chrome instead of a conversation.
	if collapsed {
		return b.String()
	}

	lines := strings.Split(content, "\n")
	for _, sourceLine := range lines {
		wrapped := wrapText(sourceLine, inner)
		if wrapped == "" {
			wrapped = " "
		}
		for _, line := range strings.Split(wrapped, "\n") {
			b.WriteByte('\n')
			b.WriteString(bar)
			b.WriteByte(' ')
			b.WriteString(m.styles.ThinkingContent.UnsetPaddingLeft().Render(line))
		}
	}
	return b.String()
}

// Live reasoning shows a bounded tail window rather than the whole buffer.
// Slicing happens after wrapping so mid-word artifacts do not jitter between
// repaints, and only the final raw lines are wrapped so per-token repaints
// stay O(tail) on long reasoning streams.
const (
	liveThinkingTailRows     = 3
	liveThinkingTailRawLines = 6
)

// renderLiveThinkingBox is the stable in-progress counterpart to a completed
// reasoning disclosure. Thinking belongs to the assistant transcript; the
// footer is reserved for operational controls such as cancel and queue.
func (m *Model) renderLiveThinkingBox(content string) string {
	width := max(4, m.chatContentWidth())
	inner := max(1, width-2)
	tail := liveThinkingTail(content, inner)

	header := "Thinking…"
	if len(tail) == 1 {
		// A buffer that wraps to a single line reads best inline; a one-row
		// tail window below would only duplicate it.
		header += " · " + tail[0]
		tail = nil
	}
	if lipgloss.Width(header) > inner {
		header = truncateDisplay(header, inner)
	}

	bar := m.styles.ThinkingBorder.Render(glyphSet(m.glyphProfile).Vertical)
	var b strings.Builder
	b.WriteString(bar)
	b.WriteByte(' ')
	b.WriteString(m.styles.ThinkingHeader.Render(header))
	for _, line := range tail {
		b.WriteByte('\n')
		b.WriteString(bar)
		b.WriteByte(' ')
		b.WriteString(m.styles.ThinkingContent.UnsetPaddingLeft().Render(line))
	}
	return b.String()
}

// liveThinkingTail returns the trailing wrapped lines of the reasoning
// buffer, bounded to liveThinkingTailRows. The window only ever grows toward
// its bound as the buffer streams, so the live surface does not oscillate in
// height per token.
func liveThinkingTail(content string, inner int) []string {
	content = lastRawLines(strings.TrimRight(content, "\r\n"), liveThinkingTailRawLines)
	content = sanitizeTerminalMultiline(content)
	content = strings.Trim(strings.ReplaceAll(content, "\t", " "), "\n")
	if strings.TrimSpace(content) == "" {
		return nil
	}
	var tail []string
	for _, sourceLine := range strings.Split(content, "\n") {
		wrapped := wrapText(sourceLine, inner)
		if wrapped == "" {
			wrapped = " "
		}
		tail = append(tail, strings.Split(wrapped, "\n")...)
	}
	if len(tail) > liveThinkingTailRows {
		tail = tail[len(tail)-liveThinkingTailRows:]
	}
	return tail
}

// lastRawLines bounds the text considered per repaint to the final count raw
// lines without scanning the whole buffer.
func lastRawLines(content string, count int) string {
	seen := 0
	for index := len(content) - 1; index >= 0; index-- {
		if content[index] != '\n' {
			continue
		}
		seen++
		if seen >= count {
			return content[index+1:]
		}
	}
	return content
}

func nonEmptyLineCount(content string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func thinkingHeader(direction string, width int) string {
	return thinkingHeaderWithDetail(direction, "", width)
}

// thinkingHeaderWithDetail builds a Zero-style reasoning label:
// "▸ Thought" or "▸ Thought · 12 lines" when a compact detail is available.
func thinkingHeaderWithDetail(direction, detail string, width int) string {
	header := direction + " Thought"
	detail = strings.TrimSpace(detail)
	if detail != "" {
		header += " · " + detail
	}
	if lipgloss.Width(header) <= width {
		return header
	}
	return truncateDisplay(header, width)
}
