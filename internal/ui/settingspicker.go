package ui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
)

type settingsAction int

const (
	settingsModel settingsAction = iota
	settingsProvider
	settingsAgent
	settingsMode
	settingsSessions
	settingsCompact
	settingsRuntime
	settingsPermissions
	settingsHelp
)

type settingsItem struct {
	action      settingsAction
	title       string
	value       string
	description string
}

func (i settingsItem) Title() string {
	title := sanitizeTerminalSingleLine(i.title)
	value := sanitizeTerminalSingleLine(i.value)
	if value == "" {
		return title
	}
	return title + " · " + value
}

func (i settingsItem) Description() string { return sanitizeTerminalSingleLine(i.description) }
func (i settingsItem) FilterValue() string {
	return sanitizeTerminalSingleLine(i.title + " " + i.value)
}

// SettingsPickerState is the transient control center that replaces the
// persistent navigation chrome. It contains list/navigation and responsive
// presentation state only; the parent Model owns and applies every setting.
type SettingsPickerState struct {
	List       list.Model
	ItemHeight int
	Compact    bool
}

func newSettingsPickerState(items []settingsItem, terminalWidth, terminalHeight int, isDark bool, themeID string, profiles ...GlyphProfile) *SettingsPickerState {
	profile := resolveGlyphProfile(profiles...)
	listItems := make([]list.Item, len(items))
	for i := range items {
		listItems[i] = items[i]
	}

	compact := compactSettingsRows(terminalWidth, terminalHeight)
	delegate := newSettingsDelegate(isDark, compact, themeID, profile)
	itemHeight := delegate.Height()

	width := pickerListWidth(terminalWidth)
	height := settingsListHeight(listItems, itemHeight, terminalHeight)

	l := list.New(listItems, delegate, width, height)
	configurePickerList(&l, isDark, themeID)
	configurePickerListGlyphProfile(&l, profile)
	l.Title = "Settings"
	setSettingsTitleDensity(&l, compact)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()

	return &SettingsPickerState{
		List:       l,
		ItemHeight: itemHeight,
		Compact:    compact,
	}
}

func compactSettingsRows(terminalWidth, terminalHeight int) bool {
	return terminalWidth <= 40 || terminalHeight <= 20
}

func newSettingsDelegate(isDark, compact bool, themeID string, profiles ...GlyphProfile) list.DefaultDelegate {
	return newPickerDelegate(isDark, compact, themeID, profiles...)
}

func setSettingsTitleDensity(l *list.Model, compact bool) {
	bottom := 1
	if compact {
		// The default title bar reserves a blank row. At the supported 30x12
		// minimum that row is better spent keeping all seven settings visible.
		bottom = 0
	}
	l.Styles.TitleBar = l.Styles.TitleBar.Padding(0, 0, bottom, 0)
}

func settingsDetailWidth(terminalWidth int) int {
	// Match the list's item indentation so the selected detail aligns with the
	// row title while remaining inside the shared picker frame.
	return max(1, pickerListWidth(terminalWidth)-2)
}

func settingsListHeight(items []list.Item, itemHeight, terminalHeight int) int {
	return pickerListHeight(terminalHeight, len(items)*itemHeight+2, 4)
}

// overlayWidthTiers is the modal width scale, widest first. Every overlay
// resolves to one of these for a given terminal, so Settings, Models, Sessions,
// Help, and Agents all share an edge instead of each picking its own maximum.
// Before this scale the codebase carried eight different maxima (52, 56, 58,
// 60, 62, 66, 68, 76); because overlayOnContent centers what it is handed, each
// one landed at a different column and navigating between modals made the panel
// jump sideways. The tiers are close enough that a terminal resize steps
// gently, and the widest matches the roomiest surface (the model picker).
var overlayWidthTiers = []int{76, 68, 60}

