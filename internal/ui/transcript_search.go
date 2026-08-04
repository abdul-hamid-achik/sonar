package ui

import (
	"fmt"
	"regexp"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	transcriptSearchQueryLimit = 256
	transcriptSearchMatchLimit = 4096
	transcriptSearchRowLimit   = 100_000
	transcriptSearchByteLimit  = 8 << 20
)

// TranscriptSearchState owns only an ephemeral index of text that has already
// crossed the transcript's safe presentation boundary. It deliberately has no
// field capable of retaining raw tool payloads, StructuredContent, or hidden
// reasoning.
type TranscriptSearchState struct {
	input textinput.Model

	rows          []transcriptSearchRow
	matches       []transcriptSearchMatch
	active        int
	matchesCapped bool
	indexCapped   bool
	indexBytes    int
	// These counters contain no transcript text. They make the source-work
	// bound observable in tests: every source-backed entry is sanitized once,
	// and its monotonic cursor never scans more bytes than were prepared.
	indexSourceBytes         int
	indexSourceScanBytes     int
	indexSourceSanitizations int
	generation               uint64

	restore         transcriptReflowAnchor
	composerFocused bool
	width           int
	activeRow       int
}

type transcriptSearchRow struct {
	BlockID      BlockID
	TurnID       TurnID
	Revision     uint64
	Logical      int
	Grapheme     int
	LocalRow     int
	AbsoluteRow  int
	RenderedText string
}

type transcriptSearchMatch struct {
	transcriptSearchRow
	StartByte int
	EndByte   int
}

// transcriptSearchSourceCursor proves provenance for plain source-backed
// transcript rows without rescanning a whole ChatEntry for every painted row.
// Both source and candidates use the same whitespace normalization as the
// previous fallback. The cursor is monotonic because painted content rows and
// their LineMap coordinates are chronological. A miss exhausts the cursor and
// fails closed: later rows cannot be proven to follow an unrecognized row.
type transcriptSearchSourceCursor struct {
	source string
	offset int
}

const transcriptSearchSystemLabel = "notice ·"

func newTranscriptSearchState(
	width int,
	isDark bool, themeID string,
	reducedMotion bool,
) *TranscriptSearchState {
	input := textinput.New()
	input.Prompt = "/ "
	input.Placeholder = "search safe transcript"
	input.CharLimit = transcriptSearchQueryLimit
	input.SetVirtualCursor(false)
	input.SetStyles(semanticTextInputStyles(isDark, themeID, reducedMotion))
	state := &TranscriptSearchState{
		input:     input,
		active:    -1,
		activeRow: -1,
	}
	state.setWidth(width)
	return state
}

func (state *TranscriptSearchState) setWidth(width int) {
	if state == nil {
		return
	}
	state.width = max(1, width)
	// Bubbles' textinput Width covers its value viewport, but View also emits
	// one cursor cell and the prompt outside that budget. Reserve both so the
	// complete search row is exactly state.width instead of overflowing by one
	// cell and acquiring a misleading truncation ellipsis.
	state.input.SetWidth(max(
		1,
		state.width-ansi.StringWidth(state.input.Prompt)-1,
	))
	// Bubbles updates its horizontal overflow window when the cursor moves, not
	// when SetWidth is called. Re-apply the current cursor so an early compact
	// WindowSizeMsg cannot leave a later wide search footer clipped at the old
	// width.
	state.input.SetCursor(state.input.Position())
}

func (m *Model) openTranscriptSearch() tea.Cmd {
	if m == nil || m.viewerModalActive() {
		return nil
	}
	if m.overlay == OverlayTranscriptSearch && m.transcriptSearch != nil {
		return m.transcriptSearch.input.Focus()
	}
	if m.overlay != OverlayNone {
		return nil
	}
	if m.transcriptSearch != nil {
		// Recover fail-closed from an orphaned pointer instead of focusing an
		// invisible input. Use the ordinary close path so observational search
		// navigation restores its captured anchor, focus, height, and line hook
		// before a fully mounted fresh surface is constructed.
		_ = m.closeTranscriptSearch(true)
	}

	state := newTranscriptSearchState(
		m.chatPaneWidth(),
		m.isDark,
		m.themeID,
		m.reducedMotion,
	)
	state.restore = m.captureTranscriptReflowAnchor()
	state.composerFocused = m.input.Focused()
	m.input.Blur()
	m.overlayParent = OverlayNone
	m.overlay = OverlayTranscriptSearch
	m.transcriptSearch = state
	m.recalcViewportHeight()
	m.rebuildTranscriptSearchIndex()
	return tea.Batch(state.input.Focus(), tea.ClearScreen)
}

