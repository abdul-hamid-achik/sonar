package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestIncrementalPlainLiveTailMatchesCompleteRenderer(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: body keeps the user block for paint identity
	m.entries = []ChatEntry{{
		Kind:    "user",
		Content: "stream a long response",
	}}
	m.state = StateStreaming
	m.appendTranscriptStreamText(
		strings.Repeat("stable streaming row with 你 and e\u0301\n", 240) +
			"mutable final row",
	)
	m.invalidateEntryCache()
	m.refreshTranscript()
	if !m.transcriptPaint.liveCache.valid {
		t.Fatal("plain live tail did not install the incremental projection")
	}

	for _, delta := range []string{
		" plus",
		" a wrapped continuation that grows the final visual row",
		"\nnext raw row",
		" and its continuation",
	} {
		m.appendTranscriptStreamText(delta)
		m.refreshTranscript()
		assertIncrementalLiveTailParity(t, m)
	}
}

func TestIncrementalPlainLiveTailMeasuresOnlyMutableRawLine(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off for full layout-record coverage
	m.entries = []ChatEntry{{Kind: "user", Content: "measure the stream delta"}}
	m.state = StateStreaming
	m.appendTranscriptStreamText(
		strings.Repeat("immutable live row\n", 2_000) + "short tail",
	)
	m.invalidateEntryCache()
	m.refreshTranscript()
	if got := len(m.transcriptPaint.liveCache.rows); got < 2_000 {
		t.Fatalf("incremental fixture has %d rows, want at least 2000", got)
	}

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	m.appendTranscriptStreamText(" plus one token")
	m.refreshTranscript()

	if probe.blocksMeasured > 2 {
		t.Fatalf(
			"append measured %d blocks, want only the mutable raw line",
			probe.blocksMeasured,
		)
	}
	if probe.lineIndexRowsBuilt > 2 {
		t.Fatalf(
			"append rebuilt %d line-index rows, want only the mutable suffix",
			probe.lineIndexRowsBuilt,
		)
	}
	if probe.measureBytesMaterialized >= len("immutable live row\n")*100 {
		t.Fatalf(
			"append measured %d bytes, scaled with the stable live prefix",
			probe.measureBytesMaterialized,
		)
	}
	assertIncrementalLiveTailParity(t, m)
}

func TestIncrementalLayoutPublicationDoesNotCopyStableHistory(t *testing.T) {
	type publicationWork struct {
		comparisons int
		updates     int
	}
	work := make(map[int]publicationWork)

	for _, historyEntries := range []int{1, 10_000} {
		t.Run(fmt.Sprintf("history_%d", historyEntries), func(t *testing.T) {
			m := newTestModel(t)
			m.height = 13 // sticky off for full layout-record coverage
			m.entries = make([]ChatEntry, 0, historyEntries)
			for index := 0; index < historyEntries; index++ {
				m.entries = append(m.entries, ChatEntry{
					Kind:    "user",
					Content: fmt.Sprintf("stable history %05d", index),
				})
			}
			m.state = StateStreaming
			m.appendTranscriptStreamText("mutable live row")
			m.invalidateEntryCache()
			m.refreshTranscript()

			if len(m.transcriptLayout.Records) != historyEntries+1 {
				t.Fatalf(
					"cold layout records = %d, want %d",
					len(m.transcriptLayout.Records),
					historyEntries+1,
				)
			}
			firstRecord := &m.transcriptLayout.Records[0]
			oldLiveHeight := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1].Height
			probe := &transcriptRenderProbe{}
			m.transcriptRenderProbe = probe

			m.appendTranscriptStreamText("\nnew visual row")
			m.refreshTranscript()

			if probe.layoutRecordsMaterialized != 0 {
				t.Fatalf(
					"incremental row materialized %d layout records",
					probe.layoutRecordsMaterialized,
				)
			}
			if probe.layoutRecordsUpdated > 2 {
				t.Fatalf(
					"incremental row updated %d layout records, want at most predecessor + live tail",
					probe.layoutRecordsUpdated,
				)
			}
			if probe.layoutRecordComparisons > 2 {
				t.Fatalf(
					"incremental row compared %d layout records, want at most predecessor + live tail",
					probe.layoutRecordComparisons,
				)
			}
			if firstRecord != &m.transcriptLayout.Records[0] {
				t.Fatal("incremental row replaced the stable layout backing")
			}
			live := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1]
			if live.Height <= oldLiveHeight {
				t.Fatalf(
					"live height = %d after adding a row, want greater than %d",
					live.Height,
					oldLiveHeight,
				)
			}
			if _, _, err := indexCurrentTranscriptLayout(m.transcriptLayout.Records); err != nil {
				t.Fatalf("incrementally published layout is invalid: %v", err)
			}
			work[historyEntries] = publicationWork{
				comparisons: probe.layoutRecordComparisons,
				updates:     probe.layoutRecordsUpdated,
			}
		})
	}

	if work[1] != work[10_000] {
		t.Fatalf(
			"publication work scales with history: 1=%+v 10K=%+v",
			work[1],
			work[10_000],
		)
	}
}

