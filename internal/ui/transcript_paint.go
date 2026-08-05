package ui

import (
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rivo/uniseg"
)

// transcriptPaintOverscanRows keeps a small runway around the visible rows so
// wheel and line-at-a-time movement do not rebuild the Bubbles paint surface on
// every event. The bound is intentionally independent of transcript length.
const transcriptPaintOverscanRows = 4

// transcriptStreamAppendHeadroomBytes keeps ordinary token deltas from
// immediately growing and copying the complete strings.Builder after a large
// append. The builder remains the canonical source; this is bounded amortized
// storage headroom, independent from the wrapping checkpoint.
const transcriptStreamAppendHeadroomBytes = 4 * 1024

// transcriptPaintBlock is one already-measured transcript fragment. Content
// remains owned by the per-entry renderer/memo; lineStarts lets paint extract a
// row inside a very large block without splitting or joining that whole block.
type transcriptPaintBlock struct {
	startRow   int
	content    string
	lineStarts []int
}

func newTranscriptPaintBlock(startRow int, content string) transcriptPaintBlock {
	starts := make([]int, 1, strings.Count(content, "\n")+1)
	starts[0] = 0
	for index := 0; index < len(content); index++ {
		if content[index] == '\n' {
			starts = append(starts, index+1)
		}
	}
	return transcriptPaintBlock{
		startRow:   max(0, startRow),
		content:    content,
		lineStarts: starts,
	}
}

func (block transcriptPaintBlock) rowCount() int {
	if block.content == "" {
		return 0
	}
	return len(block.lineStarts)
}

func (block transcriptPaintBlock) endRow() int {
	return block.startRow + block.rowCount()
}

func (block transcriptPaintBlock) row(index int) string {
	if index < 0 || index >= block.rowCount() {
		return ""
	}
	start := block.lineStarts[index]
	end := len(block.content)
	if index+1 < len(block.lineStarts) {
		end = block.lineStarts[index+1] - 1
		if end > start && block.content[end-1] == '\r' {
			end--
		}
	}
	return block.content[start:end]
}

// transcriptPaintDocument is the renderer-owned logical document. Stable
// blocks are shared with the cache; tail blocks contain only entries after the
// cached prefix plus the current streaming/activity block.
//
// Blank separator rows do not need fragments. Their geometry is already
// represented by block start rows and TranscriptLayoutRecord heights, and the
// bounded row materializer leaves those cells empty.
type transcriptPaintDocument struct {
	base      []transcriptPaintBlock
	tail      []transcriptPaintBlock
	totalRows int
}

func (document transcriptPaintDocument) materializeRows(start, end int) ([]string, int) {
	start = min(max(0, start), document.totalRows)
	end = min(max(start, end), document.totalRows)
	rows := make([]string, end-start)
	painted := materializeTranscriptBlocks(rows, start, end, document.base)
	painted += materializeTranscriptBlocks(rows, start, end, document.tail)
	return rows, painted
}

func materializeTranscriptBlocks(
	destination []string,
	start, end int,
	blocks []transcriptPaintBlock,
) int {
	if len(destination) == 0 || len(blocks) == 0 {
		return 0
	}
	first := sort.Search(len(blocks), func(index int) bool {
		return blocks[index].endRow() > start
	})
	painted := 0
	for index := first; index < len(blocks); index++ {
		block := blocks[index]
		if block.startRow >= end {
			break
		}
		from := max(start, block.startRow)
		to := min(end, block.endRow())
		if from >= to {
			continue
		}
		painted++
		for row := from; row < to; row++ {
			destination[row-start] = block.row(row - block.startRow)
		}
	}
	return painted
}

type transcriptPaintCache struct {
	valid              bool
	entryCount         int
	stableCount        int
	prefixLayoutCount  int
	prefixState        entryRenderState
	stableBlocks       []transcriptPaintBlock
	toolHitRegions     []toolHitRegion
	thinkingHitRegions []thinkingHitRegion
}

// transcriptPaintState keeps global document coordinates out of Bubbles.
// Bubbles owns only the bounded rows in [windowStart, windowEnd); top remains
// the canonical document offset used by follow, anchors, jumps, and pointer
// translation.
type transcriptPaintState struct {
	active              bool
	top                 int
	document            transcriptPaintDocument
	documentGeneration  uint64
	windowGeneration    uint64
	windowStart         int
	windowEnd           int
	windowHeight        int
	windowHighlightRow  int
	cache               transcriptPaintCache
	liveCache           transcriptLivePaintCache
	streamSourceEpoch   uint64
	streamSourceRev     uint64
	streamSourceLen     int
	streamSourceValue   string
	streamSourceTracked bool
	thinkSourceEpoch    uint64
	thinkSourceRev      uint64
	thinkSourceLen      int
	thinkSourceValue    string
	thinkSourceTracked  bool
}

type transcriptPaintBuild struct {
	document   transcriptPaintDocument
	dirtyStart int
}

// transcriptLivePaintCache owns an append-only projection of the provider's
// live tail. A cold build first renders through renderStreamingMsg and admits
// the result only when its mutable plain-text suffix matches byte-for-byte.
// Everything before that suffix (assistant chrome, a frozen reasoning window,
// and a stable Glamour Markdown prefix) remains an immutable measured prefix.
// Appending a token then replaces only the final wrapped suffix row.
//
// Terminal-unsafe input, geometry/theme/renderer changes, untrusted source
// mutations, a resumed reasoning stream, and Markdown boundary changes all
// rebuild through the canonical renderer instead of extending stale rows.
type transcriptLivePaintCache struct {
	valid               bool
	streamSourceEpoch   uint64
	streamSourceRev     uint64
	thinkSourceEpoch    uint64
	thinkSourceRev      uint64
	thinkRawLen         int
	rawLen              int
	tailRawStart        int
	startRow            int
	contentWidth        int
	wrapWidth           int
	usesWorkWidth       bool
	workWidthMutable    bool
	isDark              bool
	glyphProfile        GlyphProfile
	markdownRenderer    *MarkdownRenderer
	blockID             BlockID
	turnID              TurnID
	rows                []string
	blocks              []transcriptPaintBlock
	lineMap             LineMap
	lastRawStart        int
	lastRawLogical      int
	lastRenderedStart   int
	lastBlockStart      int
	lastLineMapStart    int
	previousLineHasPipe bool
	lastLineWrapped     bool
	wrapRawStart        int
	wrapRawLogical      int
	wrapPrefixCellWidth int
	wrapRenderedStart   int
	wrapBlockStart      int
	wrapLineMapStart    int
}

type plainLiveTailProjection struct {
	rows                []string
	lastRenderedStart   int
	previousLineHasPipe bool
	lastLineWrapped     bool
	wrapRawStart        int
	wrapRenderedStart   int
}

