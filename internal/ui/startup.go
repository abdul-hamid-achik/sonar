package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// sanitizeStartupDetail keeps transport and server errors from injecting
// multiline/unbounded content into the startup screen. Full failures remain
// available in the durable system entry and logs after initialization.
func sanitizeStartupDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	if detail[0] == '{' || detail[0] == '[' {
		return "details available in logs"
	}
	detail = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(detail)
	return strings.Join(strings.Fields(detail), " ")
}

// handleStartupStatus records a startup item's status and repaints once the
// viewport is ready.
func (m *Model) handleStartupStatus(msg StartupStatusMsg) {
	// "provider" is the ID the shipped startup path sends; "ollama" survives
	// for the inherited message shape. This flag was matched on "ollama"
	// alone, which the remote path never sends — so a hosted provider that
	// failed its ping never marked the model offline beside its name, the
	// exact signal model_location exists to carry.
	if msg.ID == "provider" || msg.ID == "ollama" {
		m.providerOffline = msg.Status == "failed"
	}
	found := false
	for i, item := range m.startupItems {
		if item.ID == msg.ID {
			m.startupItems[i].Status = msg.Status
			m.startupItems[i].Detail = msg.Detail
			found = true
			break
		}
	}
	if !found {
		m.startupItems = append(m.startupItems, startupItem(msg))
	}
	if m.ready {
		m.refreshTranscript()
	}
}

// handleCoreReady opens turn admission after the host has atomically applied
// the startup model/profile. Optional MCP, ICE, and metadata initialization may
// continue while the composer is usable.
func (m *Model) handleCoreReady(msg CoreReadyMsg) {
	m.setCurrentModelProjection(msg.Model)
	m.ollamaModels = msg.OllamaModels
	m.modelList = append([]string(nil), msg.ModelList...)
	m.ollamaInventoryAttempted = msg.OllamaInventoryAttempted
	m.setActiveProfileMetadata(msg.AgentProfile)
	if msg.NumCtx > 0 {
		m.numCtx = msg.NumCtx
	}
	m.syncEffectiveContext(false)
	m.turnReady = true
	if m.completer != nil {
		m.completer.UpdateModels(m.modelList)
	}
	if m.ready {
		m.refreshTranscript()
	}
}

// handleInitComplete applies the completed initialization snapshot and ends
// the startup phase.
func (m *Model) handleInitComplete(msg InitCompleteMsg, cmds []tea.Cmd) []tea.Cmd {
	m.setCurrentModelProjection(msg.Model)
	m.ollamaModels = msg.OllamaModels
	m.modelList = append([]string(nil), msg.ModelList...)
	m.ollamaVersion = msg.OllamaVersion
	m.ollamaInventoryAttempted = msg.OllamaInventoryAttempted
	m.setActiveProfileMetadata(msg.AgentProfile)
	m.agentList = msg.AgentList
	m.toolCount = msg.ToolCount
	m.serverCount = msg.ServerCount
	m.numCtx = msg.NumCtx
	m.syncEffectiveContext(false)
	m.applyInitialMCPStatus(msg.MCPServers, msg.FailedServers)
	m.iceEnabled = msg.ICEEnabled
	m.iceConversations = msg.ICEConversations
	m.iceSessionID = msg.ICESessionID

	if m.completer != nil {
		m.completer.UpdateModels(m.modelList)
		m.completer.UpdateAgents(msg.AgentList)
	}

	if len(m.failedServers) > 0 {
		parts := make([]string, 0, len(m.failedServers))
		for _, fs := range m.failedServers {
			parts = append(parts, fs.Name)
		}
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: "MCP unavailable: " + strings.Join(parts, ", ") + ". Open Runtime status for recovery guidance.",
		})
	}

	m.turnReady = true
	m.initializing = false
	m.startupItems = nil

	// Startup and the interactive composer reserve different footer geometry.
	// Reflow immediately so the first usable frame does not keep a stale blank
	// row until the next resize or input event.
	m.recalcViewportHeight()
	m.refreshTranscript()
	if m.startupResumeSelector != nil {
		selector := *m.startupResumeSelector
		m.startupResumeSelector = nil
		if !m.shuttingDown {
			cmds = append(cmds, m.requestSessionRestore(selector))
		}
	}
	return cmds
}
