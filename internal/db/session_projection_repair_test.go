package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/execution"
)

func repairTestLease(t *testing.T, store *Store, sessionID int64, workspaceID string) *ExecutionSessionLease {
	t.Helper()
	lease, err := store.AcquireExecutionSessionLease(context.Background(), sessionID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("release repair test lease: %v", err)
		}
	})
	return lease
}

func TestRepairSessionProjectionAdvancesCursorPastAnsweredEffects(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/projection-repair"
	session := createExecutionTestSession(t, store, workspaceID)
	if err := store.SaveSessionState(context.Background(), session.ID, `{"version":2,"goal":null,"execution_cursor":0}`); err != nil {
		t.Fatal(err)
	}
	lease := repairTestLease(t, store, session.ID, workspaceID)

	// Crash window: the effect completed durably but the transcript was never
	// saved, so the cursor still points before the terminal receipt.
	completed := appendCompletedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "crash-completed", execution.Effectful))
	failedStarted := appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "crash-failed", execution.Effectful))
	failed := failedStarted
	failed.Type = execution.EventFailed
	failed.ResultReceipt = "backend answered: exit status 7"
	failed.ResultSHA256 = execution.HashText(failed.ResultReceipt)
	failed.OccurredAt = failedStarted.OccurredAt.Add(time.Second)
	appendExecutionEvent(t, store, failed)

	receipt, err := store.RepairSessionProjection(context.Background(), lease, session.ID, workspaceID)
	if err != nil {
		t.Fatalf("projection repair failed: %v", err)
	}
	if receipt.PreviousCursor != 0 || receipt.NewCursor <= receipt.PreviousCursor {
		t.Fatalf("repair cursor movement = %+v", receipt)
	}
	if len(receipt.Repaired) != 2 {
		t.Fatalf("repaired effects = %#v, want the completed and failed executions", receipt.Repaired)
	}
	if receipt.Repaired[0].ExecutionID != completed.Identity.ExecutionID ||
		receipt.Repaired[1].EventType != execution.EventFailed ||
		receipt.Repaired[1].ResultReceipt != failed.ResultReceipt {
		t.Fatalf("repaired effect detail = %#v", receipt.Repaired)
	}

	// The durable state now carries the re-derived cursor at a bumped revision.
	record, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Revision != receipt.SessionRevision {
		t.Fatalf("state revision = %d, want %d", record.Revision, receipt.SessionRevision)
	}
	cursor, err := decodeSessionExecutionCursor(record.StateJSON)
	if err != nil || cursor != receipt.NewCursor {
		t.Fatalf("persisted cursor = %d (%v), want %d", cursor, err, receipt.NewCursor)
	}

	// Repaired state no longer reports recovery hazards, and a second repair
	// has nothing to do.
	hazards, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(hazards) != 0 {
		t.Fatalf("hazards after repair = %#v", hazards)
	}
	if _, err := store.RepairSessionProjection(context.Background(), lease, session.ID, workspaceID); !errors.Is(err, ErrSessionProjectionCurrent) {
		t.Fatalf("second repair error = %v, want current", err)
	}
}

func TestRepairSessionProjectionPagesPastReconciledBacklog(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/projection-repair-paging"
	session := createExecutionTestSession(t, store, workspaceID)
	if err := store.SaveSessionState(context.Background(), session.ID, `{"version":2,"goal":null,"execution_cursor":0}`); err != nil {
		t.Fatal(err)
	}
	lease := repairTestLease(t, store, session.ID, workspaceID)

	// More reconciled outcome_unknown rows than one raw projection page holds:
	// they sort first and must never hide later state from repair.
	backlog := effectiveProjectionPageSize + 5
	for i := 0; i < backlog; i++ {
		suffix := fmt.Sprintf("reconciled-%03d", i)
		event := appendOutcomeUnknownExecutionFixture(t, store,
			executionTestEvent(t, session.ID, workspaceID, suffix, execution.EffectUnknown))
		reconcileExecutionFixture(t, store, lease, event, suffix)
	}
	answered := appendCompletedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "beyond-page-answered", execution.Effectful))
	pending := appendOutcomeUnknownExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "beyond-page-pending", execution.EffectUnknown))

	// The unreconciled hazard sits beyond the first raw page; repair must
	// still find it and refuse.
	if _, err := store.RepairSessionProjection(context.Background(), lease, session.ID, workspaceID); !errors.Is(err, ErrSessionProjectionReconcileFirst) {
		t.Fatalf("repair with beyond-page pending reconciliation = %v, want reconcile-first refusal", err)
	}
	record, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cursor, err := decodeSessionExecutionCursor(record.StateJSON); err != nil || cursor != 0 {
		t.Fatalf("refused repair moved the cursor: %d (%v)", cursor, err)
	}

	// Once every pending execution has evidence, repair succeeds and reports
	// the answered effect that sat beyond the first page.
	reconcileExecutionFixture(t, store, lease, pending, "beyond-page-pending")
	receipt, err := store.RepairSessionProjection(context.Background(), lease, session.ID, workspaceID)
	if err != nil {
		t.Fatalf("repair after reconciling backlog: %v", err)
	}
	if receipt.AnsweredTotal != 1 || len(receipt.Repaired) != 1 ||
		receipt.Repaired[0].ExecutionID != answered.Identity.ExecutionID {
		t.Fatalf("beyond-page answered effect missing from receipt: %+v", receipt)
	}
}

