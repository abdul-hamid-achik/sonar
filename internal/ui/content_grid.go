package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Content-grid chrome is pane-relative and density-independent.
//
//	Accent(1) + LeftPad(2) + Content(flex) + RightChrome(3)
//
// Origin X is always contentLeftColumns (3). Total horizontal chrome is 6 so
// WorkWidth remains pane-6 and existing width contracts keep passing.
const (
	contentAccentColumns      = 1
	contentLeftPadColumns     = 2
	contentLeftColumns        = contentAccentColumns + contentLeftPadColumns
	contentRightChromeColumns = 3
)

// ContentGrid is the single owner of transcript content-column geometry for a
// pane width and glyph profile. It does not own density or truncation budgets.
type ContentGrid struct {
	PaneWidth int
	Profile   GlyphProfile
}

// contentGrid returns the transcript content grid for the current chat pane.
func (m *Model) contentGrid() ContentGrid {
	if m == nil {
		return ContentGrid{PaneWidth: 1, Profile: GlyphUnicode}
	}
	return ContentGrid{
		PaneWidth: m.chatPaneWidth(),
		Profile:   m.glyphProfile,
	}
}

// ContentWidth is the flex column budget for semantic text. It matches
// chatContentWidth numerically: pane minus left and right chrome, floored at
// transcriptMinimumWorkColumns so caches stay stable on very narrow panes.
func (g ContentGrid) ContentWidth() int {
	width := g.PaneWidth - contentLeftColumns - contentRightChromeColumns
	if width < transcriptMinimumWorkColumns {
		width = transcriptMinimumWorkColumns
	}
	return width
}

// OriginX is the pane-relative column where semantic content begins. It is
// always contentLeftColumns and never density-dependent.
func (g ContentGrid) OriginX() int {
	return contentLeftColumns
}

// LineWidth is accent + pad + content (= pane − right chrome when not floored).
func (g ContentGrid) LineWidth() int {
	return contentLeftColumns + g.ContentWidth()
}

// Prefix forces accent to one display cell and appends the two-cell left pad.
// Empty accents become a single space so OriginX stays aligned.
func (g ContentGrid) Prefix(accent string) string {
	switch w := lipgloss.Width(accent); {
	case w <= 0:
		accent = " "
	case w > contentAccentColumns:
		accent = truncateDisplayWithGlyphProfile(accent, contentAccentColumns, g.Profile)
	}
	return accent + strings.Repeat(" ", contentLeftPadColumns)
}

// Line paints one grid row: Prefix(accent) plus content truncated to ContentWidth.
func (g ContentGrid) Line(accent, content string) string {
	return g.Prefix(accent) + truncateDisplayWithGlyphProfile(content, g.ContentWidth(), g.Profile)
}

// IndentBlock prefixes every non-empty line with Prefix(accent). Empty lines
// stay empty so vertical rhythm is preserved.
func (g ContentGrid) IndentBlock(accent, block string) string {
	if block == "" {
		return ""
	}
	prefix := g.Prefix(accent)
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
