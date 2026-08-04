package ui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	goalRecoverySummaryHeight         = 3
	goalRecoveryMaximumItemIDBytes    = 128
	goalRecoveryMaximumSummaryBytes   = 4 * 1024
	goalRecoveryMaximumReferenceBytes = 1024
)

// GoalRecoveryItem is a sanitized, immutable presentation projection supplied
// by the smart parent. It deliberately contains no payload or raw tool
// arguments. GoalRecovery copies every item and never mutates one.
type GoalRecoveryItem struct {
	ItemID      string
	Kind        GoalRecoveryItemKind
	Subject     string
	Summary     string
	Tool        string
	ExecutionID string
	TurnID      string
	EventType   string
	EffectClass string
	Age         string
	Actionable  bool
	// DisabledReason is shown when the coordinator supplied an observational
	// parent/group item that cannot yet produce a reconciliation request.
	DisabledReason string
}

// GoalRecoveryItemKind keeps turn-level authority distinct from exact
// execution-effect evidence. The coordinator, not the UI, assigns this kind.
type GoalRecoveryItemKind string

const (
	GoalRecoveryExecutionEffect GoalRecoveryItemKind = "execution_effect"
	GoalRecoveryTurnBoundary    GoalRecoveryItemKind = "turn_boundary"
)

func (k GoalRecoveryItemKind) valid() bool {
	return k == GoalRecoveryExecutionEffect || k == GoalRecoveryTurnBoundary
}

// GoalRecoveryObservation records what the operator observed. Still unknown
// is a safe navigation choice, not evidence and never reaches an apply event.
type GoalRecoveryObservation string

const (
	GoalRecoveryEffectApplied                GoalRecoveryObservation = "effect_applied"
	GoalRecoveryEffectNotApplied             GoalRecoveryObservation = "effect_not_applied"
	GoalRecoveryEffectCompensated            GoalRecoveryObservation = "effect_compensated"
	GoalRecoveryTurnAbandonedAfterInspection GoalRecoveryObservation = "turn_abandoned_after_inspection"
	GoalRecoveryStillUnknown                 GoalRecoveryObservation = "still_unknown"
)

// GoalRecoverySource describes the evidence locator without carrying backend
// authority or claiming that the execution ledger completed.
type GoalRecoverySource string

const (
	GoalRecoveryExternalReceipt     GoalRecoverySource = "external_receipt"
	GoalRecoveryWorkspaceArtifact   GoalRecoverySource = "workspace_artifact"
	GoalRecoveryVerificationCheck   GoalRecoverySource = "verification_check"
	GoalRecoveryOperatorObservation GoalRecoverySource = "operator_observation"
)

// GoalRecoveryDraft is the bounded presentation payload emitted to the smart
// parent. The parent remains responsible for scope checks, evidence encoding,
// persistence, and every Goal Runtime transition.
type GoalRecoveryDraft struct {
	Observation GoalRecoveryObservation
	Source      GoalRecoverySource
	Summary     string
	Reference   string
}

// GoalRecoveryAction is a presentation intent, never a lifecycle mutation.
type GoalRecoveryAction string

const (
	GoalRecoveryActionNone  GoalRecoveryAction = ""
	GoalRecoveryActionClose GoalRecoveryAction = "close"
	GoalRecoveryActionApply GoalRecoveryAction = "apply"
)

// GoalRecoveryEvent asks the parent either to close the overlay or apply one
// validated draft to the exact immutable item. An empty Action is a local-only
// presentation update.
type GoalRecoveryEvent struct {
	Action GoalRecoveryAction
	ItemID string
	Draft  GoalRecoveryDraft
}

// GoalRecoveryStage identifies the currently visible step. Stages are exposed
// so the parent can describe focus without inspecting child internals.
type GoalRecoveryStage int

const (
	GoalRecoveryStageList GoalRecoveryStage = iota
	GoalRecoveryStageObservation
	GoalRecoveryStageSource
	GoalRecoveryStageSummary
	GoalRecoveryStageReference
	GoalRecoveryStageConfirmation
)

// GoalRecoveryOptions contains presentation-only host preferences.
type GoalRecoveryOptions struct {
	Width         int
	Height        int
	IsDark        bool
	ThemeID       string
	ReducedMotion bool
	GlyphProfile  GlyphProfile
	// Standalone adapts the already-designed evidence review for an ordinary
	// goal-less session. Persistence and authority remain parent-owned.
	Standalone bool
}

type goalRecoveryChoice[T ~string] struct {
	Value       T
	Label       string
	Description string
}

var goalRecoveryObservationChoices = []goalRecoveryChoice[GoalRecoveryObservation]{
	{Value: GoalRecoveryEffectApplied, Label: "Effect applied", Description: "The effect exists; a retry must not repeat it."},
	{Value: GoalRecoveryEffectNotApplied, Label: "Effect not applied", Description: "Your evidence shows the effect did not happen."},
	{Value: GoalRecoveryEffectCompensated, Label: "Effect compensated", Description: "The earlier effect was fully undone before retry."},
	{Value: GoalRecoveryStillUnknown, Label: "Still unknown", Description: "Keep the retry block; record no evidence."},
}

var goalRecoveryTurnObservationChoices = []goalRecoveryChoice[GoalRecoveryObservation]{
	{Value: GoalRecoveryTurnAbandonedAfterInspection, Label: "Abandon inspected turn", Description: "Close only the inspected lost provider turn."},
	{Value: GoalRecoveryStillUnknown, Label: "Still unknown", Description: "Keep the retry block; record no evidence."},
}

var goalRecoverySourceChoices = []goalRecoveryChoice[GoalRecoverySource]{
	{Value: GoalRecoveryExternalReceipt, Label: "External receipt", Description: "A request, job, or transaction identifier."},
	{Value: GoalRecoveryWorkspaceArtifact, Label: "Workspace artifact", Description: "A path plus a commit or digest."},
	{Value: GoalRecoveryVerificationCheck, Label: "Verification check", Description: "A command and its output receipt or digest."},
	{Value: GoalRecoveryOperatorObservation, Label: "Operator observation", Description: "Where and when you manually checked."},
}

type goalRecoveryListItem struct {
	item GoalRecoveryItem
}

func (i goalRecoveryListItem) Title() string {
	subject := goalRecoveryItemSubject(i.item)
	if !i.item.Actionable || !i.item.Kind.valid() {
		return "Unavailable · " + subject
	}
	return subject
}

func (i goalRecoveryListItem) Description() string {
	parts := []string{goalRecoveryEventLabel(i.item.EventType)}
	if executionID := strings.TrimSpace(i.item.ExecutionID); executionID != "" {
		parts = append(parts, "exec "+truncateDisplay(executionID, 18))
	}
	if turnID := strings.TrimSpace(i.item.TurnID); turnID != "" {
		parts = append(parts, "turn "+truncateDisplay(turnID, 18))
	}
	if age := strings.TrimSpace(i.item.Age); age != "" {
		parts = append(parts, age)
	}
	if !i.item.Actionable || !i.item.Kind.valid() {
		parts = append(parts, "unavailable")
	}
	return strings.Join(parts, " · ")
}

