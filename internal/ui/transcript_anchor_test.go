package ui

import "testing"

func transcriptLayout(spec ...struct {
	id     string
	turn   string
	height int
	lines  LineMap
}) []TranscriptLayoutRecord {
	records := make([]TranscriptLayoutRecord, 0, len(spec))
	start := 0
	for _, item := range spec {
		records = append(records, TranscriptLayoutRecord{
			BlockID:  BlockID(item.id),
			TurnID:   TurnID(item.turn),
			Revision: 1,
			Height:   item.height,
			StartRow: start,
			Exact:    true,
			LineMap:  item.lines,
		})
		start += item.height
	}
	return records
}

func transcriptSnapshot(records []TranscriptLayoutRecord) TranscriptLayoutSnapshot {
	return TranscriptLayoutSnapshot{Records: records}
}

func transcriptSessionSnapshot(sessionID int64, records []TranscriptLayoutRecord) TranscriptLayoutSnapshot {
	return TranscriptLayoutSnapshot{SessionID: sessionID, Records: records}
}

func TestResolveTranscriptAnchorPreservesSemanticPointAndScreenRow(t *testing.T) {
	layout := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "turn_1", 4, nil},
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{
			"b", "turn_1", 12,
			LineMap{
				{LogicalOffset: 0, Grapheme: 0, Row: 0},
				{LogicalOffset: 10, Grapheme: 0, Row: 3},
				{LogicalOffset: 10, Grapheme: 8, Row: 5},
				{LogicalOffset: 20, Grapheme: 0, Row: 9},
			},
		},
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"c", "turn_2", 20, nil},
	)
	anchor := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("b"), TurnID: TurnID("turn_1"),
		LogicalOffset: 10, Grapheme: 8, ScreenRow: 7, Bias: AnchorBiasNext,
	})

	resolved, err := ResolveTranscriptAnchor(anchor, transcriptSnapshot(layout), transcriptSnapshot(layout), 12)
	if err != nil {
		t.Fatalf("ResolveTranscriptAnchor: %v", err)
	}
	if resolved.Reason != AnchorResolutionExactBlock || resolved.BlockID != BlockID("b") {
		t.Fatalf("resolved wrong block: %+v", resolved)
	}
	if resolved.LocalRow != 5 || resolved.MappedRow != 9 {
		t.Fatalf("resolved wrong semantic row: %+v", resolved)
	}
	if resolved.ViewportTop != 2 || resolved.ScreenRow != 7 || resolved.ViewportClamped || resolved.ContentClamped {
		t.Fatalf("did not preserve requested screen row: %+v", resolved)
	}
}

func TestResolveTranscriptAnchorReportsContentAndViewportClamp(t *testing.T) {
	layout := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{
			"a", "turn_1", 3,
			LineMap{{LogicalOffset: 0, Grapheme: 0, Row: 0}, {LogicalOffset: 4, Grapheme: 0, Row: 2}},
		},
	)
	anchor := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("a"), TurnID: TurnID("turn_1"),
		LogicalOffset: 99, ScreenRow: 8, Bias: AnchorBiasPrevious,
	})

	resolved, err := ResolveTranscriptAnchor(anchor, transcriptSnapshot(layout), transcriptSnapshot(layout), 10)
	if err != nil {
		t.Fatalf("ResolveTranscriptAnchor: %v", err)
	}
	if resolved.LocalRow != 2 || !resolved.ContentClamped {
		t.Fatalf("semantic coordinate did not clamp to the last mapped row: %+v", resolved)
	}
	if resolved.ViewportTop != 0 || resolved.ScreenRow != 2 || !resolved.ViewportClamped {
		t.Fatalf("document boundary clamp was not reported: %+v", resolved)
	}
}

