package ui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestTranscriptPaintHeightOnlyResizeReusesHeightIndependentPrefix(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	const historyEntries = 192
	m.entries = make([]ChatEntry, 0, historyEntries)
	for index := 0; index < historyEntries; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind: "user",
			// Multi-line so sticky never omits the latest block mid-resize.
			Content: fmt.Sprintf("height-independent history %03d\ncontinued", index),
		})
	}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.setTranscriptYOffset(m.transcriptMaxTop() / 2)
	m.pauseFollow()
	capture := m.captureTranscriptReflowAnchor()

	cache := &m.transcriptPaint.cache
	if !cache.valid || len(cache.stableBlocks) != historyEntries {
		t.Fatalf(
			"fixture cache = valid:%t blocks:%d, want %d",
			cache.valid,
			len(cache.stableBlocks),
			historyEntries,
		)
	}
	lineIndexes := make([]*int, len(cache.stableBlocks))
	for index := range cache.stableBlocks {
		lineIndexes[index] = &cache.stableBlocks[index].lineStarts[0]
	}

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	// Grow width-stable height only; keep sticky disabled (height stays < 16).
	updated, _ := m.Update(tea.WindowSizeMsg{
		Width:  m.width,
		Height: 15,
	})
	m = updated.(*Model)

	if got := probe.blocksMeasured; got != 0 {
		t.Fatalf("height-only resize measured %d stable blocks, want none", got)
	}
	if got := probe.semanticDigestCalls; got != 0 {
		t.Fatalf("height-only resize re-admitted %d entries, want none", got)
	}
	if !cache.valid || len(cache.stableBlocks) != historyEntries {
		t.Fatalf(
			"resized cache = valid:%t blocks:%d, want preserved %d",
			cache.valid,
			len(cache.stableBlocks),
			historyEntries,
		)
	}
	for index, lineIndex := range lineIndexes {
		if got := &cache.stableBlocks[index].lineStarts[0]; got != lineIndex {
			t.Fatalf("height-only resize rebuilt line index %d", index)
		}
	}
	resolution, err := ResolveTranscriptAnchor(
		capture.Intent,
		capture.Previous,
		m.transcriptLayout,
		max(1, m.viewport.Height()),
	)
	if err != nil {
		t.Fatalf("resolve height-only anchor: %v", err)
	}
	if resolution.BlockID != capture.Intent.Manual.BlockID {
		t.Fatalf(
			"height-only anchor resolved block %q, want %q",
			resolution.BlockID,
			capture.Intent.Manual.BlockID,
		)
	}
	if !m.followPaused() {
		t.Fatal("height-only resize resumed a paused reader")
	}
	assertTranscriptPaintReferenceParity(t, m)
}
