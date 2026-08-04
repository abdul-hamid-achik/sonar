package ui

import (
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
)

const completionFilterPrompt = "Filter › "

func completionFilterPromptForGlyphProfile(profile GlyphProfile) string {
	if resolveGlyphProfile(profile) == GlyphASCII {
		return "Filter > "
	}
	return completionFilterPrompt
}

func completionFilterInputWidth(terminalWidth int) int {
	return max(1, pickerListWidth(terminalWidth)-lipgloss.Width(completionFilterPrompt))
}

// semanticTextInputStyles keeps custom Bubbles inputs in the same adaptive
// palette as lists, the composer, and ToolCards. Starting from Bubbles keeps
// its cursor behavior while replacing every hardcoded default color.
func semanticTextInputStyles(isDark bool, themeID string, reducedMotion ...bool) textinput.Styles {
	styles := textinput.DefaultStyles(isDark)
	palette := outputSemanticPalette(isDark, themeID)
	styles.Focused = textinput.StyleState{
		Text:        lipgloss.NewStyle().Foreground(palette.Text),
		Placeholder: lipgloss.NewStyle().Foreground(palette.Dim),
		Suggestion:  lipgloss.NewStyle().Foreground(palette.Dim),
		Prompt:      lipgloss.NewStyle().Foreground(palette.Accent).Bold(true),
	}
	styles.Blurred = textinput.StyleState{
		Text:        lipgloss.NewStyle().Foreground(palette.Muted),
		Placeholder: lipgloss.NewStyle().Foreground(palette.Dim),
		Suggestion:  lipgloss.NewStyle().Foreground(palette.Dim),
		Prompt:      lipgloss.NewStyle().Foreground(palette.Dim),
	}
	styles.Cursor.Color = palette.Accent
	styles.Cursor.Blink = len(reducedMotion) == 0 || !reducedMotion[0]
	return styles
}

// newPickerDelegate gives every Bubbles picker the same density, focus mark,
// and semantic colors. Individual pickers supply data; this is the shared
// visual grammar.
func newPickerDelegate(isDark, compact bool, themeID string, profiles ...GlyphProfile) list.DefaultDelegate {
	palette := outputSemanticPalette(isDark, themeID)
	profile := resolveGlyphProfile(profiles...)
	selectionBorder := lipgloss.NormalBorder()
	if profile == GlyphASCII {
		selectionBorder = borderForGlyphProfile(profile)
	}
	delegate := list.NewDefaultDelegate()
	delegate.SetSpacing(0)
	delegate.Styles.NormalTitle = lipgloss.NewStyle().
		Foreground(palette.Text).
		PaddingLeft(2)
	delegate.Styles.NormalDesc = lipgloss.NewStyle().
		Foreground(palette.Dim).
		PaddingLeft(2)
	delegate.Styles.SelectedTitle = lipgloss.NewStyle().
		Border(selectionBorder, false, false, false, true).
		BorderForeground(palette.Accent).
		Foreground(palette.Accent).
		Bold(true).
		PaddingLeft(1)
	delegate.Styles.SelectedDesc = lipgloss.NewStyle().
		Border(selectionBorder, false, false, false, true).
		BorderForeground(palette.Accent).
		Foreground(palette.Muted).
		PaddingLeft(1)
	delegate.Styles.DimmedTitle = lipgloss.NewStyle().
		Foreground(palette.Dim).
		PaddingLeft(2)
	delegate.Styles.DimmedDesc = lipgloss.NewStyle().
		Foreground(palette.Dim).
		PaddingLeft(2)
	delegate.Styles.FilterMatch = lipgloss.NewStyle().
		Foreground(palette.Accent).
		Underline(true)
	if compact {
		delegate.ShowDescription = false
		delegate.SetHeight(1)
	}
	return delegate
}

func configurePickerList(l *list.Model, isDark bool, themeID string, reducedMotion ...bool) {
	palette := outputSemanticPalette(isDark, themeID)
	l.Styles.TitleBar = lipgloss.NewStyle().Padding(0, 0, 1, 0)
	l.Styles.Title = lipgloss.NewStyle().
		Foreground(palette.Accent).
		Bold(true)
	l.Styles.Spinner = lipgloss.NewStyle().Foreground(palette.Accent)
	l.Styles.DefaultFilterCharacterMatch = lipgloss.NewStyle().
		Foreground(palette.Accent).
		Underline(true)
	l.Styles.StatusBar = lipgloss.NewStyle().
		Foreground(palette.Dim).
		Padding(0, 0, 1, 2)
	l.Styles.StatusEmpty = lipgloss.NewStyle().Foreground(palette.Dim)
	l.Styles.StatusBarActiveFilter = lipgloss.NewStyle().Foreground(palette.Text)
	l.Styles.StatusBarFilterCount = lipgloss.NewStyle().Foreground(palette.Dim)
	l.Styles.NoItems = lipgloss.NewStyle().Foreground(palette.Dim)
	l.Styles.PaginationStyle = lipgloss.NewStyle().PaddingLeft(2)
	l.Styles.HelpStyle = lipgloss.NewStyle().Foreground(palette.Dim).Padding(1, 0, 0, 2)
	l.Styles.ActivePaginationDot = lipgloss.NewStyle().Foreground(palette.Accent).SetString("•")
	l.Styles.InactivePaginationDot = lipgloss.NewStyle().Foreground(palette.Border).SetString("•")
	l.Styles.ArabicPagination = lipgloss.NewStyle().Foreground(palette.Dim)
	l.Styles.DividerDot = lipgloss.NewStyle().Foreground(palette.Border).SetString(" · ")

	filterStyles := l.FilterInput.Styles()
	filterStyles.Focused.Text = lipgloss.NewStyle().Foreground(palette.Text)
	filterStyles.Focused.Placeholder = lipgloss.NewStyle().Foreground(palette.Dim)
	filterStyles.Focused.Suggestion = lipgloss.NewStyle().Foreground(palette.Dim)
	filterStyles.Focused.Prompt = lipgloss.NewStyle().Foreground(palette.Accent).Bold(true)
	filterStyles.Blurred = filterStyles.Focused
	filterStyles.Cursor.Color = palette.Accent
	filterStyles.Cursor.Blink = len(reducedMotion) == 0 || !reducedMotion[0]
	l.FilterInput.SetStyles(filterStyles)
	l.FilterInput.Prompt = "Filter › "
	l.FilterInput.Placeholder = "type to narrow"
	configurePickerListGlyphProfile(l, GlyphUnicode)
}

