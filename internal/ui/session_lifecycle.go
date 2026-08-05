package ui

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
)

// ensureExecutionSession creates or reacquires the durable session boundary
// before a turn (or a Goal Runtime) can own work. Keeping this operation
// separate lets explicit goal creation bind Cortex and persist its state before
// the first provider command is dispatched.
func (m *Model) ensureExecutionSession(title, modeLabel string) (bool, error) {
	if m.sessionStore == nil {
		return false, nil
	}
	if m.sessionID > 0 {
		if m.executionLease != nil {
			return false, nil
		}
		workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
		if err != nil {
			return false, fmt.Errorf("lock session: %w", err)
		}
		lease, err := m.sessionStore.AcquireExecutionSessionLease(context.Background(), m.sessionID, workspaceID)
		if err != nil {
			return false, fmt.Errorf("lock session: %w", err)
		}
		m.executionLease = lease
		return false, nil
	}
	m.resetSessionStateRevision()

	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		return false, fmt.Errorf("create session: %w", err)
	}
	title = sessionTitle(title)
	session, err := m.sessionStore.CreateSession(context.Background(), db.CreateSessionParams{
		Title: title, Model: m.model, Mode: modeLabel, WorkspaceID: workspaceID,
	})
	if err != nil {
		return false, fmt.Errorf("create session: %w", err)
	}
	lease, leaseErr := m.sessionStore.AcquireExecutionSessionLease(context.Background(), session.ID, session.WorkspaceID)
	if leaseErr != nil {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), time.Second)
		cleanupErr := m.sessionStore.DeleteSession(cleanupCtx, session.ID)
		cancelCleanup()
		if cleanupErr != nil {
			leaseErr = errors.Join(leaseErr, fmt.Errorf("cleanup session: %w", cleanupErr))
		}
		return false, fmt.Errorf("lock session: %w", leaseErr)
	}
	m.sessionID = session.ID
	m.sessionPublicID = session.PublicID
	m.activeSessionTitle = session.Title
	// Provisional title is the first prompt line; background AI renames once
	// the first turn provides goal-shaped context.
	m.sessionTitleNeedsAI = true
	m.sessionTitleAIDone = false
	m.executionCursor = 0
	m.executionLease = lease
	if err := m.initializeSessionStateRevision(0); err != nil {
		return false, fmt.Errorf("initialize session state revision: %w", err)
	}
	m.agent.SetCheckpointSessionID(session.ID)
	m.agent.SetExecutionSessionID(session.ID, session.PublicID)
	m.agent.SetExecutionSnapshotCursor(0)
	return true, nil
}

// discardCreatedExecutionSession removes a session that failed before its
// first turn became durable. The caller must use this only for a session it
// created in the current send path; restored or pre-existing sessions must
// never be deleted as error cleanup.
func (m *Model) discardCreatedExecutionSession() error {
	sessionID := m.sessionID
	leaseErr := m.releaseExecutionSessionLease()
	var deleteErr error
	if m.sessionStore != nil && sessionID > 0 {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), time.Second)
		deleteErr = m.sessionStore.DeleteSession(cleanupCtx, sessionID)
		cancelCleanup()
	}
	m.cancelSessionTitleGen()
	m.sessionID = 0
	m.sessionPublicID = ""
	m.activeSessionTitle = ""
	m.sessionTitleNeedsAI = false
	m.sessionTitleAIDone = false
	m.executionCursor = 0
	m.resetSessionStateRevision()
	if m.agent != nil {
		m.agent.SetCheckpointSessionID(0)
		m.agent.SetExecutionSessionID(0, "")
		m.agent.SetExecutionSnapshotCursor(0)
	}
	return errors.Join(leaseErr, deleteErr)
}

// executionProjectionBoundaryTimeout bounds the durable work one projection
// boundary may perform on the update thread. It matches the settlement budget
// in handleAgentDone so neither boundary can stall the event loop longer than
// the other.
const executionProjectionBoundaryTimeout = 2 * time.Second

