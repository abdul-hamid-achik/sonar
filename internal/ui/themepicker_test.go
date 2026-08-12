package ui

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

func TestThemePickerLivePreviewRepaintsWholeFrameAndReverts(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	m := newTestModel(t)
	m.entries = []ChatEntry{
		{Kind: "user", Content: "hey"},
		{Kind: "assistant", Content: "an answer", RenderedContent: "an answer"},
	}
	m.openThemePicker()
	m.refreshTranscript()

	// Navigate to Dracula through the real key path so the live-apply hook runs.
	for range m.themePickerState.List.Items() {
		updated, _ := m.Update(downKey())
		m = updated.(*Model)
		if m.themePickerState.SelectedThemeID() == "dracula" {
			break
		}
	}
	if m.themePickerState.SelectedThemeID() != "dracula" {
		t.Fatal("could not navigate to dracula")
	}
	// Live preview: the active theme (and therefore the whole frame behind the
	// picker) follows the cursor without persisting anything.
	if got := m.ThemeID(); got != "dracula" {
		t.Fatalf("live preview did not apply the highlighted theme: ThemeID=%q", got)
	}
	preview := m.renderThemePicker()
	if !strings.Contains(preview, "48;2;40;42;54") {
		t.Fatalf("Dracula preview background is absent: %q", preview)
	}

	// Escape reverts to the committed theme without persisting.
	updated, _ := m.Update(escKey())
	m = updated.(*Model)
	if got := m.ThemeID(); got != defaultThemeID {
		t.Fatalf("Escape did not revert the previewed theme: ThemeID=%q", got)
	}
	if m.themePickerState != nil {
		t.Fatal("Escape left the theme picker open")
	}
}

func TestThemePickerLivePreviewRevertsToNonDefaultBase(t *testing.T) {
	m := newTestModel(t)
	if !m.SetTheme("gruvbox") {
		t.Fatal("SetTheme rejected gruvbox")
	}
	m.openThemePicker()
	if m.themePickerBase != "gruvbox" {
		t.Fatalf("picker base = %q, want gruvbox", m.themePickerBase)
	}
	// The picker opens on the committed theme (gruvbox is index 4 of the
	// alphabetical tail after the default). One down reaches kanagawa.
	updated, _ := m.Update(downKey())
	m = updated.(*Model)
	if got := m.ThemeID(); got != "kanagawa" {
		t.Fatalf("live preview did not apply kanagawa: ThemeID=%q", got)
	}
	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if got := m.ThemeID(); got != "gruvbox" {
		t.Fatalf("Escape did not restore the committed theme: ThemeID=%q", got)
	}
}

func TestThemePickerEnterAppliesPreviewedTheme(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	m := newTestModel(t)
	m.openThemePicker()
	selectThemePickerItem(t, m.themePickerState, "dracula")
	updated, _ := m.Update(enterKey())
	m = updated.(*Model)
	if got := m.ThemeID(); got != "dracula" {
		t.Fatalf("Enter applied theme %q, want dracula", got)
	}
	if m.themePickerState != nil {
		t.Fatal("Enter left the theme picker open")
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
		return
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
