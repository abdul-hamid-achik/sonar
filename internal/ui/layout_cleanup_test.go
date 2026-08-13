package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestMultilineComposerCostsOneViewportRowPerLine(t *testing.T) {
	for _, width := range []int{80, 60, 40} {
		m := newTestModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 20})
		m = updated.(*Model)
		initial := m.viewport.Height()

		m.input.SetValue("one\ntwo\nthree")
		m.syncInputHeight()
		if got, want := m.viewport.Height(), initial-2; got != want {
			t.Fatalf("width %d: three-line viewport height = %d, want %d", width, got, want)
		}

		m.input.SetValue("one")
		m.syncInputHeight()
		if got := m.viewport.Height(); got != initial {
			t.Fatalf("width %d: collapsed composer height = %d, want %d", width, got, initial)
		}
	}
}

func TestComposerGrowsForSoftWrappedTyping(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m = updated.(*Model)
	initialViewportHeight := m.viewport.Height()

	for _, character := range strings.Repeat("wrapped draft ", 9) {
		updated, _ = m.Update(charKey(character))
		m = updated.(*Model)
	}

	if got := m.input.LineCount(); got != 1 {
		t.Fatalf("soft-wrapped draft has %d logical lines, want 1", got)
	}
	if m.inputLines <= 1 || m.inputLines != m.input.Height() {
		t.Fatalf("soft-wrapped composer height = tracked %d, child %d; want > 1 and equal", m.inputLines, m.input.Height())
	}
	// A capped draft owns one explicit overflow cue in addition to its visible
	// rows; uncapped drafts retain the original one-row-to-one-row reflow.
	cueRows := 0
	if m.renderComposerOverflowCue() != "" {
		cueRows = 1
	}
	if got, want := m.viewport.Height(), initialViewportHeight-(m.inputLines-1)-cueRows; got != want {
		t.Fatalf("soft-wrapped viewport height = %d, want %d", got, want)
	}
}

func TestComposerReflowsDraftAcrossTerminalWidths(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 42, Height: 30})
	m = updated.(*Model)
	m.input.SetValue(strings.Repeat("reflow me ", 16))
	m.syncInputHeight()
	narrowRows := m.inputLines
	if narrowRows <= 1 {
		t.Fatalf("narrow composer height = %d, want wrapped rows", narrowRows)
	}

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(*Model)
	if m.inputLines >= narrowRows {
		t.Fatalf("wide composer height = %d, want less than narrow height %d", m.inputLines, narrowRows)
	}

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 42, Height: 30})
	m = updated.(*Model)
	if m.inputLines != narrowRows {
		t.Fatalf("restored narrow composer height = %d, want %d", m.inputLines, narrowRows)
	}
}

func TestSingleLinePasteUsesWrappedComposerRowsOnce(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 44, Height: 24})
	m = updated.(*Model)
	content := strings.Repeat("pasted words ", 12)

	updated, _ = m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)
	if m.pendingPaste != nil {
		t.Fatalf("small one-line paste unexpectedly requires review: %#v", m.pendingPaste)
	}
	if got := m.input.Value(); got != content {
		t.Fatalf("paste inserted %d bytes, want exact %d-byte payload", len(got), len(content))
	}
	if m.input.LineCount() != 1 || m.inputLines <= 1 {
		t.Fatalf("pasted composer logical lines=%d visible rows=%d, want 1 and >1", m.input.LineCount(), m.inputLines)
	}
}

func TestComposerCapsVisibleRowsAndKeepsDraftTailVisible(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	limit := composerVisibleRowLimit(m.height)
	m.input.SetValue(strings.Repeat("long draft ", 70) + "VISIBLE-TAIL")
	_ = m.reflowInputViewport()
	m.syncInputHeight()

	if got := m.inputLines; got != limit {
		t.Fatalf("capped composer height = %d, want %d", got, limit)
	}
	if got := m.input.ScrollYOffset(); got <= 0 {
		t.Fatalf("capped composer scroll offset = %d, want > 0 at the draft tail", got)
	}
	view := m.View().Content
	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("capped composer view height = %d, want <= %d", got, m.height)
	}
	if !strings.Contains(ansi.Strip(m.input.View()), "VISIBLE-TAIL") {
		t.Fatalf("capped composer hid the draft tail:\n%s", m.input.View())
	}
	if cue := ansi.Strip(m.renderComposerOverflowCue()); !strings.Contains(cue, "earlier") || !strings.Contains(cue, "ctrl+home") {
		t.Fatalf("capped composer has no earlier-draft recovery cue: %q", cue)
	}
}