// closeTranscriptSearch restores the exact reading/follow intent captured
// before search opened. Search navigation is therefore observational: it
// cannot silently convert follow-latest into a durable manual scroll.
func (m *Model) closeTranscriptSearch(restore bool) tea.Cmd {
	if m == nil || m.transcriptSearch == nil {
		return nil
	}
	state := m.transcriptSearch
	// A background operation can complete while search owns the keyboard and
	// legitimately focus the now-editable composer. Preserve that newer focus
	// intent as well as the snapshot taken when search opened.
	composerFocused := state.composerFocused || m.input.Focused()
	state.input.Blur()
	m.transcriptSearch = nil
	m.overlay = OverlayNone
	m.overlayParent = OverlayNone
	m.recalcViewportHeight()
	if restore {
		m.restoreTranscriptReflowAnchor(state.restore)
	}

	var focus tea.Cmd
	if composerFocused && m.composerEditable() {
		focus = m.input.Focus()
	} else {
		m.input.Blur()
	}
	return tea.Batch(focus, tea.ClearScreen)
}

func (m *Model) transcriptSearchHighlightRow() int {
	if m == nil || m.transcriptSearch == nil ||
		m.transcriptSearch.activeRow < 0 ||
		m.transcriptSearch.generation !=
			m.transcriptPaint.documentGeneration {
		return -1
	}
	return m.transcriptSearch.activeRow
}

func (m *Model) transcriptSearchActiveRowStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(outputSemanticPalette(m.isDark, m.themeID).Accent).
		// Lip Gloss v2 renders underlined text rune-by-rune so spaces can have
		// separate underline semantics. ANSI resets inside a ZWJ emoji split
		// its terminal grapheme and change measured width. Accent plus weight
		// keeps the active row unambiguous without perturbing grapheme geometry.
		Bold(true)
}

// styleTranscriptSearchWindowRows applies the active-row signal while the
// bounded paint window is staged. Wrapping already styled Markdown through the
// viewport's StyleLineFunc can expose inner SGR bytes as visible text, while
// splitting an ANSI-stripped row by cell offsets can bisect a ZWJ grapheme.
// Normalize only this staged row to the active semantic style: the immutable
// document, its grapheme geometry, and the viewport's line hook stay untouched.
func (m *Model) styleTranscriptSearchWindowRows(
	rows []string,
	windowStart int,
	highlightRow int,
) {
	localRow := highlightRow - windowStart
	if m == nil || highlightRow < 0 ||
		localRow < 0 || localRow >= len(rows) {
		return
	}
	width := ansi.StringWidth(rows[localRow])
	if width <= 0 {
		return
	}
	rows[localRow] = m.transcriptSearchActiveRowStyle().
		Render(ansi.Strip(rows[localRow]))
}

func (m *Model) restyleTranscriptSearch() {
	if m == nil || m.transcriptSearch == nil {
		return
	}
	m.transcriptSearch.input.SetStyles(
		semanticTextInputStyles(m.isDark, m.themeID, m.reducedMotion),
	)
}

func (m *Model) resizeTranscriptSearch() {
	if m == nil || m.transcriptSearch == nil {
		return
	}
	m.transcriptSearch.setWidth(m.chatPaneWidth())
}

func (m *Model) updateTranscriptSearchMessage(msg tea.Msg) tea.Cmd {
	if m == nil || m.transcriptSearch == nil {
		return nil
	}
	var cmd tea.Cmd
	m.transcriptSearch.input, cmd = m.transcriptSearch.input.Update(msg)
	return cmd
}

func (m *Model) handleTranscriptSearchKey(msg tea.KeyPressMsg) tea.Cmd {
	if m == nil || m.transcriptSearch == nil {
		return nil
	}
	switch msg.String() {
	case "enter", "down", "ctrl+n":
		m.navigateTranscriptSearch(1)
		return nil
	case "shift+enter", "up", "ctrl+p":
		m.navigateTranscriptSearch(-1)
		return nil
	case "ctrl+f":
		return m.transcriptSearch.input.Focus()
	}

	before := m.transcriptSearch.input.Value()
	var cmd tea.Cmd
	m.transcriptSearch.input, cmd = m.transcriptSearch.input.Update(msg)
	m.normalizeTranscriptSearchQuery()
	if m.transcriptSearch.input.Value() != before {
		m.recomputeTranscriptSearchMatches(true)
	}
	return cmd
}

