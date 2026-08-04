package ui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

// GoalInspectorOptions contains presentation-only host state. The durable
// snapshot and resolved actions remain immutable for the lifetime of a modal;
// the smart parent opens a fresh inspector after every lifecycle transition.
type GoalInspectorOptions struct {
	Width            int
	Height           int
	IsDark           bool
	ThemeID          string
	ReducedMotion    bool
	GlyphProfile     GlyphProfile
	Now              time.Time
	PersistenceDirty bool
	RecoveryStatus   string
}

// GoalInspectorEvent asks the parent to execute one already-resolved command
// action. The child never mutates the goal runtime or persists session state.
type GoalInspectorEvent struct {
	ActionID command.ActionID
	Action   command.Action
}

// GoalInspector is a read-only goal document plus a small action rail. A
// Bubbles viewport keeps the same information reachable at 30x12 without
// letting the modal grow beyond the terminal.
type GoalInspector struct {
	snapshot         goal.Snapshot
	actions          []command.ActionState
	selected         int
	confirming       bool
	width            int
	height           int
	isDark           bool
	themeID          string
	reducedMotion    bool
	glyphProfile     GlyphProfile
	now              time.Time
	persistenceDirty bool
	recoveryStatus   string
	styles           Styles
	viewport         viewport.Model
	cache            goalInspectorRenderCache
}

type goalInspectorRenderCache struct {
	valid   bool
	view    string
	renders int
}

// NewGoalInspector creates a focused, persistence-free inspector.
func NewGoalInspector(snapshot goal.Snapshot, actions []command.ActionState, options GoalInspectorOptions) *GoalInspector {
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	inspector := &GoalInspector{
		snapshot:         snapshot,
		actions:          append([]command.ActionState(nil), actions...),
		width:            max(minTerminalWidth, options.Width),
		height:           max(minTerminalHeight, options.Height),
		isDark:           options.IsDark,
		reducedMotion:    options.ReducedMotion,
		glyphProfile:     resolveGlyphProfile(options.GlyphProfile),
		now:              now,
		persistenceDirty: options.PersistenceDirty,
		recoveryStatus:   strings.TrimSpace(options.RecoveryStatus),
		styles:           NewStyles(options.IsDark, resolveThemeID(options.ThemeID)),
	}
	inspector.selected = inspector.firstEnabledAction()
	inspector.rebuildViewport(false)
	return inspector
}

func (i *GoalInspector) firstEnabledAction() int {
	for index := range i.actions {
		if i.actions[index].Enabled {
			return index
		}
	}
	return 0
}

// SetSize adapts both the document viewport and compact action grammar.
func (i *GoalInspector) SetSize(width, height int) {
	if i == nil {
		return
	}
	width = max(minTerminalWidth, width)
	height = max(minTerminalHeight, height)
	if i.width == width && i.height == height {
		return
	}
	i.width, i.height = width, height
	i.rebuildViewport(true)
}

// SetTheme reapplies the project LightDark-derived semantic palette.
func (i *GoalInspector) SetTheme(isDark bool, themeID string) {
	if i == nil {
		return
	}
	themeID = resolveThemeID(themeID)
	if i.isDark == isDark && resolveThemeID(i.themeID) == themeID {
		return
	}
	i.isDark, i.themeID = isDark, themeID
	i.styles = NewStyles(isDark, i.themeID)
	i.rebuildViewport(true)
}

// SetReducedMotion records the shared accessibility preference. The inspector
// intentionally has no autonomous ticker, so selection remains deterministic.
func (i *GoalInspector) SetReducedMotion(reduced bool) {
	if i == nil || i.reducedMotion == reduced {
		return
	}
	i.reducedMotion = reduced
	i.invalidate()
}

func (i *GoalInspector) contentWidth() int {
	// The frame owns one padding cell on each side. Reserve them before
	// wrapping so Lip Gloss never adds surprise rows at the terminal edge.
	return max(1, pickerListWidth(i.width)-2)
}

func (i *GoalInspector) compact() bool {
	return i.width <= 48 || i.height < 20
}

