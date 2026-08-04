package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	goalAcceptanceHeight    = 3
	goalFormMaximumCriteria = 64
)

// GoalFormField identifies one keyboard-focusable step in GoalForm.
type GoalFormField int

const (
	GoalFieldObjective GoalFormField = iota
	GoalFieldAcceptance
	GoalFieldTurns
	GoalFieldTokens
	GoalFieldTime
	GoalFieldActions
	goalFormFieldCount
)

// GoalAction is emitted to the parent when a user selects a form action. The
// parent owns persistence and the goal lifecycle; GoalForm only collects and
// validates presentation state.
type GoalAction string

const (
	GoalActionNone   GoalAction = ""
	GoalActionSave   GoalAction = "save"
	GoalActionPause  GoalAction = "pause"
	GoalActionResume GoalAction = "resume"
	GoalActionClear  GoalAction = "clear"
	GoalActionCancel GoalAction = "cancel"
)

// GoalFormValues is the typed payload collected by GoalForm. A zero budget
// means no limit.
type GoalFormValues struct {
	Objective          string
	AcceptanceCriteria string
	TurnBudget         int64
	TokenBudget        int64
	TimeBudget         time.Duration
}

// CriterionDescriptions returns the normalized, nonblank line items the
// parent can map to durable acceptance-criterion IDs.
func (v GoalFormValues) CriterionDescriptions() []string {
	return goalAcceptanceCriteria(v.AcceptanceCriteria)
}

// GoalFormChoice configures one action in the form's final focus step.
// RequiresValidGoal should be true for actions that create or update a goal.
type GoalFormChoice struct {
	Action            GoalAction
	Label             string
	Description       string
	RequiresValidGoal bool
	Destructive       bool
}

// GoalFormOptions contains display preferences owned by the parent Model.
type GoalFormOptions struct {
	Width         int
	Height        int
	IsDark        bool
	ThemeID       string
	ReducedMotion bool
	GlyphProfile  GlyphProfile
	// DraftFromPrompt tells the form that the initial definition was inferred
	// from the user's composer text. The draft remains fully editable.
	DraftFromPrompt bool
	// FollowUpPrompt replaces generic review copy when the inferred draft is
	// missing one concrete detail. It is guidance, not a validation error.
	FollowUpPrompt string
	// BudgetOnly locks the immutable objective and acceptance criteria while
	// preserving them as context for a budget amendment.
	BudgetOnly bool
	Choices    []GoalFormChoice
}

// GoalFormEvent describes a user decision. GoalActionNone means the form only
// changed local presentation state.
type GoalFormEvent struct {
	Action GoalAction
	Values GoalFormValues
}

// GoalForm is a parent-routed, persistence-free inline component. It does not
// implement tea.Model: the application Model receives messages first and may
// forward them through Update, then decide what to do with GoalFormEvent.
type GoalForm struct {
	objective  textinput.Model
	acceptance textarea.Model
	turns      textinput.Model
	tokens     textinput.Model
	time       textinput.Model

	active      GoalFormField
	choices     []GoalFormChoice
	choiceIndex int
	errorText   string

	width           int
	height          int
	isDark          bool
	themeID         string
	reducedMotion   bool
	glyphProfile    GlyphProfile
	budgetOnly      bool
	draftFromPrompt bool
	followUpPrompt  string
	customChoices   bool
	styles          Styles
	cache           goalFormRenderCache
}

type goalFormRenderCache struct {
	valid   bool
	view    string
	cursor  *tea.Cursor
	renders int
}

// NewGoalForm creates a focused, responsive goal form. The zero options value
// defaults to an 80x24 canvas and Save/Cancel actions.
func NewGoalForm(initial GoalFormValues, options GoalFormOptions) *GoalForm {
	if options.Width <= 0 {
		options.Width = 80
	}
	if options.Height <= 0 {
		options.Height = 24
	}

	f := &GoalForm{
		width:           options.Width,
		height:          options.Height,
		isDark:          options.IsDark,
		reducedMotion:   options.ReducedMotion,
		glyphProfile:    resolveGlyphProfile(options.GlyphProfile),
		budgetOnly:      options.BudgetOnly,
		draftFromPrompt: options.DraftFromPrompt,
		followUpPrompt:  strings.TrimSpace(options.FollowUpPrompt),
		styles:          NewStyles(options.IsDark, resolveThemeID(options.ThemeID)),
	}
	f.objective = newGoalTextInput("What outcome should persist?", 768)
	f.acceptance = textarea.New()
	f.acceptance.Prompt = ""
	f.acceptance.Placeholder = "One verifiable result per line (required)"
	f.acceptance.ShowLineNumbers = false
	f.acceptance.CharLimit = 4096
	f.acceptance.MaxHeight = goalAcceptanceHeight
	f.acceptance.SetHeight(goalAcceptanceHeight)
	f.turns = newGoalTextInput("no auto-turn limit", 9)
	f.tokens = newGoalTextInput("no token limit", 16)
	f.time = newGoalTextInput("e.g. 90m", 16)

	f.setChoices(options.Choices, len(options.Choices) > 0)
	f.setValues(initial)
	f.applyStyles()
	f.sizeInputs()
	f.focusField(f.firstEditableField())
	f.cache.valid = false
	return f
}