// refreshTranscript is the only production boundary that remeasures semantic
// transcript state and stages a bounded paint window. renderEntries remains a
// complete reference materializer for exports and tests, but production event
// handlers must not call it.
func (m *Model) refreshTranscript() {
	paint := &m.transcriptPaint
	reflowAnchor := m.captureTranscriptReflowAnchor()
	oldActive := paint.active
	oldTop := paint.top
	oldDocumentGeneration := paint.documentGeneration
	oldWindowGeneration := paint.windowGeneration
	oldWindowStart := paint.windowStart
	oldWindowEnd := paint.windowEnd
	oldWindowHeight := paint.windowHeight

	build := m.buildTranscriptPaintDocument()
	paint.active = true
	paint.document = build.document
	if paint.documentGeneration < ^uint64(0) {
		paint.documentGeneration++
	}
	paint.windowGeneration = 0

	if !m.ready || m.anchorActive {
		paint.top = m.transcriptMaxTop()
	} else {
		paint.top = min(max(0, paint.top), m.transcriptMaxTop())
	}

	height := max(1, m.viewport.Height())
	visibleEnd := min(paint.document.totalRows, paint.top+height)
	canReuseStableWindow := oldActive &&
		m.followPaused() &&
		oldTop == paint.top &&
		oldWindowGeneration == oldDocumentGeneration &&
		oldWindowHeight == height &&
		oldWindowStart <= paint.top &&
		visibleEnd <= oldWindowEnd &&
		oldWindowEnd <= build.dirtyStart &&
		oldWindowEnd <= paint.document.totalRows
	if canReuseStableWindow {
		paint.windowGeneration = paint.documentGeneration
		paint.windowStart = oldWindowStart
		paint.windowEnd = oldWindowEnd
		paint.windowHeight = oldWindowHeight
		m.syncTranscriptPaintWindow()
		return
	}

	if reflowAnchor.Valid && reflowAnchor.Intent.Mode == TranscriptAnchorManual {
		m.restoreTranscriptReflowAnchor(reflowAnchor)
		return
	}
	m.syncTranscriptPaintWindow()
}

// advanceRunningToolReceiptFrame repaints the running receipts already staged
// in the paint document with the activity clock's current frame, and restages
// the visible window so the change reaches the screen.
//
// It is deliberately not a refresh. A frame changes one glyph cell inside a
// block whose height, semantics, identity, and layout record are all unchanged,
// so none of the machinery that answers "what does this transcript mean" needs
// to run: no document build, no block measure, no semantic digest, no layout
// record. document.base aliases the paint cache's stable blocks, so swapping an
// element in place keeps the two coherent without invalidating either.
//
// Running receipts are the transcript's trailing entries — everything after
// them is the live tail, which lives in document.tail — so both ends are walked
// in lockstep from the end and the loop stops at the first entry that is not
// one. A chunk whose height changed is a layout event rather than a frame: it
// is left alone for the next real lifecycle event to rebuild, because silently
// swapping a taller block would desynchronize every startRow below it.
func (m *Model) advanceRunningToolReceiptFrame() bool {
	if m == nil || !m.hasRunningToolReceipt() {
		return false
	}
	paint := &m.transcriptPaint
	blocks := paint.document.base
	if !paint.active || len(blocks) == 0 {
		return false
	}
	contentW := m.chatContentWidth()
	proseW := min(contentW, m.chatProseWidth())
	advanced := false
	entryIndex, blockIndex := len(m.entries)-1, len(blocks)-1
	for entryIndex >= 0 && blockIndex >= 0 {
		entry := m.entries[entryIndex]
		if entry.Kind != "tool_group" ||
			entry.ToolIndex < 0 || entry.ToolIndex >= len(m.toolEntries) ||
			m.toolEntries[entry.ToolIndex].Status != ToolStatusRunning {
			break
		}
		var view strings.Builder
		m.renderToolGroup(&view, entry)
		chunk := strings.TrimRight(view.String(), "\n")
		block := blocks[blockIndex]
		if chunk == "" || strings.Count(chunk, "\n") != strings.Count(block.content, "\n") {
			break
		}
		blocks[blockIndex] = newTranscriptPaintBlock(block.startRow, chunk)
		// Keep the memo in step. Without this the next full render would rebuild
		// this entry from a key that no longer matches the block on screen.
		if m.entryMemo != nil {
			if key := m.entryMemoKey(entry, contentW, proseW); key != "" {
				m.entryMemo[entry.BlockID] = entryRenderMemo{key: key, chunk: chunk}
			}
		}
		advanced = true
		entryIndex--
		blockIndex--
	}
	if !advanced {
		return false
	}
	// The staged window is keyed on the document generation. Bumping it is what
	// makes syncTranscriptPaintWindow restage instead of reusing the frame the
	// user is already looking at.
	if paint.documentGeneration < ^uint64(0) {
		paint.documentGeneration++
	}
	m.syncTranscriptPaintWindow()
	return true
}

func (m *Model) buildTranscriptPaintDocument() transcriptPaintBuild {
	if m.transcriptRenderProbe != nil {
		m.transcriptRenderProbe.documentBuilds++
	}
	contentW := m.chatContentWidth()
	proseW := m.chatProseWidth()
	if !m.transcriptHasConversation() && !m.hasVisibleLiveTurn() {
		return transcriptPaintBuild{
			document:   m.buildEmptyTranscriptPaintDocument(contentW, proseW),
			dirtyStart: 0,
		}
	}

	identityReady := true
	reconciled, err := m.reconcileTranscriptEntriesForRender()
	if err != nil {
		identityReady = false
		m.resetEntryMemo()
		if m.logger != nil {
			m.logger.Error("reconcile transcript identity for paint", "error", err)
		}
	} else if reconciled {
		liveIDs := make(map[BlockID]struct{}, len(m.entries))
		for _, entry := range m.entries {
			liveIDs[entry.BlockID] = struct{}{}
		}
		for id := range m.entryMemo {
			if _, live := liveIDs[id]; !live {
				delete(m.entryMemo, id)
			}
		}
	}

	cache := &m.transcriptPaint.cache
	if build, ok := m.buildTranscriptFromPaintCache(cache, contentW, identityReady); ok {
		return build
	}

	stableCount := m.stableEntryPrefixLen()
	blocks := make([]transcriptPaintBlock, 0, len(m.entries))
	m.toolHitRegions = m.toolHitRegions[:0]
	m.thinkingHitRegions = m.thinkingHitRegions[:0]
	var state entryRenderState

	snapshotPrefix := func() {
		state.freezeLayoutPrefix()
		cache.valid = true
		cache.entryCount = len(m.entries)
		cache.stableCount = stableCount
		cache.prefixLayoutCount = state.layoutLen()
		cache.prefixState = state
		cache.stableBlocks = append(cache.stableBlocks[:0], blocks...)
		cache.stableBlocks = cache.stableBlocks[:len(cache.stableBlocks):len(cache.stableBlocks)]
		cache.toolHitRegions = append(cache.toolHitRegions[:0], m.toolHitRegions...)
		cache.thinkingHitRegions = append(cache.thinkingHitRegions[:0], m.thinkingHitRegions...)
	}

	for index := range m.entries {
		if index == stableCount {
			snapshotPrefix()
		}
		if block, ok := m.renderTranscriptPaintEntry(index, contentW, identityReady, &state); ok {
			blocks = append(blocks, block)
		}
	}
	if stableCount == len(m.entries) {
		snapshotPrefix()
	}

	tail := m.renderTranscriptPaintLiveTail(contentW, &state)
	totalRows := transcriptRenderStateHeight(&state)
	m.finalizeTranscriptLayoutHeight(&state, totalRows)
	m.bindCachedTranscriptPaintLayoutPrefix()
	return transcriptPaintBuild{
		document: transcriptPaintDocument{
			base:      cache.stableBlocks,
			tail:      tail,
			totalRows: totalRows,
		},
		dirtyStart: 0,
	}
}