func TestResolveTranscriptAnchorKeepsPointVisibleWhenViewportShrinks(t *testing.T) {
	layout := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "turn_1", 30, LineMap{{LogicalOffset: 18, Row: 18}}},
	)
	anchor := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("a"), TurnID: TurnID("turn_1"),
		LogicalOffset: 18, ScreenRow: 15, Bias: AnchorBiasNext,
	})

	resolved, err := ResolveTranscriptAnchor(anchor, transcriptSnapshot(layout), transcriptSnapshot(layout), 6)
	if err != nil {
		t.Fatalf("ResolveTranscriptAnchor: %v", err)
	}
	if resolved.ScreenRow != 5 || resolved.ViewportTop != 13 || !resolved.ViewportClamped {
		t.Fatalf("shrunk viewport did not clamp anchor onto its last visible row: %+v", resolved)
	}
}

func TestResolveTranscriptAnchorDeletedBlockFallbacks(t *testing.T) {
	previous := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "turn_1", 2, nil},
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"deleted", "turn_1", 3, nil},
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"c", "turn_2", 4, nil},
	)
	current := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "turn_1", 5, nil},
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"c", "turn_2", 7, nil},
	)

	tests := []struct {
		name      string
		bias      AnchorBias
		wantBlock BlockID
		wantRow   int
		wantWhy   AnchorResolutionReason
	}{
		{"next bias selects next survivor", AnchorBiasNext, BlockID("c"), 0, AnchorResolutionNextBlock},
		{"previous bias selects prior survivor end", AnchorBiasPrevious, BlockID("a"), 4, AnchorResolutionPreviousBlock},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			anchor := ManualTranscriptAnchor(SemanticAnchor{
				BlockID: BlockID("deleted"), TurnID: TurnID("turn_1"),
				ScreenRow: 2, Bias: test.bias,
			})
			resolved, err := ResolveTranscriptAnchor(
				anchor,
				transcriptSnapshot(previous),
				transcriptSnapshot(current),
				6,
			)
			if err != nil {
				t.Fatalf("ResolveTranscriptAnchor: %v", err)
			}
			if resolved.Reason != test.wantWhy || resolved.BlockID != test.wantBlock || resolved.LocalRow != test.wantRow {
				t.Fatalf("unexpected fallback: %+v", resolved)
			}
		})
	}
}

func TestResolveTranscriptAnchorFallsBackToTurnThenDocumentTop(t *testing.T) {
	previous := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"deleted", "turn_1", 3, nil},
	)
	current := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"turn-start", "turn_1", 2, nil},
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"other", "turn_2", 4, nil},
	)
	anchor := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("deleted"), Bias: AnchorBiasPrevious,
	})

	resolved, err := ResolveTranscriptAnchor(
		anchor,
		transcriptSnapshot(previous),
		transcriptSnapshot(current),
		4,
	)
	if err != nil {
		t.Fatalf("ResolveTranscriptAnchor turn fallback: %v", err)
	}
	if resolved.Reason != AnchorResolutionTurnStart || resolved.BlockID != BlockID("turn-start") {
		t.Fatalf("did not recover the inferred turn start: %+v", resolved)
	}

	unknown := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("unknown"), TurnID: TurnID("missing_turn"), Bias: AnchorBiasNext,
	})
	resolved, err = ResolveTranscriptAnchor(
		unknown,
		transcriptSnapshot(previous),
		transcriptSnapshot(current),
		4,
	)
	if err != nil {
		t.Fatalf("ResolveTranscriptAnchor document fallback: %v", err)
	}
	if resolved.Reason != AnchorResolutionDocumentTop || resolved.BlockID != BlockID("turn-start") || resolved.MappedRow != 0 {
		t.Fatalf("did not recover document top: %+v", resolved)
	}
}

func TestResolveTranscriptAnchorFollowLatest(t *testing.T) {
	current := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "turn_1", 8, nil},
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"b", "turn_1", 7, nil},
	)

	resolved, err := ResolveTranscriptAnchor(
		FollowLatestAnchor(),
		TranscriptLayoutSnapshot{},
		transcriptSnapshot(current),
		6,
	)
	if err != nil {
		t.Fatalf("ResolveTranscriptAnchor: %v", err)
	}
	if resolved.Reason != AnchorResolutionFollowLatest || resolved.BlockID != BlockID("b") {
		t.Fatalf("follow latest resolved wrong block: %+v", resolved)
	}
	if resolved.MappedRow != 14 || resolved.ViewportTop != 9 || resolved.ScreenRow != 5 {
		t.Fatalf("follow latest resolved wrong geometry: %+v", resolved)
	}
}

