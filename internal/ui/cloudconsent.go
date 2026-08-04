package ui

import (
	"fmt"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/abdul-hamid-achik/sonar/internal/config"
)

type cloudConsentAction uint8

const (
	cloudConsentCancel cloudConsentAction = iota
	cloudConsentAllow
)

type cloudConsentItem struct {
	action      cloudConsentAction
	title       string
	description string
}

func (item cloudConsentItem) Title() string       { return item.title }
func (item cloudConsentItem) Description() string { return item.description }
func (item cloudConsentItem) FilterValue() string { return item.title }

// CloudConsentState owns only the transient confirmation presentation. The
// parent retains policy authority and commits or rejects the exact grant.
type CloudConsentState struct {
	ModelName   string
	DisplayName string
	List        list.Model
	Error       string
	Compact     bool
	PendingLoad *SessionLoadedMsg
	PriorGrant  bool
}

func newCloudConsentState(model OllamaModelDescriptor, terminalWidth, terminalHeight int, isDark bool, themeID string, profiles ...GlyphProfile) *CloudConsentState {
	profile := resolveGlyphProfile(profiles...)
	displayName := modelDisplayName(model)
	items := []list.Item{
		cloudConsentItem{
			action:      cloudConsentCancel,
			title:       "Cancel",
			description: "Keep this conversation local-only",
		},
		cloudConsentItem{
			action:      cloudConsentAllow,
			title:       fmt.Sprintf("Use %s for this conversation", displayName),
			description: "One conversation · never auto-routed · not saved",
		},
	}
	compact := terminalWidth <= 40 || terminalHeight <= 14
	delegate := newPickerDelegate(isDark, compact, themeID, profile)
	width := pickerListWidth(terminalWidth)
	height := len(items)*delegate.Height() + 1
	choices := list.New(items, delegate, width, height)
	configurePickerList(&choices, isDark, themeID)
	configurePickerListGlyphProfile(&choices, profile)
	choices.SetShowTitle(false)
	choices.SetShowStatusBar(false)
	choices.SetShowHelp(false)
	choices.SetShowPagination(false)
	choices.SetFilteringEnabled(false)
	choices.DisableQuitKeybindings()
	choices.Select(0)
	return &CloudConsentState{ModelName: model.Name, DisplayName: displayName, List: choices, Compact: compact}
}

func (m *Model) openCloudConsent(model OllamaModelDescriptor) {
	m.cloudConsentState = newCloudConsentState(model, m.width, m.height, m.isDark, m.themeID, m.glyphProfile)
	m.overlay = OverlayCloudConsent
	m.input.Blur()
}

func (m *Model) openCloudConsentForSession(model OllamaModelDescriptor, message SessionLoadedMsg) {
	m.cloudConsentState = newCloudConsentState(model, m.width, m.height, m.isDark, m.themeID, m.glyphProfile)
	m.cloudConsentState.PendingLoad = &message
	m.cloudConsentState.PriorGrant = model.ConsentGranted
	m.overlayParent = OverlayNone
	m.overlay = OverlayCloudConsent
	m.input.Blur()
}

func (m *Model) closeCloudConsent() {
	if m.cloudConsentState != nil && m.cloudConsentState.PendingLoad != nil && m.cloudConsentState.PendingLoad.ExecutionLease != nil {
		_ = m.cloudConsentState.PendingLoad.ExecutionLease.Close()
		m.cloudConsentState.PendingLoad.ExecutionLease = nil
	}
	if m.pendingSessionSwitch != nil {
		m.restoreAndClearPendingSessionSwitch()
	}
	m.cloudConsentState = nil
	if m.modelPickerState != nil {
		m.overlay = OverlayModelPicker
		m.input.Blur()
		return
	}
	m.dismissOverlay()
}

func (m *Model) renderCloudConsent() string {
	if m.cloudConsentState == nil {
		return ""
	}
	width := pickerListWidth(m.width)
	content := m.styles.OverlayTitle.Render("Use Ollama Cloud?") + "\n" +
		m.styles.OverlayDim.Render(wrapText("Prompts, context, files, and tool results leave this machine through Ollama Cloud.", width)) + "\n\n" +
		m.cloudConsentState.List.View()
	enterAction := "select"
	if selected, ok := m.cloudConsentState.List.SelectedItem().(cloudConsentItem); ok {
		switch selected.action {
		case cloudConsentCancel:
			enterAction = "cancel"
		case cloudConsentAllow:
			enterAction = "use"
		}
	}
	hints := []keyHint{
		{Key: "esc", Action: "cancel"},
		{Key: "enter", Action: enterAction},
	}
	withMovement := append(append([]keyHint(nil), hints...), keyHint{Key: "↑/↓", Action: "move"})
	if lipgloss.Width(m.renderKeyHintSet(withMovement, 2)) <= width {
		hints = withMovement
	}
	footer := m.renderKeyHints(width, hints...)
	if m.cloudConsentState.Error != "" {
		footer += "\n" + m.styles.ErrorText.Render(truncateDisplay(sanitizeTerminalSingleLine(m.cloudConsentState.Error), width))
	}
	return m.renderPickerFrame(content, footer)
}

