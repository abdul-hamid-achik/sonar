package ui

import (
	"fmt"
	"image/color"
	"strings"

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

	// Diagram art per mermaid source, including failed parses. A streaming
	// prefix re-renders on every new safe boundary, and each pass would
	// otherwise re-run diagram layout for every block already on screen.
	// Bounded by mermaidCacheLimit and inherited across renderer rebuilds
	// via inheritMermaidArt.
	mermaidCache map[string]mermaidResult

	// Measured on first use by mermaidPaintBudget; never inherited, because
	// unlike the art it depends on this renderer's width and style path.
	mermaidBudgetCols int
	mermaidBudgetSet  bool
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

	// Project the Markdown grammar through the active scheme.
	//
	// Only inline code used to be overridden, and its background was two
	// literal hexes — Nord's, so every other scheme rendered code blocks in
	// Nord grey. Everything else came from Glamour's stock light/dark theme,
	// which meant headings, links and emphasis ignored /theme entirely: the
	// transcript changed colour around the prose while the prose did not.
	//
	// The mapping answers the existing vocabulary rather than inventing one.
	// Headings and links take Accent, the role already used for the surfaces
	// a reader navigates by. Emphasis takes Accent2, its companion. Quotes and
	// list markers take Muted, which is what every other secondary text uses.
	// No new colour meanings; the ten roles already cover this.
	palette := newSemanticPalette(isDark, themeID)
	accent := colorHex(palette.Accent)
	accent2 := colorHex(palette.Accent2)
	muted := colorHex(palette.Muted)
	text := colorHex(palette.Text)
	// Border, not Background: the page background would make inline code
	// invisible, and the background is exactly what distinguishes code from
	// prose. Border is the scheme's own "a surface separated from the page"
	// value, which is what this is.
	codeBackground := colorHex(palette.Border)

	// Inline code keeps the scheme's text colour rather than Glamour's red: in
	// a harness red already means a failure or a blocked action, so ordinary
	// paths and commands read as errors. The background is what distinguishes
	// code, and it now comes from the scheme too.
	style.Code.Color = &text
	style.Code.BackgroundColor = &codeBackground

	for _, heading := range []*ansi.StyleBlock{
		&style.Heading, &style.H1, &style.H2, &style.H3, &style.H4, &style.H5, &style.H6,
	} {
		heading.Color = &accent
	}
	style.Link.Color = &accent
	style.LinkText.Color = &accent2
	style.Emph.Color = &accent2
	style.Strong.Color = &accent
	style.BlockQuote.Color = &muted
	style.Item.Color = &muted
	style.Enumeration.Color = &muted
	style.HorizontalRule.Color = &muted

	// Glamour's stock themes inset the document by two columns. The transcript
	// already owns its left chrome through ContentGrid, so that second margin
	// pushed assistant prose to column 6 while the role header, tool receipts,
	// notices, and status all sat at the grid origin. Zero here makes the grid
	// the single owner of the left edge; newMarkdownTermRenderer gives the two
	// reclaimed columns back to the wrap budget so the right edge is unchanged.
	applySchemeSyntaxHighlighting(&style, palette)

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
	// The prose renderer must agree with the layout's prose measure. This was
	// an independent min(ProseTargetCandidate, workWidth) — a hard 96-column
	// cap — which made it the binding constraint on every wide terminal while
	// the layout believed the measure was 140. Two caps for one fact meant
	// changing the visible one changed nothing.
	proseWidth := proseWidthForWork(workWidth)
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

	content = mr.preprocessMermaid(content)

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
	char, runLength, rest, ok := markdownFenceMarker(line)
	if !ok {
		return
	}
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

// markdownFenceMarker parses a line's fence marker: up to three spaces of
// indent, then a run of at least three backticks or tildes. It returns the
// marker character, the run length, and everything after the run. It is the
// single fence grammar — the streaming boundary scanner and the mermaid
// preprocessor both build on it, so they cannot drift apart on what counts
// as a fence.
func markdownFenceMarker(line string) (char byte, length int, rest string, ok bool) {
	markerStart, valid := markdownFenceMarkerStart(line)
	if !valid || markerStart >= len(line) {
		return 0, 0, "", false
	}
	char = line[markerStart]
	if char != '`' && char != '~' {
		return 0, 0, "", false
	}
	for markerStart+length < len(line) && line[markerStart+length] == char {
		length++
	}
	if length < 3 {
		return 0, 0, "", false
	}
	return char, length, line[markerStart+length:], true
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

// applySchemeSyntaxHighlighting projects Chroma's syntax classes onto the
// active scheme.
//
// Glamour ships fixed highlighting — #00AAFF keywords, #676767 comments,
// #C69669 strings — so a code block rendered identically on all ten schemes.
// Switching to Gruvbox or Catppuccin recoloured the entire TUI except the
// inside of a fenced block, which is where a reader spends most of their
// attention.
//
// The mapping answers the existing ten roles and invents no new meanings, as
// the theme contract requires:
//
//   - Dim for comments, which is exactly what Dim is for — present, secondary,
//     not competing with the code.
//   - Special for keywords, the one role reserved for a distinct grammatical
//     class rather than a status.
//   - Success for strings and Warning for numbers, matching the convention
//     every syntax theme already trains a reader on.
//   - Accent for definitions and Accent2 for types and builtins, the same
//     pairing the prose grammar uses for headings and emphasis.
//   - Error for Chroma's error class, so broken syntax reads as broken.
func applySchemeSyntaxHighlighting(style *ansi.StyleConfig, palette semanticPalette) {
	if style.CodeBlock.Chroma == nil {
		style.CodeBlock.Chroma = &ansi.Chroma{}
	}
	text := colorHex(palette.Text)
	dim := colorHex(palette.Dim)
	accent := colorHex(palette.Accent)
	accent2 := colorHex(palette.Accent2)
	special := colorHex(palette.Special)
	success := colorHex(palette.Success)
	warning := colorHex(palette.Warning)
	failure := colorHex(palette.Error)

	chroma := style.CodeBlock.Chroma
	paint := func(target *ansi.StylePrimitive, hex string) {
		value := hex
		target.Color = &value
	}
	paint(&chroma.Text, text)
	paint(&chroma.Name, text)
	paint(&chroma.NameAttribute, text)
	paint(&chroma.Operator, text)
	paint(&chroma.Punctuation, text)

	paint(&chroma.Comment, dim)
	paint(&chroma.CommentPreproc, dim)

	paint(&chroma.Keyword, special)
	paint(&chroma.KeywordReserved, special)
	paint(&chroma.KeywordNamespace, special)

	paint(&chroma.KeywordType, accent2)
	paint(&chroma.NameBuiltin, accent2)
	paint(&chroma.NameClass, accent2)

	paint(&chroma.NameFunction, accent)
	paint(&chroma.NameTag, accent)
	paint(&chroma.NameDecorator, accent)

	paint(&chroma.Literal, success)
	paint(&chroma.LiteralString, success)
	paint(&chroma.LiteralStringEscape, success)

	paint(&chroma.LiteralNumber, warning)

	paint(&chroma.Error, failure)
	paint(&chroma.GenericDeleted, failure)
	paint(&chroma.GenericInserted, success)
	paint(&chroma.GenericEmph, accent2)
	paint(&chroma.GenericStrong, accent)
	paint(&chroma.GenericSubheading, dim)
	paint(&chroma.Background, text)
}