func TestResolveTranscriptAnchorRejectsContradictoryLayout(t *testing.T) {
	valid := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "turn", 2, nil},
	)
	tests := []struct {
		name    string
		current []TranscriptLayoutRecord
	}{
		{"duplicate IDs", append(valid, valid[0])},
		{"gap in prefix sums", []TranscriptLayoutRecord{{
			BlockID: BlockID("a"), Revision: 1, Height: 2, StartRow: 1,
		}}},
		{"zero height", []TranscriptLayoutRecord{{
			BlockID: BlockID("a"), Revision: 1, Height: 0,
		}}},
		{"unordered line map", []TranscriptLayoutRecord{{
			BlockID: BlockID("a"), Revision: 1, Height: 3,
			LineMap: LineMap{
				{LogicalOffset: 2, Row: 1},
				{LogicalOffset: 1, Row: 2},
			},
		}}},
		{"duplicate line coordinate", []TranscriptLayoutRecord{{
			BlockID: BlockID("a"), Revision: 1, Height: 3,
			LineMap: LineMap{
				{LogicalOffset: 1, Grapheme: 2, Row: 1},
				{LogicalOffset: 1, Grapheme: 2, Row: 2},
			},
		}}},
		{"line row outside block", []TranscriptLayoutRecord{{
			BlockID: BlockID("a"), Revision: 1, Height: 3,
			LineMap: LineMap{{LogicalOffset: 1, Row: 3}},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveTranscriptAnchor(
				FollowLatestAnchor(),
				TranscriptLayoutSnapshot{},
				transcriptSnapshot(test.current),
				3,
			)
			if err == nil {
				t.Fatal("invalid current layout was accepted")
			}
		})
	}

	manual := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("a"), Bias: AnchorBiasNext, ScreenRow: -1,
	})
	if _, err := ResolveTranscriptAnchor(
		manual,
		transcriptSnapshot(valid),
		transcriptSnapshot(valid),
		3,
	); err == nil {
		t.Fatal("negative semantic coordinate was accepted")
	}

	movedTurn := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "new_turn", 2, nil},
	)
	manual = ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("a"), Bias: AnchorBiasNext,
	})
	if _, err := ResolveTranscriptAnchor(
		manual,
		transcriptSnapshot(valid),
		transcriptSnapshot(movedTurn),
		3,
	); err == nil {
		t.Fatal("stable block identity was accepted after moving between turns")
	}

	claimedTurn := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("a"), TurnID: TurnID("claimed_turn"), Bias: AnchorBiasNext,
	})
	if _, err := ResolveTranscriptAnchor(
		claimedTurn,
		transcriptSnapshot(valid),
		TranscriptLayoutSnapshot{},
		3,
	); err == nil {
		t.Fatal("anchor turn contradicting the previous layout was accepted")
	}

	regressed := append([]TranscriptLayoutRecord(nil), valid...)
	regressed[0].Revision = 1
	newerPrevious := append([]TranscriptLayoutRecord(nil), valid...)
	newerPrevious[0].Revision = 2
	if _, err := ResolveTranscriptAnchor(
		ManualTranscriptAnchor(SemanticAnchor{BlockID: BlockID("a"), Bias: AnchorBiasNext}),
		transcriptSnapshot(newerPrevious),
		transcriptSnapshot(regressed),
		3,
	); err == nil {
		t.Fatal("surviving block revision regression was accepted")
	}
}

