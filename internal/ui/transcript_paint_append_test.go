package ui

import (
	"fmt"
	"strings"
	"testing"
)

func TestTranscriptPaintAppendOnlyGrowthMeasuresOnlyDelta(t *testing.T) {
	m := newTestModel(t)
	const historyEntries = 128
	m.entries = make([]ChatEntry, 0, historyEntries+3)
	for index := 0; index < historyEntries; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("stable prefix %03d\ncontinued", index),
		})
	}
	m.invalidateEntryCache()
	m.pauseFollow()
	m.transcriptPaint.top = historyEntries / 2
	m.refreshTranscript()

	cache := &m.transcriptPaint.cache
	if !cache.valid || cache.entryCount != historyEntries {
		t.Fatalf(
			"cold paint cache = {valid:%t entries:%d}, want valid prefix of %d",
			cache.valid,
			cache.entryCount,
			historyEntries,
		)
	}
	if len(cache.stableBlocks) != historyEntries {
		t.Fatalf("cold stable blocks = %d, want %d", len(cache.stableBlocks), historyEntries)
	}
	prefixTurnID := m.entries[historyEntries-1].TurnID
	prefixBlockID := m.entries[historyEntries-1].BlockID
	lineIndexes := make([]*int, len(cache.stableBlocks))
	for index := range cache.stableBlocks {
		if len(cache.stableBlocks[index].lineStarts) == 0 {
			t.Fatalf("cold block %d has no row index", index)
		}
		lineIndexes[index] = &cache.stableBlocks[index].lineStarts[0]
	}

	appended := []ChatEntry{
		{Kind: "assistant", Content: "new answer with **Markdown**"},
		{Kind: "system", Content: "new bounded notice"},
		{Kind: "user", Content: "next prompt\nwith another row"},
	}
	m.entries = append(m.entries, appended...)
	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	m.refreshTranscript()

	if got, want := probe.blocksMeasured, len(appended); got != want {
		t.Fatalf("append paint measured %d blocks, want only delta %d", got, want)
	}
	if got, want := probe.semanticDigestCalls, len(appended); got != want {
		t.Fatalf("append reconciliation hashed %d entries, want only delta %d", got, want)
	}
	if got := m.entries[historyEntries].TurnID; got != prefixTurnID {
		t.Fatalf("appended assistant turn = %q, want cached causal turn %q", got, prefixTurnID)
	}
	if got := m.entries[historyEntries-1].BlockID; got != prefixBlockID {
		t.Fatalf("append changed stable prefix block ID from %q to %q", prefixBlockID, got)
	}
	if cache.entryCount != len(m.entries) || cache.stableCount != len(m.entries) {
		t.Fatalf(
			"promoted cache counts = entries:%d stable:%d, want %d",
			cache.entryCount,
			cache.stableCount,
			len(m.entries),
		)
	}
	if got, want := len(cache.stableBlocks), historyEntries+len(appended); got != want {
		t.Fatalf("promoted stable blocks = %d, want %d", got, want)
	}
	for index, lineIndex := range lineIndexes {
		if got := &cache.stableBlocks[index].lineStarts[0]; got != lineIndex {
			t.Fatalf("append rebuilt row index for stable block %d", index)
		}
	}

	assertTranscriptPaintReferenceParity(t, m)
}

func TestTranscriptPaintMutationInvalidatesAppendReuse(t *testing.T) {
	m := newTestModel(t)
	// Sticky omits the latest single-line user from paint; paint-cache tests need
	// every entry to measure as a stable block.
	m.height = 13
	m.toolEntries = []ToolEntry{{
		ID:        "append-cache-tool",
		Name:      "bash",
		Summary:   "task verify",
		Args:      "cmd=task verify",
		Result:    "first row\nsecond row",
		Status:    ToolStatusDone,
		Collapsed: true,
	}}
	m.entries = []ChatEntry{
		{Kind: "user", Content: "run verification"},
		{Kind: "tool_group", ToolIndex: 0},
		{Kind: "assistant", Content: "verification complete"},
	}
	m.invalidateEntryCache()
	m.refreshTranscript()

	m.toolEntries[0].Collapsed = false
	m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "receipt expanded"})
	m.invalidateEntryCache()
	if m.transcriptPaint.cache.valid {
		t.Fatal("tool geometry mutation left the paint cache valid")
	}

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	m.refreshTranscript()
	if got, want := probe.blocksMeasured, len(m.entries); got != want {
		t.Fatalf("mutated append measured %d blocks, want full rebuild of %d", got, want)
	}
	if got, want := probe.semanticDigestCalls, len(m.entries); got != want {
		t.Fatalf("mutated append hashed %d entries, want full reconciliation of %d", got, want)
	}
	assertTranscriptPaintReferenceParity(t, m)
}

