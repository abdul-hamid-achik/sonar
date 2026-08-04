package ui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
)

type standaloneRecoveryState struct {
	target     agent.UnresolvedExecutionError
	inspection db.StandaloneExecutionReconciliationInspection
	token      uint64
	loading    bool
	applying   bool
}

type standaloneRecoveryInspectResultMsg struct {
	token      uint64
	sessionID  int64
	workspace  string
	execution  string
	inspection db.StandaloneExecutionReconciliationInspection
	err        error
}

type standaloneRecoveryApplyResultMsg struct {
	token      uint64
	inspection db.StandaloneExecutionReconciliationInspection
	receipt    db.StandaloneExecutionReconciliationReceipt
	// remaining is the count of executions still pending reconciliation after
	// this resolution committed, or -1 when the recount was unavailable.
	remaining int
	err       error
}

func (m *Model) rememberStandaloneRecovery(unresolved *agent.UnresolvedExecutionError) {
	if m == nil || m.goalRuntime != nil || unresolved == nil || unresolved.RecoveryInspectCommand() == "" {
		return
	}
	copy := *unresolved
	if current := m.standaloneRecovery; current != nil &&
		current.target.SessionID == copy.SessionID && current.target.ExecutionID == copy.ExecutionID {
		return
	}
	m.standaloneRecovery = &standaloneRecoveryState{target: copy}
}

func (m *Model) openStandaloneRecovery() tea.Cmd {
	if m == nil {
		return nil
	}
	state := m.standaloneRecovery
	if state == nil {
		m.appendStandaloneRecoveryError("No paused ordinary execution is available to recover.")
		return nil
	}
	if state.loading || state.applying {
		return nil
	}
	if m.goalRuntime != nil {
		m.appendStandaloneRecoveryError("This execution belongs to a durable goal; use the goal recovery inspector.")
		return nil
	}
	if m.state != StateIdle || m.sessionStore == nil || m.executionLease == nil || m.agent == nil {
		m.appendStandaloneRecoveryError("Recovery requires an idle session with its active durable lease.")
		return nil
	}
	if state.target.SessionID <= 0 || state.target.SessionID != m.sessionID || strings.TrimSpace(state.target.ExecutionID) == "" {
		m.appendStandaloneRecoveryError("The paused execution no longer belongs to this session.")
		return nil
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil || state.target.WorkspaceID != "" && state.target.WorkspaceID != workspaceID {
		m.appendStandaloneRecoveryError("The paused execution no longer belongs to this workspace.")
		return nil
	}
	m.sessionStateMu.RLock()
	known, dirty := m.sessionStateRevisionKnown, m.sessionStatePersistenceDirty
	m.sessionStateMu.RUnlock()
	if !known || dirty {
		m.appendStandaloneRecoveryError("Save the current session state before recording recovery evidence.")
		return nil
	}
	state.token++
	token := state.token
	state.loading = true
	store := m.sessionStore
	sessionID, executionID := m.sessionID, state.target.ExecutionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), goalRecoveryCoordinatorTimeout)
		defer cancel()
		inspection, inspectErr := store.InspectStandaloneExecutionReconciliation(ctx, sessionID, workspaceID, executionID)
		return standaloneRecoveryInspectResultMsg{
			token: token, sessionID: sessionID, workspace: workspaceID, execution: executionID,
			inspection: inspection, err: inspectErr,
		}
	}
}

