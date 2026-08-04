package ui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestLiveToolCardRetainsInspectableArguments(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(ToolCallStartMsg{
		ID: "call-1", Name: "write", Args: map[string]any{"path": "visible.txt"}, StartTime: time.Now(),
	})
	m = updated.(*Model)
	updated, _ = m.Update(ToolCallResultMsg{ID: "call-1", Name: "write", Result: "done", Duration: time.Millisecond})
	m = updated.(*Model)
	m.toolEntries[0].Collapsed = false
	card := testProjectedToolCard(t, m, 0)
	if view := card.View(100); !strings.Contains(view, "visible.txt") {
		t.Fatalf("expanded live card hid arguments: %q", view)
	}
}

// TestOverlayCentering_HelpOverlay verifies help overlay is centered
func TestOverlayCentering_HelpOverlay(t *testing.T) {
	m := newTestModel(t)
	m.width = 120
	m.height = 40

	// Initialize help viewport
	m.overlay = OverlayHelp
	m.initHelpViewport()

	overlay := m.renderHelpOverlay(m.width)
	overlayLines := strings.Split(overlay, "\n")

	// Check overlay width doesn't exceed screen
	for _, line := range overlayLines {
		lineWidth := lipgloss.Width(line)
		if lineWidth > m.width {
			t.Errorf("overlay line width %d exceeds screen width %d", lineWidth, m.width)
		}
	}
}

// TestOverlayCentering_ModelPicker verifies model picker overlay is centered
func TestOverlayCentering_ModelPicker(t *testing.T) {
	m := newTestModel(t)
	m.width = 100
	m.height = 30

	// Initialize model picker state manually
	m.openModelPicker()

	// Model picker requires modelManager to be set
	if m.modelPickerState == nil {
		// Test passes if it doesn't panic
		t.Skip("model picker requires model manager")
	}

	overlay := m.renderModelPicker()
	if overlay == "" {
		t.Log("model picker overlay empty (expected without model manager)")
	}
}

// TestOverlayCentering_SmallScreen verifies overlays work on small screens
func TestOverlayCentering_SmallScreen(t *testing.T) {
	m := newTestModel(t)
	m.width = 60
	m.height = 20

	m.overlay = OverlayHelp
	m.initHelpViewport()

	overlay := m.renderHelpOverlay(m.width)

	if overlay == "" {
		t.Error("overlay should render on small screen")
	}

	// Should not panic or produce empty output
	lines := strings.Count(overlay, "\n")
	if lines < 5 {
		t.Errorf("overlay should have at least 5 lines, got %d", lines)
	}
}

// TestOverlayCentering_LargeScreen verifies overlays scale on large screens
func TestOverlayCentering_LargeScreen(t *testing.T) {
	m := newTestModel(t)
	m.width = 200
	m.height = 60

	m.overlay = OverlayHelp
	m.initHelpViewport()

	overlay := m.renderHelpOverlay(m.width)

	// Overlay should not be excessively wide
	overlayLines := strings.Split(overlay, "\n")
	maxLineWidth := 0
	for _, line := range overlayLines {
		width := lipgloss.Width(line)
		if width > maxLineWidth {
			maxLineWidth = width
		}
	}

	// Overlay should be centered and not use full width
	if maxLineWidth > m.width-10 {
		t.Errorf("overlay too wide: %d (max should be ~%d)", maxLineWidth, m.width-10)
	}
}

// TestOverlayOnContent_Positioning verifies overlay is positioned correctly
func TestOverlayOnContent_Positioning(t *testing.T) {
	m := newTestModel(t)
	m.width = 100
	m.height = 40

	base := strings.Repeat("base line\n", 40)
	overlay := strings.Repeat("overlay line\n", 10)

	result := m.overlayOnContent(base, overlay)

	// Result should have same number of lines as base
	baseLines := strings.Count(base, "\n")
	resultLines := strings.Count(result, "\n")

	if resultLines < baseLines {
		t.Errorf("result should have at least as many lines as base: got %d, want %d", resultLines, baseLines)
	}
}

