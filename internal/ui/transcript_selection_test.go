package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestTranscriptDragSelectCopiesOnRelease(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent("   hello world\n   second line")

	updated, cmd := m.Update(tea.MouseClickMsg{X: 3, Y: 0, Button: tea.MouseLeft})
	m = updated.(*Model)
	if cmd != nil {
		t.Fatal("press should not copy yet")
	}
	if !m.transcriptSel.dragging {
		t.Fatal("press did not start a drag")
	}

	updated, _ = m.Update(tea.MouseMotionMsg{X: 14, Y: 0, Button: tea.MouseLeft})
	m = updated.(*Model)
	if got := m.transcriptSelectionText(); got != "hello world" {
		t.Fatalf("drag text = %q, want %q", got, "hello world")
	}

	updated, cmd = m.Update(tea.MouseReleaseMsg{X: 14, Y: 0, Button: tea.MouseLeft})
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("release should copy the selection")
	}
	if m.transcriptSel.dragging {
		t.Fatal("release left dragging set")
	}
	if !m.transcriptSel.active {
		t.Fatal("release cleared the highlight")
	}
}

func TestTranscriptDragSelectCopiesWrappedRows(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent("   hello world\n   second line")

	_, _ = m.Update(tea.MouseClickMsg{X: 3, Y: 0, Button: tea.MouseLeft})
	updated, _ := m.Update(tea.MouseMotionMsg{X: 9, Y: 1, Button: tea.MouseLeft})
	m = updated.(*Model)
	got := m.transcriptSelectionText()
	if got != "hello world\nsecond" {
		t.Fatalf("wrapped text = %q", got)
	}
}

func TestTranscriptClickWithoutDragDoesNotCopy(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent("   hello world")

	_, _ = m.Update(tea.MouseClickMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	updated, cmd := m.Update(tea.MouseReleaseMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	m = updated.(*Model)
	if cmd != nil {
		t.Fatal("click without drag copied")
	}
	if m.transcriptSel.active {
		t.Fatal("click without drag left a selection")
	}
}

func TestTranscriptClickOnToolTogglesInsteadOfSelecting(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent("   tool header")
	m.toolEntries = []ToolEntry{{Collapsed: true}}
	m.toolHitRegions = []toolHitRegion{{ToolIndex: 0, Row: 0, EndCol: 12}}

	updated, _ := m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	m = updated.(*Model)
	if m.toolEntries[0].Collapsed {
		t.Fatal("tool click did not toggle")
	}
	if m.transcriptSel.active {
		t.Fatal("tool click started a selection")
	}
}

func TestTranscriptDoubleClickSelectsWord(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent("   hello world")

	click := tea.MouseClickMsg{X: 5, Y: 0, Button: tea.MouseLeft}
	_, _ = m.Update(click)
	updated, _ := m.Update(click)
	m = updated.(*Model)
	if m.transcriptSel.clickN != 2 {
		t.Fatalf("clickN = %d, want 2", m.transcriptSel.clickN)
	}
	if got := strings.TrimSpace(m.transcriptSelectionText()); got != "hello" {
		t.Fatalf("word = %q, want hello", got)
	}
}

func TestTranscriptTripleClickSelectsLine(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent("   hello world")

	click := tea.MouseClickMsg{X: 5, Y: 0, Button: tea.MouseLeft}
	_, _ = m.Update(click)
	_, _ = m.Update(click)
	updated, _ := m.Update(click)
	m = updated.(*Model)
	if m.transcriptSel.clickN != 3 {
		t.Fatalf("clickN = %d, want 3", m.transcriptSel.clickN)
	}
	if got := strings.TrimSpace(m.transcriptSelectionText()); got != "hello world" {
		t.Fatalf("line = %q, want hello world", got)
	}
}

func TestTranscriptEscapeClearsSelection(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent("   hello world")
	_, _ = m.Update(tea.MouseClickMsg{X: 3, Y: 0, Button: tea.MouseLeft})
	updated, _ := m.Update(tea.MouseMotionMsg{X: 14, Y: 0, Button: tea.MouseLeft})
	m = updated.(*Model)
	if m.transcriptSel.empty() {
		t.Fatal("expected an active selection")
	}

	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.transcriptSel.active {
		t.Fatal("escape did not clear the selection")
	}
}

func TestCopyLastPrefersActiveSelection(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent("   hello world")
	m.entries = []ChatEntry{{Kind: "assistant", Content: "last answer"}}
	m.input.SetValue("draft kept")
	_, _ = m.Update(tea.MouseClickMsg{X: 3, Y: 0, Button: tea.MouseLeft})
	updated, _ := m.Update(tea.MouseMotionMsg{X: 8, Y: 0, Button: tea.MouseLeft})
	m = updated.(*Model)

	_, cmd := m.Update(ctrlKey('y'))
	if cmd == nil {
		t.Fatal("ctrl+y should copy the selection even with a draft")
	}
}

func TestTranscriptSelectionHighlightUsesReverse(t *testing.T) {
	m := newTestModel(t)
	m.transcriptSel = transcriptSelection{
		active:   true,
		startRow: 0,
		startCol: 3,
		endRow:   0,
		endCol:   8,
	}
	rows := []string{"   hello world"}
	m.styleTranscriptSelectionWindowRows(rows, 0)
	if rows[0] == "   hello world" {
		t.Fatal("selection did not restyle the intersecting span")
	}
	if !strings.Contains(rows[0], "hello") {
		t.Fatalf("styled row lost the selected word: %q", ansi.Strip(rows[0]))
	}
	if ansi.Strip(rows[0]) != "   hello world" {
		t.Fatalf("strip = %q", ansi.Strip(rows[0]))
	}
}

func TestOverlayClickDoesNotStartTranscriptSelection(t *testing.T) {
	m := newTestModel(t)
	m.setTestTranscriptContent("   hello world")
	m.overlay = OverlayHelp

	updated, _ := m.Update(tea.MouseClickMsg{X: 3, Y: 0, Button: tea.MouseLeft})
	m = updated.(*Model)
	if m.transcriptSel.active {
		t.Fatal("overlay click started a transcript selection")
	}
}
