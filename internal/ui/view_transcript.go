package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"

	"github.com/abdul-hamid-achik/sonar/internal/imageasset"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func (m *Model) renderSystemNotice(content string, contentW int) string {
	const label = "notice · "
	// Content grid owns the left indent; drop style padding so OriginX stays 3.
	// Dim body — notices (ICE, system) must not compete with user/assistant.
	// Budget accounts for the grid lead so 30-column rows never overflow.
	available := max(1, contentW-contentLeftColumns)
	if contentW < 36 {
		available = max(1, contentW)
	}
	plain := label + sanitizeTerminalMultiline(content)
	wrapped := m.styles.Dimmed.UnsetPaddingLeft().Render(wrapText(plain, available))
	if contentW < 36 {
		return wrapped
	}
	return m.contentGrid().IndentBlock(" ", wrapped)
}

// renderEntries builds the full chat content for the viewport.
// Uses an incremental cache: during streaming, only the streaming tail is
// re-rendered while the entries prefix is reused from cache.
// entriesFromMessages rebuilds the visible chat transcript from a restored
// agent message history. User and assistant text become chat entries; tool
// messages are omitted from the visual (they remain in the agent's context for
// the model) since re-rendering them as cards would need the tool-entry state
// that the snapshot doesn't carry.
func entriesFromMessages(msgs []llm.Message) []ChatEntry {
	var entries []ChatEntry
	for _, msg := range msgs {
		switch msg.Role {
		case "user":
			if msg.Content != "" {
				entries = append(entries, ChatEntry{Kind: "user", Content: msg.Content, Attachments: imageRefsFromMessages(msg.Images)})
			}
		case "assistant":
			if msg.Content != "" {
				entries = append(entries, ChatEntry{Kind: "assistant", Content: msg.Content})
			}
		}
	}
	return entries
}

