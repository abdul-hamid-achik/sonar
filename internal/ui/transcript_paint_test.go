package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestTranscriptPaintWindowMatchesCompleteReferenceAtTopMiddleAndBottom(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.toolEntries = []ToolEntry{{
		ID:        "paint-tool",
		Name:      "bash",
		Summary:   "task verify",
		Result:    "\x1b[32mok\x1b[0m\nsecond row",
		Status:    ToolStatusDone,
		Collapsed: false,
	}}
	m.entries = []ChatEntry{
		{Kind: "user", Content: "first line\nsecond line with 你 and e\u0301"},
		{Kind: "assistant", Content: "A paragraph with **Markdown** and `code`.", ThinkingContent: "bounded thought", ThinkingCollapsed: false},
		{Kind: "tool_group", ToolIndex: 0},
		{Kind: "system", Content: "A bounded notice"},
		{Kind: "error", Content: "A recoverable failure"},
	}
	for index := 0; index < 16; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("history %02d\ncontinued", index),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()

	reference := m.renderEntries()
	referenceRows := strings.Split(reference, "\n")
	if got := m.transcriptPaint.document.totalRows; got != len(referenceRows) {
		t.Fatalf("logical rows = %d, complete reference rows = %d", got, len(referenceRows))
	}

	tops := []int{
		0,
		max(0, m.transcriptLayout.Records[2].StartRow-1),
		min(m.transcriptMaxTop(), m.transcriptLayout.Records[2].StartRow+1),
		m.transcriptMaxTop() / 2,
		m.transcriptMaxTop(),
	}
	for _, top := range tops {
		m.pauseFollow()
		m.setTranscriptYOffset(top)
		start := m.transcriptPaint.windowStart
		end := m.transcriptPaint.windowEnd
		want := strings.Join(referenceRows[start:end], "\n")
		if got := m.viewport.GetContent(); got != want {
			t.Fatalf(
				"top %d staged rows [%d,%d) diverged from complete reference:\n--- got ---\n%s\n--- want ---\n%s",
				top,
				start,
				end,
				got,
				want,
			)
		}
		if got := m.viewport.TotalLineCount(); got > m.viewport.Height()+2*transcriptPaintOverscanRows {
			t.Fatalf("top %d staged %d rows, bound is %d", top, got, m.viewport.Height()+2*transcriptPaintOverscanRows)
		}
	}
}

func TestTranscriptPaintWindowBoundsTenThousandEntryHistory(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	const entryCount = 10_000
	m.entries = make([]ChatEntry, 0, entryCount)
	for index := 0; index < entryCount; index++ {
		m.entries = append(m.entries, ChatEntry{
			BlockID:   BlockID(fmt.Sprintf("paint_block_%05d", index)),
			TurnID:    TurnID(fmt.Sprintf("paint_turn_%05d", index)),
			Revision:  1,
			Lifecycle: BlockSettled,
			Kind:      "user",
			Content:   fmt.Sprintf("bounded history row %05d", index),
		})
	}
	m.invalidateEntryCache()
	m.pauseFollow()
	m.transcriptPaint.top = entryCount
	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe

	m.refreshTranscript()

	rowBound := m.viewport.Height() + 2*transcriptPaintOverscanRows
	if probe.renderEntriesCalls != 0 || probe.transcriptBytesMaterialized != 0 {
		t.Fatalf("virtual paint called the complete renderer: %#v", probe)
	}
	if probe.blocksMeasured != entryCount {
		t.Fatalf("cold measure visited %d blocks, want %d", probe.blocksMeasured, entryCount)
	}
	if probe.paintRowsStaged <= 0 || probe.paintRowsStaged > rowBound {
		t.Fatalf("paint staged %d rows, want 1..%d", probe.paintRowsStaged, rowBound)
	}
	if probe.viewportRowsStaged > rowBound || m.viewport.TotalLineCount() > rowBound {
		t.Fatalf(
			"Bubbles staged %d/%d rows, bound is %d",
			probe.viewportRowsStaged,
			m.viewport.TotalLineCount(),
			rowBound,
		)
	}
	if probe.paintBytesStaged >= entryCount*len("bounded history row 00000") {
		t.Fatalf("staged paint bytes %d scale with complete history", probe.paintBytesStaged)
	}
	if m.transcriptYOffset() != entryCount {
		t.Fatalf("paused logical top = %d, want %d", m.transcriptYOffset(), entryCount)
	}
}

