package ui

import (
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// transcriptSelection is in-app drag-select over the transcript. Mouse
// reporting (DEC 1002) is what makes the wheel scroll the chat; it also
// consumes press and release, so the terminal cannot drag-select. Crush, Grok
// Build, and Claude Code fullscreen all keep capture on and paint the
// selection themselves. Codex still uses /toggle-mouse-mode for the same
// trade; this is the path that does not ask the user to pick one.
//
// Coordinates are document rows (viewport offset + screen y) and cell
// columns, so a selection stays on the text when the user wheels.
type transcriptSelection struct {
	active    bool
	dragging  bool
	startRow  int
	startCol  int
	endRow    int
	endCol    int
	clickN    int
	lastClick tea.MouseClickMsg
	lastAt    int64 // unix millis; 0 means none
}

const transcriptMultiClickMs = 400
const transcriptClickSlop = 1

func (s transcriptSelection) empty() bool {
	return !s.active || (s.startRow == s.endRow && s.startCol == s.endCol)
}

func (s transcriptSelection) normalized() (row0, col0, row1, col1 int) {
	row0, col0 = s.startRow, s.startCol
	row1, col1 = s.endRow, s.endCol
	if row1 < row0 || (row1 == row0 && col1 < col0) {
		return row1, col1, row0, col0
	}
	return row0, col0, row1, col1
}

func (m *Model) clearTranscriptSelection() {
	if m == nil || !m.transcriptSel.active {
		return
	}
	m.transcriptSel = transcriptSelection{}
	m.invalidateTranscriptSelectionPaint()
}

func (m *Model) invalidateTranscriptSelectionPaint() {
	if m == nil {
		return
	}
	if m.transcriptVirtualized() {
		m.transcriptPaint.windowGeneration--
		m.syncTranscriptPaintWindow()
		return
	}
	// Non-virtual fixtures paint through viewport content directly.
}

func (m *Model) transcriptPointerPos(x, y int) (row, col int, ok bool) {
	if m == nil || x < 0 || y < 0 {
		return 0, 0, false
	}
	if y >= m.viewport.Height() || x >= m.viewport.Width() {
		return 0, 0, false
	}
	return y + m.transcriptYOffset(), x, true
}

func (m *Model) transcriptPointerOnDisclosure(x, y int) bool {
	if m == nil {
		return false
	}
	row := y + m.transcriptYOffset()
	for _, region := range m.toolHitRegions {
		if region.contains(x, row) {
			return true
		}
	}
	for _, region := range m.thinkingHitRegions {
		if region.contains(x, row) {
			return true
		}
	}
	return false
}

func (m *Model) beginTranscriptSelection(msg tea.MouseClickMsg) {
	row, col, ok := m.transcriptPointerPos(msg.X, msg.Y)
	if !ok {
		return
	}
	now := time.Now().UnixMilli()
	clicks := 1
	if m.transcriptSel.lastAt > 0 &&
		now-m.transcriptSel.lastAt <= transcriptMultiClickMs &&
		absInt(msg.X-m.transcriptSel.lastClick.X) <= transcriptClickSlop &&
		absInt(msg.Y-m.transcriptSel.lastClick.Y) <= transcriptClickSlop {
		clicks = m.transcriptSel.clickN + 1
		if clicks > 3 {
			clicks = 1
		}
	}
	m.transcriptSel = transcriptSelection{
		active:    true,
		dragging:  true,
		startRow:  row,
		startCol:  col,
		endRow:    row,
		endCol:    col,
		clickN:    clicks,
		lastClick: msg,
		lastAt:    now,
	}
	switch clicks {
	case 2:
		m.expandTranscriptSelectionWord()
	case 3:
		m.expandTranscriptSelectionLine()
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func (m *Model) updateTranscriptSelectionDrag(x, y int) {
	if m == nil || !m.transcriptSel.dragging {
		return
	}
	row, col, ok := m.transcriptPointerPos(x, y)
	if !ok {
		// Clamp to the visible transcript when the pointer leaves it.
		if y < 0 {
			row = m.transcriptYOffset()
		} else {
			row = m.transcriptYOffset() + max(0, m.viewport.Height()-1)
		}
		col = min(max(0, x), max(0, m.viewport.Width()-1))
	}
	if row == m.transcriptSel.endRow && col == m.transcriptSel.endCol {
		return
	}
	m.transcriptSel.endRow = row
	m.transcriptSel.endCol = col
	m.invalidateTranscriptSelectionPaint()
}

func (m *Model) finishTranscriptSelection() tea.Cmd {
	if m == nil || !m.transcriptSel.dragging {
		return nil
	}
	m.transcriptSel.dragging = false
	if m.transcriptSel.empty() {
		m.transcriptSel.active = false
		return nil
	}
	text := strings.TrimRight(m.transcriptSelectionText(), "\n")
	if strings.TrimSpace(text) == "" {
		m.transcriptSel.active = false
		m.invalidateTranscriptSelectionPaint()
		return nil
	}
	m.invalidateTranscriptSelectionPaint()
	return m.copyToClipboard(text)
}

func (m *Model) transcriptSelectionText() string {
	if m == nil || m.transcriptSel.empty() {
		return ""
	}
	row0, col0, row1, col1 := m.transcriptSel.normalized()
	rows := m.transcriptRowsForSelection(row0, row1)
	if len(rows) == 0 {
		return ""
	}
	origin := m.contentGrid().OriginX()
	var b strings.Builder
	for i, row := range rows {
		plain := ansi.Strip(row)
		start := 0
		end := ansi.StringWidth(plain)
		if i == 0 {
			start = col0
		}
		if i == len(rows)-1 {
			end = col1
		}
		// Gutter/accent is not part of what you meant to copy.
		if start < origin {
			start = origin
		}
		if end < start {
			end = start
		}
		line := strings.TrimRight(ansi.Cut(plain, start, end), " ")
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

func (m *Model) transcriptRowsForSelection(row0, row1 int) []string {
	if row1 < row0 {
		row0, row1 = row1, row0
	}
	if m.transcriptVirtualized() {
		rows, _ := m.transcriptPaint.document.materializeRows(row0, row1+1)
		return rows
	}
	all := strings.Split(m.viewport.GetContent(), "\n")
	if row0 < 0 {
		row0 = 0
	}
	if row0 >= len(all) {
		return nil
	}
	if row1 >= len(all) {
		row1 = len(all) - 1
	}
	return all[row0 : row1+1]
}

func (m *Model) styleTranscriptSelectionWindowRows(rows []string, windowStart int) {
	if m == nil || m.transcriptSel.empty() || len(rows) == 0 {
		return
	}
	row0, col0, row1, col1 := m.transcriptSel.normalized()
	style := m.transcriptSelectionStyle()
	origin := m.contentGrid().OriginX()
	for i := range rows {
		docRow := windowStart + i
		if docRow < row0 || docRow > row1 {
			continue
		}
		plain := ansi.Strip(rows[i])
		width := ansi.StringWidth(plain)
		start := 0
		end := width
		if docRow == row0 {
			start = col0
		}
		if docRow == row1 {
			end = col1
		}
		if start < origin {
			start = origin
		}
		if start < 0 {
			start = 0
		}
		if end > width {
			end = width
		}
		if end <= start {
			continue
		}
		left := ansi.Cut(plain, 0, start)
		mid := ansi.Cut(plain, start, end)
		right := ansi.Cut(plain, end, width)
		rows[i] = left + style.Render(mid) + right
	}
}

func (m *Model) transcriptSelectionStyle() lipgloss.Style {
	// Reverse uses the existing cell colours, so it does not invent a
	// twelfth palette role. Search already owns Accent+Bold on a whole row.
	return lipgloss.NewStyle().Reverse(true)
}

func (m *Model) expandTranscriptSelectionWord() {
	if m == nil || !m.transcriptSel.active {
		return
	}
	row, col := m.transcriptSel.startRow, m.transcriptSel.startCol
	rows := m.transcriptRowsForSelection(row, row)
	if len(rows) == 0 {
		return
	}
	plain := []rune(ansi.Strip(rows[0]))
	if col > len(plain) {
		col = len(plain)
	}
	if col < 0 {
		col = 0
	}
	start, end := col, col
	for start > 0 && isSelectWordRune(plain[start-1]) {
		start--
	}
	for end < len(plain) && isSelectWordRune(plain[end]) {
		end++
	}
	if end <= start {
		return
	}
	m.transcriptSel.startCol = start
	m.transcriptSel.endCol = end
	m.transcriptSel.endRow = row
	m.invalidateTranscriptSelectionPaint()
}

func (m *Model) expandTranscriptSelectionLine() {
	if m == nil || !m.transcriptSel.active {
		return
	}
	row := m.transcriptSel.startRow
	rows := m.transcriptRowsForSelection(row, row)
	width := 0
	if len(rows) > 0 {
		width = ansi.StringWidth(ansi.Strip(rows[0]))
	}
	m.transcriptSel.startCol = m.contentGrid().OriginX()
	m.transcriptSel.endCol = width
	m.transcriptSel.endRow = row
	m.invalidateTranscriptSelectionPaint()
}

func isSelectWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == '/'
}

func (m *Model) handleTranscriptMouseRelease(msg tea.MouseReleaseMsg) (cmd tea.Cmd, handled bool) {
	if m == nil || msg.Button != tea.MouseLeft {
		return nil, false
	}
	if m.pendingApproval != nil || m.readScopePrompt != nil ||
		m.viewerModalActive() || m.overlay != OverlayNone {
		m.transcriptSel.dragging = false
		return nil, true
	}
	if m.transcriptSel.dragging {
		return m.finishTranscriptSelection(), true
	}
	return nil, false
}

func (m *Model) handleTranscriptMouseMotion(msg tea.MouseMotionMsg) (cmd tea.Cmd, handled bool) {
	if m == nil || !m.transcriptSel.dragging {
		return nil, false
	}
	if m.pendingApproval != nil || m.readScopePrompt != nil ||
		m.viewerModalActive() || m.overlay != OverlayNone {
		return nil, true
	}
	m.updateTranscriptSelectionDrag(msg.X, msg.Y)
	return nil, true
}
