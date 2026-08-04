package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

func newReconciliationGoalRuntime(t *testing.T, sessionID int64) *goal.Runtime {
	t.Helper()
	runtime, err := goal.New(goal.Spec{
		ID: "goal_reconciliation_test", SessionID: sessionID,
		Objective:          "Recover the abandoned turn without redispatch",
		AcceptanceCriteria: []goal.AcceptanceCriterion{{ID: "safe", Description: "No unknown effect is retried"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func persistReconciliationGoalSession(t *testing.T, store *Store, sessionID int64, snapshot goal.Snapshot, cursor int64) SessionStateRecord {
	t.Helper()
	goalJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{ "version" : 2, "future" : { "opaque" : [1, 2, 3] }, "execution_cursor" : %d, "goal" : %s, "tail":"preserve" }`, cursor, goalJSON)
	if err := store.SaveSessionState(context.Background(), sessionID, raw); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetSessionStateRecord(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func blockNoToolGoal(t *testing.T, runtime *goal.Runtime, turnID string) goal.Snapshot {
	t.Helper()
	if _, err := runtime.BeginTurn(context.Background(), turnID, goal.AdmissionInitial); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecoverPendingContinuation(context.Background(), goal.PendingRecovery{
		TurnID: turnID, Kind: goal.PendingOutcomeUnknown,
		Reason:   "provider response was lost before a tool lifecycle appeared",
		Evidence: "the admitted turn has no settled provider receipt", OutcomeRef: turnID,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func blockExecutionGoal(t *testing.T, runtime *goal.Runtime, turnID, outcomeRef string) goal.Snapshot {
	t.Helper()
	if _, err := runtime.BeginTurn(context.Background(), turnID, goal.AdmissionInitial); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordTurn(context.Background(), goal.TurnReport{
		TurnID: turnID, Productive: false, Summary: "one or more execution effects are unknown",
		OutcomeUnknown: true, OutcomeRef: outcomeRef,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func reconciliationEvidence(suffix string, disposition reconciliation.Disposition) reconciliation.Request {
	return reconciliation.Request{
		Disposition: disposition,
		Source: reconciliation.Source{
			Kind:       reconciliation.SourceVerificationCheck,
			Reference:  "check:" + suffix,
			ObservedAt: time.Date(2026, time.July, 12, 16, 30, 0, 0, time.UTC),
		},
		Summary: "Verified external state for " + suffix + ".",
	}
}

func reconciliationTurnEvidence(suffix string) reconciliation.TurnRequest {
	return reconciliation.TurnRequest{
		Conclusion: reconciliation.TurnAbandonedAfterInspection,
		Source: reconciliation.Source{
			Kind:       reconciliation.SourceOperatorObservation,
			Reference:  "turn-check:" + suffix,
			ObservedAt: time.Date(2026, time.July, 12, 16, 35, 0, 0, time.UTC),
		},
		Summary: "Inspected the abandoned turn " + suffix + " and every durable execution member.",
	}
}

type finalizedZeroReconciliation struct {
	store   *Store
	session Session
	lease   *ExecutionSessionLease
	request ResolveReconciliationParentRequest
}

func finalizeZeroReconciliation(t *testing.T, suffix string) finalizedZeroReconciliation {
	t.Helper()
	store := testStore(t)
	workspaceID := "/workspace/reconcile-final-replay-" + suffix
	session := createExecutionTestSession(t, store, workspaceID)
	runtime := newReconciliationGoalRuntime(t, session.ID)
	snapshot := blockNoToolGoal(t, runtime, "turn_final_replay_"+suffix)
	record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
	lease := acquireControlTestLease(t, store, session)
	group, _, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := ResolveReconciliationParentRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
		ExpectedSessionRevision: record.Revision, Actor: "local-user",
		Evidence: reconciliationTurnEvidence("final-replay-" + suffix),
	}
	if _, err := store.ResolveReconciliationParent(context.Background(), lease, request); err != nil {
		t.Fatal(err)
	}
	return finalizedZeroReconciliation{store: store, session: session, lease: lease, request: request}
}

func TestReconciliationMemberLimitReservesTurnParentTarget(t *testing.T) {
	if got := reconciliation.MaxGroupMembers + 1; got != goal.MaxReconciliationTargets {
		t.Fatalf("execution members plus parent = %d, goal target limit = %d", got, goal.MaxReconciliationTargets)
	}
	receipt := goal.ReconciliationReceipt{
		Version: goal.ReconciliationReceiptVersion, GroupItemID: "group", FinalItemID: "group",
		FinalResolutionID: "resolution", ResolutionSetSHA256: strings.Repeat("a", 64),
		TargetCount: reconciliation.MaxGroupMembers + 1,
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("maximum reconciliation group cannot produce a valid goal receipt: %v", err)
	}
	store := testStore(t)
	var groupDDL string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'reconciliation_groups'`).Scan(&groupDDL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(groupDDL, "execution_member_count <= 9999") {
		t.Fatalf("reconciliation group schema does not reserve the parent target slot:\n%s", groupDDL)
	}
}

func TestZeroToolReconciliationParentAtomicallyClearsGoalAndReplays(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconcile-zero"
	session := createExecutionTestSession(t, store, workspaceID)
	runtime := newReconciliationGoalRuntime(t, session.ID)
	snapshot := blockNoToolGoal(t, runtime, "turn_zero")
	record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
	lease := acquireControlTestLease(t, store, session)

	group, inserted, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	})
	if err != nil || !inserted || len(group.Members) != 0 || group.ExecutionMemberCount != 0 || group.ParentResolution != nil {
		t.Fatalf("zero-tool group = %#v inserted=%v error=%v", group, inserted, err)
	}
	for name, statement := range map[string]string{
		"update": `UPDATE reconciliation_groups SET blocker_reference = blocker_reference WHERE group_item_id = ?`,
		"delete": `DELETE FROM reconciliation_groups WHERE group_item_id = ?`,
	} {
		if _, err := store.db.Exec(statement, group.GroupItemID); err == nil {
			t.Fatalf("append-only reconciliation group allowed %s", name)
		}
	}
	replayedGroup, inserted, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	})
	if err != nil || inserted || replayedGroup.GroupItemID != group.GroupItemID {
		t.Fatalf("group replay = %#v inserted=%v error=%v", replayedGroup, inserted, err)
	}

	parentRequest := ResolveReconciliationParentRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
		ExpectedSessionRevision: record.Revision, Actor: "local-user",
		Evidence: reconciliationTurnEvidence("zero"),
	}
	receipt, err := store.ResolveReconciliationParent(context.Background(), lease, parentRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Inserted || !receipt.GoalCleared || receipt.ParentPending || receipt.RemainingExecutions != 0 ||
		receipt.Goal == nil || (receipt.Goal.State != goal.StatePaused && receipt.Goal.State != goal.StateExhausted) ||
		receipt.Goal.LastBlockResolution == nil || receipt.Goal.LastBlockResolution.Resolution.Reconciliation.TargetCount != 1 {
		t.Fatalf("zero-tool final receipt = %#v", receipt)
	}
	for name, statement := range map[string]string{
		"update": `UPDATE reconciliation_group_resolutions SET resolved_by = resolved_by WHERE group_item_id = ?`,
		"delete": `DELETE FROM reconciliation_group_resolutions WHERE group_item_id = ?`,
	} {
		if _, err := store.db.Exec(statement, group.GroupItemID); err == nil {
			t.Fatalf("append-only reconciliation parent resolution allowed %s", name)
		}
	}
	storedState, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil || storedState.Revision != record.Revision+1 ||
		!strings.Contains(storedState.StateJSON, `"future" : { "opaque" : [1, 2, 3] }`) ||
		!strings.Contains(storedState.StateJSON, `"tail":"preserve"`) {
		t.Fatalf("lossless final session = %#v, error=%v", storedState, err)
	}

	replay, err := store.ResolveReconciliationParent(context.Background(), lease, parentRequest)
	if err != nil || replay.Inserted || !replay.GoalCleared || replay.SessionRevision != record.Revision+1 {
		t.Fatalf("exact parent replay = %#v, error=%v", replay, err)
	}
	parentMutations := []struct {
		name   string
		mutate func(*ResolveReconciliationParentRequest)
	}{
		{"source kind", func(value *ResolveReconciliationParentRequest) {
			value.Evidence.Source.Kind = reconciliation.SourceVerificationCheck
		}},
		{"source reference", func(value *ResolveReconciliationParentRequest) { value.Evidence.Source.Reference += ":changed" }},
		{"summary", func(value *ResolveReconciliationParentRequest) { value.Evidence.Summary += " changed" }},
		{"observed time", func(value *ResolveReconciliationParentRequest) {
			value.Evidence.Source.ObservedAt = value.Evidence.Source.ObservedAt.Add(time.Second)
		}},
		{"actor", func(value *ResolveReconciliationParentRequest) { value.Actor = "another-local-user" }},
	}
	for _, mutation := range parentMutations {
		changed := parentRequest
		mutation.mutate(&changed)
		if _, err := store.ResolveReconciliationParent(context.Background(), lease, changed); !errors.Is(err, ErrReconciliationGroupConflict) {
			t.Fatalf("changed parent %s replay error = %v", mutation.name, err)
		}
	}
	latest, err := store.LatestExecutionEventID(context.Background(), session.ID, workspaceID)
	if err != nil || latest != 0 {
		t.Fatalf("zero-tool reconciliation mutated execution ledger: latest=%d error=%v", latest, err)
	}
}

func TestMultiExecutionGroupRequiresEveryMemberThenParent(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconcile-multi"
	session := createExecutionTestSession(t, store, workspaceID)
	turnID := "turn_multi"
	firstRequest := executionTestEvent(t, session.ID, workspaceID, "multi-a", execution.EffectUnknown)
	firstRequest.Identity.TurnID = turnID
	first := appendOutcomeUnknownExecutionFixture(t, store, firstRequest)
	secondRequest := executionTestEvent(t, session.ID, workspaceID, "multi-b", execution.Effectful)
	secondRequest.Identity.TurnID = turnID
	second := appendStartedExecutionFixture(t, store, secondRequest)
	rawBefore := make(map[string][]execution.Event)
	for _, event := range []execution.Event{first, second} {
		rawBefore[event.Identity.ExecutionID], _ = store.ListExecutionEvents(context.Background(), session.ID, workspaceID, event.Identity.ExecutionID, 10)
	}
	runtime := newReconciliationGoalRuntime(t, session.ID)
	snapshot := blockExecutionGoal(t, runtime, turnID, first.Identity.ExecutionID)
	record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
	lease := acquireControlTestLease(t, store, session)
	group, inserted, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	})
	if err != nil || !inserted || len(group.Members) != 2 {
		t.Fatalf("multi group = %#v inserted=%v error=%v", group, inserted, err)
	}
	for name, statement := range map[string]string{
		"update": `UPDATE reconciliation_group_members SET turn_id = turn_id WHERE control_item_id = ?`,
		"delete": `DELETE FROM reconciliation_group_members WHERE control_item_id = ?`,
	} {
		if _, err := store.db.Exec(statement, group.Members[0].ControlItemID); err == nil {
			t.Fatalf("append-only reconciliation member allowed %s", name)
		}
	}
	parent := ResolveReconciliationParentRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
		ExpectedSessionRevision: record.Revision, Actor: "local-user", Evidence: reconciliationTurnEvidence("multi"),
	}
	if _, err := store.ResolveReconciliationParent(context.Background(), lease, parent); !errors.Is(err, ErrReconciliationGroupIncomplete) {
		t.Fatalf("early parent error = %v", err)
	}

	memberRequests := make([]ResolveExecutionReconciliationRequest, len(group.Members))
	for index, member := range group.Members {
		memberRequests[index] = ResolveExecutionReconciliationRequest{
			SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
			ControlItemID: member.ControlItemID, ExpectedSessionRevision: record.Revision,
			Actor: "local-user", Evidence: reconciliationEvidence(member.ExecutionID, reconciliation.DispositionEffectNotApplied),
		}
	}
	staleMember := memberRequests[0]
	staleMember.ExpectedSessionRevision++
	if _, err := store.ResolveExecutionReconciliation(context.Background(), lease, staleMember); !errors.Is(err, ErrSessionStateConflict) {
		t.Fatalf("stale member revision error = %v", err)
	}
	firstReceipt, err := store.ResolveExecutionReconciliation(context.Background(), lease, memberRequests[0])
	if err != nil || firstReceipt.GoalCleared || firstReceipt.RemainingExecutions != 1 || !firstReceipt.ParentPending || firstReceipt.SessionRevision != record.Revision {
		t.Fatalf("first member receipt = %#v, error=%v", firstReceipt, err)
	}
	replay, err := store.ResolveExecutionReconciliation(context.Background(), lease, memberRequests[0])
	if err != nil || replay.Inserted || replay.ResolutionID != firstReceipt.ResolutionID || replay.RemainingExecutions != 1 {
		t.Fatalf("member replay = %#v, error=%v", replay, err)
	}
	memberMutations := []struct {
		name   string
		mutate func(*ResolveExecutionReconciliationRequest)
	}{
		{"disposition", func(value *ResolveExecutionReconciliationRequest) {
			value.Evidence.Disposition = reconciliation.DispositionEffectApplied
		}},
		{"source kind", func(value *ResolveExecutionReconciliationRequest) {
			value.Evidence.Source.Kind = reconciliation.SourceExternalReceipt
		}},
		{"source reference", func(value *ResolveExecutionReconciliationRequest) { value.Evidence.Source.Reference += ":changed" }},
		{"summary", func(value *ResolveExecutionReconciliationRequest) { value.Evidence.Summary += " changed" }},
		{"observed time", func(value *ResolveExecutionReconciliationRequest) {
			value.Evidence.Source.ObservedAt = value.Evidence.Source.ObservedAt.Add(time.Second)
		}},
		{"actor", func(value *ResolveExecutionReconciliationRequest) { value.Actor = "another-local-user" }},
	}
	for _, mutation := range memberMutations {
		changed := memberRequests[0]
		mutation.mutate(&changed)
		if _, err := store.ResolveExecutionReconciliation(context.Background(), lease, changed); !errors.Is(err, ErrControlResolutionConflict) {
			t.Fatalf("changed member %s replay error = %v", mutation.name, err)
		}
	}
	secondReceipt, err := store.ResolveExecutionReconciliation(context.Background(), lease, memberRequests[1])
	if err != nil || secondReceipt.GoalCleared || secondReceipt.RemainingExecutions != 0 || !secondReceipt.ParentPending {
		t.Fatalf("second member receipt = %#v, error=%v", secondReceipt, err)
	}
	blockedRecord, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil || blockedRecord.Revision != record.Revision {
		t.Fatalf("member evidence changed session state = %#v, error=%v", blockedRecord, err)
	}
	final, err := store.ResolveReconciliationParent(context.Background(), lease, parent)
	if err != nil || !final.GoalCleared || final.Goal == nil || final.Goal.State != goal.StatePaused ||
		final.Goal.LastBlockResolution.Resolution.Reconciliation.TargetCount != 3 {
		t.Fatalf("multi final receipt = %#v, error=%v", final, err)
	}
	for executionID, before := range rawBefore {
		after, err := store.ListExecutionEvents(context.Background(), session.ID, workspaceID, executionID, 10)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("raw ledger %s changed:\nbefore=%#v\nafter=%#v\nerror=%v", executionID, before, after, err)
		}
	}
	hazards, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, final.ExecutionCursor, 100)
	if err != nil || len(hazards) != 0 {
		t.Fatalf("effective hazards after final = %#v, error=%v", hazards, err)
	}
}

