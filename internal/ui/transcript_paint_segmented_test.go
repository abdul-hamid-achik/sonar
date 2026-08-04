package ui

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestIncrementalMarkdownPrefixMeasuresOnlyMutableTail(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{Kind: "user", Content: "stream formatted prose"}}
	m.state = StateStreaming
	prefix := strings.Repeat(
		"## Stable heading\n\nA stable paragraph with **emphasis** and `code`.\n\n",
		240,
	)
	m.appendTranscriptStreamText(prefix + "mutable answer tail")
	m.invalidateEntryCache()
	m.refreshTranscript()

	cache := &m.transcriptPaint.liveCache
	if !cache.valid || cache.tailRawStart < len(prefix)-2 {
		t.Fatalf(
			"segmented Markdown cache not installed: valid=%v tailStart=%d prefix=%d",
			cache.valid,
			cache.tailRawStart,
			len(prefix),
		)
	}
	if cache.lastRenderedStart < 100 {
		t.Fatalf("fixture produced only %d stable rows", cache.lastRenderedStart)
	}
	rowsAddress := &cache.rows[0]
	lineMapAddress := &cache.lineMap[0]
	stableLineMap := append(LineMap(nil), cache.lineMap[:cache.lastLineMapStart]...)
	cachedMarkdownPrefix := m.md.cachedStreamPrefix

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	m.appendTranscriptStreamText(" plus one token")
	m.refreshTranscript()

	cache = &m.transcriptPaint.liveCache
	if !cache.valid {
		t.Fatal("Markdown tail append missed segmented cache")
	}
	if &cache.rows[0] != rowsAddress {
		t.Fatal("Markdown tail append copied the stable row backing store")
	}
	if &cache.lineMap[0] != lineMapAddress {
		t.Fatal("Markdown tail append copied the stable LineMap backing store")
	}
	if !reflect.DeepEqual(
		stableLineMap,
		cache.lineMap[:len(stableLineMap)],
	) {
		t.Fatal("Markdown tail append changed stable semantic coordinates")
	}
	if m.md.cachedStreamPrefix != cachedMarkdownPrefix {
		t.Fatal("Markdown tail append re-rendered the stable Glamour prefix")
	}
	if probe.blocksMeasured > 2 ||
		probe.lineIndexRowsBuilt > 2 ||
		probe.measureBytesMaterialized > 512 {
		t.Fatalf(
			"Markdown append scaled past mutable tail: blocks=%d rows=%d bytes=%d",
			probe.blocksMeasured,
			probe.lineIndexRowsBuilt,
			probe.measureBytesMaterialized,
		)
	}
	assertIncrementalLiveTailParity(t, m)
	assertIncrementalLiveLineMapParity(t, m)
}

func TestIncrementalFrozenReasoningMeasuresOnlyAnswerTail(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{Kind: "user", Content: "reason, then answer"}}
	m.state = StateStreaming
	m.appendTranscriptThinkingText(
		strings.Repeat("private reasoning row with details\n", 4_000) +
			"frozen reasoning tail",
	)
	m.appendTranscriptStreamText(
		strings.Repeat("stable answer row\n", 400) + "mutable answer tail",
	)
	m.invalidateEntryCache()
	m.refreshTranscript()

	cache := &m.transcriptPaint.liveCache
	if !cache.valid || cache.thinkRawLen != m.thinkBuf.Len() {
		t.Fatalf(
			"frozen reasoning cache not installed: valid=%v think=%d want=%d",
			cache.valid,
			cache.thinkRawLen,
			m.thinkBuf.Len(),
		)
	}
	thinkingRevision := cache.thinkSourceRev
	stableLineMap := append(LineMap(nil), cache.lineMap[:cache.lastLineMapStart]...)

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	m.appendTranscriptStreamText(" plus one token")
	m.refreshTranscript()

	cache = &m.transcriptPaint.liveCache
	if !cache.valid || cache.thinkSourceRev != thinkingRevision {
		t.Fatal("answer append did not preserve frozen reasoning provenance")
	}
	if !reflect.DeepEqual(
		stableLineMap,
		cache.lineMap[:len(stableLineMap)],
	) {
		t.Fatal("answer append changed reasoning/prefix semantic coordinates")
	}
	if probe.blocksMeasured > 2 ||
		probe.lineIndexRowsBuilt > 2 ||
		probe.measureBytesMaterialized > 512 {
		t.Fatalf(
			"answer append repainted frozen reasoning: blocks=%d rows=%d bytes=%d",
			probe.blocksMeasured,
			probe.lineIndexRowsBuilt,
			probe.measureBytesMaterialized,
		)
	}
	assertIncrementalLiveTailParity(t, m)
	assertIncrementalLiveLineMapParity(t, m)
}