func (i goalRecoveryListItem) FilterValue() string {
	return strings.Join([]string{i.item.Tool, i.item.Summary, i.item.ExecutionID, i.item.TurnID}, " ")
}

type goalRecoveryRenderCache struct {
	valid   bool
	view    string
	cursor  *tea.Cursor
	renders int
}

// GoalRecovery is a Charm-native, persistence-free recovery wizard. The child
// owns focus and draft presentation only; it cannot inspect storage, mutate a
// goal, resume execution, or claim completion.
type GoalRecovery struct {
	items     []GoalRecoveryItem
	itemList  list.Model
	detail    viewport.Model
	summary   textarea.Model
	reference textinput.Model

	stage             GoalRecoveryStage
	draftItemID       string
	observationIndex  int
	sourceIndex       int
	confirmationIndex int
	errorText         string
	noticeText        string
	busyText          string

	width         int
	height        int
	isDark        bool
	themeID       string
	reducedMotion bool
	glyphProfile  GlyphProfile
	standalone    bool
	styles        Styles
	cache         goalRecoveryRenderCache
}

// NewGoalRecovery creates a focused recovery list. Zero dimensions default to
// an 80x24 canvas; SetSize keeps it responsive after construction.
func NewGoalRecovery(items []GoalRecoveryItem, options GoalRecoveryOptions) *GoalRecovery {
	if options.Width <= 0 {
		options.Width = 80
	}
	if options.Height <= 0 {
		options.Height = 24
	}

	profile := resolveGlyphProfile(options.GlyphProfile)
	delegate := newPickerDelegate(options.IsDark, false, resolveThemeID(options.ThemeID), profile)
	itemList := list.New(nil, delegate, 1, 1)
	configurePickerList(&itemList, options.IsDark, resolveThemeID(options.ThemeID))
	configurePickerListGlyphProfile(&itemList, profile)
	itemList.SetShowTitle(false)
	itemList.SetShowStatusBar(false)
	itemList.SetShowHelp(false)
	itemList.SetShowPagination(false)
	itemList.SetFilteringEnabled(false)
	itemList.DisableQuitKeybindings()

	summary := textarea.New()
	summary.Prompt = ""
	summary.Placeholder = "What did you inspect, and what did it show?"
	summary.ShowLineNumbers = false
	summary.CharLimit = goalRecoveryMaximumSummaryBytes
	summary.MaxHeight = goalRecoverySummaryHeight
	summary.SetHeight(goalRecoverySummaryHeight)

	reference := textinput.New()
	reference.Prompt = ""
	reference.Placeholder = "request ID, path + digest, or check receipt"
	reference.CharLimit = goalRecoveryMaximumReferenceBytes

	recovery := &GoalRecovery{
		itemList:         itemList,
		summary:          summary,
		reference:        reference,
		stage:            GoalRecoveryStageList,
		observationIndex: len(goalRecoveryObservationChoices) - 1,
		width:            max(minTerminalWidth, options.Width),
		height:           max(minTerminalHeight, options.Height),
		isDark:           options.IsDark,
		reducedMotion:    options.ReducedMotion,
		glyphProfile:     profile,
		standalone:       options.Standalone,
		styles:           NewStyles(options.IsDark, resolveThemeID(options.ThemeID)),
	}
	recovery.applyStyles()
	recovery.resizeComponents()
	_ = recovery.SetItems(items)
	recovery.focusStage(GoalRecoveryStageList)
	return recovery
}

// Stage reports the visible presentation step.
func (r *GoalRecovery) Stage() GoalRecoveryStage {
	if r == nil {
		return GoalRecoveryStageList
	}
	return r.stage
}

// Draft returns a copy of the current, unvalidated presentation draft.
func (r *GoalRecovery) Draft() GoalRecoveryDraft {
	if r == nil {
		return GoalRecoveryDraft{}
	}
	return GoalRecoveryDraft{
		Observation: r.selectedObservation(),
		Source:      r.selectedSource(),
		Summary:     strings.TrimSpace(r.summary.Value()),
		Reference:   strings.TrimSpace(r.reference.Value()),
	}
}

// ActionableCount reports how many immutable items the parent explicitly
// marked safe to submit to its reconciliation service.
func (r *GoalRecovery) ActionableCount() int {
	if r == nil {
		return 0
	}
	count := 0
	for _, item := range r.items {
		if item.Actionable && item.Kind.valid() {
			count++
		}
	}
	return count
}