func TestResolveTranscriptAnchorRejectsCrossSessionScope(t *testing.T) {
	previous := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "turn", 2, nil},
	)
	current := append([]TranscriptLayoutRecord(nil), previous...)

	anchor := ManualTranscriptAnchor(SemanticAnchor{
		SessionID: 42, BlockID: BlockID("a"), Bias: AnchorBiasNext,
	})
	if _, err := ResolveTranscriptAnchor(
		anchor,
		transcriptSessionSnapshot(41, previous),
		transcriptSessionSnapshot(42, current),
		3,
	); err == nil {
		t.Fatal("manual anchor crossed from the previous session into the current session")
	}

	anchor.Manual.SessionID = 41
	if _, err := ResolveTranscriptAnchor(
		anchor,
		transcriptSessionSnapshot(42, previous),
		transcriptSessionSnapshot(42, current),
		3,
	); err == nil {
		t.Fatal("manual anchor from another session was accepted")
	}

	anchor.Manual.SessionID = 42
	if _, err := ResolveTranscriptAnchor(
		anchor,
		transcriptSessionSnapshot(42, previous),
		transcriptSessionSnapshot(99, nil),
		3,
	); err == nil {
		t.Fatal("empty current layout lost its session scope")
	}

	if _, err := ResolveTranscriptAnchor(
		FollowLatestAnchor(),
		TranscriptLayoutSnapshot{},
		transcriptSessionSnapshot(-1, nil),
		3,
	); err == nil {
		t.Fatal("negative current layout session was accepted")
	}
}

func TestResolveTranscriptAnchorIntegerBoundariesFailClosed(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	overflowing := []TranscriptLayoutRecord{
		{
			BlockID: BlockID("a"), Revision: 1,
			Height: maxInt, StartRow: 0,
		},
		{
			BlockID: BlockID("b"), Revision: 1,
			Height: 1, StartRow: maxInt,
		},
	}
	if _, err := ResolveTranscriptAnchor(
		FollowLatestAnchor(),
		TranscriptLayoutSnapshot{},
		transcriptSnapshot(overflowing),
		1,
	); err == nil {
		t.Fatal("layout height overflow was accepted")
	}

	layout := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "turn", 8, LineMap{{LogicalOffset: 0, Row: 0}}},
	)
	anchor := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("a"), ScreenRow: maxInt, Bias: AnchorBiasNext,
	})
	resolved, err := ResolveTranscriptAnchor(
		anchor,
		transcriptSnapshot(layout),
		transcriptSnapshot(layout),
		2,
	)
	if err != nil {
		t.Fatalf("max ScreenRow should clamp without overflow: %v", err)
	}
	if resolved.ScreenRow != 0 || !resolved.ViewportClamped {
		t.Fatalf("max ScreenRow did not clamp safely: %+v", resolved)
	}
}

func TestResolveTranscriptAnchorFallbackIsDeterministic(t *testing.T) {
	previous := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "turn", 2, nil},
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"deleted", "turn", 2, nil},
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"c", "turn", 2, nil},
	)
	current := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"a", "turn", 4, nil},
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"c", "turn", 5, nil},
	)
	anchor := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("deleted"), ScreenRow: 1, Bias: AnchorBiasNext,
	})

	first, err := ResolveTranscriptAnchor(
		anchor,
		transcriptSnapshot(previous),
		transcriptSnapshot(current),
		4,
	)
	if err != nil {
		t.Fatalf("first ResolveTranscriptAnchor: %v", err)
	}
	for iteration := 0; iteration < 100; iteration++ {
		next, err := ResolveTranscriptAnchor(
			anchor,
			transcriptSnapshot(previous),
			transcriptSnapshot(current),
			4,
		)
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		if next != first {
			t.Fatalf("iteration %d changed resolution:\nfirst=%+v\nnext=%+v", iteration, first, next)
		}
	}
}

