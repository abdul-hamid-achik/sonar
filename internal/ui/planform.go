package ui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// PlanFormField represents a single field in the plan form.
type PlanFormField struct {
	Label       string
	Kind        string   // "text" or "select"
	Value       string   // current value (for select, set from Options[OptionIndex])
	Options     []string // for "select" kind
	OptionIndex int      // for "select" kind
	Input       textinput.Model
}

// PlanFormState holds state for the composer-owned plan form.
type PlanFormState struct {
	Fields      []PlanFormField
	ActiveField int
	errorText   string
}

const planTaskCharLimit = 32 * 1024

// NewPlanFormState creates a plan form pre-filled with the user's task description.
// Presentation options are ordered as theme-dark, then reduced-motion so older
// callers that only select a theme remain source compatible.
func NewPlanFormState(task string, themeID string, presentation ...bool) *PlanFormState {
	isDark := true
	reducedMotion := false
	if len(presentation) > 0 {
		isDark = presentation[0]
	}
	if len(presentation) > 1 {
		reducedMotion = presentation[1]
	}
	taskInput := textinput.New()
	taskInput.SetStyles(semanticTextInputStyles(isDark, themeID, reducedMotion))
	taskInput.Placeholder = "Describe the task..."
	taskInput.Prompt = ""
	// Match the main composer bound so `/plan <task>` remains lossless when it
	// moves into the reviewed form. Focus remains intentionally shorter.
	taskInput.CharLimit = planTaskCharLimit
	taskInput.SetValue(task)
	taskInput.Focus()

	focusInput := textinput.New()
	focusInput.SetStyles(semanticTextInputStyles(isDark, themeID, reducedMotion))
	focusInput.Placeholder = "Any constraints or requirements? (optional)"
	focusInput.Prompt = ""
	focusInput.CharLimit = 512

	return &PlanFormState{
		Fields: []PlanFormField{
			{
				Label: "Task",
				Kind:  "text",
				Input: taskInput,
			},
			{
				Label:   "Scope",
				Kind:    "select",
				Options: []string{"single file", "module", "project-wide"},
			},
			{
				Label: "Focus (optional)",
				Kind:  "text",
				Input: focusInput,
			},
		},
		ActiveField: 0,
	}
}

// AssemblePrompt builds the structured prompt from form fields.
func (pf *PlanFormState) AssemblePrompt() string {
	task := pf.Fields[0].Input.Value()
	scope := pf.Fields[1].Options[pf.Fields[1].OptionIndex]
	focus := pf.Fields[2].Input.Value()

	var b strings.Builder
	b.WriteString("Plan the following task:\n")
	fmt.Fprintf(&b, "Task: %s\n", task)
	fmt.Fprintf(&b, "Scope: %s\n", scope)
	if focus != "" {
		fmt.Fprintf(&b, "Focus: %s\n", focus)
	}
	b.WriteString("\nReturn an implementation-ready plan with these sections:\n")
	b.WriteString("- Objective and constraints\n")
	b.WriteString("- Assumptions and open questions\n")
	b.WriteString("- Affected files or components\n")
	b.WriteString("- Ordered steps and dependencies\n")
	b.WriteString("- Risks and mitigations\n")
	b.WriteString("- Acceptance criteria\n")
	b.WriteString("- Verification commands or checks\n")
	b.WriteString("Do not modify files or run mutating tools.")
	return b.String()
}

func (pf *PlanFormState) taskValid() bool {
	if strings.TrimSpace(pf.Fields[0].Input.Value()) != "" {
		pf.errorText = ""
		return true
	}
	pf.errorText = "Task is required"
	return false
}

// updatePlanForm handles key events within the inline plan form.
// Returns the updated model, any command, and whether the form was submitted or cancelled.
func (m *Model) updatePlanForm(msg tea.KeyPressMsg) (bool, bool) {
	pf := m.planFormState
	if pf == nil {
		return false, false
	}

	field := &pf.Fields[pf.ActiveField]

	switch {
	case key.Matches(msg, m.keys.Cancel):
		// Cancel
		return false, true

	case msg.Code == tea.KeyEnter:
		if pf.ActiveField == 0 && !pf.taskValid() {
			return false, false
		}
		if pf.ActiveField == len(pf.Fields)-1 {
			if !pf.taskValid() {
				m.advancePlanFormField(-pf.ActiveField)
				return false, false
			}
			// Submit
			return true, false
		}
		// Advance to next field
		m.advancePlanFormField(1)
		return false, false

	case msg.Code == tea.KeyTab:
		if msg.Mod == tea.ModShift {
			m.advancePlanFormField(-1)
		} else {
			m.advancePlanFormField(1)
		}
		return false, false

	case msg.Code == tea.KeyUp:
		if field.Kind == "select" {
			if field.OptionIndex > 0 {
				field.OptionIndex--
			}
			return false, false
		}

	case msg.Code == tea.KeyDown:
		if field.Kind == "select" {
			if field.OptionIndex < len(field.Options)-1 {
				field.OptionIndex++
			}
			return false, false
		}

	case msg.Code == tea.KeyLeft:
		if field.Kind == "select" {
			if field.OptionIndex > 0 {
				field.OptionIndex--
			}
			return false, false
		}

	case msg.Code == tea.KeyRight:
		if field.Kind == "select" {
			if field.OptionIndex < len(field.Options)-1 {
				field.OptionIndex++
			}
			return false, false
		}
	}

	// Forward other keys to active text field
	if field.Kind == "text" {
		field.Input, _ = field.Input.Update(msg)
		if pf.ActiveField == 0 && strings.TrimSpace(field.Input.Value()) != "" {
			pf.errorText = ""
		}
	}

	return false, false
}