func newGoalTextInput(placeholder string, charLimit int) textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = placeholder
	input.CharLimit = charLimit
	return input
}

func defaultGoalFormChoices(budgetOnly bool) []GoalFormChoice {
	saveLabel := "Save goal"
	saveDescription := "Create durable goal"
	cancelDescription := "Do not create goal"
	if budgetOnly {
		saveLabel = "Save budgets"
		saveDescription = "Update limits only"
		cancelDescription = "Keep current limits"
	}
	return []GoalFormChoice{
		{
			Action:            GoalActionSave,
			Label:             saveLabel,
			Description:       saveDescription,
			RequiresValidGoal: true,
		},
		{
			Action:      GoalActionCancel,
			Label:       "Cancel",
			Description: cancelDescription,
		},
	}
}

// SetChoices replaces the final action row. Blank choices are discarded and
// an empty result restores the safe Save/Cancel defaults.
func (f *GoalForm) SetChoices(choices ...GoalFormChoice) {
	if f == nil {
		return
	}
	f.setChoices(choices, len(choices) > 0)
}

func (f *GoalForm) setChoices(choices []GoalFormChoice, custom bool) {
	selected := GoalActionNone
	if len(f.choices) > 0 && f.choiceIndex >= 0 && f.choiceIndex < len(f.choices) {
		selected = f.choices[f.choiceIndex].Action
	}

	cleaned := make([]GoalFormChoice, 0, len(choices))
	for _, choice := range choices {
		choice.Label = strings.TrimSpace(choice.Label)
		if !validGoalFormAction(choice.Action) || choice.Label == "" {
			continue
		}
		if choice.Action == GoalActionSave {
			choice.RequiresValidGoal = true
		}
		cleaned = append(cleaned, choice)
	}
	if len(cleaned) == 0 {
		cleaned = defaultGoalFormChoices(f.budgetOnly)
		custom = false
	}
	f.choices = cleaned
	f.customChoices = custom
	f.choiceIndex = 0
	for index := range f.choices {
		if f.choices[index].Action == selected {
			f.choiceIndex = index
			break
		}
	}
	f.invalidate()
}

func validGoalFormAction(action GoalAction) bool {
	switch action {
	case GoalActionSave, GoalActionPause, GoalActionResume, GoalActionClear, GoalActionCancel:
		return true
	default:
		return false
	}
}

// SetBudgetOnly switches between new-goal editing and immutable-definition
// budget amendment. Locked definition fields remain visible but are skipped by
// focus and never receive input messages.
func (f *GoalForm) SetBudgetOnly(budgetOnly bool) tea.Cmd {
	if f == nil || f.budgetOnly == budgetOnly {
		return nil
	}
	f.budgetOnly = budgetOnly
	if !f.customChoices {
		f.setChoices(nil, false)
	}
	f.sizeInputs()
	if f.active < f.firstEditableField() {
		return f.focusField(f.firstEditableField())
	}
	f.invalidate()
	return nil
}

// BudgetOnly reports whether the immutable goal definition is locked.
func (f *GoalForm) BudgetOnly() bool {
	return f != nil && f.budgetOnly
}

// SetSize adapts the cached form and its Bubbles inputs to the current canvas.
func (f *GoalForm) SetSize(width, height int) {
	if f == nil {
		return
	}
	width = max(1, width)
	height = max(1, height)
	if f.width == width && f.height == height {
		return
	}
	f.width = width
	f.height = height
	f.sizeInputs()
	f.invalidate()
}

// SetTheme reapplies the project's LightDark-derived semantic palette.
func (f *GoalForm) SetTheme(isDark bool, themeID string) {
	if f == nil {
		return
	}
	themeID = resolveThemeID(themeID)
	if f.isDark == isDark && resolveThemeID(f.themeID) == themeID {
		return
	}
	f.isDark, f.themeID = isDark, themeID
	f.styles = NewStyles(isDark, f.themeID)
	f.applyStyles()
	f.invalidate()
}

// SetReducedMotion replaces blinking cursors with static cursors. GoalForm
// never schedules decorative animation ticks of its own.
func (f *GoalForm) SetReducedMotion(reduced bool) {
	if f == nil || f.reducedMotion == reduced {
		return
	}
	f.reducedMotion = reduced
	f.applyStyles()
	f.invalidate()
}

func (f *GoalForm) applyStyles() {
	inputStyles := semanticTextInputStyles(f.isDark, f.themeID)
	inputStyles.Cursor.Blink = !f.reducedMotion
	f.objective.SetStyles(inputStyles)
	f.turns.SetStyles(inputStyles)
	f.tokens.SetStyles(inputStyles)
	f.time.SetStyles(inputStyles)
	f.acceptance.SetStyles(goalTextareaStyles(f.isDark, f.themeID, f.reducedMotion))
}