// SetError attaches a bounded parent/coordinator failure to the active step.
// It never advances, closes, or clears the operator's draft.
func (r *GoalRecovery) SetError(message string) {
	if r == nil {
		return
	}
	message = strings.TrimSpace(strings.ToValidUTF8(message, "�"))
	if len(message) > goalRecoveryMaximumSummaryBytes {
		message = message[:goalRecoveryMaximumSummaryBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	r.errorText = message
	r.invalidate()
}

// SetBusy marks a coordinator operation as in flight. Escape navigation stays
// available, but the wizard emits no second Apply event until the parent clears
// the state with an empty message.
func (r *GoalRecovery) SetBusy(message string) {
	if r == nil {
		return
	}
	message = strings.TrimSpace(strings.ToValidUTF8(message, "�"))
	if len(message) > goalRecoveryMaximumReferenceBytes {
		message = boundGoalText(message, goalRecoveryMaximumReferenceBytes)
	}
	r.busyText = message
	if message != "" {
		r.errorText = ""
	}
	r.invalidate()
}

// SelectedItem returns a copy of the selected immutable item.
func (r *GoalRecovery) SelectedItem() (GoalRecoveryItem, bool) {
	if r == nil {
		return GoalRecoveryItem{}, false
	}
	entry, ok := r.itemList.SelectedItem().(goalRecoveryListItem)
	if !ok {
		return GoalRecoveryItem{}, false
	}
	return entry.item, true
}

// SetItems replaces the parent's immutable projection while preserving the
// selected item identity when possible. A draft whose item disappeared is
// discarded and returned to the list rather than being retargeted.
func (r *GoalRecovery) SetItems(items []GoalRecoveryItem) tea.Cmd {
	if r == nil {
		return nil
	}
	selected, _ := r.SelectedItem()
	copied := append([]GoalRecoveryItem(nil), items...)
	entries := make([]list.Item, len(copied))
	for index := range copied {
		entries[index] = goalRecoveryListItem{item: copied[index]}
	}
	r.items = copied
	command := r.itemList.SetItems(entries)

	selectedIndex := 0
	for index := range copied {
		if selected.ItemID != "" && copied[index].ItemID == selected.ItemID {
			selectedIndex = index
			break
		}
	}
	if len(copied) > 0 {
		r.itemList.Select(selectedIndex)
	}

	if r.draftItemID != "" && !goalRecoveryContainsItem(copied, r.draftItemID) {
		r.resetDraft()
		r.focusStage(GoalRecoveryStageList)
	}
	r.resizeComponents()
	r.refreshDetail(true)
	r.invalidate()
	return command
}

// ShowRecordedReceipt refreshes the immutable projection after a partial
// coordinator receipt, clears the now-consumed draft, and returns focus to the
// list. Persistence remains entirely parent-owned.
func (r *GoalRecovery) ShowRecordedReceipt(items []GoalRecoveryItem, notice string) tea.Cmd {
	if r == nil {
		return nil
	}
	itemsCommand := r.SetItems(items)
	r.resetDraft()
	focusCommand := r.focusStage(GoalRecoveryStageList)
	notice = strings.TrimSpace(strings.ToValidUTF8(notice, "�"))
	if len(notice) > goalRecoveryMaximumSummaryBytes {
		notice = boundGoalText(notice, goalRecoveryMaximumSummaryBytes)
	}
	r.noticeText = notice
	r.invalidate()
	return tea.Batch(itemsCommand, focusCommand)
}

func goalRecoveryContainsItem(items []GoalRecoveryItem, itemID string) bool {
	for index := range items {
		if items[index].ItemID == itemID {
			return true
		}
	}
	return false
}

// SetSize adapts every Bubbles child to the current terminal canvas.
func (r *GoalRecovery) SetSize(width, height int) {
	if r == nil {
		return
	}
	width = max(minTerminalWidth, width)
	height = max(minTerminalHeight, height)
	if r.width == width && r.height == height {
		return
	}
	r.width, r.height = width, height
	r.resizeComponents()
	r.refreshDetail(false)
	r.invalidate()
}

// SetTheme reapplies the existing LightDark-derived semantic palette.
func (r *GoalRecovery) SetTheme(isDark bool, themeID string) {
	if r == nil {
		return
	}
	themeID = resolveThemeID(themeID)
	if r.isDark == isDark && resolveThemeID(r.themeID) == themeID {
		return
	}
	r.isDark, r.themeID = isDark, themeID
	r.styles = NewStyles(isDark, r.themeID)
	r.applyStyles()
	r.resizeComponents()
	r.refreshDetail(false)
	r.invalidate()
}

// SetReducedMotion replaces blinking Bubbles cursors with static cursors.
func (r *GoalRecovery) SetReducedMotion(reduced bool) {
	if r == nil || r.reducedMotion == reduced {
		return
	}
	r.reducedMotion = reduced
	r.applyStyles()
	r.invalidate()
}

func (r *GoalRecovery) contentWidth() int {
	return pickerListWidth(r.width)
}

func (r *GoalRecovery) compact() bool {
	return r.width <= 48 || r.height < 24
}

func (r *GoalRecovery) errorStyle() lipgloss.Style {
	return r.styles.ErrorText.PaddingLeft(0)
}

func (r *GoalRecovery) applyStyles() {
	inputStyles := semanticTextInputStyles(r.isDark, r.themeID)
	inputStyles.Cursor.Blink = !r.reducedMotion
	r.reference.SetStyles(inputStyles)
	r.summary.SetStyles(goalTextareaStyles(r.isDark, r.themeID, r.reducedMotion))
	configurePickerList(&r.itemList, r.isDark, r.themeID)
	configurePickerListGlyphProfile(&r.itemList, r.glyphProfile)
	palette := outputSemanticPalette(r.isDark, r.themeID)
	r.detail.Style = lipgloss.NewStyle().Foreground(palette.Muted)
}

func (r *GoalRecovery) resizeComponents() {
	width := r.contentWidth()
	bubbleWidth := max(1, width-2)
	delegate := newPickerDelegate(r.isDark, r.compact(), r.themeID, r.glyphProfile)
	r.itemList.SetDelegate(delegate)
	listHeight := 1
	if !r.compact() {
		listHeight = max(2, min(6, len(r.items)*delegate.Height()))
	}
	r.itemList.SetSize(bubbleWidth, listHeight)
	r.summary.SetWidth(max(1, width-4))
	r.summary.SetHeight(goalRecoverySummaryHeight)
	r.reference.SetWidth(max(1, width-2))

	detailHeight := 2
	if r.stage == GoalRecoveryStageConfirmation {
		detailHeight = 4
	}
	if !r.compact() {
		detailHeight = 6
	}
	if r.detail.Width() == 0 {
		r.detail = viewport.New(viewport.WithWidth(bubbleWidth), viewport.WithHeight(detailHeight))
		r.detail.SoftWrap = false
		r.detail.FillHeight = false
	} else {
		r.detail.SetWidth(bubbleWidth)
		r.detail.SetHeight(detailHeight)
	}
	r.detail.Style = lipgloss.NewStyle().Foreground(outputSemanticPalette(r.isDark, r.themeID).Muted)
}

func (r *GoalRecovery) selectedObservation() GoalRecoveryObservation {
	choices := r.observationChoices()
	index := min(max(0, r.observationIndex), len(choices)-1)
	return choices[index].Value
}

func (r *GoalRecovery) observationChoices() []goalRecoveryChoice[GoalRecoveryObservation] {
	item, ok := r.SelectedItem()
	if ok && item.Kind == GoalRecoveryTurnBoundary {
		return goalRecoveryTurnObservationChoices
	}
	return goalRecoveryObservationChoices
}

func (r *GoalRecovery) selectedSource() GoalRecoverySource {
	index := min(max(0, r.sourceIndex), len(goalRecoverySourceChoices)-1)
	return goalRecoverySourceChoices[index].Value
}

func (r *GoalRecovery) beginSelectedItem() tea.Cmd {
	item, ok := r.SelectedItem()
	if !ok {
		return nil
	}
	if !item.Actionable || !item.Kind.valid() {
		r.noticeText = "Unavailable · " + goalRecoveryFallback(item.DisabledReason, "Recovery evidence cannot be applied yet.")
		r.invalidate()
		return nil
	}
	if r.draftItemID != item.ItemID {
		r.resetDraft()
		r.draftItemID = item.ItemID
	}
	r.noticeText = ""
	return r.focusStage(GoalRecoveryStageObservation)
}

func (r *GoalRecovery) resetDraft() {
	r.draftItemID = ""
	r.observationIndex = len(r.observationChoices()) - 1
	r.sourceIndex = 0
	r.confirmationIndex = 0
	r.summary.SetValue("")
	r.reference.SetValue("")
	r.errorText = ""
}

func (r *GoalRecovery) focusStage(stage GoalRecoveryStage) tea.Cmd {
	stage = min(max(GoalRecoveryStageList, stage), GoalRecoveryStageConfirmation)
	r.summary.Blur()
	r.reference.Blur()
	r.stage = stage
	r.errorText = ""
	if stage == GoalRecoveryStageConfirmation {
		r.confirmationIndex = 0
	}
	r.resizeComponents()
	r.refreshDetail(true)
	r.invalidate()

	switch stage {
	case GoalRecoveryStageSummary:
		return r.summary.Focus()
	case GoalRecoveryStageReference:
		return r.reference.Focus()
	default:
		return nil
	}
}

func (r *GoalRecovery) previousStage() tea.Cmd {
	if r.stage <= GoalRecoveryStageList {
		return nil
	}
	return r.focusStage(r.stage - 1)
}

func (r *GoalRecovery) refreshDetail(reset bool) {
	if r == nil {
		return
	}
	offset := r.detail.YOffset()
	content := ""
	switch r.stage {
	case GoalRecoveryStageList:
		content = r.listDetail()
	case GoalRecoveryStageConfirmation:
		content = r.confirmationDetail()
	}
	r.detail.SetContent(content)
	if reset {
		r.detail.GotoTop()
	} else {
		r.detail.SetYOffset(offset)
	}
}

func (r *GoalRecovery) listDetail() string {
	item, ok := r.SelectedItem()
	if !ok {
		return ""
	}
	width := r.contentWidth()
	if r.compact() {
		identity := "provider turn · receipt lost"
		if executionID := strings.TrimSpace(item.ExecutionID); executionID != "" {
			identity = "exec " + executionID
			if turn := strings.TrimSpace(item.TurnID); turn != "" {
				identity += " · turn " + turn
			}
		} else if turn := strings.TrimSpace(item.TurnID); turn != "" {
			return r.styles.OverlayDim.Render("Provider turn") + "\n" +
				r.styles.OverlayDim.Render(truncateDisplay("receipt lost · "+turn, r.detail.Width()))
		}
		return r.styles.OverlayDim.Render(truncateDisplay(identity, width))
	}
	lines := []string{
		goalRecoveryDetailLine("Summary", goalRecoveryFallback(item.Summary, "Evidence is required before retry.")),
		goalRecoveryDetailLine("Subject", goalRecoveryItemSubject(item)),
		goalRecoveryDetailLine("Ledger", goalRecoveryEventLabel(item.EventType)+" · "+goalRecoveryFallback(item.EffectClass, "effect unknown")),
		goalRecoveryDetailLine("Turn", goalRecoveryFallback(item.TurnID, "unknown")),
	}
	if strings.TrimSpace(item.ExecutionID) != "" {
		lines = append(lines, goalRecoveryDetailLine("Execution", item.ExecutionID))
	} else {
		lines = append(lines, "Execution · none · provider turn receipt lost")
	}
	if !item.Actionable || !item.Kind.valid() {
		lines = append(lines, "Apply unavailable · "+goalRecoveryFallback(item.DisabledReason, "coordinator has not admitted this item"))
	}
	return r.styles.OverlayDim.Render(goalRecoveryWrapLines(lines, width))
}

func (r *GoalRecovery) confirmationDetail() string {
	draft := r.Draft()
	item, _ := r.SelectedItem()
	eventType := goalRecoveryFallback(item.EventType, "outcome_unknown")
	if item.Kind == GoalRecoveryTurnBoundary {
		if r.compact() {
			lines := []string{
				"Not proof.",
				"Provider receipt stays lost.",
				"AUTO will not resume.",
				goalRecoveryObservationLabel(draft.Observation) + " · " + goalRecoverySourceLabel(draft.Source),
				"Summary · " + draft.Summary,
				"Reference · " + draft.Reference,
			}
			return r.styles.OverlayDim.Render(goalRecoveryWrapLines(lines, r.detail.Width()))
		}
		lines := []string{
			"Abandons the inspected provider turn only; this is not proof of backend or goal completion.",
			"No execution outcome is invented; AUTO will not resume.",
			goalRecoveryObservationLabel(draft.Observation) + " · " + goalRecoverySourceLabel(draft.Source),
			"Summary · " + draft.Summary,
			"Reference · " + draft.Reference,
		}
		return r.styles.OverlayDim.Render(goalRecoveryWrapLines(lines, r.detail.Width()))
	}
	if r.standalone {
		if r.compact() {
			lines := []string{
				"Execution observation only.",
				"Ledger stays " + eventType + ".",
				"No tool is retried.",
				goalRecoveryObservationLabel(draft.Observation) + " · " + goalRecoverySourceLabel(draft.Source),
				"Summary · " + draft.Summary,
				"Reference · " + draft.Reference,
			}
			return r.styles.OverlayDim.Render(goalRecoveryWrapLines(lines, r.detail.Width()))
		}
		lines := []string{
			"Records typed evidence for this execution only; it does not claim backend or task completion.",
			"The immutable ledger stays " + eventType + "; no tool is retried.",
			"After the receipt commits, the next prompt rechecks durable state before provider work.",
			goalRecoveryObservationLabel(draft.Observation) + " · " + goalRecoverySourceLabel(draft.Source),
			"Summary · " + draft.Summary,
			"Reference · " + draft.Reference,
		}
		return r.styles.OverlayDim.Render(goalRecoveryWrapLines(lines, r.detail.Width()))
	}
	if r.compact() {
		lines := []string{
			"Not proof.",
			"Ledger " + eventType,
			"Goal stays blocked.",
			"AUTO will not resume.",
			goalRecoveryObservationLabel(draft.Observation) + " · " + goalRecoverySourceLabel(draft.Source),
			"Summary · " + draft.Summary,
			"Reference · " + draft.Reference,
		}
		return r.styles.OverlayDim.Render(goalRecoveryWrapLines(lines, r.detail.Width()))
	}
	lines := []string{
		"Records this execution observation only; the goal remains blocked until the provider turn is fully reconciled.",
		"This is not proof of backend or goal completion.",
		"Ledger stays " + eventType + "; AUTO will not resume.",
		goalRecoveryObservationLabel(draft.Observation) + " · " + goalRecoverySourceLabel(draft.Source),
		"Summary · " + draft.Summary,
		"Reference · " + draft.Reference,
	}
	return r.styles.OverlayDim.Render(goalRecoveryWrapLines(lines, r.detail.Width()))
}

func goalRecoveryWrapLines(lines []string, width int) string {
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		wrapped = append(wrapped, wrapText(line, width))
	}
	return strings.Join(wrapped, "\n")
}

func goalRecoveryDetailLine(label, value string) string {
	return label + " · " + strings.TrimSpace(value)
}

func goalRecoveryFallback(value, fallback string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "�"))
	if value == "" {
		return fallback
	}
	return value
}

