package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

const (
	goalPlanCompactWidth  = 50
	goalPlanCompactHeight = 20
	goalPlanMaxCriteria   = 4
)

// goalPlanPhase is a presentation of durable Goal Runtime receipts. It never
// derives progress from assistant prose, tool previews, or Bob convergence.
type goalPlanPhase string

const (
	goalPlanPreparing    goalPlanPhase = "preparing"
	goalPlanCoordinating goalPlanPhase = "coordinating"
	goalPlanRunning      goalPlanPhase = "running"
	goalPlanChecking     goalPlanPhase = "checking"
	goalPlanPaused       goalPlanPhase = "paused"
	goalPlanBlocked      goalPlanPhase = "blocked"
	goalPlanExhausted    goalPlanPhase = "exhausted"
	goalPlanCompleted    goalPlanPhase = "completed"
	goalPlanDropped      goalPlanPhase = "dropped"
)

type goalPlanStyles struct {
	title     lipgloss.Style
	phase     lipgloss.Style
	verified  lipgloss.Style
	pending   lipgloss.Style
	nextLabel lipgloss.Style
	value     lipgloss.Style
	warning   lipgloss.Style
}

func newGoalPlanStyles(isDark bool, themeID string, phase goalPlanPhase) goalPlanStyles {
	palette := outputSemanticPalette(isDark, themeID)
	phaseColor := palette.Accent
	switch phase {
	case goalPlanPaused, goalPlanExhausted:
		phaseColor = palette.Warning
	case goalPlanBlocked:
		phaseColor = palette.Error
	case goalPlanCompleted:
		phaseColor = palette.Success
	case goalPlanDropped:
		phaseColor = palette.Muted
	}
	return goalPlanStyles{
		title:     lipgloss.NewStyle().Foreground(palette.Accent2).Bold(true),
		phase:     lipgloss.NewStyle().Foreground(phaseColor).Bold(true),
		verified:  lipgloss.NewStyle().Foreground(palette.Success),
		pending:   lipgloss.NewStyle().Foreground(palette.Muted),
		nextLabel: lipgloss.NewStyle().Foreground(palette.Accent).Bold(true),
		value:     lipgloss.NewStyle().Foreground(palette.Text),
		warning:   lipgloss.NewStyle().Foreground(palette.Warning),
	}
}

// goalPlanCard is a cached, non-interactive Charm child. The smart parent
// owns lifecycle transitions and validated continuation freshness; this child
// only presents a bounded snapshot.
type goalPlanCard struct {
	snapshot     goal.Snapshot
	continuation *ContinuationActionPresentation
	width        int
	height       int
	isDark       bool
	themeID      string
	glyphProfile GlyphProfile
	styles       goalPlanStyles

	cachedKey    string
	cachedWidth  int
	cachedHeight int
	cachedView   string
	renders      int
}

func newGoalPlanCard(snapshot goal.Snapshot, isDark bool, themeID string, profiles ...GlyphProfile) (*goalPlanCard, bool) {
	if !validGoalPlanSnapshot(snapshot) {
		return nil, false
	}
	phase := goalPlanPhaseForSnapshot(snapshot)
	return &goalPlanCard{
		snapshot:     cloneGoalPlanSnapshot(snapshot),
		width:        1,
		height:       goalPlanCompactHeight,
		isDark:       isDark,
		glyphProfile: resolveGlyphProfile(profiles...),
		styles:       newGoalPlanStyles(isDark, themeID, phase),
	}, true
}

func validGoalPlanSnapshot(snapshot goal.Snapshot) bool {
	if strings.TrimSpace(snapshot.ID) == "" || snapshot.UpdatedAt.IsZero() || !snapshot.State.Valid() || len(snapshot.AcceptanceCriteria) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(snapshot.AcceptanceCriteria))
	for _, criterion := range snapshot.AcceptanceCriteria {
		if strings.TrimSpace(criterion.ID) == "" || strings.TrimSpace(criterion.Description) == "" {
			return false
		}
		if _, duplicate := seen[criterion.ID]; duplicate {
			return false
		}
		seen[criterion.ID] = struct{}{}
	}
	return true
}