func TestRepairSessionProjectionBoundsReceiptWithoutHidingTotals(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/projection-repair-totals"
	session := createExecutionTestSession(t, store, workspaceID)
	if err := store.SaveSessionState(context.Background(), session.ID, `{"version":2,"goal":null,"execution_cursor":0}`); err != nil {
		t.Fatal(err)
	}
	lease := repairTestLease(t, store, session.ID, workspaceID)

	total := maxExecutionRecoveryHazards + 7
	for i := 0; i < total; i++ {
		appendCompletedExecutionFixture(t, store,
			executionTestEvent(t, session.ID, workspaceID, fmt.Sprintf("answered-%03d", i), execution.Effectful))
	}
	receipt, err := store.RepairSessionProjection(context.Background(), lease, session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.AnsweredTotal != total {
		t.Fatalf("answered total = %d, want %d", receipt.AnsweredTotal, total)
	}
	if len(receipt.Repaired) != maxExecutionRecoveryHazards {
		t.Fatalf("bounded receipt detail = %d, want %d", len(receipt.Repaired), maxExecutionRecoveryHazards)
	}
}

func TestRepairSessionProjectionRefusesPendingReconciliationAndGoalSessions(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/projection-repair-refusals"
	session := createExecutionTestSession(t, store, workspaceID)
	if err := store.SaveSessionState(context.Background(), session.ID, `{"version":2,"goal":null,"execution_cursor":0}`); err != nil {
		t.Fatal(err)
	}
	lease := repairTestLease(t, store, session.ID, workspaceID)

	unknownStarted := appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "pending-unknown", execution.EffectUnknown))
	unknown := unknownStarted
	unknown.Type = execution.EventOutcomeUnknown
	unknown.ResultReceipt = "backend result not durable"
	unknown.ResultSHA256 = execution.HashText(unknown.ResultReceipt)
	unknown.OccurredAt = unknownStarted.OccurredAt.Add(time.Second)
	appendExecutionEvent(t, store, unknown)
	appendCompletedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "answered", execution.Effectful))

	if _, err := store.RepairSessionProjection(context.Background(), lease, session.ID, workspaceID); !errors.Is(err, ErrSessionProjectionReconcileFirst) {
		t.Fatalf("repair with pending reconciliation = %v, want reconcile-first refusal", err)
	}
	record, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cursor, err := decodeSessionExecutionCursor(record.StateJSON); err != nil || cursor != 0 {
		t.Fatalf("refused repair moved the cursor: %d (%v)", cursor, err)
	}

	goalSession := createExecutionTestSession(t, store, workspaceID)
	if err := store.SaveSessionState(context.Background(), goalSession.ID, `{"version":2,"goal":{"id":"goal-owned"}}`); err != nil {
		t.Fatal(err)
	}
	goalLease := repairTestLease(t, store, goalSession.ID, workspaceID)
	appendCompletedExecutionFixture(t, store,
		executionTestEvent(t, goalSession.ID, workspaceID, "goal-effect", execution.Effectful))
	if _, err := store.RepairSessionProjection(context.Background(), goalLease, goalSession.ID, workspaceID); !errors.Is(err, ErrStandaloneReconciliationGoalOwned) {
		t.Fatalf("goal-owned repair error = %v, want goal-owned refusal", err)
	}
}

// A malformed version must be reported as malformed. The decode error used to
// share a branch with the range check, so a string, float, or null version
// produced "unsupported session envelope version 0" — a value the writer never
// persists — sending the operator of a last-resort recovery command after a
// compatibility problem that does not exist.
func TestSessionEnvelopeVersionErrorsDistinguishMalformedFromUnsupported(t *testing.T) {
	for name, envelope := range map[string]string{
		"string":  `{"version":"1"}`,
		"float":   `{"version":1.5}`,
		"boolean": `{"version":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeSessionExecutionCursor(envelope)
			if err == nil {
				t.Fatal("a malformed version was accepted")
			}
			if strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("malformed version reported as a version mismatch: %v", err)
			}
			if !strings.Contains(err.Error(), "decode session envelope version") {
				t.Fatalf("error does not identify the decode failure: %v", err)
			}
		})
	}

	// An explicit null decodes without error to the zero value, so it is an
	// absent version rather than a malformed one.
	if _, err := decodeSessionExecutionCursor(`{"version":null}`); err == nil ||
		!strings.Contains(err.Error(), "has no version") {
		t.Fatalf("explicit null version = %v, want the absent-version diagnosis", err)
	}

	// A well-formed but unknown version keeps the compatibility wording.
	if _, err := decodeSessionExecutionCursor(`{"version":99}`); err == nil ||
		!strings.Contains(err.Error(), "unsupported session envelope version 99") {
		t.Fatalf("unsupported version lost its number: %v", err)
	}
}
