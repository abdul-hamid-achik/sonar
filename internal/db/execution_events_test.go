package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/execution"
)

var executionTestTime = time.Date(2026, time.July, 11, 12, 30, 0, 123_000_000, time.UTC)

func createExecutionTestSession(t *testing.T, store *Store, workspaceID string) Session {
	t.Helper()
	session, err := store.CreateSession(context.Background(), CreateSessionParams{
		Title: "execution ledger", Model: "qwen", Mode: "BUILD", WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func cleanupExecutionTestStore(t *testing.T, store *Store) {
	t.Helper()
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close execution test store: %v", err)
		}
	})
}

func executionTestEvent(t *testing.T, sessionID int64, workspaceID, suffix string, effect execution.EffectClass) execution.Event {
	t.Helper()
	argumentsSHA256, err := execution.HashCanonicalArguments(map[string]any{"path": suffix + ".txt"})
	if err != nil {
		t.Fatal(err)
	}
	return execution.Event{
		Identity: execution.Identity{
			SessionID: sessionID, WorkspaceID: workspaceID,
			RunID: "run-" + suffix, TurnID: "turn-" + suffix,
			ExecutionID: "exec-" + suffix, IdempotencyKey: "idem-" + suffix,
			ProviderCallID: "provider-" + suffix, CanonicalCallID: "call-" + suffix,
			ToolName: "read", Iteration: 1, Ordinal: 1,
			Kind: execution.KindBuiltin, EffectClass: effect,
		},
		Type:            execution.EventRequested,
		Approval:        execution.ApprovalNotApplicable,
		ArgumentsSHA256: argumentsSHA256,
		OccurredAt:      executionTestTime,
	}
}

func appendExecutionEvent(t *testing.T, store *Store, event execution.Event) execution.Event {
	t.Helper()
	stored, inserted, err := store.AppendExecutionEvent(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatalf("event %s was unexpectedly treated as a replay", event.Type)
	}
	return stored
}

func appendStartedExecutionFixture(t *testing.T, store *Store, requested execution.Event) execution.Event {
	t.Helper()
	appendExecutionEvent(t, store, requested)
	previous := requested
	if requested.Identity.EffectClass != execution.EffectReadOnly {
		approved := requested
		approved.Type = execution.EventApproved
		approved.Approval = execution.ApprovalEmbedding
		approved.OccurredAt = requested.OccurredAt.Add(time.Second)
		appendExecutionEvent(t, store, approved)
		previous = approved
	}
	started := previous
	started.Type = execution.EventStarted
	started.Approval = execution.ApprovalNotApplicable
	started.OccurredAt = previous.OccurredAt.Add(time.Second)
	return appendExecutionEvent(t, store, started)
}

func appendCompletedExecutionFixture(t *testing.T, store *Store, requested execution.Event) execution.Event {
	t.Helper()
	started := appendStartedExecutionFixture(t, store, requested)
	completed := started
	completed.Type = execution.EventCompleted
	completed.ResultReceipt = "backend completed"
	completed.ResultSHA256 = execution.HashText(completed.ResultReceipt)
	completed.OccurredAt = started.OccurredAt.Add(time.Second)
	return appendExecutionEvent(t, store, completed)
}

func TestExecutionMigrationPreservesSessionStateV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-ledger.db")
	conn, err := sql.Open("sqlite", path+"?_foreign_keys=ON")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_init.sql", "002_checkpoints.sql", "003_session_state.sql"} {
		data, readErr := migrations.ReadFile("migrations/" + name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err := conn.Exec(string(data)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	result, err := conn.Exec(`INSERT INTO sessions (title, model, mode, workspace_id) VALUES ('legacy', 'qwen', 'BUILD', '/workspace')`)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	const stateV1 = `{"version":1,"messages":[],"entries":[],"mode":2}`
	if _, err := conn.Exec(`INSERT INTO session_state (session_id, state_json) VALUES (?, ?)`, sessionID, stateV1); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExecutionTestStore(t, store)
	got, err := store.GetSessionState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got != stateV1 {
		t.Fatalf("session_state changed during migration: %q", got)
	}
	var tableCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'execution_events'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 1 {
		t.Fatalf("execution_events table count = %d", tableCount)
	}
}

func TestExecutionStoreUsesFullSynchronousWAL(t *testing.T) {
	store := testStore(t)
	var synchronous, foreignKeys, busyTimeout int
	var journalMode string
	if err := store.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if synchronous != 2 || !strings.EqualFold(journalMode, "wal") || foreignKeys != 1 || busyTimeout != 5000 {
		t.Fatalf("pragmas synchronous=%d journal=%q foreign_keys=%d busy_timeout=%d", synchronous, journalMode, foreignKeys, busyTimeout)
	}
}

func TestLatestExecutionEventIDNoEventsAndScope(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/cursor"
	session := createExecutionTestSession(t, store, workspaceID)

	latest, err := store.LatestExecutionEventID(context.Background(), session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if latest != 0 {
		t.Fatalf("empty latest event id = %d, want 0", latest)
	}
	hazards, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 100)
	if err != nil || len(hazards) != 0 {
		t.Fatalf("empty recovery hazards = %#v, %v", hazards, err)
	}
	if _, err := store.LatestExecutionEventID(context.Background(), session.ID, "/workspace/other"); !errors.Is(err, ErrExecutionWorkspaceMismatch) {
		t.Fatalf("cross-workspace latest cursor error = %v", err)
	}
	if _, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, -1, 10); err == nil {
		t.Fatal("negative recovery cursor unexpectedly accepted")
	}
	if _, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 0); err == nil {
		t.Fatal("zero recovery hazard limit unexpectedly accepted")
	}
	if _, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, maxExecutionRecoveryHazards+1); err == nil {
		t.Fatal("oversized recovery hazard limit unexpectedly accepted")
	}
	if _, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, "/workspace/other", 0, 10); !errors.Is(err, ErrExecutionWorkspaceMismatch) {
		t.Fatalf("cross-workspace recovery hazard error = %v", err)
	}

	requested := executionTestEvent(t, session.ID, workspaceID, "cursor-first", execution.EffectReadOnly)
	stored := appendExecutionEvent(t, store, requested)
	latest, err = store.LatestExecutionEventID(context.Background(), session.ID, workspaceID)
	if err != nil || latest != stored.ID {
		t.Fatalf("latest event id = %d, want %d, error %v", latest, stored.ID, err)
	}
}

