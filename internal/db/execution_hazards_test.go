package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

func appendOutcomeUnknownExecutionFixture(t *testing.T, store *Store, requested execution.Event) execution.Event {
	t.Helper()
	started := appendStartedExecutionFixture(t, store, requested)
	unknown := started
	unknown.Type = execution.EventOutcomeUnknown
	unknown.ResultReceipt = "external effect outcome is unknown"
	unknown.ResultSHA256 = execution.HashText(unknown.ResultReceipt)
	unknown.OccurredAt = started.OccurredAt.Add(time.Second)
	return appendExecutionEvent(t, store, unknown)
}

func appendExecutionReconciliationItemFixture(t *testing.T, store *Store, lease *ExecutionSessionLease, event execution.Event, suffix string) controlplane.Item {
	t.Helper()
	item := controlTestItem(t, event.Identity.SessionID, event.Identity.WorkspaceID, suffix, controlplane.KindExecutionReconciliation)
	item.Identity.ExecutionID = event.Identity.ExecutionID
	item.Identity.TurnID = event.Identity.TurnID
	stored, inserted, err := store.AppendControlItem(context.Background(), lease, item)
	if err != nil || !inserted {
		t.Fatalf("append reconciliation item %s inserted=%v error=%v", suffix, inserted, err)
	}
	return stored
}

func reconcileExecutionFixture(t *testing.T, store *Store, lease *ExecutionSessionLease, event execution.Event, suffix string) controlplane.Resolution {
	t.Helper()
	item := appendExecutionReconciliationItemFixture(t, store, lease, event, suffix)
	resolution := controlTestExecutionResolution(t, store, item, suffix, reconciliation.DispositionEffectNotApplied)
	stored, inserted := resolveExecutionReconciliationTestTx(t, store, lease, resolution)
	if !inserted {
		t.Fatalf("resolution %s was unexpectedly replayed", suffix)
	}
	return stored
}

func TestEffectiveHazardProjectionSuppressesTypedReceiptButPreservesRawLedger(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/effective-overlay"
	session := createExecutionTestSession(t, store, workspaceID)
	lease := acquireControlTestLease(t, store, session)

	unknown := appendOutcomeUnknownExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "overlay-unknown", execution.EffectUnknown))
	started := appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "overlay-started", execution.Effectful))
	before, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 100)
	if err != nil || len(before) != 2 {
		t.Fatalf("raw effective hazards before resolution = %#v, error=%v", before, err)
	}

	reconcileExecutionFixture(t, store, lease, unknown, "overlay-unknown")
	reconcileExecutionFixture(t, store, lease, started, "overlay-started")

	after, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 100)
	if err != nil || len(after) != 0 {
		t.Fatalf("effective hazards after resolution = %#v, error=%v", after, err)
	}
	unresolved, err := store.ListUnresolvedExecutions(context.Background(), session.ID, workspaceID, 100)
	if err != nil || len(unresolved) != 0 {
		t.Fatalf("effective unresolved after resolution = %#v, error=%v", unresolved, err)
	}
	for _, event := range []execution.Event{unknown, started} {
		targets, err := store.ListExecutionReconciliationTargets(context.Background(), session.ID, workspaceID, event.Identity.TurnID, 100)
		if err != nil || len(targets) != 0 {
			t.Fatalf("turn %s targets = %#v, error=%v", event.Identity.TurnID, targets, err)
		}
		raw, err := store.GetExecutionState(context.Background(), session.ID, workspaceID, event.Identity.ExecutionID)
		if err != nil || raw.Latest.Type != event.Type || raw.Latest.ID != event.ID {
			t.Fatalf("raw execution changed after overlay: %#v, error=%v", raw, err)
		}
	}
}

func TestEffectiveHazardProjectionPendingAndCompletedRemainVisible(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/effective-pending"
	session := createExecutionTestSession(t, store, workspaceID)
	lease := acquireControlTestLease(t, store, session)

	pending := appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "pending", execution.Effectful))
	appendExecutionReconciliationItemFixture(t, store, lease, pending, "pending")
	hazards, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 100)
	if err != nil || len(hazards) != 1 || hazards[0].Identity.ExecutionID != pending.Identity.ExecutionID {
		t.Fatalf("pending reconciliation hazard = %#v, error=%v", hazards, err)
	}

	completedStarted := appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "later-completed", execution.Effectful))
	reconcileExecutionFixture(t, store, lease, completedStarted, "later-completed")
	cursor := completedStarted.ID
	completed := completedStarted
	completed.Type = execution.EventCompleted
	completed.ResultReceipt = "backend completed after inspection"
	completed.ResultSHA256 = execution.HashText(completed.ResultReceipt)
	completed.OccurredAt = completedStarted.OccurredAt.Add(time.Second)
	completed = appendExecutionEvent(t, store, completed)
	hazards, err = store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundCompleted := false
	for _, hazard := range hazards {
		if hazard.Identity.ExecutionID == completed.Identity.ExecutionID {
			foundCompleted = hazard.Latest.Type == execution.EventCompleted
		}
	}
	if !foundCompleted {
		t.Fatalf("post-cursor completion was hidden by earlier reconciliation: %#v", hazards)
	}
}