func goalTextareaStyles(isDark bool, themeID string, reducedMotion bool) textarea.Styles {
	styles := textarea.DefaultStyles(isDark)
	palette := outputSemanticPalette(isDark, themeID)
	styles.Focused.Base = lipgloss.NewStyle().Foreground(palette.Text)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(palette.Text)
	styles.Focused.CursorLine = lipgloss.NewStyle().Foreground(palette.Text)
	styles.Focused.CursorLineNumber = lipgloss.NewStyle().Foreground(palette.Accent)
	styles.Focused.LineNumber = lipgloss.NewStyle().Foreground(palette.Dim)
	styles.Focused.EndOfBuffer = lipgloss.NewStyle().Foreground(palette.Dim)
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(palette.Dim)
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(palette.Accent).Bold(true)
	styles.Blurred.Base = lipgloss.NewStyle().Foreground(palette.Muted)
	styles.Blurred.Text = lipgloss.NewStyle().Foreground(palette.Muted)
	styles.Blurred.CursorLine = lipgloss.NewStyle().Foreground(palette.Muted)
	styles.Blurred.CursorLineNumber = lipgloss.NewStyle().Foreground(palette.Dim)
	styles.Blurred.LineNumber = lipgloss.NewStyle().Foreground(palette.Dim)
	styles.Blurred.EndOfBuffer = lipgloss.NewStyle().Foreground(palette.Dim)
	styles.Blurred.Placeholder = lipgloss.NewStyle().Foreground(palette.Dim)
	styles.Blurred.Prompt = lipgloss.NewStyle().Foreground(palette.Dim)
	styles.Cursor.Color = palette.Accent
	styles.Cursor.Blink = !reducedMotion
	return styles
}

func (f *GoalForm) sizeInputs() {
	contentWidth := f.contentWidth()
	fullWidth := max(1, contentWidth-2)
	budgetWidth := fullWidth
	if !f.compact() {
		budgetWidth = max(1, contentWidth-12)
	}
	f.objective.SetWidth(fullWidth)
	f.acceptance.SetWidth(goalAcceptanceInputWidth(contentWidth))
	f.acceptance.SetHeight(f.acceptanceHeight())
	f.turns.SetWidth(budgetWidth)
	f.tokens.SetWidth(budgetWidth)
	f.time.SetWidth(budgetWidth)
}

// goalAcceptanceInputWidth reserves the visible two-cell prompt gutter plus a
// two-cell frame safety margin. Without the latter, Lip Gloss can perform a
// second boundary wrap after Bubbles renders the textarea; that new row never
// passes through renderAcceptance and therefore loses its continuation indent.
func goalAcceptanceInputWidth(contentWidth int) int {
	return max(1, contentWidth-4)
}

func (f *GoalForm) contentWidth() int {
	return inlineFormContentWidth(f.width)
}

func (f *GoalForm) compact() bool {
	return f.width <= 48 || f.height < 27
}

func (f *GoalForm) minimumHeightLayout() bool {
	return f.height <= minTerminalHeight
}

func (f *GoalForm) acceptanceHeight() int {
	if f.minimumHeightLayout() {
		return 2
	}
	return goalAcceptanceHeight
}

// ActiveField returns the currently focused form step.
func (f *GoalForm) ActiveField() GoalFormField {
	if f == nil {
		return GoalFieldObjective
	}
	return f.active
}

// SetActiveField moves focus without changing any values.
func (f *GoalForm) SetActiveField(field GoalFormField) tea.Cmd {
	if f == nil {
		return nil
	}
	return f.focusField(field)
}

func (f *GoalForm) focusField(field GoalFormField) tea.Cmd {
	field = min(max(f.firstEditableField(), field), GoalFieldActions)
	// A validation error belongs to the field focus selected for it. Once the
	// user intentionally moves elsewhere, do not leave that error attached to
	// an unrelated compact step.
	f.errorText = ""
	f.objective.Blur()
	f.acceptance.Blur()
	f.turns.Blur()
	f.tokens.Blur()
	f.time.Blur()
	f.active = field
	f.invalidate()

	switch field {
	case GoalFieldObjective:
		return f.objective.Focus()
	case GoalFieldAcceptance:
		return f.acceptance.Focus()
	case GoalFieldTurns:
		return f.turns.Focus()
	case GoalFieldTokens:
		return f.tokens.Focus()
	case GoalFieldTime:
		return f.time.Focus()
	default:
		return nil
	}
}

func (f *GoalForm) advanceField(direction int) tea.Cmd {
	return f.focusField(f.active + GoalFormField(direction))
}

func (f *GoalForm) firstEditableField() GoalFormField {
	if f != nil && f.budgetOnly {
		return GoalFieldTurns
	}
	return GoalFieldObjective
}

// Error returns the most recent inline validation or parent-owned semantic
// message.
func (f *GoalForm) Error() string {
	if f == nil {
		return ""
	}
	return f.errorText
}

// SetError presents a parent-owned semantic error without changing the
// user's values, focus, or selected action. The parent remains responsible
// for deciding whether an action is authorized; GoalForm only renders the
// feedback beside the action that was rejected.
func (f *GoalForm) SetError(message string) {
	if f == nil {
		return
	}
	f.errorText = strings.TrimSpace(message)
	f.invalidate()
}

// Values validates and returns the typed form payload.
func (f *GoalForm) Values() (GoalFormValues, error) {
	values, _, err := f.validatedValues()
	return values, err
}