func TestLayoutTailPublicationCopiesOnIdentityChange(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off for full layout-record coverage
	m.entries = []ChatEntry{
		{Kind: "user", Content: "stable prefix"},
		{Kind: "assistant", Content: "stable response"},
	}
	m.state = StateStreaming
	m.appendTranscriptStreamText("live response")
	m.invalidateEntryCache()
	m.refreshTranscript()

	previous := m.transcriptLayout
	previousBacking := &previous.Records[0]
	last := len(previous.Records) - 1
	previousLiveID := previous.Records[last].BlockID
	base := previous.Records[:last:last]
	replacement := previous.Records[last]
	replacement.BlockID = "replacement_live_block"

	m.publishTranscriptLayoutSegments(base, []TranscriptLayoutRecord{replacement})

	if &m.transcriptLayout.Records[0] == previousBacking {
		t.Fatal("identity change mutated the previous layout backing")
	}
	if previous.Records[last].BlockID != previousLiveID {
		t.Fatalf(
			"previous layout identity changed from %q to %q",
			previousLiveID,
			previous.Records[last].BlockID,
		)
	}
	if got := m.transcriptLayout.Records[last].BlockID; got != replacement.BlockID {
		t.Fatalf("current layout identity = %q, want %q", got, replacement.BlockID)
	}
}

