package ui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

func TestSettingsPickerOpensWithCurrentValues(t *testing.T) {
	m := newTestModel(t)
	m.model = "qwen3.5:4b"
	m.modelPinned = true
	m.agentProfile = "reviewer"
	m.serverCount = 2
	m.toolCount = 17

	updated, _ := m.Update(ctrlKey('p'))
	m = updated.(*Model)
	if m.overlay != OverlaySettings || m.settingsPickerState == nil {
		t.Fatal("Ctrl+P did not open session settings")
	}
	rendered := m.renderSettingsPicker()
	for _, want := range []string{"Settings", "Pinned", "qwen3.5:4b", "reviewer", "ready tools", "local"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("settings missing %q:\n%s", want, rendered)
		}
	}
}

func TestSettingsDistinguishesGoalAuthorityFromNextChatMode(t *testing.T) {
	m := newTestModel(t)
	m.mode = ModePlan
	m.goalRuntime = newUIGoalRuntime(t, 43, goal.BudgetLimits{MaxContinuationTurns: 2})
	items := m.settingsItems()
	mode := items[int(settingsMode)]
	if mode.title != "Next chat mode" || mode.value != "PLAN" ||
		!strings.Contains(mode.description, "keeps AUTO") || !strings.Contains(mode.description, "after the goal") {
		t.Fatalf("goal-owned Settings mode = %#v", mode)
	}
}

func TestSettingsItemsSanitizeTerminalAndBidiControls(t *testing.T) {
	item := settingsItem{
		title: "Mode\x1b]52;c;payload\a", value: "PLAN\nspoof\u202e", description: "safe\ttext\u2066",
	}
	for label, value := range map[string]string{
		"title": item.Title(), "description": item.Description(), "filter": item.FilterValue(),
	} {
		for _, character := range value {
			if character == '\n' || character == '\r' || character == '\t' || isBidiControl(character) {
				t.Fatalf("%s retained terminal control in %q", label, value)
			}
		}
		if strings.Contains(value, "52;c") {
			t.Fatalf("%s retained OSC payload in %q", label, value)
		}
	}
}

func TestSettingsRuntimeFallbackDoesNotDoubleCountServers(t *testing.T) {
	m := newTestModel(t)
	m.agent = nil
	m.toolCount = 7
	m.mcpServers = []MCPServerStatus{
		{Name: "ready", Connected: true, ToolCount: 4},
		{Name: "offline", Connected: false},
	}

	runtimeItem := m.settingsItems()[int(settingsRuntime)]
	for _, want := range []string{"7 tools total", "2 servers", "1 connected", "1 unavailable"} {
		if !strings.Contains(runtimeItem.value, want) {
			t.Fatalf("runtime fallback missing %q: %q", want, runtimeItem.value)
		}
	}
	if strings.Count(runtimeItem.value, "servers") != 1 || strings.Count(runtimeItem.value, "connected") != 1 {
		t.Fatalf("runtime fallback duplicated server accounting: %q", runtimeItem.value)
	}
}

func TestSettingsRuntimeUsesVisibleNaturalSingularSummary(t *testing.T) {
	m := newTestModel(t)
	m.agent = nil
	m.toolCount = 1
	m.mcpServers = []MCPServerStatus{{Name: "ready", Connected: true, ToolCount: 1}}

	item := m.settingsItems()[int(settingsRuntime)]
	for _, value := range []string{item.value, item.description} {
		for _, want := range []string{"1 server", "1 connected", "0 unavailable"} {
			if !strings.Contains(value, want) {
				t.Fatalf("Settings summary missing %q: %#v", want, item)
			}
		}
	}
	if !strings.Contains(item.value, "1 tool total") || strings.Contains(item.value, "1 tools") {
		t.Fatalf("Settings tool count is not naturally singular: %#v", item)
	}
}

func TestSettingsCompactToggleStaysInPicker(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	m.openSettingsPicker()
	m.settingsPickerState.List.Select(int(settingsCompact))

	updated, _ = m.Update(enterKey())
	m = updated.(*Model)
	if !m.forceCompact {
		t.Fatal("settings did not enable compact layout")
	}
	if m.overlay != OverlaySettings || m.settingsPickerState == nil {
		t.Fatal("compact toggle should keep settings open")
	}
	if item, ok := m.settingsPickerState.List.SelectedItem().(settingsItem); !ok || item.value != "On" {
		t.Fatalf("compact row was not refreshed: %#v", m.settingsPickerState.List.SelectedItem())
	}
	if got := m.settingsPickerState.List.Index(); got != int(settingsCompact) {
		t.Fatalf("selected setting after refresh = %d, want %d", got, settingsCompact)
	}
	if !m.settingsPickerState.Compact || m.settingsPickerState.ItemHeight != 1 {
		t.Fatalf("compact refresh changed row density: %#v", m.settingsPickerState)
	}
}

