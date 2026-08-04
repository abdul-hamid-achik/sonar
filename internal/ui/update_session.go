package ui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// handleSessionList applies a sessions listing receipt to the picker overlay.
func (m *Model) handleSessionList(msg SessionListMsg) {
	if !m.sessionListing || msg.ListToken != m.sessionListToken {
		return
	}
	if m.sessionListCancel != nil {
		m.sessionListCancel()
		m.sessionListCancel = nil
	}
	m.sessionListing = false
	if m.shuttingDown {
		return
	}
	if m.state != StateIdle {
		m.sessionsPickerState = nil
		if m.overlay == OverlaySessionsPicker {
			m.overlayParent = OverlayNone
			m.overlay = OverlayNone
		}
		return
	}
	m.preemptTranscriptSearch()
	if msg.Err != nil {
		m.sessionsPickerState = newSessionsMessageState(sessionsFailed, msg.Err.Error())
		m.overlay = OverlaySessionsPicker
	} else if len(msg.Sessions) == 0 {
		m.sessionsPickerState = newSessionsMessageState(sessionsEmpty, "")
		m.overlay = OverlaySessionsPicker
	} else {
		m.sessionsPickerState = newSessionsPickerState(msg.Sessions, m.width, m.height, m.isDark, m.themeID, m.reducedMotion)
		m.restylePickerOverlays()
		m.overlay = OverlaySessionsPicker
	}
	m.input.Blur()
}

// handleImportResult commits a completed conversation import receipt.
func (m *Model) handleImportResult(msg ImportResultMsg) {
	if !m.fileLoading || msg.Token != m.fileOpToken {
		return
	}
	m.fileLoading = false
	if !m.shuttingDown {
		m.input.Focus()
	}
	if msg.Err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: fmt.Sprintf("Import failed: %v", msg.Err)})
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
		return
	}

	// Commit the visible and model transcripts together, and detach from
	// the previous persisted session. The typed export intentionally omits
	// tool authority and hidden runtime state.
	m.agent.ReplaceMessages(msg.Messages)
	m.entries = msg.Entries
	m.toolEntries = nil
	m.resetConversationSession()
	m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf(
		"Imported %d user/assistant messages into a new session. %d display-only system sections were not sent to the model; %d tool sections were omitted because Markdown does not preserve safe tool-call state.",
		len(msg.Messages), msg.UIOnlySections, msg.ToolSections,
	)})
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.resumeFollow()
}

// handleExportResult applies a completed conversation export receipt; it
// returns the accumulated commands slice.
func (m *Model) handleExportResult(msg ExportResultMsg, cmds []tea.Cmd) []tea.Cmd {
	if !m.exportRunning || msg.Token != m.exportToken {
		return cmds
	}
	m.exportRunning = false
	if !m.shuttingDown {
		m.input.Focus()
		if exportWasPublished(msg.Err) {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf("Exported conversation to: %s. Durability warning (do not retry blindly): %v", msg.Path, msg.Err)})
		} else if msg.Err != nil {
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: fmt.Sprintf("Export failed: %v", msg.Err)})
		} else {
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf("Exported conversation to: %s", msg.Path)})
		}
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
	}
	m.appendShutdownQuit(&cmds)
	return cmds
}