func (m *Model) handleStandaloneRecoveryInspect(message standaloneRecoveryInspectResultMsg) tea.Cmd {
	if m == nil {
		return nil
	}
	state := m.standaloneRecovery
	if state == nil || !state.loading || state.token != message.token ||
		m.sessionID != message.sessionID || state.target.ExecutionID != message.execution {
		return nil
	}
	state.loading = false
	if m.goalRuntime != nil {
		m.appendStandaloneRecoveryError("Goal authority became active; use the goal recovery inspector.")
		return nil
	}
	if message.err != nil {
		m.appendStandaloneRecoveryError("Could not inspect the immutable execution receipt · " + message.err.Error())
		return nil
	}
	if message.inspection.SessionID != message.sessionID || message.inspection.WorkspaceID != message.workspace ||
		message.inspection.ExecutionID != message.execution || message.inspection.EventID <= 0 || message.inspection.ItemID == "" {
		m.appendStandaloneRecoveryError("The recovery inspection returned an inconsistent execution identity.")
		return nil
	}
	if !m.standaloneRecoveryRevisionCurrent(message.inspection) {
		m.appendStandaloneRecoveryError("Session authority changed; inspect the execution again.")
		return nil
	}
	if message.inspection.Resolved {
		if m.agent == nil {
			m.appendStandaloneRecoveryError("Evidence exists, but the in-session recovery cache is unavailable.")
			return nil
		}
		if err := m.appendStandaloneReconciliationContext(message.inspection.Context); err != nil {
			m.appendStandaloneRecoveryError("Evidence exists, but its bounded host receipt is invalid · " + err.Error())
			return nil
		}
		if err := m.agent.RecheckExecutionRecovery(); err != nil {
			m.appendStandaloneRecoveryError("Evidence exists, but recovery recheck failed · " + err.Error())
			return nil
		}
		m.standaloneRecovery = nil
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Recovery evidence already exists · durable context restored · the next prompt will recheck execution state."})
		m.refreshTranscript()
		m.resumeFollow()
		return nil
	}
	m.preemptTranscriptSearch()
	state.inspection = message.inspection
	item := GoalRecoveryItem{
		ItemID: message.inspection.ItemID, Kind: GoalRecoveryExecutionEffect,
		Subject: message.inspection.ToolName, Tool: message.inspection.ToolName,
		Summary:     standaloneRecoverySummary(message.inspection),
		ExecutionID: message.inspection.ExecutionID, TurnID: message.inspection.TurnID,
		EventType: string(message.inspection.EventType), EffectClass: string(message.inspection.EffectClass),
		Actionable: true,
	}
	m.goalRecoveryState = NewGoalRecovery([]GoalRecoveryItem{item}, GoalRecoveryOptions{
		Width: m.width, Height: m.height, IsDark: m.isDark,
		ReducedMotion: m.reducedMotion, GlyphProfile: m.glyphProfile, Standalone: true,
	})
	m.overlayParent = OverlayNone
	m.overlay = OverlayGoalRecovery
	m.input.Blur()
	return nil
}

func standaloneRecoverySummary(inspection db.StandaloneExecutionReconciliationInspection) string {
	switch inspection.EventType {
	case execution.EventOutcomeUnknown:
		return "Dispatch occurred, but the host cannot verify the effect; inspect the exact target before continuing."
	case execution.EventStarted:
		return "Dispatch started without a terminal receipt; inspect the exact target before continuing."
	default:
		return "The execution requires explicit evidence before this session can continue."
	}
}

func (m *Model) remindStandaloneRecoveryDraftPreserved() {
	if m == nil || m.standaloneRecovery == nil {
		return
	}
	const message = "Chat paused · your draft is still in the composer. Run /recover to inspect the uncertain execution before sending another prompt."
	for index := len(m.entries) - 1; index >= 0; index-- {
		entry := m.entries[index]
		if entry.Kind == "user" {
			break
		}
		if entry.Kind == "system" && entry.Content == message {
			return
		}
	}
	m.entries = append(m.entries, ChatEntry{Kind: "system", Content: message})
	m.refreshTranscript()
	m.resumeFollow()
}

func (m *Model) beginStandaloneRecoveryApply(event GoalRecoveryEvent) tea.Cmd {
	if m == nil {
		return nil
	}
	state := m.standaloneRecovery
	if state == nil || state.applying || state.loading ||
		m.goalRecoveryState == nil || m.overlay != OverlayGoalRecovery {
		return nil
	}
	if m.goalRuntime != nil {
		m.goalRecoveryState.SetError("Goal authority became active; use the goal recovery inspector.")
		return nil
	}
	inspection := state.inspection
	if event.ItemID != inspection.ItemID || inspection.Resolved {
		m.goalRecoveryState.SetError("Recovery target changed; inspect it again before recording evidence.")
		return nil
	}
	evidence, _, err := goalRecoveryTypedEvidence(GoalRecoveryExecutionEffect, event.Draft, m.nowTime().UTC())
	if err != nil {
		m.goalRecoveryState.SetError("Evidence is invalid · " + goalRecoveryCoordinatorError(err))
		return nil
	}
	m.sessionStateMu.RLock()
	revision, known, dirty := m.sessionStateRevision, m.sessionStateRevisionKnown, m.sessionStatePersistenceDirty
	m.sessionStateMu.RUnlock()
	if !known || dirty || revision != inspection.SessionRevision || m.sessionID != inspection.SessionID ||
		m.sessionStore == nil || m.executionLease == nil {
		m.goalRecoveryState.SetError("Session authority changed; inspect the execution again.")
		return nil
	}
	state.token++
	token := state.token
	state.applying = true
	m.goalRecoveryState.SetBusy("Recording immutable evidence…")
	store, lease := m.sessionStore, m.executionLease
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), goalRecoveryCoordinatorTimeout)
		defer cancel()
		receipt, applyErr := store.ResolveStandaloneExecutionReconciliation(ctx, lease, db.ResolveStandaloneExecutionReconciliationRequest{
			SessionID: inspection.SessionID, WorkspaceID: inspection.WorkspaceID,
			ExecutionID: inspection.ExecutionID, ExpectedSessionRevision: inspection.SessionRevision,
			ExpectedEventID: inspection.EventID, Actor: goalActor, Evidence: evidence,
		})
		remaining := -1
		if applyErr == nil {
			if pending, pendingErr := store.ListStandaloneExecutionReconciliationPending(ctx, inspection.SessionID, inspection.WorkspaceID, 100); pendingErr == nil {
				remaining = len(pending)
			}
		}
		return standaloneRecoveryApplyResultMsg{token: token, inspection: inspection, receipt: receipt, remaining: remaining, err: applyErr}
	}
}