func TestEmptyTranscriptPaintVirtualizesUnboundedStartupNotices(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	const noticeCount = 2_000
	for index := 0; index < noticeCount; index++ {
		kind := "system"
		if index%3 == 0 {
			kind = "error"
		}
		m.entries = append(m.entries, ChatEntry{
			Kind: kind,
			Content: fmt.Sprintf(
				"startup notice %04d %s",
				index,
				strings.Repeat("bounded detail ", index%5+1),
			),
		})
	}
	m.invalidateEntryCache()
	m.pauseFollow()
	m.transcriptPaint.top = noticeCount
	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe

	m.refreshTranscript()

	rowBound := m.viewport.Height() + 2*transcriptPaintOverscanRows
	if probe.renderEntriesCalls != 0 || probe.transcriptBytesMaterialized != 0 {
		t.Fatalf("notice-only history called the complete renderer: %#v", probe)
	}
	if probe.blocksMeasured != noticeCount+1 {
		t.Fatalf("measured blocks = %d, want welcome + %d notices", probe.blocksMeasured, noticeCount)
	}
	if probe.paintRowsStaged <= 0 || probe.paintRowsStaged > rowBound {
		t.Fatalf("notice paint staged %d rows, bound %d", probe.paintRowsStaged, rowBound)
	}
	start := m.transcriptPaint.windowStart
	end := m.transcriptPaint.windowEnd
	referenceRows := strings.Split(m.renderEntries(), "\n")
	if got := m.transcriptPaint.document.totalRows; got != len(referenceRows) {
		t.Fatalf("notice rows = %d, complete reference = %d", got, len(referenceRows))
	}
	if got, want := m.viewport.GetContent(), strings.Join(referenceRows[start:end], "\n"); got != want {
		t.Fatalf("notice window [%d,%d) diverged from complete reference", start, end)
	}
}

func TestTranscriptPaintWindowExtractsRowsInsideOneLargeBlock(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	const sourceRows = 20_000
	m.entries = []ChatEntry{{
		Kind:    "user",
		Content: strings.Repeat("large block row\n", sourceRows),
	}}
	m.invalidateEntryCache()
	m.pauseFollow()
	m.transcriptPaint.top = sourceRows / 2
	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe

	m.refreshTranscript()

	rowBound := m.viewport.Height() + 2*transcriptPaintOverscanRows
	if probe.blocksMeasured != 1 || probe.blocksPainted != 1 {
		t.Fatalf("large block measure/paint = %d/%d, want 1/1", probe.blocksMeasured, probe.blocksPainted)
	}
	if probe.paintRowsStaged > rowBound || m.viewport.TotalLineCount() > rowBound {
		t.Fatalf(
			"large block staged %d/%d rows, bound %d",
			probe.paintRowsStaged,
			m.viewport.TotalLineCount(),
			rowBound,
		)
	}
	if strings.Count(m.viewport.GetContent(), "\n")+1 > rowBound {
		t.Fatal("large block paint leaked rows outside the bounded window")
	}
}

func TestWarmStreamingPaintDoesNotRevisitSettledHistory(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	const historyEntries = 2_000
	for index := 0; index < historyEntries; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("stable history %04d", index),
		})
	}
	m.state = StateStreaming
	m.streamBuf.WriteString("initial live tail")
	m.invalidateEntryCache()
	m.pauseFollow()
	m.transcriptPaint.top = historyEntries / 2
	m.refreshTranscript()
	beforeTop := m.transcriptYOffset()
	beforeWindow := m.viewport.GetContent()

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	m.streamBuf.WriteString(" plus one token")
	m.refreshTranscript()

	if probe.blocksMeasured != 1 {
		t.Fatalf("warm streaming measured %d blocks, want only live tail", probe.blocksMeasured)
	}
	if probe.semanticDigestCalls != 0 ||
		probe.layoutRecordsMaterialized != 0 ||
		probe.layoutRecordComparisons > 2 {
		t.Fatalf("warm streaming repeated semantic/layout work: %#v", probe)
	}
	if got := m.transcriptYOffset(); got != beforeTop {
		t.Fatalf("offscreen stream moved paused top from %d to %d", beforeTop, got)
	}
	if got := m.viewport.GetContent(); got != beforeWindow {
		t.Fatal("offscreen stream changed the paused paint window")
	}
	if probe.paintRowsStaged != 0 ||
		probe.paintBytesStaged != 0 ||
		probe.blocksPainted != 0 ||
		probe.viewportRowsStaged != 0 {
		t.Fatalf("offscreen warm stream restaged stable paint: %#v", probe)
	}
}

func TestTranscriptPagingCrossesPaintWindowsWithoutChangingRowOrder(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	for index := 0; index < 120; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("ordered row %03d", index),
		})
	}
	m.invalidateEntryCache()
	m.pauseFollow()
	m.transcriptPaint.top = 0
	m.refreshTranscript()
	referenceRows := strings.Split(m.renderEntries(), "\n")

	for step := 0; step < 8; step++ {
		before := m.transcriptYOffset()
		_ = m.updateTranscriptViewport(tea.KeyPressMsg{Code: tea.KeyPgDown})
		wantTop := min(before+m.viewport.Height(), m.transcriptMaxTop())
		if got := m.transcriptYOffset(); got != wantTop {
			t.Fatalf("page %d top = %d, want %d", step, got, wantTop)
		}
		start := m.transcriptPaint.windowStart
		end := m.transcriptPaint.windowEnd
		if got, want := m.viewport.GetContent(), strings.Join(referenceRows[start:end], "\n"); got != want {
			t.Fatalf("page %d repeated or skipped rows", step)
		}
	}

	for step := 0; step < 8; step++ {
		before := m.transcriptYOffset()
		_ = m.updateTranscriptViewport(tea.KeyPressMsg{Code: tea.KeyPgUp})
		wantTop := max(0, before-m.viewport.Height())
		if got := m.transcriptYOffset(); got != wantTop {
			t.Fatalf("reverse page %d top = %d, want %d", step, got, wantTop)
		}
	}
}