func (m *Model) normalizeTranscriptSearchQuery() {
	if m == nil || m.transcriptSearch == nil {
		return
	}
	// Sanitize controls without collapsing or trimming ordinary spaces while
	// the user is still typing. Applying strings.Fields on every key would
	// delete a trailing space before the next word arrived.
	value := sanitizeTerminalMultiline(m.transcriptSearch.input.Value())
	value = strings.NewReplacer("\n", " ", "\t", " ").Replace(value)
	if value == m.transcriptSearch.input.Value() {
		return
	}
	m.transcriptSearch.input.SetValue(value)
	m.transcriptSearch.input.CursorEnd()
}

func (m *Model) rebuildTranscriptSearchIndex() {
	m.rebuildTranscriptSearchIndexBounded(
		transcriptSearchRowLimit,
		transcriptSearchByteLimit,
	)
}

func (m *Model) rebuildTranscriptSearchIndexBounded(rowLimit, byteLimit int) {
	if m == nil || m.transcriptSearch == nil {
		return
	}
	state := m.transcriptSearch
	rowLimit = max(0, rowLimit)
	byteLimit = max(0, byteLimit)
	state.rows = state.rows[:0]
	state.indexBytes = 0
	state.indexCapped = false
	state.indexSourceBytes = 0
	state.indexSourceScanBytes = 0
	state.indexSourceSanitizations = 0
	state.generation = m.transcriptPaint.documentGeneration

	records, rowsCapped := recentTranscriptSearchRecords(
		m.transcriptLayout.Records,
		rowLimit,
	)
	if rowsCapped {
		state.indexCapped = true
	}
	wanted := make(map[BlockID]struct{}, len(records))
	for _, record := range records {
		if record.BlockID.Valid() && record.BlockID != m.liveTailLayoutID {
			wanted[record.BlockID] = struct{}{}
		}
	}
	entries := make(map[BlockID]ChatEntry, len(wanted))
	if len(wanted) > 0 {
		for index := len(m.entries) - 1; index >= 0; index-- {
			entry := m.entries[index]
			if _, ok := wanted[entry.BlockID]; !ok {
				continue
			}
			if _, exists := entries[entry.BlockID]; exists {
				continue
			}
			entries[entry.BlockID] = entry
			if len(entries) == len(wanted) {
				break
			}
		}
	}
	var bytesCapped bool
	records, bytesCapped = m.recentTranscriptSearchRecordsByByte(
		records,
		entries,
		byteLimit,
	)
	if bytesCapped {
		state.indexCapped = true
	}
	sourceCursors := make(
		map[BlockID]*transcriptSearchSourceCursor,
		len(wanted),
	)
	rowsVisited := 0
recordLoop:
	for _, record := range records {
		if rowsVisited >= rowLimit ||
			state.indexBytes >= byteLimit {
			state.indexCapped = true
			break
		}
		entry, settled := entries[record.BlockID]
		live := !settled && record.BlockID == m.liveTailLayoutID
		if !settled && !live {
			continue
		}

		answerRecord := live || (settled && entry.Kind == "assistant")
		var sourceCursor *transcriptSearchSourceCursor
		if settled && transcriptSearchSourceBackedKind(entry.Kind) {
			sourceCursor = sourceCursors[record.BlockID]
			if sourceCursor == nil {
				remainingSourceBytes := byteLimit - state.indexSourceBytes
				if len(entry.Content) > remainingSourceBytes {
					state.indexCapped = true
					continue recordLoop
				}
				var sourceBytes int
				sourceCursor, sourceBytes = newTranscriptSearchSourceCursor(
					entry.Content,
				)
				if sourceBytes > remainingSourceBytes {
					state.indexCapped = true
					continue recordLoop
				}
				sourceCursors[record.BlockID] = sourceCursor
				state.indexSourceBytes += sourceBytes
				state.indexSourceSanitizations++
			}
		}
		if answerRecord {
			// The recent-record selector already bounds source work and painted
			// bytes independently. Do not charge source bytes against the
			// semantic-row budget here: a large safe ToolCard followed by
			// compact Markdown could otherwise hide the newest answer even
			// though both independent bounds admit the complete suffix.
			if record.Height > rowLimit-rowsVisited {
				state.indexCapped = true
				continue recordLoop
			}
		}
		recordEnd := record.StartRow + record.Height
		materializeEnd := min(
			recordEnd,
			record.StartRow+rowLimit-rowsVisited,
		)
		if materializeEnd < recordEnd {
			state.indexCapped = true
		}
		rows, _ := m.transcriptPaint.document.materializeRows(
			record.StartRow,
			materializeEnd,
		)
		rowsVisited += len(rows)
		answerStart, answerEnd := -1, -1
		if answerRecord {
			var ok bool
			answerStart, answerEnd, ok = m.transcriptSearchAnswerSuffix(
				entry,
				live,
				rows,
				m.chatContentWidth(),
			)
			if !ok {
				// Fail closed when the answer-only canonical projection cannot
				// be proven to be the exact painted suffix.
				continue
			}
		}
		for localRow, rendered := range rows {
			if len(state.rows) >= rowLimit {
				state.indexCapped = true
				break
			}
			remainingBytes := byteLimit - state.indexBytes
			if len(rendered) > remainingBytes {
				state.indexCapped = true
				break recordLoop
			}
			semantic := transcriptSemanticRow(rendered)
			// Turn digests restate assistant prose for scannability; indexing
			// them would double-count needles against the answer body.
			if isTurnRecapSemanticRow(semantic) {
				continue
			}
			point, _, ok := anchorPointAtOrAfter(
				record,
				localRow,
				record.StartRow,
			)
			if !ok {
				point = TranscriptLinePoint{}
			}
			answerOnly := ok &&
				answerStart >= 0 &&
				localRow >= answerStart &&
				localRow < answerEnd
			if semantic == "" {
				continue
			}
			admitted := answerOnly
			if !admitted {
				var scanned int
				admitted, scanned = m.admitTranscriptSearchRow(
					entry,
					settled,
					localRow,
					semantic,
					point,
					ok,
					sourceCursor,
				)
				state.indexSourceScanBytes += scanned
			}
			if !admitted {
				continue
			}
			if state.indexBytes+len(semantic) > byteLimit {
				state.indexCapped = true
				break recordLoop
			}
			state.rows = append(state.rows, transcriptSearchRow{
				BlockID:      record.BlockID,
				TurnID:       record.TurnID,
				Revision:     record.Revision,
				Logical:      point.LogicalOffset,
				Grapheme:     point.Grapheme,
				LocalRow:     localRow,
				AbsoluteRow:  record.StartRow + localRow,
				RenderedText: semantic,
			})
			state.indexBytes += len(semantic)
		}
	}
}