// advancePlanFormField moves to the next or previous field.
func (m *Model) advancePlanFormField(dir int) {
	pf := m.planFormState
	if pf == nil {
		return
	}
	anchor := m.captureInlineFormTranscriptAnchor()
	defer m.refreshInlineFormLayout(anchor)

	// Blur current field
	current := &pf.Fields[pf.ActiveField]
	if current.Kind == "text" {
		current.Input.Blur()
	}

	pf.ActiveField += dir
	if pf.ActiveField < 0 {
		pf.ActiveField = 0
	}
	if pf.ActiveField >= len(pf.Fields) {
		pf.ActiveField = len(pf.Fields) - 1
	}

	// Focus new field
	next := &pf.Fields[pf.ActiveField]
	if next.Kind == "text" {
		next.Input.Focus()
	}
}

func compactPlanForm(width, height int) bool {
	return width <= 40 || height <= 20
}

func (m *Model) renderPlanTextFieldView(field PlanFormField, active bool, width int) (string, *tea.Cursor) {
	valueWidth := max(1, width-2)
	if active {
		// Render a sized copy so View remains pure while the parent continues to
		// own the live Bubbles input and all message handling.
		input := field.Input
		input.SetWidth(valueWidth)
		input.SetVirtualCursor(false)
		return m.styles.FocusIndicator.Render("> ") + input.View(),
			offsetCursor(input.Cursor(), lipgloss.Width("> "), 0)
	}

	value := strings.TrimSpace(field.Input.Value())
	if value == "" {
		value = "(empty)"
	}
	return "  " + m.styles.OverlayDim.Render(truncateDisplay(value, valueWidth)), nil
}

func (m *Model) renderPlanSelectField(field PlanFormField, active, compact bool, width int) string {
	if len(field.Options) == 0 {
		return m.styles.OverlayDim.Render("(no choices)")
	}
	selected := min(max(0, field.OptionIndex), len(field.Options)-1)
	glyphs := glyphSet(m.glyphProfile)
	if compact {
		control := glyphs.Left + " " + field.Options[selected] + " " + glyphs.Right
		return m.styles.FocusIndicator.Render(truncateDisplayWithGlyphProfile(control, width, m.glyphProfile))
	}

	lines := make([]string, 0, len(field.Options))
	for i, option := range field.Options {
		prefix := "  "
		style := m.styles.OverlayDim
		if i == selected {
			prefix = glyphs.Selected + " "
			if active {
				prefix = glyphs.Collapsed + " "
				style = m.styles.FocusIndicator
			}
		}
		lines = append(lines, style.Render(prefix+truncateDisplay(option, max(1, width-2))))
	}
	return strings.Join(lines, "\n")
}

func planFormFooter(pf *PlanFormState, width int, profiles ...GlyphProfile) string {
	if pf == nil || len(pf.Fields) == 0 {
		return "esc cancel"
	}
	active := min(max(0, pf.ActiveField), len(pf.Fields)-1)
	field := pf.Fields[active]
	last := active == len(pf.Fields)-1
	profile := resolveGlyphProfile(profiles...)
	horizontal := "←/→"
	horizontalCompact := "←→"
	separator := " · "
	if profile == GlyphASCII {
		horizontal = "left/right"
		horizontalCompact = "left/right"
		separator = " - "
	}

	if width >= 42 {
		switch {
		case last:
			return strings.Join([]string{"esc cancel", "enter submit", "shift+tab back"}, separator)
		case field.Kind == "select":
			return strings.Join([]string{"esc cancel", "enter/tab next", horizontal + " choose"}, separator)
		default:
			return strings.Join([]string{"esc cancel", "enter/tab next"}, separator)
		}
	}
	if width >= 24 {
		switch {
		case last:
			return "esc cancel\n" + strings.Join([]string{"enter submit", "shift+tab back"}, separator)
		case field.Kind == "select":
			return "esc cancel\n" + strings.Join([]string{"enter next", horizontal + " choose"}, separator)
		default:
			return strings.Join([]string{"esc cancel", "enter next"}, separator)
		}
	}

	switch {
	case last:
		return "esc cancel\nenter submit\nshift+tab back"
	case field.Kind == "select":
		return "esc cancel\nenter next\n" + horizontalCompact + " choose"
	default:
		return "esc cancel\nenter next"
	}
}

