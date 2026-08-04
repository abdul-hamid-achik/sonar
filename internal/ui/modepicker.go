package ui

import (
	"charm.land/bubbles/v2/list"
)

type modeItem struct {
	mode        Mode
	title       string
	description string
	current     bool
	profile     GlyphProfile
}

func (i modeItem) Title() string {
	if i.current {
		return i.title + "  " + glyphSet(i.profile).Success
	}
	return i.title
}

func (i modeItem) Description() string { return i.description }
func (i modeItem) FilterValue() string { return i.title }

type ModePickerState struct {
	List list.Model
}

func newModePickerState(current Mode, terminalWidth, terminalHeight int, isDark bool, themeID string, profiles ...GlyphProfile) *ModePickerState {
	profile := resolveGlyphProfile(profiles...)
	definitions := []modeItem{
		{mode: ModeNormal, title: "NORMAL", description: "Interactive work with approval-gated changes"},
		{mode: ModePlan, title: "PLAN", description: "Explore and design without mutations"},
		{mode: ModeAuto, title: "AUTO", description: "Work proactively with configured approvals"},
	}
	items := make([]list.Item, len(definitions))
	selected := 0
	for i := range definitions {
		definitions[i].current = definitions[i].mode == current
		definitions[i].profile = profile
		if definitions[i].current {
			selected = i
		}
		items[i] = definitions[i]
	}

	delegate := newPickerDelegate(isDark, false, themeID, profile)
	width := pickerListWidth(terminalWidth)
	height := pickerListHeight(terminalHeight, len(items)*delegate.Height()+2, 4)
	l := list.New(items, delegate, width, height)
	configurePickerList(&l, isDark, themeID)
	configurePickerListGlyphProfile(&l, profile)
	l.Title = "Mode"
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()
	l.Select(selected)
	return &ModePickerState{List: l}
}

func (m *Model) openModePicker() {
	m.modePickerState = newModePickerState(m.mode, m.width, m.height, m.isDark, m.themeID, m.glyphProfile)
	m.overlay = OverlayModePicker
	m.input.Blur()
}

func (m *Model) closeModePicker() {
	m.modePickerState = nil
	m.closeOverlayToParent()
}

func (m *Model) renderModePicker() string {
	if m.modePickerState == nil {
		return ""
	}
	return m.renderPickerFrame(
		m.modePickerState.List.View(), m.pickerNavigationFooter(false),
	)
}