func (m *Model) renderEntries() string {
	if m.transcriptRenderProbe != nil {
		m.transcriptRenderProbe.renderEntriesCalls++
	}
	contentW := m.chatContentWidth()
	proseW := m.chatProseWidth()
	identityReady := true
	reconciled, err := m.reconcileTranscriptEntriesForRender()
	if err != nil {
		identityReady = false
		m.resetEntryMemo()
		if m.logger != nil {
			m.logger.Error("reconcile transcript identity", "error", err)
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

	// Welcome message when no user messages yet
	hasUserMsg := false
	for _, e := range m.entries {
		if e.Kind == "user" || e.Kind == "assistant" {
			hasUserMsg = true
			break
		}
	}
	if !hasUserMsg && !m.hasVisibleLiveTurn() {
		var b strings.Builder
		m.renderWelcome(&b)
		hasNotice := false
		for _, entry := range m.entries {
			if entry.Kind == "system" || entry.Kind == "error" {
				hasNotice = true
				break
			}
		}
		if !hasNotice {
			welcome := strings.TrimRight(b.String(), "\n")
			top := emptyWelcomeTopPad(m.viewport.Height(), lipgloss.Height(welcome))
			m.endLiveTailLayoutEpisode()
			m.publishTranscriptLayout(nil)
			return m.recordTranscriptRender(strings.Repeat("\n", top) + welcome)
		}
		// Welcome lines already end with a trailing newline from renderWelcome.
		// Start notices on a real row so they sit under the orientation surface.
		b.WriteByte('\n')
		// Append any system entries (e.g. failed server notices) below welcome
		for _, e := range m.entries {
			switch e.Kind {
			case "system":
				b.WriteString(m.renderSystemNotice(e.Content, proseW))
				b.WriteString("\n")
			case "error":
				m.renderEntryError(&b, e.Content, contentW)
			}
		}
		m.endLiveTailLayoutEpisode()
		m.publishTranscriptLayout(nil)
		return m.recordTranscriptRender(b.String())
	}

	// Fast path: the cached stable transcript is current. Running ToolCards are
	// event-driven, so only a streaming tail renders between semantic events.
	if m.entryCacheValid && len(m.entries) == m.cachedEntryCount {
		m.toolHitRegions = append(m.toolHitRegions[:0], m.cachedToolHitRegions...)
		m.thinkingHitRegions = append(m.thinkingHitRegions[:0], m.cachedThinkingHitRegions...)
		var b strings.Builder
		b.WriteString(m.cachedEntriesRender)
		state := m.cachedPrefixState
		for index := m.cachedStableCount; index < len(m.entries); index++ {
			m.renderEntryInto(&b, index, contentW, identityReady, &state)
		}
		m.renderLiveTail(&b, contentW, &state)
		rendered := b.String()
		m.finalizeTranscriptLayout(&state, rendered)
		m.bindCachedTranscriptLayoutPrefix()
		return m.recordTranscriptRender(rendered)
	}

	// Full render: cache every settled or running transcript entry. Tool and
	// expert progress handlers invalidate this cache on real lifecycle events;
	// clocks never make a transcript entry live.
	stableCount := m.stableEntryPrefixLen()
	var b strings.Builder
	m.toolHitRegions = m.toolHitRegions[:0]
	m.thinkingHitRegions = m.thinkingHitRegions[:0]
	var state entryRenderState
	snapshotPrefix := func() {
		state.freezeLayoutPrefix()
		m.cachedEntriesRender = b.String()
		m.cachedEntryCount = len(m.entries)
		m.cachedStableCount = stableCount
		m.cachedPrefixLayoutCount = state.layoutLen()
		m.cachedPrefixState = state
		m.cachedToolHitRegions = append(m.cachedToolHitRegions[:0], m.toolHitRegions...)
		m.cachedThinkingHitRegions = append(m.cachedThinkingHitRegions[:0], m.thinkingHitRegions...)
		m.entryCacheValid = true
	}
	for index := range m.entries {
		if index == stableCount {
			snapshotPrefix()
		}
		m.renderEntryInto(&b, index, contentW, identityReady, &state)
	}
	if stableCount == len(m.entries) {
		snapshotPrefix()
	}
	m.renderLiveTail(&b, contentW, &state)
	rendered := b.String()
	m.finalizeTranscriptLayout(&state, rendered)
	m.bindCachedTranscriptLayoutPrefix()
	return m.recordTranscriptRender(rendered)
}

func (m *Model) recordTranscriptRender(rendered string) string {
	if m.transcriptRenderProbe != nil {
		m.transcriptRenderProbe.transcriptBytesMaterialized += len(rendered)
	}
	return rendered
}

// entryRenderState carries the transcript loop state across the cached stable
// prefix so live entries and the streaming tail continue the exact separator,
// role-header, and hit-region arithmetic of a full render.
type entryRenderState struct {
	renderedLines    int
	previousKind     string
	renderedAny      bool
	assistantStarted bool
	// layoutBase is an immutable prefix, normally sharing the current
	// published snapshot. layoutRecords is the small mutable tail. Rendering
	// a live ToolCard thaws only the previous record plus that live suffix.
	layoutBase    []TranscriptLayoutRecord
	layoutRecords []TranscriptLayoutRecord
}

func (state *entryRenderState) layoutLen() int {
	if state == nil {
		return 0
	}
	return len(state.layoutBase) + len(state.layoutRecords)
}

// freezeLayoutPrefix transfers the currently owned contiguous records into an
// immutable, capacity-clamped prefix. Subsequent appends must allocate a tail
// and cannot mutate the frozen cache or a previously published anchor frame.
func (state *entryRenderState) freezeLayoutPrefix() {
	if state == nil || len(state.layoutRecords) == 0 {
		return
	}
	if len(state.layoutBase) != 0 {
		combined := make([]TranscriptLayoutRecord, 0, state.layoutLen())
		combined = append(combined, state.layoutBase...)
		combined = append(combined, state.layoutRecords...)
		state.layoutBase = combined[:len(combined):len(combined)]
		state.layoutRecords = nil
		return
	}
	state.layoutBase = state.layoutRecords[:len(state.layoutRecords):len(state.layoutRecords)]
	state.layoutRecords = nil
}

// thawLayoutTail copies only the last immutable record because adding the next
// block finalizes that previous record's separator-inclusive height.
func (state *entryRenderState) thawLayoutTail() {
	if state == nil || len(state.layoutRecords) != 0 || len(state.layoutBase) == 0 {
		return
	}
	lastIndex := len(state.layoutBase) - 1
	last := state.layoutBase[lastIndex]
	state.layoutBase = state.layoutBase[:lastIndex:lastIndex]
	state.layoutRecords = append(state.layoutRecords, last)
}

func (state *entryRenderState) closeLayoutAt(endRow int) {
	if state == nil || state.layoutLen() == 0 {
		return
	}
	state.thawLayoutTail()
	last := &state.layoutRecords[len(state.layoutRecords)-1]
	last.Height = max(1, endRow-last.StartRow)
}

func (state *entryRenderState) appendLayoutRecord(record TranscriptLayoutRecord) {
	state.closeLayoutAt(record.StartRow)
	state.layoutRecords = append(state.layoutRecords, record)
}

// stableEntryPrefixLen returns how many leading entries render identically
// between semantic events. Running ToolCards are static transcript receipts;
// their progress handlers invalidate the cache explicitly.
func (m *Model) stableEntryPrefixLen() int {
	return len(m.entries)
}

// entryRenderMemo is one settled entry's rendered chunk plus the composite
// key it was rendered under. A key mismatch means the memo is stale and the
// entry renders fresh; hit regions are recomputed from the chunk either way.
type entryRenderMemo struct {
	key   string
	chunk string
}

func (m *Model) renderEntryInto(b *strings.Builder, entryIndex, contentW int, memoAllowed bool, state *entryRenderState) {
	entry := m.entries[entryIndex]
	// Sticky last-user strip owns the prompt; do not reprint it in the body.
	if entry.Kind == "user" && m.omitUserEntryFromTranscript(entryIndex) {
		state.assistantStarted = false
		return
	}
	// A run of consecutive settled read-family receipts renders as one line at
	// its first entry; the rest render nothing. Both answers come from the same
	// pair of predicates, so exactly one entry in a run produces a chunk, and
	// the 1:1 entry-to-block invariant the paint layer relies on still holds —
	// a chunk-less entry simply contributes no block and no layout record.
	//
	// Every run member is settled by construction, so a run can only change
	// when a tool lifecycle event lands, and those already invalidate the entry
	// cache. Without that, appending a sixth read would have to mutate a block
	// the append-only paint cache considers immutable.
	if m.collapsedReadRunFollower(entryIndex) {
		return
	}
	proseW := min(contentW, m.chatProseWidth())
	memoKey := m.entryMemoKey(entryIndex, contentW, proseW)
	var chunk string
	hit := false
	if memoAllowed && memoKey != "" {
		if memo, ok := m.entryMemo[entry.BlockID]; ok && memo.key == memoKey {
			chunk = memo.chunk
			hit = true
		}
	}
	if !hit {
		var entryView strings.Builder
		switch entry.Kind {
		case "user":
			m.renderUserMsg(&entryView, entry.Content, entry.Attachments, proseW)
		case "assistant":
			m.renderAssistantMsg(&entryView, entry, contentW)
		case "tool_group":
			if run, collapsed := m.collapsedReadRunAt(entryIndex); collapsed {
				m.renderCollapsedReadRun(&entryView, run)
			} else {
				m.renderToolGroup(&entryView, entry)
			}
		case "error":
			m.renderEntryError(&entryView, entry.Content, contentW)
		case "system":
			entryView.WriteString(m.renderSystemNotice(entry.Content, proseW))
			entryView.WriteString("\n")
		}
		chunk = strings.TrimRight(entryView.String(), "\n")
		if memoAllowed && memoKey != "" {
			if m.entryMemo == nil {
				m.entryMemo = make(map[BlockID]entryRenderMemo)
			}
			m.entryMemo[entry.BlockID] = entryRenderMemo{key: memoKey, chunk: chunk}
		}
	}
	if entry.Kind == "user" {
		state.assistantStarted = false
	}
	if chunk == "" {
		return
	}
	if entry.Kind == "assistant" {
		state.assistantStarted = true
	}
	m.appendEntryChunk(b, entryIndex, entry, chunk, state)
}

// appendEntryChunk applies one rendered entry chunk to the transcript
// builder: separator, hit regions derived from the chunk and the running
// line count, and loop-state bookkeeping. Fresh renders and memo hits share
// this path exactly, so a memoized frame is byte- and region-identical.
func (m *Model) appendEntryChunk(b *strings.Builder, entryIndex int, entry ChatEntry, chunk string, state *entryRenderState) {
	if state.renderedAny {
		separator := transcriptEntrySeparator(state.previousKind, entry.Kind)
		b.WriteString(separator)
		state.renderedLines += strings.Count(separator, "\n")
	}
	state.appendLayoutRecord(TranscriptLayoutRecord{
		BlockID:  entry.BlockID,
		TurnID:   entry.TurnID,
		Revision: entry.Revision,
		Height:   max(1, lipgloss.Height(chunk)),
		StartRow: state.renderedLines,
		Exact:    false,
		LineMap:  semanticTranscriptLineMapForEntry(entry, chunk),
	})
	if entry.Kind == "tool_group" {
		header, _, _ := strings.Cut(chunk, "\n")
		if startCol, endCol, ok := renderedLineHorizontalBounds(header); ok {
			m.toolHitRegions = append(m.toolHitRegions, toolHitRegion{
				ToolIndex: entry.ToolIndex,
				Row:       state.renderedLines,
				StartCol:  startCol,
				EndCol:    endCol,
			})
		}
		// The "N lines hidden · ctrl+r" row is the only part of a receipt that
		// names a key, and it was the only part that did not respond to a
		// click: the region above covers the header line alone. Give the cue
		// the same disclosure it advertises.
		if rowOffset, startCol, endCol, ok := m.collapsedToolFooterRegion(entry, chunk); ok {
			m.toolHitRegions = append(m.toolHitRegions, toolHitRegion{
				ToolIndex: entry.ToolIndex,
				Row:       state.renderedLines + rowOffset,
				StartCol:  startCol,
				EndCol:    endCol,
			})
		}
	}
	if entry.Kind == "assistant" && strings.TrimSpace(entry.ThinkingContent) != "" {
		if rowOffset, startCol, endCol, ok := completedThinkingHeaderRegion(chunk, m.glyphProfile); ok {
			m.thinkingHitRegions = append(m.thinkingHitRegions, thinkingHitRegion{
				EntryIndex: entryIndex,
				Row:        state.renderedLines + rowOffset,
				StartCol:   startCol,
				EndCol:     endCol,
				Digest:     reasoningReceiptDigest(entry.ThinkingContent),
			})
		}
	}
	b.WriteString(chunk)
	state.renderedLines += strings.Count(chunk, "\n")
	state.previousKind = entry.Kind
	state.renderedAny = true
}

// semanticTranscriptLineMap gives reflow a renderer-owned logical coordinate
// for every visible row. It uses ANSI-free grapheme counts rather than byte
// offsets. Repeated rails and indentation are removed from the coordinate so
// width changes do not make presentation chrome look like message content.
//
// Glamour can transform Markdown, so the map is deliberately marked inexact
// by its TranscriptLayoutRecord. The resolver still preserves block identity
// and the closest stable content coordinate instead of a document-wide row.
func semanticTranscriptLineMap(chunk string) LineMap {
	lines := strings.Split(chunk, "\n")
	if len(lines) == 0 {
		return nil
	}
	lineMap := make(LineMap, 0, len(lines))
	logicalOffset := 0
	for row, line := range lines {
		lineMap = append(lineMap, TranscriptLinePoint{
			LogicalOffset: logicalOffset,
			Row:           row,
		})
		semantic := strings.TrimSpace(ansi.Strip(line))
		for _, rail := range []string{"│ ", "▌ ", "↳ ", "| ", "> "} {
			semantic = strings.TrimSpace(strings.TrimPrefix(semantic, rail))
		}
		logicalOffset += max(1, uniseg.GraphemeClusterCount(semantic)+1)
	}
	return lineMap
}

func semanticTranscriptLineMapForEntry(entry ChatEntry, chunk string) LineMap {
	switch entry.Kind {
	case "user", "system", "error":
		return semanticTranscriptLineMapFromSource(chunk, entry.Content)
	case "assistant":
		source := entry.Content
		if strings.TrimSpace(entry.ThinkingContent) != "" {
			source = entry.ThinkingContent + "\n" + source
		}
		return semanticTranscriptLineMapFromSource(chunk, source)
	default:
		return semanticTranscriptLineMap(chunk)
	}
}

// semanticTranscriptLineMapFromSource anchors rendered rows to the message
// source rather than to presentation bytes. Glamour may remove Markdown
// punctuation, wrap links into extra rows, or add chrome; source coordinates
// remain stable across the live/plain and settled/Markdown projections.
func semanticTranscriptLineMapFromSource(chunk, source string) LineMap {
	source = sanitizeTerminalMultiline(source)
	if source == "" {
		return semanticTranscriptLineMap(chunk)
	}
	lines := strings.Split(chunk, "\n")
	lineMap := make(LineMap, 0, len(lines))
	searchByte := 0
	logicalAtSearch := 0
	previousLogical := -1
	for row, line := range lines {
		semantic := transcriptSemanticRow(line)
		logical := previousLogical + 1
		if semantic != "" && searchByte <= len(source) {
			if relative := strings.Index(source[searchByte:], semantic); relative >= 0 {
				matchStart := searchByte + relative
				coordinateStart := markdownSourceConstructStart(source, matchStart, semantic)
				sourceOffset := logicalAtSearch +
					uniseg.GraphemeClusterCount(source[searchByte:coordinateStart])
				// Reserve logical zero for leading presentation chrome such as
				// the assistant label. Source byte zero therefore maps to one.
				logical = sourceOffset + 1
				matchEnd := matchStart + len(semantic)
				logicalAtSearch += uniseg.GraphemeClusterCount(source[searchByte:matchEnd])
				searchByte = matchEnd
			}
		}
		if logical <= previousLogical {
			logical = previousLogical + 1
		}
		lineMap = append(lineMap, TranscriptLinePoint{
			LogicalOffset: logical,
			Row:           row,
		})
		previousLogical = logical
	}
	return lineMap
}

func transcriptSemanticRow(line string) string {
	semantic := strings.TrimSpace(ansi.Strip(line))
	for _, rail := range []string{"│ ", "▌ ", "↳ ", "| ", "> "} {
		semantic = strings.TrimSpace(strings.TrimPrefix(semantic, rail))
	}
	return semantic
}

func markdownSourceConstructStart(source string, matchStart int, semantic string) int {
	if matchStart <= 0 || source[matchStart-1] != '[' {
		return matchStart
	}
	matchEnd := matchStart + len(semantic)
	if matchEnd+1 < len(source) && source[matchEnd] == ']' && source[matchEnd+1] == '(' {
		return matchStart - 1
	}
	return matchStart
}

func (m *Model) finalizeTranscriptLayout(state *entryRenderState, rendered string) {
	m.finalizeTranscriptLayoutHeight(state, max(1, lipgloss.Height(rendered)))
}

func (m *Model) finalizeTranscriptLayoutHeight(state *entryRenderState, totalHeight int) {
	if state == nil || state.layoutLen() == 0 {
		m.publishTranscriptLayout(nil)
		return
	}
	totalHeight = max(1, totalHeight)
	state.closeLayoutAt(totalHeight)
	m.publishTranscriptLayoutSegments(state.layoutBase, state.layoutRecords)
}

func (m *Model) publishTranscriptLayout(records []TranscriptLayoutRecord) {
	m.publishTranscriptLayoutSegments(nil, records)
}

// publishTranscriptLayoutSegments reuses the current contiguous snapshot when
// paint did not change. When a shared prefix proves that only the renderer-owned
// tail changed, it updates that tail in place instead of copying the entire
// history. Identity/order changes and full reflows still publish a new snapshot,
// so any previous frame needed for deletion fallback remains independent.
//
// LineMap slices are append-only inside the incremental live-tail cache and are
// intentionally shared. Replacing the tail record publishes its current slice
// length without copying the stable semantic points.
func (m *Model) publishTranscriptLayoutSegments(base, tail []TranscriptLayoutRecord) {
	sessionID := max(int64(0), m.sessionID)
	if m.transcriptLayoutMatchesSegments(sessionID, base, tail) {
		return
	}
	if m.updateTranscriptLayoutTail(sessionID, base, tail) {
		return
	}
	records := make([]TranscriptLayoutRecord, len(base)+len(tail))
	copy(records, base)
	copy(records[len(base):], tail)
	m.transcriptLayout = TranscriptLayoutSnapshot{
		SessionID: sessionID,
		Records:   records,
	}
	if m.transcriptRenderProbe != nil {
		m.transcriptRenderProbe.layoutRecordsMaterialized += len(records)
	}
}

// updateTranscriptLayoutTail preserves the public contiguous Records contract
// while making publication proportional to the changed suffix. A non-empty
// base must be the exact current backing prefix; the tiny all-tail case covers
// a transcript containing only the transient live block (and its predecessor).
//
// The semantic anchor resolver uses a previous frame only for identity/order.
// Those fields are required to match before an in-place geometry update, so a
// captured frame may observe newer tail geometry without losing deletion or
// turn-move evidence. Any identity/order change falls through to copy-on-write.
func (m *Model) updateTranscriptLayoutTail(
	sessionID int64,
	base, tail []TranscriptLayoutRecord,
) bool {
	current := &m.transcriptLayout
	if current.SessionID != sessionID ||
		len(tail) == 0 ||
		len(current.Records) != len(base)+len(tail) {
		return false
	}
	switch {
	case len(base) > 0:
		if len(current.Records) < len(base) ||
			&base[0] != &current.Records[0] {
			return false
		}
	case len(tail) > 2:
		// A nil base normally denotes a cold/full render. Updating a large
		// all-tail frame would mutate structural history rather than a bounded
		// renderer suffix.
		return false
	}

	offset := len(base)
	for index := range tail {
		previous := current.Records[offset+index]
		next := tail[index]
		if previous.BlockID != next.BlockID ||
			previous.TurnID != next.TurnID ||
			previous.Revision != next.Revision ||
			previous.Exact != next.Exact {
			return false
		}
	}
	copy(current.Records[offset:], tail)
	if m.transcriptRenderProbe != nil {
		m.transcriptRenderProbe.layoutRecordsUpdated += len(tail)
	}
	return true
}

func (m *Model) transcriptLayoutMatchesSegments(
	sessionID int64,
	base, tail []TranscriptLayoutRecord,
) bool {
	current := m.transcriptLayout
	if current.SessionID != sessionID || len(current.Records) != len(base)+len(tail) {
		return false
	}
	baseShared := len(base) == 0
	if len(base) > 0 && len(current.Records) >= len(base) {
		baseShared = &base[0] == &current.Records[0]
	}
	if !baseShared {
		for index := range base {
			if !m.transcriptLayoutRecordEqual(base[index], current.Records[index]) {
				return false
			}
		}
	}
	for index := range tail {
		if !m.transcriptLayoutRecordEqual(tail[index], current.Records[len(base)+index]) {
			return false
		}
	}
	return true
}

func (m *Model) transcriptLayoutRecordEqual(left, right TranscriptLayoutRecord) bool {
	if m.transcriptRenderProbe != nil {
		m.transcriptRenderProbe.layoutRecordComparisons++
	}
	if left.BlockID != right.BlockID ||
		left.TurnID != right.TurnID ||
		left.Revision != right.Revision ||
		left.Height != right.Height ||
		left.StartRow != right.StartRow ||
		left.Exact != right.Exact ||
		len(left.LineMap) != len(right.LineMap) {
		return false
	}
	if len(left.LineMap) == 0 || &left.LineMap[0] == &right.LineMap[0] {
		return true
	}
	for index := range left.LineMap {
		if left.LineMap[index] != right.LineMap[index] {
			return false
		}
	}
	return true
}

// bindCachedTranscriptLayoutPrefix makes the next visual tick share the exact
// immutable prefix of the snapshot just published. The mutable state keeps its
// line count and role/header arithmetic, but no longer owns a clone of every
// record or LineMap.
func (m *Model) bindCachedTranscriptLayoutPrefix() {
	if !m.entryCacheValid ||
		m.cachedPrefixLayoutCount < 0 ||
		m.cachedPrefixLayoutCount > len(m.transcriptLayout.Records) {
		return
	}
	count := m.cachedPrefixLayoutCount
	m.cachedPrefixState.layoutBase = m.transcriptLayout.Records[:count:count]
	m.cachedPrefixState.layoutRecords = nil
}

func (m *Model) liveTailLayoutIdentity() (BlockID, TurnID) {
	var turnID TurnID
	for index := len(m.entries) - 1; index >= 0; index-- {
		if m.entries[index].TurnID != "" {
			turnID = m.entries[index].TurnID
			break
		}
	}
	sessionID := max(int64(0), m.sessionID)
	if m.liveTailLayoutID != "" &&
		m.liveTailLayoutSessionID == sessionID &&
		m.liveTailLayoutTurnID == turnID &&
		m.liveTailLayoutReconcile == m.transcriptReconcileEpoch {
		return m.liveTailLayoutID, turnID
	}

	occupied := make(map[BlockID]struct{}, len(m.entries))
	for _, entry := range m.entries {
		if entry.BlockID != "" {
			occupied[entry.BlockID] = struct{}{}
		}
	}
	var candidate BlockID
	for attempt := uint64(0); ; attempt++ {
		candidate = liveTailLayoutCandidate(sessionID, turnID, m.liveTailLayoutEpoch, attempt)
		if _, collision := occupied[candidate]; !collision {
			break
		}
	}
	m.liveTailLayoutID = candidate
	m.liveTailLayoutTurnID = turnID
	m.liveTailLayoutSessionID = sessionID
	m.liveTailLayoutReconcile = m.transcriptReconcileEpoch
	return candidate, turnID
}

func liveTailLayoutCandidate(sessionID int64, turnID TurnID, episode, attempt uint64) BlockID {
	seed := fmt.Sprintf(
		"sonar.transcript.live-layout.v1\x00%d\x00%s\x00%d\x00%d",
		sessionID,
		turnID,
		episode,
		attempt,
	)
	digest := sha256.Sum256([]byte(seed))
	return BlockID("transient_live_" + hex.EncodeToString(digest[:16]))
}

func (m *Model) beginLiveTailLayoutEpisode() {
	if m.liveTailLayoutVisible {
		return
	}
	m.liveTailLayoutVisible = true
	if m.liveTailLayoutEpoch < ^uint64(0) {
		m.liveTailLayoutEpoch++
	}
	m.liveTailLayoutID = ""
}

func (m *Model) endLiveTailLayoutEpisode() {
	if !m.liveTailLayoutVisible {
		return
	}
	m.liveTailLayoutVisible = false
	m.liveTailLayoutID = ""
}

func fnv64(parts ...string) uint64 {
	h := fnv.New64a()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
	}
	return h.Sum64()
}

// entryMemoKey builds the composite key capturing every input that affects
// one entry's rendered chunk. An empty key means the entry cannot be projected
// safely enough to memoize.
// entryMemoKey takes the entry INDEX rather than the entry, because one entry
// no longer determines its own rendering: a read-run summary is produced by its
// first member and describes all of them, and only the index locates the run.
func (m *Model) entryMemoKey(entryIndex, contentW, proseW int) string {
	if entryIndex < 0 || entryIndex >= len(m.entries) {
		return ""
	}
	entry := m.entries[entryIndex]
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%d|%d|%d|%t|%t|%d",
		entry.Kind, entry.Revision, contentW, proseW, m.isDark, m.toolsCollapsed, m.glyphProfile)
	switch entry.Kind {
	case "user":
		fmt.Fprintf(&b, "|%d|%x|%d", len(entry.Content), fnv64(entry.Content), len(entry.Attachments))
	case "assistant":
		fmt.Fprintf(&b, "|%x|%t|%t",
			fnv64(entry.Content, "\x00", entry.ThinkingContent),
			entry.ThinkingCollapsed, entry.RenderedContent != "")
	case "tool_group":
		// A run's summary is rendered by its FIRST entry and describes every
		// member, so a key derived from that one entry's tool goes stale the
		// moment a fourth read lands and the transcript keeps claiming three.
		// The run's own shape is the key.
		if run, collapsed := m.collapsedReadRunAt(entryIndex); collapsed {
			fmt.Fprintf(&b, "|run%d:%d:%d:%s", run.Length, run.Reads, run.Searches, run.Target)
			break
		}
		toolKey, ok := m.toolGroupMemoKey(entry)
		if !ok {
			return ""
		}
		b.WriteString(toolKey)
	case "error", "system":
		fmt.Fprintf(&b, "|%x", fnv64(entry.Content))
	}
	return b.String()
}