func (f *GoalForm) validatedValues() (GoalFormValues, GoalFormField, error) {
	values := GoalFormValues{
		Objective: strings.TrimSpace(f.objective.Value()),
	}
	criteria := goalAcceptanceCriteria(f.acceptance.Value())
	values.AcceptanceCriteria = strings.Join(criteria, "\n")
	if !f.budgetOnly {
		if values.Objective == "" {
			return values, GoalFieldObjective, fmt.Errorf("objective is required")
		}
		if len(criteria) == 0 {
			return values, GoalFieldAcceptance, fmt.Errorf("add at least one acceptance criterion")
		}
		if len(criteria) > goalFormMaximumCriteria {
			return values, GoalFieldAcceptance, fmt.Errorf("acceptance criteria are limited to %d lines", goalFormMaximumCriteria)
		}
	}

	turns, err := parsePositiveBudget(f.turns.Value())
	if err != nil {
		return values, GoalFieldTurns, fmt.Errorf("auto-turn budget must be a positive whole number")
	}
	values.TurnBudget = turns

	tokens, err := parsePositiveBudget(f.tokens.Value())
	if err != nil {
		return values, GoalFieldTokens, fmt.Errorf("token budget must be a positive whole number")
	}
	values.TokenBudget = tokens

	timeText := strings.TrimSpace(f.time.Value())
	if timeText != "" {
		values.TimeBudget, err = time.ParseDuration(timeText)
		if err != nil || values.TimeBudget <= 0 {
			return values, GoalFieldTime, fmt.Errorf("time budget must be a positive duration such as 90m")
		}
	}
	if values.TurnBudget == 0 && values.TokenBudget == 0 && values.TimeBudget == 0 {
		return values, GoalFieldTurns, fmt.Errorf("set at least one auto-turn, token, or time budget")
	}
	return values, GoalFieldObjective, nil
}

func goalAcceptanceCriteria(value string) []string {
	lines := strings.Split(value, "\n")
	criteria := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			criteria = append(criteria, line)
		}
	}
	return criteria
}

func parsePositiveBudget(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	budget, err := strconv.ParseInt(value, 10, 64)
	if err != nil || budget <= 0 {
		return 0, fmt.Errorf("budget must be positive")
	}
	return budget, nil
}

func (f *GoalForm) setValues(values GoalFormValues) {
	f.objective.SetValue(values.Objective)
	f.acceptance.SetValue(values.AcceptanceCriteria)
	f.turns.SetValue(formatGoalBudget(values.TurnBudget))
	f.tokens.SetValue(formatGoalBudget(values.TokenBudget))
	if values.TimeBudget > 0 {
		f.time.SetValue(formatGoalTimeInput(values.TimeBudget))
	} else {
		f.time.SetValue("")
	}
	f.invalidate()
}

func formatGoalTimeInput(duration time.Duration) string {
	value := duration.String()
	if strings.HasSuffix(value, "m0s") {
		value = strings.TrimSuffix(value, "0s")
	}
	if strings.HasSuffix(value, "h0m") {
		value = strings.TrimSuffix(value, "0m")
	}
	return value
}

func formatGoalBudget[T ~int | ~int64](budget T) string {
	if budget <= 0 {
		return ""
	}
	return strconv.FormatInt(int64(budget), 10)
}

// Update applies one parent-routed Bubble Tea message and emits a semantic
// action when appropriate. The returned command belongs in the parent's normal
// command batch so Bubbles cursor behavior remains cancellable.
func (f *GoalForm) Update(msg tea.Msg) (GoalFormEvent, tea.Cmd) {
	if f == nil {
		return GoalFormEvent{}, nil
	}
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		f.SetSize(size.Width, size.Height)
		return GoalFormEvent{}, nil
	}

	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case keyMsg.Code == tea.KeyEscape:
			return GoalFormEvent{Action: GoalActionCancel}, nil
		case keyMsg.Code == tea.KeyTab:
			if keyMsg.Mod == tea.ModShift {
				return GoalFormEvent{}, f.advanceField(-1)
			}
			return GoalFormEvent{}, f.advanceField(1)
		case f.active == GoalFieldActions && isGoalChoicePrevious(keyMsg):
			if f.choiceIndex > 0 {
				f.choiceIndex--
				f.invalidate()
			}
			return GoalFormEvent{}, nil
		case f.active == GoalFieldActions && isGoalChoiceNext(keyMsg):
			if f.choiceIndex < len(f.choices)-1 {
				f.choiceIndex++
				f.invalidate()
			}
			return GoalFormEvent{}, nil
		case (keyMsg.Code == tea.KeyEnter || keyMsg.Code == tea.KeySpace) && f.active == GoalFieldActions:
			return f.selectChoice()
		case keyMsg.Code == tea.KeyEnter && f.active != GoalFieldAcceptance:
			return GoalFormEvent{}, f.advanceField(1)
		}
	}

	f.errorText = ""
	var command tea.Cmd
	switch f.active {
	case GoalFieldObjective:
		f.objective, command = f.objective.Update(msg)
	case GoalFieldAcceptance:
		f.acceptance, command = f.acceptance.Update(msg)
	case GoalFieldTurns:
		f.turns, command = f.turns.Update(msg)
	case GoalFieldTokens:
		f.tokens, command = f.tokens.Update(msg)
	case GoalFieldTime:
		f.time, command = f.time.Update(msg)
	}
	f.invalidate()
	return GoalFormEvent{}, command
}