// advanceExecutionProjectionBoundary settles the durable projection exactly the
// way turn settlement does: prove every post-cursor hazard is already answered
// in the transcript, save that transcript, and only then advance the snapshot
// cursor that the agent's strict pre-provider scan reads.
//
// An AUTO segment boundary needs this. The previous Run settled every receipt
// into the message history before it returned, so the projection check passes
// by construction; skipping it left the next segment scanning from the old
// cursor, where its own completed non-read-only effects look like unprojected
// hazards and kill the run before any provider work. Advancing here also makes
// a long AUTO run crash-safe per segment instead of only per logical turn.
//
// Callers must hold the update thread with no turn running: this writes the
// cursor the agent reads under its own lock, and there is no second writer
// while the agent is idle between segments.
func (m *Model) advanceExecutionProjectionBoundary() error {
	if m == nil || m.agent == nil || m.sessionStore == nil || m.sessionID <= 0 {
		return nil
	}
	if m.executionLease == nil {
		return errors.New("the execution session lease is unavailable, so the snapshot cursor cannot advance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), executionProjectionBoundaryTimeout)
	defer cancel()
	previousCursor := m.executionCursor
	cursor, err := m.snapshotExecutionCursor(ctx)
	if err != nil {
		return err
	}
	m.executionCursor = cursor
	if err := m.persistSessionState(ctx); err != nil {
		m.executionCursor = previousCursor
		return err
	}
	m.agent.SetExecutionSnapshotCursor(m.executionCursor)
	return nil
}

func (m *Model) snapshotExecutionCursor(ctx context.Context) (int64, error) {
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		return m.executionCursor, err
	}
	hazards, err := m.sessionStore.ListExecutionRecoveryHazards(ctx, m.sessionID, workspaceID, m.executionCursor, 100)
	if err != nil {
		return m.executionCursor, fmt.Errorf("inspect execution projection: %w", err)
	}
	messages := m.agent.Messages()
	for _, state := range hazards {
		if state.Latest.Type != execution.EventCompleted && state.Latest.Type != execution.EventFailed {
			return m.executionCursor, fmt.Errorf(
				"execution %s remains %s/%s and cannot cross the snapshot boundary",
				state.Identity.ExecutionID, state.Latest.Type, state.Identity.EffectClass,
			)
		}
		projected := false
		for _, message := range messages {
			resultContent := message.Content
			if message.DurableContent != "" {
				resultContent = message.DurableContent
			}
			if message.Role == "tool" &&
				message.ToolCallID == state.Identity.CanonicalCallID &&
				execution.HashText(resultContent) == state.Latest.ResultSHA256 {
				projected = true
				break
			}
		}
		if !projected {
			return m.executionCursor, fmt.Errorf("%s effect %s is absent from the session snapshot", state.Latest.Type, state.Identity.ExecutionID)
		}
	}
	latest, err := m.sessionStore.LatestExecutionEventID(ctx, m.sessionID, workspaceID)
	if err != nil {
		return m.executionCursor, fmt.Errorf("read execution cursor: %w", err)
	}
	return latest, nil
}

func (m *Model) resetConversationSession() {
	m.preemptTranscriptSearch()
	m.clearViewerModals(false)
	m.outputDetails = NewOutputDetailStore()
	m.resetEntryMemo()
	m.clearQueuedFollowUpForSessionReplacement()
	m.clearBobWorkspaceContext()
	if m.imageAttachCancel != nil {
		m.imageAttachCancel()
	}
	m.imageAttachCancel = nil
	m.imageAttachToken++
	m.imageAttachRunning = false
	m.imageAttachFallback = ""
	m.clearImageAttachmentQueue()
	m.clearPendingImages()
	m.turnImages = nil
	if m.goalOperationCancel != nil {
		m.goalOperationCancel()
	}
	m.goalOperationCancel = nil
	m.goalOperation = ""
	m.goalOperationRunning = false
	m.cortexDecision = nil
	m.cortexDecisionOp = nil
	m.cortexDecisionAttempt = nil
	m.cortexDecisionGen++
	m.standaloneRecovery = nil
	m.goalOperationToken++
	m.goalRuntime = nil
	m.goalPlan = nil
	m.syncComposerAuthority()
	m.goalFormState = nil
	m.goalInspectorState = nil
	m.resetGoalRecoveryPresentation()
	m.goalTurnID = ""
	m.goalTurnToolCalls = 0
	m.goalTurnSuccesses = 0
	m.goalNeedsEvaluation = false
	m.goalPersistenceDirty = false
	m.cancelSessionLoad()
	m.cancelSessionList()
	m.cancelSessionTitleGen()
	m.sessionID = 0
	m.sessionPublicID = ""
	m.activeSessionTitle = ""
	m.sessionTitleNeedsAI = false
	m.sessionTitleAIDone = false
	m.executionCursor = 0
	m.resetSessionStateRevision()
	_ = m.releaseExecutionSessionLease()
	if m.agent != nil {
		m.agent.SetCheckpointSessionID(0)
		m.agent.SetExecutionSessionID(0, "")
		m.agent.SetExecutionSnapshotCursor(0)
	}
	m.sessionEvalTotal = 0
	m.sessionPromptTotal = 0
	m.sessionTurnCount = 0
	m.resetTurnDiagnostics()
	m.fileChanges = nil
	m.toolsPending = 0
	_ = m.revokeTemporaryWriteScopes()
}

// ReleaseExecutionSessionLease releases the cross-process ownership held by
// the active interactive session. The main program calls it after Bubble Tea
// has joined the current turn and before SQLite closes.
func (m *Model) ReleaseExecutionSessionLease() error {
	return m.releaseExecutionSessionLease()
}

func (m *Model) releaseExecutionSessionLease() error {
	if m.executionLease == nil {
		return nil
	}
	lease := m.executionLease
	m.executionLease = nil
	return lease.Close()
}

func (m *Model) cancelSessionLoad() {
	if m.sessionLoadCancel != nil {
		m.sessionLoadCancel()
		m.sessionLoadCancel = nil
	}
	if m.sessionLoading {
		m.sessionLoadToken++
	}
	m.sessionLoading = false
	if m.pendingSessionSwitch != nil {
		m.restoreAndClearPendingSessionSwitch()
	}
	if !m.sessionListing {
		m.input.Focus()
	}
}

func (m *Model) cancelSessionLoadForShutdown() {
	if m.sessionLoadCancel != nil {
		m.sessionLoadCancel()
		m.sessionLoadCancel = nil
		return
	}
	// Tests and embedders can mark a synthetic load without installing an
	// owned command. There is nothing to join in that case.
	m.sessionLoading = false
}

func (m *Model) cancelSessionList() {
	if m.sessionListing {
		m.sessionListToken++
	}
	if m.sessionListCancel != nil {
		m.sessionListCancel()
		m.sessionListCancel = nil
	}
	m.sessionListing = false
	if !m.sessionLoading {
		m.input.Focus()
	}
}

func (m *Model) cancelSessionListForShutdown() {
	if m.sessionListCancel != nil {
		m.sessionListCancel()
		m.sessionListCancel = nil
		return
	}
	// Tests and embedders can mark a synthetic lookup without installing an
	// owned command. There is nothing to join in that case.
	m.sessionListing = false
}