func TestLayoutTailPublicationPreservesCapturedAnchorSemantics(t *testing.T) {
	newPausedLiveCapture := func(t *testing.T) (*Model, transcriptReflowAnchor, BlockID) {
		t.Helper()
		m := newTestModel(t)
		m.height = 13 // sticky off for full layout-record coverage
		m.entries = []ChatEntry{{
			Kind:    "user",
			Content: strings.Repeat("stable history row\n", 24),
		}}
		m.state = StateStreaming
		m.appendTranscriptStreamText(
			strings.Repeat("live semantic row\n", 80) + "mutable tail",
		)
		m.invalidateEntryCache()
		m.refreshTranscript()

		live := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1]
		m.setTranscriptYOffset(live.StartRow + 8)
		m.pauseFollow()
		capture := m.captureTranscriptReflowAnchor()
		if capture.Intent.Manual.BlockID != live.BlockID {
			t.Fatalf(
				"captured block = %q, want live block %q",
				capture.Intent.Manual.BlockID,
				live.BlockID,
			)
		}
		return m, capture, m.transcriptLayout.Records[len(m.transcriptLayout.Records)-2].BlockID
	}

	t.Run("surviving tail reuses backing and resolves semantic point", func(t *testing.T) {
		m, capture, _ := newPausedLiveCapture(t)
		previousBacking := &capture.Previous.Records[0]
		liveID := capture.Intent.Manual.BlockID

		m.appendTranscriptStreamText("\nnew live row")
		m.refreshTranscript()

		if &m.transcriptLayout.Records[0] != previousBacking {
			t.Fatal("identity-stable live append replaced the layout backing")
		}
		resolution, err := ResolveTranscriptAnchor(
			capture.Intent,
			capture.Previous,
			m.transcriptLayout,
			max(1, m.viewport.Height()),
		)
		if err != nil {
			t.Fatalf("resolve surviving live capture: %v", err)
		}
		if resolution.BlockID != liveID ||
			resolution.Reason != AnchorResolutionExactBlock {
			t.Fatalf(
				"surviving capture resolved block/reason = %q/%v, want %q/%v",
				resolution.BlockID,
				resolution.Reason,
				liveID,
				AnchorResolutionExactBlock,
			)
		}
	})

	t.Run("identity change copies backing and preserves deletion fallback", func(t *testing.T) {
		m, capture, previousBlockID := newPausedLiveCapture(t)
		previousBacking := &capture.Previous.Records[0]
		last := len(m.transcriptLayout.Records) - 1
		base := m.transcriptLayout.Records[:last:last]
		replacement := m.transcriptLayout.Records[last]
		replacement.BlockID = "replacement_live_block"

		m.publishTranscriptLayoutSegments(base, []TranscriptLayoutRecord{replacement})

		if &m.transcriptLayout.Records[0] == previousBacking {
			t.Fatal("identity change mutated the captured layout backing")
		}
		if got := capture.Previous.Records[last].BlockID; got != capture.Intent.Manual.BlockID {
			t.Fatalf(
				"captured live identity = %q, want %q",
				got,
				capture.Intent.Manual.BlockID,
			)
		}
		resolution, err := ResolveTranscriptAnchor(
			capture.Intent,
			capture.Previous,
			m.transcriptLayout,
			max(1, m.viewport.Height()),
		)
		if err != nil {
			t.Fatalf("resolve replaced live capture: %v", err)
		}
		if resolution.BlockID != previousBlockID ||
			resolution.Reason != AnchorResolutionPreviousBlock {
			t.Fatalf(
				"replaced capture resolved block/reason = %q/%v, want %q/%v",
				resolution.BlockID,
				resolution.Reason,
				previousBlockID,
				AnchorResolutionPreviousBlock,
			)
		}
	})
}

func TestIncrementalPlainLiveTailRejectsUntrackedMutationAndRebuildsMarkdownBoundary(t *testing.T) {
	t.Run("untracked builder mutation", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off for full layout-record coverage
		m.entries = []ChatEntry{{Kind: "user", Content: "do not trust a reset"}}
		m.state = StateStreaming
		m.appendTranscriptStreamText(strings.Repeat("old row\n", 80) + "old tail")
		m.invalidateEntryCache()
		m.refreshTranscript()

		replacementLen := m.streamBuf.Len()
		m.streamBuf.Reset()
		m.streamBuf.WriteString(strings.Repeat("x", replacementLen))
		probe := &transcriptRenderProbe{}
		m.transcriptRenderProbe = probe
		m.refreshTranscript()

		if probe.blocksMeasured != 1 {
			t.Fatalf(
				"untracked replacement measured %d blocks, want canonical cold block",
				probe.blocksMeasured,
			)
		}
		if m.transcriptPaint.liveCache.valid {
			t.Fatal("untracked replacement retained incremental provenance")
		}
		assertIncrementalLiveTailParity(t, m)
	})

	t.Run("new safe markdown boundary", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off for full layout-record coverage
		m.entries = []ChatEntry{{Kind: "user", Content: "format complete blocks"}}
		m.state = StateStreaming
		m.appendTranscriptStreamText(strings.Repeat("plain row\n", 40) + "paragraph")
		m.invalidateEntryCache()
		m.refreshTranscript()
		beforeBoundary := m.transcriptPaint.liveCache.tailRawStart

		m.appendTranscriptStreamText("\n\n**formatted block**")
		m.refreshTranscript()
		if !m.transcriptPaint.liveCache.valid {
			t.Fatal("Markdown boundary rebuild did not install a segmented cache")
		}
		if got := m.transcriptPaint.liveCache.tailRawStart; got <= beforeBoundary {
			t.Fatalf(
				"Markdown boundary did not advance stable prefix: before=%d after=%d",
				beforeBoundary,
				got,
			)
		}
		assertIncrementalLiveTailParity(t, m)
	})
}