func cloneGoalPlanSnapshot(snapshot goal.Snapshot) goal.Snapshot {
	copy := snapshot
	copy.AcceptanceCriteria = append([]goal.AcceptanceCriterion(nil), snapshot.AcceptanceCriteria...)
	if snapshot.PendingContinuation != nil {
		value := *snapshot.PendingContinuation
		copy.PendingContinuation = &value
	}
	if snapshot.LastTurn != nil {
		value := *snapshot.LastTurn
		copy.LastTurn = &value
	}
	if snapshot.Blocker != nil {
		value := *snapshot.Blocker
		copy.Blocker = &value
	}
	if snapshot.Completion != nil {
		value := *snapshot.Completion
		value.Results = append([]goal.AcceptanceResult(nil), snapshot.Completion.Results...)
		copy.Completion = &value
	}
	return copy
}

// SetSnapshot rejects stale or same-revision-equivocating projections. A new
// goal identity gets a new child from the parent rather than mutating this one.
func (c *goalPlanCard) SetSnapshot(snapshot goal.Snapshot) bool {
	if c == nil || !validGoalPlanSnapshot(snapshot) || snapshot.ID != c.snapshot.ID {
		return false
	}
	currentKey := goalPlanSnapshotKey(c.snapshot)
	nextKey := goalPlanSnapshotKey(snapshot)
	switch {
	case snapshot.UpdatedAt.Before(c.snapshot.UpdatedAt):
		return false
	case snapshot.UpdatedAt.Equal(c.snapshot.UpdatedAt) && currentKey != nextKey:
		return false
	case currentKey == nextKey:
		return true
	}
	c.snapshot = cloneGoalPlanSnapshot(snapshot)
	c.styles = newGoalPlanStyles(c.isDark, c.themeID, goalPlanPhaseForSnapshot(snapshot))
	c.invalidate()
	return true
}

// SetContinuation accepts only the same bounded UI type produced by the
// exact continuation interpreter. Command strings and downstream raw maps are
// intentionally not part of this surface.
func (c *goalPlanCard) SetContinuation(action *ContinuationActionPresentation) {
	if c == nil {
		return
	}
	var normalized *ContinuationActionPresentation
	if action != nil {
		if value, ok := normalizeContinuationActionPresentation(*action); ok {
			normalized = &value
		}
	}
	if goalPlanContinuationKey(c.continuation) == goalPlanContinuationKey(normalized) {
		return
	}
	c.continuation = normalized
	c.invalidate()
}

func (c *goalPlanCard) SetSize(width, height int) {
	if c == nil {
		return
	}
	width, height = max(1, width), max(1, height)
	if c.width == width && c.height == height {
		return
	}
	c.width, c.height = width, height
	c.invalidate()
}

func (c *goalPlanCard) SetTheme(isDark bool, themeID string) {
	if c == nil {
		return
	}
	themeID = resolveThemeID(themeID)
	if c.isDark == isDark && resolveThemeID(c.themeID) == themeID {
		return
	}
	c.isDark, c.themeID = isDark, themeID
	c.styles = newGoalPlanStyles(isDark, themeID, goalPlanPhaseForSnapshot(c.snapshot))
	c.invalidate()
}

func (c *goalPlanCard) compact() bool {
	return c == nil || c.width < goalPlanCompactWidth || c.height < goalPlanCompactHeight
}

func (c *goalPlanCard) ownsContinuation() bool {
	return c != nil && !c.compact() && c.continuation != nil
}

func (c *goalPlanCard) invalidate() {
	if c == nil {
		return
	}
	c.cachedKey = ""
	c.cachedWidth = 0
	c.cachedHeight = 0
	c.cachedView = ""
}

