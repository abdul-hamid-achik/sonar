package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

const (
	goalInspectorRecoveryActionID  command.ActionID = "goal.recover"
	goalRecoveryCoordinatorTimeout                  = 5 * time.Second
)

type goalRecoveryOperationScope struct {
	store            *db.Store
	lease            *db.ExecutionSessionLease
	sessionID        int64
	workspaceID      string
	goalID           string
	blockerReference string
	revision         int64
}

func (s goalRecoveryOperationScope) same(other goalRecoveryOperationScope) bool {
	return s.store == other.store && s.lease == other.lease &&
		s.sessionID == other.sessionID && s.workspaceID == other.workspaceID &&
		s.goalID == other.goalID && s.blockerReference == other.blockerReference &&
		s.revision == other.revision
}

type goalRecoveryProjection struct {
	sessionID            int64
	workspaceID          string
	goalID               string
	groupItemID          string
	turnID               string
	revision             int64
	executionMemberCount int
	remainingExecutions  int
	items                []GoalRecoveryItem
	errorText            string
}

type goalRecoveryLoadResultMsg struct {
	token      uint64
	scope      goalRecoveryOperationScope
	projection goalRecoveryProjection
	err        error
}

type goalRecoveryApplyResultMsg struct {
	token   uint64
	scope   goalRecoveryOperationScope
	itemID  string
	kind    GoalRecoveryItemKind
	receipt db.ReconciliationCommitReceipt
	err     error
}

func (m *Model) goalRecoveryAuthority(snapshot goal.Snapshot) (goalRecoveryOperationScope, error) {
	if m == nil || m.sessionStore == nil || m.executionLease == nil || m.agent == nil {
		return goalRecoveryOperationScope{}, errors.New("durable recovery requires an active session lease")
	}
	if snapshot.SessionID <= 0 || snapshot.SessionID != m.sessionID {
		return goalRecoveryOperationScope{}, errors.New("goal recovery session is no longer active")
	}
	if snapshot.State != goal.StateBlocked || snapshot.Blocker == nil || snapshot.Blocker.Kind != goal.BlockOutcomeUnknown {
		return goalRecoveryOperationScope{}, goal.ErrOutcomeUnknown
	}
	if m.state != StateIdle || m.goalOperationRunning {
		return goalRecoveryOperationScope{}, errors.New("recovery is unavailable while another operation is active")
	}
	if m.goalPersistenceDirty {
		return goalRecoveryOperationScope{}, errors.New("goal persistence must be recovered before recording evidence")
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		return goalRecoveryOperationScope{}, fmt.Errorf("resolve recovery workspace: %w", err)
	}
	m.sessionStateMu.RLock()
	revision := m.sessionStateRevision
	known := m.sessionStateRevisionKnown
	dirty := m.sessionStatePersistenceDirty
	m.sessionStateMu.RUnlock()
	if !known {
		return goalRecoveryOperationScope{}, ErrSessionStateRevisionUnknown
	}
	if dirty {
		return goalRecoveryOperationScope{}, errors.New("session persistence must be recovered before recording evidence")
	}
	return goalRecoveryOperationScope{
		store: m.sessionStore, lease: m.executionLease,
		sessionID: snapshot.SessionID, workspaceID: workspaceID,
		goalID: snapshot.ID, blockerReference: snapshot.Blocker.Reference,
		revision: revision,
	}, nil
}

func (m *Model) goalRecoveryScopeCurrent(scope goalRecoveryOperationScope) (goal.Snapshot, error) {
	if m == nil || m.goalRuntime == nil || m.sessionStore != scope.store || m.executionLease != scope.lease ||
		m.sessionID != scope.sessionID {
		return goal.Snapshot{}, errors.New("recovery result belongs to an inactive session")
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		return goal.Snapshot{}, fmt.Errorf("read current goal: %w", err)
	}
	if snapshot.SessionID != scope.sessionID || snapshot.ID != scope.goalID || snapshot.State != goal.StateBlocked ||
		snapshot.Blocker == nil || snapshot.Blocker.Kind != goal.BlockOutcomeUnknown ||
		snapshot.Blocker.Reference != scope.blockerReference {
		return goal.Snapshot{}, errors.New("recovery result no longer matches the blocked goal")
	}
	current, err := m.goalRecoveryAuthority(snapshot)
	if err != nil {
		return goal.Snapshot{}, err
	}
	if !current.same(scope) {
		return goal.Snapshot{}, errors.New("recovery result belongs to a stale session revision")
	}
	return snapshot, nil
}

