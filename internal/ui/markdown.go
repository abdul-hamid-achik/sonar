package ui

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	glamourStyles "github.com/charmbracelet/glamour/styles"
)

// MarkdownRenderer handles markdown rendering with caching support.
type MarkdownRenderer struct {
	renderer      *glamour.TermRenderer
	proseRenderer *glamour.TermRenderer
	width         int
	proseWidth    int
	isDark        bool
	themeID       string

	// Stable-prefix streaming cache: the rendered output of the last stable
	// markdown prefix, reused across paints so we only re-render when a new
	// safe boundary is crossed.
	cachedStreamPrefix string
	cachedStreamRender string
}

func glamourStyle(isDark bool) string {
	if noColor {
		return "notty"
	}
	if isDark {
		return "dark"
	}
	return "light"
}

func markdownStyleConfig(isDark bool, themeID string) ansi.StyleConfig {
	style := glamourStyles.LightStyleConfig
	if isDark {
		style = glamourStyles.DarkStyleConfig
	}

	// Glamour's standard themes use the same red foreground for inline code
	// in both light and dark terminals. In a harness, red already means a
	// failure or blocked action, so ordinary paths and commands looked like
	// errors. Keep the standard Markdown grammar and code-block highlighting,
	// but project inline code through sonar's adaptive text vocabulary;
	// the background already gives code its distinct visual treatment.
	palette := newSemanticPalette(isDark, themeID)
	lightDark := lipgloss.LightDark(isDark)
	foreground := colorHex(palette.Text)
	background := colorHex(lightDark(
		lipgloss.Color("#ECEFF4"),
		lipgloss.Color("#3B4252"),
	))
	style.Code.Color = &foreground
	style.Code.BackgroundColor = &background

	// Glamour's stock themes inset the document by two columns. The transcript
	// already owns its left chrome through ContentGrid, so that second margin
	// pushed assistant prose to column 6 while the role header, tool receipts,
	// notices, and status all sat at the grid origin. Zero here makes the grid
	// the single owner of the left edge; newMarkdownTermRenderer gives the two
	// reclaimed columns back to the wrap budget so the right edge is unchanged.
	style.Document.Margin = uintPointer(0)
	return style
}

func uintPointer(value uint) *uint { return &value }