func (c *goalPlanCard) View() string {
	if c == nil {
		return ""
	}
	key := fmt.Sprintf("%s\x1d%s\x1d%d", goalPlanSnapshotKey(c.snapshot), goalPlanContinuationKey(c.continuation), c.glyphProfile)
	if c.cachedView != "" && c.cachedKey == key && c.cachedWidth == c.width && c.cachedHeight == c.height {
		return c.cachedView
	}

	view := c.compactView()
	if !c.compact() {
		view = c.expandedView()
	}
	view = strings.TrimRight(view, "\n")
	c.cachedKey, c.cachedWidth, c.cachedHeight, c.cachedView = key, c.width, c.height, view
	c.renders++
	return view
}

func (c *goalPlanCard) compactView() string {
	verified, total := goalPlanAcceptanceProgress(c.snapshot)
	phase := string(goalPlanPhaseForSnapshot(c.snapshot))
	full := fmt.Sprintf("plan · %s · %d/%d verified", phase, verified, total)
	withoutVerified := fmt.Sprintf("plan · %s · %d/%d", phase, verified, total)
	minimal := fmt.Sprintf("plan · %s", phase)
	value := full
	if lipgloss.Width(value) > c.width {
		value = withoutVerified
	}
	if lipgloss.Width(value) > c.width {
		value = minimal
	}
	return truncateDisplay(c.styleCompactHeader(value), c.width)
}

func (c *goalPlanCard) styleCompactHeader(value string) string {
	phase := string(goalPlanPhaseForSnapshot(c.snapshot))
	index := strings.Index(value, phase)
	if index < 0 {
		return c.styles.title.Render(value)
	}
	return c.styles.title.Render(value[:index]) + c.styles.phase.Render(phase) + c.styles.title.Render(value[index+len(phase):])
}

func (c *goalPlanCard) expandedView() string {
	verified, total := goalPlanAcceptanceProgress(c.snapshot)
	header := c.styles.title.Render("plan") + c.styles.pending.Render(" · ") +
		c.styles.phase.Render(string(goalPlanPhaseForSnapshot(c.snapshot))) + c.styles.pending.Render(
		fmt.Sprintf(" · %d/%d verified", verified, total),
	)
	rows := []string{truncateDisplay(header, c.width)}
	results := goalPlanAcceptanceResults(c.snapshot)
	glyphs := glyphSet(c.glyphProfile)
	visible := min(len(c.snapshot.AcceptanceCriteria), goalPlanMaxCriteria)
	for _, criterion := range c.snapshot.AcceptanceCriteria[:visible] {
		_, isVerified := results[criterion.ID]
		label, style := "□ pending", c.styles.pending
		if c.glyphProfile == GlyphASCII {
			label = glyphs.Unselected + " pending"
		}
		if isVerified {
			label, style = glyphs.Success+" verified", c.styles.verified
		}
		description := sanitizeTerminalSingleLine(criterion.Description)
		line := "  " + label + " · " + description
		rows = append(rows, style.Render(truncateDisplay(line, c.width)))
	}
	if hidden := len(c.snapshot.AcceptanceCriteria) - visible; hidden > 0 {
		omission := fmt.Sprintf("  … +%d criteria", hidden)
		if c.glyphProfile == GlyphASCII {
			omission = fmt.Sprintf("  ~ +%d criteria", hidden)
		}
		rows = append(rows, c.styles.pending.Render(truncateDisplayWithGlyphProfile(omission, c.width, c.glyphProfile)))
	}
	if c.continuation != nil {
		rows = append(rows, renderContinuationField(c.width, "next:", c.continuation.Tool, c.styles.nextLabel, c.styles.value))
		if len(c.continuation.Inputs) > 0 {
			rows = append(rows, renderContinuationField(
				c.width, "needs:", continuationIdentifierList(c.continuation.Inputs), c.styles.warning, c.styles.warning,
			))
		}
		blocked := continuationIdentifierList(c.continuation.BlockedBy)
		if blocked == "" {
			blocked = c.continuation.ReasonCode
		}
		if blocked != "" {
			rows = append(rows, renderContinuationField(c.width, "blocked:", blocked, c.styles.warning, c.styles.warning))
		}
	}
	return strings.Join(rows, "\n")
}

