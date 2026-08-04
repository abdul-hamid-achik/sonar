package ui

import (
	"errors"
	"fmt"
	"sort"
)

// AnchorBias controls which neighboring block is preferred when the anchored
// block no longer survives in the current transcript.
type AnchorBias uint8

const (
	AnchorBiasNext AnchorBias = iota
	AnchorBiasPrevious
)

func (bias AnchorBias) valid() bool {
	return bias == AnchorBiasNext || bias == AnchorBiasPrevious
}

// TranscriptAnchorMode separates the intent to follow new output from a
// manually selected semantic reading position.
type TranscriptAnchorMode uint8

const (
	TranscriptAnchorFollowLatest TranscriptAnchorMode = iota
	TranscriptAnchorManual
)

// SemanticAnchor identifies a logical point inside a block and the viewport
// row where the reader expects that point to remain. TurnID is retained for a
// deterministic turn-start fallback if the block is deleted.
type SemanticAnchor struct {
	SessionID     int64
	BlockID       BlockID
	TurnID        TurnID
	LogicalOffset int
	Grapheme      int
	ScreenRow     int
	Bias          AnchorBias
}

// TranscriptAnchor is a serializable sum type for follow/manual scroll intent.
// Manual is ignored while Mode is TranscriptAnchorFollowLatest.
type TranscriptAnchor struct {
	Mode   TranscriptAnchorMode
	Manual SemanticAnchor
}

// FollowLatestAnchor constructs an anchor that follows the newest output.
func FollowLatestAnchor() TranscriptAnchor {
	return TranscriptAnchor{Mode: TranscriptAnchorFollowLatest}
}

// ManualTranscriptAnchor constructs an anchor for a semantic reading point.
func ManualTranscriptAnchor(anchor SemanticAnchor) TranscriptAnchor {
	return TranscriptAnchor{Mode: TranscriptAnchorManual, Manual: anchor}
}

// TranscriptLinePoint maps a stable logical text position to a rendered local
// row. LogicalOffset and Grapheme are semantic coordinates supplied by the
// block renderer; neither is a byte offset.
type TranscriptLinePoint struct {
	LogicalOffset int
	Grapheme      int
	Row           int
}

// LineMap is ordered lexicographically by LogicalOffset then Grapheme, with
// non-decreasing rendered rows.
type LineMap []TranscriptLinePoint

func (lineMap LineMap) validate(height int) error {
	for index, point := range lineMap {
		if point.LogicalOffset < 0 || point.Grapheme < 0 {
			return fmt.Errorf("line point %d has a negative semantic coordinate", index)
		}
		if point.Row < 0 || point.Row >= height {
			return fmt.Errorf("line point %d row %d is outside block height %d", index, point.Row, height)
		}
		if index == 0 {
			continue
		}
		previous := lineMap[index-1]
		if point.LogicalOffset < previous.LogicalOffset ||
			(point.LogicalOffset == previous.LogicalOffset && point.Grapheme <= previous.Grapheme) {
			return fmt.Errorf("line point %d is not in strict semantic order", index)
		}
		if point.Row < previous.Row {
			return fmt.Errorf("line point %d regresses from row %d to %d", index, previous.Row, point.Row)
		}
	}
	return nil
}

// resolve returns the row at or immediately before the requested semantic
// point. Requests outside the map clamp to its first or last point.
func (lineMap LineMap) resolve(logicalOffset, grapheme int) (row int, exact bool) {
	index := sort.Search(len(lineMap), func(index int) bool {
		point := lineMap[index]
		return point.LogicalOffset > logicalOffset ||
			(point.LogicalOffset == logicalOffset && point.Grapheme >= grapheme)
	})
	if index < len(lineMap) {
		point := lineMap[index]
		if point.LogicalOffset == logicalOffset && point.Grapheme == grapheme {
			return point.Row, true
		}
	}
	if index == 0 {
		return lineMap[0].Row, false
	}
	return lineMap[index-1].Row, false
}

// TranscriptLayoutRecord is the renderer-independent geometry required to
// restore an anchor. StartRow is validated as a contiguous prefix sum so the
// resolver cannot silently consume a stale or contradictory frame.
type TranscriptLayoutRecord struct {
	BlockID  BlockID
	TurnID   TurnID
	Revision uint64
	Height   int
	StartRow int
	Exact    bool
	LineMap  LineMap
}

// TranscriptLayoutSnapshot binds a layout, including an empty layout, to one
// session. Session scope belongs to the snapshot rather than each record so a
// newly created or cleared conversation cannot lose its identity.
type TranscriptLayoutSnapshot struct {
	SessionID int64
	Records   []TranscriptLayoutRecord
}

