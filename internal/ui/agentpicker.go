package ui

import (
	"strings"

	"charm.land/bubbles/v2/list"
)

type agentItem struct {
	name      string
	display   string
	current   bool
	isDefault bool
	profile   GlyphProfile
}

func (i agentItem) Title() string {
	title := i.display
	if i.current {
		title += "  " + glyphSet(i.profile).Success
	}
	return title
}

func (i agentItem) Description() string {
	if i.isDefault {
		return "Project instructions with no named profile"
	}
	return "Apply this profile's prompt, skills, model, and MCP scope"
}

func (i agentItem) FilterValue() string { return i.display }

type AgentPickerState struct {
	List list.Model
}

func newAgentPickerState(names []string, current string, terminalWidth, terminalHeight int, isDark bool, themeID string, profiles ...GlyphProfile) *AgentPickerState {
	profile := resolveGlyphProfile(profiles...)
	items := make([]list.Item, 0, len(names)+1)
	items = append(items, agentItem{
		display:   "Default",
		current:   current == "",
		isDefault: true,
		profile:   profile,
	})
	selected := 0
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		if name == current {
			selected = len(items)
		}
		items = append(items, agentItem{name: name, display: name, current: name == current, profile: profile})
	}

	delegate := newPickerDelegate(isDark, false, themeID, profile)
	width := pickerListWidth(terminalWidth)
	height := pickerListHeight(terminalHeight, len(items)*delegate.Height()+2, 4)
	l := list.New(items, delegate, width, height)
	configurePickerList(&l, isDark, themeID)
	configurePickerListGlyphProfile(&l, profile)
	l.Title = "Profile"
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()
	l.Select(selected)
	return &AgentPickerState{List: l}
}

func (m *Model) openAgentPicker() {
	m.agentPickerState = newAgentPickerState(m.agentList, m.agentProfile, m.width, m.height, m.isDark, m.themeID, m.glyphProfile)
	m.overlay = OverlayAgentPicker
	m.input.Blur()
}

func (m *Model) closeAgentPicker() {
	m.agentPickerState = nil
	m.closeOverlayToParent()
}

func (m *Model) selectAgentProfile(name string) {
	display := name
	if display == "" {
		display = "Default"
	}
	if err := m.applyAgentProfile(name); err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: err.Error()})
	} else {
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Agent profile: " + display})
	}
	m.closeAgentPicker()
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.resumeFollow()
}

func (m *Model) renderAgentPicker() string {
	if m.agentPickerState == nil {
		return ""
	}
	return m.renderPickerFrame(
		m.agentPickerState.List.View(), m.pickerNavigationFooter(false),
	)
}