func TestSessionExecutionEventExistsRequiresExactSessionScope(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/cursor-membership"
	session := createExecutionTestSession(t, store, workspaceID)
	other := createExecutionTestSession(t, store, workspaceID)

	owned := appendExecutionEvent(t, store,
		executionTestEvent(t, session.ID, workspaceID, "owned-cursor", execution.EffectReadOnly))
	foreign := appendExecutionEvent(t, store,
		executionTestEvent(t, other.ID, workspaceID, "foreign-cursor", execution.EffectReadOnly))

	exists, err := store.SessionExecutionEventExists(context.Background(), session.ID, workspaceID, owned.ID)
	if err != nil || !exists {
		t.Fatalf("owned event exists = %t, error %v", exists, err)
	}
	exists, err = store.SessionExecutionEventExists(context.Background(), session.ID, workspaceID, foreign.ID)
	if err != nil || exists {
		t.Fatalf("foreign event exists = %t, error %v", exists, err)
	}
	if _, err := store.SessionExecutionEventExists(context.Background(), session.ID, workspaceID, 0); err == nil {
		t.Fatal("zero execution event id unexpectedly accepted")
	}
	if _, err := store.SessionExecutionEventExists(context.Background(), session.ID, "/workspace/other", owned.ID); !errors.Is(err, ErrExecutionWorkspaceMismatch) {
		t.Fatalf("cross-workspace cursor membership error = %v", err)
	}
}

func TestListExecutionRecoveryHazardsClassifiesCursorStates(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/recovery-cursor"
	session := createExecutionTestSession(t, store, workspaceID)

	completedBefore := appendCompletedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "completed-before", execution.Effectful))
	unknownStarted := appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "outcome-unknown", execution.EffectUnknown))
	outcomeUnknown := unknownStarted
	outcomeUnknown.Type = execution.EventOutcomeUnknown
	outcomeUnknown.ResultReceipt = "backend outcome is unknown"
	outcomeUnknown.ResultSHA256 = execution.HashText(outcomeUnknown.ResultReceipt)
	outcomeUnknown.OccurredAt = unknownStarted.OccurredAt.Add(time.Second)
	appendExecutionEvent(t, store, outcomeUnknown)
	startedHazard := appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "started-hazard", execution.Effectful))

	cursor, err := store.LatestExecutionEventID(context.Background(), session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != startedHazard.ID {
		t.Fatalf("snapshot cursor = %d, want latest started id %d", cursor, startedHazard.ID)
	}
	completedAfter := appendCompletedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "completed-after", execution.Effectful))
	readCompleted := appendCompletedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "read-after", execution.EffectReadOnly))

	states, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 3 {
		t.Fatalf("recovery hazards = %#v, want outcome_unknown, started, and post-cursor completed", states)
	}
	if states[0].Latest.Type != execution.EventOutcomeUnknown || states[1].Latest.Type != execution.EventStarted {
		t.Fatalf("always-blocking hazards were not prioritized: %#v", states)
	}
	if states[2].Identity.ExecutionID != completedAfter.Identity.ExecutionID || states[2].Latest.ID <= cursor {
		t.Fatalf("post-cursor completion missing or misordered: %#v", states[2])
	}
	for _, state := range states {
		if state.Identity.ExecutionID == completedBefore.Identity.ExecutionID {
			t.Fatal("pre-cursor completed execution leaked into recovery hazards")
		}
		if state.Identity.ExecutionID == readCompleted.Identity.ExecutionID {
			t.Fatal("read-only completed execution leaked into recovery hazards")
		}
	}
}