func (i *GoalInspector) viewportHeight() int {
	// Border (2), title/state (2), action header/rail/detail (4), and footer
	// (1), plus the top-level View's trailing safety row, leave two readable
	// document rows at the supported 12-row minimum.
	return max(2, min(14, i.height-10))
}

func (i *GoalInspector) rebuildViewport(preserveOffset bool) {
	if i == nil {
		return
	}
	offset := 0
	if preserveOffset {
		offset = i.viewport.YOffset()
	}
	vp := viewport.New(
		viewport.WithWidth(i.contentWidth()),
		viewport.WithHeight(i.viewportHeight()),
	)
	vp.KeyMap.Up.SetEnabled(false)
	vp.KeyMap.Down.SetEnabled(false)
	vp.KeyMap.PageUp.SetEnabled(false)
	vp.KeyMap.PageDown.SetEnabled(false)
	vp.KeyMap.HalfPageUp.SetEnabled(false)
	vp.KeyMap.HalfPageDown.SetEnabled(false)
	vp.SetContent(i.buildDocument())
	vp.SetYOffset(offset)
	i.viewport = vp
	i.invalidate()
}

// Update handles presentation navigation only and returns intent to the smart
// parent. Destructive actions require a second explicit Enter receipt.
func (i *GoalInspector) Update(msg tea.KeyPressMsg) (GoalInspectorEvent, tea.Cmd) {
	if i == nil {
		return GoalInspectorEvent{}, nil
	}
	switch msg.String() {
	case "left", "h":
		i.moveAction(-1)
	case "right", "l":
		i.moveAction(1)
	case "enter", " ":
		if len(i.actions) == 0 {
			return GoalInspectorEvent{}, nil
		}
		selected := i.actions[i.selected]
		if !selected.Enabled {
			i.confirming = false
			i.invalidate()
			return GoalInspectorEvent{}, nil
		}
		if selected.Spec.Destructive && !i.confirming {
			i.confirming = true
			i.invalidate()
			return GoalInspectorEvent{}, nil
		}
		i.confirming = false
		i.invalidate()
		return GoalInspectorEvent{ActionID: selected.Spec.ID, Action: selected.Spec.Action}, nil
	default:
		if navigateReadOnlyViewport(&i.viewport, msg.String()) {
			i.invalidate()
		}
	}
	return GoalInspectorEvent{}, nil
}

func (i *GoalInspector) moveAction(delta int) {
	if len(i.actions) == 0 || delta == 0 {
		return
	}
	i.selected = (i.selected + delta + len(i.actions)) % len(i.actions)
	i.confirming = false
	i.invalidate()
}

// CancelConfirmation consumes Escape only while a destructive action is
// armed. A second Escape is then owned by the parent and closes the modal.
func (i *GoalInspector) CancelConfirmation() bool {
	if i == nil || !i.confirming {
		return false
	}
	i.confirming = false
	i.invalidate()
	return true
}

func (i *GoalInspector) updateViewport(msg tea.MouseWheelMsg) {
	if i == nil {
		return
	}
	i.viewport, _ = i.viewport.Update(msg)
	i.invalidate()
}

func (i *GoalInspector) invalidate() {
	if i != nil {
		i.cache.valid = false
	}
}

// headerShowsFullObjective reports whether the phase line above the document
// carries the objective in full, so the document can skip repeating it.
func (i *GoalInspector) headerShowsFullObjective() bool {
	objective := strings.TrimSpace(i.snapshot.Objective)
	if objective == "" {
		return true
	}
	header := ansi.Strip(RenderGoalStatusLine(GoalSummary{
		Objective: objective,
		Phase:     GoalPhase(i.snapshot.State),
	}, i.contentWidth(), i.isDark, i.themeID, i.glyphProfile))
	return strings.Contains(header, objective)
}