// buildTranscriptFromPaintCache admits both an unchanged transcript and strict
// append-only growth. Existing blocks may be reused only while every formerly
// measured entry remains inside the stable prefix. All mutation paths must call
// invalidateEntryCache, which clears cache.valid before this boundary.
//
// Appended entries are promoted into the immutable prefix before the live tail
// is rendered. A later stream repaint therefore measures only that live block,
// while another append starts from the newly promoted prefix.
func (m *Model) buildTranscriptFromPaintCache(
	cache *transcriptPaintCache,
	contentW int,
	identityReady bool,
) (transcriptPaintBuild, bool) {
	if cache == nil ||
		!cache.valid ||
		cache.entryCount > len(m.entries) ||
		cache.stableCount != cache.entryCount ||
		m.stableEntryPrefixLen() != len(m.entries) {
		return transcriptPaintBuild{}, false
	}

	dirtyStart := transcriptRenderStateHeight(&cache.prefixState)
	oldEntryCount := cache.entryCount
	m.toolHitRegions = append(m.toolHitRegions[:0], cache.toolHitRegions...)
	m.thinkingHitRegions = append(m.thinkingHitRegions[:0], cache.thinkingHitRegions...)
	state := cache.prefixState
	appendedBlocks := make(
		[]transcriptPaintBlock,
		0,
		len(m.entries)-oldEntryCount,
	)
	for index := oldEntryCount; index < len(m.entries); index++ {
		if block, ok := m.renderTranscriptPaintEntry(index, contentW, identityReady, &state); ok {
			appendedBlocks = append(appendedBlocks, block)
		}
	}

	if oldEntryCount != len(m.entries) {
		state.freezeLayoutPrefix()
		promotedBlocks := make(
			[]transcriptPaintBlock,
			0,
			len(cache.stableBlocks)+len(appendedBlocks),
		)
		promotedBlocks = append(promotedBlocks, cache.stableBlocks...)
		promotedBlocks = append(promotedBlocks, appendedBlocks...)

		cache.entryCount = len(m.entries)
		cache.stableCount = len(m.entries)
		cache.prefixLayoutCount = state.layoutLen()
		cache.prefixState = state
		cache.stableBlocks = promotedBlocks[:len(promotedBlocks):len(promotedBlocks)]
		cache.toolHitRegions = append(cache.toolHitRegions[:0], m.toolHitRegions...)
		cache.thinkingHitRegions = append(cache.thinkingHitRegions[:0], m.thinkingHitRegions...)
	}

	state = cache.prefixState
	tail := m.renderTranscriptPaintLiveTail(contentW, &state)
	totalRows := transcriptRenderStateHeight(&state)
	m.finalizeTranscriptLayoutHeight(&state, totalRows)
	m.bindCachedTranscriptPaintLayoutPrefix()
	return transcriptPaintBuild{
		document: transcriptPaintDocument{
			base:      cache.stableBlocks,
			tail:      tail,
			totalRows: totalRows,
		},
		dirtyStart: dirtyStart,
	}, true
}

func (m *Model) buildEmptyTranscriptPaintDocument(contentW, proseW int) transcriptPaintDocument {
	var welcomeBuilder strings.Builder
	m.renderWelcome(&welcomeBuilder)
	welcome := welcomeBuilder.String()

	hasNotice := false
	for _, entry := range m.entries {
		if entry.Kind == "system" || entry.Kind == "error" {
			hasNotice = true
			break
		}
	}

	m.endLiveTailLayoutEpisode()
	m.publishTranscriptLayout(nil)
	if !hasNotice {
		welcome = strings.TrimRight(welcome, "\n")
		top := emptyWelcomeTopPad(m.viewport.Height(), lipgloss.Height(welcome))
		if welcome == "" {
			return transcriptPaintDocument{}
		}
		block := m.measureTranscriptPaintBlock(top, welcome)
		return transcriptPaintDocument{
			base:      []transcriptPaintBlock{block},
			totalRows: top + block.rowCount(),
		}
	}

	blocks := make([]transcriptPaintBlock, 0, len(m.entries)+1)
	row := 0
	appendChunk := func(chunk string) {
		if chunk == "" {
			return
		}
		blocks = append(blocks, m.measureTranscriptPaintBlock(row, chunk))
		row += strings.Count(chunk, "\n")
	}
	appendChunk(welcome + "\n")
	for _, entry := range m.entries {
		switch entry.Kind {
		case "system":
			appendChunk(m.renderSystemNotice(entry.Content, proseW) + "\n")
		case "error":
			if notice, ok := compactOllamaStartupNotice(entry.Content, contentW, m.ollamaOffline); ok {
				appendChunk(m.styles.ErrorText.Render(notice) + "\n")
			} else if isOllamaStartupRecovery(entry.Content, m.ollamaOffline) {
				appendChunk(m.renderSystemNotice(entry.Content, proseW) + "\n")
			} else {
				var rendered strings.Builder
				m.renderEntryError(&rendered, entry.Content, contentW)
				appendChunk(rendered.String())
			}
		}
	}
	return transcriptPaintDocument{
		base:      blocks,
		totalRows: row + 1,
	}
}

func (m *Model) transcriptHasConversation() bool {
	for _, entry := range m.entries {
		if entry.Kind == "user" || entry.Kind == "assistant" {
			return true
		}
	}
	return false
}

func (m *Model) renderTranscriptPaintEntry(
	entryIndex, contentW int,
	memoAllowed bool,
	state *entryRenderState,
) (transcriptPaintBlock, bool) {
	separator := ""
	if state.renderedAny {
		separator = transcriptEntrySeparator(state.previousKind, m.entries[entryIndex].Kind)
	}
	layoutCount := state.layoutLen()
	var rendered strings.Builder
	m.renderEntryInto(&rendered, entryIndex, contentW, memoAllowed, state)
	if state.layoutLen() == layoutCount {
		return transcriptPaintBlock{}, false
	}
	chunk := strings.TrimPrefix(rendered.String(), separator)
	record, ok := lastTranscriptLayoutRecord(state)
	if !ok || chunk == "" {
		return transcriptPaintBlock{}, false
	}
	return m.measureTranscriptPaintBlock(record.StartRow, chunk), true
}

func (m *Model) renderTranscriptPaintLiveTail(
	contentW int,
	state *entryRenderState,
) []transcriptPaintBlock {
	if blocks, ok := m.renderIncrementalPlainLiveTail(contentW, state); ok {
		return blocks
	}
	m.transcriptPaint.liveCache.valid = false

	separator := ""
	if state.renderedAny {
		separator = transcriptEntrySeparator(state.previousKind, "assistant")
	}
	layoutCount := state.layoutLen()
	var rendered strings.Builder
	m.renderLiveTail(&rendered, contentW, state)
	if state.layoutLen() == layoutCount {
		return nil
	}
	chunk := strings.TrimPrefix(rendered.String(), separator)
	record, ok := lastTranscriptLayoutRecord(state)
	if !ok || chunk == "" {
		return nil
	}
	return []transcriptPaintBlock{
		m.measureTranscriptPaintBlock(record.StartRow, chunk),
	}
}

