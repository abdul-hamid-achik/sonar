package ui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var transcriptPaintMarkerPattern = regexp.MustCompile(`MARK[0-9]{3}`)

func newTranscriptPaintRegressionModel(t *testing.T, width, height, entries int) *Model {
	t.Helper()

	m := newTestModel(t)
	m.handleWindowSize(tea.WindowSizeMsg{Width: width, Height: height}, nil)
	m.entries = nil
	for index := 0; index < entries; index++ {
		kind := "user"
		if index%3 == 1 {
			kind = "assistant"
		}
		m.entries = append(m.entries, ChatEntry{
			Kind: kind,
			Content: fmt.Sprintf(
				"regression %03d · 你 · e\u0301\n%s",
				index,
				strings.Repeat("wrapped cells ", index%7+1),
			),
		})
	}
	m.invalidateEntryCache()
	m.pauseFollow()
	m.transcriptPaint.top = 0
	m.refreshTranscript()
	return m
}

func newMarkedStreamingTranscript(t *testing.T, prompt string) *Model {
	t.Helper()

	m := newTestModel(t)
	m.handleWindowSize(tea.WindowSizeMsg{Width: 54, Height: 12}, nil)
	m.entries = []ChatEntry{{Kind: "user", Content: prompt}}

	var response strings.Builder
	for index := 0; index < 500; index++ {
		fmt.Fprintf(
			&response,
			"[MARK%03d](https://example.invalid/a/very/long/path/%03d/abcdefghijk) ",
			index,
			index,
		)
	}
	m.state = StateStreaming
	m.streamBuf.WriteString(response.String())
	m.invalidateEntryCache()
	m.refreshTranscript()

	live := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1]
	m.setTranscriptYOffset(live.StartRow + 120)
	m.pauseFollow()
	return m
}

func visibleTranscriptMarker(t *testing.T, m *Model) (string, int, string) {
	t.Helper()

	view := ansi.Strip(m.viewport.View())
	location := transcriptPaintMarkerPattern.FindStringIndex(view)
	if location == nil {
		t.Fatalf("fixture has no visible semantic marker:\n%s", view)
	}
	return view[location[0]:location[1]], strings.Count(view[:location[0]], "\n"), view
}

func assertTranscriptMarkerAtRow(
	t *testing.T,
	m *Model,
	marker string,
	wantRow int,
	beforeView string,
) {
	t.Helper()

	afterView := ansi.Strip(m.viewport.View())
	location := strings.Index(afterView, marker)
	if location < 0 {
		t.Fatalf(
			"transcript lost visible semantic marker %s:\n--- before ---\n%s\n--- after ---\n%s",
			marker,
			beforeView,
			afterView,
		)
	}
	if gotRow := strings.Count(afterView[:location], "\n"); gotRow != wantRow {
		t.Fatalf(
			"semantic marker %s moved from screen row %d to %d",
			marker,
			wantRow,
			gotRow,
		)
	}
}

func transcriptBlockScreenRow(t *testing.T, m *Model, blockID BlockID) int {
	t.Helper()

	for _, record := range m.transcriptLayout.Records {
		if record.BlockID == blockID {
			return record.StartRow - m.transcriptYOffset()
		}
	}
	t.Fatalf("semantic block %s disappeared", blockID)
	return 0
}

func TestTranscriptPaintHeightGrowthPreservesPausedFollowIntent(t *testing.T) {
	m := newTranscriptPaintRegressionModel(t, 60, 12, 10)
	m.setTranscriptYOffset(max(1, m.transcriptMaxTop()/2))
	m.pauseFollow()
	if m.transcriptAtBottom() {
		t.Fatal("fixture did not start above bottom")
	}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 200})
	m = updated.(*Model)
	if !m.followPaused() {
		t.Fatalf(
			"height growth resumed follow without user intent: top=%d max=%d",
			m.transcriptYOffset(),
			m.transcriptMaxTop(),
		)
	}

	m.state = StateStreaming
	beforeTop := m.transcriptYOffset()
	m.streamBuf.WriteString(strings.Repeat("new output row\n", 300))
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
	if got := m.transcriptYOffset(); got != beforeTop {
		t.Fatalf("new output snapped formerly paused reader %d -> %d", beforeTop, got)
	}
}

func TestTranscriptPaintHeightOnlyResizeReflowsExpandedDiffBudget(t *testing.T) {
	m := newTestModel(t)
	m.handleWindowSize(tea.WindowSizeMsg{Width: 80, Height: 44}, nil)
	lines := make([]DiffLine, 0, 80)
	for index := 0; index < 80; index++ {
		lines = append(lines, DiffLine{
			Kind:    DiffAdded,
			Content: fmt.Sprintf("height-sensitive diff row %02d", index),
			NewLine: index + 1,
		})
	}
	m.toolEntries = []ToolEntry{{
		ID:        "height-diff",
		Name:      "write_file",
		Summary:   "updated file",
		Status:    ToolStatusDone,
		Collapsed: false,
		DiffLines: lines,
	}}
	m.entries = []ChatEntry{
		// Multi-line so sticky strip does not omit this block from paint.
		{Kind: "user", Content: "show the expanded patch\nfull diff please"},
		{Kind: "tool_group", ToolIndex: 0},
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	largeBudget := m.inlineDiffPreviewRows()
	largeBlock := m.transcriptPaint.document.base[1].content

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 14})
	m = updated.(*Model)
	smallBudget := m.inlineDiffPreviewRows()
	smallBlock := m.transcriptPaint.document.base[1].content

	if smallBudget >= largeBudget {
		t.Fatalf("height-only resize kept diff budget %d, previous %d", smallBudget, largeBudget)
	}
	if lipgloss.Height(smallBlock) >= lipgloss.Height(largeBlock) {
		t.Fatalf(
			"height-only resize kept stale expanded diff height %d, previous %d:\n--- large ---\n%s\n--- small ---\n%s",
			lipgloss.Height(smallBlock),
			lipgloss.Height(largeBlock),
			ansi.Strip(largeBlock),
			ansi.Strip(smallBlock),
		)
	}
}