// runningToolActivityGlyph is the frame a running receipt paints where its
// static cue would go. It is the parent's one Bubbles spinner — the same clock
// the footer rides — so a transcript full of running cards adds no second
// animation clock and every card moves in step.
//
// Empty means "keep the static cue", which is what reduced motion gets: the
// ellipsis still says unfinished, and nothing on screen moves.
func (m *Model) runningToolActivityGlyph() string {
	if m == nil || m.reducedMotion {
		return ""
	}
	// The card and the rail move together. Two moving glyphs in one frame,
	// driven by one clock but drawn from two vocabularies, would read as two
	// unrelated things happening rather than one.
	if pulse := m.sonarPulseGlyph(); pulse != "" {
		return pulse
	}
	return m.spin.View()
}

// toolGroupMemoKey summarizes the strict render projection consumed by a tool
// receipt.
//
// Clocks still live only in the footer — a running card carries no elapsed
// time, so there is exactly one place to read how long this has taken. The
// activity frame is the one exception the key must admit: a running receipt
// that memoizes its glyph freezes on whatever frame it first rendered, which
// is precisely how "… Running" came to look like a stuck card rather than a
// working one. Settled receipts contribute nothing here and stay free.
func (m *Model) toolGroupMemoKey(chat ChatEntry) (string, bool) {
	if chat.ToolIndex < 0 || chat.ToolIndex >= len(m.toolEntries) {
		return "", false
	}
	model, err := m.projectToolRenderModel(chat)
	if err != nil {
		return "|invalid-tool-projection", true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "|%x|%d|%t|%d|%t|%d|%d",
		fnv64(
			model.InvocationID, "\x00", model.ToolName, "\x00",
			model.Operation, "\x00", model.Target, "\x00", model.Summary,
			"\x00", model.Preview.Arguments, "\x00", model.Preview.Result,
		),
		model.Lifecycle, model.Preview.Expanded, model.Duration,
		model.Preview.DiffPending, m.chatPaneWidth(), m.inlineDiffPreviewRows())
	fmt.Fprintf(&b, "|%+v|%t", model.Preview.OutputDigest, model.Preview.OutputAvailable)
	if model.Lifecycle == ToolLifecycleRunning {
		fmt.Fprintf(&b, "|f%s", m.runningToolActivityGlyph())
	}
	p := model.Projection
	fmt.Fprintf(&b, "|%s|%s|%s|%s|%s|%t|%s|%+v",
		p.Specialist, p.Operation, p.Role, model.Transport, model.Domain, p.DomainTyped, model.Evidence, p.Route)
	if p.Digest != nil {
		fmt.Fprintf(&b, "|%+v", *p.Digest)
	}
	if model.Artifact != nil {
		fmt.Fprintf(&b, "|%+v", *model.Artifact)
	}
	for _, line := range model.Preview.DiffLines {
		fmt.Fprintf(&b, "|d%d:%d:%d:%x", line.Kind, line.OldLine, line.NewLine, fnv64(line.Content))
		if line.Hunk != nil {
			fmt.Fprintf(&b, ":%+v", *line.Hunk)
		}
	}
	for _, line := range model.Preview.ansiResultLines {
		for _, segment := range line {
			fmt.Fprintf(&b, "|a%t:%d:%x", segment.bold, segment.fg, fnv64(segment.text))
		}
	}
	return b.String(), true
}