func TestExecutionReconciliationTypedTargetForgeryFailsClosed(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconciliation-forgery"
	session := createExecutionTestSession(t, store, workspaceID)
	lease := acquireControlTestLease(t, store, session)
	event := appendOutcomeUnknownExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "forgery", execution.EffectUnknown))
	item := appendExecutionReconciliationItemFixture(t, store, lease, event, "forgery")
	valid := controlTestExecutionResolution(t, store, item, "forgery", reconciliation.DispositionEffectApplied)
	envelope, err := reconciliation.Parse(valid.EvidenceJSON, valid.EvidenceSHA256)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Actor = "forged-actor"
	forged := valid
	forged.EvidenceJSON, forged.EvidenceSHA256, err = envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	release, err := store.holdControlLease(context.Background(), lease, session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		release()
		t.Fatal(err)
	}
	_, _, resolveErr := resolveControlItemTx(context.Background(), tx, forged, true)
	_ = tx.Rollback()
	release()
	if !errors.Is(resolveErr, ErrControlReconciliationInvalid) {
		t.Fatalf("forged target error = %v", resolveErr)
	}
	state, err := store.GetControlState(context.Background(), session.ID, workspaceID, item.ItemID)
	if err != nil || !state.Pending() {
		t.Fatalf("forged resolution changed item state = %#v, error=%v", state, err)
	}
}

func TestEffectiveHazardProjectionRejectsDirectSQLCorruptionAndDuplicates(t *testing.T) {
	t.Run("malformed resolution", func(t *testing.T) {
		store := testStore(t)
		workspaceID := "/workspace/reconciliation-corrupt"
		session := createExecutionTestSession(t, store, workspaceID)
		lease := acquireControlTestLease(t, store, session)
		event := appendOutcomeUnknownExecutionFixture(t, store,
			executionTestEvent(t, session.ID, workspaceID, "corrupt", execution.EffectUnknown))
		item := appendExecutionReconciliationItemFixture(t, store, lease, event, "corrupt")
		malformed := controlTestResolution(t, item, "corrupt", controlplane.OutcomeReconciled)
		if _, err := store.db.Exec(`
			INSERT INTO control_resolutions (
				resolution_id, idempotency_key, item_id, session_id, workspace_id,
				outcome, evidence_json, evidence_sha256, resolved_by, detail, resolved_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			malformed.ResolutionID, malformed.IdempotencyKey, malformed.ItemID,
			malformed.SessionID, malformed.WorkspaceID, malformed.Outcome,
			malformed.EvidenceJSON, malformed.EvidenceSHA256, malformed.ResolvedBy,
			malformed.Detail, formatExecutionTime(malformed.ResolvedAt),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 100); !errors.Is(err, ErrExecutionReconciliationCorrupt) {
			t.Fatalf("corrupt resolution projection error = %v", err)
		}
	})

	t.Run("duplicate items", func(t *testing.T) {
		store := testStore(t)
		workspaceID := "/workspace/reconciliation-duplicate"
		session := createExecutionTestSession(t, store, workspaceID)
		lease := acquireControlTestLease(t, store, session)
		event := appendStartedExecutionFixture(t, store,
			executionTestEvent(t, session.ID, workspaceID, "duplicate", execution.Effectful))
		appendExecutionReconciliationItemFixture(t, store, lease, event, "duplicate-first")
		second := controlTestItem(t, session.ID, workspaceID, "duplicate-second", controlplane.KindExecutionReconciliation)
		second.Identity.ExecutionID = event.Identity.ExecutionID
		second.Identity.TurnID = event.Identity.TurnID
		if _, err := store.db.Exec(`DROP INDEX ux_control_items_execution_reconciliation_target`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`
			INSERT INTO control_items (
				item_id, idempotency_key, session_id, workspace_id, kind,
				goal_id, execution_id, turn_id, external_id, summary,
				payload_json, payload_sha256, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			second.ItemID, second.IdempotencyKey, session.ID, workspaceID, second.Kind,
			second.Identity.GoalID, second.Identity.ExecutionID, second.Identity.TurnID,
			second.ExternalID, second.Summary, second.PayloadJSON, second.PayloadSHA256,
			formatExecutionTime(second.CreatedAt),
		); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 100); !errors.Is(err, ErrExecutionReconciliationCorrupt) {
			t.Fatalf("duplicate projection error = %v", err)
		}
	})
}