func TestSettingsModeCommitsOnlyOnSelection(t *testing.T) {
	m := newTestModel(t)
	m.openSettingsPicker()
	m.settingsPickerState.List.Select(int(settingsMode))

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)
	if m.overlay != OverlayModePicker || m.mode != ModeAsk {
		t.Fatalf("mode picker state = overlay %d mode %d", m.overlay, m.mode)
	}
	updated, _ = m.Update(downKey())
	m = updated.(*Model)
	if m.mode != ModeAsk || len(m.entries) != 0 {
		t.Fatal("mode picker navigation mutated the session")
	}
	updated, _ = m.Update(enterKey())
	m = updated.(*Model)
	if m.mode != ModePlan || m.overlay != OverlaySettings {
		t.Fatalf("selected mode = %d overlay=%d, want PLAN/settings", m.mode, m.overlay)
	}
	if item, ok := m.settingsPickerState.List.SelectedItem().(settingsItem); !ok || item.action != settingsMode || item.value != "PLAN" {
		t.Fatalf("mode row was not refreshed after selection: %#v", m.settingsPickerState.List.SelectedItem())
	}
	if len(m.entries) != 0 {
		t.Fatalf("empty-state mode switch added redundant receipt: %#v", m.entries)
	}
}

func TestSettingsAutoSelectionChangesAuthorityWithoutCreatingGoal(t *testing.T) {
	m := newTestModel(t)
	m.openSettingsPicker()
	m.settingsPickerState.List.Select(int(settingsMode))

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)
	updated, _ = m.Update(downKey())
	m = updated.(*Model)
	updated, _ = m.Update(downKey())
	m = updated.(*Model)
	updated, _ = m.Update(enterKey())
	m = updated.(*Model)

	if m.mode != ModeAuto || m.overlay != OverlaySettings || m.goalFormState != nil {
		t.Fatalf("AUTO selection = mode %d overlay %d goal form %v", m.mode, m.overlay, m.goalFormState != nil)
	}
	if m.settingsPickerState == nil || m.overlayParent != OverlayNone {
		t.Fatalf("AUTO selection lost Settings hierarchy: settings=%v parent=%d", m.settingsPickerState != nil, m.overlayParent)
	}
}

func TestSettingsChildrenReturnToRootButDirectPickerCloses(t *testing.T) {
	m := newTestModel(t)
	m.agentList = []string{"reviewer"}
	m.openSettingsPicker()
	m.settingsPickerState.List.Select(int(settingsAgent))

	updated, _ := m.Update(enterKey())
	m = updated.(*Model)
	if m.overlay != OverlayAgentPicker || m.overlayParent != OverlaySettings {
		t.Fatalf("agent child hierarchy = overlay %d parent %d", m.overlay, m.overlayParent)
	}
	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlaySettings || m.settingsPickerState == nil {
		t.Fatalf("child Escape did not return to Settings: overlay=%d", m.overlay)
	}
	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlayNone {
		t.Fatalf("root Escape did not return to chat: overlay=%d", m.overlay)
	}

	m.openAgentPicker()
	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlayNone {
		t.Fatalf("direct picker Escape returned to a stale parent: overlay=%d", m.overlay)
	}
}

func TestSettingsHelpAndRuntimeQReturnToRoot(t *testing.T) {
	for _, action := range []settingsAction{settingsRuntime, settingsHelp} {
		m := newTestModel(t)
		m.openSettingsPicker()
		m.settingsPickerState.List.Select(int(action))
		updated, _ := m.Update(enterKey())
		m = updated.(*Model)
		updated, _ = m.Update(charKey('q'))
		m = updated.(*Model)
		if m.overlay != OverlaySettings {
			t.Fatalf("action %d q returned to overlay %d, want Settings", action, m.overlay)
		}
	}
}