// renderLiveTail renders the in-flight provider turn (streaming text or the
// inline waiting label). Provider calls can legitimately produce no tokens
// while compacting, awaiting permission, or continuing after a tool receipt;
// keeping that phase next to the last transcript event avoids a tall blank
// viewport above the footer.
func (m *Model) renderLiveTail(b *strings.Builder, contentW int, state *entryRenderState) {
	var tail strings.Builder
	if m.hasLiveTurnContent() {
		m.renderStreamingMsg(&tail, m.streamBuf.String(), contentW)
	} else if label := m.inlineTurnActivity(); label != "" {
		m.renderInlineTurnActivity(&tail, label, contentW)
	}
	tailRendered := tail.String()
	if tailRendered == "" {
		m.endLiveTailLayoutEpisode()
		return
	}
	m.beginLiveTailLayoutEpisode()
	if state.renderedAny {
		separator := transcriptEntrySeparator(state.previousKind, "assistant")
		b.WriteString(separator)
		state.renderedLines += strings.Count(separator, "\n")
	}
	semanticChunk := strings.TrimRight(tailRendered, "\n")
	blockID, turnID := m.liveTailLayoutIdentity()
	state.appendLayoutRecord(TranscriptLayoutRecord{
		BlockID:  blockID,
		TurnID:   turnID,
		Revision: 1,
		Height:   max(1, lipgloss.Height(semanticChunk)),
		StartRow: state.renderedLines,
		Exact:    false,
		LineMap:  semanticTranscriptLineMapFromSource(semanticChunk, m.liveTailSemanticSource()),
	})
	b.WriteString(tailRendered)
	state.renderedLines += strings.Count(tailRendered, "\n")
	state.previousKind = "assistant"
	state.renderedAny = true
}