func (m *Model) renderIncrementalPlainLiveTail(
	contentW int,
	state *entryRenderState,
) ([]transcriptPaintBlock, bool) {
	if state == nil {
		return nil, false
	}
	raw := m.streamBuf.String()
	if raw == "" {
		return nil, false
	}
	thinking := m.thinkBuf.String()

	separatorRows := 0
	if state.renderedAny {
		separatorRows = strings.Count(
			transcriptEntrySeparator(state.previousKind, "assistant"),
			"\n",
		)
	}
	startRow := state.renderedLines + separatorRows

	appendTrusted := m.syncTranscriptStreamSource()
	thinkingTrusted := m.syncTranscriptThinkingSource()
	m.beginLiveTailLayoutEpisode()
	blockID, turnID := m.liveTailLayoutIdentity()
	cache := &m.transcriptPaint.liveCache
	if cache.valid &&
		((!appendTrusted &&
			cache.streamSourceEpoch != m.transcriptPaint.streamSourceEpoch) ||
			(!thinkingTrusted &&
				cache.thinkSourceEpoch != m.transcriptPaint.thinkSourceEpoch)) {
		// A direct builder mutation is not an append proof. Let the canonical
		// renderer rebuild this frame instead of deriving a segmented suffix
		// from untrusted provenance.
		return nil, false
	}
	sameProjection := cache.valid &&
		cache.streamSourceEpoch == m.transcriptPaint.streamSourceEpoch &&
		cache.thinkSourceEpoch == m.transcriptPaint.thinkSourceEpoch &&
		cache.thinkSourceRev == m.transcriptPaint.thinkSourceRev &&
		cache.thinkRawLen == len(thinking) &&
		cache.startRow == startRow &&
		cache.contentWidth == contentW &&
		cache.isDark == m.isDark &&
		cache.glyphProfile == m.glyphProfile &&
		cache.markdownRenderer == m.md &&
		cache.blockID == blockID &&
		cache.turnID == turnID

	switch {
	case sameProjection &&
		cache.streamSourceRev == m.transcriptPaint.streamSourceRev &&
		cache.rawLen == len(raw):
		// A non-transcript event requested a frame while the live source and
		// geometry stayed fixed. Reuse every measured row and semantic point.
	case sameProjection &&
		appendTrusted &&
		cache.streamSourceRev < m.transcriptPaint.streamSourceRev &&
		cache.rawLen < len(raw) &&
		m.updateIncrementalPlainLiveTail(cache, raw):
		// The append updater changed only the final raw line and any rows added
		// after it. The stable row fragments remain shared.
	default:
		if strings.TrimSpace(raw) == "" ||
			sanitizeTerminalMultiline(raw) != raw ||
			sanitizeTerminalMultiline(thinking) != thinking {
			return nil, false
		}
		messageWidth := min(contentW, m.chatProseWidth())
		usesWorkWidth, workWidthMutable :=
			transcriptLiveWorkWidthClassification(raw)
		if usesWorkWidth {
			messageWidth = contentW
		}
		// Content-grid Prefix owns the left three cells; wrap the flex content
		// budget only so live paint stays byte-identical to renderStreamingAnswer.
		wrapWidth := max(10, messageWidth)
		tail := raw
		if m.md != nil {
			_, tail = m.md.RenderStreamingFormatted(raw)
		}
		if strings.TrimSpace(tail) == "" {
			// There is no mutable visible suffix to checkpoint yet. The next
			// non-blank append can establish one through a canonical rebuild.
			return nil, false
		}
		tailRawStart := len(raw) - len(tail)
		if tailRawStart < 0 ||
			tailRawStart > len(raw) ||
			raw[tailRawStart:] != tail ||
			!m.buildIncrementalPlainLiveTail(
				cache,
				raw,
				thinking,
				tail,
				tailRawStart,
				startRow,
				contentW,
				wrapWidth,
				usesWorkWidth,
				workWidthMutable,
				blockID,
				turnID,
			) {
			return nil, false
		}
	}

	if state.renderedAny {
		state.renderedLines += separatorRows
	}
	state.appendLayoutRecord(TranscriptLayoutRecord{
		BlockID:  blockID,
		TurnID:   turnID,
		Revision: 1,
		Height:   max(1, len(cache.rows)),
		StartRow: state.renderedLines,
		Exact:    false,
		LineMap:  cache.lineMap,
	})
	state.renderedLines += len(cache.rows)
	state.previousKind = "assistant"
	state.renderedAny = true
	return cache.blocks, true
}

func (m *Model) buildIncrementalPlainLiveTail(
	cache *transcriptLivePaintCache,
	raw string,
	thinking string,
	tail string,
	tailRawStart int,
	startRow, contentW, wrapWidth int,
	usesWorkWidth, workWidthMutable bool,
	blockID BlockID,
	turnID TurnID,
) bool {
	projection := m.plainLiveTailRows(
		tail,
		wrapWidth,
		false,
	)

	var canonical strings.Builder
	m.renderStreamingMsg(&canonical, raw, contentW)
	canonicalText := strings.TrimSuffix(canonical.String(), "\n")
	if canonicalText == "" {
		return false
	}
	canonicalRows := strings.Split(canonicalText, "\n")
	if len(projection.rows) == 0 ||
		len(projection.rows) > len(canonicalRows) {
		return false
	}
	prefixRows := len(canonicalRows) - len(projection.rows)
	for index := range projection.rows {
		if canonicalRows[prefixRows+index] != projection.rows[index] {
			// The incremental path is deliberately not an alternative
			// Markdown/reasoning renderer. If the canonical suffix is not the
			// exact plain projection, keep the complete canonical block.
			return false
		}
	}
	rows := reservePlainLiveRows(canonicalRows)
	blocks := make([]transcriptPaintBlock, 0, len(rows))
	lastRenderedStart := prefixRows + projection.lastRenderedStart
	wrapRenderedStart := prefixRows + projection.wrapRenderedStart
	lastBlockStart := len(blocks)
	wrapBlockStart := len(blocks)
	for index, row := range rows {
		if index == lastRenderedStart {
			lastBlockStart = len(blocks)
		}
		if index == wrapRenderedStart {
			wrapBlockStart = len(blocks)
		}
		if row != "" {
			blocks = append(
				blocks,
				m.measureTranscriptPaintBlock(startRow+index, row),
			)
		}
	}
	if lastRenderedStart == len(rows) {
		lastBlockStart = len(blocks)
	}
	if wrapRenderedStart == len(rows) {
		wrapBlockStart = len(blocks)
	}
	blocks = reservePlainLiveBlocks(blocks)

	mapRows := trimTrailingTranscriptPaintRows(rows)
	semanticSource := raw
	rawLogicalBase := 0
	if strings.TrimSpace(thinking) != "" {
		semanticSource = thinking + "\n" + raw
		rawLogicalBase = uniseg.GraphemeClusterCount(thinking + "\n")
	}
	lineMap := semanticTranscriptLineMapFromSource(
		strings.Join(mapRows, "\n"),
		semanticSource,
	)
	// Most token appends only add semantic rows. Keep spare immutable tail
	// capacity so extending the map does not repeatedly copy its stable prefix.
	lineMapCapacity := len(lineMap) + max(64, len(lineMap)/2)
	reservedLineMap := make(LineMap, len(lineMap), lineMapCapacity)
	copy(reservedLineMap, lineMap)
	lastLineMapStart := transcriptLineMapRowStart(
		reservedLineMap,
		lastRenderedStart,
	)
	wrapLineMapStart := transcriptLineMapRowStart(
		reservedLineMap,
		wrapRenderedStart,
	)
	lastRawRelative := strings.LastIndex(tail, "\n") + 1
	lastRawStart := tailRawStart + lastRawRelative
	tailLogicalBase := rawLogicalBase +
		uniseg.GraphemeClusterCount(raw[:tailRawStart])

	*cache = transcriptLivePaintCache{
		valid:             true,
		streamSourceEpoch: m.transcriptPaint.streamSourceEpoch,
		streamSourceRev:   m.transcriptPaint.streamSourceRev,
		thinkSourceEpoch:  m.transcriptPaint.thinkSourceEpoch,
		thinkSourceRev:    m.transcriptPaint.thinkSourceRev,
		thinkRawLen:       len(thinking),
		rawLen:            len(raw),
		tailRawStart:      tailRawStart,
		startRow:          startRow,
		contentWidth:      contentW,
		wrapWidth:         wrapWidth,
		usesWorkWidth:     usesWorkWidth,
		workWidthMutable:  workWidthMutable,
		isDark:            m.isDark,
		glyphProfile:      m.glyphProfile,
		markdownRenderer:  m.md,
		blockID:           blockID,
		turnID:            turnID,
		rows:              rows,
		blocks:            blocks,
		lineMap:           reservedLineMap,
		lastRawStart:      lastRawStart,
		lastRawLogical: tailLogicalBase +
			uniseg.GraphemeClusterCount(tail[:lastRawRelative]),
		lastRenderedStart:   lastRenderedStart,
		lastBlockStart:      lastBlockStart,
		lastLineMapStart:    lastLineMapStart,
		previousLineHasPipe: projection.previousLineHasPipe,
		lastLineWrapped:     projection.lastLineWrapped,
		wrapRawStart:        tailRawStart + projection.wrapRawStart,
		wrapRawLogical: tailLogicalBase + uniseg.GraphemeClusterCount(
			tail[:projection.wrapRawStart],
		),
		wrapPrefixCellWidth: lipgloss.Width(
			tail[lastRawRelative:projection.wrapRawStart],
		),
		wrapRenderedStart: wrapRenderedStart,
		wrapBlockStart:    wrapBlockStart,
		wrapLineMapStart:  wrapLineMapStart,
	}
	return true
}