func isGoalChoicePrevious(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeyLeft || msg.Code == tea.KeyUp ||
		(msg.Mod == 0 && (msg.Text == "h" || msg.Text == "k"))
}

func isGoalChoiceNext(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeyRight || msg.Code == tea.KeyDown ||
		(msg.Mod == 0 && (msg.Text == "l" || msg.Text == "j"))
}

func (f *GoalForm) selectChoice() (GoalFormEvent, tea.Cmd) {
	if len(f.choices) == 0 {
		return GoalFormEvent{}, nil
	}
	selected := f.choices[min(max(0, f.choiceIndex), len(f.choices)-1)]
	if selected.Action == GoalActionCancel {
		return GoalFormEvent{Action: GoalActionCancel}, nil
	}
	if selected.RequiresValidGoal {
		values, field, err := f.validatedValues()
		if err != nil {
			f.errorText = err.Error()
			command := f.focusField(field)
			f.errorText = err.Error()
			f.invalidate()
			return GoalFormEvent{}, command
		}
		f.errorText = ""
		return GoalFormEvent{Action: selected.Action, Values: values}, nil
	}

	values, _, _ := f.validatedValues()
	return GoalFormEvent{Action: selected.Action, Values: values}, nil
}

func (f *GoalForm) invalidate() {
	if f != nil {
		f.cache.valid = false
	}
}

// View renders the cached form without a hardware-cursor position.
func (f *GoalForm) View() string {
	view, _ := f.ViewWithCursor()
	return view
}

// ViewWithCursor renders the form and a cursor local to the returned frame.
// The parent translates it from the inline footer's origin.
func (f *GoalForm) ViewWithCursor() (string, *tea.Cursor) {
	if f == nil {
		return "", nil
	}
	if f.cache.valid {
		return f.cache.view, cloneGoalCursor(f.cache.cursor)
	}

	var view string
	var cursor *tea.Cursor
	if f.compact() {
		view, cursor = f.renderCompactView()
	} else {
		view, cursor = f.renderFullView()
	}
	f.cache.valid = true
	f.cache.view = view
	f.cache.cursor = cloneGoalCursor(cursor)
	f.cache.renders++
	return view, cloneGoalCursor(cursor)
}

func cloneGoalCursor(cursor *tea.Cursor) *tea.Cursor {
	if cursor == nil {
		return nil
	}
	copy := *cursor
	return &copy
}

func (f *GoalForm) renderCompactView() (string, *tea.Cursor) {
	width := f.contentWidth()
	var b strings.Builder
	title := "Goal"
	if f.budgetOnly {
		title = "Goal budgets"
	}
	b.WriteString(f.styles.OverlayTitle.Render(fmt.Sprintf("%s · %d/%d", title, f.visibleFieldNumber(), f.visibleFieldCount())))
	b.WriteString("\n")
	if f.draftFromPrompt && !f.budgetOnly {
		prompt := "Prompt draft · review"
		if f.followUpPrompt != "" {
			prompt = "Needs detail · " + f.followUpPrompt
		}
		if f.minimumHeightLayout() {
			if f.followUpPrompt != "" {
				prompt = "Needs detail"
			}
			b.WriteString(f.styles.OverlayDim.Render(truncateDisplay(prompt, width)))
		} else if f.followUpPrompt != "" {
			b.WriteString(f.styles.OverlayDim.Render(wrapText(prompt, width)))
		} else {
			b.WriteString(f.styles.OverlayDim.Render(truncateDisplay(prompt, width)))
		}
		b.WriteString("\n")
		if f.active == GoalFieldActions && !f.minimumHeightLayout() {
			b.WriteString(f.renderCompactGoalSummary(width))
			b.WriteString("\n")
		}
	}
	if f.budgetOnly {
		b.WriteString(f.renderLockedDefinitionContext(width))
		b.WriteString("\n")
	}
	b.WriteString(f.styles.FocusIndicator.Render(goalFormFieldLabel(f.active)))
	b.WriteString("\n")
	controlY := strings.Count(b.String(), "\n")

	control, cursor := f.renderActiveControl(f.active, width)
	b.WriteString(control)
	cursor = offsetCursor(cursor, 0, controlY)
	if f.errorText != "" {
		b.WriteString("\n")
		b.WriteString(f.styles.ErrorText.Render(truncateDisplay("! "+f.errorText, max(1, width-2))))
	}
	return f.renderFrame(b.String(), f.renderFooter(width)), pickerFrameCursor(cursor)
}