func TestSettingsSessionsKeepLoadingEmptyAndErrorInOverlay(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "empty", want: "No saved sessions"},
		{name: "error", err: errors.New("database unavailable"), want: "database unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t)
			m.openSettingsPicker()
			m.settingsPickerState.List.Select(int(settingsSessions))
			updated, cmd := m.Update(enterKey())
			m = updated.(*Model)
			if cmd == nil || m.overlay != OverlaySessionsPicker || !m.sessionListing || m.sessionsPickerState.Phase != sessionsLoading {
				t.Fatalf("sessions did not open loading child: overlay=%d listing=%v state=%#v", m.overlay, m.sessionListing, m.sessionsPickerState)
			}

			updated, _ = m.Update(SessionListMsg{ListToken: m.sessionListToken, Err: tc.err})
			m = updated.(*Model)
			if m.overlay != OverlaySessionsPicker || m.sessionListing {
				t.Fatalf("session result state = overlay %d listing=%v", m.overlay, m.sessionListing)
			}
			if rendered := m.renderSessionsPicker(); !strings.Contains(rendered, tc.want) {
				t.Fatalf("sessions overlay missing %q:\n%s", tc.want, rendered)
			}

			updated, _ = m.Update(escKey())
			m = updated.(*Model)
			if m.overlay != OverlaySettings {
				t.Fatalf("sessions Escape returned to overlay %d", m.overlay)
			}
		})
	}
}

func TestSettingsOverlayDoesNotSwallowQuit(t *testing.T) {
	m := newTestModel(t)
	m.openSettingsPicker()
	updated, cmd := m.Update(ctrlKey('c'))
	m = updated.(*Model)
	if !m.shuttingDown || cmd == nil {
		t.Fatalf("Ctrl+C did not start graceful shutdown: shuttingDown=%v cmd=%v", m.shuttingDown, cmd)
	}
}

func TestSettingsEscapeClosesAndResizePreservesSelection(t *testing.T) {
	m := newTestModel(t)
	m.openSettingsPicker()
	m.settingsPickerState.List.Select(int(settingsSessions))
	if got := m.settingsPickerState.ItemHeight; got != 2 {
		t.Fatalf("normal settings item height = %d, want 2", got)
	}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	if got := m.settingsPickerState.List.Index(); got != int(settingsSessions) {
		t.Fatalf("selected setting after resize = %d, want %d", got, settingsSessions)
	}
	if got := m.settingsPickerState.ItemHeight; got != 1 || !m.settingsPickerState.Compact {
		t.Fatalf("compact settings layout = height %d compact %v, want 1/true", got, m.settingsPickerState.Compact)
	}
	assertRenderedLinesFit(t, m.renderSettingsPicker(), 40)
	assertRenderedHeightFits(t, m.renderSettingsPicker(), 20)

	updated, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*Model)
	if got := m.settingsPickerState.List.Index(); got != int(settingsSessions) {
		t.Fatalf("selected setting after resize back = %d, want %d", got, settingsSessions)
	}
	if got := m.settingsPickerState.ItemHeight; got != 2 || m.settingsPickerState.Compact {
		t.Fatalf("restored settings layout = height %d compact %v, want 2/false", got, m.settingsPickerState.Compact)
	}

	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlayNone || m.settingsPickerState != nil {
		t.Fatalf("Escape left settings open: overlay=%d state=%v", m.overlay, m.settingsPickerState)
	}
}

func TestSettingsCompactRowsShowOnlySelectedSingleLineDetail(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	m.openSettingsPicker()

	state := m.settingsPickerState
	if !state.Compact || state.ItemHeight != 1 {
		t.Fatalf("compact state = compact %v height %d, want true/1", state.Compact, state.ItemHeight)
	}
	if got := len(state.List.Items()); got != 9 {
		t.Fatalf("settings item count = %d, want 9", got)
	}
	if got := state.List.Height(); got >= 16 {
		t.Fatalf("compact list height = %d, want denser than the normal 16 rows", got)
	}

	rendered := m.renderSettingsPicker()
	if !strings.Contains(rendered, "Choose an installed local") {
		t.Fatalf("selected Model detail missing:\n%s", rendered)
	}
	if strings.Contains(rendered, "Change prompt, skills") {
		t.Fatalf("unselected Agent detail leaked into compact picker:\n%s", rendered)
	}
	if got := strings.Count(rendered, "Choose an installed local"); got != 1 {
		t.Fatalf("selected detail rendered %d times, want once:\n%s", got, rendered)
	}

	state.List.Select(int(settingsCompact))
	rendered = m.renderSettingsPicker()
	if !strings.Contains(rendered, "Toggle the explicit compact") {
		t.Fatalf("selected Compact detail missing:\n%s", rendered)
	}
	if strings.Contains(rendered, "Choose an installed local") {
		t.Fatalf("previous selected detail remained visible:\n%s", rendered)
	}
	assertRenderedLinesFit(t, rendered, 40)
	assertRenderedHeightFits(t, rendered, 20)
}

