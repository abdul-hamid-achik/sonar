package db

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

func TestInspectReconciliationGroupIsReadOnlyAndRequiresExistingGroup(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconcile-inspect-zero"
	session := createExecutionTestSession(t, store, workspaceID)
	runtime := newReconciliationGoalRuntime(t, session.ID)
	snapshot := blockNoToolGoal(t, runtime, "turn_inspect_zero")
	record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)

	before := reconciliationTableCounts(t, store)
	if _, err := store.InspectReconciliationGroup(context.Background(), session.ID, workspaceID); !errors.Is(err, ErrReconciliationGroupNotFound) {
		t.Fatalf("inspection without group error = %v", err)
	}
	after := reconciliationTableCounts(t, store)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only inspection mutated tables: before=%v after=%v", before, after)
	}
	unchanged, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil || unchanged.Revision != record.Revision || unchanged.StateJSON != record.StateJSON {
		t.Fatalf("read-only inspection changed session: %#v error=%v", unchanged, err)
	}

	lease := acquireControlTestLease(t, store, session)
	group, inserted, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	})
	if err != nil || !inserted {
		t.Fatalf("ensure group = %#v inserted=%v error=%v", group, inserted, err)
	}
	inspection, err := store.InspectReconciliationGroup(context.Background(), session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.SessionRevision != record.Revision || inspection.GoalState != goal.StateBlocked ||
		inspection.Group.GroupItemID != group.GroupItemID || inspection.TurnID != group.TurnID ||
		inspection.Group.ExecutionMemberCount != 0 || inspection.Group.ParentResolution != nil {
		t.Fatalf("zero-member inspection = %#v", inspection)
	}
}

func TestInspectReconciliationGroupReportsMemberAndResolvedParentState(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconcile-inspect-member"
	session := createExecutionTestSession(t, store, workspaceID)
	turnID := "turn_inspect_member"
	request := executionTestEvent(t, session.ID, workspaceID, "inspect-member", execution.EffectUnknown)
	request.Identity.TurnID = turnID
	unknown := appendOutcomeUnknownExecutionFixture(t, store, request)
	runtime := newReconciliationGoalRuntime(t, session.ID)
	snapshot := blockExecutionGoal(t, runtime, turnID, unknown.Identity.ExecutionID)
	record := persistReconciliationGoalSession(t, store, session.ID, snapshot, 0)
	lease := acquireControlTestLease(t, store, session)
	group, _, err := store.EnsureReconciliationGroup(context.Background(), lease, EnsureReconciliationGroupRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, ExpectedSessionRevision: record.Revision,
	})
	if err != nil || len(group.Members) != 1 {
		t.Fatalf("member group = %#v error=%v", group, err)
	}

	inspection, err := store.InspectReconciliationGroup(context.Background(), session.ID, workspaceID)
	if err != nil || inspection.Group.Members[0].Resolved {
		t.Fatalf("unresolved inspection = %#v error=%v", inspection, err)
	}
	_, err = store.ResolveExecutionReconciliation(context.Background(), lease, ResolveExecutionReconciliationRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
		ControlItemID: group.Members[0].ControlItemID, ExpectedSessionRevision: record.Revision,
		Actor: "local-user", Evidence: reconciliationEvidence("inspect", reconciliation.DispositionEffectNotApplied),
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err = store.InspectReconciliationGroup(context.Background(), session.ID, workspaceID)
	if err != nil || !inspection.Group.Members[0].Resolved || inspection.Group.ParentResolution != nil {
		t.Fatalf("resolved member inspection = %#v error=%v", inspection, err)
	}
	final, err := store.ResolveReconciliationParent(context.Background(), lease, ResolveReconciliationParentRequest{
		SessionID: session.ID, WorkspaceID: workspaceID, GroupItemID: group.GroupItemID,
		ExpectedSessionRevision: record.Revision, Actor: "local-user", Evidence: reconciliationTurnEvidence("inspect"),
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err = store.InspectReconciliationGroup(context.Background(), session.ID, workspaceID)
	if err != nil || inspection.SessionRevision != final.SessionRevision || inspection.GoalState != goal.StatePaused ||
		inspection.Group.ParentResolution == nil || inspection.Group.ParentResolution.ResolutionID != final.ResolutionID {
		t.Fatalf("final inspection = %#v error=%v", inspection, err)
	}
}

func reconciliationTableCounts(t *testing.T, store *Store) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for _, table := range []string{"reconciliation_groups", "reconciliation_group_members", "reconciliation_group_resolutions", "control_items", "control_resolutions"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table] = count
	}
	return counts
}