// AnchorResolutionReason records whether the requested semantic point
// survived or which deterministic fallback selected the resolved block.
type AnchorResolutionReason uint8

const (
	AnchorResolutionFollowLatest AnchorResolutionReason = iota
	AnchorResolutionExactBlock
	AnchorResolutionNextBlock
	AnchorResolutionPreviousBlock
	AnchorResolutionTurnStart
	AnchorResolutionDocumentTop
)

// TranscriptAnchorResolution is a pure projection into viewport coordinates.
// ViewportClamped explains a changed screen row at document boundaries;
// ContentClamped explains a missing exact line-map coordinate.
type TranscriptAnchorResolution struct {
	Reason             AnchorResolutionReason
	BlockID            BlockID
	LocalRow           int
	MappedRow          int
	ViewportTop        int
	ScreenRow          int
	RequestedScreenRow int
	ContentClamped     bool
	ViewportClamped    bool
	LayoutExact        bool
}

// ResolveTranscriptAnchor maps scroll intent into the current layout.
//
// The previous layout is used only as an identity/order snapshot when a block
// was deleted. The fallback order is:
//   - same block and semantic offset;
//   - next surviving block when Bias is AnchorBiasNext;
//   - previous surviving block;
//   - first surviving block in the same turn;
//   - document top.
//
// AnchorBiasPrevious intentionally skips the next-block step.
func ResolveTranscriptAnchor(
	anchor TranscriptAnchor,
	previous TranscriptLayoutSnapshot,
	current TranscriptLayoutSnapshot,
	viewportHeight int,
) (TranscriptAnchorResolution, error) {
	if viewportHeight <= 0 {
		return TranscriptAnchorResolution{}, errors.New("transcript viewport height must be positive")
	}
	if current.SessionID < 0 {
		return TranscriptAnchorResolution{}, errors.New("current transcript layout session ID must be non-negative")
	}

	currentIndex, totalHeight, err := indexCurrentTranscriptLayout(current.Records)
	if err != nil {
		return TranscriptAnchorResolution{}, err
	}
	if anchor.Mode == TranscriptAnchorFollowLatest {
		return resolveFollowLatest(current.Records, totalHeight, viewportHeight), nil
	}
	if anchor.Mode != TranscriptAnchorManual {
		return TranscriptAnchorResolution{}, errors.New("transcript anchor mode is invalid")
	}
	if err := validateSemanticAnchor(anchor.Manual); err != nil {
		return TranscriptAnchorResolution{}, err
	}
	if previous.SessionID < 0 {
		return TranscriptAnchorResolution{}, errors.New("previous transcript layout session ID must be non-negative")
	}
	if anchor.Manual.SessionID != current.SessionID || anchor.Manual.SessionID != previous.SessionID {
		return TranscriptAnchorResolution{}, fmt.Errorf(
			"semantic anchor session %d does not match previous/current layout sessions %d/%d",
			anchor.Manual.SessionID,
			previous.SessionID,
			current.SessionID,
		)
	}

	previousIndex, err := indexTranscriptOrder(previous.Records)
	if err != nil {
		return TranscriptAnchorResolution{}, err
	}

	manual := anchor.Manual
	if err := validateSurvivingLayoutIdentity(previous.Records, current.Records, currentIndex); err != nil {
		return TranscriptAnchorResolution{}, err
	}
	oldIndex, existed := previousIndex[manual.BlockID]
	if existed && manual.TurnID != "" && previous.Records[oldIndex].TurnID != manual.TurnID {
		return TranscriptAnchorResolution{}, fmt.Errorf(
			"semantic anchor block %q claims turn %q, previous layout records turn %q",
			manual.BlockID,
			manual.TurnID,
			previous.Records[oldIndex].TurnID,
		)
	}
	if index, exists := currentIndex[manual.BlockID]; exists {
		record := current.Records[index]
		expectedTurnID := manual.TurnID
		if expectedTurnID == "" && existed {
			expectedTurnID = previous.Records[oldIndex].TurnID
		}
		if expectedTurnID != "" && expectedTurnID != record.TurnID {
			return TranscriptAnchorResolution{}, fmt.Errorf(
				"semantic anchor block %q moved from turn %q to %q",
				manual.BlockID,
				expectedTurnID,
				record.TurnID,
			)
		}
		localRow, exact := resolveRecordPoint(record, manual.LogicalOffset, manual.Grapheme)
		return projectResolvedAnchor(
			AnchorResolutionExactBlock,
			record,
			localRow,
			!exact,
			manual.ScreenRow,
			totalHeight,
			viewportHeight,
		), nil
	}

	turnID := manual.TurnID
	if existed && turnID == "" {
		turnID = previous.Records[oldIndex].TurnID
	}
	if existed {
		if manual.Bias == AnchorBiasNext {
			if record, ok := nextSurvivingRecord(previous.Records, current.Records, currentIndex, oldIndex); ok {
				return projectResolvedAnchor(
					AnchorResolutionNextBlock,
					record,
					0,
					false,
					manual.ScreenRow,
					totalHeight,
					viewportHeight,
				), nil
			}
		}
		if record, ok := previousSurvivingRecord(previous.Records, current.Records, currentIndex, oldIndex); ok {
			return projectResolvedAnchor(
				AnchorResolutionPreviousBlock,
				record,
				record.Height-1,
				false,
				manual.ScreenRow,
				totalHeight,
				viewportHeight,
			), nil
		}
	}

	if turnID != "" {
		for _, record := range current.Records {
			if record.TurnID == turnID {
				return projectResolvedAnchor(
					AnchorResolutionTurnStart,
					record,
					0,
					false,
					manual.ScreenRow,
					totalHeight,
					viewportHeight,
				), nil
			}
		}
	}
	if len(current.Records) == 0 {
		return TranscriptAnchorResolution{
			Reason:             AnchorResolutionDocumentTop,
			RequestedScreenRow: manual.ScreenRow,
			ViewportClamped:    manual.ScreenRow != 0,
		}, nil
	}
	return projectResolvedAnchor(
		AnchorResolutionDocumentTop,
		current.Records[0],
		0,
		false,
		manual.ScreenRow,
		totalHeight,
		viewportHeight,
	), nil
}