// recentTranscriptSearchRecords selects a contiguous suffix of whole records
// under the row budget and keeps that suffix in chronological order. A newest
// record that does not fit wholly is omitted: partial assistant records could
// cross the answer/reasoning boundary, so bounded search fails closed.
func recentTranscriptSearchRecords(
	records []TranscriptLayoutRecord,
	rowLimit int,
) ([]TranscriptLayoutRecord, bool) {
	if len(records) == 0 {
		return nil, false
	}
	if rowLimit <= 0 {
		return nil, true
	}
	remaining := rowLimit
	start := len(records)
	for start > 0 {
		height := max(1, records[start-1].Height)
		if height > remaining {
			break
		}
		remaining -= height
		start--
	}
	if start == len(records) {
		return nil, true
	}
	return records[start:], start > 0
}

// recentTranscriptSearchRecordsByByte applies a conservative whole-record
// byte window after the row window. It walks newest to oldest, then returns
// the selected suffix in its original chronological order. Counting painted
// bytes (including chrome) can admit fewer records than the semantic index
// would, but guarantees an oversized older record never blocks newer history.
func (m *Model) recentTranscriptSearchRecordsByByte(
	records []TranscriptLayoutRecord,
	entries map[BlockID]ChatEntry,
	byteLimit int,
) ([]TranscriptLayoutRecord, bool) {
	if len(records) == 0 {
		return nil, false
	}
	if byteLimit <= 0 {
		return nil, true
	}
	renderedRemaining := byteLimit
	sourceRemaining := byteLimit
	start := len(records)
	for start > 0 {
		record := records[start-1]
		recordEnd := record.StartRow + record.Height
		rows, _ := m.transcriptPaint.document.materializeRows(
			record.StartRow,
			recordEnd,
		)
		renderedBytes := 0
		fits := true
		for _, row := range rows {
			if len(row) > renderedRemaining-renderedBytes {
				fits = false
				break
			}
			renderedBytes += len(row)
		}

		sourceBytes := 0
		if entry, settled := entries[record.BlockID]; settled {
			if entry.Kind == "assistant" ||
				transcriptSearchSourceBackedKind(entry.Kind) {
				sourceBytes = len(entry.Content)
			}
		} else if record.BlockID == m.liveTailLayoutID {
			sourceBytes = m.streamBuf.Len()
		}
		if !fits || sourceBytes > sourceRemaining {
			break
		}
		renderedRemaining -= renderedBytes
		sourceRemaining -= sourceBytes
		start--
	}
	if start == len(records) {
		// A newest record that cannot be proven wholly inside the bounds is
		// intentionally omitted. Partial assistant records could cross the
		// answer/reasoning boundary, so the safe contract is fail-closed.
		return nil, true
	}
	return records[start:], start > 0
}