// pickerContentWidth is the canonical modal body width for a terminal. It
// deliberately takes no per-caller maximum: a shared anchor is worth more than
// letting each overlay pick a bespoke width.
func pickerContentWidth(terminalWidth int) int {
	// One cell of breathing room outside each border edge.
	available := terminalWidth - 4
	for _, tier := range overlayWidthTiers {
		if available >= tier {
			return tier
		}
	}
	if available < 20 {
		return 20
	}
	return available
}

// pickerListWidth leaves room for the modal's horizontal padding. Bubbles
// delegates size and truncate their rows against this width, so keeping the
// list and box interiors aligned prevents narrow rows from wrapping into
// surprise extra lines.
func pickerListWidth(terminalWidth int) int {
	return max(1, pickerContentWidth(terminalWidth)-2)
}

// pickerListHeight keeps transient navigation inside the terminal. Chrome is
// the number of rows used by the surrounding border/footer outside the list.
func pickerListHeight(terminalHeight, desired, chrome int) int {
	height := min(desired, 20)
	available := terminalHeight - chrome
	if available < 4 {
		available = 4
	}
	return min(height, available)
}

// resizePickerOverlays preserves the active Bubbles list state while adapting
// its viewport to a terminal resize.
func (m *Model) resizePickerOverlays() {
	if state := m.completionState; state != nil {
		state.Filter.SetWidth(completionFilterInputWidth(m.width))
	}
	if state := m.settingsPickerState; state != nil {
		compact := compactSettingsRows(m.width, m.height)
		delegate := newSettingsDelegate(m.isDark, compact, m.themeID, m.glyphProfile)
		state.List.SetDelegate(delegate)
		setSettingsTitleDensity(&state.List, compact)
		state.ItemHeight = delegate.Height()
		state.Compact = compact
		state.List.SetSize(
			pickerListWidth(m.width),
			settingsListHeight(state.List.Items(), state.ItemHeight, m.height),
		)
	}
	if state := m.agentPickerState; state != nil {
		state.List.SetSize(
			pickerListWidth(m.width),
			pickerListHeight(m.height, len(state.List.Items())*defaultPickerItemHeight+2, 4),
		)
	}
	if state := m.providerPickerState; state != nil {
		state.List.SetSize(
			pickerListWidth(m.width),
			pickerListHeight(
				m.height,
				len(state.List.Items())*(state.ItemHeight+max(0, state.ItemSpacing))+2,
				4,
			),
		)
	}
	if state := m.modePickerState; state != nil {
		state.List.SetSize(
			pickerListWidth(m.width),
			pickerListHeight(m.height, len(state.List.Items())*defaultPickerItemHeight+2, 4),
		)
	}
	if state := m.themePickerState; state != nil {
		state.List.SetSize(
			pickerListWidth(m.width),
			pickerListHeight(m.height, len(state.List.Items())*defaultPickerItemHeight+2, themePickerChromeRows),
		)
	}
	if state := m.modelPickerState; state != nil {
		count := len(state.Inventory)
		if count == 0 {
			count = len(state.Models)
		}
		compact := compactModelPicker(m.width, m.height)
		// The model picker reserves metadata for its selected-detail strip;
		// terminal resizing must not turn navigable rows into descriptions.
		delegate := newPickerDelegate(m.isDark, true, m.themeID, m.glyphProfile)
		state.List.SetDelegate(delegate)
		setSettingsTitleDensity(&state.List, compact)
		state.Compact = compact
		state.ItemHeight = delegate.Height()
		state.List.SetSize(
			pickerListWidth(m.width),
			modelPickerListHeight(m.height, count, state.ItemHeight),
		)
		state.List.SetShowPagination(!compact && count*state.ItemHeight > state.List.Height()-2)
	}
	if state := m.cloudConsentState; state != nil {
		state.Compact = m.width <= 40 || m.height <= 14
		delegate := newPickerDelegate(m.isDark, state.Compact, m.themeID, m.glyphProfile)
		state.List.SetDelegate(delegate)
		state.List.SetSize(
			pickerListWidth(m.width),
			len(state.List.Items())*delegate.Height()+1,
		)
	}
	if state := m.sessionsPickerState; state != nil {
		if state.ready() {
			state.List.SetSize(
				pickerListWidth(m.width),
				pickerListHeight(m.height, len(state.Sessions)*defaultPickerItemHeight+4, 4),
			)
		}
	}
	if m.runtimeStatusState != nil {
		m.refreshRuntimeStatus(true)
	}
	if m.goalInspectorState != nil {
		m.goalInspectorState.SetSize(m.width, m.height)
	}
}