func (m *Model) liveTailSemanticSource() string {
	source := m.streamBuf.String()
	if strings.TrimSpace(m.thinkBuf.String()) != "" {
		source = m.thinkBuf.String() + "\n" + source
	}
	return source
}

// collapsedToolFooterRegion locates a collapsed receipt's "N lines hidden ·
// ctrl+r" row inside its own rendered chunk.
//
// It reads the chunk rather than the render model on purpose: fresh renders
// and memo hits both reach appendEntryChunk with nothing but the chunk, and
// the two must produce byte- and region-identical frames. The collapsed check
// is the one thing the chunk cannot answer — an expanded receipt's result body
// may contain a line that reads like the cue — and it is an O(1) field read.
func (m *Model) collapsedToolFooterRegion(entry ChatEntry, chunk string) (rowOffset, startCol, endCol int, ok bool) {
	if entry.ToolIndex < 0 || entry.ToolIndex >= len(m.toolEntries) ||
		!m.toolEntries[entry.ToolIndex].Collapsed {
		return 0, 0, 0, false
	}
	glyphs := glyphSet(m.glyphProfile)
	for row, line := range strings.Split(chunk, "\n") {
		if row == 0 {
			continue
		}
		plain := strings.TrimLeft(ansi.Strip(line), " ")
		plain = strings.TrimLeft(strings.TrimPrefix(plain, glyphs.Vertical), " ")
		if !collapsedToolBodyFooterLine(plain) {
			continue
		}
		startCol, endCol, ok := renderedLineHorizontalBounds(line)
		return row, startCol, endCol, ok
	}
	return 0, 0, 0, false
}