func TestIncrementalReasoningResumeAndUntrackedMutationRebuildSafely(t *testing.T) {
	t.Run("tracked reasoning resume", func(t *testing.T) {
		m := newTestModel(t)
		m.entries = []ChatEntry{{Kind: "user", Content: "continue reasoning"}}
		m.state = StateStreaming
		m.appendTranscriptThinkingText("first reasoning phase")
		m.appendTranscriptStreamText(
			strings.Repeat("answer row\n", 80) + "answer tail",
		)
		m.invalidateEntryCache()
		m.refreshTranscript()
		beforeRevision := m.transcriptPaint.liveCache.thinkSourceRev

		probe := &transcriptRenderProbe{}
		m.transcriptRenderProbe = probe
		m.appendTranscriptThinkingText("\nreasoning resumed")
		m.refreshTranscript()

		cache := &m.transcriptPaint.liveCache
		if !cache.valid || cache.thinkSourceRev <= beforeRevision {
			t.Fatalf(
				"reasoning resume did not rebuild provenance: valid=%v before=%d after=%d",
				cache.valid,
				beforeRevision,
				cache.thinkSourceRev,
			)
		}
		if probe.blocksMeasured <= 2 {
			t.Fatalf(
				"reasoning resume extended the old suffix instead of rebuilding: blocks=%d",
				probe.blocksMeasured,
			)
		}
		assertIncrementalLiveTailParity(t, m)
		assertIncrementalLiveLineMapParity(t, m)
	})

	t.Run("untracked reasoning mutation", func(t *testing.T) {
		m := newTestModel(t)
		m.entries = []ChatEntry{{Kind: "user", Content: "do not trust builders"}}
		m.state = StateStreaming
		m.appendTranscriptThinkingText("tracked reasoning")
		m.appendTranscriptStreamText("tracked answer")
		m.invalidateEntryCache()
		m.refreshTranscript()

		replacementLen := m.thinkBuf.Len()
		m.thinkBuf.Reset()
		m.thinkBuf.WriteString(strings.Repeat("x", replacementLen))
		probe := &transcriptRenderProbe{}
		m.transcriptRenderProbe = probe
		m.refreshTranscript()

		if m.transcriptPaint.liveCache.valid {
			t.Fatal("untracked reasoning mutation retained segmented provenance")
		}
		if probe.blocksMeasured != 1 {
			t.Fatalf(
				"untracked reasoning measured %d blocks, want one canonical block",
				probe.blocksMeasured,
			)
		}
		assertIncrementalLiveTailParity(t, m)
	})
}

func TestIncrementalMarkdownBoundaryFenceAndResizeRebuildSafely(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{Kind: "user", Content: "exercise boundaries"}}
	m.state = StateStreaming
	m.appendTranscriptStreamText(
		strings.Repeat("Stable paragraph.\n\n", 80) + "``",
	)
	m.invalidateEntryCache()
	m.refreshTranscript()
	if !m.transcriptPaint.liveCache.valid ||
		m.transcriptPaint.liveCache.usesWorkWidth {
		t.Fatal("pre-fence fixture did not start as segmented prose")
	}

	m.appendTranscriptStreamText("`")
	m.refreshTranscript()
	if !m.transcriptPaint.liveCache.valid ||
		!m.transcriptPaint.liveCache.usesWorkWidth {
		t.Fatal("completed fence opener did not rebuild at work width")
	}
	assertIncrementalLiveTailParity(t, m)
	assertIncrementalLiveLineMapParity(t, m)

	beforeBoundary := m.transcriptPaint.liveCache.tailRawStart
	m.appendTranscriptStreamText("\ncode\n```\n\nnew mutable paragraph")
	m.refreshTranscript()
	if !m.transcriptPaint.liveCache.valid ||
		m.transcriptPaint.liveCache.tailRawStart <= beforeBoundary {
		t.Fatal("safe post-fence boundary did not rebuild stable Markdown prefix")
	}
	assertIncrementalLiveTailParity(t, m)
	assertIncrementalLiveLineMapParity(t, m)

	oldRenderer := m.md
	updated, _ := m.Update(tea.WindowSizeMsg{
		Width:  m.width - 9,
		Height: m.height,
	})
	m = updated.(*Model)
	if m.md == oldRenderer {
		t.Fatal("width change retained the previous Markdown renderer")
	}
	if !m.transcriptPaint.liveCache.valid ||
		m.transcriptPaint.liveCache.markdownRenderer != m.md ||
		m.transcriptPaint.liveCache.contentWidth != m.chatContentWidth() {
		t.Fatal("width change did not rebuild segmented cache geometry")
	}
	assertIncrementalLiveTailParity(t, m)
	assertIncrementalLiveLineMapParity(t, m)
}