func TestReconciliationRejectsStaleCompletedAndOutsideGroupHazards(t *testing.T) {
	t.Run("member completed", func(t *testing.T) {
		store := testStore(t)
		workspaceID := "/workspace/reconcile-completed"
		session := createExecutionTestSession(t, store, workspaceID)
		turnID := "turn_completed"
		request := executionTestEvent(t, session.ID, workspaceID, "member-completed", execution.Effectful)
		request.Identity.TurnID = turnID
		started := appendStartedExecutionFixture(t, store, request)
		runtime := newReconciliationGoalRuntime(t, session.ID)
		snapshot := blockExecutionGoal(t, runtime, turnID, started.Identity.ExecutionID)
		record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
		lease := acquireControlTestLease(t, store, session)
		group, _, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
			SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
		})
		if err != nil {
			t.Fatal(err)
		}
		completed := started
		completed.Type = execution.EventCompleted
		completed.ResultReceipt = "completed after frozen group"
		completed.ResultSHA256 = execution.HashText(completed.ResultReceipt)
		completed.OccurredAt = started.OccurredAt.Add(time.Second)
		appendExecutionEvent(t, store, completed)
		if _, _, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
			SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
		}); !errors.Is(err, ErrReconciliationProjectionRequired) {
			t.Fatalf("completed group replay error = %v", err)
		}
		_, err = store.ResolveExecutionReconciliation(context.Background(), lease, ResolveExecutionReconciliationRequest{
			SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
			ControlItemID: group.Members[0].ControlItemID, ExpectedSessionRevision: record.Revision,
			Actor: "local-user", Evidence: reconciliationEvidence("completed", reconciliation.DispositionEffectApplied),
		})
		if !errors.Is(err, ErrReconciliationProjectionRequired) {
			t.Fatalf("completed member error = %v", err)
		}
		state, _ := store.GetControlState(context.Background(), session.ID, workspaceID, group.Members[0].ControlItemID)
		if !state.Pending() {
			t.Fatal("stale completed member persisted a resolution")
		}
	})

	t.Run("outside frozen group", func(t *testing.T) {
		store := testStore(t)
		workspaceID := "/workspace/reconcile-outside"
		session := createExecutionTestSession(t, store, workspaceID)
		runtime := newReconciliationGoalRuntime(t, session.ID)
		snapshot := blockNoToolGoal(t, runtime, "turn_parent")
		record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
		lease := acquireControlTestLease(t, store, session)
		group, _, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
			SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
		})
		if err != nil {
			t.Fatal(err)
		}
		outside := executionTestEvent(t, session.ID, workspaceID, "outside", execution.Effectful)
		outside.Identity.TurnID = "turn_outside"
		appendStartedExecutionFixture(t, store, outside)
		_, err = store.ResolveReconciliationParent(context.Background(), lease, ResolveReconciliationParentRequest{
			SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
			ExpectedSessionRevision: record.Revision, Actor: "local-user", Evidence: reconciliationTurnEvidence("outside"),
		})
		if !errors.Is(err, ErrReconciliationStaleEvidence) {
			t.Fatalf("outside hazard error = %v", err)
		}
		current, _ := store.GetReconciliationGroup(context.Background(), session.ID, workspaceID, group.GroupItemID)
		if current.ParentResolution != nil {
			t.Fatal("outside hazard persisted parent resolution")
		}
	})
}