func completedThinkingHeaderRegion(
	rendered string,
	profiles ...GlyphProfile,
) (rowOffset, startCol, endCol int, ok bool) {
	glyphs := glyphSet(resolveGlyphProfile(profiles...))
	for row, line := range strings.Split(rendered, "\n") {
		plain := strings.TrimLeft(ansi.Strip(line), " ")
		if strings.HasPrefix(plain, glyphs.Vertical+" "+glyphs.Collapsed) ||
			strings.HasPrefix(plain, glyphs.Vertical+" "+glyphs.Expanded) {
			startCol, endCol, ok := renderedLineHorizontalBounds(line)
			return row, startCol, endCol, ok
		}
	}
	return 0, 0, 0, false
}

// renderedLineHorizontalBounds projects the exact painted cells of one
// transcript header. Transcript indentation is layout whitespace rather than
// part of the interactive disclosure, so pointer ownership starts at the first
// rendered non-space cell and ends immediately after the last rendered cell.
func renderedLineHorizontalBounds(rendered string) (startCol, endCol int, ok bool) {
	plain := ansi.Strip(rendered)
	content := strings.TrimLeft(plain, " ")
	if content == "" {
		return 0, 0, false
	}
	endCol = lipgloss.Width(plain)
	startCol = endCol - lipgloss.Width(content)
	return startCol, endCol, startCol < endCol
}

