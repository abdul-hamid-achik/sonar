package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestSemanticTranscriptLineMapUsesGraphemeCoordinates(t *testing.T) {
	lineMap := semanticTranscriptLineMap("│ e\u0301🙂\n│ next")
	if len(lineMap) != 2 {
		t.Fatalf("line map length = %d, want 2", len(lineMap))
	}
	if lineMap[0] != (TranscriptLinePoint{LogicalOffset: 0, Row: 0}) {
		t.Fatalf("first point = %+v", lineMap[0])
	}
	// "e\u0301" is one grapheme and 🙂 is one grapheme. The separator advances
	// one additional logical coordinate.
	if lineMap[1] != (TranscriptLinePoint{LogicalOffset: 3, Row: 1}) {
		t.Fatalf("second point = %+v, want grapheme offset 3", lineMap[1])
	}
}

func TestRenderEntriesPublishesContiguousIdentityLayout(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.ready = true
	m.entries = []ChatEntry{
		{Kind: "user", Content: "inspect the renderer"},
		{Kind: "assistant", Content: "I will inspect the semantic layout."},
		{Kind: "system", Content: "Provider · local"},
	}

	rendered := m.renderEntries()
	snapshot := m.transcriptLayout
	if snapshot.SessionID != max(int64(0), m.sessionID) {
		t.Fatalf("layout session = %d, want %d", snapshot.SessionID, m.sessionID)
	}
	if len(snapshot.Records) != len(m.entries) {
		t.Fatalf("layout records = %d, want %d", len(snapshot.Records), len(m.entries))
	}
	_, totalHeight, err := indexCurrentTranscriptLayout(snapshot.Records)
	if err != nil {
		t.Fatalf("published layout is invalid: %v", err)
	}
	if totalHeight != lipgloss.Height(rendered) {
		t.Fatalf("layout height = %d, rendered height = %d", totalHeight, lipgloss.Height(rendered))
	}
	for index, record := range snapshot.Records {
		if record.BlockID != m.entries[index].BlockID || record.Revision != m.entries[index].Revision {
			t.Fatalf("record %d identity = %+v, entry = %+v", index, record, m.entries[index])
		}
		if record.Exact {
			t.Fatalf("record %d unexpectedly claims exact Markdown mapping", index)
		}
	}
}

func TestWidthReflowKeepsPausedBlockAtItsScreenRow(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.handleWindowSize(tea.WindowSizeMsg{Width: 88, Height: 14}, nil)
	for index := 0; index < 10; index++ {
		kind := "assistant"
		if index%2 == 0 {
			kind = "user"
		}
		m.entries = append(m.entries, ChatEntry{
			Kind:    kind,
			Content: strings.Repeat("semantic anchor content ", 8),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()

	anchored := m.transcriptLayout.Records[4]
	m.setTranscriptYOffset(anchored.StartRow)
	m.pauseFollow()

	m.handleWindowSize(tea.WindowSizeMsg{Width: 46, Height: 14}, nil)

	var current TranscriptLayoutRecord
	found := false
	for _, record := range m.transcriptLayout.Records {
		if record.BlockID == anchored.BlockID {
			current, found = record, true
			break
		}
	}
	if !found {
		t.Fatalf("anchored block %q disappeared after width reflow", anchored.BlockID)
	}
	if m.anchorActive || !m.userScrolledUp {
		t.Fatal("width reflow stole paused-follow ownership")
	}
	if screenRow := current.StartRow - m.transcriptYOffset(); screenRow != 0 {
		t.Fatalf("anchored block screen row = %d, want 0 (start=%d offset=%d)",
			screenRow, current.StartRow, m.transcriptYOffset())
	}
}

func TestSemanticAnchorSurvivesInsertionBeforeBlock(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.handleWindowSize(tea.WindowSizeMsg{Width: 72, Height: 12}, nil)
	for index := 0; index < 8; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: strings.Repeat("stable identity ", 6),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	anchored := m.transcriptLayout.Records[3]
	m.setTranscriptYOffset(anchored.StartRow)
	m.pauseFollow()
	capture := m.captureTranscriptReflowAnchor()

	m.entries = append([]ChatEntry{{Kind: "system", Content: "inserted before history"}}, m.entries...)
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.restoreTranscriptReflowAnchor(capture)

	for _, record := range m.transcriptLayout.Records {
		if record.BlockID != anchored.BlockID {
			continue
		}
		if screenRow := record.StartRow - m.transcriptYOffset(); screenRow != 0 {
			t.Fatalf("inserted block moved semantic anchor to screen row %d", screenRow)
		}
		return
	}
	t.Fatalf("anchored block %q not found after insertion", anchored.BlockID)
}

func TestToolDisclosureBeforeViewportPreservesSemanticReadingPosition(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.handleWindowSize(tea.WindowSizeMsg{Width: 64, Height: 12}, nil)
	m.toolEntries = []ToolEntry{{
		ID: "tool-1", Name: "bash", Result: strings.Repeat("output row\n", 20),
		Status: ToolStatusDone, Collapsed: true,
	}}
	m.entries = []ChatEntry{{Kind: "tool_group", ToolIndex: 0}}
	for index := 0; index < 7; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind: "assistant", Content: strings.Repeat("reader block ", 7),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	anchored := m.transcriptLayout.Records[4]
	m.setTranscriptYOffset(anchored.StartRow)
	m.pauseFollow()

	m.toggleToolReceipt(0, false)

	for _, record := range m.transcriptLayout.Records {
		if record.BlockID != anchored.BlockID {
			continue
		}
		if screenRow := record.StartRow - m.transcriptYOffset(); screenRow != 0 {
			t.Fatalf("tool expansion moved anchored block to screen row %d", screenRow)
		}
		if !m.followPaused() {
			t.Fatal("tool expansion resumed follow for a paused reader")
		}
		return
	}
	t.Fatalf("anchored block %q disappeared after tool expansion", anchored.BlockID)
}