func TestReconciliationFinalTransactionRollsBackAndForgedReplayFails(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconcile-atomic"
	session := createExecutionTestSession(t, store, workspaceID)
	runtime := newReconciliationGoalRuntime(t, session.ID)
	snapshot := blockNoToolGoal(t, runtime, "turn_atomic")
	record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
	lease := acquireControlTestLease(t, store, session)
	group, _, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := ResolveReconciliationParentRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
		ExpectedSessionRevision: record.Revision, Actor: "local-user", Evidence: reconciliationTurnEvidence("atomic"),
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_reconciliation_session_update BEFORE UPDATE ON session_state BEGIN SELECT RAISE(ABORT, 'injected session failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveReconciliationParent(context.Background(), lease, request); err == nil || !strings.Contains(err.Error(), "injected session failure") {
		t.Fatalf("injected final error = %v", err)
	}
	current, err := store.GetReconciliationGroup(context.Background(), session.ID, workspaceID, group.GroupItemID)
	if err != nil || current.ParentResolution != nil {
		t.Fatalf("failed final left parent resolution = %#v, error=%v", current.ParentResolution, err)
	}
	stillBlocked, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil || stillBlocked.Revision != record.Revision || stillBlocked.StateJSON != record.StateJSON {
		t.Fatalf("failed final changed session = %#v, error=%v", stillBlocked, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_reconciliation_session_update`); err != nil {
		t.Fatal(err)
	}
	final, err := store.ResolveReconciliationParent(context.Background(), lease, request)
	if err != nil || !final.GoalCleared {
		t.Fatalf("final after rollback = %#v, error=%v", final, err)
	}

	stored, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeReconciliationSession(stored)
	if err != nil {
		t.Fatal(err)
	}
	forged := decoded.goal
	forged.LastBlockResolution.Resolution.Reconciliation.ResolutionSetSHA256 = strings.Repeat("f", 64)
	forged.LastBlockResolution.Resolution.Reconciliation.TargetCount++
	forgedJSON, err := patchReconciliationSession(stored.StateJSON, forged, decoded.executionCursor)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), session.ID, forgedJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveReconciliationParent(context.Background(), lease, request); !errors.Is(err, ErrReconciliationRepairRequired) {
		t.Fatalf("forged final replay error = %v", err)
	}
}

func TestReconciliationCoordinatorRequiresLeaseContextAndRevision(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconcile-guards"
	session := createExecutionTestSession(t, store, workspaceID)
	runtime := newReconciliationGoalRuntime(t, session.ID)
	snapshot := blockNoToolGoal(t, runtime, "turn_guards")
	record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
	lease := acquireControlTestLease(t, store, session)
	ensure := EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.EnsureReconciliationGroup(canceled, lease, ensure); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled group ensure error = %v", err)
	}
	if _, _, err := store.EnsureReconciliationGroup(context.Background(), nil, ensure); !errors.Is(err, ErrControlLeaseRequired) {
		t.Fatalf("missing group lease error = %v", err)
	}
	wrongScope := ensure
	wrongScope.WorkspaceID = "/workspace/not-owned"
	if _, _, err := store.EnsureReconciliationGroup(context.Background(), lease, wrongScope); !errors.Is(err, ErrControlLeaseScope) {
		t.Fatalf("wrong-scope group lease error = %v", err)
	}
	staleEnsure := ensure
	staleEnsure.ExpectedSessionRevision++
	if _, _, err := store.EnsureReconciliationGroup(context.Background(), lease, staleEnsure); !errors.Is(err, ErrSessionStateConflict) {
		t.Fatalf("stale group revision error = %v", err)
	}

	group, inserted, err := store.EnsureReconciliationGroup(context.Background(), lease, ensure)
	if err != nil || !inserted {
		t.Fatalf("guard group = %#v inserted=%v error=%v", group, inserted, err)
	}
	parent := ResolveReconciliationParentRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
		ExpectedSessionRevision: record.Revision, Actor: "local-user", Evidence: reconciliationTurnEvidence("guards"),
	}
	if _, err := store.ResolveReconciliationParent(canceled, lease, parent); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled parent error = %v", err)
	}
	if _, err := store.ResolveReconciliationParent(context.Background(), nil, parent); !errors.Is(err, ErrControlLeaseRequired) {
		t.Fatalf("missing parent lease error = %v", err)
	}
	staleParent := parent
	staleParent.ExpectedSessionRevision++
	if _, err := store.ResolveReconciliationParent(context.Background(), lease, staleParent); !errors.Is(err, ErrSessionStateConflict) {
		t.Fatalf("stale parent revision error = %v", err)
	}
	current, err := store.GetReconciliationGroup(context.Background(), session.ID, workspaceID, group.GroupItemID)
	if err != nil || current.ParentResolution != nil {
		t.Fatalf("guard failures persisted parent = %#v, error=%v", current.ParentResolution, err)
	}
}

func TestReconciliationDetectsParentResolutionWithoutGoalReceipt(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconcile-mixed"
	session := createExecutionTestSession(t, store, workspaceID)
	runtime := newReconciliationGoalRuntime(t, session.ID)
	snapshot := blockNoToolGoal(t, runtime, "turn_mixed")
	record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
	lease := acquireControlTestLease(t, store, session)
	group, _, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := reconciliationTurnEvidence("mixed")
	target := reconciliation.GroupTarget{
		SessionID: group.SessionID, WorkspaceID: group.WorkspaceID,
		GoalID: group.GoalID, TurnID: group.TurnID, GroupItemID: group.GroupItemID,
		GroupPayloadSHA256: group.PayloadSHA256, BlockerReference: group.BlockerReference,
		GoalSnapshotSHA256: group.GoalSnapshotSHA256, SnapshotCursor: group.SnapshotCursor,
		MemberSetSHA256: group.MemberSetSHA256, ExecutionMemberCount: group.ExecutionMemberCount,
		Actor: "local-user",
	}
	envelope, err := evidence.Bind(target)
	if err != nil {
		t.Fatal(err)
	}
	evidenceJSON, evidenceSHA, err := envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	identitySHA := reconciliation.Hash("reconciliation-parent-resolution\x00" + group.GroupItemID + "\x00" + evidenceSHA)
	candidate := ReconciliationGroupResolution{
		ResolutionID: "recongrpres_" + identitySHA[:32], IdempotencyKey: "recongrpres_idem_" + identitySHA[:32],
		GroupItemID: group.GroupItemID, SessionID: session.ID, WorkspaceID: workspaceID,
		EvidenceJSON: evidenceJSON, EvidenceSHA256: evidenceSHA, ResolvedBy: "local-user",
	}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := appendGroupResolutionTx(context.Background(), tx, group, candidate, target); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	}); !errors.Is(err, ErrReconciliationRepairRequired) {
		t.Fatalf("mixed parent/session group replay error = %v", err)
	}

	request := ResolveReconciliationParentRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
		ExpectedSessionRevision: record.Revision, Actor: "local-user", Evidence: evidence,
	}
	if _, err := store.ResolveReconciliationParent(context.Background(), lease, request); !errors.Is(err, ErrReconciliationRepairRequired) {
		t.Fatalf("mixed parent/session error = %v", err)
	}
	unchanged, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil || unchanged.Revision != record.Revision || unchanged.StateJSON != record.StateJSON {
		t.Fatalf("mixed-state detection changed session = %#v, error=%v", unchanged, err)
	}
}

func TestReconciliationCompletedReplayRejectsMisalignedGoalBlockAndCursor(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*goal.Snapshot, *int64)
	}{
		{
			name: "cross goal receipt",
			mutate: func(snapshot *goal.Snapshot, _ *int64) {
				snapshot.ID = "goal_copied_receipt"
			},
		},
		{
			name: "misaligned blocker",
			mutate: func(snapshot *goal.Snapshot, _ *int64) {
				snapshot.LastBlockResolution.Reference = "different-blocker"
				snapshot.LastBlockResolution.Resolution.Reference = "different-blocker"
			},
		},
		{
			name: "cursor ahead of ledger",
			mutate: func(_ *goal.Snapshot, cursor *int64) {
				*cursor++
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := finalizeZeroReconciliation(t, fmt.Sprintf("%d", index))
			stored, err := fixture.store.GetSessionStateRecord(context.Background(), fixture.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeReconciliationSession(stored)
			if err != nil {
				t.Fatal(err)
			}
			forgedGoal, forgedCursor := decoded.goal, decoded.executionCursor
			test.mutate(&forgedGoal, &forgedCursor)
			forgedJSON, err := patchReconciliationSession(stored.StateJSON, forgedGoal, forgedCursor)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.store.SaveSessionState(context.Background(), fixture.session.ID, forgedJSON); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.store.ResolveReconciliationParent(context.Background(), fixture.lease, fixture.request); !errors.Is(err, ErrReconciliationRepairRequired) {
				t.Fatalf("misaligned completed replay error = %v", err)
			}
		})
	}
}

func TestReconciliationCompletedReplayRejectsMemberLifecycleDrift(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconcile-final-member-drift"
	session := createExecutionTestSession(t, store, workspaceID)
	turnID := "turn_final_member_drift"
	request := executionTestEvent(t, session.ID, workspaceID, "final-member-drift", execution.Effectful)
	request.Identity.TurnID = turnID
	started := appendStartedExecutionFixture(t, store, request)
	runtime := newReconciliationGoalRuntime(t, session.ID)
	snapshot := blockExecutionGoal(t, runtime, turnID, started.Identity.ExecutionID)
	record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
	lease := acquireControlTestLease(t, store, session)
	group, _, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	})
	if err != nil || len(group.Members) != 1 {
		t.Fatalf("member drift group = %#v, error=%v", group, err)
	}
	if _, err := store.ResolveExecutionReconciliation(context.Background(), lease, ResolveExecutionReconciliationRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
		ControlItemID: group.Members[0].ControlItemID, ExpectedSessionRevision: record.Revision,
		Actor: "local-user", Evidence: reconciliationEvidence("final-member-drift", reconciliation.DispositionEffectNotApplied),
	}); err != nil {
		t.Fatal(err)
	}
	parent := ResolveReconciliationParentRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
		ExpectedSessionRevision: record.Revision, Actor: "local-user",
		Evidence: reconciliationTurnEvidence("final-member-drift"),
	}
	if _, err := store.ResolveReconciliationParent(context.Background(), lease, parent); err != nil {
		t.Fatal(err)
	}
	completed := started
	completed.Type = execution.EventCompleted
	completed.ResultReceipt = "late completion after final reconciliation"
	completed.ResultSHA256 = execution.HashText(completed.ResultReceipt)
	completed.OccurredAt = started.OccurredAt.Add(time.Second)
	appendExecutionEvent(t, store, completed)

	if _, err := store.ResolveReconciliationParent(context.Background(), lease, parent); !errors.Is(err, ErrReconciliationProjectionRequired) {
		t.Fatalf("completed replay member drift error = %v", err)
	}
}

func TestReconciliationGroupRejectsPreseededNoncanonicalExecutionItem(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconcile-preseeded-item"
	session := createExecutionTestSession(t, store, workspaceID)
	turnID := "turn_preseeded_item"
	request := executionTestEvent(t, session.ID, workspaceID, "preseeded-item", execution.Effectful)
	request.Identity.TurnID = turnID
	started := appendStartedExecutionFixture(t, store, request)
	runtime := newReconciliationGoalRuntime(t, session.ID)
	snapshot := blockExecutionGoal(t, runtime, turnID, started.Identity.ExecutionID)
	record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
	lease := acquireControlTestLease(t, store, session)
	payload, payloadSHA, err := controlplane.MarshalDocument(map[string]any{"misleading": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, inserted, err := store.AppendControlItem(context.Background(), lease, controlplane.Item{
		ItemID: "ctrl_execution_preseeded", IdempotencyKey: "ctrlidem_execution_preseeded",
		Kind: controlplane.KindExecutionReconciliation,
		Identity: controlplane.Identity{
			SessionID: session.ID, WorkspaceID: workspaceID, GoalID: snapshot.ID,
			ExecutionID: started.Identity.ExecutionID, TurnID: turnID,
		},
		ExternalID: started.Identity.CanonicalCallID, Summary: "Reconcile a misleading target",
		PayloadJSON: payload, PayloadSHA256: payloadSHA,
	}); err != nil || !inserted {
		t.Fatalf("preseed execution item inserted=%v error=%v", inserted, err)
	}
	if _, _, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	}); !errors.Is(err, ErrReconciliationRepairRequired) {
		t.Fatalf("preseeded item group error = %v", err)
	}
	var groupCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM reconciliation_groups WHERE session_id = ?`, session.ID).Scan(&groupCount); err != nil {
		t.Fatal(err)
	}
	if groupCount != 0 {
		t.Fatalf("preseeded noncanonical item created %d reconciliation groups", groupCount)
	}
}