func (f *GoalForm) renderCompactGoalSummary(width int) string {
	lineWidth := max(1, width-2)
	objective := strings.TrimSpace(f.objective.Value())
	if objective == "" {
		objective = "untitled goal"
	}
	criteriaValues := goalAcceptanceCriteria(f.acceptance.Value())
	criteria := len(criteriaValues)
	criteriaLabel := "criteria"
	if criteria == 1 {
		criteriaLabel = "criterion"
	}
	limits := make([]string, 0, 3)
	if value := strings.TrimSpace(f.turns.Value()); value != "" {
		limits = append(limits, value+" turns")
	}
	if value := strings.TrimSpace(f.tokens.Value()); value != "" {
		limits = append(limits, value+" tokens")
	}
	if value := strings.TrimSpace(f.time.Value()); value != "" {
		limits = append(limits, value)
	}
	if len(limits) == 1 && strings.TrimSpace(f.time.Value()) != "" {
		limits = append(limits, "turns/tokens unlimited")
	}
	definition := f.styles.OverlayDim.Render(truncateDisplay(objective, lineWidth))
	budget := fmt.Sprintf("%d %s", criteria, criteriaLabel)
	if len(limits) > 0 {
		budget += " · " + strings.Join(limits, " · ")
	}
	lines := []string{definition, f.styles.OverlayDim.Render(truncateDisplay(budget, lineWidth))}
	if f.height >= 16 && len(criteriaValues) > 0 {
		lines = append(lines, f.styles.OverlayDim.Render(truncateDisplay("Proof · "+criteriaValues[0], lineWidth)))
		if len(criteriaValues) > 1 {
			remaining := len(criteriaValues) - 1
			label := "criterion"
			if remaining != 1 {
				label = "criteria"
			}
			lines = append(lines, f.styles.OverlayDim.Render(truncateDisplay(fmt.Sprintf("+%d more %s", remaining, label), lineWidth)))
		}
	}
	return strings.Join(lines, "\n")
}

func (f *GoalForm) renderFullView() (string, *tea.Cursor) {
	width := f.contentWidth()
	var b strings.Builder
	var cursor *tea.Cursor

	title := "Goal"
	subtitle := "Define done, then bound the run."
	if f.draftFromPrompt && !f.budgetOnly {
		title = "Review goal draft"
		subtitle = "Inferred from your prompt · edit objective, proof, and limits before AUTO starts."
		if f.followUpPrompt != "" {
			title = "Complete goal details"
			subtitle = f.followUpPrompt
		}
	}
	if f.budgetOnly {
		title = "Goal budgets"
		subtitle = "Definition locked · adjust limits only."
	}
	b.WriteString(f.styles.OverlayTitle.Render(title))
	b.WriteString("\n")
	b.WriteString(f.styles.OverlayDim.Render(subtitle))
	b.WriteString("\n\n")

	b.WriteString(f.fieldLabel("Objective", GoalFieldObjective))
	b.WriteString("\n")
	controlY := strings.Count(b.String(), "\n")
	objective, objectiveCursor := f.renderTextInput(&f.objective, f.active == GoalFieldObjective, width, "(required)")
	b.WriteString(objective)
	b.WriteString(f.renderInlineError(GoalFieldObjective, width))
	if objectiveCursor != nil {
		cursor = offsetCursor(objectiveCursor, 0, controlY)
	}

	b.WriteString("\n\n")
	b.WriteString(f.fieldLabel("Acceptance criteria", GoalFieldAcceptance))
	b.WriteString("\n")
	controlY = strings.Count(b.String(), "\n")
	acceptance, acceptanceCursor := f.renderAcceptance(f.active == GoalFieldAcceptance, width)
	b.WriteString(acceptance)
	b.WriteString(f.renderInlineError(GoalFieldAcceptance, width))
	if acceptanceCursor != nil {
		cursor = offsetCursor(acceptanceCursor, 0, controlY)
	}

	b.WriteString("\n\n")
	b.WriteString(f.styles.OverlayAccent.Render("Limits"))
	b.WriteString(f.styles.OverlayDim.Render(" · set at least one"))
	b.WriteString("\n")
	for _, budget := range []struct {
		label string
		field GoalFormField
		input *textinput.Model
	}{
		{label: "Auto turns", field: GoalFieldTurns, input: &f.turns},
		{label: "Tokens", field: GoalFieldTokens, input: &f.tokens},
		{label: "Time", field: GoalFieldTime, input: &f.time},
	} {
		lineY := strings.Count(b.String(), "\n")
		line, lineCursor := f.renderBudgetLine(budget.label, budget.field, budget.input, width)
		b.WriteString(line)
		b.WriteString(f.renderInlineError(budget.field, width))
		b.WriteString("\n")
		if lineCursor != nil {
			cursor = offsetCursor(lineCursor, 0, lineY)
		}
	}

	b.WriteString("\n")
	b.WriteString(f.fieldLabel("Actions", GoalFieldActions))
	b.WriteString("\n")
	b.WriteString(f.renderActionControl(width, false))
	b.WriteString(f.renderInlineError(GoalFieldActions, width))

	return f.renderFrame(strings.TrimRight(b.String(), "\n"), f.renderFooter(width)), pickerFrameCursor(cursor)
}

func goalFormFieldLabel(field GoalFormField) string {
	switch field {
	case GoalFieldObjective:
		return "Objective"
	case GoalFieldAcceptance:
		return "Acceptance criteria"
	case GoalFieldTurns:
		return "Auto-turn budget"
	case GoalFieldTokens:
		return "Token budget"
	case GoalFieldTime:
		return "Time budget"
	case GoalFieldActions:
		return "Actions"
	default:
		return "Goal"
	}
}

func (f *GoalForm) fieldLabel(label string, field GoalFormField) string {
	if f.budgetOnly && field < GoalFieldTurns {
		return f.styles.OverlayDim.Render(label + " · locked")
	}
	if f.active == field {
		return f.styles.FocusIndicator.Render(label)
	}
	return f.styles.OverlayAccent.Render(label)
}