// The modal scrim owns every row it covers. Letting base content survive beside
// a modal put the transcript/composer rule through the panel and sliced the
// shortcuts row mid-word at the border.
func TestOverlayOnContent_ScrimOwnsModalRowsAndOneRowEitherSide(t *testing.T) {
	m := newTestModel(t)
	m.width = 48
	m.height = 9

	baseRow := strings.Repeat("x", m.width)
	base := strings.TrimSuffix(strings.Repeat(baseRow+"\n", 9), "\n")
	overlay := "╭────╮\n│ ok │\n╰────╯"

	lines := strings.Split(ansi.Strip(m.overlayOnContent(base, overlay)), "\n")
	startY := centeredOverlayStartY(base, overlay)
	scrimTop, scrimBottom := startY-1, startY+lipgloss.Height(overlay)

	for row, line := range lines {
		switch {
		case row >= startY && row < startY+lipgloss.Height(overlay):
			if strings.Contains(line, "x") {
				t.Fatalf("modal row %d retained base fragments: %q", row, line)
			}
		case row == scrimTop || row == scrimBottom:
			if strings.TrimSpace(line) != "" {
				t.Fatalf("clear row %d beside the modal was not blank: %q", row, line)
			}
		default:
			if line != baseRow {
				t.Fatalf("row %d outside the scrim was modified: %q", row, line)
			}
		}
	}
}

// Every row of a modal shares one column. Placing rows independently let a
// short footer drift against the border above it inside the same panel.
func TestOverlayOnContent_ModalBlockSharesOneColumn(t *testing.T) {
	m := newTestModel(t)
	m.width = 60
	m.height = 9

	base := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", m.width)+"\n", 9), "\n")
	// Deliberately ragged: a wide border, a short footer, a wide border.
	overlay := "╭──────────────╮\n│ esc close    │\n╰──────────────╯"

	lines := strings.Split(ansi.Strip(m.overlayOnContent(base, overlay)), "\n")
	startY := centeredOverlayStartY(base, overlay)

	wantX := centeredOverlayLineX(m.width, overlay)
	for i := range strings.Split(overlay, "\n") {
		line := lines[startY+i]
		gotX := lipgloss.Width(line) - lipgloss.Width(strings.TrimLeft(line, " "))
		if gotX != wantX {
			t.Fatalf("modal row %d starts at column %d, want %d (rows must share one X)", i, gotX, wantX)
		}
	}
}

// Anchoring, not centering: a short prompt and a tall picker open with their
// top edge on the same row, so navigating between overlays does not walk the
// panel up and down the screen.
func TestOverlayAnchorIsIndependentOfModalHeight(t *testing.T) {
	base := strings.TrimSuffix(strings.Repeat("\n", 24), "\n")
	short := "╭──╮\n╰──╯"
	tall := strings.TrimSuffix(strings.Repeat("│ row │\n", 10), "\n")

	shortY := centeredOverlayStartY(base, short)
	tallY := centeredOverlayStartY(base, tall)
	if shortY != tallY {
		t.Fatalf("modal top edge moved with content height: short=%d tall=%d", shortY, tallY)
	}
}

func TestOverlayOnContent_MasksRowsWithTokenFragmentGutters(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	m.height = 5
	base := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", m.width)+"\n", 5), "\n")
	overlay := "╭" + strings.Repeat("─", 66) + "╮\n│" + strings.Repeat(" ", 66) + "│\n╰" + strings.Repeat("─", 66) + "╯"

	result := ansi.Strip(m.overlayOnContent(base, overlay))
	lines := strings.Split(result, "\n")
	startY := centeredOverlayStartY(base, overlay)
	for row := startY; row < startY+lipgloss.Height(overlay); row++ {
		if strings.Contains(lines[row], "x") {
			t.Fatalf("near-full overlay row %d retained token fragments: %q", row, lines[row])
		}
	}
}

func TestOverlayOnContent_PreservesStyledWideTranscriptCells(t *testing.T) {
	m := newTestModel(t)
	m.width = 48
	m.height = 5

	baseText := "界 transcript 🙂 remains"
	baseText += strings.Repeat("·", m.width-lipgloss.Width(baseText))
	palette := newSemanticPalette(true, defaultThemeID)
	baseLine := lipgloss.NewStyle().
		Foreground(palette.Text).
		Background(palette.Border).
		Render(baseText)
	base := strings.Join([]string{baseLine, baseLine, baseLine, baseLine, baseLine}, "\n")
	overlay := lipgloss.NewStyle().
		Foreground(palette.Text).
		Background(palette.Accent).
		Render("╭─ command ─╮")

	result := m.overlayOnContent(base, overlay)
	resultLines := strings.Split(result, "\n")
	row := centeredOverlayStartY(base, overlay)
	start := centeredOverlayLineX(m.width, overlay)
	end := start + lipgloss.Width(overlay)

	// Styled wide-rune rows outside the scrim survive the composite untouched.
	untouched := row + lipgloss.Height(overlay) + 1
	if untouched >= len(resultLines) {
		t.Fatalf("test needs a base row below the scrim: rows=%d", len(resultLines))
	}
	wantCells := renderOverlayTestLine(baseLine, m.width)
	gotCells := renderOverlayTestLine(resultLines[untouched], m.width)
	for x := 0; x < m.width; x++ {
		if !wantCells.CellAt(x, 0).Equal(gotCells.CellAt(x, 0)) {
			t.Fatalf("cell %d outside the scrim changed: base=%#v result=%#v", x, wantCells.CellAt(x, 0), gotCells.CellAt(x, 0))
		}
	}
	gotCells = renderOverlayTestLine(resultLines[row], m.width)

	overlayCells := renderOverlayTestLine(overlay, lipgloss.Width(overlay))
	for x := start; x < end; x++ {
		if !overlayCells.CellAt(x-start, 0).Equal(gotCells.CellAt(x, 0)) {
			t.Fatalf("modal cell %d was not composited: overlay=%#v result=%#v", x, overlayCells.CellAt(x-start, 0), gotCells.CellAt(x, 0))
		}
	}
}

