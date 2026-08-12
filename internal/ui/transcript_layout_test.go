package ui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestTranscriptEntrySeparatorOwnsVerticalRhythm(t *testing.T) {
	tests := []struct {
		name              string
		previous, current string
		want              string
	}{
		// Consistent one-blank-row pad between messages (Grok rhythm).
		{name: "user to assistant", previous: "user", current: "assistant", want: "\n\n"},
		{name: "user to notice", previous: "user", current: "system", want: "\n\n"},
		{name: "notice to assistant", previous: "system", current: "assistant", want: "\n\n"},
		{name: "tool to answer", previous: "tool_group", current: "assistant", want: "\n\n"},
		{name: "new user turn", previous: "assistant", current: "user", want: "\n\n"},
		// Dense stacks only.
		{name: "tool sequence", previous: "tool_group", current: "tool_group", want: "\n"},
		{name: "assistant segments", previous: "assistant", current: "assistant", want: "\n"},
		{name: "notice stack", previous: "system", current: "system", want: "\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transcriptEntrySeparator(tt.previous, tt.current); got != tt.want {
				t.Fatalf("separator %q -> %q = %q, want %q", tt.previous, tt.current, got, tt.want)
			}
		})
	}
}

func TestSystemNoticesAreExplicitAndBounded(t *testing.T) {
	m := newTestModel(t)
	for _, width := range []int{30, 80} {
		m.width = width
		plain := ansi.Strip(m.renderSystemNotice("Model changed to local", width-2))
		if !strings.Contains(plain, "notice · Model changed") {
			t.Fatalf("%d-column host notice is indistinguishable:\n%s", width, plain)
		}
		for _, line := range strings.Split(plain, "\n") {
			if len([]rune(line)) > width {
				t.Fatalf("%d-column host notice overflowed: %q", width, line)
			}
		}
	}
}

func TestRenderEntriesNestsReasoningAndDenselyStacksTools(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{
		{Kind: "user", Content: "check it"},
		{Kind: "assistant", Content: "starting", RenderedContent: "starting", ThinkingContent: "inspect first", ThinkingCollapsed: true},
		{Kind: "tool_group", ToolIndex: 0},
		{Kind: "tool_group", ToolIndex: 1},
		{Kind: "assistant", Content: "finished", RenderedContent: "finished"},
	}
	m.toolEntries = []ToolEntry{
		{ID: "one", Name: "read_file", Status: ToolStatusDone, Duration: 10 * time.Millisecond, Collapsed: true},
		{ID: "two", Name: "bash", Status: ToolStatusDone, Duration: 20 * time.Millisecond, Collapsed: true},
	}

	plain := ansi.Strip(m.renderEntries())
	// Turn ownership is structural, not labelled: reasoning comes first, then
	// the answer it produced. No role header separates them.
	reasoningAt := strings.Index(plain, "Thought")
	startingAt := strings.Index(plain, "starting")
	if reasoningAt < 0 || startingAt < reasoningAt {
		t.Fatalf("reasoning did not precede the answer it produced:\n%s", plain)
	}
	if strings.Contains(plain, "assistant\n") {
		t.Fatalf("transcript reintroduced a role label:\n%s", plain)
	}
	// ToolCard owns content-grid accent+pad; dense stack is a single newline
	// between receipts with no parent indent. Collapsed success stays calm
	// (no ▸ on tool lines; duration only when the content column is wide).
	if !strings.Contains(plain, "✓ Read") {
		t.Fatalf("collapsed success receipt missing success glyph:\n%s", plain)
	}
	for _, line := range strings.Split(plain, "\n") {
		trimmed := strings.TrimSpace(line)
		// Tool headers start with the vertical bar accent; thinking may still
		// use ▸. Assert only tool lines stay disclosure-free when collapsed.
		if strings.HasPrefix(trimmed, "│") && (strings.Contains(trimmed, "✓") || strings.Contains(trimmed, "✗") || strings.Contains(trimmed, "!")) {
			if strings.Contains(trimmed, "▸") {
				t.Fatalf("collapsed tool receipt still paints collapsed disclosure:\n%s", line)
			}
		}
	}
	// Default test width yields ContentWidth >= 72, so tertiary duration may
	// appear; either stacked form is valid as long as density holds.
	if !strings.Contains(plain, "Read\n│") && !strings.Contains(plain, "Read (10ms)\n│") {
		t.Fatalf("consecutive tool receipts are not densely stacked:\n%s", plain)
	}
	if strings.Contains(plain, "starting\n\n\n") || strings.Contains(plain, "Read\n\n\n") || strings.Contains(plain, "(20ms)\n\n\n") {
		t.Fatalf("semantic boundary contains duplicate blank rows:\n%s", plain)
	}
	if len(m.toolHitRegions) != 2 || m.toolHitRegions[1].Row != m.toolHitRegions[0].Row+1 {
		t.Fatalf("dense ToolCard headers do not have exact adjacent hit rows: %#v", m.toolHitRegions)
	}
	if m.toolHitRegions[1].EndCol <= 0 {
		t.Fatalf("ToolCard header has no horizontal hit bound: %#v", m.toolHitRegions[1])
	}
	secondRegion := m.toolHitRegions[1]
	// Accent bar owns column 0 of the content grid; the hit region starts there
	// (no parent left-margin indent).
	if secondRegion.StartCol < 0 || secondRegion.StartCol >= secondRegion.EndCol {
		t.Fatalf("ToolCard header hit bounds are invalid: %#v", secondRegion)
	}
	m.handleMouseClick(secondRegion.EndCol, secondRegion.Row)
	if !m.toolEntries[0].Collapsed || !m.toolEntries[1].Collapsed {
		t.Fatal("clicking immediately beyond a rendered header toggled a receipt")
	}
	if secondRegion.StartCol > 0 {
		m.handleMouseClick(secondRegion.StartCol-1, secondRegion.Row)
		if !m.toolEntries[0].Collapsed || !m.toolEntries[1].Collapsed {
			t.Fatal("clicking immediately before a rendered header toggled a receipt")
		}
	}
	m.handleMouseClick(secondRegion.StartCol, secondRegion.Row)
	if !m.toolEntries[0].Collapsed || m.toolEntries[1].Collapsed {
		t.Fatalf("clicking the first rendered header cell toggled the wrong receipt: %#v", m.toolEntries)
	}
	m.toolEntries[1].Collapsed = true
	m.invalidateEntryCache()
	m.refreshTranscript()
	secondRegion = m.toolHitRegions[1]
	m.handleMouseClick(secondRegion.EndCol-1, secondRegion.Row)
	if !m.toolEntries[0].Collapsed || m.toolEntries[1].Collapsed {
		t.Fatalf("clicking the last rendered header cell toggled the wrong receipt: %#v", m.toolEntries)
	}
	m.toolEntries[1].Collapsed = true
	m.invalidateEntryCache()
	m.refreshTranscript()
	secondRegion = m.toolHitRegions[1]
	m.handleMouseClick(secondRegion.StartCol, secondRegion.Row-1)
	if m.toolEntries[0].Collapsed || !m.toolEntries[1].Collapsed {
		t.Fatal("clicking the preceding dense header did not target only that ToolCard")
	}
	m.toolEntries[0].Collapsed = true
	m.invalidateEntryCache()
	m.refreshTranscript()
	secondRegion = m.toolHitRegions[1]
	m.handleMouseClick(secondRegion.StartCol, secondRegion.Row+1)
	if !m.toolEntries[0].Collapsed || !m.toolEntries[1].Collapsed {
		t.Fatal("clicking below a rendered ToolCard header toggled a receipt")
	}
}