func (f *GoalForm) renderActiveControl(field GoalFormField, width int) (string, *tea.Cursor) {
	switch field {
	case GoalFieldObjective:
		return f.renderTextInput(&f.objective, true, width, "(required)")
	case GoalFieldAcceptance:
		return f.renderAcceptance(true, width)
	case GoalFieldTurns:
		return f.renderBudgetStep(&f.turns, width)
	case GoalFieldTokens:
		return f.renderBudgetStep(&f.tokens, width)
	case GoalFieldTime:
		return f.renderBudgetStep(&f.time, width)
	case GoalFieldActions:
		return f.renderActionControl(width, true), nil
	default:
		return "", nil
	}
}

func (f *GoalForm) renderBudgetStep(input *textinput.Model, width int) (string, *tea.Cursor) {
	control, cursor := f.renderTextInput(input, true, width, "unlimited")
	if f.errorText != "" {
		return control, cursor
	}
	if f.minimumHeightLayout() {
		if f.budgetOnly {
			return control, cursor
		}
		return control + "\n" + f.styles.OverlayDim.Render(truncateDisplay("Blank skips · set one", width)), cursor
	}
	helper := lipgloss.JoinVertical(
		lipgloss.Left,
		f.styles.OverlayDim.Render(truncateDisplay("Blank skips this limit", width)),
		f.styles.OverlayDim.Render(truncateDisplay("set at least one limit", width)),
	)
	return control + "\n" + helper, cursor
}

func (f *GoalForm) renderInlineError(field GoalFormField, width int) string {
	if f.errorText == "" || f.active != field {
		return ""
	}
	return "\n" + f.styles.ErrorText.Render(truncateDisplay("! "+f.errorText, max(1, width-2)))
}

func (f *GoalForm) renderTextInput(input *textinput.Model, active bool, width int, empty string) (string, *tea.Cursor) {
	if input == nil {
		return "", nil
	}
	if !active {
		value := strings.TrimSpace(input.Value())
		if value == "" {
			value = empty
		}
		return "  " + f.styles.OverlayDim.Render(truncateDisplay(value, max(1, width-2))), nil
	}

	view := *input
	view.SetWidth(max(1, width-2))
	view.SetVirtualCursor(false)
	return f.styles.FocusIndicator.Render("> ") + view.View(), offsetCursor(view.Cursor(), 2, 0)
}

func (f *GoalForm) renderAcceptance(active bool, width int) (string, *tea.Cursor) {
	view := f.acceptance
	view.SetWidth(goalAcceptanceInputWidth(width))
	view.SetHeight(f.acceptanceHeight())
	// Textarea populates and repositions its private viewport during Update.
	// Initial values can be taller than the compact window before the child has
	// received a key, so reflow a render-only copy to keep the visible rows and
	// hardware cursor on the same multiline position.
	view, _ = view.Update(goalAcceptanceReflowMsg{})
	view.SetVirtualCursor(false)
	lines := strings.Split(view.View(), "\n")
	for index := range lines {
		prefix := "  "
		if active && index == 0 {
			prefix = "> "
		}
		if active {
			prefix = f.styles.FocusIndicator.Render(prefix)
		}
		lines[index] = prefix + lines[index]
	}
	if !active && strings.TrimSpace(f.acceptance.Value()) == "" && len(lines) > 0 {
		lines[0] = "  " + f.styles.OverlayDim.Render("(required)")
	}
	if !active {
		return strings.Join(lines, "\n"), nil
	}
	return strings.Join(lines, "\n"), offsetCursor(view.Cursor(), 2, 0)
}

type goalAcceptanceReflowMsg struct{}

func (f *GoalForm) visibleFieldCount() int {
	if f.budgetOnly {
		return int(goalFormFieldCount - GoalFieldTurns)
	}
	return int(goalFormFieldCount)
}

func (f *GoalForm) visibleFieldNumber() int {
	return int(f.active-f.firstEditableField()) + 1
}

func (f *GoalForm) renderLockedDefinitionContext(width int) string {
	objective := strings.TrimSpace(f.objective.Value())
	if objective == "" {
		objective = "untitled goal"
	}
	count := len(goalAcceptanceCriteria(f.acceptance.Value()))
	noun := "criteria"
	if count == 1 {
		noun = "criterion"
	}
	lineWidth := max(1, width-2)
	definition := f.styles.OverlayDim.Render(truncateDisplay("Locked · "+objective, lineWidth))
	criteria := f.styles.OverlayDim.Render(truncateDisplay(fmt.Sprintf("%d acceptance %s", count, noun), lineWidth))
	return definition + "\n" + criteria
}

func (f *GoalForm) renderBudgetLine(label string, field GoalFormField, input *textinput.Model, width int) (string, *tea.Cursor) {
	labelStyle := f.styles.OverlayAccent
	if f.active == field {
		labelStyle = f.styles.FocusIndicator
	}
	prefix := labelStyle.Render(fmt.Sprintf("%-10s", label)) + " "
	control, cursor := f.renderTextInput(input, f.active == field, max(1, width-lipgloss.Width(prefix)), "unlimited")
	return prefix + control, offsetCursor(cursor, lipgloss.Width(prefix), 0)
}

