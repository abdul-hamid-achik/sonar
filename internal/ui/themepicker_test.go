package ui

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestViewUsesSelectedThemeBackgroundInBothAppearances(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	m := newTestModel(t)
	assertViewBackground(t, m.View(), newSemanticPalette(true, defaultThemeID).Background)

	if !m.SetTheme("dracula") {
		t.Fatal("SetTheme rejected dracula")
	}
	assertViewBackground(t, m.View(), newSemanticPalette(true, "dracula").Background)

	updated, _ := m.Update(tea.BackgroundColorMsg{Color: color.White})
	m = updated.(*Model)
	assertViewBackground(t, m.View(), newSemanticPalette(false, "dracula").Background)
}

func TestViewLeavesTerminalBackgroundAloneWithNoColor(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })

	if background := newTestModel(t).View().BackgroundColor; background != nil {
		t.Fatalf("NO_COLOR view background = %#v, want terminal default", background)
	}
}

func TestThemePickerPreviewTracksHighlightWithoutApplying(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	m := newTestModel(t)
	m.openThemePicker()
	selectThemePickerItem(t, m.themePickerState, "dracula")

	preview := m.renderThemePicker()
	if !strings.Contains(ansi.Strip(preview), "Preview · Dracula") {
		t.Fatalf("highlighted theme preview is absent:\n%s", ansi.Strip(preview))
	}
	// Dracula's #282A36 surface must be painted inside the preview itself;
	// Bubble Tea's view-level background is still Nord until Enter is pressed.
	if !strings.Contains(preview, "48;2;40;42;54") {
		t.Fatalf("Dracula preview background is absent: %q", preview)
	}
	if got := m.ThemeID(); got != defaultThemeID {
		t.Fatalf("preview changed active theme to %q before confirmation", got)
	}

	updated, _ := m.Update(escKey())
	m = updated.(*Model)
	if got := m.ThemeID(); got != defaultThemeID {
		t.Fatalf("Escape applied previewed theme %q", got)
	}

	m.openThemePicker()
	selectThemePickerItem(t, m.themePickerState, "dracula")
	updated, _ = m.Update(enterKey())
	m = updated.(*Model)
	if got := m.ThemeID(); got != "dracula" {
		t.Fatalf("Enter applied theme %q, want dracula", got)
	}
}

func TestThemePickerPreviewFitsMinimumTerminal(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(*Model)
	m.openThemePicker()
	rendered := m.renderThemePicker()
	assertRenderedLinesFit(t, rendered, minTerminalWidth)
	assertRenderedHeightFits(t, rendered, minTerminalHeight)
}

func selectThemePickerItem(t *testing.T, state *ThemePickerState, id string) {
	t.Helper()
	if state == nil {
		t.Fatal("theme picker is not open")
	}
	for index, item := range state.List.Items() {
		entry, ok := item.(themeItem)
		if ok && entry.id == id {
			state.List.Select(index)
			return
		}
	}
	t.Fatalf("theme picker does not contain %q", id)
}

func assertViewBackground(t *testing.T, view tea.View, want color.Color) {
	t.Helper()
	if view.BackgroundColor == nil {
		t.Fatal("view background is nil")
	}
	assertSameColor(t, "view background", view.BackgroundColor, want)
}