func (m *Model) ensureCurrentGoalRecoveryProjection(force bool) tea.Cmd {
	if m == nil || m.goalRuntime == nil {
		return nil
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		return nil
	}
	return m.ensureGoalRecoveryProjection(snapshot, force)
}

// ensureGoalRecoveryProjection is the only read/create boundary used by the
// Inspector, settled-turn recovery, and restored-session recovery. The command
// returns only a redacted projection; durable payload JSON never enters Model.
func (m *Model) ensureGoalRecoveryProjection(snapshot goal.Snapshot, force bool) tea.Cmd {
	if snapshot.State != goal.StateBlocked || snapshot.Blocker == nil || snapshot.Blocker.Kind != goal.BlockOutcomeUnknown {
		return nil
	}
	scope, err := m.goalRecoveryAuthority(snapshot)
	if err != nil {
		m.goalRecoveryProjection = goalRecoveryProjection{
			sessionID: snapshot.SessionID, goalID: snapshot.ID, revision: -1,
			errorText: boundedGoalRecoveryPresentationError(goalRecoveryCoordinatorError(err)),
		}
		return nil
	}
	if m.goalRecoveryLoadRunning && m.goalRecoveryLoadScope.same(scope) {
		return nil
	}
	if !force {
		if projection, ok := m.goalRecoveryProjectionFor(snapshot); ok && projection.errorText == "" {
			return nil
		}
	}

	m.goalRecoveryLoadToken++
	token := m.goalRecoveryLoadToken
	m.goalRecoveryLoadRunning = true
	m.goalRecoveryLoadScope = scope
	if m.goalRecoveryState != nil && m.overlay == OverlayGoalRecovery {
		m.goalRecoveryState.SetBusy("Refreshing durable recovery group…")
	}
	store, lease := scope.store, scope.lease
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), goalRecoveryCoordinatorTimeout)
		defer cancel()
		group, _, ensureErr := store.EnsureReconciliationGroup(ctx, lease, db.EnsureReconciliationGroupRequest{
			SessionID: scope.sessionID, WorkspaceID: scope.workspaceID,
			ExpectedSessionRevision: scope.revision,
		})
		if ensureErr != nil {
			return goalRecoveryLoadResultMsg{token: token, scope: scope, err: ensureErr}
		}
		projection, projectionErr := goalRecoveryProjectionFromGroup(scope, group)
		return goalRecoveryLoadResultMsg{token: token, scope: scope, projection: projection, err: projectionErr}
	}
}