func (m *Model) renderCompactPlanFormView(pf *PlanFormState, contentWidth int) (string, *tea.Cursor) {
	active := min(max(0, pf.ActiveField), len(pf.Fields)-1)
	field := pf.Fields[active]

	var b strings.Builder
	b.WriteString(m.styles.OverlayTitle.Render(fmt.Sprintf("Plan · %d/%d", active+1, len(pf.Fields))))
	b.WriteString("\n")
	b.WriteString(m.styles.FocusIndicator.Render(field.Label))
	b.WriteString("\n")
	controlY := strings.Count(b.String(), "\n")
	var cursor *tea.Cursor
	if field.Kind == "select" {
		b.WriteString(m.renderPlanSelectField(field, true, true, contentWidth))
	} else {
		var fieldView string
		fieldView, cursor = m.renderPlanTextFieldView(field, true, contentWidth)
		b.WriteString(fieldView)
		cursor = offsetCursor(cursor, 0, controlY)
	}
	if pf.errorText != "" {
		b.WriteString("\n")
		b.WriteString(m.styles.ErrorText.UnsetPaddingLeft().Render(truncateDisplay("! "+pf.errorText, max(1, contentWidth-2))))
	}

	return renderInlineFormFrame(m.styles, b.String(), planFormFooter(pf, contentWidth, m.glyphProfile), m.width, m.glyphProfile), pickerFrameCursor(cursor)
}

// renderPlanForm renders a responsive parent-owned form. Compact terminals show
// one active step; normal terminals retain the complete form without spending
// rows on decorative whitespace.
func (m *Model) renderPlanForm() string {
	view, _ := m.renderPlanFormView()
	return view
}

func (m *Model) renderPlanFormView() (string, *tea.Cursor) {
	pf := m.planFormState
	if pf == nil || len(pf.Fields) == 0 {
		return "", nil
	}

	contentWidth := inlineFormContentWidth(m.width)
	if compactPlanForm(m.width, m.height) {
		return m.renderCompactPlanFormView(pf, contentWidth)
	}

	var b strings.Builder
	var cursor *tea.Cursor
	b.WriteString(m.styles.OverlayTitle.Render("Plan task"))
	b.WriteString("\n\n")
	for i, field := range pf.Fields {
		active := i == pf.ActiveField
		labelStyle := m.styles.OverlayAccent
		if active {
			labelStyle = m.styles.FocusIndicator
		}
		b.WriteString(labelStyle.Render(field.Label))
		b.WriteString("\n")
		controlY := strings.Count(b.String(), "\n")
		if field.Kind == "select" {
			b.WriteString(m.renderPlanSelectField(field, active, false, contentWidth))
		} else {
			fieldView, fieldCursor := m.renderPlanTextFieldView(field, active, contentWidth)
			b.WriteString(fieldView)
			if active {
				cursor = offsetCursor(fieldCursor, 0, controlY)
			}
		}
		if i == 0 && pf.errorText != "" {
			b.WriteString("\n")
			b.WriteString(m.styles.ErrorText.UnsetPaddingLeft().Render(truncateDisplay("! "+pf.errorText, max(1, contentWidth-2))))
		}
		if i < len(pf.Fields)-1 {
			b.WriteString("\n\n")
		}
	}

	return renderInlineFormFrame(m.styles, b.String(), planFormFooter(pf, contentWidth, m.glyphProfile), m.width, m.glyphProfile), pickerFrameCursor(cursor)
}

// openPlanForm gives the inline Plan form temporary composer ownership.
func (m *Model) openPlanForm(task string) {
	anchor := m.captureInlineFormTranscriptAnchor()
	if !m.prepareInlineFormOpen() {
		return
	}
	m.planFormState = NewPlanFormState(task, m.themeID, m.isDark, m.reducedMotion)
	m.restylePickerOverlays()
	m.overlay = OverlayPlanForm
	m.input.Blur()
	m.refreshInlineFormLayout(anchor)
}

// closePlanForm releases composer ownership without changing its saved draft.
func (m *Model) closePlanForm() {
	anchor := m.captureInlineFormTranscriptAnchor()
	m.planFormState = nil
	if m.overlay == OverlayPlanForm {
		m.overlay = OverlayNone
	}
	if m.composerEditable() {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	m.refreshInlineFormLayout(anchor)
}

// submitPlanFormPrompt sends the assembled plan prompt to the agent.
func (m *Model) submitPlanFormPrompt(prompt string) tea.Cmd {
	// The form is a PLAN authority boundary, not a prompt template that can be
	// dispatched under whichever conversational preset happens to be active.
	// Reassert the read-only contract defensively before every submission,
	// including legacy PlanFormCompletedMsg callers.
	m.setMode(ModePlan)
	return m.sendToAgent(prompt)
}