func TestIncrementalPlainLiveTailPreservesPausedSemanticRow(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off for full layout-record coverage
	m.entries = []ChatEntry{{Kind: "user", Content: "preserve my reading position"}}
	m.state = StateStreaming
	for index := 0; index < 400; index++ {
		m.appendTranscriptStreamText(fmt.Sprintf("semantic row %03d marker-%03d\n", index, index))
	}
	m.appendTranscriptStreamText("mutable tail")
	m.invalidateEntryCache()
	m.refreshTranscript()

	live := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1]
	m.setTranscriptYOffset(live.StartRow + 120)
	m.pauseFollow()
	before := ansi.Strip(m.viewport.View())
	const marker = "marker-120"
	location := strings.Index(before, marker)
	if location < 0 {
		t.Fatalf("paused fixture does not show %s:\n%s", marker, before)
	}
	screenRow := strings.Count(before[:location], "\n")

	m.appendTranscriptStreamText(" plus a token")
	m.refreshTranscript()
	after := ansi.Strip(m.viewport.View())
	location = strings.Index(after, marker)
	if location < 0 {
		t.Fatalf("incremental append lost %s:\n%s", marker, after)
	}
	if got := strings.Count(after[:location], "\n"); got != screenRow {
		t.Fatalf("paused marker moved from screen row %d to %d", screenRow, got)
	}
	if !m.followPaused() {
		t.Fatal("incremental append resumed paused follow")
	}
}

func BenchmarkTranscriptRefreshIncrementalStreamingDelta10K(b *testing.B) {
	m := newTestModel(b)
	for index := 0; index < 10_000; index++ {
		m.entries = append(m.entries, ChatEntry{
			Kind:    "user",
			Content: fmt.Sprintf("benchmark history %05d", index),
		})
	}
	m.state = StateStreaming
	m.appendTranscriptStreamText(
		strings.Repeat("stable streaming row\n", 2_000) + "mutable",
	)
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.transcriptGotoBottom()

	b.ResetTimer()
	for range b.N {
		m.appendTranscriptStreamText("delta row\n")
		m.refreshTranscript()
	}
}

func BenchmarkTranscriptLayoutPublishIncrementalRow(b *testing.B) {
	for _, historyEntries := range []int{1, 10_000} {
		b.Run(fmt.Sprintf("history_%d", historyEntries), func(b *testing.B) {
			m := newTestModel(b)
			m.entries = make([]ChatEntry, 0, historyEntries)
			for index := 0; index < historyEntries; index++ {
				m.entries = append(m.entries, ChatEntry{
					Kind:    "user",
					Content: fmt.Sprintf("benchmark history %05d", index),
				})
			}
			m.state = StateStreaming
			m.appendTranscriptStreamText("mutable")
			m.invalidateEntryCache()
			m.refreshTranscript()
			m.transcriptGotoBottom()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.appendTranscriptStreamText("\nrow")
				m.refreshTranscript()
			}
		})
	}
}

func assertIncrementalLiveTailParity(t *testing.T, m *Model) {
	t.Helper()
	referenceRows := strings.Split(m.renderEntries(), "\n")
	if got, want := m.transcriptPaint.document.totalRows, len(referenceRows); got != want {
		t.Fatalf("incremental rows = %d, complete renderer = %d", got, want)
	}
	for _, top := range []int{
		0,
		m.transcriptMaxTop() / 2,
		m.transcriptMaxTop(),
	} {
		m.pauseFollow()
		m.setTranscriptYOffset(top)
		start := m.transcriptPaint.windowStart
		end := m.transcriptPaint.windowEnd
		want := strings.Join(referenceRows[start:end], "\n")
		if got := m.viewport.GetContent(); got != want {
			t.Fatalf(
				"incremental window [%d,%d) at top %d diverged:\n--- got ---\n%s\n--- want ---\n%s",
				start,
				end,
				top,
				got,
				want,
			)
		}
	}
}