func goalRecoveryProjectionFromGroup(scope goalRecoveryOperationScope, group db.ReconciliationGroup) (goalRecoveryProjection, error) {
	if group.SessionID != scope.sessionID || group.WorkspaceID != scope.workspaceID || group.GoalID != scope.goalID ||
		group.BlockerReference != scope.blockerReference {
		return goalRecoveryProjection{}, errors.New("reconciliation group does not match the active blocked goal")
	}
	if group.ParentResolution != nil {
		return goalRecoveryProjection{}, errors.New("reconciliation group is already finalized; reload the session")
	}
	if err := validateGoalRecoveryItemID(group.GroupItemID); err != nil {
		return goalRecoveryProjection{}, fmt.Errorf("invalid reconciliation group identity: %w", err)
	}
	if strings.TrimSpace(group.TurnID) == "" || group.ExecutionMemberCount != len(group.Members) {
		return goalRecoveryProjection{}, errors.New("reconciliation group membership is inconsistent")
	}

	items := make([]GoalRecoveryItem, 0, len(group.Members)+1)
	seen := make(map[string]struct{}, len(group.Members)+1)
	remaining := 0
	for _, member := range group.Members {
		if member.GroupItemID != group.GroupItemID || member.TurnID != group.TurnID || strings.TrimSpace(member.ExecutionID) == "" {
			return goalRecoveryProjection{}, errors.New("reconciliation member correlation is inconsistent")
		}
		if err := validateGoalRecoveryItemID(member.ControlItemID); err != nil {
			return goalRecoveryProjection{}, fmt.Errorf("invalid reconciliation member identity: %w", err)
		}
		if _, duplicate := seen[member.ControlItemID]; duplicate {
			return goalRecoveryProjection{}, errors.New("reconciliation group contains duplicate member identities")
		}
		seen[member.ControlItemID] = struct{}{}
		item := GoalRecoveryItem{
			ItemID: member.ControlItemID, Kind: GoalRecoveryExecutionEffect,
			Subject: "Execution effect", Summary: "Inspect the external effect before allowing any retry.",
			ExecutionID: member.ExecutionID, TurnID: member.TurnID,
			EventType: string(member.EventType), EffectClass: string(member.EffectClass),
			Actionable: !member.Resolved,
		}
		if member.Resolved {
			item.Summary = "Durable evidence is recorded; the immutable execution event remains outcome unknown."
			item.DisabledReason = "Evidence was already recorded for this execution member."
		} else {
			remaining++
		}
		items = append(items, item)
	}

	parent := GoalRecoveryItem{
		ItemID: group.GroupItemID, Kind: GoalRecoveryTurnBoundary,
		Subject: "Provider turn boundary", TurnID: group.TurnID,
		EventType: "provider_receipt_lost", EffectClass: "turn_boundary",
		Actionable: remaining == 0,
	}
	if remaining == 0 {
		parent.Summary = "Every execution effect is accounted for; inspect the lost provider turn before clearing the retry block."
	} else {
		parent.Summary = "The provider turn remains blocked until every execution effect has durable evidence."
		parent.DisabledReason = fmt.Sprintf("Resolve %s before abandoning this inspected turn.", goalRecoveryExecutionCount(remaining))
	}
	items = append(items, parent)

	return goalRecoveryProjection{
		sessionID: scope.sessionID, workspaceID: scope.workspaceID, goalID: scope.goalID,
		groupItemID: group.GroupItemID, turnID: group.TurnID, revision: scope.revision,
		executionMemberCount: group.ExecutionMemberCount, remainingExecutions: remaining,
		items: items,
	}, nil
}

func goalRecoveryExecutionCount(count int) string {
	if count == 1 {
		return "1 execution member"
	}
	return fmt.Sprintf("%d execution members", count)
}

func (m *Model) handleGoalRecoveryLoadResult(message goalRecoveryLoadResultMsg) tea.Cmd {
	if m == nil || !m.goalRecoveryLoadRunning || message.token != m.goalRecoveryLoadToken ||
		!m.goalRecoveryLoadScope.same(message.scope) {
		return nil
	}
	m.goalRecoveryLoadRunning = false
	m.goalRecoveryLoadScope = goalRecoveryOperationScope{}
	snapshot, currentErr := m.goalRecoveryScopeCurrent(message.scope)
	if currentErr != nil {
		errorText := boundedGoalRecoveryPresentationError("Recovery load became stale · " + goalRecoveryCoordinatorError(currentErr))
		m.goalRecoveryProjection = goalRecoveryProjection{
			sessionID: message.scope.sessionID, workspaceID: message.scope.workspaceID,
			goalID: message.scope.goalID, revision: message.scope.revision,
			errorText: errorText,
		}
		m.setGoalRecoveryInlineError(errorText)
		if m.overlay == OverlayGoalInspector && m.goalRuntime != nil {
			if current, err := m.goalRuntime.Snapshot(context.Background()); err == nil {
				m.renderGoalInspector(current)
			}
		}
		return nil
	}
	projection := message.projection
	if message.err != nil {
		projection = goalRecoveryProjection{
			sessionID: message.scope.sessionID, workspaceID: message.scope.workspaceID,
			goalID: message.scope.goalID, revision: message.scope.revision,
			errorText: boundedGoalRecoveryPresentationError(goalRecoveryCoordinatorError(message.err)),
		}
	}
	m.goalRecoveryProjection = projection
	var command tea.Cmd
	if m.goalRecoveryState != nil && m.overlay == OverlayGoalRecovery {
		m.goalRecoveryState.SetBusy("")
		command = m.goalRecoveryState.SetItems(projection.items)
		if projection.errorText != "" {
			m.goalRecoveryState.SetError("Recovery group unavailable · " + projection.errorText)
		}
	} else if m.overlay == OverlayGoalInspector {
		m.renderGoalInspector(snapshot)
	}
	return command
}