func (m *Model) hasLiveTurn() bool {
	return m.hasLiveTurnContent()
}

func (m *Model) hasVisibleLiveTurn() bool {
	return m.hasLiveTurnContent() || m.inlineTurnActivity() != ""
}

func (m *Model) hasLiveTurnContent() bool {
	return strings.TrimSpace(m.streamBuf.String()) != "" || strings.TrimSpace(m.thinkBuf.String()) != ""
}

func (m *Model) inlineTurnActivity() string {
	if m == nil || m.hasLiveTurnContent() || m.toolsPending > 0 {
		return ""
	}
	if m.pendingApproval != nil {
		return "Waiting for permission below…"
	}
	if m.compactingContext {
		return "Preparing context…"
	}
	if m.state == StateWaiting || m.state == StateStreaming {
		return "Waiting for model…"
	}
	return ""
}

func (m *Model) renderInlineTurnActivity(b *strings.Builder, label string, contentW int) {
	label = sanitizeTerminalSingleLine(label)
	if label == "" {
		return
	}
	b.WriteString(m.contentGrid().IndentBlock(" ", m.styles.StreamHint.Render(label)))
	b.WriteString("\n")
}

// transcriptEntrySeparator is the single owner of vertical rhythm between
// transcript entries. Grok uses consistent breathing: one blank row between
// distinct surfaces; tool cards stack denser; assistant segments of the same
// turn stay adjacent.
func transcriptEntrySeparator(previous, current string) string {
	// Tool receipts form one instrument panel — no gap between cards.
	if previous == "tool_group" && current == "tool_group" {
		return "\n"
	}
	// Multi-segment assistant of the same turn (reasoning chunks + answer)
	// stay stacked without a chapter break.
	if previous == "assistant" && current == "assistant" {
		return "\n"
	}
	// Tool → answer / notice → assistant / user → * : one blank row.
	// This matches Grok's readable vertical rhythm without sparse voids.
	return "\n\n"
}

func (m *Model) renderEntryError(b *strings.Builder, content string, contentW int) {
	content = strings.TrimSpace(sanitizeTerminalMultiline(content))
	if content == "" {
		content = "The operation failed without an error message."
	}
	grid := m.contentGrid()
	b.WriteString(grid.Prefix(" ") + m.styles.ErrorChip.Render(glyphSet(m.glyphProfile).Error+" error"))
	b.WriteString("\n")
	b.WriteString(m.styles.ToolErrorText.Render(grid.IndentBlock(" ", wrapText(content, max(1, contentW)))))
	b.WriteString("\n")
}

// renderUserMsg renders a user message as an accent-guttered content block.
// No "you" role label — the gutter (and sticky strip for the latest turn) is
// enough identity, matching denser Grok-style transcript chrome.
func (m *Model) renderUserMsg(b *strings.Builder, content string, attachments []imageasset.Ref, contentW int) {
	content = sanitizeTerminalMultiline(content)
	grid := m.contentGrid()
	gutter := grid.Prefix(m.styles.UserGutter.Render(glyphSet(m.glyphProfile).UserRail))
	text := m.styles.UserContent.UnsetPaddingLeft()
	for _, line := range strings.Split(wrapText(content, max(10, contentW)), "\n") {
		b.WriteString(gutter + text.Render(line))
		b.WriteString("\n")
	}
	if len(attachments) > 0 {
		b.WriteString(m.renderImageAttachmentSummary(attachments, contentW))
		b.WriteString("\n")
	}
}

// renderAssistantMsg renders a completed assistant message block.
// Uses cached RenderedContent if available (snap-into-place pattern).
func (m *Model) renderAssistantMsg(b *strings.Builder, entry ChatEntry, contentW int) {
	content := sanitizeTerminalMultiline(entry.Content)
	hasContent := strings.TrimSpace(content) != ""
	hasThinking := strings.TrimSpace(entry.ThinkingContent) != ""
	if !hasContent && !hasThinking {
		return
	}

	grid := m.contentGrid()
	// Reasoning belongs to this assistant turn, so its disclosure follows the
	// role header instead of appearing as an unowned block above it.
	if hasThinking {
		thinkBox := m.renderThinkingBox(entry.ThinkingContent, entry.ThinkingCollapsed)
		b.WriteString(grid.IndentBlock(" ", thinkBox))
		b.WriteString("\n")
	}
	if !hasContent {
		return
	}

	// Use cached rendered content if available.
	rendered := entry.RenderedContent
	if content != entry.Content {
		// Cached Glamour output is trusted only when it was derived from the same
		// sanitized source. Restored or synthetic entries must be rendered again.
		rendered = ""
	}
	if rendered == "" {
		rendered = content
		if m.md != nil {
			rendered = m.md.RenderFull(rendered)
		}
	}
	// Trim Glamour's own document padding. Its Document style carries
	// BlockPrefix/BlockSuffix newlines, and only the trailing one was being
	// removed — the leading one survived as a blank row on top of the blank row
	// transcriptEntrySeparator already inserts, so every assistant turn opened
	// with a two-row gap. Vertical rhythm belongs to the separator alone.
	rendered = strings.Trim(rendered, "\n")
	rendered = strings.TrimRight(rendered, " \t\n")
	rendered = grid.IndentBlock(" ", rendered)
	b.WriteString(rendered)
	b.WriteString("\n")
	// Scannable turn digest (agent-harness research: Claude Code coherence,
	// Zero recap, OpenCode structured sessions). Pure function of content so
	// memo keys stay content-bound. Bound to prose width so the digest never
	// widens the work column, and so transcript search can treat it as chrome.
	if recap := formatTurnRecapLine(
		buildTurnRecap(content),
		min(contentW, m.chatProseWidth()),
		m.isDark,
		m.themeID,
		m.glyphProfile,
	); recap != "" {
		b.WriteString(grid.IndentBlock(" ", recap))
		b.WriteString("\n")
	}
}