func colorHex(value color.Color) string {
	r, g, b, _ := value.RGBA()
	return fmt.Sprintf("#%02X%02X%02X", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

func newMarkdownTermRenderer(width int, isDark bool, themeID string) (*glamour.TermRenderer, error) {
	style := glamour.WithStyles(markdownStyleConfig(isDark, themeID))
	if noColor {
		style = glamour.WithStandardStyle(glamourStyle(isDark))
	}
	return glamour.NewTermRenderer(
		style,
		// Glamour counts its document margin inside the wrap budget, so this
		// stays width-4 and the block keeps its right edge. Zeroing the margin
		// moves prose two columns left onto the content grid and hands those two
		// columns back to the text instead of to dead inset.
		glamour.WithWordWrap(max(1, width-4)),
	)
}

// NewMarkdownRenderer creates a renderer for the given terminal width and theme.
func NewMarkdownRenderer(width int, isDark bool, themeID string) *MarkdownRenderer {
	workWidth := max(1, width)
	proseWidth := min(ProseTargetCandidate, workWidth)
	workRenderer, _ := newMarkdownTermRenderer(workWidth, isDark, themeID)
	proseRenderer := workRenderer
	if proseWidth != workWidth {
		proseRenderer, _ = newMarkdownTermRenderer(proseWidth, isDark, themeID)
	}

	return &MarkdownRenderer{
		renderer:      workRenderer,
		proseRenderer: proseRenderer,
		width:         workWidth,
		proseWidth:    proseWidth,
		isDark:        isDark,
		themeID:       resolveThemeID(themeID),
	}
}

// RenderFull renders a complete markdown document (for finished messages).
// This is the "format-on-complete" path used when streaming ends.
func (mr *MarkdownRenderer) RenderFull(content string) string {
	if content == "" || mr.renderer == nil {
		return content
	}

	renderer := mr.proseRenderer
	if renderer == nil || markdownUsesWorkWidth(content) {
		renderer = mr.renderer
	}
	rendered, err := renderer.Render(content)
	if err != nil {
		return content
	}

	return strings.TrimRight(rendered, "\n")
}

// markdownUsesWorkWidth keeps structural work surfaces out of the readable
// prose measure. A mixed document currently stays on the work renderer as one
// semantic unit; this avoids narrowing code, tables, or indented logs while a
// future AST renderer can assign measures block by block.
func markdownUsesWorkWidth(content string) bool {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	for index, line := range lines {
		if _, ok := markdownFenceMarkerStart(line); ok {
			trimmed := strings.TrimLeft(line, " ")
			if len(trimmed) >= 3 &&
				(strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
				return true
			}
		}
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ") {
			return true
		}
		if index > 0 && strings.Contains(lines[index-1], "|") && markdownTableDelimiterLine(line) {
			return true
		}
	}
	return false
}

func markdownTableDelimiterLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || !strings.Contains(line, "-") {
		return false
	}
	line = strings.Trim(line, "|")
	cells := strings.Split(line, "|")
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		cell = strings.TrimPrefix(cell, ":")
		cell = strings.TrimSuffix(cell, ":")
		if len(cell) < 3 || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

// RenderStreaming renders content during streaming (plain text, no Glamour).
// This avoids jitter from re-rendering incomplete markdown.
func (mr *MarkdownRenderer) RenderStreaming(content string) string {
	return content
}

// RenderStreamingFormatted renders in-progress content using the stable-prefix
// technique: the portion of the document up to the last safe markdown boundary
// (a blank line not inside an open code fence) is rendered with Glamour and
// cached; the trailing partial paragraph is returned separately to be shown as
// plain text. This makes streaming look formatted instead of "popping" into
// shape on completion, without the jitter of re-rendering incomplete markdown.
func (mr *MarkdownRenderer) RenderStreamingFormatted(content string) (formatted, tail string) {
	if mr == nil || mr.renderer == nil || content == "" {
		return "", content
	}

	b := findSafeMarkdownBoundary(content)
	if b <= 0 {
		// No complete block yet — stream the whole thing as plain text.
		return "", content
	}

	prefix := content[:b]
	tail = strings.TrimLeft(content[b:], "\n")

	if prefix == mr.cachedStreamPrefix {
		return mr.cachedStreamRender, tail
	}
	rendered := strings.TrimRight(mr.RenderFull(prefix), "\n")
	mr.cachedStreamPrefix = prefix
	mr.cachedStreamRender = rendered
	return rendered, tail
}

// findSafeMarkdownBoundary returns the byte offset of the latest blank-line
// paragraph break in content that is not inside an open fenced code block, so
// we never split a fence in half. Returns 0 if there is no safe boundary yet.
//
// It runs in a single linear pass: fence state is tracked line-by-line as we
// scan, and each "\n\n" is recorded as the latest safe boundary whenever the
// fence is currently closed. Both ``` and ~~~ fences are tracked. A closing
// fence must use the opening character and at least the opening run length.
func findSafeMarkdownBoundary(content string) int {
	var fence markdownFenceState
	lineStart := 0
	lastSafe := 0

	for i := 0; i < len(content); i++ {
		if content[i] != '\n' {
			continue
		}
		// The line content[lineStart:i] is now complete; fold it into fence state.
		fence.applyLine(content[lineStart:i])
		// A blank line (a second '\n') marks a paragraph boundary at i. It is
		// safe to split there only if no code fence is currently open.
		if i+1 < len(content) && content[i+1] == '\n' && !fence.open {
			lastSafe = i
		}
		lineStart = i + 1
	}
	return lastSafe
}

type markdownFenceState struct {
	open   bool
	char   byte
	length int
}

// applyLine recognizes the CommonMark fence rules that affect streaming
// boundaries. Openers and closers may be indented by up to three spaces.
// Closers must use the opening character, contain at least as many markers, and
// have no trailing content. Backtick info strings may not contain a backtick.
func (state *markdownFenceState) applyLine(line string) {
	markerStart, ok := markdownFenceMarkerStart(line)
	if !ok || markerStart >= len(line) {
		return
	}
	char := line[markerStart]
	if char != '`' && char != '~' {
		return
	}
	runLength := 0
	for markerStart+runLength < len(line) && line[markerStart+runLength] == char {
		runLength++
	}
	if runLength < 3 {
		return
	}

	rest := line[markerStart+runLength:]
	if !state.open {
		if char == '`' && strings.ContainsRune(rest, '`') {
			return
		}
		state.open = true
		state.char = char
		state.length = runLength
		return
	}

	if char != state.char || runLength < state.length || !markdownFenceClosingSuffix(rest) {
		return
	}
	*state = markdownFenceState{}
}

func markdownFenceMarkerStart(line string) (int, bool) {
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent++
		if indent > 3 {
			return 0, false
		}
	}
	return indent, true
}

func markdownFenceClosingSuffix(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r':
		default:
			return false
		}
	}
	return true
}

// SetWidth updates the renderer for a new terminal width.
func (mr *MarkdownRenderer) SetWidth(width int) {
	mr.width = width
	mr.cachedStreamPrefix = ""
	mr.cachedStreamRender = ""
	r, err := newMarkdownTermRenderer(width, mr.isDark, mr.themeID)
	if err == nil {
		mr.renderer = r
	}
}