func boundedGoalRecoveryPresentationError(value string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "�"))
	const maximum = 512
	if len(value) <= maximum {
		return value
	}
	return boundGoalText(value, maximum)
}

func goalRecoveryCoordinatorError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, db.ErrSessionStateConflict), errors.Is(err, ErrSessionStateRevisionUnknown):
		return "the session revision changed; reload the session before recording evidence"
	case errors.Is(err, db.ErrControlLeaseRequired), errors.Is(err, db.ErrControlLeaseScope):
		return "the active session lease is unavailable; reload the session before recording evidence"
	case errors.Is(err, db.ErrReconciliationStaleEvidence), errors.Is(err, db.ErrReconciliationProjectionRequired):
		return "the execution evidence changed; reload and inspect the latest durable state"
	case errors.Is(err, db.ErrReconciliationGroupIncomplete):
		return "execution members still require evidence"
	case errors.Is(err, db.ErrReconciliationRepairRequired), errors.Is(err, db.ErrReconciliationGroupConflict):
		return "durable recovery state needs repair before it can be changed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "the recovery operation did not finish; no evidence was changed"
	default:
		return boundedGoalRecoveryPresentationError(err.Error())
	}
}

func (m *Model) goalRecoveryProjectionFor(snapshot goal.Snapshot) (goalRecoveryProjection, bool) {
	projection := m.goalRecoveryProjection
	if projection.sessionID != snapshot.SessionID || projection.goalID != snapshot.ID {
		return goalRecoveryProjection{}, false
	}
	m.sessionStateMu.RLock()
	revision, known := m.sessionStateRevision, m.sessionStateRevisionKnown
	m.sessionStateMu.RUnlock()
	if projection.revision != revision || !known {
		return goalRecoveryProjection{}, false
	}
	projection.items = append([]GoalRecoveryItem(nil), projection.items...)
	return projection, true
}

func (m *Model) decorateGoalInspectorRecovery(snapshot goal.Snapshot, actions []command.ActionState) ([]command.ActionState, string) {
	if snapshot.Blocker == nil || snapshot.Blocker.Kind != goal.BlockOutcomeUnknown {
		return actions, ""
	}
	state := command.ActionState{Spec: command.ActionSpec{
		ID: goalInspectorRecoveryActionID, Command: "goal", Argument: "recover",
		Title: "Recovery", Description: "Inspect and record durable evidence without resuming AUTO",
	}}
	status := "loading durable recovery group · retry remains blocked"
	projection, matched := m.goalRecoveryProjectionFor(snapshot)
	_, authorityErr := m.goalRecoveryAuthority(snapshot)
	switch {
	case authorityErr != nil:
		state.DisabledReason = goalRecoveryCoordinatorError(authorityErr)
		status = "unavailable · " + state.DisabledReason
	case m.goalRecoveryApplyRunning:
		state.DisabledReason = "A durable evidence receipt is still being recorded."
		status = "recording evidence · retry remains blocked"
	case m.goalRecoveryLoadRunning:
		state.DisabledReason = "The durable recovery group is loading."
	case matched && projection.errorText != "":
		state.DisabledReason = "Recovery items could not be loaded; no evidence was changed."
		status = "unavailable · " + projection.errorText
	case !matched || len(projection.items) == 0:
		state.DisabledReason = "No durable recovery item is available yet."
		status = "awaiting durable recovery items · retry remains blocked"
	default:
		state.Enabled = true
		actionable := goalRecoveryActionableItems(projection.items)
		state.Spec.Description = fmt.Sprintf("Review %s; %d can record evidence", goalRecoveryItemCount(len(projection.items)), actionable)
		if actionable == 0 {
			status = fmt.Sprintf("%d items · review only · retry remains blocked", len(projection.items))
		} else {
			status = fmt.Sprintf("%d items · %d actionable · retry remains blocked", len(projection.items), actionable)
		}
	}
	return insertGoalInspectorRecoveryAction(actions, state), status
}