// renderStreamingMsg renders the in-progress assistant message (plain text).
func (m *Model) renderStreamingMsg(b *strings.Builder, content string, contentW int) {
	content = sanitizeTerminalMultiline(content)
	hasContent := strings.TrimSpace(content) != ""
	hasThinking := strings.TrimSpace(m.thinkBuf.String()) != ""
	if !hasContent && !hasThinking {
		return
	}

	// Live reasoning uses the same assistant-owned hierarchy as the completed
	// disclosure. A bounded tail window keeps token-by-token height stable; the
	// full receipt becomes expandable only after the turn settles.
	if hasThinking {
		b.WriteString(m.contentGrid().IndentBlock(" ", m.renderLiveThinkingBox(m.thinkBuf.String())))
		b.WriteString("\n")
	}
	if !hasContent {
		return
	}

	m.renderStreamingAnswer(b, content, contentW)
}

// renderStreamingAnswer owns the canonical answer-only projection. Reasoning
// and role chrome stay in renderStreamingMsg so consumers such as transcript
// search can derive a safe exact suffix without ever receiving thinkBuf.
func (m *Model) renderStreamingAnswer(
	b *strings.Builder,
	content string,
	contentW int,
) {
	content = sanitizeTerminalMultiline(content)
	if strings.TrimSpace(content) == "" {
		return
	}

	// During streaming: render the stable markdown prefix with Glamour (cached)
	// and only the trailing partial paragraph as plain wrapped text. This shows
	// formatted output live instead of popping into shape on completion, while
	// avoiding the jitter of re-rendering incomplete markdown. Content-grid
	// Prefix owns the left three cells; wrap to the flex content budget only.
	messageWidth := min(contentW, m.chatProseWidth())
	if markdownUsesWorkWidth(content) {
		messageWidth = contentW
	}
	wrapWidth := messageWidth
	if wrapWidth < 10 {
		wrapWidth = 10
	}
	grid := m.contentGrid()

	var formatted, tail string
	if m.md != nil {
		formatted, tail = m.md.RenderStreamingFormatted(content)
	} else {
		tail = content
	}

	if formatted != "" {
		b.WriteString(grid.IndentBlock(" ", strings.TrimRight(formatted, " \t\n")))
		b.WriteString("\n")
	}
	if strings.TrimSpace(tail) != "" {
		b.WriteString(grid.IndentBlock(" ", wrapText(tail, wrapWidth)))
		b.WriteString("\n")
	}
}

// renderToolGroup renders one tight tool receipt. The parent transcript owns
// all spacing between this block and its neighbors. ToolCard owns the
// content-grid accent + pad; the parent must not re-indent.
func (m *Model) renderToolGroup(b *strings.Builder, chat ChatEntry) {
	if chat.ToolIndex < 0 || chat.ToolIndex >= len(m.toolEntries) {
		return
	}
	grid := m.contentGrid()
	glyphs := glyphSet(m.glyphProfile)
	invalidReceipt := func() {
		b.WriteString(grid.Line(
			glyphs.Vertical,
			m.styles.ToolErrorText.Render(glyphs.Error+" Invalid tool receipt"),
		))
	}
	model, err := m.projectToolRenderModel(chat)
	if err != nil {
		invalidReceipt()
		return
	}
	card, err := ToolCardFromRenderModel(model, m.isDark, m.themeID, m.glyphProfile)
	if err != nil {
		invalidReceipt()
		return
	}
	// LineWidth = accent + pad + content. ToolCard paints accent+pad internally.
	availableWidth := max(4, grid.LineWidth())
	cardView := card.ViewWithActivity(availableWidth, m.runningToolActivityGlyph(), 0)
	if model.Preview.Expanded && model.Lifecycle.Terminal() {
		var diffView string
		if model.Preview.DiffPending {
			diffView = renderDiffLoadingAtWidth(
				model.Summary,
				m.styles,
				availableWidth,
				m.glyphProfile,
			)
		} else if len(model.Preview.DiffLines) > 0 {
			diffView = strings.TrimRight(
				renderUnifiedDiffAtWidth(
					model.Summary,
					model.Preview.DiffLines,
					m.styles,
					m.inlineDiffPreviewRows(),
					availableWidth,
					m.glyphProfile,
				),
				"\n",
			)
		}
		if diffView != "" {
			cardView += "\n" + diffView
		}
	}
	if shouldShowToolViewerHint(m, model.Preview) {
		if hint := m.toolViewerActionHint(chat); hint != "" {
			// Align viewer hints under the card content column (after accent+pad).
			cardView += "\n" + grid.Prefix(glyphs.Vertical) + m.styles.Dimmed.Render(
				truncateDisplayWithGlyphProfile(hint, grid.ContentWidth(), m.glyphProfile),
			)
		}
	}
	b.WriteString(cardView)
}

func (m *Model) inlineDiffPreviewRows() int {
	if m == nil {
		return inlineDiffPreviewRowsForHeight(0)
	}
	return inlineDiffPreviewRowsForHeight(m.viewport.Height())
}

func inlineDiffPreviewRowsForHeight(height int) int {
	if height <= 0 {
		return 16
	}
	// Keep an expanded patch within one transcript viewport whenever possible.
	// The final budgeted row is an explicit omission receipt; the Diff Viewer
	// owns deeper inspection without inflating transcript geometry.
	return max(minTranscriptRows, min(32, height))
}

func (m *Model) projectToolRenderModel(chat ChatEntry) (ToolRenderModel, error) {
	model, err := ToolRenderModelFromEntry(chat, m.toolEntries[chat.ToolIndex])
	if err != nil {
		return ToolRenderModel{}, err
	}
	ref := m.toolEntries[chat.ToolIndex].OutputDetail.Ref
	if !m.outputDetails.Available(ref) {
		model.Preview.OutputAvailable = false
	}
	return model, nil
}

// shouldShowToolViewerHint decides whether a card advertises its viewers.
//
// The hint used to render only on an expanded card, so a reader had to already
// know the card expanded in order to learn that the full diff was one
// keystroke away. A collapsed write whose diff is longer than the inline
// preview now says so. Every harness that collapses tool output by default
// pairs the collapsed state with a visible way out of it, because an
// affordance nobody can see is not an affordance.
//
// A diff that is still building stays quiet: promising a viewer that is not
// ready yet is an affordance that fails when used.
func shouldShowToolViewerHint(m *Model, preview ToolPreview) bool {
	if preview.Expanded {
		return true
	}
	if preview.DiffPending || len(preview.DiffLines) == 0 {
		return false
	}
	return len(preview.DiffLines) > m.inlineDiffPreviewRows()
}