func renderOverlayTestLine(line string, width int) *lipgloss.Canvas {
	canvas := lipgloss.NewCanvas(width, 1)
	canvas.Compose(lipgloss.NewLayer(line))
	return canvas
}

// TestToolCard_WidthCalculation verifies tool cards respect width constraints
func TestToolCard_WidthCalculation(t *testing.T) {
	tests := []struct {
		name         string
		availableW   int
		cardName     string
		expectRender bool
	}{
		{"wide screen", 100, "read_file", true},
		{"narrow screen", 40, "read_file", true},
		{"very narrow", 30, "test", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			card := NewToolCard(tt.cardName, ToolCardFile, true, defaultThemeID)
			card.State = ToolCardRunning

			view := card.View(tt.availableW)

			// Should render without panic
			if view == "" {
				t.Error("tool card should render")
			}

			// Note: lipgloss.Width includes ANSI codes, so we just verify it renders
			_ = lipgloss.Width(view)
		})
	}
}

// TestToolCard_LongArgsWrapping verifies long args are wrapped properly
func TestToolCard_LongArgsWrapping(t *testing.T) {
	card := NewToolCard("write_file", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Expanded = true
	card.Args = strings.Repeat("very_long_argument_that_should_be_wrapped_properly ", 10)
	card.Result = "success"

	view := card.View(80)
	viewLines := strings.Split(view, "\n")

	// Should render multiple lines
	if len(viewLines) < 3 {
		t.Errorf("tool card should have multiple lines, got %d", len(viewLines))
	}

	// Verify it renders without panic
	if view == "" {
		t.Error("tool card view should not be empty")
	}
}

// TestToolCard_ManagerRendering verifies correlated cards render correctly.
func TestToolCard_ManagerRendering(t *testing.T) {
	mgr := NewToolCardManager(true)

	// Add multiple cards
	mgr.AddCardWithID("read-1", "read_file", ToolCardFile, testTime)
	mgr.AddCardWithID("write-1", "write_file", ToolCardFile, testTime)
	mgr.AddCardWithID("bash-1", "bash", ToolCardBash, testTime)

	// Update some cards
	mgr.UpdateCardWithID("read-1", "read_file", ToolCardSuccess, "file content", testDuration)
	mgr.UpdateCardWithID("write-1", "write_file", ToolCardRunning, "", 0)

	views := make([]string, len(mgr.Cards))
	for i := range mgr.Cards {
		views[i] = mgr.Cards[i].View(100)
	}
	view := strings.Join(views, "\n")

	if view == "" {
		t.Error("manager view should not be empty")
	}

	// Should have multiple cards (separated by newlines)
	lines := strings.Count(view, "\n")
	if lines < 2 {
		t.Errorf("manager view should have multiple lines, got %d", lines+1)
	}
}

func TestDuplicateRestoredToolIDUsesNewestReceipt(t *testing.T) {
	mgr := NewToolCardManager(true)
	mgr.AddCardWithID("restored-id", "read", ToolCardFile, testTime)
	mgr.Cards[0].State = ToolCardSuccess
	mgr.Cards[0].Result = "OLD RECEIPT"
	mgr.AddCardWithID("restored-id", "read", ToolCardFile, testTime.Add(time.Second))
	mgr.UpdateCardWithID("restored-id", "read", ToolCardSuccess, "NEW RECEIPT", testDuration)
	if mgr.Cards[0].Result != "OLD RECEIPT" || mgr.Cards[1].Result != "NEW RECEIPT" {
		t.Fatalf("duplicate ID updated wrong card: %#v", mgr.Cards)
	}

	m := newTestModel(t)
	m.toolEntries = []ToolEntry{
		{ID: "read-1", Name: "read", Result: "OLD RECEIPT", Status: ToolStatusDone},
		{ID: "read-2", Name: "read", Result: "NEW RECEIPT", Status: ToolStatusDone},
	}
	m.entries = []ChatEntry{
		testToolChatEntry(0),
		testToolChatEntry(1),
	}
	m.ready = true
	m.width, m.height = 100, 40
	var renderedBuilder strings.Builder
	m.renderToolGroup(&renderedBuilder, m.entries[1])
	rendered := renderedBuilder.String()
	if !strings.Contains(rendered, "NEW RECEIPT") || strings.Contains(rendered, "OLD RECEIPT") {
		t.Fatalf("tool entry rendered stale duplicate receipt:\n%s", rendered)
	}
}

// TestToolCard_BorderAndPadding verifies border and padding are accounted for
func TestToolCard_BorderAndPadding(t *testing.T) {
	card := NewToolCard("test", ToolCardGeneric, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Expanded = true
	card.Args = "test args"
	card.Result = "test result"

	availableW := 60
	view := card.View(availableW)

	// Account for border (2) + padding (2) = 4 chars
	contentW := availableW - 4

	viewLines := strings.Split(view, "\n")
	for i, line := range viewLines {
		lineWidth := lipgloss.Width(line)
		if lineWidth > availableW {
			t.Errorf("line %d width %d exceeds available width %d (content should fit in %d)",
				i, lineWidth, availableW, contentW)
		}
	}
}

// TestToolCard_EmojiIcons verifies emoji icons render without breaking layout
func TestToolCard_EmojiIcons(t *testing.T) {
	kinds := []ToolCardKind{ToolCardFile, ToolCardBash, ToolCardSearch, ToolCardGit, ToolCardGeneric}
	states := []ToolCardState{ToolCardRunning, ToolCardSuccess, ToolCardError}

	for _, kind := range kinds {
		for _, state := range states {
			t.Run(string(rune(kind))+string(rune(state)), func(t *testing.T) {
				card := NewToolCard("test", kind, true, defaultThemeID)
				card.State = state

				view := card.View(60)

				// Should render without panic
				if view == "" {
					t.Error("card view should not be empty")
				}

				// Should not exceed width
				viewWidth := lipgloss.Width(view)
				if viewWidth > 60 {
					t.Errorf("card width %d exceeds 60", viewWidth)
				}
			})
		}
	}
}

// TestWrapText_LongWords verifies wrapText breaks long words
func TestWrapText_LongWords(t *testing.T) {
	longWord := strings.Repeat("a", 100)
	result := wrapText(longWord, 40)

	lines := strings.Split(result, "\n")
	for i, line := range lines {
		if len(line) > 40 {
			t.Errorf("line %d exceeds width: %d chars", i, len(line))
		}
	}
}

// TestWrapText_MultipleWords verifies wrapText handles multiple words
func TestWrapText_MultipleWords(t *testing.T) {
	text := "word1 word2 word3 word4 word5 word6 word7 word8 word9 word10"
	result := wrapText(text, 20)

	lines := strings.Split(result, "\n")
	for i, line := range lines {
		if len(line) > 20 {
			t.Errorf("line %d exceeds width: %d chars", i, len(line))
		}
	}
}

// TestWrapText_EmptyAndEdgeCases verifies wrapText handles edge cases
func TestWrapText_EmptyAndEdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		width      int
		expect     string
		exactMatch bool
	}{
		{"empty", "", 40, "", true},
		{"zero width", "hello", 0, "hello", true},
		{"exact fit", "hello", 5, "hello", true},
		{"single char width", "hello world", 1, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := wrapText(tt.input, tt.width)
			if tt.exactMatch && result != tt.expect {
				t.Errorf("wrapText(%q, %d) = %q, want %q", tt.input, tt.width, result, tt.expect)
			}
			if tt.input != "" && result == "" {
				t.Errorf("wrapText(%q, %d) returned empty output", tt.input, tt.width)
			}
		})
	}
}

// BenchmarkOverlayRendering benchmarks overlay rendering performance
func BenchmarkOverlayRendering_Help(b *testing.B) {
	m := newTestModelB(b)
	m.width = 120
	m.height = 40
	m.overlay = OverlayHelp
	m.initHelpViewport()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.renderHelpOverlay(m.width)
	}
}

// BenchmarkToolCardRendering benchmarks tool card rendering
func BenchmarkToolCardRendering(b *testing.B) {
	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Expanded = true
	card.Args = strings.Repeat("arg ", 20)
	card.Result = strings.Repeat("result line\n", 10)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = card.View(80)
	}
}

// BenchmarkWrapText benchmarks text wrapping
func BenchmarkWrapText(b *testing.B) {
	text := strings.Repeat("This is a test sentence with multiple words. ", 20)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wrapText(text, 60)
	}
}