func TestListExecutionReconciliationTargetsScopesTurnAndExcludesCompleted(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconciliation-turn"
	session := createExecutionTestSession(t, store, workspaceID)

	target := executionTestEvent(t, session.ID, workspaceID, "target-started", execution.Effectful)
	target.Identity.TurnID = "turn_target"
	targetStarted := appendStartedExecutionFixture(t, store, target)
	other := executionTestEvent(t, session.ID, workspaceID, "other-started", execution.Effectful)
	other.Identity.TurnID = "turn_other"
	appendStartedExecutionFixture(t, store, other)
	completed := executionTestEvent(t, session.ID, workspaceID, "target-completed", execution.Effectful)
	completed.Identity.TurnID = "turn_target"
	appendCompletedExecutionFixture(t, store, completed)

	states, err := store.ListExecutionReconciliationTargets(context.Background(), session.ID, workspaceID, "turn_target", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Identity.ExecutionID != targetStarted.Identity.ExecutionID || states[0].Latest.Type != execution.EventStarted {
		t.Fatalf("reconciliation targets = %#v", states)
	}
	if _, err := store.ListExecutionReconciliationTargets(context.Background(), session.ID, workspaceID, " turn_target ", 10); err == nil {
		t.Fatal("non-canonical turn id unexpectedly accepted")
	}
	if _, err := store.ListExecutionReconciliationTargets(context.Background(), session.ID, workspaceID, "turn_target", 0); err == nil {
		t.Fatal("unbounded reconciliation target query unexpectedly accepted")
	}
	if _, err := store.ListExecutionReconciliationTargets(context.Background(), session.ID, "/workspace/other", "turn_target", 10); !errors.Is(err, ErrExecutionWorkspaceMismatch) {
		t.Fatalf("cross-workspace reconciliation target error = %v", err)
	}
}

func TestListStandaloneReconciliationPendingExcludesAnsweredTerminals(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconciliation-pending"
	session := createExecutionTestSession(t, store, workspaceID)
	if err := store.SaveSessionState(context.Background(), session.ID, `{"version":2,"goal":null,"execution_cursor":0}`); err != nil {
		t.Fatal(err)
	}

	unknownStarted := appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "pending-unknown", execution.EffectUnknown))
	outcomeUnknown := unknownStarted
	outcomeUnknown.Type = execution.EventOutcomeUnknown
	outcomeUnknown.ResultReceipt = "backend outcome is unknown"
	outcomeUnknown.ResultSHA256 = execution.HashText(outcomeUnknown.ResultReceipt)
	outcomeUnknown.OccurredAt = unknownStarted.OccurredAt.Add(time.Second)
	appendExecutionEvent(t, store, outcomeUnknown)

	appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "pending-started", execution.Effectful))

	appendCompletedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "answered-completed", execution.Effectful))

	failedStarted := appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "answered-failed", execution.Effectful))
	failed := failedStarted
	failed.Type = execution.EventFailed
	failed.ResultReceipt = "backend answered: exit status 7"
	failed.ResultSHA256 = execution.HashText(failed.ResultReceipt)
	failed.OccurredAt = failedStarted.OccurredAt.Add(time.Second)
	appendExecutionEvent(t, store, failed)

	states, err := store.ListStandaloneExecutionReconciliationPending(context.Background(), session.ID, workspaceID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Fatalf("pending reconciliations = %#v, want the unknown and started hazards only", states)
	}
	if states[0].Latest.Type != execution.EventOutcomeUnknown || states[1].Latest.Type != execution.EventStarted {
		t.Fatalf("pending reconciliations misordered or misclassified: %#v", states)
	}
	for _, state := range states {
		if !executionStateCanBeReconciled(state) {
			t.Fatalf("listed state cannot be reconciled: %#v", state)
		}
	}
}

func TestListStandaloneReconciliationPendingPreservesBoundAndDetectsOverflow(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/reconciliation-pending-bound"
	session := createExecutionTestSession(t, store, workspaceID)
	if err := store.SaveSessionState(context.Background(), session.ID, `{"version":2,"goal":null,"execution_cursor":0}`); err != nil {
		t.Fatal(err)
	}

	for index := 0; index < maxExecutionRecoveryHazards; index++ {
		appendStartedExecutionFixture(t, store,
			executionTestEvent(t, session.ID, workspaceID, "bounded-"+strconv.Itoa(index), execution.Effectful))
	}
	states, err := store.ListStandaloneExecutionReconciliationPending(
		context.Background(), session.ID, workspaceID, maxExecutionRecoveryHazards,
	)
	if err != nil || len(states) != maxExecutionRecoveryHazards {
		t.Fatalf("bounded pending reconciliations = %d, error=%v", len(states), err)
	}

	appendStartedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "overflow", execution.Effectful))
	if _, err := store.ListStandaloneExecutionReconciliationPending(
		context.Background(), session.ID, workspaceID, maxExecutionRecoveryHazards,
	); !errors.Is(err, ErrExecutionHazardOverflow) {
		t.Fatalf("pending reconciliation overflow error = %v", err)
	}
}