func TestResolveTranscriptAnchorGeometrySweep(t *testing.T) {
	for blockCount := 1; blockCount <= 12; blockCount++ {
		layout := make([]TranscriptLayoutRecord, 0, blockCount)
		start := 0
		for index := range blockCount {
			height := 1 + (index*7)%11
			layout = append(layout, TranscriptLayoutRecord{
				BlockID:  BlockID("block_" + string(rune('a'+index))),
				TurnID:   TurnID("turn"),
				Revision: 1,
				Height:   height,
				StartRow: start,
				Exact:    true,
				LineMap:  LineMap{{LogicalOffset: 0, Row: 0}},
			})
			start += height
		}
		for viewportHeight := 1; viewportHeight <= 18; viewportHeight++ {
			for requestedScreenRow := 0; requestedScreenRow <= 24; requestedScreenRow++ {
				for _, record := range layout {
					anchor := ManualTranscriptAnchor(SemanticAnchor{
						BlockID: record.BlockID, TurnID: record.TurnID,
						ScreenRow: requestedScreenRow, Bias: AnchorBiasNext,
					})
					resolved, err := ResolveTranscriptAnchor(
						anchor,
						transcriptSnapshot(layout),
						transcriptSnapshot(layout),
						viewportHeight,
					)
					if err != nil {
						t.Fatalf(
							"blocks=%d viewport=%d screen=%d block=%q: %v",
							blockCount,
							viewportHeight,
							requestedScreenRow,
							record.BlockID,
							err,
						)
					}
					maxTop := max(0, start-viewportHeight)
					if resolved.ViewportTop < 0 || resolved.ViewportTop > maxTop {
						t.Fatalf("viewport top escaped bounds: %+v total=%d viewport=%d", resolved, start, viewportHeight)
					}
					if resolved.MappedRow < resolved.ViewportTop ||
						resolved.MappedRow >= resolved.ViewportTop+viewportHeight {
						t.Fatalf("anchor point is not visible: %+v viewport=%d", resolved, viewportHeight)
					}
					if resolved.ScreenRow != resolved.MappedRow-resolved.ViewportTop {
						t.Fatalf("screen-row identity failed: %+v", resolved)
					}
				}
			}
		}
	}
}

func TestResolveTranscriptAnchorEmptyDocument(t *testing.T) {
	manual := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("gone"), ScreenRow: 3, Bias: AnchorBiasNext,
	})
	resolved, err := ResolveTranscriptAnchor(
		manual,
		TranscriptLayoutSnapshot{},
		TranscriptLayoutSnapshot{},
		10,
	)
	if err != nil {
		t.Fatalf("ResolveTranscriptAnchor: %v", err)
	}
	if resolved.Reason != AnchorResolutionDocumentTop || resolved.BlockID != "" ||
		resolved.ViewportTop != 0 || !resolved.ViewportClamped {
		t.Fatalf("unexpected empty-document resolution: %+v", resolved)
	}
}

func TestLineMapCoordinatesAreRendererSupplied(t *testing.T) {
	// The renderer supplies grapheme coordinates explicitly: the resolver does
	// not inspect UTF-8 bytes and therefore maps a multi-codepoint grapheme just
	// like any other stable semantic point.
	layout := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{
			"unicode", "turn", 4,
			LineMap{
				{LogicalOffset: 2, Grapheme: 0, Row: 0},
				{LogicalOffset: 2, Grapheme: 1, Row: 2},
			},
		},
	)
	anchor := ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("unicode"), LogicalOffset: 2, Grapheme: 1,
		Bias: AnchorBiasNext,
	})
	resolved, err := ResolveTranscriptAnchor(
		anchor,
		transcriptSnapshot(layout),
		transcriptSnapshot(layout),
		4,
	)
	if err != nil {
		t.Fatalf("ResolveTranscriptAnchor: %v", err)
	}
	if resolved.LocalRow != 2 || resolved.ContentClamped {
		t.Fatalf("grapheme coordinate was treated as a byte offset: %+v", resolved)
	}

	withoutMap := transcriptLayout(
		struct {
			id     string
			turn   string
			height int
			lines  LineMap
		}{"plain", "turn", 4, nil},
	)
	anchor = ManualTranscriptAnchor(SemanticAnchor{
		BlockID: BlockID("plain"), LogicalOffset: 2, Grapheme: 1,
		Bias: AnchorBiasNext,
	})
	resolved, err = ResolveTranscriptAnchor(
		anchor,
		transcriptSnapshot(withoutMap),
		transcriptSnapshot(withoutMap),
		4,
	)
	if err != nil {
		t.Fatalf("ResolveTranscriptAnchor without LineMap: %v", err)
	}
	if resolved.LocalRow != 0 || !resolved.ContentClamped {
		t.Fatalf("resolver guessed a row without renderer coordinates: %+v", resolved)
	}
}
