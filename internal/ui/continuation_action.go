package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

const (
	maxContinuationToolCells       = 96
	maxContinuationIdentifierCells = 64
	maxContinuationItems           = 6
	continuationCompactWidth       = 48
	continuationFieldRows          = 1
)

type continuationActionStyles struct {
	nextLabel      lipgloss.Style
	attentionLabel lipgloss.Style
	value          lipgloss.Style
	attention      lipgloss.Style
}

type continuationActionState struct {
	card     *continuationActionCard
	turnID   string
	sequence uint64
}

func newContinuationActionStyles(isDark bool, themeID string) continuationActionStyles {
	palette := outputSemanticPalette(isDark, themeID)
	return continuationActionStyles{
		nextLabel:      lipgloss.NewStyle().Foreground(palette.Accent).Bold(true),
		attentionLabel: lipgloss.NewStyle().Foreground(palette.Warning).Bold(true),
		value:          lipgloss.NewStyle().Foreground(palette.Text),
		attention:      lipgloss.NewStyle().Foreground(palette.Warning),
	}
}

// continuationActionCard is a dumb, non-interactive child. The parent owns
// freshness and visibility; this child owns only bounded responsive rendering.
type continuationActionCard struct {
	action  ContinuationActionPresentation
	width   int
	isDark  bool
	themeID string
	styles  continuationActionStyles

	cachedKey   string
	cachedWidth int
	cachedView  string
}

func newContinuationActionCard(action ContinuationActionPresentation, isDark bool, themeID string) *continuationActionCard {
	return &continuationActionCard{
		action: action,
		isDark: isDark,
		styles: newContinuationActionStyles(isDark, themeID),
	}
}

func (c *continuationActionCard) SetWidth(width int) {
	width = max(1, width)
	if c.width == width {
		return
	}
	c.width = width
	c.invalidate()
}

func (c *continuationActionCard) SetTheme(isDark bool, themeID string) {
	if c.isDark == isDark {
		return
	}
	c.isDark = isDark
	c.styles = newContinuationActionStyles(isDark, c.themeID)
	c.invalidate()
}

func (c *continuationActionCard) invalidate() {
	c.cachedKey = ""
	c.cachedWidth = 0
	c.cachedView = ""
}

func (c *continuationActionCard) View() string {
	if c == nil {
		return ""
	}
	width := max(1, c.width)
	key := continuationActionCacheKey(c.action)
	if c.cachedView != "" && c.cachedKey == key && c.cachedWidth == width {
		return c.cachedView
	}

	var view string
	if width < continuationCompactWidth {
		view = c.compactView(width)
	} else {
		view = c.expandedView(width)
	}
	view = strings.TrimRight(view, "\n")
	c.cachedKey = key
	c.cachedWidth = width
	c.cachedView = view
	return view
}

func (c *continuationActionCard) compactView(width int) string {
	available := max(1, width-1)
	nextLabel := "next: "
	next := c.styles.nextLabel.Render(nextLabel) +
		c.styles.value.Render(truncateDisplay(c.action.Tool, max(1, available-lipgloss.Width(nextLabel))))
	attention := make([]string, 0, 2)
	if len(c.action.Inputs) > 0 {
		attention = append(attention, compactContinuationCount("needs", len(c.action.Inputs), "input"))
	}
	blockerCount := len(c.action.BlockedBy)
	if blockerCount == 0 && c.action.ReasonCode != "" {
		blockerCount = 1
	}
	if blockerCount > 0 {
		attention = append(attention, compactContinuationCount("blocked", blockerCount, "blocker"))
	}
	rows := []string{" " + next}
	if len(attention) > 0 {
		attentionText := truncateDisplay(strings.Join(attention, " · "), available)
		rows = append(rows, " "+c.styles.attention.Render(attentionText))
	}
	return strings.Join(rows, "\n")
}

func compactContinuationCount(label string, count int, noun string) string {
	if count == 1 {
		return label + ": 1 " + noun
	}
	return fmt.Sprintf("%s: %d %ss", label, count, noun)
}

func (c *continuationActionCard) expandedView(width int) string {
	rows := []string{renderContinuationField(
		width, "next:", c.action.Tool, c.styles.nextLabel, c.styles.value,
	)}
	if len(c.action.Inputs) > 0 {
		rows = append(rows, renderContinuationField(
			width, "needs:", continuationIdentifierList(c.action.Inputs),
			c.styles.attentionLabel, c.styles.attention,
		))
	}
	blocked := continuationIdentifierList(c.action.BlockedBy)
	if blocked == "" {
		blocked = c.action.ReasonCode
	}
	if blocked != "" {
		rows = append(rows, renderContinuationField(
			width, "blocked:", blocked, c.styles.attentionLabel, c.styles.attention,
		))
	}
	return strings.Join(rows, "\n")
}

