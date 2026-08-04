package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

func standaloneReconciliationFixture(t *testing.T, suffix string) (*Store, Session, *ExecutionSessionLease, execution.Event, SessionStateRecord) {
	t.Helper()
	store := testStore(t)
	workspace := "/workspace/standalone-" + suffix
	session := createExecutionTestSession(t, store, workspace)
	if err := store.SaveSessionState(context.Background(), session.ID, `{"version":2,"goal":null,"execution_cursor":0}`); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	unknown := appendOutcomeUnknownExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspace, suffix, execution.EffectUnknown))
	lease := acquireControlTestLease(t, store, session)
	return store, session, lease, unknown, record
}

func standaloneEvidence(suffix string) reconciliation.Request {
	return reconciliation.Request{
		Disposition: reconciliation.DispositionEffectNotApplied,
		Source: reconciliation.Source{
			Kind:       reconciliation.SourceVerificationCheck,
			Reference:  "workspace-check:" + suffix,
			ObservedAt: executionTestTime.Add(10 * time.Minute).UTC(),
		},
		Summary: "Inspected the workspace and verified the external effect was not applied.",
	}
}

func TestStandaloneExecutionReconciliationInspectApplyAndReplay(t *testing.T) {
	store, session, lease, unknown, record := standaloneReconciliationFixture(t, "roundtrip")
	ctx := context.Background()

	inspection, err := store.InspectStandaloneExecutionReconciliation(ctx, session.ID, session.WorkspaceID, unknown.Identity.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Resolved || inspection.EventID != unknown.ID || inspection.SessionRevision != record.Revision || inspection.ItemID == "" {
		t.Fatalf("inspection = %#v", inspection)
	}
	beforeProjection, err := store.ProjectExecutionRecovery(ctx, session.ID, session.WorkspaceID, 0, 100)
	if err != nil || len(beforeProjection.Hazards) != 1 || len(beforeProjection.Contexts) != 0 {
		t.Fatalf("pre-resolution projection = %#v, error=%v", beforeProjection, err)
	}
	states, err := store.ListControlStates(ctx, controlplane.Query{
		SessionID: session.ID, WorkspaceID: session.WorkspaceID,
		Kind: controlplane.KindExecutionReconciliation, Limit: 10,
	})
	if err != nil || len(states) != 0 {
		t.Fatalf("read-only inspection created state: %#v, error=%v", states, err)
	}

	request := ResolveStandaloneExecutionReconciliationRequest{
		SessionID: session.ID, WorkspaceID: session.WorkspaceID,
		ExecutionID:             unknown.Identity.ExecutionID,
		ExpectedSessionRevision: record.Revision, ExpectedEventID: unknown.ID,
		Actor: "local-user", Evidence: standaloneEvidence("roundtrip"),
	}
	receipt, err := store.ResolveStandaloneExecutionReconciliation(ctx, lease, request)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Inserted || receipt.EventID != unknown.ID || receipt.SessionRevision != record.Revision || receipt.ResolutionID == "" ||
		receipt.Context.ResolutionID != receipt.ResolutionID || receipt.Context.Disposition != reconciliation.DispositionEffectNotApplied ||
		receipt.Context.SourceKind != reconciliation.SourceVerificationCheck || receipt.Context.EvidenceSHA256 == "" ||
		receipt.Context.ExecutionID != unknown.Identity.ExecutionID || receipt.Context.ToolName != unknown.Identity.ToolName ||
		receipt.Context.ArgumentsSHA256 != unknown.ArgumentsSHA256 {
		t.Fatalf("receipt = %#v", receipt)
	}
	replay, err := store.ResolveStandaloneExecutionReconciliation(ctx, lease, request)
	if err != nil || replay.Inserted || replay.ResolutionID != receipt.ResolutionID {
		t.Fatalf("replay = %#v, error=%v", replay, err)
	}
	hazards, err := store.ListExecutionRecoveryHazards(ctx, session.ID, session.WorkspaceID, 0, 100)
	if err != nil || len(hazards) != 0 {
		t.Fatalf("effective hazards = %#v, error=%v", hazards, err)
	}
	afterProjection, err := store.ProjectExecutionRecovery(ctx, session.ID, session.WorkspaceID, 0, 100)
	if err != nil || len(afterProjection.Hazards) != 0 || len(afterProjection.Contexts) != 1 ||
		afterProjection.Contexts[0] != receipt.Context {
		t.Fatalf("post-resolution projection = %#v, error=%v", afterProjection, err)
	}
	// Outcome-unknown reconciliations remain necessary model context even when
	// a later snapshot cursor is nonzero; the cursor never authorizes forgetting
	// the immutable target/disposition mapping.
	afterAdvancedCursor, err := store.ProjectExecutionRecovery(ctx, session.ID, session.WorkspaceID, unknown.ID+100, 100)
	if err != nil || len(afterAdvancedCursor.Hazards) != 0 || len(afterAdvancedCursor.Contexts) != 1 ||
		afterAdvancedCursor.Contexts[0] != receipt.Context {
		t.Fatalf("advanced-cursor projection = %#v, error=%v", afterAdvancedCursor, err)
	}
	raw, err := store.GetExecutionState(ctx, session.ID, session.WorkspaceID, unknown.Identity.ExecutionID)
	if err != nil || raw.Latest.Type != execution.EventOutcomeUnknown || raw.Latest.ID != unknown.ID {
		t.Fatalf("immutable ledger changed: %#v, error=%v", raw, err)
	}
	after, err := store.GetSessionStateRecord(ctx, session.ID)
	if err != nil || after.Revision != record.Revision || after.StateJSON != record.StateJSON {
		t.Fatalf("session state changed: %#v, error=%v", after, err)
	}
	resolved, err := store.InspectStandaloneExecutionReconciliation(ctx, session.ID, session.WorkspaceID, unknown.Identity.ExecutionID)
	if err != nil || !resolved.Resolved || resolved.ResolutionID != receipt.ResolutionID || resolved.Context != receipt.Context {
		t.Fatalf("resolved inspection = %#v, error=%v", resolved, err)
	}
}

func TestStandaloneExecutionReconciliationFailsClosedOnStaleOrUnleasedApply(t *testing.T) {
	store, session, lease, unknown, record := standaloneReconciliationFixture(t, "guards")
	base := ResolveStandaloneExecutionReconciliationRequest{
		SessionID: session.ID, WorkspaceID: session.WorkspaceID,
		ExecutionID:             unknown.Identity.ExecutionID,
		ExpectedSessionRevision: record.Revision, ExpectedEventID: unknown.ID,
		Actor: "local-user", Evidence: standaloneEvidence("guards"),
	}
	for name, mutate := range map[string]func(*ResolveStandaloneExecutionReconciliationRequest){
		"stale revision": func(r *ResolveStandaloneExecutionReconciliationRequest) { r.ExpectedSessionRevision++ },
		"stale event":    func(r *ResolveStandaloneExecutionReconciliationRequest) { r.ExpectedEventID++ },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			if _, err := store.ResolveStandaloneExecutionReconciliation(context.Background(), lease, candidate); err == nil {
				t.Fatal("stale recovery was accepted")
			}
		})
	}
	if _, err := store.ResolveStandaloneExecutionReconciliation(context.Background(), nil, base); !errors.Is(err, ErrControlLeaseRequired) {
		t.Fatalf("unleased apply error = %v", err)
	}
	states, err := store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: session.ID, WorkspaceID: session.WorkspaceID,
		Kind: controlplane.KindExecutionReconciliation, Limit: 10,
	})
	if err != nil || len(states) != 0 {
		t.Fatalf("failed apply mutated state: %#v, error=%v", states, err)
	}
}

