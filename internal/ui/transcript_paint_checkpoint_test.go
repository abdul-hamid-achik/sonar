package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestPlainLiveWrappedLineMatchesCanonicalWrapLine(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		width int
	}{
		{name: "fits with whitespace", line: "  alpha   beta  ", width: 40},
		{name: "words reflow", line: "alpha beta gamma delta", width: 9},
		{name: "long word", line: strings.Repeat("a", 80), width: 13},
		{name: "wide graphemes", line: strings.Repeat("界", 24), width: 11},
		{name: "combining graphemes", line: strings.Repeat("e\u0301", 30), width: 12},
		{name: "emoji zwj", line: strings.Repeat("👩‍💻", 16), width: 9},
		{name: "wrapped whitespace", line: strings.Repeat(" ", 30), width: 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows, rawStart, rowStart, _ := plainLiveWrappedLine(
				test.line,
				test.width,
				false,
			)
			if got, want := strings.Join(rows, "\n"), wrapLine(test.line, test.width); got != want {
				t.Fatalf("checkpoint wrapper diverged:\n--- got ---\n%q\n--- want ---\n%q", got, want)
			}
			if rawStart < 0 || rawStart > len(test.line) {
				t.Fatalf("checkpoint raw start = %d outside 0..%d", rawStart, len(test.line))
			}
			if rowStart < 0 || rowStart >= len(rows) {
				t.Fatalf("checkpoint row = %d outside 0..%d", rowStart, len(rows)-1)
			}
		})
	}
}

func TestHugeSingleLineAppendUsesBoundedWrapCheckpoint(t *testing.T) {
	for _, size := range []int{1 << 10, 1 << 20} {
		t.Run(fmt.Sprintf("bytes_%d", size), func(t *testing.T) {
			m := newHugeSingleLineTranscript(t, strings.Repeat("a", size))
			cache := &m.transcriptPaint.liveCache
			if cache.wrapRawStart < size-cache.wrapWidth*2 {
				t.Fatalf(
					"checkpoint raw start = %d, want within bounded final rows of %d bytes",
					cache.wrapRawStart,
					size,
				)
			}

			probe := &transcriptRenderProbe{}
			m.transcriptRenderProbe = probe
			m.appendTranscriptStreamText("z")
			m.refreshTranscript()

			if !m.transcriptPaint.liveCache.valid {
				t.Fatal("safe append left the incremental live cache")
			}
			if probe.blocksMeasured > 2 || probe.lineIndexRowsBuilt > 2 {
				t.Fatalf(
					"checkpoint append rebuilt historical rows: %#v",
					probe,
				)
			}
			if probe.measureBytesMaterialized > cache.wrapWidth*4 {
				t.Fatalf(
					"checkpoint append measured %d bytes at wrap width %d",
					probe.measureBytesMaterialized,
					cache.wrapWidth,
				)
			}
			assertIncrementalLiveTailParity(t, m)
		})
	}
}

func TestHugeSingleLineCheckpointPreservesUnicodeParity(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		delta string
	}{
		{
			name:  "wide",
			raw:   strings.Repeat("界", 8_000),
			delta: "語",
		},
		{
			name:  "combining continuation",
			raw:   strings.Repeat("e\u0301", 8_000) + "e",
			delta: "\u0301",
		},
		{
			name:  "emoji zwj continuation",
			raw:   strings.Repeat("🙂", 8_000) + "👩",
			delta: "\u200d💻",
		},
		{
			name:  "word migrates from final row",
			raw:   strings.Repeat("alpha ", 8_000) + "beta",
			delta: "gamma",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newHugeSingleLineTranscript(t, test.raw)
			probe := &transcriptRenderProbe{}
			m.transcriptRenderProbe = probe

			m.appendTranscriptStreamText(test.delta)
			m.refreshTranscript()

			if !m.transcriptPaint.liveCache.valid {
				t.Fatal("Unicode-safe append left the incremental live cache")
			}
			if probe.blocksMeasured > 3 || probe.lineIndexRowsBuilt > 3 {
				t.Fatalf("Unicode append rebuilt stable rows: %#v", probe)
			}
			for _, row := range m.transcriptPaint.liveCache.rows {
				// Content-grid left chrome (accent+pad) sits outside wrapWidth.
				maxRow := m.transcriptPaint.liveCache.wrapWidth + contentLeftColumns
				if width := lipgloss.Width(row); width > maxRow {
					t.Fatalf(
						"row width = %d, want <= indented wrap width %d: %q",
						width,
						maxRow,
						row,
					)
				}
			}
			assertIncrementalLiveTailParity(t, m)
		})
	}
}