func renderContinuationField(width int, label, value string, labelStyle, valueStyle lipgloss.Style) string {
	const leftPadding = "  "
	prefix := leftPadding + labelStyle.Render(label) + " "
	indent := strings.Repeat(" ", lipgloss.Width(prefix))
	valueWidth := max(1, width-lipgloss.Width(prefix))
	value = truncateDisplay(value, valueWidth*continuationFieldRows)
	lines := strings.Split(wrapText(value, valueWidth), "\n")
	if len(lines) > continuationFieldRows {
		lines = lines[:continuationFieldRows]
	}
	for index := range lines {
		lines[index] = valueStyle.Render(lines[index])
		if index == 0 {
			lines[index] = prefix + lines[index]
		} else {
			lines[index] = indent + lines[index]
		}
	}
	return strings.Join(lines, "\n")
}

func continuationIdentifierList(values []string) string {
	return strings.Join(values, ", ")
}

func continuationActionCacheKey(action ContinuationActionPresentation) string {
	return strings.Join([]string{
		action.Tool,
		strings.Join(action.Inputs, "\x1f"),
		strings.Join(action.BlockedBy, "\x1f"),
		action.ReasonCode,
	}, "\x1e")
}

func normalizeContinuationActionPresentation(input ContinuationActionPresentation) (ContinuationActionPresentation, bool) {
	tool := boundedContinuationIdentifier(input.Tool, maxContinuationToolCells)
	if tool == "" {
		return ContinuationActionPresentation{}, false
	}
	return ContinuationActionPresentation{
		Tool:       tool,
		Inputs:     boundedContinuationIdentifiers(input.Inputs),
		BlockedBy:  boundedContinuationIdentifiers(input.BlockedBy),
		ReasonCode: boundedContinuationIdentifier(input.ReasonCode, maxContinuationIdentifierCells),
	}, true
}

func boundedContinuationIdentifiers(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, min(len(values), maxContinuationItems))
	seen := make(map[string]struct{}, min(len(values), maxContinuationItems))
	for _, value := range values {
		value = boundedContinuationIdentifier(value, maxContinuationIdentifierCells)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if len(result) == maxContinuationItems {
			break
		}
	}
	return result
}

func boundedContinuationIdentifier(value string, limit int) string {
	value = safeToolIdentifier(sanitizeTerminalSingleLine(value))
	return strings.TrimSpace(truncateDisplay(value, limit))
}

func (m *Model) beginContinuationTurn(turnID string) {
	m.continuation = continuationActionState{turnID: strings.TrimSpace(turnID)}
}

func (m *Model) clearContinuationAction() {
	m.continuation = continuationActionState{}
}

func (m *Model) handleContinuationAction(message ContinuationActionMsg) {
	if strings.TrimSpace(message.TurnID) == "" || message.TurnID != m.continuation.turnID ||
		message.Sequence == 0 || message.Sequence <= m.continuation.sequence {
		return
	}

	followWasPaused := m.followPaused()
	followYOffset := m.transcriptYOffset()
	m.continuation.sequence = message.Sequence
	m.continuation.card = nil
	if message.Action != nil {
		if normalized, ok := normalizeContinuationActionPresentation(*message.Action); ok {
			m.continuation.card = newContinuationActionCard(normalized, m.isDark, m.themeID)
			m.continuation.card.SetWidth(m.chatPaneWidth())
		}
	}
	if m.ready {
		m.recalcViewportHeight()
		m.refreshTranscript()
		m.restoreFollowPosition(followWasPaused, followYOffset)
	}
}

func (m *Model) continuationActionVisible() bool {
	if m == nil || m.continuation.card == nil || m.state != StateIdle || m.initializing || m.shuttingDown ||
		m.overlay != OverlayNone || m.pendingApproval != nil || m.pendingPaste != nil ||
		m.readScopePrompt != nil || m.queuedFollowUp != nil || m.composerIsBusy() {
		return false
	}
	return true
}

func (m *Model) renderContinuationAction() string {
	if !m.continuationActionVisible() {
		return ""
	}
	m.continuation.card.SetWidth(m.chatPaneWidth())
	return m.continuation.card.View()
}