func TestSettingsCompactRowsUseEitherResponsiveBreakpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		width   int
		height  int
		compact bool
	}{
		{name: "narrow", width: 40, height: 24, compact: true},
		{name: "short", width: 80, height: 20, compact: true},
		{name: "normal", width: 41, height: 21, compact: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: tc.width, Height: tc.height})
			m = updated.(*Model)
			m.openSettingsPicker()

			state := m.settingsPickerState
			if state.Compact != tc.compact {
				t.Fatalf("compact = %v, want %v", state.Compact, tc.compact)
			}
			wantHeight := 2
			if tc.compact {
				wantHeight = 1
			}
			if state.ItemHeight != wantHeight {
				t.Fatalf("item height = %d, want %d", state.ItemHeight, wantHeight)
			}
			assertRenderedLinesFit(t, m.renderSettingsPicker(), tc.width)
			assertRenderedHeightFits(t, m.renderSettingsPicker(), tc.height)
		})
	}
}

func TestSettingsCompactRowsFitMinimumWithLabelsAndFooter(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(*Model)
	m.openSettingsPicker()
	m.settingsPickerState.List.Select(int(settingsRuntime))

	rendered := m.renderSettingsPicker()
	if strings.Contains(rendered, "Inspect local") {
		t.Fatalf("minimum settings spent its action row on selected prose:\n%s", rendered)
	}
	plain := ansi.Strip(rendered)
	if !strings.Contains(plain, "esc close") || !strings.Contains(plain, "enter") || !strings.Contains(plain, "↑/↓") {
		t.Fatalf("minimum settings lost actionable navigation:\n%s", rendered)
	}
	for _, label := range []string{"Model", "Provider", "Agent profile", "Mode", "Sessions", "Compact layout", "Runtime status"} {
		if !strings.Contains(rendered, label) {
			t.Fatalf("minimum settings hid %q behind prose:\n%s", label, rendered)
		}
	}
	// Permissions and Help can sit one viewport step down at the minimum size.
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
	if got := lipgloss.Height(rendered); got > minTerminalHeight-1 || !strings.Contains(rendered, "╰") {
		t.Fatalf("minimum Settings frame risks clipping the closing border: height=%d\n%s", got, rendered)
	}
}

func TestSettingsNormalRowsKeepAllDescriptions(t *testing.T) {
	m := newTestModel(t)
	m.openSettingsPicker()
	state := m.settingsPickerState
	if state.Compact || state.ItemHeight != 2 {
		t.Fatalf("normal state = compact %v height %d, want false/2", state.Compact, state.ItemHeight)
	}

	rendered := m.renderSettingsPicker()
	for _, want := range []string{
		"Choose an installed local model",
		"Change prompt, skills, model, and MCP scope",
		"Keyboard reference and slash commands",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("normal settings missing description %q:\n%s", want, rendered)
		}
	}
}

func TestSettingsAndPickersFitNarrowTerminal(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	m.model = "ornith:latest"
	m.agentList = []string{"reviewer", "builder"}

	m.openSettingsPicker()
	assertRenderedLinesFit(t, m.renderSettingsPicker(), 40)
	assertRenderedHeightFits(t, m.renderSettingsPicker(), 20)
	m.openAgentPicker()
	assertRenderedLinesFit(t, m.renderAgentPicker(), 40)
	assertRenderedHeightFits(t, m.renderAgentPicker(), 20)
	m.openModePicker()
	assertRenderedLinesFit(t, m.renderModePicker(), 40)
	assertRenderedHeightFits(t, m.renderModePicker(), 20)
	m.openRuntimeStatus()
	assertRenderedLinesFit(t, m.renderRuntimeStatus(), 40)
	assertRenderedHeightFits(t, m.renderRuntimeStatus(), 20)
}

func TestSettingsAndPickersFitSupportedMinimum(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(*Model)
	m.agentList = []string{"reviewer"}

	m.openSettingsPicker()
	assertRenderedLinesFit(t, m.renderSettingsPicker(), minTerminalWidth)
	assertRenderedHeightFits(t, m.renderSettingsPicker(), minTerminalHeight)
	m.openAgentPicker()
	assertRenderedLinesFit(t, m.renderAgentPicker(), minTerminalWidth)
	assertRenderedHeightFits(t, m.renderAgentPicker(), minTerminalHeight)
	m.openModePicker()
	assertRenderedLinesFit(t, m.renderModePicker(), minTerminalWidth)
	assertRenderedHeightFits(t, m.renderModePicker(), minTerminalHeight)
	m.openRuntimeStatus()
	assertRenderedLinesFit(t, m.renderRuntimeStatus(), minTerminalWidth)
	assertRenderedHeightFits(t, m.renderRuntimeStatus(), minTerminalHeight)
}

func assertRenderedHeightFits(t *testing.T, rendered string, height int) {
	t.Helper()
	if got := lipgloss.Height(rendered); got > height {
		t.Fatalf("rendered height = %d, want <= %d:\n%s", got, height, rendered)
	}
}