func goalRecoveryItemSubject(item GoalRecoveryItem) string {
	if subject := strings.TrimSpace(item.Subject); subject != "" {
		return subject
	}
	if tool := strings.TrimSpace(item.Tool); tool != "" {
		return tool
	}
	return "Provider turn"
}

func goalRecoveryEventLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "outcome_unknown" {
		return "outcome unknown"
	}
	return strings.ReplaceAll(value, "_", " ")
}

func goalRecoveryObservationLabel(value GoalRecoveryObservation) string {
	for _, choices := range [][]goalRecoveryChoice[GoalRecoveryObservation]{goalRecoveryObservationChoices, goalRecoveryTurnObservationChoices} {
		for _, choice := range choices {
			if choice.Value == value {
				return choice.Label
			}
		}
	}
	return "Observation unavailable"
}

func goalRecoverySourceLabel(value GoalRecoverySource) string {
	for _, choice := range goalRecoverySourceChoices {
		if choice.Value == value {
			return choice.Label
		}
	}
	return "Source unavailable"
}

// Update handles presentation messages routed by the smart parent and emits
// only Close or Apply intent. It never performs asynchronous work itself.
func (r *GoalRecovery) Update(msg tea.Msg) (GoalRecoveryEvent, tea.Cmd) {
	if r == nil {
		return GoalRecoveryEvent{}, nil
	}
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		r.SetSize(size.Width, size.Height)
		return GoalRecoveryEvent{}, nil
	}
	if wheel, ok := msg.(tea.MouseWheelMsg); ok {
		return r.updateMouse(wheel)
	}

	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return r.updateFocusedBubble(msg)
	}
	if keyMsg.Code == tea.KeyEscape {
		if r.stage == GoalRecoveryStageList {
			return GoalRecoveryEvent{Action: GoalRecoveryActionClose}, nil
		}
		return GoalRecoveryEvent{}, r.previousStage()
	}
	if r.busyText != "" {
		return GoalRecoveryEvent{}, nil
	}
	if keyMsg.Code == tea.KeyTab && keyMsg.Mod == tea.ModShift {
		return GoalRecoveryEvent{}, r.previousStage()
	}

	switch r.stage {
	case GoalRecoveryStageList:
		return r.updateList(keyMsg)
	case GoalRecoveryStageObservation:
		return r.updateObservation(keyMsg)
	case GoalRecoveryStageSource:
		return r.updateSource(keyMsg)
	case GoalRecoveryStageSummary:
		return r.updateSummary(keyMsg)
	case GoalRecoveryStageReference:
		return r.updateReference(keyMsg)
	case GoalRecoveryStageConfirmation:
		return r.updateConfirmation(keyMsg)
	default:
		return GoalRecoveryEvent{}, nil
	}
}