func TestIncrementalSegmentedUnicodeAndWorkWidthParity(t *testing.T) {
	tests := []struct {
		name     string
		thinking string
		content  string
		deltas   []string
	}{
		{
			name: "formatted unicode",
			content: strings.Repeat(
				"## Café 你\n\nTexto estable con **énfasis**.\n\n",
				20,
			) + "cola e",
			deltas: []string{"\u0301", " 👩", "\u200d", "💻"},
		},
		{
			name: "formatted work-width table",
			content: strings.Repeat(
				"| Campo | Estado |\n| --- | --- |\n| ancho | estable |\n\n",
				20,
			) + "mutable table explanation",
			deltas: []string{" con", " Unicode", " 你"},
		},
		{
			name: "frozen unicode reasoning",
			thinking: strings.Repeat(
				"razonamiento e\u0301 👩\u200d💻 你\n",
				100,
			) + "fin",
			content: strings.Repeat("Respuesta estable.\n\n", 20) +
				"respuesta mutable",
			deltas: []string{" e", "\u0301", " 🙂"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.entries = []ChatEntry{{Kind: "user", Content: "unicode"}}
			m.state = StateStreaming
			m.appendTranscriptThinkingText(test.thinking)
			m.appendTranscriptStreamText(test.content)
			m.invalidateEntryCache()
			m.refreshTranscript()
			if !m.transcriptPaint.liveCache.valid {
				t.Fatal("segmented Unicode fixture did not install cache")
			}

			for _, delta := range test.deltas {
				m.appendTranscriptStreamText(delta)
				m.refreshTranscript()
				assertIncrementalLiveTailParity(t, m)
				if m.transcriptPaint.liveCache.valid {
					assertIncrementalLiveLineMapParity(t, m)
				}
			}
		})
	}
}

func BenchmarkTranscriptRefreshIncrementalMarkdownPrefix(b *testing.B) {
	for _, blocks := range []int{1, 2_000} {
		b.Run(testNameWithCount("blocks", blocks), func(b *testing.B) {
			m := newTestModel(b)
			m.state = StateStreaming
			m.appendTranscriptStreamText(
				strings.Repeat("Stable **Markdown** paragraph.\n\n", blocks) +
					"mutable tail",
			)
			m.invalidateEntryCache()
			m.refreshTranscript()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.appendTranscriptStreamText("x")
				m.refreshTranscript()
			}
		})
	}
}

func BenchmarkTranscriptRefreshIncrementalFrozenReasoning(b *testing.B) {
	for _, rows := range []int{1, 20_000} {
		b.Run(testNameWithCount("rows", rows), func(b *testing.B) {
			m := newTestModel(b)
			m.state = StateStreaming
			m.appendTranscriptThinkingText(
				strings.Repeat("stable reasoning row\n", rows),
			)
			m.appendTranscriptStreamText("mutable answer")
			m.invalidateEntryCache()
			m.refreshTranscript()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.appendTranscriptStreamText("x")
				m.refreshTranscript()
			}
		})
	}
}

func assertIncrementalLiveLineMapParity(t *testing.T, m *Model) {
	t.Helper()
	cache := &m.transcriptPaint.liveCache
	if !cache.valid {
		t.Fatal("LineMap parity requires a valid segmented cache")
	}
	rows := trimTrailingTranscriptPaintRows(cache.rows)
	want := semanticTranscriptLineMapFromSource(
		strings.Join(rows, "\n"),
		m.liveTailSemanticSource(),
	)
	if !reflect.DeepEqual(cache.lineMap, want) {
		t.Fatalf(
			"incremental LineMap diverged:\n got: %#v\nwant: %#v",
			cache.lineMap,
			want,
		)
	}
}

func testNameWithCount(label string, count int) string {
	return label + "_" + strconv.Itoa(count)
}