func TestTranscriptPaintAgentSettlementPreservesVisibleSemanticMarker(t *testing.T) {
	m := newMarkedStreamingTranscript(t, "produce the marked response")
	marker, screenRow, beforeView := visibleTranscriptMarker(t, m)

	updated, _ := m.Update(AgentDoneMsg{})
	m = updated.(*Model)
	if !m.followPaused() {
		t.Fatal("agent settlement resumed follow")
	}
	assertTranscriptMarkerAtRow(t, m, marker, screenRow, beforeView)
}

func TestTranscriptPaintToolBoundaryPreservesVisibleSemanticMarker(t *testing.T) {
	m := newMarkedStreamingTranscript(t, "produce the marked response then call a tool")
	marker, screenRow, beforeView := visibleTranscriptMarker(t, m)

	updated, _ := m.Update(ToolCallStartMsg{
		ID:        "tool-boundary",
		Name:      "read_file",
		Args:      map[string]any{"path": "README.md"},
		StartTime: testTime,
	})
	m = updated.(*Model)
	if !m.followPaused() {
		t.Fatal("tool boundary resumed follow")
	}
	assertTranscriptMarkerAtRow(t, m, marker, screenRow, beforeView)
}

func TestTranscriptPaintAsyncToolResultPreservesSemanticBlock(t *testing.T) {
	m := newTestModel(t)
	m.handleWindowSize(tea.WindowSizeMsg{Width: 64, Height: 12}, nil)
	m.toolEntries = []ToolEntry{{
		ID:        "async-tool",
		Name:      "bash",
		Summary:   "running command",
		Status:    ToolStatusRunning,
		StartTime: testTime,
		Collapsed: false,
	}}
	m.entries = []ChatEntry{{Kind: "tool_group", ToolIndex: 0}}
	for index := 0; index < 30; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("ANCHOR-BLOCK-%02d %s", index, strings.Repeat("reader ", 8)),
		})
	}
	m.toolsPending = 1
	m.state = StateStreaming
	m.invalidateEntryCache()
	m.refreshTranscript()

	anchor := m.transcriptLayout.Records[12]
	m.setTranscriptYOffset(anchor.StartRow)
	m.pauseFollow()
	wantScreenRow := transcriptBlockScreenRow(t, m, anchor.BlockID)

	var result strings.Builder
	for index := 0; index < 40; index++ {
		fmt.Fprintf(&result, "result row %02d with bounded output\n", index)
	}
	updated, _ := m.Update(ToolCallResultMsg{
		ID:       "async-tool",
		Name:     "bash",
		Result:   result.String(),
		Duration: 2 * testDuration,
	})
	m = updated.(*Model)
	if !m.followPaused() {
		t.Fatal("async tool completion resumed follow")
	}
	if got := transcriptBlockScreenRow(t, m, anchor.BlockID); got != wantScreenRow {
		t.Fatalf(
			"async tool completion above viewport moved semantic block from screen row %d to %d",
			wantScreenRow,
			got,
		)
	}
}

func TestTranscriptPaintAsyncDiffResultPreservesSemanticBlock(t *testing.T) {
	m := newTestModel(t)
	m.handleWindowSize(tea.WindowSizeMsg{Width: 80, Height: 14}, nil)
	m.toolEntries = []ToolEntry{{
		ID:             "async-diff",
		Name:           "write_file",
		Summary:        "updated file",
		Status:         ToolStatusDone,
		Collapsed:      false,
		DiffPending:    true,
		DiffGeneration: 7,
	}}
	m.entries = []ChatEntry{{Kind: "tool_group", ToolIndex: 0}}
	for index := 0; index < 30; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("DIFF-ANCHOR-%02d %s", index, strings.Repeat("reader ", 8)),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()

	anchor := m.transcriptLayout.Records[12]
	m.setTranscriptYOffset(anchor.StartRow)
	m.pauseFollow()
	wantScreenRow := transcriptBlockScreenRow(t, m, anchor.BlockID)

	lines := make([]DiffLine, 0, 40)
	for index := 0; index < 40; index++ {
		lines = append(lines, DiffLine{
			Kind:    DiffAdded,
			Content: fmt.Sprintf("added row %02d with content", index),
			NewLine: index + 1,
		})
	}
	updated, _ := m.Update(diffBuildResultMsg{
		Generation: 7,
		ToolID:     "async-diff",
		ToolName:   "write_file",
		Lines:      lines,
		Available:  true,
	})
	m = updated.(*Model)
	if !m.followPaused() {
		t.Fatal("async diff completion resumed follow")
	}
	if got := transcriptBlockScreenRow(t, m, anchor.BlockID); got != wantScreenRow {
		t.Fatalf(
			"async diff completion above viewport moved semantic block from screen row %d to %d",
			wantScreenRow,
			got,
		)
	}
}