func validateSemanticAnchor(anchor SemanticAnchor) error {
	if !anchor.BlockID.Valid() {
		return errors.New("semantic anchor block ID is required and must be canonical")
	}
	if anchor.TurnID != "" && !anchor.TurnID.Valid() {
		return errors.New("semantic anchor turn ID must be canonical")
	}
	if anchor.SessionID < 0 || anchor.LogicalOffset < 0 || anchor.Grapheme < 0 || anchor.ScreenRow < 0 {
		return errors.New("semantic anchor coordinates must be non-negative")
	}
	if !anchor.Bias.valid() {
		return errors.New("semantic anchor bias is invalid")
	}
	return nil
}

func indexCurrentTranscriptLayout(records []TranscriptLayoutRecord) (map[BlockID]int, int, error) {
	index := make(map[BlockID]int, len(records))
	nextStart := 0
	maxInt := int(^uint(0) >> 1)
	for position, record := range records {
		if !record.BlockID.Valid() {
			return nil, 0, fmt.Errorf("current transcript layout record %d has an invalid block ID", position)
		}
		if record.TurnID != "" && !record.TurnID.Valid() {
			return nil, 0, fmt.Errorf("current transcript layout record %d has an invalid turn ID", position)
		}
		if record.Revision == 0 {
			return nil, 0, fmt.Errorf("current transcript layout record %d has a zero revision", position)
		}
		if _, exists := index[record.BlockID]; exists {
			return nil, 0, fmt.Errorf("current transcript layout contains duplicate block ID %q", record.BlockID)
		}
		if record.Height <= 0 {
			return nil, 0, fmt.Errorf("current transcript layout record %d has non-positive height", position)
		}
		if record.StartRow != nextStart {
			return nil, 0, fmt.Errorf(
				"current transcript layout record %d starts at row %d, want %d",
				position,
				record.StartRow,
				nextStart,
			)
		}
		if record.Height > maxInt-nextStart {
			return nil, 0, errors.New("current transcript layout height overflows int")
		}
		if err := record.LineMap.validate(record.Height); err != nil {
			return nil, 0, fmt.Errorf("current transcript layout record %d: %w", position, err)
		}
		index[record.BlockID] = position
		nextStart += record.Height
	}
	return index, nextStart, nil
}