func TestStandaloneExecutionReconciliationCannotBypassGoalRecovery(t *testing.T) {
	store, session, lease, unknown, record := standaloneReconciliationFixture(t, "goal-owned")
	if err := store.SaveSessionState(context.Background(), session.ID, `{"version":2,"goal":{"id":"goal-owned"}}`); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InspectStandaloneExecutionReconciliation(context.Background(), session.ID, session.WorkspaceID, unknown.Identity.ExecutionID); !errors.Is(err, ErrStandaloneReconciliationGoalOwned) {
		t.Fatalf("goal-owned inspection error = %v", err)
	}
	_, err = store.ResolveStandaloneExecutionReconciliation(context.Background(), lease, ResolveStandaloneExecutionReconciliationRequest{
		SessionID: session.ID, WorkspaceID: session.WorkspaceID,
		ExecutionID:             unknown.Identity.ExecutionID,
		ExpectedSessionRevision: record.Revision, ExpectedEventID: unknown.ID,
		Actor: "local-user", Evidence: standaloneEvidence("goal-owned"),
	})
	if !errors.Is(err, ErrStandaloneReconciliationGoalOwned) {
		t.Fatalf("goal-owned apply error = %v", err)
	}
	if _, err := store.ProjectExecutionRecovery(context.Background(), session.ID, session.WorkspaceID, 0, 100); !errors.Is(err, ErrStandaloneReconciliationGoalOwned) {
		t.Fatalf("goal-owned projection error = %v", err)
	}
	if _, err := store.ListStandaloneExecutionReconciliationPending(context.Background(), session.ID, session.WorkspaceID, 100); !errors.Is(err, ErrStandaloneReconciliationGoalOwned) {
		t.Fatalf("goal-owned pending-list error = %v", err)
	}
}