func (m *Model) updateIncrementalPlainLiveTail(
	cache *transcriptLivePaintCache,
	raw string,
) bool {
	if cache == nil ||
		!cache.valid ||
		cache.rawLen < 0 ||
		cache.rawLen >= len(raw) ||
		cache.tailRawStart < 0 ||
		cache.tailRawStart > cache.rawLen ||
		cache.lastRawStart < 0 ||
		cache.lastRawStart < cache.tailRawStart ||
		cache.lastRawStart > cache.rawLen ||
		cache.lastRenderedStart < 0 ||
		cache.lastRenderedStart > len(cache.rows) ||
		cache.lastBlockStart < 0 ||
		cache.lastBlockStart > len(cache.blocks) ||
		cache.lastLineMapStart < 0 ||
		cache.lastLineMapStart > len(cache.lineMap) ||
		cache.wrapRawStart < cache.lastRawStart ||
		cache.wrapRawStart > cache.rawLen ||
		cache.wrapPrefixCellWidth < 0 ||
		cache.wrapRenderedStart < cache.lastRenderedStart ||
		cache.wrapRenderedStart > len(cache.rows) ||
		cache.wrapBlockStart < cache.lastBlockStart ||
		cache.wrapBlockStart > len(cache.blocks) ||
		cache.wrapLineMapStart < cache.lastLineMapStart ||
		cache.wrapLineMapStart > len(cache.lineMap) {
		return false
	}
	delta := raw[cache.rawLen:]
	if sanitizeTerminalMultiline(delta) != delta ||
		strings.Contains(delta, "\n\n") ||
		(cache.rawLen > 0 &&
			raw[cache.rawLen-1] == '\n' &&
			len(delta) > 0 &&
			delta[0] == '\n') {
		return false
	}

	deltaHasNewline := strings.Contains(delta, "\n")
	nextUsesWorkWidth := cache.usesWorkWidth
	nextWorkWidthMutable := cache.workWidthMutable
	if deltaHasNewline {
		nextUsesWorkWidth, nextWorkWidthMutable =
			transcriptLiveWorkWidthClassification(raw)
		if nextUsesWorkWidth != cache.usesWorkWidth {
			return false
		}
	} else if !cache.usesWorkWidth &&
		transcriptLiveLineNowUsesWorkWidth(raw[cache.lastRawStart:]) {
		// A fence opener or four-space/tab indentation can emerge one token at
		// a time on the mutable line. Rebuild before changing from prose to work
		// width; inspecting the bounded line prefix avoids a document scan.
		return false
	} else if cache.workWidthMutable {
		// The only non-monotonic work-width cause is a table delimiter in
		// the mutable line (or a prefix which can still become one). Any append
		// can change that classification in either direction, so force the cold
		// whole-document classification/build path.
		return false
	}

	restartRawStart := cache.lastRawStart
	restartRawLogical := cache.lastRawLogical
	restartRenderedStart := cache.lastRenderedStart
	restartBlockStart := cache.lastBlockStart
	restartLineMapStart := cache.lastLineMapStart
	if !deltaHasNewline {
		restartRawStart = cache.wrapRawStart
		restartRawLogical = cache.wrapRawLogical
		restartRenderedStart = cache.wrapRenderedStart
		restartBlockStart = cache.wrapBlockStart
		restartLineMapStart = cache.wrapLineMapStart
	}
	sourceSuffix := raw[restartRawStart:]
	if !deltaHasNewline &&
		cache.lastLineWrapped &&
		cache.wrapPrefixCellWidth+lipgloss.Width(sourceSuffix) <= cache.wrapWidth {
		// A variation selector or another grapheme continuation can reduce the
		// final cluster's cell width. If that makes the complete line fit again,
		// canonical wrapLine preserves its raw whitespace and the prior wrapped
		// rows must be rebuilt as one line.
		return false
	}
	projection := m.plainLiveTailRows(
		sourceSuffix,
		cache.wrapWidth,
		cache.lastLineWrapped,
	)
	if !deltaHasNewline {
		projection.previousLineHasPipe = cache.previousLineHasPipe
	}
	nextLastRenderedStart := restartRenderedStart +
		projection.lastRenderedStart
	nextWrapRenderedStart := restartRenderedStart +
		projection.wrapRenderedStart
	mapRows := trimTrailingTranscriptPaintRows(projection.rows)
	suffixLineMap := semanticTranscriptLineMapFromSource(
		strings.Join(mapRows, "\n"),
		sourceSuffix,
	)
	for index := range suffixLineMap {
		suffixLineMap[index].Row += restartRenderedStart
		suffixLineMap[index].LogicalOffset += restartRawLogical
	}
	previousSuffixMap := cache.lineMap[restartLineMapStart:]
	if len(suffixLineMap) < len(previousSuffixMap) ||
		!transcriptLineMapPrefixEqual(suffixLineMap, previousSuffixMap) {
		return false
	}

	nextLineMap := cache.lineMap
	nextLineMap = append(nextLineMap, suffixLineMap[len(previousSuffixMap):]...)
	nextRows := cache.rows[:restartRenderedStart]
	nextRows = append(nextRows, projection.rows...)
	nextBlocks := cache.blocks[:restartBlockStart]
	nextLastBlockStart := len(nextBlocks)
	nextWrapBlockStart := len(nextBlocks)
	for relativeRow, row := range projection.rows {
		if relativeRow == projection.lastRenderedStart {
			nextLastBlockStart = len(nextBlocks)
		}
		if relativeRow == projection.wrapRenderedStart {
			nextWrapBlockStart = len(nextBlocks)
		}
		if row != "" {
			nextBlocks = append(
				nextBlocks,
				m.measureTranscriptPaintBlock(
					cache.startRow+restartRenderedStart+relativeRow,
					row,
				),
			)
		}
	}
	if projection.lastRenderedStart == len(projection.rows) {
		nextLastBlockStart = len(nextBlocks)
	}
	if projection.wrapRenderedStart == len(projection.rows) {
		nextWrapBlockStart = len(nextBlocks)
	}

	nextLastRawRelative := strings.LastIndex(sourceSuffix, "\n") + 1
	nextLastRawStart := restartRawStart + nextLastRawRelative
	nextLastRawLogical := restartRawLogical +
		uniseg.GraphemeClusterCount(sourceSuffix[:nextLastRawRelative])
	if !deltaHasNewline {
		nextLastRawStart = cache.lastRawStart
		nextLastRawLogical = cache.lastRawLogical
		nextLastRenderedStart = cache.lastRenderedStart
		nextLastBlockStart = cache.lastBlockStart
	}
	nextWrapRawStart := restartRawStart + projection.wrapRawStart
	nextWrapRawLogical := restartRawLogical +
		uniseg.GraphemeClusterCount(sourceSuffix[:projection.wrapRawStart])
	nextWrapPrefixCellWidth := cache.wrapPrefixCellWidth +
		lipgloss.Width(sourceSuffix[:projection.wrapRawStart])
	if deltaHasNewline {
		nextWrapPrefixCellWidth = lipgloss.Width(
			raw[nextLastRawStart:nextWrapRawStart],
		)
	}
	cache.streamSourceRev = m.transcriptPaint.streamSourceRev
	cache.rawLen = len(raw)
	cache.rows = nextRows
	cache.blocks = nextBlocks
	cache.lineMap = nextLineMap
	cache.lastRawStart = nextLastRawStart
	cache.lastRawLogical = nextLastRawLogical
	cache.lastRenderedStart = nextLastRenderedStart
	cache.lastBlockStart = nextLastBlockStart
	cache.lastLineMapStart = transcriptLineMapRowStart(
		nextLineMap,
		nextLastRenderedStart,
	)
	cache.previousLineHasPipe = projection.previousLineHasPipe
	cache.lastLineWrapped = projection.lastLineWrapped
	cache.wrapRawStart = nextWrapRawStart
	cache.wrapRawLogical = nextWrapRawLogical
	cache.wrapPrefixCellWidth = nextWrapPrefixCellWidth
	cache.wrapRenderedStart = nextWrapRenderedStart
	cache.wrapBlockStart = nextWrapBlockStart
	cache.wrapLineMapStart = transcriptLineMapRowStart(
		nextLineMap,
		nextWrapRenderedStart,
	)
	cache.usesWorkWidth = nextUsesWorkWidth
	cache.workWidthMutable = nextWorkWidthMutable
	return true
}