// firstNonSpaceCol returns the display column of the first non-space cell in
// an ANSI-stripped line, or -1 when the line is empty/whitespace.
func firstNonSpaceCol(line string) int {
	plain := ansi.Strip(line)
	trimmed := strings.TrimLeft(plain, " ")
	if trimmed == "" {
		return -1
	}
	return lipgloss.Width(plain) - lipgloss.Width(trimmed)
}

func TestContentGridOriginSharedAcrossSurfaces(t *testing.T) {
	// Minimum welcome (chrome off): semantic text begins at OriginX after pad.
	// Roomy frames omit welcome entirely — OriginX is proven on assistant/tools.
	m := newTestModel(t)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	var welcome strings.Builder
	m.renderWelcome(&welcome)
	welcomeFirst := strings.Split(welcome.String(), "\n")[0]
	// Micro welcome may use full-row paint without OriginX pad (30-col contract).
	if m.chatPaneWidth() >= 36 {
		if got := firstNonSpaceCol(welcomeFirst); got != contentLeftColumns {
			t.Fatalf("welcome first semantic col = %d, want OriginX=%d\nline=%q", got, contentLeftColumns, ansi.Strip(welcomeFirst))
		}
	}
	// Restore roomy size for assistant/tool origin checks below.
	m.width = 81
	m.height = 24

	// Assistant body: IndentBlock space accent → OriginX.
	m.entries = []ChatEntry{
		{Kind: "assistant", Content: "hello body", RenderedContent: "hello body"},
	}
	assistantPlain := ansi.Strip(m.renderEntries())
	var assistantBodyCol = -1
	for _, line := range strings.Split(assistantPlain, "\n") {
		if strings.Contains(line, "hello body") {
			assistantBodyCol = firstNonSpaceCol(line)
			break
		}
	}
	if assistantBodyCol != contentLeftColumns {
		t.Fatalf("assistant body semantic col = %d, want OriginX=%d\n%s", assistantBodyCol, contentLeftColumns, assistantPlain)
	}

	// Tool card: │ at column 0; semantic content after bar+pad at OriginX.
	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.SetSummary("path/to/file")
	lineWidth := contentLeftColumns + 40
	toolLine := strings.Split(ansi.Strip(card.View(lineWidth)), "\n")[0]
	if !strings.HasPrefix(strings.TrimRight(toolLine, " "), "│") {
		t.Fatalf("tool header must start with vertical bar at col 0: %q", toolLine)
	}
	// After accent(1)+pad(2), the status glyph is the first semantic cell.
	if got := firstNonSpaceCol(toolLine); got != 0 {
		t.Fatalf("tool accent bar col = %d, want 0: %q", got, toolLine)
	}
	// Semantic content begins at OriginX (status glyph after bar + pad).
	runes := []rune(toolLine)
	if len(runes) < contentLeftColumns+1 {
		t.Fatalf("tool header too short for OriginX check: %q", toolLine)
	}
	// Prefix is "│  " (3 cells); next rune is the status glyph.
	if string(runes[:contentLeftColumns]) != "│  " {
		t.Fatalf("tool grid prefix = %q, want │ + two spaces", string(runes[:contentLeftColumns]))
	}
	if string(runes[contentLeftColumns]) != "✓" {
		t.Fatalf("tool semantic content at OriginX = %q, want ✓: %q", string(runes[contentLeftColumns]), toolLine)
	}
}