func (m *Model) settingsItems() []settingsItem {
	modelValue := m.currentModelSurfaceLabel(false)
	if !m.modelPinned {
		if m.currentModelIsNonLocal() {
			modelValue += " · Auto"
		} else {
			modelValue = "Auto · " + modelValue
		}
	} else if modelValue != "" {
		if m.currentModelIsNonLocal() {
			modelValue += " · Pinned"
		} else {
			modelValue = "Pinned · " + modelValue
		}
	}
	profile := m.agentProfile
	if profile == "" {
		profile = "Default"
	}
	compact := "Auto"
	if m.forceCompact {
		compact = "On"
	}
	connected, unavailable, _ := m.mcpStatusCounts()
	runtime := fmt.Sprintf("%d %s total", m.toolCount, pluralizeNoun(m.toolCount, "tool", "tools"))
	if m.agent != nil {
		availability := m.agent.ToolAvailability()
		runtime = fmt.Sprintf("%d ready %s · %d local · %d MCP",
			availability.Ready(), pluralizeNoun(availability.Ready(), "tool", "tools"),
			availability.Local, availability.MCPConnected,
		)
		if retainedUnavailable := availability.MCPRetained - availability.MCPConnected; retainedUnavailable > 0 {
			runtime += fmt.Sprintf(" · %d MCP unavailable", retainedUnavailable)
		}
	}
	if len(m.mcpServers) > 0 {
		runtime += fmt.Sprintf(" · %d %s · %d connected · %d unavailable",
			len(m.mcpServers), pluralizeServer(len(m.mcpServers)), connected, unavailable,
		)
	} else if len(m.failedServers) > 0 {
		runtime += fmt.Sprintf(" · %d unavailable", len(m.failedServers))
	}
	if m.iceEnabled {
		runtime += " · ICE"
	}
	if m.skipApprovalsEnabled() {
		runtime += " · no approval prompts"
	} else if m.acceptWorkspaceEditsEnabled() {
		runtime += " · accept workspace edits"
	}

	modelDescription := "Choose an installed local model or Ollama Cloud model"
	if len(m.ollamaModels) > 0 {
		modelDescription = ollamaInventorySummary(m.ollamaModels)
	}
	providerValue := m.activeProviderName()
	if providerValue == "" {
		providerValue = "ollama"
	}
	if m.modelManager != nil && m.modelManager.RemoteProvider() {
		providerValue += " · remote"
	} else {
		providerValue += " · local"
	}
	providerDescription := "Switch Ollama or OpenAI-compatible remote profiles"
	if names := m.providerNames(); len(names) > 1 {
		providerDescription = fmt.Sprintf("%d profiles · /provider", len(names))
	}
	runtimeDescription := "Inspect model, approval posture, tools, servers, and failures"
	if len(m.mcpServers) > 0 {
		runtimeDescription = fmt.Sprintf("%d %s · %d connected · %d unavailable · inspect approval and runtime details",
			len(m.mcpServers), pluralizeServer(len(m.mcpServers)), connected, unavailable,
		)
	}
	permissionsValue := "Manage"
	permissionsDescription := "Accept-edits, session grants, durable rules, export/import"
	if m.agent != nil {
		sessionN := len(m.agent.ListSessionApprovalSummary())
		rules := m.agent.WorkspaceRulesSnapshot()
		ruleN := len(rules.BashPrefixes) + len(rules.MCPTools) + len(rules.WritePaths)
		switch {
		case m.acceptWorkspaceEditsEnabled():
			permissionsValue = fmt.Sprintf("accept-edits · %d session · %d rules", sessionN, ruleN)
		case m.skipApprovalsEnabled():
			permissionsValue = fmt.Sprintf("prompts skipped · %d session · %d rules", sessionN, ruleN)
		default:
			permissionsValue = fmt.Sprintf("%d session · %d rules", sessionN, ruleN)
		}
	}
	modeTitle := "Mode"
	modeDescription := "NORMAL, PLAN, or AUTO authority"
	if m.goalRuntime != nil {
		modeTitle = "Next chat mode"
		modeDescription = "Goal Runtime keeps AUTO; this applies after the goal"
	}
	return []settingsItem{
		{action: settingsModel, title: "Model", value: modelValue, description: modelDescription},
		{action: settingsProvider, title: "Provider", value: providerValue, description: providerDescription},
		{action: settingsAgent, title: "Agent profile", value: profile, description: "Change prompt, skills, model, and MCP scope"},
		{action: settingsMode, title: modeTitle, value: m.modeConfigs[m.mode].Label, description: modeDescription},
		{action: settingsSessions, title: "Sessions", value: "Resume", description: "Open a saved workspace session"},
		{action: settingsCompact, title: "Compact layout", value: compact, description: "Toggle the explicit compact transcript preference"},
		{action: settingsRuntime, title: "Runtime status", value: runtime, description: runtimeDescription},
		{action: settingsPermissions, title: "Permissions", value: permissionsValue, description: permissionsDescription},
		{action: settingsHelp, title: "Help", value: "Shortcuts", description: "Keyboard reference and slash commands"},
	}
}