func TestComposerOverflowCueTracksTopMiddleAndTail(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	m.input.SetValue(strings.Repeat("draft row\n", 16))
	_ = m.reflowInputViewport()

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	m = updated.(*Model)
	if cue := ansi.Strip(m.renderComposerOverflowCue()); !strings.Contains(cue, "later") {
		t.Fatalf("top cue = %q, want later rows", cue)
	}

	for range 8 {
		updated, _ = m.Update(downKey())
		m = updated.(*Model)
	}
	if cue := ansi.Strip(m.renderComposerOverflowCue()); !strings.Contains(cue, "earlier") || !strings.Contains(cue, "later") {
		t.Fatalf("middle cue = %q, want both earlier and later rows", cue)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	m = updated.(*Model)
	if cue := ansi.Strip(m.renderComposerOverflowCue()); !strings.Contains(cue, "earlier") {
		t.Fatalf("tail cue = %q, want earlier rows", cue)
	}
}

func TestComposerOverflowCueStaysHiddenWhenMultilineDraftFits(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	m.input.SetValue("first row\nsecond row\nthird row")
	_ = m.reflowInputViewport()

	if earlier, later := m.composerHiddenRows(); earlier != 0 || later != 0 {
		t.Fatalf("fully visible draft hidden rows = earlier %d later %d", earlier, later)
	}
	if cue := ansi.Strip(m.renderComposerOverflowCue()); cue != "" {
		t.Fatalf("fully visible multiline draft showed overflow cue %q", cue)
	}
}

func TestComposerShrinksAfterEditingDeletion(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	for _, character := range strings.Repeat("wrapped draft ", 20) {
		updated, _ = m.Update(charKey(character))
		m = updated.(*Model)
	}
	if m.inputLines <= 1 {
		t.Fatal("fixture did not grow the composer")
	}

	updated, _ = m.Update(ctrlKey('u'))
	m = updated.(*Model)
	if got := m.input.Value(); got != "" {
		t.Fatalf("Ctrl+U left draft %q, want empty", got)
	}
	if m.inputLines != 1 || m.input.Height() != 1 {
		t.Fatalf("deleted composer height = parent %d child %d, want 1", m.inputLines, m.input.Height())
	}
}

func TestSubmittingTallSlashCommandResetsComposerAllocation(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	m.input.SetValue(strings.Repeat("/missing-command argument ", 20))
	_ = m.reflowInputViewport()
	if m.inputLines <= 1 {
		t.Fatal("fixture did not grow the composer")
	}

	_ = m.submitPreparedInput(m.input.Value())
	if m.input.Value() != "" || m.inputLines != 1 || m.input.Height() != 1 {
		t.Fatalf("submitted composer = %q, parent height %d, child height %d", m.input.Value(), m.inputLines, m.input.Height())
	}
}

func TestFiveRowComposerFitsTerminalAndKeepsTailVisible(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 32})
	m = updated.(*Model)
	lines := make([]string, 14)
	for i := range lines {
		lines[i] = "fixture line " + string(rune('A'+i))
	}
	m.input.SetValue(strings.Join(lines, "\n"))
	m.syncInputHeight()
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	m = updated.(*Model)

	view := m.View().Content
	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("five-row composer view height = %d, want <= %d", got, m.height)
	}
	if !strings.Contains(view, lines[len(lines)-1]) {
		t.Fatalf("five-row composer clipped its tail:\n%s", view)
	}
}

func TestMultilineComposerUsesOneSendMarkerAndContinuationRails(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("first\nsecond\nthird")
	m.syncInputHeight()

	view := ansi.Strip(m.input.View())
	if got := strings.Count(view, "❯ "); got != 1 {
		t.Fatalf("multiline composer rendered %d send markers, want one:\n%s", got, view)
	}
	if got := strings.Count(view, "│ "); got != 2 {
		t.Fatalf("multiline composer rendered %d continuation rails, want two:\n%s", got, view)
	}
}

func TestHeightOnlyResizePreservesRenderedCaches(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{
		Kind: "assistant", Content: "cached answer", RenderedContent: "cached answer",
	}}
	m.refreshTranscript()
	if !m.transcriptPaint.cache.valid {
		t.Fatal("precondition: transcript paint cache should be valid")
	}
	markdown := m.md
	rendered := m.entries[0].RenderedContent

	updated, _ := m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height + 5})
	m = updated.(*Model)

	if m.md != markdown {
		t.Fatal("height-only resize recreated the markdown renderer")
	}
	if m.entries[0].RenderedContent != rendered {
		t.Fatal("height-only resize rebuilt completed assistant markdown")
	}
	if !m.transcriptPaint.cache.valid {
		t.Fatal("height-only resize invalidated the transcript paint cache")
	}
}