func insertGoalInspectorRecoveryAction(actions []command.ActionState, recovery command.ActionState) []command.ActionState {
	result := make([]command.ActionState, 0, len(actions)+1)
	inserted := false
	for _, action := range actions {
		result = append(result, action)
		if action.Spec.ID == command.GoalActionResume {
			result = append(result, recovery)
			inserted = true
		}
	}
	if !inserted {
		result = append(result, recovery)
	}
	return result
}

func goalRecoveryActionableItems(items []GoalRecoveryItem) int {
	count := 0
	for _, item := range items {
		if item.Actionable && item.Kind.valid() {
			count++
		}
	}
	return count
}

func goalRecoveryItemCount(count int) string {
	if count == 1 {
		return "1 recovery item"
	}
	return fmt.Sprintf("%d recovery items", count)
}

func (m *Model) openGoalRecovery() {
	if m == nil || m.goalRuntime == nil || m.goalInspectorState == nil ||
		m.goalRecoveryLoadRunning || m.goalRecoveryApplyRunning {
		return
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil || snapshot.Blocker == nil || snapshot.Blocker.Kind != goal.BlockOutcomeUnknown {
		return
	}
	if _, err := m.goalRecoveryAuthority(snapshot); err != nil {
		m.renderGoalInspector(snapshot)
		return
	}
	projection, matched := m.goalRecoveryProjectionFor(snapshot)
	if !matched || projection.errorText != "" || len(projection.items) == 0 {
		m.renderGoalInspector(snapshot)
		return
	}
	m.preemptTranscriptSearch()
	m.goalRecoveryState = NewGoalRecovery(projection.items, GoalRecoveryOptions{
		Width: m.width, Height: m.height, IsDark: m.isDark, ReducedMotion: m.reducedMotion,
		GlyphProfile: m.glyphProfile,
	})
	m.overlayParent = OverlayGoalInspector
	m.overlay = OverlayGoalRecovery
	m.input.Blur()
}

func (m *Model) closeGoalRecovery() {
	if m == nil {
		return
	}
	if m.standaloneRecovery != nil {
		m.closeStandaloneRecovery()
		return
	}
	m.goalRecoveryState = nil
	if m.overlayParent == OverlayGoalInspector && m.goalInspectorState != nil {
		if m.goalRuntime != nil {
			if snapshot, err := m.goalRuntime.Snapshot(context.Background()); err == nil {
				m.renderGoalInspector(snapshot)
				return
			}
		}
		m.dismissOverlay()
		return
	}
	m.dismissOverlay()
}

func (m *Model) handleGoalRecoveryEvent(event GoalRecoveryEvent) tea.Cmd {
	if m == nil {
		return nil
	}
	switch event.Action {
	case GoalRecoveryActionClose:
		m.closeGoalRecovery()
		return nil
	case GoalRecoveryActionApply:
		if m.standaloneRecovery != nil {
			return m.beginStandaloneRecoveryApply(event)
		}
		return m.beginGoalRecoveryApply(event)
	default:
		return nil
	}
}

func (m *Model) beginGoalRecoveryApply(event GoalRecoveryEvent) tea.Cmd {
	if m.goalRuntime == nil || m.goalRecoveryState == nil || m.overlay != OverlayGoalRecovery {
		return nil
	}
	if m.goalRecoveryApplyRunning || m.goalRecoveryLoadRunning {
		m.goalRecoveryState.SetError("A recovery receipt is already in flight; wait for its durable result.")
		return nil
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		m.goalRecoveryState.SetError("Read goal before recovery · " + goalRecoveryCoordinatorError(err))
		return nil
	}
	projection, matched := m.goalRecoveryProjectionFor(snapshot)
	item, allowed := goalRecoveryProjectionItem(projection.items, event.ItemID)
	if !matched || !allowed || projection.groupItemID == "" {
		m.goalRecoveryState.SetError("Recovery item is stale or not actionable; no evidence was changed.")
		return nil
	}
	scope, err := m.goalRecoveryAuthority(snapshot)
	if err != nil || scope.revision != projection.revision || scope.workspaceID != projection.workspaceID {
		m.goalRecoveryState.SetError("Recovery authority changed · " + goalRecoveryCoordinatorError(err))
		return nil
	}
	observedAt := m.nowTime().UTC()
	executionEvidence, turnEvidence, err := goalRecoveryTypedEvidence(item.Kind, event.Draft, observedAt)
	if err != nil {
		m.goalRecoveryState.SetError("Evidence is invalid · " + goalRecoveryCoordinatorError(err))
		return nil
	}

	m.goalRecoveryApplyToken++
	token := m.goalRecoveryApplyToken
	m.goalRecoveryApplyRunning = true
	m.goalRecoveryApplyItemID = item.ItemID
	m.goalRecoveryState.SetBusy("Recording immutable evidence…")
	store, lease := scope.store, scope.lease
	groupItemID := projection.groupItemID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), goalRecoveryCoordinatorTimeout)
		defer cancel()
		var receipt db.ReconciliationCommitReceipt
		var applyErr error
		if item.Kind == GoalRecoveryTurnBoundary {
			receipt, applyErr = store.ResolveReconciliationParent(ctx, lease, db.ResolveReconciliationParentRequest{
				SessionID: scope.sessionID, WorkspaceID: scope.workspaceID, GroupItemID: groupItemID,
				ExpectedSessionRevision: scope.revision, Actor: goalActor, Evidence: turnEvidence,
			})
		} else {
			receipt, applyErr = store.ResolveExecutionReconciliation(ctx, lease, db.ResolveExecutionReconciliationRequest{
				SessionID: scope.sessionID, WorkspaceID: scope.workspaceID, GroupItemID: groupItemID,
				ControlItemID: item.ItemID, ExpectedSessionRevision: scope.revision,
				Actor: goalActor, Evidence: executionEvidence,
			})
		}
		return goalRecoveryApplyResultMsg{
			token: token, scope: scope, itemID: item.ItemID, kind: item.Kind,
			receipt: receipt, err: applyErr,
		}
	}
}

