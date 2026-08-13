package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

type providerSwitchResultMsg struct {
	Token uint64
	Name  string
	Text  string
	Err   error
}

// ProviderFailureCopy is host-owned presentation for provider failures. Raw
// provider/configuration errors can contain endpoints or backend prose and must
// never be sent to the transcript boundary.
const ProviderFailureCopy = "Provider unavailable. Check the provider profile and credential environment."

func (m *Model) activeProviderName() string {
	if m.modelManager == nil {
		return "ollama"
	}
	if name := strings.TrimSpace(m.modelManager.ActiveProviderName()); name != "" {
		return name
	}
	if m.modelManager.RemoteProvider() {
		return m.modelManager.RemoteProviderLabel()
	}
	return "ollama"
}

func (m *Model) providerNames() []string {
	if m.modelManager == nil {
		return []string{"ollama"}
	}
	names := m.modelManager.ProviderNames()
	if len(names) == 0 {
		return []string{"ollama"}
	}
	return names
}

// beginProviderSwitch moves provider admission and local inventory refresh out
// of Bubble Tea's event loop. A token prevents a late cancelled result from
// repainting a newer selection.
func (m *Model) beginProviderSwitch(name, receiptText string) tea.Cmd {
	name = strings.TrimSpace(name)
	if m.providerSwitchCancel != nil {
		m.providerSwitchCancel()
	}
	m.providerSwitchToken++
	token := m.providerSwitchToken
	ctx, cancel := context.WithCancel(context.Background())
	m.providerSwitchCancel = cancel
	m.providerSwitchRunning = true
	m.providerSwitchName = sanitizeTerminalSingleLine(name)
	m.input.Blur()
	// The busy activity rail is a footer owner. Reproject immediately so the
	// transcript cannot paint into rows that changed ownership in this Update.
	m.recalcViewportHeight()

	manager := m.modelManager
	run := func() tea.Msg {
		var err error
		switch {
		case name == "":
			err = fmt.Errorf("provider name is empty")
		case manager == nil:
			err = fmt.Errorf("model manager is unavailable")
		default:
			err = manager.SwitchProviderContext(ctx, name)
		}
		return providerSwitchResultMsg{
			Token: token,
			Name:  name,
			Text:  receiptText,
			Err:   err,
		}
	}
	return tea.Batch(m.startActivityCmd(), run)
}

func (m *Model) handleProviderSwitchResult(msg providerSwitchResultMsg, cmds []tea.Cmd) []tea.Cmd {
	if !m.providerSwitchRunning || msg.Token != m.providerSwitchToken {
		return cmds
	}
	m.providerSwitchRunning = false
	m.providerSwitchName = ""
	if m.providerSwitchCancel != nil {
		m.providerSwitchCancel()
		m.providerSwitchCancel = nil
	}

	followWasPaused := m.followPaused()
	followYOffset := m.transcriptYOffset()
	if !m.shuttingDown {
		switch {
		case errors.Is(msg.Err, context.Canceled):
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Provider switch cancelled."})
		case msg.Err != nil:
			// Manager/configuration errors may carry transport URLs or provider
			// prose. Keep the raw error transient and render only host-owned copy so
			// credentials can never enter the transcript or session snapshot.
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: ProviderFailureCopy})
		default:
			m.applySwitchedProvider(msg.Name)
			text := sanitizeTerminalSingleLine(strings.TrimSpace(msg.Text))
			if text == "" {
				text = fmt.Sprintf("Provider: %s", sanitizeTerminalSingleLine(m.activeProviderName()))
			}
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: fmt.Sprintf("%s · model %s", text, sanitizeTerminalSingleLine(m.model)),
			})
		}
		m.input.Focus()
		m.invalidateEntryCache()
		if m.ready {
			m.refreshTranscript()
			m.restoreFollowPosition(followWasPaused, followYOffset)
		}
		m.recalcViewportHeight()
	}
	m.appendShutdownQuit(&cmds)
	return cmds
}

// applySwitchedProvider reconciles presentation state after ModelManager has
// atomically committed the provider runtime.
func (m *Model) applySwitchedProvider(name string) {
	m.model = m.modelManager.Model()
	m.modelPinned = true
	if m.modelManager.RemoteProvider() {
		m.modelList = config.SelectableRemoteModels(m.activeProviderName(), m.model)
		if policy := m.modelManager.ContextPolicy(m.model); policy.Effective > 0 {
			m.numCtx = policy.Effective
		}
	} else {
		// Restore Ollama context allocation when returning from a remote profile.
		if m.modelManager.NumCtx() > 0 {
			m.numCtx = m.modelManager.NumCtx()
		}
		// Keep modelList as previously discovered Ollama inventory when present.
		if len(m.modelList) == 0 && m.model != "" {
			m.modelList = []string{m.model}
		}
	}
	if m.completer != nil {
		m.completer.UpdateModels(m.modelList)
		m.completer.UpdateProviders(m.providerNames())
	}
	// Remote providers do not use local memory-admission for model names.
	if !m.modelManager.RemoteProvider() {
		if err := config.CheckModelMemorySafe(m.model); err != nil {
			// Soft warning only: SwitchProvider already selected the model.
			_ = err
		}
	}
	m.saveManualProviderPreference(name)
}