func goalPlanPhaseForSnapshot(snapshot goal.Snapshot) goalPlanPhase {
	switch snapshot.State {
	case goal.StatePaused:
		return goalPlanPaused
	case goal.StateBlocked:
		return goalPlanBlocked
	case goal.StateExhausted:
		return goalPlanExhausted
	case goal.StateCompleted:
		return goalPlanCompleted
	case goal.StateDropped:
		return goalPlanDropped
	case goal.StateActive:
		if snapshot.PendingContinuation != nil {
			return goalPlanRunning
		}
		if snapshot.LastTurn != nil {
			return goalPlanChecking
		}
		if strings.TrimSpace(snapshot.Cortex.TaskID) != "" {
			return goalPlanCoordinating
		}
		return goalPlanPreparing
	default:
		return goalPlanBlocked
	}
}

func goalPlanAcceptanceResults(snapshot goal.Snapshot) map[string]goal.AcceptanceResult {
	results := make(map[string]goal.AcceptanceResult)
	// Completion is the only durable authority that can check a criterion. A
	// productive turn, Cortex link, or Bob clean receipt is never enough.
	if snapshot.State != goal.StateCompleted || snapshot.Completion == nil {
		return results
	}
	for _, result := range snapshot.Completion.Results {
		if result.Satisfied && strings.TrimSpace(result.Evidence) != "" {
			results[result.CriterionID] = result
		}
	}
	return results
}

func goalPlanAcceptanceProgress(snapshot goal.Snapshot) (verified, total int) {
	results := goalPlanAcceptanceResults(snapshot)
	for _, criterion := range snapshot.AcceptanceCriteria {
		if _, ok := results[criterion.ID]; ok {
			verified++
		}
	}
	return verified, len(snapshot.AcceptanceCriteria)
}

func goalPlanSnapshotKey(snapshot goal.Snapshot) string {
	verified := goalPlanAcceptanceResults(snapshot)
	parts := []string{
		snapshot.ID,
		snapshot.UpdatedAt.UTC().Format(time.RFC3339Nano),
		string(snapshot.State),
		string(goalPlanPhaseForSnapshot(snapshot)),
	}
	for _, criterion := range snapshot.AcceptanceCriteria {
		state := "pending"
		if _, ok := verified[criterion.ID]; ok {
			state = "verified"
		}
		parts = append(parts, criterion.ID, criterion.Description, state)
	}
	return strings.Join(parts, "\x1e")
}

func goalPlanContinuationKey(action *ContinuationActionPresentation) string {
	if action == nil {
		return ""
	}
	return continuationActionCacheKey(*action)
}

func (m *Model) syncGoalPlan() {
	if m == nil || m.goalRuntime == nil {
		m.goalPlan = nil
		return
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		return
	}
	if m.goalPlan == nil || m.goalPlan.snapshot.ID != snapshot.ID {
		card, ok := newGoalPlanCard(snapshot, m.isDark, m.themeID, m.glyphProfile)
		if !ok {
			m.goalPlan = nil
			return
		}
		m.goalPlan = card
	} else if !m.goalPlan.SetSnapshot(snapshot) {
		return
	}
	m.goalPlan.SetSize(m.chatPaneWidth(), m.height)
	var action *ContinuationActionPresentation
	if m.continuation.card != nil {
		value := m.continuation.card.action
		action = &value
	}
	m.goalPlan.SetContinuation(action)
}

func (m *Model) goalPlanVisible() bool {
	return m != nil && m.goalRuntime != nil && !m.initializing && !m.shuttingDown &&
		m.overlay == OverlayNone && m.pendingApproval == nil && m.readScopePrompt == nil &&
		m.pendingPaste == nil && m.pendingSessionSwitch == nil
}

func (m *Model) renderGoalPlan() string {
	if !m.goalPlanVisible() {
		return ""
	}
	m.syncGoalPlan()
	if m.goalPlan == nil {
		return ""
	}
	return m.goalPlan.View()
}

func (m *Model) goalPlanOwnsContinuation() bool {
	return m != nil && m.goalPlanVisible() && m.goalPlan != nil && m.goalPlan.ownsContinuation()
}