// View renders a cached modal frame. The viewport is only rebuilt when data,
// size, theme, or navigation changes.
func (i *GoalInspector) View() string {
	if i == nil {
		return ""
	}
	if i.cache.valid {
		return i.cache.view
	}

	width := i.contentWidth()
	verified, total := goalAcceptanceProgress(i.snapshot)
	phase := RenderGoalStatusLine(GoalSummary{
		Objective: i.snapshot.Objective,
		Phase:     GoalPhase(i.snapshot.State),
	}, width, i.isDark, i.themeID, i.glyphProfile)
	var b strings.Builder
	title := fmt.Sprintf("Goal inspector · %d/%d verified", verified, total)
	if i.compact() {
		title = fmt.Sprintf("Goal · %d/%d verified", verified, total)
	}
	title = truncateDisplay(title, width)
	b.WriteString(i.styles.OverlayTitle.Render(title))
	b.WriteByte('\n')
	b.WriteString(phase)
	b.WriteByte('\n')
	b.WriteString(i.viewport.View())
	b.WriteByte('\n')
	b.WriteString(i.renderActionHeader(width))
	b.WriteByte('\n')
	b.WriteString(i.renderActionRail(width))
	b.WriteByte('\n')
	b.WriteString(i.renderActionDetail(width))
	b.WriteByte('\n')
	b.WriteString(i.renderFooter(width))

	view := lipgloss.NewStyle().
		Border(borderForGlyphProfile(i.glyphProfile)).
		BorderForeground(i.styles.OverlayBorder).
		Padding(0, 1).
		Width(pickerListWidth(i.width) + 2).
		Render(b.String())
	i.cache.valid = true
	i.cache.view = view
	i.cache.renders++
	return view
}

func (i *GoalInspector) buildDocument() string {
	width := i.contentWidth()
	var b strings.Builder
	// The panel header already reads "Ⅱ paused · <objective>". Repeating it as
	// an Objective section directly underneath printed the same sentence twice
	// in the top three rows. Keep the section only when the header had to
	// truncate, which is the one case where the body still adds something.
	if !i.headerShowsFullObjective() {
		i.writeSection(&b, "Objective", i.snapshot.Objective, width)
	}

	results := goalAcceptanceResults(i.snapshot)
	glyphs := glyphSet(i.glyphProfile)
	b.WriteString(i.styles.OverlayAccent.Render("Acceptance criteria"))
	b.WriteByte('\n')
	for _, criterion := range i.snapshot.AcceptanceCriteria {
		result, verified := results[criterion.ID]
		marker := glyphs.Unselected + " pending"
		style := i.styles.OverlayDim
		if verified {
			marker = glyphs.Success + " verified"
			style = i.styles.StatusCheck
		}
		line := marker + " · " + criterion.Description
		for _, wrapped := range strings.Split(wrapText(line, width), "\n") {
			b.WriteString(style.Render(wrapped))
			b.WriteByte('\n')
		}
		if verified && strings.TrimSpace(result.Evidence) != "" && !i.compact() {
			b.WriteString(i.styles.OverlayDim.Render(wrapText("  proof · "+result.Evidence, width)))
			b.WriteByte('\n')
		}
	}
	b.WriteString(i.styles.OverlayDim.Render("Productive turns are progress signals, not acceptance proof."))
	b.WriteString("\n\n")

	status := string(i.snapshot.State)
	if reason := strings.TrimSpace(i.snapshot.StateReason); reason != "" {
		status += " · " + reason
	}
	i.writeSection(&b, "State", status, width)
	if i.persistenceDirty {
		i.writeSection(&b, "Persistence", "retry required before dispatch or lifecycle changes", width)
	}
	if blocker := i.snapshot.Blocker; blocker != nil {
		value := string(blocker.Kind) + " · " + blocker.Reason
		if blocker.Reference != "" {
			value += " · ref " + blocker.Reference
		}
		i.writeSection(&b, "Blocker", value, width)
	}
	if i.recoveryStatus != "" {
		i.writeSection(&b, "Recovery", i.recoveryStatus, width)
	}
	if pending := i.snapshot.PendingContinuation; pending != nil {
		value := fmt.Sprintf("turn %s · %s admission has no settled receipt", pending.TurnID, pending.Kind)
		if pending.Kind == goal.AdmissionAutomatic {
			value = fmt.Sprintf("turn %s · automatic permit %d has no settled receipt", pending.TurnID, pending.Ordinal)
		}
		i.writeSection(&b, "In flight", value, width)
	}

	if turn := i.snapshot.LastTurn; turn != nil {
		kind := "not productive"
		if turn.Productive {
			kind = "productive signal"
		}
		if turn.OutcomeUnknown {
			kind = "outcome unknown"
		}
		value := fmt.Sprintf("%s · %s eval · %s", kind, formatGoalTokens(turn.EvalTokens), goalRelativeTime(i.now, turn.RecordedAt))
		// A paused goal's State reason is usually the last turn's summary word
		// for word, which printed the same failure text twice a few rows apart.
		// State is the more prominent of the two, so it keeps the text.
		if summary := strings.TrimSpace(turn.Summary); summary != "" &&
			!strings.Contains(status, summary) {
			value += " · " + summary
		}
		i.writeSection(&b, "Last turn", value, width)
	} else {
		i.writeSection(&b, "Last turn", "none recorded", width)
	}

	if cortex := i.snapshot.Cortex; cortex.TaskID != "" {
		i.writeSection(&b, "Cortex", fmt.Sprintf("%s · revision %d", cortex.TaskID, cortex.Revision), width)
	} else {
		i.writeSection(&b, "Cortex", "not linked · bounded local goal", width)
	}

	i.writeSection(&b, "Budget", goalInspectorBudget(i.snapshot, i.now), width)
	return strings.TrimRight(b.String(), "\n")
}