func (f *GoalForm) renderActionControl(width int, compact bool) string {
	if len(f.choices) == 0 {
		return f.styles.OverlayDim.Render("(no actions)")
	}
	selected := min(max(0, f.choiceIndex), len(f.choices)-1)
	choice := f.choices[selected]
	glyphs := glyphSet(f.glyphProfile)
	if compact {
		left := ""
		right := ""
		if selected > 0 {
			left = glyphs.Left + " "
		}
		if selected < len(f.choices)-1 {
			right = " " + glyphs.Right
		}
		label := left + glyphs.Collapsed + " " + choice.Label + right
		style := f.styles.FocusIndicator
		if choice.Destructive {
			style = f.styles.ErrorText
		}
		result := style.Render(truncateDisplayWithGlyphProfile(label, width, f.glyphProfile))
		if description := strings.TrimSpace(choice.Description); description != "" && !f.minimumHeightLayout() {
			result += "\n" + f.styles.OverlayDim.Render(truncateDisplay(description, max(1, width-1)))
		}
		return result
	}

	parts := make([]string, 0, len(f.choices))
	for index, option := range f.choices {
		marker := "  "
		style := f.styles.OverlayDim
		if index == selected {
			marker = glyphs.Collapsed + " "
			style = f.styles.FocusIndicator
			if option.Destructive {
				style = f.styles.ErrorText
			}
		}
		parts = append(parts, style.Render(marker+option.Label))
	}
	choiceLine := truncateDisplay(strings.Join(parts, f.styles.OverlayDim.Render(" · ")), width)
	description := f.styles.OverlayDim.Render(truncateDisplay(strings.TrimSpace(choice.Description), width))
	if strings.TrimSpace(choice.Description) == "" {
		return choiceLine
	}
	return choiceLine + "\n" + description
}

type goalFormHint struct {
	key    string
	action string
}

func (f *GoalForm) renderFooter(width int) string {
	if f.minimumHeightLayout() {
		return f.renderMinimumFooter(width)
	}
	hints := []goalFormHint{{key: "esc", action: "cancel"}}
	switch f.active {
	case GoalFieldAcceptance:
		hints = append(hints,
			goalFormHint{key: "tab/shift+tab", action: "move"},
			goalFormHint{key: "enter", action: "newline"},
		)
	case GoalFieldActions:
		hints = append(hints,
			goalFormHint{key: "enter/space", action: "select"},
			goalFormHint{key: "arrows/hjkl", action: "choose"},
			goalFormHint{key: "shift+tab", action: "back"},
		)
	default:
		hints = append(hints,
			goalFormHint{key: "enter/tab", action: "next"},
		)
		if f.active > f.firstEditableField() {
			hints = append(hints, goalFormHint{key: "shift+tab", action: "back"})
		}
	}

	separator := f.styles.OverlayDim.Render(goalStatusSeparator(f.glyphProfile))
	rows := make([]string, 0, 2)
	current := ""
	for _, hint := range hints {
		part := f.styles.FocusIndicator.Render(f.staticChrome(hint.key))
		if hint.action != "" {
			part += " " + f.styles.OverlayDim.Render(f.staticChrome(hint.action))
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

func (f *GoalForm) renderMinimumFooter(width int) string {
	if f.errorText != "" {
		return f.renderMinimumHint(width, "esc/tab", "cancel or move")
	}

	var hints []goalFormHint
	switch f.active {
	case GoalFieldAcceptance:
		hints = []goalFormHint{
			{key: "esc", action: "cancel · tab/⇧ move"},
			{key: "enter", action: "newline"},
		}
	case GoalFieldActions:
		if f.budgetOnly {
			hints = []goalFormHint{
				{key: "esc", action: "cancel · ⇧tab back"},
				{key: "enter/space · arrows/hjkl"},
			}
		} else {
			hints = []goalFormHint{
				{key: "esc", action: "cancel · ⇧tab back"},
				{key: "enter/space", action: "select"},
				{key: "arrows/hjkl", action: "choose"},
			}
		}
	default:
		back := goalFormHint{key: "esc", action: "cancel"}
		if f.active > f.firstEditableField() {
			back = goalFormHint{key: "esc", action: "cancel · ⇧tab back"}
		}
		hints = []goalFormHint{back, {key: "enter/tab", action: "next"}}
	}

	rows := make([]string, 0, len(hints))
	for _, hint := range hints {
		rows = append(rows, f.renderMinimumHint(width, hint.key, hint.action))
	}
	return strings.Join(rows, "\n")
}

func (f *GoalForm) renderMinimumHint(width int, keyText, action string) string {
	line := f.styles.FocusIndicator.Render(f.staticChrome(keyText))
	if action != "" {
		line += " " + f.styles.OverlayDim.Render(f.staticChrome(action))
	}
	return truncateDisplayWithGlyphProfile(line, width, f.glyphProfile)
}

func (f *GoalForm) staticChrome(value string) string {
	if f != nil && f.glyphProfile == GlyphASCII {
		return strings.NewReplacer("⇧", "shift+", " · ", " - ").Replace(value)
	}
	return value
}

func (f *GoalForm) renderFrame(content, footer string) string {
	return renderInlineFormFrame(f.styles, content, footer, f.width, f.glyphProfile)
}