func (m *Model) admitTranscriptSearchRow(
	entry ChatEntry,
	settled bool,
	localRow int,
	semantic string,
	point TranscriptLinePoint,
	pointOK bool,
	sourceCursor *transcriptSearchSourceCursor,
) (bool, int) {
	if semantic == "" {
		return false, 0
	}
	if !settled {
		return false, 0
	}
	switch entry.Kind {
	case "tool_group":
		// Tool cards are rendered exclusively from ToolRenderModel, whose
		// constructor rejects raw/structured/private fields. The painted row is
		// therefore the narrowest searchable source.
		return entry.ToolIndex >= 0 &&
			entry.ToolIndex < len(m.toolEntries), 0
	case "user", "system", "error":
		// Source-backed line maps reserve logical zero for presentation chrome.
		systemPrefixRow := entry.Kind == "system" &&
			localRow == 0 &&
			(semantic == transcriptSearchSystemLabel ||
				strings.HasPrefix(
					semantic,
					transcriptSearchSystemLabel+" ",
				))
		if !pointOK ||
			(point.LogicalOffset <= 0 && !systemPrefixRow) ||
			sourceCursor == nil {
			return false, 0
		}
		candidate, ok := transcriptSearchSourceCandidate(
			entry.Kind,
			localRow,
			semantic,
		)
		if !ok {
			return false, 0
		}
		return sourceCursor.contains(candidate)
	default:
		return false, 0
	}
}

func transcriptSearchSourceBackedKind(kind string) bool {
	switch kind {
	case "user", "system", "error":
		return true
	default:
		return false
	}
}

func newTranscriptSearchSourceCursor(
	source string,
) (*transcriptSearchSourceCursor, int) {
	normalized := strings.Join(
		strings.Fields(sanitizeTerminalMultiline(source)),
		" ",
	)
	// Charge the larger representation against the source-work budget. Invalid
	// UTF-8 can expand during sanitization; ordinary whitespace normalization
	// normally makes the prepared source smaller than the input.
	sourceBytes := max(len(source), len(normalized))
	return &transcriptSearchSourceCursor{source: normalized}, sourceBytes
}

func transcriptSearchSourceCandidate(
	kind string,
	localRow int,
	semantic string,
) (string, bool) {
	switch kind {
	case "user":
		// User rows are gutter + content only (no role-label chrome). A message
		// whose source is literally "you" is real content and must be indexed.
	case "error":
		// The first row is always the renderer-owned error chip, even when an
		// adversarial source repeats the same visible text.
		if localRow == 0 {
			return "", false
		}
	case "system":
		// Validate only the source-backed part of the first notice row. Keep the
		// original semantic row in the index so users can search visible chrome.
		if localRow == 0 {
			if semantic == transcriptSearchSystemLabel {
				// At narrow widths the label can occupy a row by itself. It is
				// chrome-only, so skip it without consuming/exhausting source.
				return "", false
			}
			semantic = strings.TrimSpace(strings.TrimPrefix(
				semantic,
				transcriptSearchSystemLabel+" ",
			))
		}
	}
	normalized := strings.Join(strings.Fields(semantic), " ")
	return normalized, normalized != ""
}