func TestHugeSingleLineCheckpointPreservesPausedAnchor(t *testing.T) {
	m := newHugeSingleLineTranscript(t, strings.Repeat("anchor", 32_000))
	live := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1]
	m.setTranscriptYOffset(live.StartRow + 1_000)
	m.pauseFollow()
	before := m.viewport.GetContent()

	m.appendTranscriptStreamText("tail")
	m.refreshTranscript()

	if !m.followPaused() {
		t.Fatal("checkpoint append resumed paused follow")
	}
	if after := m.viewport.GetContent(); after != before {
		t.Fatal("checkpoint append repainted the stable paused window")
	}
}

func TestHugeSingleLineCheckpointRebuildsAfterWidthChange(t *testing.T) {
	m := newHugeSingleLineTranscript(t, strings.Repeat("width", 40_000))
	oldWrapWidth := m.transcriptPaint.liveCache.wrapWidth

	m.handleWindowSize(tea.WindowSizeMsg{Width: 54, Height: 20}, nil)
	if !m.transcriptPaint.liveCache.valid {
		t.Fatal("width reflow did not rebuild the live checkpoint")
	}
	if got := m.transcriptPaint.liveCache.wrapWidth; got == oldWrapWidth {
		t.Fatalf("width reflow retained wrap width %d", got)
	}

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	m.appendTranscriptStreamText("x")
	m.refreshTranscript()
	if probe.blocksMeasured > 2 || probe.lineIndexRowsBuilt > 2 {
		t.Fatalf("post-reflow checkpoint append rebuilt stable rows: %#v", probe)
	}
	assertIncrementalLiveTailParity(t, m)
}

func TestTranscriptLiveWorkWidthClassificationIsBidirectional(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantUses    bool
		wantMutable bool
	}{
		{
			name:     "plain stable",
			content:  "ordinary prose",
			wantUses: false,
		},
		{
			name:        "potential delimiter",
			content:     "Header | State\n--",
			wantUses:    false,
			wantMutable: true,
		},
		{
			name:        "mutable table",
			content:     "Header | State\n--- | ---",
			wantUses:    true,
			wantMutable: true,
		},
		{
			name:     "invalid delimiter is stable prose",
			content:  "Header | State\n--- | ---x",
			wantUses: false,
		},
		{
			name:     "settled table before mutable row",
			content:  "Header | State\n--- | ---\nready",
			wantUses: true,
		},
		{
			name:     "indented cause is append stable",
			content:  "    make test",
			wantUses: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			uses, mutable := transcriptLiveWorkWidthClassification(test.content)
			if uses != test.wantUses || mutable != test.wantMutable {
				t.Fatalf(
					"classification = uses:%t mutable:%t, want uses:%t mutable:%t",
					uses,
					mutable,
					test.wantUses,
					test.wantMutable,
				)
			}
			if uses != markdownUsesWorkWidth(test.content) {
				t.Fatalf(
					"classification uses=%t diverges from canonical %t",
					uses,
					markdownUsesWorkWidth(test.content),
				)
			}
		})
	}
}