func TestFlushStreamPromotesOnlyLiveTail(t *testing.T) {
	m := newTestModel(t)
	// Disable sticky so single-line history users stay as paint blocks.
	m.height = 13
	const historyEntries = 256
	m.entries = make([]ChatEntry, 0, historyEntries+1)
	for index := 0; index < historyEntries; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("stable history %03d", index),
		})
	}
	m.state = StateStreaming
	m.appendTranscriptStreamText(
		strings.Repeat("settling semantic row\n", 80) + "final marker",
	)
	m.invalidateEntryCache()
	m.refreshTranscript()

	cache := &m.transcriptPaint.cache
	if !cache.valid || cache.entryCount != historyEntries {
		t.Fatalf(
			"live cache prefix = {valid:%t entries:%d}, want %d stable entries",
			cache.valid,
			cache.entryCount,
			historyEntries,
		)
	}
	live := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1]
	m.setTranscriptYOffset(live.StartRow + 20)
	m.pauseFollow()
	capture := m.captureTranscriptReflowAnchor()
	if capture.Intent.Manual.BlockID != live.BlockID {
		t.Fatalf(
			"captured block = %q, want live block %q",
			capture.Intent.Manual.BlockID,
			live.BlockID,
		)
	}

	prefixLineIndexes := make([]*int, len(cache.stableBlocks))
	for index := range cache.stableBlocks {
		if len(cache.stableBlocks[index].lineStarts) == 0 {
			t.Fatalf("stable block %d has no line index", index)
		}
		prefixLineIndexes[index] = &cache.stableBlocks[index].lineStarts[0]
	}

	m.flushStream()
	m.state = StateIdle
	if !cache.valid {
		t.Fatal("flush invalidated the append-aware paint prefix")
	}
	if !m.transcriptReconcileValid ||
		m.transcriptReconciledCount != historyEntries {
		t.Fatalf(
			"flush discarded reconciliation prefix: valid=%t count=%d",
			m.transcriptReconcileValid,
			m.transcriptReconciledCount,
		)
	}

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	m.refreshTranscript()

	if got := probe.blocksMeasured; got != 1 {
		t.Fatalf("flush promotion measured %d blocks, want only settled tail", got)
	}
	if got := probe.semanticDigestCalls; got != 1 {
		t.Fatalf("flush promotion admitted %d entries, want only settled tail", got)
	}
	if cache.entryCount != historyEntries+1 ||
		len(cache.stableBlocks) != historyEntries+1 {
		t.Fatalf(
			"promoted cache = entries:%d blocks:%d, want %d",
			cache.entryCount,
			len(cache.stableBlocks),
			historyEntries+1,
		)
	}
	for index, lineIndex := range prefixLineIndexes {
		if got := &cache.stableBlocks[index].lineStarts[0]; got != lineIndex {
			t.Fatalf("flush rebuilt stable line index %d", index)
		}
	}
	settled := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1]
	if settled.BlockID != live.BlockID || settled.TurnID != live.TurnID {
		t.Fatalf(
			"settled identity = %q/%q, want live %q/%q",
			settled.BlockID,
			settled.TurnID,
			live.BlockID,
			live.TurnID,
		)
	}
	resolution, err := ResolveTranscriptAnchor(
		capture.Intent,
		capture.Previous,
		m.transcriptLayout,
		max(1, m.viewport.Height()),
	)
	if err != nil {
		t.Fatalf("resolve promoted live anchor: %v", err)
	}
	if resolution.BlockID != live.BlockID ||
		resolution.Reason != AnchorResolutionExactBlock {
		t.Fatalf(
			"promoted anchor resolved %q/%v, want %q/%v",
			resolution.BlockID,
			resolution.Reason,
			live.BlockID,
			AnchorResolutionExactBlock,
		)
	}
	if !m.followPaused() {
		t.Fatal("flush promotion resumed a paused reader")
	}
	assertTranscriptPaintReferenceParity(t, m)
}

func TestFlushWhitespacePreservesStablePaintAndReconciliation(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{
		{Kind: "user", Content: "stable prompt"},
		{Kind: "assistant", Content: "stable answer"},
	}
	m.invalidateEntryCache()
	m.refreshTranscript()

	cache := &m.transcriptPaint.cache
	if !cache.valid || !m.transcriptReconcileValid {
		t.Fatal("fixture did not establish stable paint and reconciliation")
	}
	stableBlocks := cache.stableBlocks
	reconcileEpoch := m.transcriptReconcileEpoch

	m.appendTranscriptStreamText(" \n\t ")
	m.thinkBuf.WriteString("\n ")
	m.flushStream()
	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	m.refreshTranscript()

	if !cache.valid {
		t.Fatal("whitespace flush invalidated the paint prefix")
	}
	if len(cache.stableBlocks) > 0 && &cache.stableBlocks[0] != &stableBlocks[0] {
		t.Fatal("whitespace flush replaced stable paint blocks")
	}
	if got := probe.blocksMeasured; got != 0 {
		t.Fatalf("whitespace flush measured %d blocks, want none", got)
	}
	if got := probe.semanticDigestCalls; got != 0 {
		t.Fatalf("whitespace flush admitted %d entries, want none", got)
	}
	if m.transcriptReconcileEpoch != reconcileEpoch {
		t.Fatalf(
			"whitespace flush advanced reconcile epoch from %d to %d",
			reconcileEpoch,
			m.transcriptReconcileEpoch,
		)
	}
	assertTranscriptPaintReferenceParity(t, m)
}

func assertTranscriptPaintReferenceParity(t *testing.T, m *Model) {
	t.Helper()

	m.transcriptRenderProbe = nil
	referenceRows := strings.Split(m.renderEntries(), "\n")
	if got, want := m.transcriptPaint.document.totalRows, len(referenceRows); got != want {
		t.Fatalf("paint rows = %d, complete reference rows = %d", got, want)
	}

	tops := []int{0, m.transcriptMaxTop() / 2, m.transcriptMaxTop()}
	for _, top := range tops {
		m.pauseFollow()
		m.setTranscriptYOffset(top)
		start := m.transcriptPaint.windowStart
		end := m.transcriptPaint.windowEnd
		want := strings.Join(referenceRows[start:end], "\n")
		if got := m.viewport.GetContent(); got != want {
			t.Fatalf(
				"paint window at top %d rows [%d,%d) diverged from complete reference:\n--- got ---\n%s\n--- want ---\n%s",
				top,
				start,
				end,
				got,
				want,
			)
		}
	}
}