func (m *Model) handleStandaloneRecoveryApply(message standaloneRecoveryApplyResultMsg) tea.Cmd {
	if m == nil {
		return nil
	}
	state := m.standaloneRecovery
	if state == nil || !state.applying || state.token != message.token ||
		state.inspection.ItemID != message.inspection.ItemID {
		return nil
	}
	state.applying = false
	if m.goalRecoveryState != nil {
		m.goalRecoveryState.SetBusy("")
	}
	if message.err != nil {
		if m.goalRecoveryState != nil {
			m.goalRecoveryState.SetError("Evidence was not recorded · " + goalRecoveryCoordinatorError(message.err))
		}
		return nil
	}
	r := message.receipt
	if r.SessionID != message.inspection.SessionID || r.WorkspaceID != message.inspection.WorkspaceID ||
		r.SessionRevision != message.inspection.SessionRevision || r.ExecutionID != message.inspection.ExecutionID ||
		r.EventID != message.inspection.EventID || r.ItemID != message.inspection.ItemID || r.ResolutionID == "" {
		if m.goalRecoveryState != nil {
			m.goalRecoveryState.SetError("The durable recovery receipt did not match the reviewed execution.")
		}
		return nil
	}
	if m.goalRuntime != nil || !m.standaloneRecoveryRevisionCurrent(message.inspection) {
		if m.goalRecoveryState != nil {
			m.goalRecoveryState.SetError("Evidence committed, but the active session authority changed; reload before continuing.")
		}
		return nil
	}
	if m.agent == nil {
		if m.goalRecoveryState != nil {
			m.goalRecoveryState.SetError("Evidence committed, but the in-session recovery cache is unavailable.")
		}
		return nil
	}
	if err := m.appendStandaloneReconciliationContext(r.Context); err != nil {
		if m.goalRecoveryState != nil {
			m.goalRecoveryState.SetError("Evidence committed, but its bounded host receipt is invalid; reload before continuing · " + err.Error())
		}
		return nil
	}
	if err := m.agent.RecheckExecutionRecovery(); err != nil {
		if m.goalRecoveryState != nil {
			m.goalRecoveryState.SetError("Evidence committed, but recovery recheck failed · " + err.Error())
		}
		return nil
	}
	m.goalRecoveryState = nil
	m.standaloneRecovery = nil
	m.overlayParent = OverlayNone
	m.overlay = OverlayNone
	m.input.Focus()
	completion := fmt.Sprintf(
		"Recovery evidence recorded · %s · no tool was retried. The next prompt will recheck durable state.", r.ResolutionID,
	)
	switch {
	case message.remaining == 0:
		completion += " No executions remain pending reconciliation."
	case message.remaining == 1:
		completion += " 1 execution remains pending reconciliation · run /recover again."
	case message.remaining > 1:
		completion += fmt.Sprintf(" %d executions remain pending reconciliation · run /recover again.", message.remaining)
	}
	m.entries = append(m.entries, ChatEntry{Kind: "system", Content: completion})
	m.refreshTranscript()
	m.resumeFollow()
	return tea.ClearScreen
}

func (m *Model) standaloneRecoveryRevisionCurrent(inspection db.StandaloneExecutionReconciliationInspection) bool {
	if m == nil || m.sessionStore == nil || m.executionLease == nil || m.sessionID != inspection.SessionID {
		return false
	}
	m.sessionStateMu.RLock()
	defer m.sessionStateMu.RUnlock()
	return m.sessionStateRevisionKnown && !m.sessionStatePersistenceDirty &&
		m.sessionStateRevision == inspection.SessionRevision
}

func (m *Model) closeStandaloneRecovery() {
	if m == nil {
		return
	}
	m.goalRecoveryState = nil
	m.overlayParent = OverlayNone
	m.overlay = OverlayNone
	m.input.Focus()
}

func (m *Model) appendStandaloneRecoveryError(message string) {
	if m == nil {
		return
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Recovery · " + message})
	m.refreshTranscript()
	m.resumeFollow()
}