func goalRecoveryProjectionItem(items []GoalRecoveryItem, itemID string) (GoalRecoveryItem, bool) {
	for _, item := range items {
		if item.ItemID == itemID {
			return item, item.Actionable && item.Kind.valid()
		}
	}
	return GoalRecoveryItem{}, false
}

func goalRecoveryTypedEvidence(kind GoalRecoveryItemKind, draft GoalRecoveryDraft, observedAt time.Time) (reconciliation.Request, reconciliation.TurnRequest, error) {
	if observedAt.IsZero() {
		return reconciliation.Request{}, reconciliation.TurnRequest{}, errors.New("evidence observation time is unavailable")
	}
	sourceKind := reconciliation.SourceKind(draft.Source)
	source := reconciliation.Source{
		Kind: sourceKind, Reference: strings.TrimSpace(draft.Reference), ObservedAt: observedAt.UTC(),
	}
	summary := strings.TrimSpace(draft.Summary)
	if kind == GoalRecoveryTurnBoundary {
		request := reconciliation.TurnRequest{
			Conclusion: reconciliation.TurnConclusion(draft.Observation), Source: source, Summary: summary,
		}
		return reconciliation.Request{}, request, request.Validate()
	}
	if kind != GoalRecoveryExecutionEffect {
		return reconciliation.Request{}, reconciliation.TurnRequest{}, errors.New("recovery item kind is unavailable")
	}
	request := reconciliation.Request{
		Disposition: reconciliation.Disposition(draft.Observation), Source: source, Summary: summary,
	}
	return request, reconciliation.TurnRequest{}, request.Validate()
}