func TestMutableWorkWidthCauseFallsBackToCanonicalProse(t *testing.T) {
	m := newTestModel(t)
	m.handleWindowSize(tea.WindowSizeMsg{Width: 180, Height: 24}, nil)
	m.entries = []ChatEntry{{Kind: "user", Content: "classify the table"}}
	m.state = StateStreaming
	m.appendTranscriptStreamText("Header | State\n--- | ---")
	m.invalidateEntryCache()
	m.refreshTranscript()
	if cache := m.transcriptPaint.liveCache; !cache.valid ||
		!cache.usesWorkWidth ||
		!cache.workWidthMutable {
		t.Fatalf("mutable table did not enter guarded work-width cache: %#v", cache)
	}

	m.appendTranscriptStreamText("x")
	m.refreshTranscript()

	if markdownUsesWorkWidth(m.streamBuf.String()) {
		t.Fatal("fixture still classifies as work width after invalidating append")
	}
	if cache := m.transcriptPaint.liveCache; !cache.valid ||
		cache.usesWorkWidth ||
		cache.workWidthMutable {
		t.Fatalf("cold fallback did not rebuild a prose checkpoint: %#v", cache)
	}
	assertIncrementalLiveTailParity(t, m)
}

func TestStableWorkWidthCauseKeepsBoundedCheckpoint(t *testing.T) {
	m := newTestModel(t)
	m.handleWindowSize(tea.WindowSizeMsg{Width: 180, Height: 24}, nil)
	m.entries = []ChatEntry{{Kind: "user", Content: "stream indented output"}}
	m.state = StateStreaming
	m.appendTranscriptStreamText("    " + strings.Repeat("x", 1<<20))
	m.invalidateEntryCache()
	m.refreshTranscript()
	if cache := m.transcriptPaint.liveCache; !cache.valid ||
		!cache.usesWorkWidth ||
		cache.workWidthMutable {
		t.Fatalf("stable work-width cause was not classified correctly: %#v", cache)
	}

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	m.appendTranscriptStreamText("x")
	m.refreshTranscript()
	if !m.transcriptPaint.liveCache.valid {
		t.Fatal("append-stable work-width cause left incremental cache")
	}
	if probe.blocksMeasured > 2 || probe.lineIndexRowsBuilt > 2 {
		t.Fatalf("stable work-width append rebuilt history: %#v", probe)
	}
	assertIncrementalLiveTailParity(t, m)
}

func TestUnsafeHugeLineAppendFallsBackToCanonicalRenderer(t *testing.T) {
	m := newHugeSingleLineTranscript(t, strings.Repeat("safe", 32_000))
	m.appendTranscriptStreamText("\x1b[31m")
	m.refreshTranscript()
	if m.transcriptPaint.liveCache.valid {
		t.Fatal("terminal-unsafe append remained in trusted incremental cache")
	}
	assertIncrementalLiveTailParity(t, m)
}

func BenchmarkTranscriptHugeSingleLineCheckpointAppend(b *testing.B) {
	for _, size := range []int{1 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			m := newTestModel(b)
			wrapWidth := max(10, min(m.chatContentWidth(), m.chatProseWidth()))
			lineSize := size - size%wrapWidth
			m.entries = []ChatEntry{{Kind: "user", Content: "benchmark checkpoint"}}
			m.state = StateStreaming
			m.appendTranscriptStreamText(strings.Repeat("a", lineSize))
			m.invalidateEntryCache()
			m.refreshTranscript()
			m.transcriptGotoBottom()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.appendTranscriptStreamText("x")
				m.refreshTranscript()
			}
		})
	}
}

func newHugeSingleLineTranscript(t testing.TB, raw string) *Model {
	t.Helper()
	m := newTestModel(t)
	m.entries = []ChatEntry{{Kind: "user", Content: "stream one huge line"}}
	m.state = StateStreaming
	m.appendTranscriptStreamText(raw)
	m.invalidateEntryCache()
	m.refreshTranscript()
	if !m.transcriptPaint.liveCache.valid {
		t.Fatal("huge plain line did not enter incremental cache")
	}
	return m
}