func (i *GoalInspector) writeSection(b *strings.Builder, label, value string, width int) {
	b.WriteString(i.styles.OverlayAccent.Render(label))
	b.WriteByte('\n')
	b.WriteString(i.styles.OverlayDim.Render(wrapText(strings.TrimSpace(value), width)))
	b.WriteString("\n\n")
}

func (i *GoalInspector) renderActionHeader(width int) string {
	if len(i.actions) == 0 {
		return i.styles.OverlayAccent.Render("Actions")
	}
	// The rail below lists every action with the selected one marked, so an
	// "n/total" counter beside the heading restated it in a form nobody reads.
	return i.styles.OverlayAccent.Render(truncateDisplay("Actions", width))
}

func (i *GoalInspector) renderActionRail(width int) string {
	if len(i.actions) == 0 {
		return i.styles.OverlayDim.Render("No actions available")
	}
	if i.compact() {
		selected := i.actions[i.selected]
		left, right := "‹", "›"
		if i.glyphProfile == GlyphASCII {
			left, right = glyphSet(i.glyphProfile).Left, glyphSet(i.glyphProfile).Right
		}
		label := left + " " + selected.Spec.Title + " " + right
		if !selected.Enabled {
			label += " · unavailable"
		}
		return i.styles.FocusIndicator.Render(truncateDisplay(label, width))
	}

	parts := make([]string, 0, len(i.actions))
	selectedMarker := "›"
	disabledMarker := "×"
	if i.glyphProfile == GlyphASCII {
		selectedMarker = glyphSet(i.glyphProfile).Right
		disabledMarker = glyphSet(i.glyphProfile).Error
	}
	for index, action := range i.actions {
		label := action.Spec.Title
		if !action.Enabled {
			label += " " + disabledMarker
		}
		if index == i.selected {
			label = selectedMarker + " " + label
			if action.Spec.Destructive {
				parts = append(parts, i.styles.ErrorText.Render(label))
			} else {
				parts = append(parts, i.styles.FocusIndicator.Render(label))
			}
		} else {
			parts = append(parts, i.styles.OverlayDim.Render(label))
		}
	}
	rail := strings.Join(parts, i.styles.OverlayDim.Render("  "))
	if lipgloss.Width(rail) > width {
		selected := i.actions[i.selected]
		left, right := "‹", "›"
		if i.glyphProfile == GlyphASCII {
			left, right = glyphSet(i.glyphProfile).Left, glyphSet(i.glyphProfile).Right
		}
		return i.styles.FocusIndicator.Render(truncateDisplayWithGlyphProfile(
			left+" "+selected.Spec.Title+" "+right, width, i.glyphProfile,
		))
	}
	return rail
}

