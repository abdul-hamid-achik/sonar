package ui

import (
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// handleSessionLoadedReceipt validates the owned async receipt before either
// committing it or pausing on an explicit Ollama Cloud boundary. A pending
// execution lease stays owned by the confirmation and is closed on cancel.
func (m *Model) handleSessionLoadedReceipt(message SessionLoadedMsg) tea.Cmd {
	if !m.sessionLoading || message.LoadToken != m.sessionLoadToken {
		if message.ExecutionLease != nil {
			_ = message.ExecutionLease.Close()
		}
		return nil
	}
	m.sessionLoading = false
	if m.sessionLoadCancel != nil {
		m.sessionLoadCancel()
	}
	m.sessionLoadCancel = nil
	if m.shuttingDown {
		if message.ExecutionLease != nil {
			_ = message.ExecutionLease.Close()
		}
		return nil
	}
	if m.state != StateIdle {
		if message.ExecutionLease != nil {
			_ = message.ExecutionLease.Close()
		}
		return nil
	}
	m.input.Focus()
	m.invalidateEntryCache()
	if message.Err != nil {
		m.failLoadedSession(message, message.Err)
		return nil
	}
	if err := validateLoadedSessionStateRecord(message.SessionID, message.StateRecord); err != nil {
		m.failLoadedSession(message, err)
		return nil
	}
	// Bind an interactive switch to its exact settled target before a loaded
	// cloud model can open a new authority surface. A receipt for another session
	// fails closed without asking the user to authorize that session's model.
	if err := m.validatePendingSessionSwitch(message); err != nil {
		m.failLoadedSession(message, err)
		return nil
	}
	if descriptor, ok := m.ollamaModelDescriptor(message.State.Model); ok && m.localOnly && descriptor.Source == OllamaModelCloud {
		m.openCloudConsentForSession(descriptor, message)
		return nil
	}
	_, cmd := m.finishLoadedSession(message)
	return cmd
}

func (m *Model) finishLoadedSession(message SessionLoadedMsg) (bool, tea.Cmd) {
	if err := validateLoadedSessionStateRecord(message.SessionID, message.StateRecord); err != nil {
		m.failLoadedSession(message, err)
		return false, nil
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		m.failLoadedSession(message, err)
		return false, nil
	}
	if err := validateLoadedStandaloneRecoveryMetadata(message, workspaceID); err != nil {
		m.failLoadedSession(message, err)
		return false, nil
	}
	if err := validateStandaloneReconciliationContexts(message.RecoveryContexts); err != nil {
		m.failLoadedSession(message, fmt.Errorf("validate durable recovery context: %w", err))
		return false, nil
	}
	if err := m.validatePendingSessionSwitch(message); err != nil {
		m.failLoadedSession(message, err)
		return false, nil
	}
	m.preemptTranscriptSearch()
	if err := m.restoreSessionState(message.State); err != nil {
		m.failLoadedSession(message, err)
		return false, nil
	}
	targetIsCloud := false
	if descriptor, ok := m.ollamaModelDescriptor(message.State.Model); ok {
		targetIsCloud = m.localOnly && descriptor.Source == OllamaModelCloud
	}
	if !targetIsCloud {
		m.revokeOllamaCloudConsent()
	}
	m.resetGoalRecoveryPresentation()
	if message.ExecutionLease != nil {
		_ = m.releaseExecutionSessionLease()
		m.executionLease = message.ExecutionLease
	}
	m.sessionID = message.SessionID
	m.sessionPublicID = message.SessionPublicID
	m.activeSessionTitle = sanitizeTerminalSingleLine(message.Title)
	// Restored titles are durable as authored; do not re-title in the background.
	m.sessionTitleNeedsAI = false
	m.sessionTitleAIDone = true
	_ = m.initializeSessionStateRevision(message.StateRecord.Revision)
	m.agent.SetCheckpointSessionID(message.SessionID)
	m.agent.SetExecutionSessionID(message.SessionID, message.SessionPublicID)
	m.agent.SetExecutionSnapshotCursor(m.executionCursor)
	m.standaloneRecovery = nil
	if m.goalRuntime == nil {
		m.rememberStandaloneRecovery(message.RecoveryTarget)
	}
	if err := m.installStandaloneReconciliationContexts(message.RecoveryContexts); err != nil {
		m.failLoadedSession(message, fmt.Errorf("restore recovery context: %w", err))
		return false, nil
	}
	m.applyPendingSessionSwitchSuccess(message)
	var cmd tea.Cmd
	if err := m.recoverRestoredGoal(); err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: fmt.Sprintf("Restore goal recovery: %v", err)})
	} else {
		cmd = m.ensureCurrentGoalRecoveryProjection(false)
		if decisionCmd := m.beginRestoredCortexDecisionRefresh(); decisionCmd != nil {
			cmd = tea.Batch(cmd, decisionCmd)
		}
	}
	m.entries = append(m.entries, ChatEntry{Kind: "system", Content: fmt.Sprintf(
		"Restored session %s", sessionDisplayLabel(message.SessionPublicID, m.activeSessionTitle, 72),
	)})
	if message.RecoveryWarning != "" {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: message.RecoveryWarning})
	}
	m.refreshTranscript()
	m.resumeFollow()
	return true, cmd
}

func validateLoadedStandaloneRecoveryMetadata(message SessionLoadedMsg, workspaceID string) error {
	if message.State.Goal != nil {
		if message.RecoveryTarget != nil || len(message.RecoveryContexts) != 0 {
			return errors.New("goal session carried ordinary execution recovery metadata")
		}
		return nil
	}
	target := message.RecoveryTarget
	if target == nil {
		return nil
	}
	if target.SessionID != message.SessionID || target.WorkspaceID != workspaceID ||
		target.SnapshotCursor != message.State.ExecutionCursor || target.RecoveryInspectCommand() == "" {
		return errors.New("ordinary execution recovery metadata does not match the restored session")
	}
	return nil
}

func (m *Model) failLoadedSession(message SessionLoadedMsg, err error) {
	if message.ExecutionLease != nil {
		_ = message.ExecutionLease.Close()
	}
	if m.pendingSessionSwitch != nil && (m.pendingSessionSwitch.LoadToken == 0 || m.pendingSessionSwitch.LoadToken == message.LoadToken) {
		m.restoreAndClearPendingSessionSwitch()
	}
	m.entries = append(m.entries, ChatEntry{Kind: "error", Content: fmt.Sprintf("Load session: %v", err)})
	m.refreshTranscript()
	m.resumeFollow()
}