func (m *Model) openSettingsPicker() {
	m.overlayParent = OverlayNone
	m.settingsPickerState = newSettingsPickerState(m.settingsItems(), m.width, m.height, m.isDark, m.themeID, m.glyphProfile)
	m.overlay = OverlaySettings
	m.input.Blur()
}

func (m *Model) refreshSettingsPicker() {
	if m.settingsPickerState == nil {
		return
	}
	selected := m.settingsPickerState.List.Index()
	m.settingsPickerState = newSettingsPickerState(m.settingsItems(), m.width, m.height, m.isDark, m.themeID, m.glyphProfile)
	if selected >= 0 && selected < len(m.settingsPickerState.List.Items()) {
		m.settingsPickerState.List.Select(selected)
	}
}

func (m *Model) closeSettingsPicker() {
	m.settingsPickerState = nil
	m.dismissOverlay()
}

func (m *Model) activateSettings(action settingsAction) tea.Cmd {
	switch action {
	case settingsModel:
		m.openSettingsChild(m.openModelPicker)
	case settingsProvider:
		m.openSettingsChild(m.openProviderPicker)
	case settingsAgent:
		m.openSettingsChild(m.openAgentPicker)
	case settingsMode:
		m.openSettingsChild(m.openModePicker)
	case settingsSessions:
		m.openSettingsChild(m.openSessionsPicker)
		return m.requestSessions()
	case settingsCompact:
		m.forceCompact = !m.forceCompact
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.refreshSettingsPicker()
	case settingsRuntime:
		m.openSettingsChild(m.openRuntimeStatus)
	case settingsPermissions:
		m.openSettingsChild(m.openPermissionsPanel)
	case settingsHelp:
		m.openSettingsChild(func() {
			m.overlay = OverlayHelp
			m.initHelpViewport()
			m.input.Blur()
		})
	}
	return nil
}

func (m *Model) renderSettingsPicker() string {
	if m.settingsPickerState == nil {
		return ""
	}
	content := m.settingsPickerState.List.View()
	if m.settingsPickerState.Compact && m.width >= 36 {
		if item, ok := m.settingsPickerState.List.SelectedItem().(settingsItem); ok && strings.TrimSpace(item.description) != "" {
			detail := truncateDisplay(strings.TrimSpace(item.description), settingsDetailWidth(m.width))
			content = strings.TrimRight(content, "\n") + "\n" +
				m.styles.OverlayDim.Render("  "+detail)
		}
	}
	return m.renderPickerFrame(content, m.pickerNavigationFooter(false))
}