func (i *GoalInspector) renderActionDetail(width int) string {
	if len(i.actions) == 0 {
		return ""
	}
	selected := i.actions[i.selected]
	detail := selected.Spec.Description
	style := i.styles.OverlayDim
	switch {
	case i.confirming:
		detail = "Confirm drop · press enter again; completion will not be claimed."
		style = i.styles.ErrorText
	case !selected.Enabled:
		detail = "Unavailable · " + selected.DisabledReason
		style = i.styles.ErrorText
	}
	if i.height <= minTerminalHeight {
		return style.Render(truncateDisplay(detail, width))
	}
	lines := strings.Split(wrapText(detail, width), "\n")
	const maxLines = 2
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] = truncateDisplay(lines[maxLines-1], width)
	}
	return style.Render(strings.Join(lines, "\n"))
}

func (i *GoalInspector) renderFooter(width int) string {
	horizontal := "←/→"
	if i.glyphProfile == GlyphASCII {
		horizontal = "left/right"
	}
	hints := []keyHint{
		{Key: "esc", Action: "close"},
		{Key: horizontal, Action: "action"},
		{Key: "enter", Action: "select"},
		{Key: "j/k", Action: "scroll"},
	}
	if i.confirming {
		hints[0].Action = "cancel"
		hints[2].Action = "confirm"
	}
	for keep := len(hints); keep > 0; keep-- {
		parts := make([]string, 0, keep)
		for _, hint := range hints[:keep] {
			part := i.styles.FocusIndicator.Render(hint.Key)
			if hint.Action != "" {
				part += " " + i.styles.OverlayDim.Render(hint.Action)
			}
			parts = append(parts, part)
		}
		line := strings.Join(parts, i.styles.OverlayDim.Render(" · "))
		if lipgloss.Width(line) <= width {
			return line
		}
	}
	return truncateDisplay(i.styles.FocusIndicator.Render("esc")+" "+i.styles.OverlayDim.Render("close"), width)
}

func goalAcceptanceResults(snapshot goal.Snapshot) map[string]goal.AcceptanceResult {
	results := make(map[string]goal.AcceptanceResult)
	if snapshot.Completion == nil {
		return results
	}
	for _, result := range snapshot.Completion.Results {
		if result.Satisfied && strings.TrimSpace(result.Evidence) != "" {
			results[result.CriterionID] = result
		}
	}
	return results
}

func goalAcceptanceProgress(snapshot goal.Snapshot) (verified, total int) {
	results := goalAcceptanceResults(snapshot)
	for _, criterion := range snapshot.AcceptanceCriteria {
		if _, ok := results[criterion.ID]; ok {
			verified++
		}
	}
	return verified, len(snapshot.AcceptanceCriteria)
}

func goalRelativeTime(now, recorded time.Time) string {
	if recorded.IsZero() {
		return "time unknown"
	}
	elapsed := now.Sub(recorded)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed < time.Minute {
		return "just now"
	}
	return formatGoalDuration(elapsed) + " ago"
}

func goalInspectorBudget(snapshot goal.Snapshot, now time.Time) string {
	parts := make([]string, 0, 3)
	if snapshot.Budget.MaxContinuationTurns > 0 {
		parts = append(parts, fmt.Sprintf("turns %d/%d", snapshot.Usage.ContinuationTurns, snapshot.Budget.MaxContinuationTurns))
	}
	if snapshot.Budget.MaxEvalTokens > 0 {
		parts = append(parts, fmt.Sprintf("tokens %s/%s", formatGoalTokens(snapshot.Usage.EvalTokens), formatGoalTokens(snapshot.Budget.MaxEvalTokens)))
	}
	if snapshot.Budget.MaxWallTime > 0 && !snapshot.State.Terminal() {
		elapsed := now.Sub(snapshot.CreatedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		parts = append(parts, fmt.Sprintf("time %s/%s", formatGoalDuration(elapsed), formatGoalDuration(snapshot.Budget.MaxWallTime)))
	}
	if len(parts) == 0 {
		return "unlimited"
	}
	return strings.Join(parts, " · ")
}