func transcriptLiveLineNowUsesWorkWidth(line string) bool {
	if line == "" {
		return false
	}
	if line[0] == '\t' || strings.HasPrefix(line, "    ") {
		return true
	}
	markerStart := 0
	for markerStart < len(line) &&
		markerStart < 4 &&
		line[markerStart] == ' ' {
		markerStart++
	}
	if markerStart > 3 || markerStart+3 > len(line) {
		return false
	}
	marker := line[markerStart : markerStart+3]
	return marker == "```" || marker == "~~~"
}

func (m *Model) plainLiveTailRows(
	raw string,
	wrapWidth int,
	forceFirstLineWrap bool,
) plainLiveTailProjection {
	rawLines := strings.Split(raw, "\n")
	rows := make([]string, 0, len(rawLines)+1)
	lastRenderedStart := len(rows)
	wrapRawStart := 0
	wrapRenderedStart := lastRenderedStart
	rawStart := 0
	lastLineWrapped := false
	// Content-grid Prefix(" ") = one space accent + two pad spaces.
	indent := strings.Repeat(" ", contentLeftColumns)
	for index, rawLine := range rawLines {
		if index == len(rawLines)-1 {
			lastRenderedStart = len(rows)
		}
		lineRows, checkpointRaw, checkpointRow, wrapped :=
			plainLiveWrappedLine(
				rawLine,
				wrapWidth,
				index == 0 && forceFirstLineWrap,
			)
		if index == len(rawLines)-1 {
			wrapRawStart = rawStart + checkpointRaw
			wrapRenderedStart = len(rows) + checkpointRow
			lastLineWrapped = wrapped
		}
		for _, row := range lineRows {
			if row != "" {
				row = indent + row
			}
			rows = append(rows, row)
		}
		rawStart += len(rawLine) + 1
	}
	previousLineHasPipe := len(rawLines) > 1 &&
		strings.Contains(rawLines[len(rawLines)-2], "|")
	return plainLiveTailProjection{
		rows:                rows,
		lastRenderedStart:   lastRenderedStart,
		previousLineHasPipe: previousLineHasPipe,
		lastLineWrapped:     lastLineWrapped,
		wrapRawStart:        wrapRawStart,
		wrapRenderedStart:   wrapRenderedStart,
	}
}

// plainLiveWrappedLine mirrors wrapLine and additionally records the raw byte
// and rendered-row start of its final row. Once a line has entered wrap mode,
// forceWrap preserves wrapLine's field-normalization semantics when only that
// bounded final-row suffix is reprocessed.
func plainLiveWrappedLine(
	line string,
	width int,
	forceWrap bool,
) (rows []string, checkpointRaw, checkpointRow int, wrapped bool) {
	chunks, wrapped := wrapLineChunks(line, width, forceWrap)
	rows = make([]string, len(chunks))
	for index := range chunks {
		rows[index] = chunks[index].text
	}
	if !wrapped {
		return rows, 0, 0, false
	}
	checkpointRow = len(chunks) - 1
	checkpointRaw = chunks[checkpointRow].rawStart
	return rows, checkpointRaw, checkpointRow, true
}

func reservePlainLiveRows(rows []string) []string {
	headroom := max(64, min(4096, max(1, len(rows)/8)))
	reserved := make([]string, len(rows), len(rows)+headroom)
	copy(reserved, rows)
	return reserved
}

func reservePlainLiveBlocks(
	blocks []transcriptPaintBlock,
) []transcriptPaintBlock {
	headroom := max(64, min(4096, max(1, len(blocks)/8)))
	reserved := make(
		[]transcriptPaintBlock,
		len(blocks),
		len(blocks)+headroom,
	)
	copy(reserved, blocks)
	return reserved
}

