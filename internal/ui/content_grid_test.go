package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestContentGridOriginStableAcrossPaneWidths(t *testing.T) {
	for _, pane := range []int{30, 40, 72, 112} {
		grid := ContentGrid{PaneWidth: pane, Profile: GlyphUnicode}
		if got := grid.OriginX(); got != contentLeftColumns {
			t.Fatalf("pane %d: OriginX = %d, want %d", pane, got, contentLeftColumns)
		}
		if grid.OriginX() != 3 {
			t.Fatalf("pane %d: OriginX must be density-independent constant 3", pane)
		}
	}
}

func TestContentGridContentWidthFloors(t *testing.T) {
	tests := []struct {
		pane int
		want int
	}{
		// pane - 6 below the floor → transcriptMinimumWorkColumns
		{pane: 10, want: transcriptMinimumWorkColumns},
		{pane: 19, want: transcriptMinimumWorkColumns}, // 19-6=13 < 14
		{pane: 20, want: 14},                           // 20-6=14
		{pane: 30, want: 24},
		{pane: 40, want: 34},
		{pane: 72, want: 66},
		{pane: 112, want: 106},
	}
	for _, test := range tests {
		grid := ContentGrid{PaneWidth: test.pane, Profile: GlyphUnicode}
		if got := grid.ContentWidth(); got != test.want {
			t.Fatalf("pane %d: ContentWidth = %d, want %d", test.pane, got, test.want)
		}
		// LineWidth = left chrome + flex content; total chrome remains 6 when
		// the floor is not binding (LineWidth == pane - right chrome).
		if got := grid.LineWidth(); got != contentLeftColumns+test.want {
			t.Fatalf("pane %d: LineWidth = %d, want %d", test.pane, got, contentLeftColumns+test.want)
		}
	}
}

func TestContentGridIndentBlockEmptyLines(t *testing.T) {
	grid := ContentGrid{PaneWidth: 80, Profile: GlyphUnicode}
	input := "hello\n\nworld"
	got := grid.IndentBlock(" ", input)
	want := grid.Prefix(" ") + "hello\n\n" + grid.Prefix(" ") + "world"
	if got != want {
		t.Fatalf("IndentBlock empty lines:\n got %q\nwant %q", got, want)
	}
	// Explicitly: middle empty line must remain empty (no prefix).
	lines := strings.Split(got, "\n")
	if len(lines) != 3 || lines[1] != "" {
		t.Fatalf("empty line was not preserved: %#v", lines)
	}
}

func TestContentGridPrefixAccentWidthOne(t *testing.T) {
	grid := ContentGrid{PaneWidth: 80, Profile: GlyphUnicode}

	// Empty accent becomes a single space cell before pad.
	empty := grid.Prefix("")
	if lipgloss.Width(empty) != contentLeftColumns {
		t.Fatalf("empty Prefix width = %d, want %d (%q)", lipgloss.Width(empty), contentLeftColumns, empty)
	}
	if empty != "   " {
		t.Fatalf("empty Prefix = %q, want three spaces", empty)
	}

	// Single-cell accent + two pad spaces.
	bar := grid.Prefix("│")
	if lipgloss.Width(bar) != contentLeftColumns {
		t.Fatalf("bar Prefix width = %d, want %d", lipgloss.Width(bar), contentLeftColumns)
	}
	if !strings.HasPrefix(bar, "│") || !strings.HasSuffix(bar, "  ") {
		t.Fatalf("bar Prefix = %q, want │ + two spaces", bar)
	}

	// Multi-cell accent is forced to one display cell.
	wide := grid.Prefix(">>")
	if lipgloss.Width(wide) != contentLeftColumns {
		t.Fatalf("wide Prefix width = %d, want %d (%q)", lipgloss.Width(wide), contentLeftColumns, wide)
	}
}

func TestContentGridChromeTokensSumToSix(t *testing.T) {
	if contentLeftColumns+contentRightChromeColumns != transcriptContentChromeColumns {
		t.Fatalf(
			"chrome tokens %d+%d != transcriptContentChromeColumns %d",
			contentLeftColumns,
			contentRightChromeColumns,
			transcriptContentChromeColumns,
		)
	}
	if transcriptContentChromeColumns != 6 {
		t.Fatalf("total chrome = %d, want 6 for WorkWidth = pane-6", transcriptContentChromeColumns)
	}
}

func TestModelContentGridFactory(t *testing.T) {
	m := newTestModel(t)
	m.width = 81 // chatPaneWidth = 80
	m.glyphProfile = GlyphASCII
	grid := m.contentGrid()
	if grid.PaneWidth != m.chatPaneWidth() {
		t.Fatalf("PaneWidth = %d, want chatPaneWidth %d", grid.PaneWidth, m.chatPaneWidth())
	}
	if grid.Profile != GlyphASCII {
		t.Fatalf("Profile = %d, want ASCII", grid.Profile)
	}
	if grid.OriginX() != 3 || grid.ContentWidth() != m.chatContentWidth() {
		t.Fatalf("grid geometry mismatch: origin=%d content=%d chatContent=%d",
			grid.OriginX(), grid.ContentWidth(), m.chatContentWidth())
	}
}