func TestTranscriptPaintGeometrySweep(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	for index := 0; index < 18; index++ {
		kind := "user"
		if index%3 == 1 {
			kind = "assistant"
		}
		m.entries = append(m.entries, ChatEntry{
			Kind: kind,
			Content: fmt.Sprintf(
				"geometry %02d\n%s",
				index,
				strings.Repeat("wrapped cells ", index%5+1),
			),
		})
	}
	m.invalidateEntryCache()
	m.pauseFollow()
	m.transcriptPaint.top = 0
	m.refreshTranscript()
	referenceRows := strings.Split(m.renderEntries(), "\n")

	for _, height := range []int{1, 2, 5, 11, 24} {
		m.viewport.SetHeight(height)
		for top := 0; top <= m.transcriptMaxTop(); top++ {
			m.setTranscriptYOffset(top)
			start := m.transcriptPaint.windowStart
			end := m.transcriptPaint.windowEnd
			if got, want := m.viewport.GetContent(), strings.Join(referenceRows[start:end], "\n"); got != want {
				t.Fatalf(
					"height %d top %d window [%d,%d) diverged from reference",
					height,
					top,
					start,
					end,
				)
			}
			if got := m.viewport.TotalLineCount(); got > height+2*transcriptPaintOverscanRows {
				t.Fatalf("height %d top %d staged %d rows", height, top, got)
			}
		}
	}
}

func TestVirtualTranscriptMouseUsesLogicalDocumentRow(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	for index := 0; index < 20; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("history before tool %02d", index),
		})
	}
	m.toolEntries = []ToolEntry{{
		ID:        "virtual-pointer-tool",
		Name:      "bash",
		Summary:   "task test",
		Result:    "ok",
		Status:    ToolStatusDone,
		Collapsed: true,
		Duration:  time.Second,
	}}
	m.entries = append(m.entries, ChatEntry{Kind: "tool_group", ToolIndex: 0})
	for index := 0; index < 20; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "assistant",
			Content: fmt.Sprintf("history after tool %02d", index),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()

	var region toolHitRegion
	found := false
	for _, candidate := range m.toolHitRegions {
		if candidate.ToolIndex == 0 {
			region = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatal("virtual transcript did not publish the tool header region")
	}
	m.pauseFollow()
	m.setTranscriptYOffset(region.Row)
	_ = m.handleMouseClick(region.StartCol, 0)
	if m.toolEntries[0].Collapsed {
		t.Fatal("click on screen row zero did not toggle the logical tool row")
	}
}

func BenchmarkTranscriptPaintWindowPaused10K(b *testing.B) {
	m := newTestModel(b)
	for index := 0; index < 10_000; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("benchmark history %05d", index),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.pauseFollow()
	m.setTranscriptYOffset(m.transcriptMaxTop() / 2)
	b.ResetTimer()
	for range b.N {
		m.transcriptPaint.windowGeneration = 0
		m.syncTranscriptPaintWindow()
	}
}

func BenchmarkTranscriptScrollWindow10K(b *testing.B) {
	m := newTestModel(b)
	for index := 0; index < 10_000; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("benchmark history %05d", index),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.pauseFollow()
	m.setTranscriptYOffset(m.transcriptMaxTop() / 2)
	b.ResetTimer()
	for range b.N {
		m.scrollTranscriptBy(m.viewport.Height())
		m.scrollTranscriptBy(-m.viewport.Height())
	}
}

func BenchmarkTranscriptRefreshWarmStreamingTail10K(b *testing.B) {
	m := newTestModel(b)
	for index := 0; index < 10_000; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("benchmark history %05d", index),
		})
	}
	tail := strings.Repeat("long streaming tail row\n", 2_000)
	m.state = StateStreaming
	m.streamBuf.WriteString(tail)
	m.invalidateEntryCache()
	m.pauseFollow()
	m.transcriptPaint.top = 10_000
	m.refreshTranscript()

	b.ResetTimer()
	for index := range b.N {
		b.StopTimer()
		m.streamBuf.Reset()
		m.streamBuf.WriteString(tail)
		if index%2 == 0 {
			m.streamBuf.WriteByte('x')
		}
		b.StartTimer()
		m.refreshTranscript()
	}
}