// TestCollapsedToolFooterIsClickable closes a gap the receipt advertised
// itself: "N lines hidden · <expand chord>" is the only row of a tool card that
// names a key, and it was the only row a click could not reach — the pointer
// region covered the header line alone. An affordance that states how to reach
// hidden content and then ignores the pointer aimed at it is worse than a
// silent one.
func TestCollapsedToolFooterIsClickable(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{
		{BlockID: "block-hidden-user", Revision: 1, Lifecycle: BlockSettled, Kind: "user", Content: "run it"},
		{BlockID: "block-hidden-1", Revision: 1, Lifecycle: BlockSettled, Kind: "tool_group", ToolIndex: 0},
	}
	m.toolEntries = []ToolEntry{{
		ID: "one", Name: "bash", Status: ToolStatusDone, Collapsed: true,
		Result: "first line\nsecond line\nthird line",
	}}

	plain := ansi.Strip(m.renderEntries())
	wantCue := "lines hidden · " + m.keys.ToggleFocusedTool.Help().Key
	if !strings.Contains(plain, wantCue) {
		t.Fatalf("collapsed receipt did not advertise its hidden body (%q):\n%s", wantCue, plain)
	}
	if len(m.toolHitRegions) != 2 {
		t.Fatalf("collapsed receipt has %d hit regions, want header and footer: %#v",
			len(m.toolHitRegions), m.toolHitRegions)
	}
	header, footer := m.toolHitRegions[0], m.toolHitRegions[1]
	if footer.ToolIndex != 0 || footer.Row <= header.Row {
		t.Fatalf("footer region does not sit below its own header: %#v %#v", header, footer)
	}
	if footer.StartCol < 0 || footer.StartCol >= footer.EndCol {
		t.Fatalf("footer region has invalid horizontal bounds: %#v", footer)
	}

	m.handleMouseClick(footer.StartCol, footer.Row-m.transcriptYOffset())
	if m.toolEntries[0].Collapsed {
		t.Fatal("clicking the hidden-lines cue did not expand the receipt")
	}

	// Expanded, the cue is gone and so is its region: the body it pointed at is
	// on screen, and a stale region would toggle a row that no longer says so.
	m.renderEntries()
	if len(m.toolHitRegions) != 1 {
		t.Fatalf("expanded receipt kept a footer hit region: %#v", m.toolHitRegions)
	}
}

// TestCollapsedToolFooterRegionIgnoresExpandedBodyText pins the one thing the
// rendered chunk cannot tell the scanner on its own. An expanded receipt whose
// output happens to contain a line reading like the cue must not gain a
// pointer region for it — that row is tool output, not chrome.
func TestCollapsedToolFooterRegionIgnoresExpandedBodyText(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{
		{BlockID: "block-shown-user", Revision: 1, Lifecycle: BlockSettled, Kind: "user", Content: "run it"},
		{BlockID: "block-hidden-2", Revision: 1, Lifecycle: BlockSettled, Kind: "tool_group", ToolIndex: 0},
	}
	m.toolEntries = []ToolEntry{{
		ID: "one", Name: "bash", Status: ToolStatusDone, Collapsed: false,
		Result: "grep output\n42 lines hidden · ctrl+t\ntail",
	}}

	m.renderEntries()
	if len(m.toolHitRegions) != 1 {
		t.Fatalf("expanded receipt body was mistaken for the hidden-lines cue: %#v", m.toolHitRegions)
	}
}