func indexTranscriptOrder(records []TranscriptLayoutRecord) (map[BlockID]int, error) {
	index := make(map[BlockID]int, len(records))
	for position, record := range records {
		if !record.BlockID.Valid() {
			return nil, fmt.Errorf("previous transcript layout record %d has an invalid block ID", position)
		}
		if record.TurnID != "" && !record.TurnID.Valid() {
			return nil, fmt.Errorf("previous transcript layout record %d has an invalid turn ID", position)
		}
		if record.Revision == 0 {
			return nil, fmt.Errorf("previous transcript layout record %d has a zero revision", position)
		}
		if _, exists := index[record.BlockID]; exists {
			return nil, fmt.Errorf("previous transcript layout contains duplicate block ID %q", record.BlockID)
		}
		index[record.BlockID] = position
	}
	return index, nil
}

func validateSurvivingLayoutIdentity(
	previous []TranscriptLayoutRecord,
	current []TranscriptLayoutRecord,
	currentIndex map[BlockID]int,
) error {
	for _, oldRecord := range previous {
		position, survives := currentIndex[oldRecord.BlockID]
		if !survives {
			continue
		}
		newRecord := current[position]
		if newRecord.TurnID != oldRecord.TurnID {
			return fmt.Errorf(
				"transcript block %q moved from turn %q to %q",
				oldRecord.BlockID,
				oldRecord.TurnID,
				newRecord.TurnID,
			)
		}
		if newRecord.Revision < oldRecord.Revision {
			return fmt.Errorf(
				"transcript block %q revision regressed from %d to %d",
				oldRecord.BlockID,
				oldRecord.Revision,
				newRecord.Revision,
			)
		}
	}
	return nil
}

func resolveFollowLatest(
	current []TranscriptLayoutRecord,
	totalHeight int,
	viewportHeight int,
) TranscriptAnchorResolution {
	if len(current) == 0 {
		return TranscriptAnchorResolution{Reason: AnchorResolutionFollowLatest}
	}
	record := current[len(current)-1]
	mappedRow := totalHeight - 1
	viewportTop := max(0, totalHeight-viewportHeight)
	return TranscriptAnchorResolution{
		Reason:      AnchorResolutionFollowLatest,
		BlockID:     record.BlockID,
		LocalRow:    record.Height - 1,
		MappedRow:   mappedRow,
		ViewportTop: viewportTop,
		ScreenRow:   mappedRow - viewportTop,
		LayoutExact: record.Exact,
	}
}

func resolveRecordPoint(record TranscriptLayoutRecord, logicalOffset, grapheme int) (int, bool) {
	if len(record.LineMap) != 0 {
		return record.LineMap.resolve(logicalOffset, grapheme)
	}
	// Without a renderer-supplied LineMap, only the block origin is known.
	// Guessing a row from a byte/rune-like offset would create unstable anchors
	// across wrapping and Unicode normalization.
	return 0, logicalOffset == 0 && grapheme == 0
}

func nextSurvivingRecord(
	previous []TranscriptLayoutRecord,
	current []TranscriptLayoutRecord,
	currentIndex map[BlockID]int,
	oldIndex int,
) (TranscriptLayoutRecord, bool) {
	for index := oldIndex + 1; index < len(previous); index++ {
		if position, survives := currentIndex[previous[index].BlockID]; survives {
			return current[position], true
		}
	}
	return TranscriptLayoutRecord{}, false
}

func previousSurvivingRecord(
	previous []TranscriptLayoutRecord,
	current []TranscriptLayoutRecord,
	currentIndex map[BlockID]int,
	oldIndex int,
) (TranscriptLayoutRecord, bool) {
	for index := oldIndex - 1; index >= 0; index-- {
		if position, survives := currentIndex[previous[index].BlockID]; survives {
			return current[position], true
		}
	}
	return TranscriptLayoutRecord{}, false
}

func projectResolvedAnchor(
	reason AnchorResolutionReason,
	record TranscriptLayoutRecord,
	localRow int,
	contentClamped bool,
	requestedScreenRow int,
	totalHeight int,
	viewportHeight int,
) TranscriptAnchorResolution {
	mappedRow := record.StartRow + localRow
	maxTop := max(0, totalHeight-viewportHeight)
	targetScreenRow := min(requestedScreenRow, viewportHeight-1)
	viewportTop := min(max(mappedRow-targetScreenRow, 0), maxTop)
	screenRow := mappedRow - viewportTop
	return TranscriptAnchorResolution{
		Reason:             reason,
		BlockID:            record.BlockID,
		LocalRow:           localRow,
		MappedRow:          mappedRow,
		ViewportTop:        viewportTop,
		ScreenRow:          screenRow,
		RequestedScreenRow: requestedScreenRow,
		ContentClamped:     contentClamped,
		ViewportClamped:    screenRow != requestedScreenRow,
		LayoutExact:        record.Exact,
	}
}
