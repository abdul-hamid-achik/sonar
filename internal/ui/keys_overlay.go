package ui

import (
	"errors"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/command"
)

// handleOverlayKey routes keyboard input while an overlay is active. Every
// key press is owned by the active overlay; unknown overlays swallow input.
func (m *Model) handleOverlayKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	var cmds []tea.Cmd
	// ESC always closes the current overlay.
	if key.Matches(msg, m.keys.Cancel) {
		switch m.overlay {
		case OverlayCompletion:
			m.dismissCompletion()
		case OverlayTranscriptSearch:
			return m.closeTranscriptSearch(true), true
		case OverlayModelPicker:
			if m.modelPickerState != nil && m.modelPickerState.List.FilterState() != list.Unfiltered {
				var cmd tea.Cmd
				m.modelPickerState.List, cmd = m.modelPickerState.List.Update(msg)
				cmds = append(cmds, cmd)
				return tea.Batch(cmds...), true
			}
			m.closeModelPicker()
		case OverlayCloudConsent:
			m.closeCloudConsent()
			return nil, true
		case OverlayModelDetails:
			m.closeModelDetails()
			return nil, true
		case OverlayModelPull:
			if m.modelPullState != nil && m.modelPullState.Phase == ModelPullRunning {
				m.cancelModelPull()
				m.modelPullState.Apply(OllamaModelPullProgressMsg{Name: m.modelPullState.Name, Err: errors.New("model download cancelled")})
				return nil, true
			}
			m.closeModelPull()
			return nil, true
		case OverlayPlanForm:
			m.closePlanForm()
		case OverlayGoalForm:
			m.closeGoalForm()
		case OverlaySessionsPicker:
			// If the list is filtering, let ESC clear the filter first.
			if m.sessionsPickerState != nil && m.sessionsPickerState.ready() && m.sessionsPickerState.List.FilterState() != list.Unfiltered {
				var cmd tea.Cmd
				m.sessionsPickerState.List, cmd = m.sessionsPickerState.List.Update(msg)
				cmds = append(cmds, cmd)
				return tea.Batch(cmds...), true
			}
			m.closeSessionsPicker()
		case OverlaySettings:
			m.closeSettingsPicker()
		case OverlayPermissions:
			m.closePermissionsPanel()
		case OverlayAgentPicker:
			m.closeAgentPicker()
		case OverlayProviderPicker:
			m.closeProviderPicker()
		case OverlayAgents:
			if m.agentHubBack() {
				return nil, true
			}
			m.closeAgentHub()
		case OverlayModePicker:
			m.closeModePicker()
		case OverlayThemePicker:
			m.closeThemePicker()
		case OverlayContextDoctor:
			m.closeContextDoctor()
		case OverlayRuntimeStatus:
			m.closeRuntimeStatus()
		case OverlayGoalRecovery:
			if m.goalRecoveryState != nil {
				event, cmd := m.goalRecoveryState.Update(msg)
				cmds = append(cmds, cmd, m.handleGoalRecoveryEvent(event))
			} else {
				m.closeGoalRecovery()
			}
			cmds = append(cmds, tea.ClearScreen)
			return tea.Batch(cmds...), true
		case OverlayGoalInspector:
			if m.goalInspectorState != nil && m.goalInspectorState.CancelConfirmation() {
				return nil, true
			}
			m.closeGoalInspector()
		case OverlayHelp:
			m.closeHelpOverlay()
		default:
			m.dismissOverlay()
		}
		return tea.ClearScreen, true
	}

	if m.overlay == OverlayTranscriptSearch {
		return m.handleTranscriptSearchKey(msg), true
	}

	// Help overlay: scroll keys forwarded to helpViewport, ? or q to dismiss.
	if m.overlay == OverlayHelp {
		switch msg.String() {
		case "?", "q":
			m.closeHelpOverlay()
			return tea.ClearScreen, true
		default:
			navigateReadOnlyViewport(&m.helpViewport, msg.String())
		}
		return nil, true
	}

	if m.overlay == OverlayRuntimeStatus {
		switch msg.String() {
		case "q":
			m.closeRuntimeStatus()
			return tea.ClearScreen, true
		default:
			if m.runtimeStatusState != nil {
				navigateReadOnlyViewport(&m.runtimeStatusState.Viewport, msg.String())
			}
		}
		return nil, true
	}

	if m.overlay == OverlayContextDoctor {
		if msg.String() == "q" {
			m.closeContextDoctor()
			return tea.ClearScreen, true
		}
		return nil, true
	}

	if m.overlay == OverlayAgents {
		return m.handleAgentHubKey(msg), true
	}

	if m.overlay == OverlaySettings && m.settingsPickerState != nil {
		if key.Matches(msg, m.keys.CompleteSelect) {
			if item := m.settingsPickerState.List.SelectedItem(); item != nil {
				cmds = append(cmds, m.activateSettings(item.(settingsItem).action))
			}
		} else {
			var cmd tea.Cmd
			m.settingsPickerState.List, cmd = m.settingsPickerState.List.Update(msg)
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...), true
	}

	if m.overlay == OverlayPermissions && m.permissionsPanelState != nil {
		if key.Matches(msg, m.keys.CompleteSelect) {
			if item := m.permissionsPanelState.List.SelectedItem(); item != nil {
				cmds = append(cmds, m.activatePermissionsItem(item.(permissionsItem)))
			}
		} else {
			var cmd tea.Cmd
			m.permissionsPanelState.List, cmd = m.permissionsPanelState.List.Update(msg)
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...), true
	}

	if m.overlay == OverlayAgentPicker && m.agentPickerState != nil {
		if key.Matches(msg, m.keys.CompleteSelect) {
			if item := m.agentPickerState.List.SelectedItem(); item != nil {
				m.selectAgentProfile(item.(agentItem).name)
			}
		} else {
			var cmd tea.Cmd
			m.agentPickerState.List, cmd = m.agentPickerState.List.Update(msg)
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...), true
	}

	if m.overlay == OverlayProviderPicker && m.providerPickerState != nil {
		if key.Matches(msg, m.keys.CompleteSelect) {
			if item := m.providerPickerState.List.SelectedItem(); item != nil {
				cmds = append(cmds, m.activateProviderItem(item.(providerItem)))
			}
		} else {
			var cmd tea.Cmd
			m.providerPickerState.List, cmd = m.providerPickerState.List.Update(msg)
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...), true
	}

	if m.overlay == OverlayThemePicker && m.themePickerState != nil {
		if key.Matches(msg, m.keys.CompleteSelect) {
			selected := m.themePickerState.SelectedThemeID()
			m.closeThemePicker()
			cmds = append(cmds, m.applyTheme(selected))
		} else {
			var cmd tea.Cmd
			m.themePickerState.List, cmd = m.themePickerState.List.Update(msg)
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...), true
	}

	if m.overlay == OverlayModePicker && m.modePickerState != nil {
		if key.Matches(msg, m.keys.CompleteSelect) {
			if item := m.modePickerState.List.SelectedItem(); item != nil {
				selectedMode := item.(modeItem).mode
				m.closeModePicker()
				m.setMode(selectedMode)
			}
		} else {
			var cmd tea.Cmd
			m.modePickerState.List, cmd = m.modePickerState.List.Update(msg)
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...), true
	}

	// Model picker overlay: forward keys to list, Enter selects.
	if m.overlay == OverlayModelPicker && m.modelPickerState != nil {
		if m.modelPickerState.List.FilterState() == list.Filtering {
			var cmd tea.Cmd
			m.modelPickerState.List, cmd = m.modelPickerState.List.Update(msg)
			cmds = append(cmds, cmd)
			if !key.Matches(msg, m.keys.CompleteSelect) {
				return tea.Batch(cmds...), true
			}
		}
		handled := false
		switch {
		case msg.String() == "a":
			cmds = append(cmds, m.openModelPull())
			handled = true
		case msg.String() == "d":
			if descriptor, ok := m.modelPickerState.SelectedDescriptor(); ok {
				m.openModelDetails(descriptor)
				cmds = append(cmds, m.requestOllamaModelDetails(descriptor))
			}
			handled = true
		case msg.String() == "r":
			m.modelPickerState.Notice = "Refreshing Ollama inventory…"
			if !m.reducedMotion {
				cmds = append(cmds, m.modelPickerState.List.StartSpinner())
			}
			cmds = append(cmds, m.refreshOllamaInventory())
			handled = true
		case key.Matches(msg, m.keys.CompleteSelect):
			if descriptor, ok := m.modelPickerState.SelectedDescriptor(); ok && descriptor.Selectable && descriptor.Fit {
				m.selectModel(descriptor.Name)
			} else if reason := m.modelPickerState.SelectedReason(); reason != "" {
				m.modelPickerState.List.Title = "Unavailable · " + reason
			}
			handled = true
		}
		if !handled {
			var cmd tea.Cmd
			m.modelPickerState.List, cmd = m.modelPickerState.List.Update(msg)
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...), true
	}

	if m.overlay == OverlayCloudConsent && m.cloudConsentState != nil {
		if key.Matches(msg, m.keys.CompleteSelect) {
			if item, ok := m.cloudConsentState.List.SelectedItem().(cloudConsentItem); ok {
				if item.action == cloudConsentAllow {
					cmds = append(cmds, m.confirmCloudModel(m.cloudConsentState.ModelName))
				} else {
					m.closeCloudConsent()
				}
			}
		} else {
			var cmd tea.Cmd
			m.cloudConsentState.List, cmd = m.cloudConsentState.List.Update(msg)
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...), true
	}

	if m.overlay == OverlayModelPull && m.modelPullState != nil {
		cmds = append(cmds, m.modelPullState.Update(msg))
		return tea.Batch(cmds...), true
	}

	if m.overlay == OverlayModelDetails {
		return nil, true
	}

	// Composer-owned inline Plan form.
	if m.overlay == OverlayPlanForm && m.planFormState != nil {
		anchor := m.captureInlineFormTranscriptAnchor()
		submitted, cancelled := m.updatePlanForm(msg)
		if cancelled {
			m.closePlanForm()
			return nil, true
		}
		if submitted {
			prompt := m.planFormState.AssemblePrompt()
			m.closePlanForm()
			cmd := m.submitPlanFormPrompt(prompt)
			m.restoreInlineFormTranscriptAnchor(anchor)
			return cmd, true
		}
		m.refreshInlineFormLayout(anchor)
		return nil, true
	}

	if m.overlay == OverlayGoalForm && m.goalFormState != nil {
		anchor := m.captureInlineFormTranscriptAnchor()
		event, cmd := m.goalFormState.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		switch event.Action {
		case GoalActionCancel:
			m.closeGoalForm()
		case GoalActionSave:
			cmds = append(cmds, m.applyGoalForm(event))
		}
		m.refreshInlineFormLayout(anchor)
		return tea.Batch(cmds...), true
	}

	if m.overlay == OverlayGoalRecovery && m.goalRecoveryState != nil {
		event, cmd := m.goalRecoveryState.Update(msg)
		cmds = append(cmds, cmd, m.handleGoalRecoveryEvent(event))
		return tea.Batch(cmds...), true
	}

	if m.overlay == OverlayGoalInspector && m.goalInspectorState != nil {
		event, cmd := m.goalInspectorState.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if event.ActionID == goalInspectorRecoveryActionID {
			m.openGoalRecovery()
		} else if event.Action != command.ActionNone {
			m.closeGoalInspector()
			cmds = append(cmds, m.handleCommandAction(command.Result{Action: event.Action}))
		}
		return tea.Batch(cmds...), true
	}

	// Sessions picker overlay: forward keys to list, Enter loads.
	if m.overlay == OverlaySessionsPicker && m.sessionsPickerState != nil {
		if !m.sessionsPickerState.ready() {
			return nil, true
		}
		if m.sessionsPickerState.List.FilterState() == list.Filtering {
			var cmd tea.Cmd
			m.sessionsPickerState.List, cmd = m.sessionsPickerState.List.Update(msg)
			cmds = append(cmds, cmd)
			if !key.Matches(msg, m.keys.CompleteSelect) {
				return tea.Batch(cmds...), true
			}
		}
		if key.Matches(msg, m.keys.CompleteSelect) {
			if item := m.sessionsPickerState.List.SelectedItem(); item != nil {
				si := item.(sessionItem)
				return m.beginSessionSwitch(si.id, si.publicID, si.title), true
			}
		} else {
			var cmd tea.Cmd
			m.sessionsPickerState.List, cmd = m.sessionsPickerState.List.Update(msg)
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...), true
	}

	// Completion overlay: handle navigation and filter keys.
	if m.overlay == OverlayCompletion && m.isCompletionActive() {
		cs := m.completionState
		switch {
		case key.Matches(msg, m.keys.CompleteUp):
			if cs.Index > 0 {
				cs.Index--
				return m.refreshCompletionPreview(), true
			}
			return nil, true
		case key.Matches(msg, m.keys.CompleteDown):
			if cs.Index < len(cs.FilteredItems)-1 {
				cs.Index++
				return m.refreshCompletionPreview(), true
			}
			return nil, true
		case key.Matches(msg, m.keys.CompleteSelect):
			// Enter: if item is a folder, drill into it; otherwise accept
			if cs.Index < len(cs.FilteredItems) && cs.Kind == "attachments" && cs.FilteredItems[cs.Index].Category == "folder" {
				return m.drillIntoFolder(), true
			} else {
				m.acceptCompletion()
			}
		case key.Matches(msg, m.keys.CompleteToggle):
			// Tab toggles multi-select
			m.toggleCompletionSelection()
		default:
			// Check for backspace on empty filter => go up directory for @ kind
			if msg.Code == tea.KeyBackspace && cs.Filter.Value() == "" && cs.Kind == "attachments" && cs.CurrentPath != "" {
				return m.drillUpFolder(), true
			}

			// Forward all other keys to filter input
			oldFilter := cs.Filter.Value()
			var cmd tea.Cmd
			cs.Filter, cmd = cs.Filter.Update(msg)
			if cs.Kind == "command" && strings.ContainsAny(cs.Filter.Value(), " \t\n") {
				// Once arguments begin, completion has done its job. Return the
				// entire draft to the composer so Enter executes the command
				// instead of selecting a suggestion and discarding arguments.
				draft, cursorRune := m.completionDraftAndCursor()
				m.closeCompletion()
				m.setComposerDraftAtRune(draft, cursorRune)
				m.completionSuppressedDraft = draft
				return cmd, true
			}

			// Re-filter if text changed
			if cs.Filter.Value() != oldFilter {
				if cs.CommandPrefix != "" {
					cs.BaseItems = m.completer.CompleteStatic(cs.CommandPrefix + cs.Filter.Value())
					cs.AllItems = append([]Completion(nil), cs.BaseItems...)
					cs.FilteredItems = append([]Completion(nil), cs.BaseItems...)
				} else {
					cs.FilteredItems = FilterCompletions(cs.AllItems, cs.Filter.Value())
				}
				cs.Index = 0
				previewCmd := m.refreshCompletionPreview()

				if cs.Kind == "attachments" {
					searchCmd := m.scheduleCompletionSearch(
						cs.Filter.Value(),
						cs.CurrentPath,
						cs.Filter.Value() != "",
					)
					return tea.Batch(cmd, previewCmd, searchCmd), true
				}
				return tea.Batch(cmd, previewCmd), true
			}
			return cmd, true
		}
		return nil, true
	}

	// Unknown overlay — swallow.
	return nil, true
}