func (r *GoalRecovery) updateMouse(msg tea.MouseWheelMsg) (GoalRecoveryEvent, tea.Cmd) {
	var command tea.Cmd
	switch r.stage {
	case GoalRecoveryStageConfirmation:
		r.detail, command = r.detail.Update(msg)
	case GoalRecoveryStageList:
		before := r.itemList.Index()
		r.itemList, command = r.itemList.Update(msg)
		if before != r.itemList.Index() {
			r.refreshDetail(true)
		}
	}
	r.invalidate()
	return GoalRecoveryEvent{}, command
}

func (r *GoalRecovery) updateFocusedBubble(msg tea.Msg) (GoalRecoveryEvent, tea.Cmd) {
	var command tea.Cmd
	switch r.stage {
	case GoalRecoveryStageSummary:
		r.summary, command = r.summary.Update(msg)
	case GoalRecoveryStageReference:
		r.reference, command = r.reference.Update(msg)
	default:
		return GoalRecoveryEvent{}, nil
	}
	r.errorText = ""
	r.invalidate()
	return GoalRecoveryEvent{}, command
}

func (r *GoalRecovery) updateList(msg tea.KeyPressMsg) (GoalRecoveryEvent, tea.Cmd) {
	if msg.Code == tea.KeyEnter || msg.Code == tea.KeySpace {
		return GoalRecoveryEvent{}, r.beginSelectedItem()
	}
	if msg.Mod == 0 && (msg.Text == "u" || msg.Text == "d") || msg.String() == "pgup" || msg.String() == "pgdown" {
		if navigateReadOnlyViewport(&r.detail, msg.String()) {
			r.invalidate()
			return GoalRecoveryEvent{}, nil
		}
	}
	before := r.itemList.Index()
	updated, command := r.itemList.Update(msg)
	r.itemList = updated
	if before != r.itemList.Index() {
		r.refreshDetail(true)
	}
	r.invalidate()
	return GoalRecoveryEvent{}, command
}