func (m *Model) confirmCloudModel(name string) tea.Cmd {
	if m.modelManager == nil {
		m.cloudConsentState.Error = "Ollama Cloud is unavailable"
		return nil
	}
	if err := m.modelManager.GrantOllamaCloudModel(name); err != nil {
		m.cloudConsentState.Error = fmt.Sprintf("Ollama Cloud: %v", err)
		return nil
	}
	m.cloudConsentState.Error = ""
	m.setCloudConsentProjection(name, true)
	if m.cloudConsentState.PendingLoad != nil {
		message := *m.cloudConsentState.PendingLoad
		priorGrant := m.cloudConsentState.PriorGrant
		m.cloudConsentState.PendingLoad = nil
		m.cloudConsentState = nil
		m.overlay = OverlayNone
		m.cloudRestoreAuthorized = config.CanonicalModelName(name)
		succeeded, cmd := m.finishLoadedSession(message)
		m.cloudRestoreAuthorized = ""
		if !succeeded && !priorGrant {
			m.modelManager.RevokeOllamaCloudModel(name)
			m.setCloudConsentProjection(name, false)
		}
		return cmd
	}
	m.switchSelectedModel(name)
	return nil
}

func (m *Model) cancelPendingCloudSessionRestore() {
	if m.cloudConsentState == nil || m.cloudConsentState.PendingLoad == nil {
		return
	}
	if m.cloudConsentState.PendingLoad.ExecutionLease != nil {
		_ = m.cloudConsentState.PendingLoad.ExecutionLease.Close()
		m.cloudConsentState.PendingLoad.ExecutionLease = nil
	}
	m.cloudConsentState = nil
	m.clearPendingSessionSwitchSnapshot()
	if m.overlay == OverlayCloudConsent {
		m.overlay = OverlayNone
	}
}

func (m *Model) setCloudConsentProjection(name string, granted bool) {
	for index := range m.ollamaModels {
		if config.CanonicalModelName(m.ollamaModels[index].Name) != config.CanonicalModelName(name) {
			continue
		}
		m.ollamaModels[index].ConsentGranted = granted
		m.ollamaModels[index].RequiresConsent = !granted && m.localOnly && m.ollamaModels[index].Source == OllamaModelCloud
		if granted {
			m.ollamaModels[index].Reason = "Ollama Cloud · conversation consent"
		} else if m.ollamaModels[index].RequiresConsent {
			m.ollamaModels[index].Reason = "conversation confirmation required"
		}
	}
}

func (m *Model) revokeOllamaCloudConsent() {
	currentWasCloud := false
	current := config.CanonicalModelName(m.model)
	for _, descriptor := range m.ollamaModels {
		if descriptor.Source == OllamaModelCloud && config.CanonicalModelName(descriptor.Name) == current {
			currentWasCloud = true
			break
		}
	}
	if m.modelManager != nil {
		m.modelManager.RevokeOllamaCloudGrants()
	}
	for index := range m.ollamaModels {
		if m.ollamaModels[index].Source != OllamaModelCloud || !m.localOnly {
			continue
		}
		m.ollamaModels[index].ConsentGranted = false
		m.ollamaModels[index].RequiresConsent = true
		m.ollamaModels[index].Reason = "conversation confirmation required"
	}
	if !currentWasCloud {
		return
	}
	fallback := m.firstLocalAutoModel()
	if fallback == "" {
		m.modelPinned = true
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Ollama Cloud consent ended. Select a local model before sending the next turn."})
		return
	}
	if err := m.switchToLocalFallback(fallback); err != nil {
		m.modelPinned = true
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: fmt.Sprintf("Ollama Cloud consent ended; local fallback failed: %v", err)})
		return
	}
	m.modelPinned = false
	m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf("Cloud consent ended · resumed local model %s", fallback)})
}

func (m *Model) currentModelUsesOllamaCloud() bool {
	current := config.CanonicalModelName(m.model)
	for _, descriptor := range m.ollamaModels {
		if descriptor.Source == OllamaModelCloud && config.CanonicalModelName(descriptor.Name) == current {
			return true
		}
	}
	return false
}

func (m *Model) firstLocalAutoModel() string {
	for _, descriptor := range m.ollamaModels {
		if descriptor.Source == OllamaModelLocal && descriptor.Selectable && descriptor.Fit && descriptor.AutoRoutable {
			return descriptor.Name
		}
	}
	return ""
}

func (m *Model) switchToLocalFallback(name string) error {
	if name == "" {
		return fmt.Errorf("no local automatic model is available")
	}
	if m.modelManager != nil {
		m.prepareModelSwitch()
		if err := m.modelManager.SetCurrentModel(name); err != nil {
			return err
		}
	}
	m.setCurrentModelProjection(name)
	for index := range m.ollamaModels {
		m.ollamaModels[index].Current = config.CanonicalModelName(m.ollamaModels[index].Name) == config.CanonicalModelName(name)
	}
	return nil
}

func (m *Model) enableAutomaticModelRouting() error {
	// Prove the cloud-to-local transition is possible before mutating the
	// durable preference. Once this preflight passes, clearing the preference
	// comes first so a failed provider switch cannot silently re-pin the model
	// on the next process start.
	if m.currentModelUsesOllamaCloud() && m.firstLocalAutoModel() == "" {
		return fmt.Errorf("automatic routing needs an available local model; the current Ollama Cloud model remains pinned")
	}
	if err := m.clearManualModelPreference(); err != nil {
		return err
	}
	if m.currentModelUsesOllamaCloud() {
		cloudModel := m.model
		fallback := m.firstLocalAutoModel()
		if err := m.switchToLocalFallback(fallback); err != nil {
			return fmt.Errorf("switch to local automatic model: %w", err)
		}
		if m.modelManager != nil {
			m.modelManager.RevokeOllamaCloudGrants()
		}
		m.setCloudConsentProjection(cloudModel, false)
	}
	m.modelPinned = false
	return nil
}