// configurePickerListGlyphProfile swaps only terminal-capability-sensitive
// chrome. Color and motion stay independent presentation axes.
func configurePickerListGlyphProfile(l *list.Model, profile GlyphProfile) {
	if l == nil {
		return
	}
	if resolveGlyphProfile(profile) != GlyphASCII {
		l.Styles.ActivePaginationDot = l.Styles.ActivePaginationDot.SetString("•")
		l.Styles.InactivePaginationDot = l.Styles.InactivePaginationDot.SetString("•")
		l.Styles.DividerDot = l.Styles.DividerDot.SetString(" · ")
		l.FilterInput.Prompt = "Filter › "
		return
	}
	glyphs := glyphSet(GlyphASCII)
	l.Styles.ActivePaginationDot = l.Styles.ActivePaginationDot.SetString(glyphs.Selected)
	l.Styles.InactivePaginationDot = l.Styles.InactivePaginationDot.SetString(glyphs.Unselected)
	l.Styles.DividerDot = l.Styles.DividerDot.SetString(" - ")
	l.FilterInput.Prompt = "Filter > "
}

func (m *Model) restylePickerOverlays() {
	if state := m.settingsPickerState; state != nil {
		delegate := newPickerDelegate(m.isDark, state.Compact, m.themeID, m.glyphProfile)
		state.List.SetDelegate(delegate)
		state.ItemHeight = delegate.Height()
		configurePickerList(&state.List, m.isDark, m.themeID, m.reducedMotion)
		configurePickerListGlyphProfile(&state.List, m.glyphProfile)
		setSettingsTitleDensity(&state.List, state.Compact)
	}
	if state := m.permissionsPanelState; state != nil {
		delegate := newPickerDelegate(m.isDark, state.Compact, m.themeID, m.glyphProfile)
		state.List.SetDelegate(delegate)
		state.ItemHeight = delegate.Height()
		configurePickerList(&state.List, m.isDark, m.themeID, m.reducedMotion)
		configurePickerListGlyphProfile(&state.List, m.glyphProfile)
		setSettingsTitleDensity(&state.List, state.Compact)
	}
	if state := m.agentPickerState; state != nil {
		state.List.SetDelegate(newPickerDelegate(m.isDark, false, m.themeID, m.glyphProfile))
		configurePickerList(&state.List, m.isDark, m.themeID, m.reducedMotion)
		configurePickerListGlyphProfile(&state.List, m.glyphProfile)
	}
	if state := m.providerPickerState; state != nil {
		delegate := newPickerDelegate(m.isDark, false, m.themeID, m.glyphProfile)
		state.List.SetDelegate(delegate)
		state.ItemHeight = delegate.Height()
		state.ItemSpacing = delegate.Spacing()
		configurePickerList(&state.List, m.isDark, m.themeID, m.reducedMotion)
		configurePickerListGlyphProfile(&state.List, m.glyphProfile)
	}
	if state := m.modePickerState; state != nil {
		state.List.SetDelegate(newPickerDelegate(m.isDark, false, m.themeID, m.glyphProfile))
		configurePickerList(&state.List, m.isDark, m.themeID, m.reducedMotion)
		configurePickerListGlyphProfile(&state.List, m.glyphProfile)
	}
	if state := m.themePickerState; state != nil {
		delegate := newPickerDelegate(m.isDark, false, m.themeID, m.glyphProfile)
		state.List.SetDelegate(delegate)
		state.ItemHeight = delegate.Height()
		configurePickerList(&state.List, m.isDark, m.themeID, m.reducedMotion)
		configurePickerListGlyphProfile(&state.List, m.glyphProfile)
	}
	if state := m.modelPickerState; state != nil {
		// Model metadata has a dedicated selected-detail strip. Keep its
		// navigable rows single-line when colors/glyphs are restyled.
		delegate := newPickerDelegate(m.isDark, true, m.themeID, m.glyphProfile)
		state.List.SetDelegate(delegate)
		state.ItemHeight = delegate.Height()
		configurePickerList(&state.List, m.isDark, m.themeID, m.reducedMotion)
		configurePickerListGlyphProfile(&state.List, m.glyphProfile)
		setSettingsTitleDensity(&state.List, state.Compact)
	}
	if state := m.sessionsPickerState; state != nil && state.ready() {
		state.List.SetDelegate(newPickerDelegate(m.isDark, false, m.themeID, m.glyphProfile))
		configurePickerList(&state.List, m.isDark, m.themeID, m.reducedMotion)
		configurePickerListGlyphProfile(&state.List, m.glyphProfile)
	}
	if state := m.completionState; state != nil {
		state.Filter.SetStyles(semanticTextInputStyles(m.isDark, m.themeID, m.reducedMotion))
	}
	if state := m.planFormState; state != nil {
		for index := range state.Fields {
			if state.Fields[index].Kind == "text" {
				state.Fields[index].Input.SetStyles(semanticTextInputStyles(m.isDark, m.themeID, m.reducedMotion))
			}
		}
	}
}