func (r *GoalRecovery) updateObservation(msg tea.KeyPressMsg) (GoalRecoveryEvent, tea.Cmd) {
	choices := r.observationChoices()
	if isGoalRecoveryChoicePrevious(msg) {
		r.observationIndex = max(0, r.observationIndex-1)
		r.errorText = ""
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	if isGoalRecoveryChoiceNext(msg) {
		r.observationIndex = min(len(choices)-1, r.observationIndex+1)
		r.errorText = ""
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	if msg.Code != tea.KeyEnter && msg.Code != tea.KeySpace && msg.Code != tea.KeyTab {
		return GoalRecoveryEvent{}, nil
	}
	if r.selectedObservation() == GoalRecoveryStillUnknown {
		r.resetDraft()
		r.noticeText = "No evidence recorded · goal remains blocked."
		return GoalRecoveryEvent{}, r.focusStage(GoalRecoveryStageList)
	}
	return GoalRecoveryEvent{}, r.focusStage(GoalRecoveryStageSource)
}

func (r *GoalRecovery) updateSource(msg tea.KeyPressMsg) (GoalRecoveryEvent, tea.Cmd) {
	if isGoalRecoveryChoicePrevious(msg) {
		r.sourceIndex = max(0, r.sourceIndex-1)
		r.errorText = ""
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	if isGoalRecoveryChoiceNext(msg) {
		r.sourceIndex = min(len(goalRecoverySourceChoices)-1, r.sourceIndex+1)
		r.errorText = ""
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	if msg.Code == tea.KeyEnter || msg.Code == tea.KeySpace || msg.Code == tea.KeyTab {
		return GoalRecoveryEvent{}, r.focusStage(GoalRecoveryStageSummary)
	}
	return GoalRecoveryEvent{}, nil
}

func (r *GoalRecovery) updateSummary(msg tea.KeyPressMsg) (GoalRecoveryEvent, tea.Cmd) {
	if msg.Code == tea.KeyTab {
		if err := validateGoalRecoveryText("evidence summary", r.summary.Value(), goalRecoveryMaximumSummaryBytes); err != nil {
			r.errorText = err.Error()
			r.invalidate()
			return GoalRecoveryEvent{}, nil
		}
		return GoalRecoveryEvent{}, r.focusStage(GoalRecoveryStageReference)
	}
	r.errorText = ""
	var command tea.Cmd
	r.summary, command = r.summary.Update(msg)
	r.invalidate()
	return GoalRecoveryEvent{}, command
}

func (r *GoalRecovery) updateReference(msg tea.KeyPressMsg) (GoalRecoveryEvent, tea.Cmd) {
	if msg.Code == tea.KeyEnter || msg.Code == tea.KeyTab {
		if _, err := r.validatedDraft(); err != nil {
			r.errorText = err.Error()
			r.invalidate()
			return GoalRecoveryEvent{}, nil
		}
		return GoalRecoveryEvent{}, r.focusStage(GoalRecoveryStageConfirmation)
	}
	r.errorText = ""
	var command tea.Cmd
	r.reference, command = r.reference.Update(msg)
	r.invalidate()
	return GoalRecoveryEvent{}, command
}

func (r *GoalRecovery) updateConfirmation(msg tea.KeyPressMsg) (GoalRecoveryEvent, tea.Cmd) {
	if isGoalRecoveryHorizontalPrevious(msg) {
		r.confirmationIndex = 0
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	if isGoalRecoveryHorizontalNext(msg) {
		r.confirmationIndex = 1
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	if navigateReadOnlyViewport(&r.detail, msg.String()) {
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	if msg.Code != tea.KeyEnter && msg.Code != tea.KeySpace {
		return GoalRecoveryEvent{}, nil
	}
	if r.confirmationIndex == 0 {
		return GoalRecoveryEvent{}, r.focusStage(GoalRecoveryStageReference)
	}
	draft, err := r.validatedDraft()
	if err != nil {
		r.errorText = err.Error()
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	item, ok := r.SelectedItem()
	if !ok || item.ItemID != r.draftItemID {
		r.errorText = "recovery item is no longer selected"
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	if !item.Actionable || !item.Kind.valid() {
		r.errorText = goalRecoveryFallback(item.DisabledReason, "recovery item is not actionable")
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	if err := validateGoalRecoveryItemID(item.ItemID); err != nil {
		r.errorText = err.Error()
		r.invalidate()
		return GoalRecoveryEvent{}, nil
	}
	return GoalRecoveryEvent{Action: GoalRecoveryActionApply, ItemID: item.ItemID, Draft: draft}, nil
}

func (r *GoalRecovery) validatedDraft() (GoalRecoveryDraft, error) {
	draft := r.Draft()
	item, ok := r.SelectedItem()
	if !ok || !item.Kind.valid() {
		return GoalRecoveryDraft{}, fmt.Errorf("recovery item kind is unavailable")
	}
	if draft.Observation == GoalRecoveryStillUnknown || !goalRecoveryObservationValidForItem(draft.Observation, item.Kind) {
		return GoalRecoveryDraft{}, fmt.Errorf("choose an observed outcome or keep the goal blocked")
	}
	if !draft.Source.valid() {
		return GoalRecoveryDraft{}, fmt.Errorf("choose an evidence source")
	}
	if err := validateGoalRecoveryText("evidence summary", draft.Summary, goalRecoveryMaximumSummaryBytes); err != nil {
		return GoalRecoveryDraft{}, err
	}
	if err := validateGoalRecoveryText("evidence reference", draft.Reference, goalRecoveryMaximumReferenceBytes); err != nil {
		return GoalRecoveryDraft{}, err
	}
	return draft, nil
}

func goalRecoveryObservationValidForItem(observation GoalRecoveryObservation, kind GoalRecoveryItemKind) bool {
	if kind == GoalRecoveryTurnBoundary {
		return observation == GoalRecoveryTurnAbandonedAfterInspection
	}
	if kind != GoalRecoveryExecutionEffect {
		return false
	}
	switch observation {
	case GoalRecoveryEffectApplied, GoalRecoveryEffectNotApplied, GoalRecoveryEffectCompensated:
		return true
	default:
		return false
	}
}

func (s GoalRecoverySource) valid() bool {
	switch s {
	case GoalRecoveryExternalReceipt, GoalRecoveryWorkspaceArtifact, GoalRecoveryVerificationCheck, GoalRecoveryOperatorObservation:
		return true
	default:
		return false
	}
}

func validateGoalRecoveryText(name, value string, maximum int) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", name)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", name, maximum)
	}
	return nil
}

func validateGoalRecoveryItemID(value string) error {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return fmt.Errorf("recovery item identity is invalid")
	}
	if len(value) > goalRecoveryMaximumItemIDBytes {
		return fmt.Errorf("recovery item identity exceeds %d bytes", goalRecoveryMaximumItemIDBytes)
	}
	return nil
}

func isGoalRecoveryChoicePrevious(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeyLeft || msg.Code == tea.KeyUp ||
		(msg.Mod == 0 && (msg.Text == "h" || msg.Text == "k"))
}

func isGoalRecoveryChoiceNext(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeyRight || msg.Code == tea.KeyDown ||
		(msg.Mod == 0 && (msg.Text == "l" || msg.Text == "j"))
}

func isGoalRecoveryHorizontalPrevious(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeyLeft || (msg.Mod == 0 && msg.Text == "h")
}

func isGoalRecoveryHorizontalNext(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeyRight || (msg.Mod == 0 && msg.Text == "l")
}

func (r *GoalRecovery) invalidate() {
	if r != nil {
		r.cache.valid = false
	}
}

// View renders the cached modal without a hardware cursor.
func (r *GoalRecovery) View() string {
	view, _ := r.ViewWithCursor()
	return view
}

// ViewWithCursor renders the modal and a frame-local Bubble Tea cursor.
func (r *GoalRecovery) ViewWithCursor() (string, *tea.Cursor) {
	if r == nil {
		return "", nil
	}
	if r.cache.valid {
		return r.cache.view, cloneGoalRecoveryCursor(r.cache.cursor)
	}

	var view string
	var cursor *tea.Cursor
	switch r.stage {
	case GoalRecoveryStageList:
		view = r.renderList()
	case GoalRecoveryStageObservation:
		view = r.renderChoiceStep("Observation", r.renderObservationChoice())
	case GoalRecoveryStageSource:
		view = r.renderChoiceStep("Evidence source", r.renderSourceChoice())
	case GoalRecoveryStageSummary:
		control, controlCursor := r.renderSummaryInput()
		view, cursor = r.renderInputStep("Evidence summary", control, controlCursor)
	case GoalRecoveryStageReference:
		control, controlCursor := r.renderReferenceInput()
		view, cursor = r.renderInputStep("Evidence reference", control, controlCursor)
	case GoalRecoveryStageConfirmation:
		view = r.renderConfirmation()
	}
	r.cache.valid = true
	r.cache.view = view
	r.cache.cursor = cloneGoalRecoveryCursor(cursor)
	r.cache.renders++
	return view, cloneGoalRecoveryCursor(cursor)
}

func cloneGoalRecoveryCursor(cursor *tea.Cursor) *tea.Cursor {
	if cursor == nil {
		return nil
	}
	copy := *cursor
	return &copy
}

func (r *GoalRecovery) renderList() string {
	width := r.contentWidth()
	count := len(r.items)
	noun := "checks"
	if count == 1 {
		noun = "check"
	}
	var b strings.Builder
	title := fmt.Sprintf("Recovery · %d %s", count, noun)
	if r.standalone {
		title = fmt.Sprintf("Execution recovery · %d %s", count, noun)
	}
	if r.compact() {
		title = fmt.Sprintf("Recovery · %d", count)
		if r.standalone {
			title = fmt.Sprintf("Execution recovery · %d", count)
		}
	}
	b.WriteString(r.styles.OverlayTitle.Render(truncateDisplay(title, width)))
	b.WriteByte('\n')
	banner := "The prior turn may have produced effects. Recovery records observations; it is not proof of backend or goal completion."
	if r.standalone {
		banner = "The prior execution may have produced an effect. Review it before recording typed evidence; no tool is retried."
	}
	if r.compact() {
		banner = "Turn effects unknown.\nNot completion proof."
		if r.standalone {
			banner = "Execution effect unknown.\nNo tool retry."
		}
	}
	b.WriteString(r.errorStyle().Render(wrapText(banner, width)))
	b.WriteByte('\n')
	if count == 0 {
		b.WriteString(r.styles.OverlayDim.Render("No recovery items."))
	} else {
		b.WriteString(strings.TrimRight(r.itemList.View(), "\n"))
		b.WriteByte('\n')
		b.WriteString(r.detail.View())
	}
	if message := strings.TrimSpace(r.errorText); message != "" {
		b.WriteByte('\n')
		b.WriteString(r.errorStyle().Render(truncateDisplay("! "+message, width)))
	}
	if notice := strings.TrimSpace(r.noticeText); notice != "" {
		b.WriteByte('\n')
		b.WriteString(r.styles.OverlayAccent.Render(truncateDisplay(notice, width)))
	}
	if busy := strings.TrimSpace(r.busyText); busy != "" {
		b.WriteByte('\n')
		b.WriteString(r.styles.FocusIndicator.Render(truncateDisplay(busy, width)))
	}
	footer := r.renderFooter(width)
	return r.renderFrame(strings.TrimRight(b.String(), "\n"), footer)
}

func (r *GoalRecovery) renderChoiceStep(label, control string) string {
	width := r.contentWidth()
	var b strings.Builder
	b.WriteString(r.styles.OverlayTitle.Render(fmt.Sprintf("Recovery · %d/5", r.stageStep())))
	b.WriteByte('\n')
	b.WriteString(r.renderItemContext(width))
	b.WriteByte('\n')
	b.WriteString(r.styles.FocusIndicator.Render(label))
	b.WriteByte('\n')
	b.WriteString(control)
	if r.errorText != "" {
		b.WriteByte('\n')
		b.WriteString(r.errorStyle().Render(truncateDisplay("! "+r.errorText, width)))
	}
	return r.renderFrame(b.String(), r.renderFooter(width))
}

func (r *GoalRecovery) renderInputStep(label string, control string, cursor *tea.Cursor) (string, *tea.Cursor) {
	width := r.contentWidth()
	var b strings.Builder
	b.WriteString(r.styles.OverlayTitle.Render(fmt.Sprintf("Recovery · %d/5", r.stageStep())))
	b.WriteByte('\n')
	b.WriteString(r.renderItemContext(width))
	b.WriteByte('\n')
	b.WriteString(r.styles.FocusIndicator.Render(label))
	b.WriteByte('\n')
	controlY := strings.Count(b.String(), "\n")
	b.WriteString(control)
	cursor = offsetCursor(cursor, 0, controlY)
	if r.errorText != "" {
		b.WriteByte('\n')
		b.WriteString(r.errorStyle().Render(truncateDisplay("! "+r.errorText, width)))
	}
	return r.renderFrame(b.String(), r.renderFooter(width)), pickerFrameCursor(cursor)
}

func (r *GoalRecovery) renderObservationChoice() string {
	return renderGoalRecoveryChoices(r, r.observationChoices(), r.observationIndex)
}

func (r *GoalRecovery) renderSourceChoice() string {
	return renderGoalRecoveryChoices(r, goalRecoverySourceChoices, r.sourceIndex)
}

func renderGoalRecoveryChoices[T ~string](r *GoalRecovery, choices []goalRecoveryChoice[T], selected int) string {
	width := r.contentWidth()
	selected = min(max(0, selected), len(choices)-1)
	glyphs := glyphSet(r.glyphProfile)
	if r.compact() {
		choice := choices[selected]
		left, right := "", ""
		if selected > 0 {
			left = glyphs.Left + " "
		}
		if selected < len(choices)-1 {
			right = " " + glyphs.Right
		}
		return r.styles.FocusIndicator.Render(truncateDisplayWithGlyphProfile(
			left+glyphs.Collapsed+" "+choice.Label+right, width, r.glyphProfile,
		)) + "\n" +
			r.styles.OverlayDim.Render(truncateDisplay(choice.Description, width))
	}

	lines := make([]string, 0, len(choices)+1)
	for index, choice := range choices {
		marker := "  "
		style := r.styles.OverlayDim
		if index == selected {
			marker = glyphs.Collapsed + " "
			style = r.styles.FocusIndicator
		}
		lines = append(lines, style.Render(truncateDisplay(marker+choice.Label, width)))
	}
	lines = append(lines, r.styles.OverlayDim.Render(truncateDisplay(choices[selected].Description, width)))
	return strings.Join(lines, "\n")
}

func (r *GoalRecovery) renderSummaryInput() (string, *tea.Cursor) {
	view := r.summary
	view.SetWidth(max(1, r.contentWidth()-4))
	view.SetHeight(goalRecoverySummaryHeight)
	view.SetVirtualCursor(false)
	lines := strings.Split(view.View(), "\n")
	for index := range lines {
		prefix := "  "
		if index == 0 {
			prefix = "> "
		}
		lines[index] = r.styles.FocusIndicator.Render(prefix) + lines[index]
	}
	return strings.Join(lines, "\n"), offsetCursor(view.Cursor(), 2, 0)
}

func (r *GoalRecovery) renderReferenceInput() (string, *tea.Cursor) {
	view := r.reference
	view.SetWidth(max(1, r.contentWidth()-2))
	view.SetVirtualCursor(false)
	control := r.styles.FocusIndicator.Render("> ") + view.View()
	helper := r.styles.OverlayDim.Render(truncateDisplay("Use an ID or digest; never paste secrets.", r.contentWidth()))
	return control + "\n" + helper, offsetCursor(view.Cursor(), 2, 0)
}

func (r *GoalRecovery) renderConfirmation() string {
	width := r.contentWidth()
	var b strings.Builder
	b.WriteString(r.styles.OverlayTitle.Render("Confirm · 5/5"))
	b.WriteByte('\n')
	b.WriteString(r.detail.View())
	b.WriteByte('\n')
	b.WriteString(r.renderConfirmationActions(width))
	if r.errorText != "" {
		b.WriteByte('\n')
		b.WriteString(r.errorStyle().Render(truncateDisplay("! "+r.errorText, width)))
	}
	return r.renderFrame(b.String(), r.renderFooter(width))
}

func (r *GoalRecovery) renderConfirmationActions(width int) string {
	if r.busyText != "" {
		return r.styles.FocusIndicator.Render(truncateDisplay(r.busyText, width)) + "\n" +
			r.styles.OverlayDim.Render(truncateDisplay("Waiting for the durable coordinator receipt", width))
	}
	labels := []string{"Back", "Record evidence"}
	parts := make([]string, 0, len(labels))
	glyphs := glyphSet(r.glyphProfile)
	for index, label := range labels {
		marker := "  "
		style := r.styles.OverlayDim
		if index == r.confirmationIndex {
			marker = glyphs.Collapsed + " "
			style = r.styles.FocusIndicator
			if index == 1 {
				style = r.errorStyle()
			}
		}
		parts = append(parts, style.Render(marker+label))
	}
	line := truncateDisplay(strings.Join(parts, r.styles.OverlayDim.Render(" · ")), width)
	detail := "Review the reference"
	if r.confirmationIndex == 1 {
		detail = "Append an immutable receipt"
	}
	return line + "\n" + r.styles.OverlayDim.Render(truncateDisplay(detail, width))
}

func (r *GoalRecovery) renderItemContext(width int) string {
	item, ok := r.SelectedItem()
	if !ok {
		return r.errorStyle().Render("Recovery item unavailable")
	}
	return r.styles.OverlayDim.Render(truncateDisplay(goalRecoveryItemSubject(item)+" · "+goalRecoveryEventLabel(item.EventType), width))
}

func (r *GoalRecovery) stageStep() int {
	switch r.stage {
	case GoalRecoveryStageObservation:
		return 1
	case GoalRecoveryStageSource:
		return 2
	case GoalRecoveryStageSummary:
		return 3
	case GoalRecoveryStageReference:
		return 4
	case GoalRecoveryStageConfirmation:
		return 5
	default:
		return 0
	}
}

type goalRecoveryHint struct {
	key    string
	action string
}

func (r *GoalRecovery) renderFooter(width int) string {
	horizontalKeys := "←/→"
	if r.glyphProfile == GlyphASCII {
		horizontalKeys = "left/right"
	}
	if r.busyText != "" {
		action := "back"
		if r.stage == GoalRecoveryStageList {
			action = "close"
		}
		return r.renderHints(width, goalRecoveryHint{key: "esc", action: action})
	}
	if r.compact() {
		switch r.stage {
		case GoalRecoveryStageList:
			if len(r.items) == 0 {
				return r.renderHints(width, goalRecoveryHint{key: "esc", action: "close"})
			}
			return r.renderHints(width,
				goalRecoveryHint{key: "esc", action: "close"},
				goalRecoveryHint{key: "enter", action: "review"},
			)
		case GoalRecoveryStageSummary:
			return r.renderHints(width,
				goalRecoveryHint{key: "esc", action: "back"},
				goalRecoveryHint{key: "tab", action: "next"},
				goalRecoveryHint{key: "enter", action: "newline"},
			)
		case GoalRecoveryStageReference:
			return r.renderHints(width,
				goalRecoveryHint{key: "esc", action: "back"},
				goalRecoveryHint{key: "enter/tab", action: "next"},
			)
		case GoalRecoveryStageConfirmation:
			return r.renderHints(width,
				goalRecoveryHint{key: "esc", action: "back"},
				goalRecoveryHint{key: horizontalKeys, action: "choose"},
				goalRecoveryHint{key: "enter", action: "select"},
			)
		default:
			return r.renderHints(width,
				goalRecoveryHint{key: "esc", action: "back"},
				goalRecoveryHint{key: horizontalKeys, action: "choose"},
				goalRecoveryHint{key: "enter", action: "next"},
			)
		}
	}

	switch r.stage {
	case GoalRecoveryStageList:
		return r.renderHints(width,
			goalRecoveryHint{key: "esc", action: "close"},
			goalRecoveryHint{key: "enter", action: "review"},
			goalRecoveryHint{key: "j/k", action: "choose"},
			goalRecoveryHint{key: "u/d", action: "details"},
		)
	case GoalRecoveryStageSummary:
		return r.renderHints(width,
			goalRecoveryHint{key: "esc", action: "back"},
			goalRecoveryHint{key: "tab", action: "next"},
			goalRecoveryHint{key: "enter", action: "newline"},
		)
	case GoalRecoveryStageReference:
		return r.renderHints(width,
			goalRecoveryHint{key: "esc", action: "back"},
			goalRecoveryHint{key: "enter/tab", action: "next"},
		)
	case GoalRecoveryStageConfirmation:
		return r.renderHints(width,
			goalRecoveryHint{key: "esc", action: "back"},
			goalRecoveryHint{key: horizontalKeys, action: "choose"},
			goalRecoveryHint{key: "enter", action: "select"},
			goalRecoveryHint{key: "j/k", action: "details"},
		)
	default:
		return r.renderHints(width,
			goalRecoveryHint{key: "esc", action: "back"},
			goalRecoveryHint{key: "arrows/hjkl", action: "choose"},
			goalRecoveryHint{key: "enter/tab", action: "next"},
		)
	}
}

func (r *GoalRecovery) renderHints(width int, hints ...goalRecoveryHint) string {
	separator := r.styles.OverlayDim.Render(" · ")
	rows := make([]string, 0, 2)
	current := ""
	for _, hint := range hints {
		part := r.styles.FocusIndicator.Render(hint.key)
		if hint.action != "" {
			part += " " + r.styles.OverlayDim.Render(hint.action)
		}
		candidate := part
		if current != "" {
			candidate = current + separator + part
		}
		if current != "" && lipgloss.Width(candidate) > width {
			rows = append(rows, current)
			current = part
			continue
		}
		current = candidate
	}
	if current != "" {
		rows = append(rows, truncateDisplay(current, width))
	}
	return strings.Join(rows, "\n")
}

func (r *GoalRecovery) renderFrame(content, footer string) string {
	if footer != "" {
		content = strings.TrimRight(content, "\n") + "\n" + footer
	}
	return lipgloss.NewStyle().
		Border(borderForGlyphProfile(r.glyphProfile)).
		BorderForeground(r.styles.OverlayBorder).
		Padding(0, 1).
		Width(r.contentWidth() + 2).
		Render(content)
}