func (m *Model) handleGoalRecoveryApplyResult(message goalRecoveryApplyResultMsg) tea.Cmd {
	if m == nil || !m.goalRecoveryApplyRunning || message.token != m.goalRecoveryApplyToken ||
		message.itemID != m.goalRecoveryApplyItemID {
		return nil
	}
	m.goalRecoveryApplyRunning = false
	m.goalRecoveryApplyItemID = ""
	if m.goalRecoveryState != nil {
		m.goalRecoveryState.SetBusy("")
	}
	snapshot, currentErr := m.goalRecoveryScopeCurrent(message.scope)
	if currentErr != nil {
		m.setGoalRecoveryCoordinatorError("Recovery result became stale · " + goalRecoveryCoordinatorError(currentErr))
		return nil
	}
	projection, matched := m.goalRecoveryProjectionFor(snapshot)
	item, allowed := goalRecoveryProjectionItem(projection.items, message.itemID)
	if !matched || !allowed || item.Kind != message.kind || projection.groupItemID != message.receipt.GroupItemID && message.err == nil {
		m.setGoalRecoveryCoordinatorError("Recovery result no longer matches the active durable group.")
		return nil
	}
	if message.err != nil {
		m.setGoalRecoveryCoordinatorError("Evidence was not recorded · " + goalRecoveryCoordinatorError(message.err))
		return nil
	}
	if err := validateGoalRecoveryReceipt(message.scope, item, message.receipt); err != nil {
		m.setGoalRecoveryCoordinatorError("Coordinator receipt was rejected · " + goalRecoveryCoordinatorError(err))
		return nil
	}
	if item.Kind == GoalRecoveryTurnBoundary {
		if err := m.hydrateGoalRecoveryReceipt(message.scope, message.receipt); err != nil {
			m.setGoalRecoveryCoordinatorError("Durable recovery committed, but local hydration failed · " + goalRecoveryCoordinatorError(err))
			return nil
		}
		return nil
	}

	for index := range projection.items {
		if projection.items[index].ItemID == item.ItemID {
			projection.items[index].Actionable = false
			projection.items[index].Summary = "Durable evidence is recorded; the immutable execution event remains outcome unknown."
			projection.items[index].DisabledReason = "Evidence was already recorded for this execution member."
		}
	}
	projection.remainingExecutions = message.receipt.RemainingExecutions
	m.goalRecoveryProjection = projection
	var commands []tea.Cmd
	if m.goalRecoveryState != nil && m.overlay == OverlayGoalRecovery {
		commands = append(commands, m.goalRecoveryState.ShowRecordedReceipt(
			projection.items,
			"Evidence recorded · the goal and session revision remain blocked until the turn parent is resolved.",
		))
	}
	commands = append(commands, m.ensureGoalRecoveryProjection(snapshot, true))
	return tea.Batch(commands...)
}