func TestEffectiveHazardProjectionFiltersBeforeLimitAndDetectsOverflow(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconciliation-pagination"
	session := createExecutionTestSession(t, store, workspaceID)
	if err := store.SaveSessionState(context.Background(), session.ID, `{"version":2,"goal":null,"execution_cursor":0}`); err != nil {
		t.Fatal(err)
	}
	lease := acquireControlTestLease(t, store, session)

	for index := 0; index < effectiveProjectionPageSize+1; index++ {
		suffix := "page-" + strconv.Itoa(index)
		event := appendStartedExecutionFixture(t, store,
			executionTestEvent(t, session.ID, workspaceID, suffix, execution.Effectful))
		reconcileExecutionFixture(t, store, lease, event, suffix)
	}
	unresolvedEvent := executionTestEvent(t, session.ID, workspaceID, "after-reconciled-page", execution.EffectReadOnly)
	appendExecutionEvent(t, store, unresolvedEvent)
	states, err := store.ListUnresolvedExecutions(context.Background(), session.ID, workspaceID, 1)
	if err != nil || len(states) != 1 || states[0].Identity.ExecutionID != unresolvedEvent.Identity.ExecutionID {
		t.Fatalf("filter-before-limit states = %#v, error=%v", states, err)
	}
	hazards, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 100)
	if err != nil || len(hazards) != 0 {
		t.Fatalf("paged reconciled recovery hazards = %#v, error=%v", hazards, err)
	}

	for index := 0; index < 2; index++ {
		suffix := fmt.Sprintf("overflow-%d", index)
		event := executionTestEvent(t, session.ID, workspaceID, suffix, execution.Effectful)
		event.Identity.TurnID = "turn-overflow"
		appendStartedExecutionFixture(t, store, event)
	}
	if _, err := store.ListExecutionReconciliationTargets(context.Background(), session.ID, workspaceID, "turn-overflow", 1); !errors.Is(err, ErrExecutionHazardOverflow) {
		t.Fatalf("reconciliation target overflow error = %v", err)
	}
	if _, err := store.ListStandaloneExecutionReconciliationPending(context.Background(), session.ID, workspaceID, 1); !errors.Is(err, ErrExecutionHazardOverflow) {
		t.Fatalf("standalone reconciliation pending overflow error = %v", err)
	}
	if _, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 1); !errors.Is(err, ErrExecutionHazardOverflow) {
		t.Fatalf("recovery hazard overflow error = %v", err)
	}
	truncated, err := store.ListUnresolvedExecutions(context.Background(), session.ID, workspaceID, 1)
	if err != nil || len(truncated) != 1 {
		t.Fatalf("observational unresolved bound = %#v, error=%v", truncated, err)
	}
}

func TestExecutionReconciliationSchemaGuardsExactTurnAndUniqueTarget(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconciliation-schema"
	session := createExecutionTestSession(t, store, workspaceID)
	lease := acquireControlTestLease(t, store, session)
	event := appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "schema", execution.Effectful))
	first := appendExecutionReconciliationItemFixture(t, store, lease, event, "schema-first")
	second := controlTestItem(t, session.ID, workspaceID, "schema-second", controlplane.KindExecutionReconciliation)
	second.Identity.ExecutionID = event.Identity.ExecutionID
	second.Identity.TurnID = event.Identity.TurnID
	if _, _, err := store.AppendControlItem(context.Background(), lease, second); !errors.Is(err, ErrControlItemConflict) {
		t.Fatalf("duplicate Store target error = %v", err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO control_items (
			item_id, idempotency_key, session_id, workspace_id, kind,
			goal_id, execution_id, turn_id, summary, payload_json, payload_sha256, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		second.ItemID, second.IdempotencyKey, session.ID, workspaceID, second.Kind,
		second.Identity.GoalID, second.Identity.ExecutionID, second.Identity.TurnID,
		second.Summary, second.PayloadJSON, second.PayloadSHA256, formatExecutionTime(second.CreatedAt),
	); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint") {
		t.Fatalf("duplicate SQL target error = %v", err)
	}
	wrongTurn := controlTestItem(t, session.ID, workspaceID, "schema-wrong-turn", controlplane.KindExecutionReconciliation)
	wrongTurn.Identity.ExecutionID = "exec-schema-other"
	other := executionTestEvent(t, session.ID, workspaceID, "schema-other", execution.Effectful)
	other.Identity.ExecutionID = wrongTurn.Identity.ExecutionID
	other.Identity.IdempotencyKey = "idem-schema-other-unique"
	other.Identity.TurnID = "turn-schema-real"
	other.Identity.CanonicalCallID = "call-schema-other-unique"
	appendStartedExecutionFixture(t, store, other)
	wrongTurn.Identity.TurnID = "turn-schema-forged"
	if _, err := store.db.Exec(`
		INSERT INTO control_items (
			item_id, idempotency_key, session_id, workspace_id, kind,
			goal_id, execution_id, turn_id, summary, payload_json, payload_sha256, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		wrongTurn.ItemID, wrongTurn.IdempotencyKey, session.ID, workspaceID, wrongTurn.Kind,
		wrongTurn.Identity.GoalID, wrongTurn.Identity.ExecutionID, wrongTurn.Identity.TurnID,
		wrongTurn.Summary, wrongTurn.PayloadJSON, wrongTurn.PayloadSHA256, formatExecutionTime(wrongTurn.CreatedAt),
	); err == nil || !strings.Contains(err.Error(), "exact latest hazardous turn") {
		t.Fatalf("wrong-turn SQL target error = %v", err)
	}
	_ = first
}