func (cursor *transcriptSearchSourceCursor) contains(
	candidate string,
) (bool, int) {
	if cursor == nil || candidate == "" || cursor.offset >= len(cursor.source) {
		return false, 0
	}
	start := cursor.offset
	relative := strings.Index(cursor.source[start:], candidate)
	if relative < 0 {
		cursor.offset = len(cursor.source)
		return false, len(cursor.source) - start
	}
	cursor.offset = start + relative + len(candidate)
	return true, cursor.offset - start
}

func (m *Model) transcriptSearchAnswerSuffix(
	entry ChatEntry,
	live bool,
	paintedRows []string,
	contentWidth int,
) (int, int, bool) {
	var projected strings.Builder
	if live {
		m.renderStreamingAnswer(
			&projected,
			m.streamBuf.String(),
			contentWidth,
		)
	} else {
		answerOnly := entry
		answerOnly.ThinkingContent = ""
		m.renderAssistantMsg(
			&projected,
			answerOnly,
			contentWidth,
		)
	}
	projection := strings.TrimRight(projected.String(), "\n")
	if projection == "" {
		return 0, 0, false
	}
	projectedRows := strings.Split(projection, "\n")
	end := len(paintedRows)
	for end > 0 && paintedRows[end-1] == "" {
		end--
	}
	start := end - len(projectedRows)
	if start < 0 {
		return 0, 0, false
	}
	for index := range projectedRows {
		if paintedRows[start+index] != projectedRows[index] {
			return 0, 0, false
		}
	}
	return start, end, true
}

func (m *Model) recomputeTranscriptSearchMatches(jump bool) {
	if m == nil || m.transcriptSearch == nil {
		return
	}
	state := m.transcriptSearch
	var previous transcriptSearchMatch
	hadPrevious := state.active >= 0 && state.active < len(state.matches)
	if hadPrevious {
		previous = state.matches[state.active]
	}
	if state.generation != m.transcriptPaint.documentGeneration {
		m.rebuildTranscriptSearchIndex()
	}
	state.matches = state.matches[:0]
	state.matchesCapped = false
	state.active = -1
	state.activeRow = -1

	query := strings.TrimSpace(state.input.Value())
	if query == "" {
		return
	}
	expression, err := regexp.Compile("(?i)" + regexp.QuoteMeta(query))
	if err != nil {
		return
	}
	for _, row := range state.rows {
		remaining := transcriptSearchMatchLimit - len(state.matches)
		if remaining == 0 {
			if expression.FindStringIndex(row.RenderedText) != nil {
				state.matchesCapped = true
				break
			}
			continue
		}
		bounds := expression.FindAllStringIndex(row.RenderedText, remaining+1)
		if len(bounds) > remaining {
			state.matchesCapped = true
			bounds = bounds[:remaining]
		}
		for _, bound := range bounds {
			state.matches = append(state.matches, transcriptSearchMatch{
				transcriptSearchRow: row,
				StartByte:           bound[0],
				EndByte:             bound[1],
			})
		}
		if state.matchesCapped {
			break
		}
	}
	if len(state.matches) == 0 {
		return
	}
	state.active = 0
	if !jump && hadPrevious {
		for index, match := range state.matches {
			if transcriptSearchMatchEqual(match, previous) {
				state.active = index
				break
			}
		}
	}
	if jump {
		m.jumpToTranscriptSearchMatch(state.matches[state.active])
	} else if row, ok := m.transcriptSearchMatchRow(
		state.matches[state.active],
	); ok {
		state.activeRow = row
	}
}

func transcriptSearchMatchEqual(left, right transcriptSearchMatch) bool {
	return left.BlockID == right.BlockID &&
		left.TurnID == right.TurnID &&
		left.Revision == right.Revision &&
		left.Logical == right.Logical &&
		left.Grapheme == right.Grapheme &&
		left.LocalRow == right.LocalRow &&
		left.StartByte == right.StartByte &&
		left.EndByte == right.EndByte
}