// transcriptLiveWorkWidthClassification separates append-stable causes
// (fences, indentation, or a table before the mutable line) from a delimiter
// in the mutable last line. The latter can flip in either direction on append
// and therefore must fall back to canonical whole-document classification.
func transcriptLiveWorkWidthClassification(content string) (uses, mutable bool) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	last := len(lines) - 1
	for index, line := range lines {
		if _, ok := markdownFenceMarkerStart(line); ok {
			trimmed := strings.TrimLeft(line, " ")
			if len(trimmed) >= 3 &&
				(strings.HasPrefix(trimmed, "```") ||
					strings.HasPrefix(trimmed, "~~~")) {
				return true, false
			}
		}
		if strings.HasPrefix(line, "\t") ||
			strings.HasPrefix(line, "    ") {
			return true, false
		}
		if index == 0 || !strings.Contains(lines[index-1], "|") {
			continue
		}
		if markdownTableDelimiterLine(line) {
			if index < last {
				return true, false
			}
			uses = true
			mutable = true
			continue
		}
		if index == last && transcriptTableDelimiterMayChange(line) {
			mutable = true
		}
	}
	return uses, mutable
}

func transcriptTableDelimiterMayChange(line string) bool {
	for _, char := range line {
		switch char {
		case ' ', '\t', '\r', ':', '-', '|':
		default:
			return false
		}
	}
	return true
}

func trimTrailingTranscriptPaintRows(rows []string) []string {
	end := len(rows)
	for end > 0 && rows[end-1] == "" {
		end--
	}
	return rows[:end]
}

func transcriptLineMapRowStart(lineMap LineMap, row int) int {
	return sort.Search(len(lineMap), func(index int) bool {
		return lineMap[index].Row >= row
	})
}

func transcriptLineMapPrefixEqual(current, previous LineMap) bool {
	if len(current) < len(previous) {
		return false
	}
	for index := range previous {
		if current[index] != previous[index] {
			return false
		}
	}
	return true
}

func (m *Model) measureTranscriptPaintBlock(startRow int, content string) transcriptPaintBlock {
	block := newTranscriptPaintBlock(startRow, content)
	if m.transcriptRenderProbe != nil {
		m.transcriptRenderProbe.blocksMeasured++
		m.transcriptRenderProbe.measureBytesMaterialized += len(content)
		m.transcriptRenderProbe.lineIndexRowsBuilt += block.rowCount()
	}
	return block
}

func lastTranscriptLayoutRecord(state *entryRenderState) (TranscriptLayoutRecord, bool) {
	if state == nil {
		return TranscriptLayoutRecord{}, false
	}
	if len(state.layoutRecords) > 0 {
		return state.layoutRecords[len(state.layoutRecords)-1], true
	}
	if len(state.layoutBase) > 0 {
		return state.layoutBase[len(state.layoutBase)-1], true
	}
	return TranscriptLayoutRecord{}, false
}

func transcriptRenderStateHeight(state *entryRenderState) int {
	if state == nil || !state.renderedAny {
		return 0
	}
	return max(1, state.renderedLines+1)
}

func (m *Model) bindCachedTranscriptPaintLayoutPrefix() {
	cache := &m.transcriptPaint.cache
	if !cache.valid ||
		cache.prefixLayoutCount < 0 ||
		cache.prefixLayoutCount > len(m.transcriptLayout.Records) {
		return
	}
	count := cache.prefixLayoutCount
	cache.prefixState.layoutBase = m.transcriptLayout.Records[:count:count]
	cache.prefixState.layoutRecords = nil
}

func (m *Model) syncTranscriptPaintWindow() {
	paint := &m.transcriptPaint
	if !paint.active {
		return
	}
	paint.top = min(max(0, paint.top), m.transcriptMaxTop())
	height := max(1, m.viewport.Height())
	visibleEnd := min(paint.document.totalRows, paint.top+height)
	highlightRow := m.transcriptSearchHighlightRow()
	if paint.windowGeneration == paint.documentGeneration &&
		paint.windowHeight == height &&
		paint.windowHighlightRow == highlightRow &&
		paint.windowStart <= paint.top &&
		visibleEnd <= paint.windowEnd {
		m.viewport.SetYOffset(paint.top - paint.windowStart)
		return
	}

	start := max(0, paint.top-transcriptPaintOverscanRows)
	end := min(
		paint.document.totalRows,
		paint.top+height+transcriptPaintOverscanRows,
	)
	rows, blocksPainted := paint.document.materializeRows(start, end)
	m.styleTranscriptSearchWindowRows(rows, start, highlightRow)
	m.viewport.SetContentLines(rows)
	m.viewport.SetYOffset(paint.top - start)
	paint.windowStart = start
	paint.windowEnd = end
	paint.windowHeight = height
	paint.windowHighlightRow = highlightRow
	paint.windowGeneration = paint.documentGeneration

	if m.transcriptRenderProbe != nil {
		stagedBytes := max(0, len(rows)-1)
		for _, row := range rows {
			stagedBytes += len(row)
		}
		m.transcriptRenderProbe.paintBytesStaged += stagedBytes
		m.transcriptRenderProbe.paintRowsStaged += len(rows)
		m.transcriptRenderProbe.viewportRowsStaged += len(rows)
		m.transcriptRenderProbe.blocksPainted += blocksPainted
		m.transcriptRenderProbe.windowStart = start
		m.transcriptRenderProbe.windowEnd = end
	}
}

func (m *Model) transcriptVirtualized() bool {
	return m != nil && m.transcriptPaint.active
}

func (m *Model) transcriptYOffset() int {
	if m.transcriptVirtualized() {
		return m.transcriptPaint.top
	}
	return m.viewport.YOffset()
}

func (m *Model) transcriptMaxTop() int {
	if !m.transcriptVirtualized() {
		return max(0, m.viewport.TotalLineCount()-m.viewport.Height())
	}
	return max(0, m.transcriptPaint.document.totalRows-m.viewport.Height())
}

func (m *Model) setTranscriptYOffset(offset int) {
	if !m.transcriptVirtualized() {
		m.viewport.SetYOffset(offset)
		return
	}
	m.transcriptPaint.top = min(max(0, offset), m.transcriptMaxTop())
	m.syncTranscriptPaintWindow()
}

func (m *Model) transcriptAtBottom() bool {
	if !m.transcriptVirtualized() {
		return m.viewport.AtBottom()
	}
	return m.transcriptPaint.top >= m.transcriptMaxTop()
}

func (m *Model) transcriptPastBottom() bool {
	if !m.transcriptVirtualized() {
		return m.viewport.PastBottom()
	}
	return m.transcriptPaint.top > m.transcriptMaxTop()
}

func (m *Model) transcriptGotoTop() {
	m.setTranscriptYOffset(0)
}

func (m *Model) transcriptGotoBottom() {
	m.setTranscriptYOffset(m.transcriptMaxTop())
}

func (m *Model) scrollTranscriptBy(delta int) {
	if delta == 0 {
		return
	}
	m.setTranscriptYOffset(m.transcriptYOffset() + delta)
}

