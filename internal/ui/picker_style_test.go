package ui

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
)

type pickerStyleItem string

func (i pickerStyleItem) Title() string       { return string(i) }
func (i pickerStyleItem) Description() string { return "description" }
func (i pickerStyleItem) FilterValue() string { return string(i) }

func TestPickerListUsesSharedSemanticPalette(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	for _, isDark := range []bool{false, true} {
		delegate := newPickerDelegate(isDark, false, defaultThemeID)
		l := list.New([]list.Item{pickerStyleItem("first")}, delegate, 40, 8)
		configurePickerList(&l, isDark, defaultThemeID)

		palette := newSemanticPalette(isDark, defaultThemeID)
		assertSameColor(t, "title", l.Styles.Title.GetForeground(), palette.Accent)
		assertSameColor(t, "selected title", delegate.Styles.SelectedTitle.GetForeground(), palette.Accent)
		assertSameColor(t, "selected description", delegate.Styles.SelectedDesc.GetForeground(), palette.Muted)
		if got := l.FilterInput.Prompt; got != "Filter › " {
			t.Fatalf("filter prompt = %q", got)
		}
	}
}

func TestNoColorAppliesToToolCardsAndPickers(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })

	tool := NewToolCardStyles(true, defaultThemeID).TitleRunning.Render("tool")
	composer := agentTextareaStyles(true, defaultThemeID).Focused.Prompt.Render("❯")
	delegate := newPickerDelegate(true, false, defaultThemeID)
	l := list.New([]list.Item{pickerStyleItem("first")}, delegate, 40, 8)
	configurePickerList(&l, true, defaultThemeID)
	l.SetShowStatusBar(false)
	l.SetShowPagination(false)
	l.SetShowHelp(false)
	picker := l.View()
	plan := NewPlanFormState("polish the interface", defaultThemeID)
	planInput := plan.Fields[0].Input.View()
	completion := newCompletionState("command", []Completion{{Label: "/help"}}, false, defaultThemeID)
	completionInput := completion.Filter.View()
	if hasANSIColor(tool) || hasANSIColor(composer) || hasANSIColor(picker) || hasANSIColor(planInput) || hasANSIColor(completionInput) {
		t.Fatalf("NO_COLOR rendering emitted ANSI color sequences: tool=%q composer=%q picker=%q plan=%q completion=%q", tool, composer, picker, planInput, completionInput)
	}
}

func TestTransientInputsAndCompactDensitySurviveThemeChange(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(*Model)
	m.openSettingsPicker()
	m.planFormState = NewPlanFormState("polish the interface", defaultThemeID)
	m.completionState = newCompletionState("command", []Completion{{Label: "/help"}}, false, defaultThemeID, true)

	updated, _ = m.Update(tea.BackgroundColorMsg{Color: color.White})
	m = updated.(*Model)
	palette := newSemanticPalette(false, defaultThemeID)
	assertSameColor(t, "plan text", m.planFormState.Fields[0].Input.Styles().Focused.Text.GetForeground(), palette.Text)
	assertSameColor(t, "plan placeholder", m.planFormState.Fields[0].Input.Styles().Focused.Placeholder.GetForeground(), palette.Dim)
	assertSameColor(t, "completion text", m.completionState.Filter.Styles().Focused.Text.GetForeground(), palette.Text)
	assertSameColor(t, "completion cursor", m.completionState.Filter.Styles().Cursor.Color, palette.Accent)

	rendered := m.renderSettingsPicker()
	for _, label := range []string{"Model", "Provider", "Agent profile", "Mode", "Sessions", "Compact layout", "Runtime status"} {
		if !strings.Contains(rendered, label) {
			t.Fatalf("theme change hid compact setting %q:\n%s", label, rendered)
		}
	}
	// Permissions/Help may sit one scroll step below at min height.
	m.settingsPickerState.List.Select(int(settingsPermissions))
	if !strings.Contains(m.renderSettingsPicker(), "Permissions") {
		t.Fatalf("Permissions setting missing after scroll:\n%s", m.renderSettingsPicker())
	}
	m.settingsPickerState.List.Select(int(settingsHelp))
	if !strings.Contains(m.renderSettingsPicker(), "Help") {
		t.Fatalf("Help setting missing after scroll:\n%s", m.renderSettingsPicker())
	}
	assertRenderedLinesFit(t, rendered, minTerminalWidth)
	assertRenderedHeightFits(t, rendered, minTerminalHeight)
}

func TestTransientInputReducedMotionSurvivesThemeChange(t *testing.T) {
	m := newTestModel(t)
	m.reducedMotion = true
	m.planFormState = NewPlanFormState("polish the interface", m.themeID, m.isDark, m.reducedMotion)
	m.completionState = newCompletionState(
		"command", []Completion{{Label: "/help"}}, false, m.themeID, m.isDark, m.reducedMotion,
	)
	m.modelPickerState = newOllamaModelPickerState([]OllamaModelDescriptor{{
		Name: "local-code", Source: OllamaModelLocal, Selectable: true, Fit: true,
	}}, "local-code", m.width, m.height, m.isDark, m.themeID, m.reducedMotion)
	m.sessionsPickerState = newSessionsPickerState([]SessionListItem{{
		ID: 1, Title: "Reduced motion", CreatedAt: "just now",
	}}, m.width, m.height, m.isDark, m.themeID, m.reducedMotion)
	if m.modelPickerState.List.FilterInput.Styles().Cursor.Blink || m.sessionsPickerState.List.FilterInput.Styles().Cursor.Blink {
		t.Fatal("reduced motion left a picker filter cursor blinking")
	}

	updated, _ := m.Update(tea.BackgroundColorMsg{Color: color.White})
	m = updated.(*Model)
	if m.completionState.Filter.Styles().Cursor.Blink {
		t.Fatal("theme change re-enabled the reduced-motion completion cursor")
	}
	for _, field := range m.planFormState.Fields {
		if field.Kind == "text" && field.Input.Styles().Cursor.Blink {
			t.Fatalf("theme change re-enabled the reduced-motion Plan cursor for %q", field.Label)
		}
	}
	if m.modelPickerState.List.FilterInput.Styles().Cursor.Blink || m.sessionsPickerState.List.FilterInput.Styles().Cursor.Blink {
		t.Fatal("theme change re-enabled a reduced-motion picker filter cursor")
	}
}

func hasANSIColor(value string) bool {
	return strings.Contains(value, "\x1b[38;") || strings.Contains(value, "\x1b[48;")
}

func assertSameColor(t *testing.T, name string, got, want interface {
	RGBA() (uint32, uint32, uint32, uint32)
}) {
	t.Helper()
	gotR, gotG, gotB, gotA := got.RGBA()
	wantR, wantG, wantB, wantA := want.RGBA()
	if gotR != wantR || gotG != wantG || gotB != wantB || gotA != wantA {
		t.Fatalf("%s color = %#v, want %#v", name, got, want)
	}
}
