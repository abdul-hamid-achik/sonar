package ui

import "sort"

// transcriptReflowAnchor captures both semantic intent and a numeric fallback.
// The fallback is used only if the renderer cannot produce a valid semantic
// snapshot; it keeps malformed or legacy state from stealing follow ownership.
type transcriptReflowAnchor struct {
	Intent          TranscriptAnchor
	Previous        TranscriptLayoutSnapshot
	FallbackYOffset int
	Valid           bool
}

func (m *Model) captureTranscriptReflowAnchor() transcriptReflowAnchor {
	capture := transcriptReflowAnchor{
		Intent: FollowLatestAnchor(),
		// Structural publication is copy-on-write, so identity/order remain an
		// exact previous frame without cloning every record and LineMap. The
		// renderer may advance geometry in an identity-stable tail in place;
		// ResolveTranscriptAnchor intentionally reads previous geometry only
		// after a block disappears, which forces structural publication.
		Previous:        m.transcriptLayout,
		FallbackYOffset: m.transcriptYOffset(),
		Valid:           m.ready,
	}
	if !m.followPaused() {
		return capture
	}

	records := capture.Previous.Records
	if len(records) == 0 {
		capture.Valid = false
		return capture
	}
	top := max(0, m.transcriptYOffset())
	recordIndex := sort.Search(len(records), func(index int) bool {
		record := records[index]
		return record.StartRow+record.Height > top
	})
	if recordIndex == len(records) {
		recordIndex = len(records) - 1
	}
	record := records[recordIndex]
	localRow := max(0, top-record.StartRow)

	point, screenRow, ok := anchorPointAtOrAfter(record, localRow, top)
	if !ok && recordIndex+1 < len(records) {
		record = records[recordIndex+1]
		point = TranscriptLinePoint{}
		screenRow = max(0, record.StartRow-top)
		ok = true
	}
	if !ok {
		point = TranscriptLinePoint{}
		screenRow = 0
	}

	capture.Intent = ManualTranscriptAnchor(SemanticAnchor{
		SessionID:     capture.Previous.SessionID,
		BlockID:       record.BlockID,
		TurnID:        record.TurnID,
		LogicalOffset: point.LogicalOffset,
		Grapheme:      point.Grapheme,
		ScreenRow:     screenRow,
		Bias:          AnchorBiasNext,
	})
	return capture
}

func anchorPointAtOrAfter(
	record TranscriptLayoutRecord,
	localRow int,
	viewportTop int,
) (TranscriptLinePoint, int, bool) {
	if len(record.LineMap) == 0 {
		if localRow == 0 {
			return TranscriptLinePoint{}, max(0, record.StartRow-viewportTop), true
		}
		return TranscriptLinePoint{}, 0, false
	}
	index := sort.Search(len(record.LineMap), func(index int) bool {
		return record.LineMap[index].Row >= localRow
	})
	if index == len(record.LineMap) {
		return TranscriptLinePoint{}, 0, false
	}
	point := record.LineMap[index]
	return point, max(0, record.StartRow+point.Row-viewportTop), true
}

func (m *Model) restoreTranscriptReflowAnchor(capture transcriptReflowAnchor) {
	if !capture.Valid {
		m.restoreFollowPosition(m.followPaused(), capture.FallbackYOffset)
		return
	}
	if capture.Intent.Mode == TranscriptAnchorFollowLatest {
		m.resumeFollow()
		return
	}
	resolution, err := ResolveTranscriptAnchor(
		capture.Intent,
		capture.Previous,
		m.transcriptLayout,
		max(1, m.viewport.Height()),
	)
	if err != nil {
		if m.logger != nil {
			m.logger.Error("restore transcript anchor", "error", err)
		}
		m.setTranscriptYOffset(capture.FallbackYOffset)
		m.pauseFollow()
		return
	}
	m.setTranscriptYOffset(resolution.ViewportTop)
	m.pauseFollow()
}