func validateGoalRecoveryReceipt(scope goalRecoveryOperationScope, item GoalRecoveryItem, receipt db.ReconciliationCommitReceipt) error {
	if receipt.GroupItemID == "" || receipt.ItemID != item.ItemID || receipt.ResolutionID == "" || receipt.SessionRevision < 0 {
		return errors.New("reconciliation receipt identity is incomplete")
	}
	if item.Kind == GoalRecoveryExecutionEffect {
		if receipt.GoalCleared || receipt.Goal != nil || receipt.SessionRevision != scope.revision ||
			receipt.RemainingExecutions < 0 || !receipt.ParentPending {
			return errors.New("partial reconciliation receipt changed goal authority")
		}
		return nil
	}
	if !receipt.GoalCleared || receipt.Goal == nil || receipt.SessionRevision <= scope.revision ||
		receipt.RemainingExecutions != 0 || receipt.ParentPending || receipt.ExecutionCursor < 0 {
		return errors.New("final reconciliation receipt is incomplete")
	}
	return nil
}

func (m *Model) hydrateGoalRecoveryReceipt(scope goalRecoveryOperationScope, receipt db.ReconciliationCommitReceipt) error {
	next := *receipt.Goal
	if next.SessionID != scope.sessionID || next.ID != scope.goalID ||
		(next.State != goal.StatePaused && next.State != goal.StateExhausted) || next.Blocker != nil {
		return errors.New("final reconciliation goal does not represent a cleared paused or exhausted state")
	}
	restored, err := goal.Restore(next)
	if err != nil {
		return fmt.Errorf("restore reconciled goal: %w", err)
	}

	m.sessionStateMu.Lock()
	if m.sessionID != scope.sessionID || !m.sessionStateRevisionKnown || m.sessionStateRevision != scope.revision {
		m.sessionStateMu.Unlock()
		return errors.New("session revision changed before final hydration")
	}
	// These three fields are one coordinator-owned projection. No intermediate
	// Update can observe a new goal with the old cursor or revision.
	m.goalRuntime = restored
	m.executionCursor = receipt.ExecutionCursor
	m.sessionStateRevision = receipt.SessionRevision
	m.sessionStateRevisionKnown = true
	m.sessionStatePersistenceDirty = false
	m.sessionStateMu.Unlock()
	m.goalPersistenceDirty = false
	if m.agent != nil {
		m.agent.SetExecutionSnapshotCursor(receipt.ExecutionCursor)
	}
	var recheckErr error
	if m.agent == nil {
		recheckErr = errors.New("agent recovery cache is unavailable")
	} else {
		recheckErr = m.agent.RecheckExecutionRecovery()
	}
	m.resetGoalRecoveryPresentation()
	m.renderGoalInspector(next)
	if recheckErr != nil {
		m.appendGoalError("Recheck execution recovery: " + recheckErr.Error())
	}
	return nil
}

func (m *Model) setGoalRecoveryInlineError(message string) {
	if m != nil && m.goalRecoveryState != nil && m.overlay == OverlayGoalRecovery {
		m.goalRecoveryState.SetBusy("")
		m.goalRecoveryState.SetError(boundedGoalRecoveryPresentationError(message))
	}
}

func (m *Model) setGoalRecoveryCoordinatorError(message string) {
	message = boundedGoalRecoveryPresentationError(message)
	projection := m.goalRecoveryProjection
	projection.errorText = message
	m.goalRecoveryProjection = projection
	m.setGoalRecoveryInlineError(message)
	if m.overlay == OverlayGoalInspector && m.goalRuntime != nil {
		if snapshot, err := m.goalRuntime.Snapshot(context.Background()); err == nil {
			m.renderGoalInspector(snapshot)
		}
	}
}

func (m *Model) resetGoalRecoveryPresentation() {
	if m == nil {
		return
	}
	m.goalRecoveryLoadToken++
	m.goalRecoveryApplyToken++
	m.goalRecoveryLoadRunning = false
	m.goalRecoveryApplyRunning = false
	m.goalRecoveryApplyItemID = ""
	m.goalRecoveryLoadScope = goalRecoveryOperationScope{}
	m.goalRecoveryState = nil
	m.goalRecoveryProjection = goalRecoveryProjection{}
	if m.overlay == OverlayGoalRecovery {
		m.overlayParent = OverlayNone
		m.overlay = OverlayNone
	}
}
