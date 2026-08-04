package ui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

// requestSessionRestore is the sole asynchronous session-read authority for
// picker and startup restores. It owns one generation, cancellation context,
// exact workspace scope, and execution lease through the terminal receipt.
func (m *Model) requestSessionRestore(selector SessionResumeSelector) tea.Cmd {
	if m.sessionLoadCancel != nil {
		m.sessionLoadCancel()
		m.sessionLoadCancel = nil
	}
	m.sessionLoadToken++
	loadToken := m.sessionLoadToken
	m.sessionLoading = true
	m.input.Blur()

	store := m.sessionStore
	workDir := ""
	if m.agent != nil {
		workDir = m.agent.WorkDir()
	}
	activeSessionID := m.sessionID
	activeSessionLeased := m.executionLease != nil
	loadCtx, loadCancel := context.WithCancel(context.Background())
	m.sessionLoadCancel = loadCancel

	load := func() tea.Msg {
		if store == nil {
			return SessionLoadedMsg{LoadToken: loadToken, Err: fmt.Errorf("session persistence is unavailable")}
		}
		workspaceID, err := canonicalWorkspaceID(workDir)
		if err != nil {
			return SessionLoadedMsg{LoadToken: loadToken, Err: err}
		}
		sessionID, err := selector.resolve(loadCtx, store, workspaceID)
		if err != nil {
			return SessionLoadedMsg{LoadToken: loadToken, Err: err}
		}

		var lease *db.ExecutionSessionLease
		if sessionID != activeSessionID || !activeSessionLeased {
			lease, err = store.AcquireExecutionSessionLease(loadCtx, sessionID, workspaceID)
			if err != nil {
				return SessionLoadedMsg{LoadToken: loadToken, Err: err}
			}
		}
		session, state, stateRecord, err := loadPersistedSession(loadCtx, store, sessionID, workspaceID)
		if err != nil {
			if lease != nil {
				_ = lease.Close()
			}
			return SessionLoadedMsg{LoadToken: loadToken, Err: err}
		}

		var (
			unresolved       []execution.State
			recoveryTarget   *agent.UnresolvedExecutionError
			recoveryContexts []db.StandaloneReconciliationContext
			unresolvedErr    error
		)
		if state.Goal == nil {
			projection, projectErr := store.ProjectExecutionRecovery(loadCtx, session.ID, workspaceID, state.ExecutionCursor, 100)
			if projectErr != nil {
				if lease != nil {
					_ = lease.Close()
				}
				return SessionLoadedMsg{LoadToken: loadToken, Err: fmt.Errorf("project execution recovery: %w", projectErr)}
			}
			unresolved, recoveryContexts = projection.Hazards, projection.Contexts
			recoveryTarget = standaloneRecoveryTarget(unresolved, state.ExecutionCursor, session.PublicID)
		} else {
			unresolved, unresolvedErr = store.ListExecutionRecoveryHazards(loadCtx, session.ID, workspaceID, state.ExecutionCursor, 100)
		}
		warning := unresolvedExecutionWarning(unresolved, state.Goal != nil)
		if unresolvedErr != nil {
			warning = fmt.Sprintf("Recovery check failed: %v. This session will remain blocked until durable execution state can be verified.", unresolvedErr)
		}
		if recoveryTarget != nil && sessionref.Valid(session.PublicID) {
			recoveryTarget.SessionPublicID = session.PublicID
		}
		return SessionLoadedMsg{
			LoadToken: loadToken, SessionID: session.ID, SessionPublicID: session.PublicID, State: state,
			StateRecord: stateRecord, Title: sanitizeTerminalSingleLine(session.Title), RecoveryWarning: warning,
			RecoveryTarget: recoveryTarget, RecoveryContexts: recoveryContexts, ExecutionLease: lease,
		}
	}
	return tea.Batch(m.startActivityCmd(), load)
}