func TestRunningToolGroupIsPartOfStableEventDrivenCache(t *testing.T) {
	m := newTestModel(t)
	m.ready = true
	m.now = func() time.Time { return time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC) }
	m.entries = []ChatEntry{
		{Kind: "user", Content: "hola"},
		{Kind: "assistant", Content: "working", RenderedContent: "working"},
		{Kind: "tool_group", ToolIndex: 0},
	}
	m.toolEntries = []ToolEntry{{ID: "t1", Name: "bash", Status: ToolStatusRunning}}
	m.toolsPending = 1

	first := m.renderEntries()
	if !m.entryCacheValid {
		t.Fatal("full render did not cache the stable prefix")
	}
	if m.cachedStableCount != len(m.entries) {
		t.Fatalf("stable prefix length = %d, want every event-driven entry %d", m.cachedStableCount, len(m.entries))
	}
	cachedPrefix := m.cachedEntriesRender

	// An ordinary warm render takes the fast path with byte-identical output
	// and leaves the complete event-driven prefix untouched.
	second := m.renderEntries()
	if second != first {
		t.Fatalf("fast path diverged from full render:\n--- full ---\n%s\n--- fast ---\n%s", first, second)
	}
	if m.cachedEntriesRender != cachedPrefix || m.cachedStableCount != len(m.entries) {
		t.Fatal("fast path mutated the cached stable prefix")
	}

	// The full/fast outputs must also agree hit-region arithmetic.
	fastRegions := append([]toolHitRegion(nil), m.toolHitRegions...)
	m.invalidateEntryCache()
	if full := m.renderEntries(); full != second {
		t.Fatalf("invalidated full render diverged from fast path:\n--- full ---\n%s\n--- fast ---\n%s", full, second)
	}
	if len(fastRegions) != len(m.toolHitRegions) {
		t.Fatalf("hit regions diverged: fast %d full %d", len(fastRegions), len(m.toolHitRegions))
	}
	for i := range fastRegions {
		if fastRegions[i] != m.toolHitRegions[i] {
			t.Fatalf("hit region %d diverged: fast %#v full %#v", i, fastRegions[i], m.toolHitRegions[i])
		}
	}

	// A real lifecycle event invalidates and replaces the cached tool receipt.
	m.toolEntries[0].Status = ToolStatusDone
	m.toolsPending = 0
	m.invalidateEntryCache()
	_ = m.renderEntries()
	if m.cachedStableCount != len(m.entries) {
		t.Fatalf("settled transcript stable prefix = %d, want %d", m.cachedStableCount, len(m.entries))
	}
}

func TestMouseHitTestingStartsAtViewportRowZero(t *testing.T) {
	m := newTestModel(t)
	m.toolEntries = []ToolEntry{{Collapsed: true}}
	m.toolHitRegions = []toolHitRegion{{ToolIndex: 0, Row: 0, EndCol: 12}}
	m.handleMouseClick(0, 0)
	if m.toolEntries[0].Collapsed {
		t.Fatal("row-zero tool card was not toggled")
	}

	m.setTestTranscriptContent(strings.Repeat("line\n", 100))
	m.setTranscriptYOffset(3)
	m.toolEntries[0].Collapsed = true
	m.toolHitRegions[0].Row = 3
	m.handleMouseClick(0, 0)
	if m.toolEntries[0].Collapsed {
		t.Fatal("scrolled row-zero tool card was not toggled")
	}
}

func TestViewEnablesCellMotionForTranscriptWheel(t *testing.T) {
	m := newTestModel(t)
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode = %v, want cell motion", got)
	}
}

func TestDenseToolCardHitRegionsNeverOverlap(t *testing.T) {
	m := newTestModel(t)
	m.toolEntries = []ToolEntry{{Collapsed: true}, {Collapsed: true}}
	m.toolHitRegions = []toolHitRegion{
		{ToolIndex: 0, Row: 4, EndCol: 12},
		{ToolIndex: 1, Row: 5, EndCol: 12},
	}

	m.handleMouseClick(0, 5)
	if !m.toolEntries[0].Collapsed || m.toolEntries[1].Collapsed {
		t.Fatalf("second dense receipt toggled the wrong card: %#v", m.toolEntries)
	}

	m.toolEntries[0].Collapsed = true
	m.toolEntries[1].Collapsed = true
	m.handleMouseClick(0, 6)
	if !m.toolEntries[0].Collapsed || !m.toolEntries[1].Collapsed {
		t.Fatal("clicking below an exact ToolCard header toggled a receipt")
	}
}

func TestToolCardMouseTargetsExcludeFooterAndRightPadding(t *testing.T) {
	m := newTestModel(t)
	m.toolEntries = []ToolEntry{{Collapsed: true}}
	m.viewport.SetHeight(5)
	m.toolHitRegions = []toolHitRegion{{ToolIndex: 0, Row: 5, EndCol: 8}}

	// Terminal row viewport.Height() is the divider immediately below the
	// transcript. It must never alias a scrolled transcript row.
	m.handleMouseClick(0, m.viewport.Height())
	if !m.toolEntries[0].Collapsed {
		t.Fatal("clicking the divider toggled an offscreen ToolCard")
	}

	m.toolHitRegions[0].Row = 0
	m.handleMouseClick(8, 0)
	if !m.toolEntries[0].Collapsed {
		t.Fatal("clicking right-side padding toggled a ToolCard")
	}
	m.handleMouseClick(7, 0)
	if m.toolEntries[0].Collapsed {
		t.Fatal("clicking inside the rendered ToolCard header did not toggle it")
	}
}

func TestModeReceiptDoesNotGrowViewBeyondTerminal(t *testing.T) {
	m := newTestModel(t)
	m.setMode(ModePlan)
	view := m.View().Content
	assertRenderedHeightFits(t, view, m.height)
}