// appendTranscriptStreamText is the sole production writer for provider prose.
// Generation and a zero-copy Builder snapshot prove append-only growth. Normal
// paints compare the same string storage in constant time; a direct replacement
// pays a content comparison once and is adopted only as a cold source episode.
func (m *Model) appendTranscriptStreamText(value string) {
	if value == "" {
		return
	}
	m.adoptTranscriptStreamSource()
	if available := m.streamBuf.Cap() - m.streamBuf.Len(); available < len(value) {
		m.streamBuf.Grow(len(value) + transcriptStreamAppendHeadroomBytes)
	}
	m.streamBuf.WriteString(value)
	m.transcriptPaint.streamSourceLen += len(value)
	m.transcriptPaint.streamSourceValue = m.streamBuf.String()
	incrementTranscriptPaintGeneration(&m.transcriptPaint.streamSourceRev)
}

// resetTranscriptStreamText starts a new append episode. A reset is never
// admitted as an append even if the replacement happens to share the old
// response's length or prefix.
func (m *Model) resetTranscriptStreamText() {
	m.streamBuf.Reset()
	incrementTranscriptPaintGeneration(&m.transcriptPaint.streamSourceEpoch)
	incrementTranscriptPaintGeneration(&m.transcriptPaint.streamSourceRev)
	m.transcriptPaint.streamSourceTracked = true
	m.transcriptPaint.streamSourceLen = 0
	m.transcriptPaint.streamSourceValue = ""
	m.transcriptPaint.liveCache.valid = false
}

// syncTranscriptStreamSource reports whether every mutation since the previous
// paint passed through appendTranscriptStreamText. Tests and restore paths may
// write the builder directly; they are adopted as a fresh cold episode rather
// than being trusted as append-only.
func (m *Model) syncTranscriptStreamSource() bool {
	paint := &m.transcriptPaint
	current := m.streamBuf.String()
	if !paint.streamSourceTracked ||
		paint.streamSourceLen != len(current) ||
		paint.streamSourceValue != current {
		incrementTranscriptPaintGeneration(&paint.streamSourceEpoch)
		incrementTranscriptPaintGeneration(&paint.streamSourceRev)
		paint.streamSourceTracked = true
		paint.streamSourceLen = len(current)
		paint.streamSourceValue = current
		return false
	}
	return true
}

func (m *Model) adoptTranscriptStreamSource() {
	paint := &m.transcriptPaint
	current := m.streamBuf.String()
	if paint.streamSourceTracked &&
		paint.streamSourceLen == len(current) &&
		paint.streamSourceValue == current {
		return
	}
	incrementTranscriptPaintGeneration(&paint.streamSourceEpoch)
	incrementTranscriptPaintGeneration(&paint.streamSourceRev)
	paint.streamSourceTracked = true
	paint.streamSourceLen = len(current)
	paint.streamSourceValue = current
	paint.liveCache.valid = false
}

// appendTranscriptThinkingText is the sole production writer for native or
// tag-derived reasoning. The live cache can reuse a rendered reasoning prefix
// only while this provenance remains byte-for-byte unchanged.
func (m *Model) appendTranscriptThinkingText(value string) {
	if value == "" {
		return
	}
	m.adoptTranscriptThinkingSource()
	if available := m.thinkBuf.Cap() - m.thinkBuf.Len(); available < len(value) {
		m.thinkBuf.Grow(len(value) + transcriptStreamAppendHeadroomBytes)
	}
	m.thinkBuf.WriteString(value)
	m.transcriptPaint.thinkSourceLen += len(value)
	m.transcriptPaint.thinkSourceValue = m.thinkBuf.String()
	incrementTranscriptPaintGeneration(&m.transcriptPaint.thinkSourceRev)
}

// resetTranscriptThinkingText starts a new reasoning provenance episode.
func (m *Model) resetTranscriptThinkingText() {
	m.thinkBuf.Reset()
	incrementTranscriptPaintGeneration(&m.transcriptPaint.thinkSourceEpoch)
	incrementTranscriptPaintGeneration(&m.transcriptPaint.thinkSourceRev)
	m.transcriptPaint.thinkSourceTracked = true
	m.transcriptPaint.thinkSourceLen = 0
	m.transcriptPaint.thinkSourceValue = ""
	m.transcriptPaint.liveCache.valid = false
}

// syncTranscriptThinkingSource reports whether every reasoning mutation since
// the previous paint passed through appendTranscriptThinkingText.
func (m *Model) syncTranscriptThinkingSource() bool {
	paint := &m.transcriptPaint
	current := m.thinkBuf.String()
	if !paint.thinkSourceTracked ||
		paint.thinkSourceLen != len(current) ||
		paint.thinkSourceValue != current {
		incrementTranscriptPaintGeneration(&paint.thinkSourceEpoch)
		incrementTranscriptPaintGeneration(&paint.thinkSourceRev)
		paint.thinkSourceTracked = true
		paint.thinkSourceLen = len(current)
		paint.thinkSourceValue = current
		return false
	}
	return true
}

func (m *Model) adoptTranscriptThinkingSource() {
	paint := &m.transcriptPaint
	current := m.thinkBuf.String()
	if paint.thinkSourceTracked &&
		paint.thinkSourceLen == len(current) &&
		paint.thinkSourceValue == current {
		return
	}
	incrementTranscriptPaintGeneration(&paint.thinkSourceEpoch)
	incrementTranscriptPaintGeneration(&paint.thinkSourceRev)
	paint.thinkSourceTracked = true
	paint.thinkSourceLen = len(current)
	paint.thinkSourceValue = current
	paint.liveCache.valid = false
}

func incrementTranscriptPaintGeneration(generation *uint64) {
	if generation != nil && *generation < ^uint64(0) {
		*generation++
	}
}

// updateTranscriptViewport is the smart-parent input boundary for transcript
// navigation. Vertical messages update global logical coordinates; Bubbles
// receives only horizontal input and paints the already-bounded local rows.
func (m *Model) updateTranscriptViewport(msg tea.Msg) tea.Cmd {
	if !m.transcriptVirtualized() {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return cmd
	}

	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, m.viewport.KeyMap.PageDown):
			m.scrollTranscriptBy(m.viewport.Height())
		case key.Matches(msg, m.viewport.KeyMap.PageUp):
			m.scrollTranscriptBy(-m.viewport.Height())
		case key.Matches(msg, m.viewport.KeyMap.HalfPageDown):
			m.scrollTranscriptBy(m.viewport.Height() / 2) //nolint:mnd
		case key.Matches(msg, m.viewport.KeyMap.HalfPageUp):
			m.scrollTranscriptBy(-m.viewport.Height() / 2) //nolint:mnd
		default:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return cmd
		}
	case tea.MouseWheelMsg:
		if !m.viewport.MouseWheelEnabled {
			return nil
		}
		if msg.Mod.Contains(tea.ModShift) {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return cmd
		}
		switch msg.Button {
		case tea.MouseWheelDown:
			m.scrollTranscriptBy(m.viewport.MouseWheelDelta)
		case tea.MouseWheelUp:
			m.scrollTranscriptBy(-m.viewport.MouseWheelDelta)
		case tea.MouseWheelLeft, tea.MouseWheelRight:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return cmd
		}
	default:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return cmd
	}
	return nil
}