func TestListExecutionRecoveryHazardsCannotHidePostCursorCompletion(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/recovery-bound"
	session := createExecutionTestSession(t, store, workspaceID)

	for i := 0; i < maxExecutionRecoveryHazards+1; i++ {
		safe := executionTestEvent(t, session.ID, workspaceID, "cursor-safe-"+strconv.Itoa(i), execution.EffectReadOnly)
		appendExecutionEvent(t, store, safe)
	}
	cursor, err := store.LatestExecutionEventID(context.Background(), session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	completed := appendCompletedExecutionFixture(t, store,
		executionTestEvent(t, session.ID, workspaceID, "cursor-later-completed", execution.Effectful))

	states, err := store.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, cursor, maxExecutionRecoveryHazards)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Identity.ExecutionID != completed.Identity.ExecutionID || states[0].Latest.Type != execution.EventCompleted {
		t.Fatalf("post-cursor completion was hidden by older safe rows: %#v", states)
	}
}

func TestExecutionAppendReplayStateAndPrivacy(t *testing.T) {
	store := testStore(t)
	workspaceID := "/private/workspace"
	session := createExecutionTestSession(t, store, workspaceID)
	const secret = "api-token-super-secret"
	hash, err := execution.HashCanonicalArguments(map[string]any{"token": secret, "path": "notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	requested := executionTestEvent(t, session.ID, workspaceID, "privacy", execution.EffectReadOnly)
	requested.ArgumentsSHA256 = hash
	stored, inserted, err := store.AppendExecutionEvent(context.Background(), requested)
	if err != nil || !inserted || stored.ID <= 0 || stored.RecordedAt.IsZero() {
		t.Fatalf("append requested = id %d inserted %v error %v", stored.ID, inserted, err)
	}
	replayed, inserted, err := store.AppendExecutionEvent(context.Background(), requested)
	if err != nil || inserted || replayed.ID != stored.ID {
		t.Fatalf("replay = id %d inserted %v error %v", replayed.ID, inserted, err)
	}
	conflict := requested
	conflict.Detail = "different immutable payload"
	if _, _, err := store.AppendExecutionEvent(context.Background(), conflict); !errors.Is(err, ErrExecutionEventConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}

	approved := requested
	approved.Type = execution.EventApproved
	appendExecutionEvent(t, store, approved)
	started := approved
	started.Type = execution.EventStarted
	started.OccurredAt = requested.OccurredAt.Add(time.Second)
	appendExecutionEvent(t, store, started)
	completed := started
	completed.Type = execution.EventCompleted
	completed.ResultReceipt = "read completed"
	completed.ResultSHA256 = execution.HashText("complete unbounded backend result")
	completed.OccurredAt = started.OccurredAt.Add(time.Second)
	appendExecutionEvent(t, store, completed)

	state, err := store.GetExecutionState(context.Background(), session.ID, workspaceID, requested.Identity.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Terminal() || state.Latest.Type != execution.EventCompleted || state.EventCount != 4 {
		t.Fatalf("unexpected state: %#v", state)
	}
	events, err := store.ListExecutionEvents(context.Background(), session.ID, workspaceID, requested.Identity.ExecutionID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Type != execution.EventRequested || events[3].Type != execution.EventCompleted {
		t.Fatalf("event order = %#v", events)
	}

	var retained string
	if err := store.db.QueryRow(`
		SELECT workspace_id || run_id || turn_id || execution_id || idempotency_key ||
		       provider_call_id || canonical_call_id || tool_name || arguments_sha256 ||
		       result_sha256 || result_receipt || detail
		  FROM execution_events WHERE id = ?`, stored.ID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(retained, secret) {
		t.Fatal("raw argument secret was retained in the execution ledger")
	}
	rows, err := store.db.Query(`PRAGMA table_info(execution_events)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(name, "argument") && name != "arguments_sha256" {
			t.Fatalf("migration retained raw argument-shaped column %q", name)
		}
	}
}

func TestExecutionApprovalTransitionsAndEffectiveArgumentHash(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/transitions"
	session := createExecutionTestSession(t, store, workspaceID)
	requested := executionTestEvent(t, session.ID, workspaceID, "effect", execution.Effectful)
	appendExecutionEvent(t, store, requested)

	started := requested
	started.Type = execution.EventStarted
	if _, _, err := store.AppendExecutionEvent(context.Background(), started); !errors.Is(err, ErrIllegalExecutionTransition) {
		t.Fatalf("unapproved effect start error = %v", err)
	}
	effectiveHash, err := execution.HashCanonicalArguments(map[string]any{"path": "normalized.txt"})
	if err != nil {
		t.Fatal(err)
	}
	approvalRequested := requested
	approvalRequested.Type = execution.EventApprovalRequested
	approvalRequested.Approval = execution.ApprovalRequested
	approvalRequested.ArgumentsSHA256 = effectiveHash
	appendExecutionEvent(t, store, approvalRequested)
	approved := approvalRequested
	approved.Type = execution.EventApproved
	approved.Approval = execution.ApprovalOnce
	appendExecutionEvent(t, store, approved)

	started = approved
	started.Type = execution.EventStarted
	started.Approval = execution.ApprovalNotApplicable
	started.ArgumentsSHA256 = requested.ArgumentsSHA256
	if _, _, err := store.AppendExecutionEvent(context.Background(), started); !errors.Is(err, ErrExecutionEventConflict) {
		t.Fatalf("changed effective argument hash error = %v", err)
	}
	started.ArgumentsSHA256 = effectiveHash
	appendExecutionEvent(t, store, started)

	failed := started
	failed.Type = execution.EventFailed
	failed.ResultReceipt = "backend answered: exit status 7"
	failed.ResultSHA256 = execution.HashText(failed.ResultReceipt)
	appendExecutionEvent(t, store, failed)
	unknown := started
	unknown.Type = execution.EventOutcomeUnknown
	unknown.ResultReceipt = "backend result not durable"
	unknown.ResultSHA256 = execution.HashText(unknown.ResultReceipt)
	if _, _, err := store.AppendExecutionEvent(context.Background(), unknown); !errors.Is(err, ErrIllegalExecutionTransition) {
		t.Fatalf("terminal after answered failure error = %v", err)
	}
	completed := started
	completed.Type = execution.EventCompleted
	if _, _, err := store.AppendExecutionEvent(context.Background(), completed); !errors.Is(err, ErrIllegalExecutionTransition) {
		t.Fatalf("second terminal error = %v", err)
	}

	// An unanswered dispatch still terminates as outcome_unknown.
	unansweredRequested := executionTestEvent(t, session.ID, workspaceID, "effect-unanswered", execution.Effectful)
	appendExecutionEvent(t, store, unansweredRequested)
	unansweredApprovalRequested := unansweredRequested
	unansweredApprovalRequested.Type = execution.EventApprovalRequested
	unansweredApprovalRequested.Approval = execution.ApprovalRequested
	appendExecutionEvent(t, store, unansweredApprovalRequested)
	unansweredApproved := unansweredApprovalRequested
	unansweredApproved.Type = execution.EventApproved
	unansweredApproved.Approval = execution.ApprovalOnce
	appendExecutionEvent(t, store, unansweredApproved)
	unansweredStarted := unansweredApproved
	unansweredStarted.Type = execution.EventStarted
	unansweredStarted.Approval = execution.ApprovalNotApplicable
	appendExecutionEvent(t, store, unansweredStarted)
	unansweredUnknown := unansweredStarted
	unansweredUnknown.Type = execution.EventOutcomeUnknown
	unansweredUnknown.ResultReceipt = "backend result not durable"
	unansweredUnknown.ResultSHA256 = execution.HashText(unansweredUnknown.ResultReceipt)
	appendExecutionEvent(t, store, unansweredUnknown)
}

func TestExecutionRejectsIdentityScopeAndTransitionConflicts(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/a"
	session := createExecutionTestSession(t, store, workspaceID)
	requested := executionTestEvent(t, session.ID, workspaceID, "identity", execution.EffectReadOnly)

	started := requested
	started.Type = execution.EventStarted
	if _, _, err := store.AppendExecutionEvent(context.Background(), started); !errors.Is(err, ErrIllegalExecutionTransition) {
		t.Fatalf("non-request first error = %v", err)
	}
	appendExecutionEvent(t, store, requested)

	changed := requested
	changed.Identity.ToolName = "write"
	if _, _, err := store.AppendExecutionEvent(context.Background(), changed); !errors.Is(err, ErrExecutionIdentityConflict) {
		t.Fatalf("identity conflict error = %v", err)
	}
	sameIdempotency := executionTestEvent(t, session.ID, workspaceID, "other", execution.EffectReadOnly)
	sameIdempotency.Identity.IdempotencyKey = requested.Identity.IdempotencyKey
	if _, _, err := store.AppendExecutionEvent(context.Background(), sameIdempotency); !errors.Is(err, ErrExecutionIdentityConflict) {
		t.Fatalf("idempotency conflict error = %v", err)
	}
	positionConflict := executionTestEvent(t, session.ID, workspaceID, "position", execution.EffectReadOnly)
	positionConflict.Identity.RunID = requested.Identity.RunID
	positionConflict.Identity.TurnID = requested.Identity.TurnID
	positionConflict.Identity.Iteration = requested.Identity.Iteration
	positionConflict.Identity.Ordinal = requested.Identity.Ordinal
	if _, _, err := store.AppendExecutionEvent(context.Background(), positionConflict); !errors.Is(err, ErrExecutionIdentityConflict) {
		t.Fatalf("position conflict error = %v", err)
	}
	wrongWorkspace := executionTestEvent(t, session.ID, "/workspace/b", "scope", execution.EffectReadOnly)
	if _, _, err := store.AppendExecutionEvent(context.Background(), wrongWorkspace); !errors.Is(err, ErrExecutionWorkspaceMismatch) {
		t.Fatalf("workspace conflict error = %v", err)
	}
}

func TestExecutionBoundsImmutabilityAndSessionCascade(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/bounds"
	session := createExecutionTestSession(t, store, workspaceID)
	requested := executionTestEvent(t, session.ID, workspaceID, "bounds", execution.EffectReadOnly)
	requested.Detail = strings.Repeat("d", execution.MaxDetailBytes+1)
	if _, _, err := store.AppendExecutionEvent(context.Background(), requested); err == nil {
		t.Fatal("oversized detail unexpectedly appended")
	}
	requested.Detail = ""
	stored := appendExecutionEvent(t, store, requested)
	if _, err := store.db.Exec(`UPDATE execution_events SET detail = 'changed' WHERE id = ?`, stored.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("immutable update error = %v", err)
	}
	if _, err := store.db.Exec(`DELETE FROM execution_events WHERE id = ?`, stored.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("immutable delete error = %v", err)
	}

	oversized := executionTestEvent(t, session.ID, workspaceID, "raw-bounds", execution.EffectReadOnly)
	oversized.ResultReceipt = strings.Repeat("r", execution.MaxResultReceiptBytes+1)
	if err := insertExecutionEventRaw(store, oversized); err == nil {
		t.Fatal("database accepted oversized raw receipt")
	}
	invalidHash := executionTestEvent(t, session.ID, workspaceID, "raw-hash", execution.EffectReadOnly)
	invalidHash.ArgumentsSHA256 = "not-a-sha256"
	if err := insertExecutionEventRaw(store, invalidHash); err == nil {
		t.Fatal("database accepted malformed argument hash")
	}

	if err := store.DeleteSession(context.Background(), session.ID); err != nil {
		t.Fatalf("session cascade was blocked by append-only trigger: %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM execution_events WHERE session_id = ?`, session.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("session cascade retained %d execution events", count)
	}
}

func TestListUnresolvedExecutionsIsScopedAndBounded(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/unresolved"
	session := createExecutionTestSession(t, store, workspaceID)

	first := executionTestEvent(t, session.ID, workspaceID, "unresolved-a", execution.EffectReadOnly)
	appendExecutionEvent(t, store, first)
	second := executionTestEvent(t, session.ID, workspaceID, "unresolved-b", execution.EffectReadOnly)
	appendExecutionEvent(t, store, second)
	started := second
	started.Type = execution.EventStarted
	appendExecutionEvent(t, store, started)
	terminal := executionTestEvent(t, session.ID, workspaceID, "terminal", execution.EffectReadOnly)
	appendExecutionEvent(t, store, terminal)
	terminalStarted := terminal
	terminalStarted.Type = execution.EventStarted
	appendExecutionEvent(t, store, terminalStarted)
	terminalCompleted := terminalStarted
	terminalCompleted.Type = execution.EventCompleted
	appendExecutionEvent(t, store, terminalCompleted)

	states, err := store.ListUnresolvedExecutions(context.Background(), session.ID, workspaceID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].Latest.Type != execution.EventRequested || states[1].Latest.Type != execution.EventStarted {
		t.Fatalf("unresolved states = %#v", states)
	}
	limited, err := store.ListUnresolvedExecutions(context.Background(), session.ID, workspaceID, 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("bounded unresolved = %d, %v", len(limited), err)
	}
	if _, err := store.ListUnresolvedExecutions(context.Background(), session.ID, workspaceID, 0); err == nil {
		t.Fatal("zero unresolved limit unexpectedly accepted")
	}
	if _, err := store.ListUnresolvedExecutions(context.Background(), session.ID, workspaceID, maxUnresolvedList+1); err == nil {
		t.Fatal("oversized unresolved limit unexpectedly accepted")
	}
	if _, err := store.ListUnresolvedExecutions(context.Background(), session.ID, "/workspace/other", 10); !errors.Is(err, ErrExecutionWorkspaceMismatch) {
		t.Fatalf("cross-workspace unresolved error = %v", err)
	}
}

func TestListUnresolvedExecutionsPrioritizesHazardsWithinBound(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/hazard-priority"
	session := createExecutionTestSession(t, store, workspaceID)

	// These safe, older rows would fill the bounded result if it were ordered
	// only by insertion ID, hiding the later execution that may have taken effect.
	for i := 0; i < maxUnresolvedList+1; i++ {
		safe := executionTestEvent(t, session.ID, workspaceID, "safe-"+strconv.Itoa(i), execution.EffectReadOnly)
		appendExecutionEvent(t, store, safe)
	}

	hazard := executionTestEvent(t, session.ID, workspaceID, "later-hazard", execution.EffectUnknown)
	appendExecutionEvent(t, store, hazard)
	approved := hazard
	approved.Type = execution.EventApproved
	approved.Approval = execution.ApprovalEmbedding
	appendExecutionEvent(t, store, approved)
	started := approved
	started.Type = execution.EventStarted
	started.Approval = execution.ApprovalNotApplicable
	appendExecutionEvent(t, store, started)

	states, err := store.ListUnresolvedExecutions(context.Background(), session.ID, workspaceID, maxUnresolvedList)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != maxUnresolvedList {
		t.Fatalf("unresolved count = %d, want bound %d", len(states), maxUnresolvedList)
	}
	if states[0].Identity.ExecutionID != hazard.Identity.ExecutionID ||
		states[0].Latest.Type != execution.EventStarted ||
		states[0].Identity.EffectClass == execution.EffectReadOnly {
		t.Fatalf("hazard was not prioritized within bounded result: first=%#v", states[0])
	}
}

func TestListUnresolvedExecutionsPrioritizesOutcomeUnknownWithinBound(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/unknown-priority"
	session := createExecutionTestSession(t, store, workspaceID)

	for i := 0; i < maxUnresolvedList+1; i++ {
		safe := executionTestEvent(t, session.ID, workspaceID, "unknown-safe-"+strconv.Itoa(i), execution.EffectReadOnly)
		appendExecutionEvent(t, store, safe)
	}

	unknown := executionTestEvent(t, session.ID, workspaceID, "durable-unknown", execution.EffectUnknown)
	appendExecutionEvent(t, store, unknown)
	approved := unknown
	approved.Type = execution.EventApproved
	approved.Approval = execution.ApprovalEmbedding
	appendExecutionEvent(t, store, approved)
	started := approved
	started.Type = execution.EventStarted
	started.Approval = execution.ApprovalNotApplicable
	appendExecutionEvent(t, store, started)
	outcomeUnknown := started
	outcomeUnknown.Type = execution.EventOutcomeUnknown
	outcomeUnknown.ResultReceipt = "backend may have taken effect"
	outcomeUnknown.ResultSHA256 = execution.HashText(outcomeUnknown.ResultReceipt)
	appendExecutionEvent(t, store, outcomeUnknown)

	states, err := store.ListUnresolvedExecutions(context.Background(), session.ID, workspaceID, maxUnresolvedList)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != maxUnresolvedList {
		t.Fatalf("unresolved count = %d, want bound %d", len(states), maxUnresolvedList)
	}
	if states[0].Identity.ExecutionID != unknown.Identity.ExecutionID ||
		states[0].Latest.Type != execution.EventOutcomeUnknown ||
		!states[0].Terminal() {
		t.Fatalf("durable outcome_unknown was not prioritized within bounded result: first=%#v", states[0])
	}
}

func TestConcurrentExecutionReplayInsertsExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	first, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExecutionTestStore(t, first)
	second, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExecutionTestStore(t, second)
	workspaceID := "/workspace/concurrent"
	session := createExecutionTestSession(t, first, workspaceID)
	event := executionTestEvent(t, session.ID, workspaceID, "concurrent", execution.EffectReadOnly)

	const workers = 16
	start := make(chan struct{})
	results := make(chan bool, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		store := first
		if i%2 == 1 {
			store = second
		}
		go func() {
			defer wg.Done()
			<-start
			_, inserted, appendErr := store.AppendExecutionEvent(context.Background(), event)
			results <- inserted
			errs <- appendErr
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	insertedCount := 0
	for inserted := range results {
		if inserted {
			insertedCount++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent replay error: %v", err)
		}
	}
	if insertedCount != 1 {
		t.Fatalf("concurrent inserted count = %d", insertedCount)
	}
}

func TestConcurrentExecutionTerminalRaceKeepsOneReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terminal-race.db")
	first, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExecutionTestStore(t, first)
	second, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExecutionTestStore(t, second)
	workspaceID := "/workspace/terminal-race"
	session := createExecutionTestSession(t, first, workspaceID)
	requested := executionTestEvent(t, session.ID, workspaceID, "terminal-race", execution.Effectful)
	appendExecutionEvent(t, first, requested)
	approved := requested
	approved.Type = execution.EventApproved
	approved.Approval = execution.ApprovalEmbedding
	appendExecutionEvent(t, first, approved)
	started := approved
	started.Type = execution.EventStarted
	started.Approval = execution.ApprovalNotApplicable
	appendExecutionEvent(t, first, started)
	completed := started
	completed.Type = execution.EventCompleted
	completed.ResultSHA256 = execution.HashText("completed")
	unknown := started
	unknown.Type = execution.EventOutcomeUnknown
	unknown.ResultSHA256 = execution.HashText("unknown")

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, attempt := range []struct {
		store *Store
		event execution.Event
	}{{first, completed}, {second, unknown}} {
		_ = i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, appendErr := attempt.store.AppendExecutionEvent(context.Background(), attempt.event)
			errs <- appendErr
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	successes, failures := 0, 0
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrIllegalExecutionTransition) {
			failures++
		} else {
			t.Fatalf("terminal race error = %v", err)
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("terminal race successes=%d failures=%d", successes, failures)
	}
	state, err := first.GetExecutionState(context.Background(), session.ID, workspaceID, requested.Identity.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Terminal() || state.EventCount != 4 {
		t.Fatalf("terminal race state = %#v", state)
	}
}

func insertExecutionEventRaw(store *Store, event execution.Event) error {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = executionTestTime
	}
	_, err := store.db.Exec(`
		INSERT INTO execution_events (
			session_id, workspace_id, run_id, turn_id, execution_id,
			idempotency_key, provider_call_id, canonical_call_id, iteration,
			ordinal, tool_name, kind, effect_class, event_type, approval,
			arguments_sha256, result_sha256, result_receipt, detail, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.Identity.SessionID, event.Identity.WorkspaceID,
		event.Identity.RunID, event.Identity.TurnID, event.Identity.ExecutionID,
		event.Identity.IdempotencyKey, event.Identity.ProviderCallID,
		event.Identity.CanonicalCallID, event.Identity.Iteration,
		event.Identity.Ordinal, event.Identity.ToolName, event.Identity.Kind,
		event.Identity.EffectClass, event.Type, event.Approval,
		event.ArgumentsSHA256, event.ResultSHA256, event.ResultReceipt,
		event.Detail, event.OccurredAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func TestExecutionMigrationObjectsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	for attempt := 0; attempt < 3; attempt++ {
		store, err := OpenPath(path)
		if err != nil {
			t.Fatalf("open attempt %d: %v", attempt, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExecutionTestStore(t, store)
	for _, object := range []struct {
		kind string
		name string
	}{
		{"table", "execution_events"},
		{"trigger", "execution_events_workspace_guard"},
		{"trigger", "execution_events_no_update"},
		{"trigger", "execution_events_no_delete"},
		{"index", "ux_execution_events_phase"},
		{"index", "ux_execution_events_terminal"},
		{"index", "ux_control_items_execution_reconciliation_target"},
		{"trigger", "control_items_execution_reconciliation_target_guard"},
		{"table", "reconciliation_groups"},
		{"table", "reconciliation_group_members"},
		{"table", "reconciliation_group_resolutions"},
		{"index", "idx_reconciliation_groups_session"},
		{"index", "idx_reconciliation_group_members_group"},
		{"trigger", "reconciliation_groups_scope_guard"},
		{"trigger", "reconciliation_group_members_target_guard"},
		{"trigger", "reconciliation_group_resolutions_scope_guard"},
		{"trigger", "reconciliation_groups_no_update"},
		{"trigger", "reconciliation_groups_no_delete"},
		{"trigger", "reconciliation_group_members_no_update"},
		{"trigger", "reconciliation_group_members_no_delete"},
		{"trigger", "reconciliation_group_resolutions_no_update"},
		{"trigger", "reconciliation_group_resolutions_no_delete"},
	} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, object.kind, object.name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s %s count = %d", object.kind, object.name, count)
		}
	}
	revisionFound, err := tableColumnExists(store.db, "session_state", "revision")
	if err != nil || !revisionFound {
		t.Fatalf("session_state revision found=%v error=%v", revisionFound, err)
	}
}

func TestWorkspaceApprovalPromptStatsAggregatesAcrossSessions(t *testing.T) {
	store := testStore(t)
	first := createExecutionTestSession(t, store, "/workspace/audit")
	second := createExecutionTestSession(t, store, "/workspace/audit")
	other := createExecutionTestSession(t, store, "/workspace/elsewhere")

	prompt := func(session Session, suffix, tool, detail string, at time.Time) {
		t.Helper()
		requested := executionTestEvent(t, session.ID, session.WorkspaceID, suffix, execution.Effectful)
		requested.Identity.ToolName = tool
		requested.OccurredAt = at
		appendExecutionEvent(t, store, requested)
		asked := requested
		asked.Type = execution.EventApprovalRequested
		asked.Approval = execution.ApprovalRequested
		asked.Detail = detail
		asked.OccurredAt = at.Add(time.Second)
		appendExecutionEvent(t, store, asked)
	}

	taskDetail := "interactive approval requested: executable outside the host catalog · grant prefix: task deploy"
	prompt(first, "a1", "bash", taskDetail, executionTestTime)
	prompt(first, "a2", "bash", taskDetail, executionTestTime.Add(time.Minute))
	prompt(second, "a3", "bash", taskDetail, executionTestTime.Add(2*time.Minute))
	prompt(second, "a4", "remove", "interactive approval requested", executionTestTime.Add(3*time.Minute))
	prompt(other, "a5", "bash", taskDetail, executionTestTime)

	stats, err := store.WorkspaceApprovalPromptStats(context.Background(), "/workspace/audit", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("stats = %d rows, want 2 (same-workspace pairs only): %+v", len(stats), stats)
	}
	if stats[0].ToolName != "bash" || stats[0].Detail != taskDetail || stats[0].Count != 3 {
		t.Fatalf("top row = %+v, want the bash prefix pair counted 3 times across both sessions", stats[0])
	}
	if stats[1].ToolName != "remove" || stats[1].Count != 1 {
		t.Fatalf("second row = %+v, want the single remove prompt", stats[1])
	}
	if !strings.HasPrefix(stats[0].LastAt, "2026-07-11T12:32") {
		t.Fatalf("LastAt = %q, want the most recent same-workspace occurrence", stats[0].LastAt)
	}

	if _, err := store.WorkspaceApprovalPromptStats(context.Background(), "  ", 10); err == nil {
		t.Fatal("blank workspace id was accepted")
	}
}