func (m *Model) navigateTranscriptSearch(delta int) {
	if m == nil || m.transcriptSearch == nil {
		return
	}
	state := m.transcriptSearch
	if state.generation != m.transcriptPaint.documentGeneration {
		hadActive := state.active >= 0 && state.active < len(state.matches)
		var previous transcriptSearchMatch
		if hadActive {
			previous = state.matches[state.active]
		}
		m.recomputeTranscriptSearchMatches(false)
		preserved := hadActive &&
			state.active >= 0 &&
			state.active < len(state.matches) &&
			transcriptSearchMatchEqual(
				state.matches[state.active],
				previous,
			)
		if !preserved {
			if state.active >= 0 && state.active < len(state.matches) {
				m.jumpToTranscriptSearchMatch(state.matches[state.active])
			}
			return
		}
	}
	if len(state.matches) == 0 {
		return
	}
	if state.active < 0 || state.active >= len(state.matches) {
		state.active = 0
	} else {
		state.active = (state.active + delta) % len(state.matches)
		if state.active < 0 {
			state.active += len(state.matches)
		}
	}
	m.jumpToTranscriptSearchMatch(state.matches[state.active])
}

func (m *Model) jumpToTranscriptSearchMatch(match transcriptSearchMatch) {
	if m == nil {
		return
	}
	targetRow, ok := m.transcriptSearchMatchRow(match)
	if !ok {
		return
	}
	if m.transcriptSearch != nil {
		m.transcriptSearch.activeRow = targetRow
	}
	screenBias := min(2, max(0, m.viewport.Height()/3))
	m.setTranscriptYOffset(targetRow - screenBias)
	m.pauseFollow()
}

func (m *Model) transcriptSearchMatchRow(
	match transcriptSearchMatch,
) (int, bool) {
	if m == nil || m.transcriptSearch == nil ||
		m.transcriptSearch.generation !=
			m.transcriptPaint.documentGeneration ||
		match.AbsoluteRow < 0 ||
		match.AbsoluteRow >= m.transcriptPaint.document.totalRows {
		return 0, false
	}
	return match.AbsoluteRow, true
}

func (m *Model) renderTranscriptSearchView() (string, *tea.Cursor) {
	if m == nil || m.transcriptSearch == nil {
		return "", nil
	}
	state := m.transcriptSearch
	state.setWidth(m.chatPaneWidth())
	inputRow := transcriptSearchFitRowForProfile(
		state.input.View(),
		state.width,
		m.glyphProfile,
	)

	status := "type to search safe transcript"
	separator := glyphSeparator(m.glyphProfile)
	nextKey := "enter/↓"
	previousKey := "↑"
	if resolveGlyphProfile(m.glyphProfile) == GlyphASCII {
		nextKey = "enter/down"
		previousKey = "up"
	}
	switch {
	case strings.TrimSpace(state.input.Value()) == "":
	case len(state.matches) == 0:
		status = "0 matches"
	default:
		count := fmt.Sprintf("%d/%d", state.active+1, len(state.matches))
		if state.matchesCapped {
			count = fmt.Sprintf("%d/%d+", state.active+1, len(state.matches))
		}
		status = count + separator + nextKey + " next" +
			separator + previousKey + " previous"
	}
	if state.indexCapped {
		status += separator + "bounded index"
	}
	if state.generation != m.transcriptPaint.documentGeneration {
		status += separator + "transcript changed"
	}
	status += separator + "esc close"
	statusRow := transcriptSearchFitRowForProfile(
		m.styles.OverlayDim.Render(status),
		state.width,
		m.glyphProfile,
	)

	cursor := state.input.Cursor()
	if cursor != nil {
		local := *cursor
		local.X = min(max(0, local.X), max(0, state.width-1))
		local.Y = 0
		cursor = &local
	}
	return inputRow + "\n" + statusRow, cursor
}

func transcriptSearchFitRow(value string, width int) string {
	return transcriptSearchFitRowForProfile(value, width, GlyphUnicode)
}

func transcriptSearchFitRowForProfile(
	value string,
	width int,
	profile GlyphProfile,
) string {
	width = max(1, width)
	if ansi.StringWidth(value) > width {
		// textinput and the status row are semantically styled. The ordinary
		// truncateDisplay helper walks plain grapheme clusters, so feeding it
		// SGR sequences makes invisible color bytes consume the visible cell
		// budget and can clip a colored footer at roughly half its width.
		value = ansi.Truncate(value, width, glyphEllipsis(profile))
	}
	padding := max(0, width-ansi.StringWidth(value))
	return value + strings.Repeat(" ", padding)
}
