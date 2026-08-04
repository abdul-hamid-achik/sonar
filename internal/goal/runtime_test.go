package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func testClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)}
}

func testSpec(budget BudgetLimits) Spec {
	return Spec{
		ID:        "goal_test",
		SessionID: 42,
		Objective: "Ship the goal runtime safely",
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "tests", Description: "All focused tests pass"},
			{ID: "recovery", Description: "Unknown outcomes block continuation"},
		},
		Budget: budget,
	}
}

func testRuntime(t *testing.T, budget BudgetLimits) (*Runtime, *fakeClock) {
	t.Helper()
	clock := testClock()
	runtime, err := New(testSpec(budget), WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	return runtime, clock
}

func mustSnapshot(t *testing.T, runtime *Runtime) Snapshot {
	t.Helper()
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func productiveTurn(id string, tokens int64) TurnReport {
	return TurnReport{TurnID: id, EvalTokens: tokens, Productive: true, Summary: "completed a verified unit of work"}
}

func recordAdmittedTurn(t *testing.T, runtime *Runtime, kind TurnAdmissionKind, report TurnReport) {
	t.Helper()
	if _, err := runtime.BeginTurn(context.Background(), report.TurnID, kind); err != nil {
		t.Fatalf("admit %s turn %s: %v", kind, report.TurnID, err)
	}
	if err := runtime.RecordTurn(context.Background(), report); err != nil {
		t.Fatalf("record %s turn %s: %v", kind, report.TurnID, err)
	}
}

func completionRequest() CompletionRequest {
	return CompletionRequest{
		ValidatedBy: "sonar-host",
		Summary:     "every acceptance criterion was verified",
		Results: []AcceptanceResult{
			{CriterionID: "tests", Satisfied: true, Evidence: "go test ./internal/goal passed"},
			{CriterionID: "recovery", Satisfied: true, Evidence: "outcome-unknown recovery test passed"},
		},
	}
}

func testReconciliationReceipt() *ReconciliationReceipt {
	return &ReconciliationReceipt{
		Version:             ReconciliationReceiptVersion,
		GroupItemID:         "ctrl_turn_group",
		FinalItemID:         "ctrl_execution_final",
		FinalResolutionID:   "ctrlres_execution_final",
		ResolutionSetSHA256: strings.Repeat("a", 64),
		TargetCount:         1,
	}
}

func TestNewValidatesDefinitionAndReturnsIsolatedSnapshots(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{})
	first := mustSnapshot(t, runtime)
	if first.Version != SnapshotVersion || first.State != StateActive || first.SessionID != 42 {
		t.Fatalf("initial snapshot = %#v", first)
	}
	if !strings.HasPrefix(first.ID, "goal_") && first.ID != "goal_test" {
		t.Fatalf("unexpected goal id %q", first.ID)
	}

	first.AcceptanceCriteria[0].Description = "mutated"
	second := mustSnapshot(t, runtime)
	if second.AcceptanceCriteria[0].Description == "mutated" {
		t.Fatal("snapshot criteria alias runtime state")
	}

	tests := []struct {
		name string
		edit func(*Spec)
	}{
		{name: "session", edit: func(spec *Spec) { spec.SessionID = 0 }},
		{name: "objective", edit: func(spec *Spec) { spec.Objective = " " }},
		{name: "criteria", edit: func(spec *Spec) { spec.AcceptanceCriteria = nil }},
		{name: "duplicate criterion", edit: func(spec *Spec) { spec.AcceptanceCriteria[1].ID = "tests" }},
		{name: "negative budget", edit: func(spec *Spec) { spec.Budget.MaxEvalTokens = -1 }},
		{name: "partial cortex", edit: func(spec *Spec) { spec.Cortex.TaskID = "task_1" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := testSpec(BudgetLimits{})
			test.edit(&spec)
			if _, err := New(spec); !errors.Is(err, ErrInvalid) {
				t.Fatalf("New() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestNewGeneratesDistinctGoalIDs(t *testing.T) {
	spec := testSpec(BudgetLimits{})
	spec.ID = ""
	first, err := New(spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(spec)
	if err != nil {
		t.Fatal(err)
	}
	firstSnapshot := mustSnapshot(t, first)
	secondSnapshot := mustSnapshot(t, second)
	if !strings.HasPrefix(firstSnapshot.ID, "goal_") || firstSnapshot.ID == secondSnapshot.ID {
		t.Fatalf("generated goal IDs = %q, %q", firstSnapshot.ID, secondSnapshot.ID)
	}
}

func TestEveryTurnKindRequiresAndSettlesExactDurableAdmission(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{MaxContinuationTurns: 3})
	ctx := context.Background()
	initial := productiveTurn("turn_initial", 5)

	if err := runtime.RecordTurn(ctx, initial); !errors.Is(err, ErrTurnPending) {
		t.Fatalf("unadmitted initial receipt error = %v", err)
	}
	if _, err := runtime.BeginTurn(ctx, initial.TurnID, TurnAdmissionKind("forged")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid admission kind error = %v", err)
	}
	admission, err := runtime.BeginTurn(ctx, initial.TurnID, AdmissionInitial)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Kind != AdmissionInitial || admission.Ordinal != 0 {
		t.Fatalf("initial admission = %#v", admission)
	}
	if err := runtime.RecordTurn(ctx, productiveTurn("wrong_initial", 1)); !errors.Is(err, ErrTurnPending) {
		t.Fatalf("mismatched initial receipt error = %v", err)
	}
	if snapshot := mustSnapshot(t, runtime); snapshot.PendingContinuation == nil || snapshot.PendingContinuation.TurnID != initial.TurnID {
		t.Fatalf("mismatch consumed initial admission: %#v", snapshot.PendingContinuation)
	}
	if err := runtime.RecordTurn(ctx, initial); err != nil {
		t.Fatal(err)
	}

	manual := productiveTurn("turn_manual", 7)
	admission, err = runtime.BeginTurn(ctx, manual.TurnID, AdmissionManual)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Kind != AdmissionManual || admission.Ordinal != 0 {
		t.Fatalf("manual admission = %#v", admission)
	}
	if usage := mustSnapshot(t, runtime).Usage.ContinuationTurns; usage != 0 {
		t.Fatalf("manual admission consumed %d continuation turns", usage)
	}
	if err := runtime.RecordTurn(ctx, manual); err != nil {
		t.Fatal(err)
	}

	automatic := productiveTurn("turn_automatic", 11)
	admission, err = runtime.BeginContinuation(ctx, automatic.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Kind != AdmissionAutomatic || admission.Ordinal != 1 {
		t.Fatalf("automatic admission = %#v", admission)
	}
	if err := runtime.RecordTurn(ctx, automatic); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordTurn(ctx, automatic); err != nil {
		t.Fatalf("exact settled receipt replay failed: %v", err)
	}
	snapshot := mustSnapshot(t, runtime)
	if snapshot.PendingContinuation != nil || snapshot.Usage.ContinuationTurns != 1 || snapshot.Usage.EvalTokens != 23 {
		t.Fatalf("settled admission snapshot = %#v", snapshot)
	}
	if _, err := runtime.BeginTurn(ctx, "late_initial", AdmissionInitial); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("second initial admission error = %v", err)
	}

	fresh, _ := testRuntime(t, BudgetLimits{})
	if _, err := fresh.BeginTurn(ctx, "manual_first", AdmissionManual); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("manual first-turn admission error = %v", err)
	}
}

func TestInitialAndManualAdmissionRecoveryNeverRedispatchesOrChangesContinuationUsage(t *testing.T) {
	for _, test := range []struct {
		name  string
		kind  TurnAdmissionKind
		setup func(*testing.T, *Runtime)
	}{
		{name: "initial", kind: AdmissionInitial},
		{name: "manual", kind: AdmissionManual, setup: func(t *testing.T, runtime *Runtime) {
			recordAdmittedTurn(t, runtime, AdmissionInitial, productiveTurn("prior", 1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := testRuntime(t, BudgetLimits{MaxContinuationTurns: 2})
			if test.setup != nil {
				test.setup(t, runtime)
			}
			turnID := "orphaned_" + test.name
			if _, err := runtime.BeginTurn(context.Background(), turnID, test.kind); err != nil {
				t.Fatal(err)
			}
			if err := runtime.RecoverPendingContinuation(context.Background(), PendingRecovery{
				TurnID: turnID, Kind: PendingCancelledBeforeDispatch,
				Reason: "host proved dispatch never began", Evidence: "provider command was never created",
			}); err != nil {
				t.Fatal(err)
			}
			snapshot := mustSnapshot(t, runtime)
			if snapshot.State != StatePaused || snapshot.PendingContinuation != nil || snapshot.Usage.ContinuationTurns != 0 {
				t.Fatalf("%s recovery = %#v", test.kind, snapshot)
			}
			if snapshot.LastPendingRecovery == nil || snapshot.LastPendingRecovery.Permit.Kind != test.kind {
				t.Fatalf("%s recovery receipt = %#v", test.kind, snapshot.LastPendingRecovery)
			}
		})
	}
}

func TestRestoreMigratesOnlyProvableLegacyAutomaticPermits(t *testing.T) {
	runtime, clock := testRuntime(t, BudgetLimits{MaxContinuationTurns: 2})
	recordAdmittedTurn(t, runtime, AdmissionInitial, productiveTurn("initial", 1))
	if _, err := runtime.BeginContinuation(context.Background(), "legacy_auto"); err != nil {
		t.Fatal(err)
	}
	legacy := mustSnapshot(t, runtime)
	legacy.Version = LegacySnapshotVersion
	legacy.PendingContinuation.Kind = ""
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(decoded, WithClock(clock))
	if err != nil {
		t.Fatalf("restore legacy automatic permit: %v", err)
	}
	if pending := mustSnapshot(t, restored).PendingContinuation; pending == nil || pending.Kind != AdmissionAutomatic || pending.Ordinal != 1 {
		t.Fatalf("migrated legacy permit = %#v", pending)
	}

	initial, initialClock := testRuntime(t, BudgetLimits{})
	if _, err := initial.BeginTurn(context.Background(), "stripped_initial", AdmissionInitial); err != nil {
		t.Fatal(err)
	}
	forged := mustSnapshot(t, initial)
	forged.PendingContinuation.Kind = ""
	if _, err := Restore(forged, WithClock(initialClock)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero-ordinal missing kind error = %v", err)
	}
}

func TestConcurrentTurnAdmissionHasExactlyOneWinner(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{})
	type result struct {
		turnID string
		err    error
	}
	results := make(chan result, 16)
	var wait sync.WaitGroup
	for worker := 0; worker < cap(results); worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			turnID := fmt.Sprintf("concurrent_%d", worker)
			_, err := runtime.BeginTurn(context.Background(), turnID, AdmissionInitial)
			results <- result{turnID: turnID, err: err}
		}(worker)
	}
	wait.Wait()
	close(results)

	winner := ""
	for result := range results {
		if result.err == nil {
			if winner != "" {
				t.Fatalf("multiple admissions won: %q and %q", winner, result.turnID)
			}
			winner = result.turnID
			continue
		}
		if !errors.Is(result.err, ErrTurnPending) {
			t.Fatalf("losing admission %q error = %v", result.turnID, result.err)
		}
	}
	if winner == "" {
		t.Fatal("no concurrent admission succeeded")
	}
	snapshot := mustSnapshot(t, runtime)
	if snapshot.PendingContinuation == nil || snapshot.PendingContinuation.TurnID != winner || snapshot.PendingContinuation.Kind != AdmissionInitial {
		t.Fatalf("winning admission = %#v, winner %q", snapshot.PendingContinuation, winner)
	}
	if snapshot.Usage.ContinuationTurns != 0 {
		t.Fatalf("concurrent initial admission consumed continuation usage: %#v", snapshot.Usage)
	}
}

func TestStaleReceiptCannotSettleOrRedispatchAnotherAdmission(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{})
	initial := productiveTurn("settled_initial", 3)
	recordAdmittedTurn(t, runtime, AdmissionInitial, initial)
	if _, err := runtime.BeginTurn(context.Background(), "pending_manual", AdmissionManual); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordTurn(context.Background(), initial); !errors.Is(err, ErrTurnPending) {
		t.Fatalf("stale settled receipt error = %v", err)
	}
	if pending := mustSnapshot(t, runtime).PendingContinuation; pending == nil || pending.TurnID != "pending_manual" {
		t.Fatalf("stale receipt consumed newer admission: %#v", pending)
	}
	if err := runtime.RecoverPendingContinuation(context.Background(), PendingRecovery{
		TurnID: "pending_manual", Kind: PendingCancelledBeforeDispatch,
		Reason: "provider command was never returned", Evidence: "host retained the undispatched command boundary",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Resume(context.Background(), "continue with a new turn identity"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.BeginTurn(context.Background(), initial.TurnID, AdmissionManual); !errors.Is(err, ErrTurnConflict) {
		t.Fatalf("settled turn id reuse error = %v", err)
	}
	if _, err := runtime.BeginTurn(context.Background(), "pending_manual", AdmissionManual); !errors.Is(err, ErrTurnConflict) {
		t.Fatalf("recovered turn id redispatch error = %v", err)
	}
}

func TestContinuationPermitExactBudgetBoundaryAndIdempotency(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{MaxContinuationTurns: 1})
	if decision, err := runtime.CanAutoContinue(context.Background()); err != nil || decision.Reason != ContinuationNoTurnReceipt {
		t.Fatalf("initial continuation decision = %#v, %v", decision, err)
	}
	recordAdmittedTurn(t, runtime, AdmissionInitial, productiveTurn("initial", 10))
	if decision, _ := runtime.CanAutoContinue(context.Background()); !decision.Allowed {
		t.Fatalf("productive turn did not enable continuation: %#v", decision)
	}

	permit, err := runtime.BeginContinuation(context.Background(), "continuation-1")
	if err != nil {
		t.Fatal(err)
	}
	if permit.Ordinal != 1 {
		t.Fatalf("permit ordinal = %d", permit.Ordinal)
	}
	// Consuming the final unit marks the budget exhausted, but the in-flight
	// permit is the higher-priority safety fact.
	if decision, _ := runtime.CanAutoContinue(context.Background()); decision.Reason != ContinuationTurnPending {
		t.Fatalf("exact-boundary pending decision = %#v, want turn_pending", decision)
	}
	if _, err := runtime.BeginContinuation(context.Background(), "continuation-1"); !errors.Is(err, ErrTurnPending) {
		t.Fatalf("duplicate begin did not fail closed: %v", err)
	}
	if _, err := runtime.BeginContinuation(context.Background(), "other"); !errors.Is(err, ErrTurnPending) {
		t.Fatalf("second pending permit error = %v", err)
	}
	if err := runtime.RecordTurn(context.Background(), productiveTurn("wrong", 1)); !errors.Is(err, ErrTurnPending) {
		t.Fatalf("mismatched receipt error = %v", err)
	}
	continuation := productiveTurn("continuation-1", 20)
	if err := runtime.RecordTurn(context.Background(), continuation); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordTurn(context.Background(), continuation); err != nil {
		t.Fatalf("duplicate receipt was not idempotent: %v", err)
	}
	snapshot := mustSnapshot(t, runtime)
	if snapshot.State != StateExhausted || snapshot.Completion != nil || snapshot.PendingContinuation != nil {
		t.Fatalf("settled boundary snapshot = %#v", snapshot)
	}
	if snapshot.Usage.ContinuationTurns != 1 || snapshot.Usage.EvalTokens != 30 {
		t.Fatalf("usage = %#v", snapshot.Usage)
	}
	if decision, _ := runtime.CanAutoContinue(context.Background()); decision.Reason != ContinuationBudget {
		t.Fatalf("post-settlement continuation decision = %#v", decision)
	}
}

func TestRecoverPendingContinuationNeverRedispatchesOrRefunds(t *testing.T) {
	t.Run("restored permit cannot be redispatched", func(t *testing.T) {
		runtime, clock := testRuntime(t, BudgetLimits{MaxContinuationTurns: 2})
		recordAdmittedTurn(t, runtime, AdmissionInitial, productiveTurn("initial", 5))
		if _, err := runtime.BeginContinuation(context.Background(), "orphaned"); err != nil {
			t.Fatal(err)
		}
		restored, err := Restore(mustSnapshot(t, runtime), WithClock(clock))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := restored.BeginContinuation(context.Background(), "orphaned"); !errors.Is(err, ErrTurnPending) {
			t.Fatalf("restored permit was eligible for redispatch: %v", err)
		}
		if err := restored.RecoverPendingContinuation(context.Background(), PendingRecovery{
			TurnID: "orphaned", Kind: PendingCancelledBeforeDispatch,
			Reason: "durable dispatch ledger has no requested event", Evidence: "checked through cursor 17",
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("proven pre-dispatch cancellation pauses", func(t *testing.T) {
		runtime, _ := testRuntime(t, BudgetLimits{MaxContinuationTurns: 2})
		recordAdmittedTurn(t, runtime, AdmissionInitial, productiveTurn("initial", 5))
		if _, err := runtime.BeginContinuation(context.Background(), "cancelled"); err != nil {
			t.Fatal(err)
		}
		recovery := PendingRecovery{
			TurnID: "cancelled", Kind: PendingCancelledBeforeDispatch,
			Reason: "host cancelled before MCP dispatch", Evidence: "dispatch marker was never written",
		}
		withoutEvidence := recovery
		withoutEvidence.Evidence = ""
		if err := runtime.RecoverPendingContinuation(context.Background(), withoutEvidence); !errors.Is(err, ErrInvalid) {
			t.Fatalf("missing evidence error = %v", err)
		}
		if err := runtime.RecoverPendingContinuation(context.Background(), recovery); err != nil {
			t.Fatal(err)
		}
		if err := runtime.RecoverPendingContinuation(context.Background(), recovery); err != nil {
			t.Fatalf("duplicate recovery was not idempotent: %v", err)
		}
		snapshot := mustSnapshot(t, runtime)
		if snapshot.State != StatePaused || snapshot.PendingContinuation != nil || snapshot.Usage.ContinuationTurns != 1 {
			t.Fatalf("pre-dispatch recovery snapshot = %#v", snapshot)
		}
		if snapshot.LastPendingRecovery == nil || snapshot.LastPendingRecovery.Recovery.Evidence != recovery.Evidence {
			t.Fatalf("recovery evidence was not retained: %#v", snapshot.LastPendingRecovery)
		}
		if err := runtime.Resume(context.Background(), "host explicitly resumed"); err != nil {
			t.Fatal(err)
		}
		if decision, _ := runtime.CanAutoContinue(context.Background()); !decision.Allowed {
			t.Fatalf("explicitly resumed proven cancellation should be eligible: %#v", decision)
		}
	})

	t.Run("uncertain dispatch blocks", func(t *testing.T) {
		runtime, clock := testRuntime(t, BudgetLimits{MaxContinuationTurns: 3})
		recordAdmittedTurn(t, runtime, AdmissionInitial, productiveTurn("initial", 5))
		if _, err := runtime.BeginContinuation(context.Background(), "uncertain"); err != nil {
			t.Fatal(err)
		}
		recovery := PendingRecovery{
			TurnID: "uncertain", Kind: PendingOutcomeUnknown, Reason: "connection closed after possible dispatch",
			Evidence: "no downstream terminal receipt", OutcomeRef: "exec_123",
		}
		if err := runtime.RecoverPendingContinuation(context.Background(), recovery); err != nil {
			t.Fatal(err)
		}
		snapshot := mustSnapshot(t, runtime)
		if snapshot.State != StateBlocked || snapshot.Blocker == nil || snapshot.Blocker.Reference != "exec_123" || snapshot.Usage.ContinuationTurns != 1 {
			t.Fatalf("unknown recovery snapshot = %#v", snapshot)
		}
		if decision, _ := runtime.CanAutoContinue(context.Background()); decision.Reason != ContinuationOutcomeUnknown {
			t.Fatalf("unknown outcome decision = %#v", decision)
		}
		if err := runtime.Complete(context.Background(), completionRequest()); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("completion with unknown outcome error = %v", err)
		}
		if err := runtime.ResolveBlock(context.Background(), BlockResolution{Reference: "exec_123", Reason: "checked workspace"}); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("unreconciled resolution error = %v", err)
		}
		resolution := BlockResolution{
			Reference: "exec_123", Reason: "workspace inspected", Reconciled: true,
			Evidence:       "execution ledger proves the write never started",
			Reconciliation: testReconciliationReceipt(),
		}
		if err := runtime.ResolveBlock(context.Background(), resolution); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("generic outcome resolution error = %v", err)
		}
		resolved, err := ApplyVerifiedReconciliation(context.Background(), snapshot, resolution, clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		snapshot = resolved
		if snapshot.State != StatePaused || snapshot.LastBlockResolution == nil || snapshot.LastBlockResolution.Resolution.Evidence != resolution.Evidence {
			t.Fatalf("resolved snapshot = %#v", snapshot)
		}
		if current := mustSnapshot(t, runtime); current.State != StateBlocked {
			t.Fatalf("pure reconciliation mutated caller runtime: %#v", current)
		}
	})
}

func TestUnproductiveTurnRequiresNewHostDirectedProgress(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{})
	unproductive := TurnReport{TurnID: "stalled", EvalTokens: 12, Summary: "repeated the same hypothesis"}
	recordAdmittedTurn(t, runtime, AdmissionInitial, unproductive)
	if decision, _ := runtime.CanAutoContinue(context.Background()); decision.Reason != ContinuationUnproductive {
		t.Fatalf("unproductive decision = %#v", decision)
	}
	if _, err := runtime.BeginContinuation(context.Background(), "must-not-run"); !errors.Is(err, ErrAutoContinuationDenied) {
		t.Fatalf("unproductive continuation error = %v", err)
	}
	if err := runtime.Resume(context.Background(), "user supplied new direction"); err != nil {
		t.Fatal(err)
	}
	if decision, _ := runtime.CanAutoContinue(context.Background()); decision.Reason != ContinuationUnproductive {
		t.Fatalf("resume incorrectly converted stale output into progress: %#v", decision)
	}
	recordAdmittedTurn(t, runtime, AdmissionManual, productiveTurn("user-directed", 8))
	if decision, _ := runtime.CanAutoContinue(context.Background()); !decision.Allowed {
		t.Fatalf("new productive turn did not restore eligibility: %#v", decision)
	}
}

func TestManualTransitionMatrixAndGenericBlock(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{})
	if err := runtime.Pause(context.Background(), "user requested a review break"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Pause(context.Background(), "again"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("second pause error = %v", err)
	}
	if err := runtime.Resume(context.Background(), "user returned"); err != nil {
		t.Fatal(err)
	}
	blocker := Blocker{Kind: BlockDependency, Reference: "ollama", Reason: "required model is unavailable"}
	if err := runtime.Block(context.Background(), blocker); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Block(context.Background(), blocker); err != nil {
		t.Fatalf("identical blocker was not idempotent: %v", err)
	}
	conflict := blocker
	conflict.Reference = "other"
	if err := runtime.Block(context.Background(), conflict); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("conflicting blocker error = %v", err)
	}
	if decision, _ := runtime.CanAutoContinue(context.Background()); decision.Reason != ContinuationBlocked {
		t.Fatalf("generic blocker decision = %#v", decision)
	}
	if err := runtime.Complete(context.Background(), completionRequest()); !errors.Is(err, ErrBlocked) {
		t.Fatalf("blocked completion error = %v", err)
	}
	if err := runtime.ResolveBlock(context.Background(), BlockResolution{Reference: "other", Reason: "wrong"}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("mismatched block resolution error = %v", err)
	}
	if err := runtime.ResolveBlock(context.Background(), BlockResolution{Reference: "ollama", Reason: "model installed"}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Resume(context.Background(), "dependency resolved"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Drop(context.Background(), "user no longer wants this goal"); err != nil {
		t.Fatal(err)
	}
	if snapshot := mustSnapshot(t, runtime); snapshot.State != StateDropped || snapshot.StateReason == "" {
		t.Fatalf("dropped snapshot = %#v", snapshot)
	}
	for name, operation := range map[string]func() error{
		"pause":  func() error { return runtime.Pause(context.Background(), "late") },
		"resume": func() error { return runtime.Resume(context.Background(), "late") },
		"drop":   func() error { return runtime.Drop(context.Background(), "late") },
		"budget": func() error { return runtime.AmendBudget(context.Background(), BudgetLimits{}, "late") },
		"cortex": func() error {
			return runtime.AttachCortex(context.Background(), CortexCorrelation{TaskID: "task", Actor: "actor"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, ErrTerminal) {
				t.Fatalf("terminal operation error = %v", err)
			}
		})
	}
}

func TestOutcomeUnknownTurnBlocksImmediately(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{MaxEvalTokens: 100})
	if _, err := runtime.BeginTurn(context.Background(), "unknown", AdmissionInitial); err != nil {
		t.Fatal(err)
	}
	invalid := productiveTurn("unknown", 12)
	invalid.OutcomeUnknown = true
	invalid.OutcomeRef = "exec_1"
	if err := runtime.RecordTurn(context.Background(), invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("productive unknown outcome error = %v", err)
	}
	report := TurnReport{
		TurnID: "unknown", EvalTokens: 12, Summary: "write may have started",
		OutcomeUnknown: true, OutcomeRef: "exec_1",
	}
	if err := runtime.RecordTurn(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	snapshot := mustSnapshot(t, runtime)
	if snapshot.State != StateBlocked || snapshot.Blocker == nil || snapshot.Blocker.Kind != BlockOutcomeUnknown || snapshot.Usage.EvalTokens != 12 {
		t.Fatalf("outcome-unknown turn snapshot = %#v", snapshot)
	}
	if err := runtime.Resume(context.Background(), "unsafe"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("blocked resume error = %v", err)
	}
}

func TestBudgetExhaustionNeverCompletesGoal(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{MaxEvalTokens: 100})
	recordAdmittedTurn(t, runtime, AdmissionInitial, productiveTurn("token-limit", 100))
	snapshot := mustSnapshot(t, runtime)
	if snapshot.State != StateExhausted || snapshot.Completion != nil {
		t.Fatalf("budget exhaustion implied completion: %#v", snapshot)
	}
	incomplete := completionRequest()
	incomplete.Results = incomplete.Results[:1]
	if err := runtime.Complete(context.Background(), incomplete); !errors.Is(err, ErrAcceptanceIncomplete) {
		t.Fatalf("incomplete acceptance error = %v", err)
	}
	if err := runtime.Complete(context.Background(), completionRequest()); err != nil {
		t.Fatal(err)
	}
	snapshot = mustSnapshot(t, runtime)
	if snapshot.State != StateCompleted || snapshot.Completion == nil || len(snapshot.Completion.Results) != 2 {
		t.Fatalf("explicit completion snapshot = %#v", snapshot)
	}
	if err := runtime.Drop(context.Background(), "too late"); !errors.Is(err, ErrTerminal) {
		t.Fatalf("terminal transition error = %v", err)
	}
}

func TestWallBudgetRestoreAndExplicitReplenishment(t *testing.T) {
	runtime, clock := testRuntime(t, BudgetLimits{MaxWallTime: time.Hour})
	saved := mustSnapshot(t, runtime)
	clock.Advance(time.Hour)
	restored, err := Restore(saved, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := mustSnapshot(t, restored)
	if snapshot.State != StateExhausted || len(snapshot.ExhaustedBy) != 1 || snapshot.ExhaustedBy[0] != BudgetWallTime {
		t.Fatalf("wall-time restore snapshot = %#v", snapshot)
	}
	if err := restored.Resume(context.Background(), "without budget"); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("resume before amendment error = %v", err)
	}
	if err := restored.AmendBudget(context.Background(), BudgetLimits{MaxWallTime: 2 * time.Hour}, "user extended deadline"); err != nil {
		t.Fatal(err)
	}
	snapshot = mustSnapshot(t, restored)
	if snapshot.State != StateExhausted || len(snapshot.ExhaustedBy) != 0 {
		t.Fatalf("budget amendment silently resumed goal: %#v", snapshot)
	}
	if err := restored.Resume(context.Background(), "budget extension approved"); err != nil {
		t.Fatal(err)
	}
	if snapshot = mustSnapshot(t, restored); snapshot.State != StateActive {
		t.Fatalf("explicit resume state = %s", snapshot.State)
	}
}

func TestCortexCorrelationIsMonotonicAndImmutable(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{})
	first := CortexCorrelation{TaskID: "task_1", Revision: 3, Actor: "sonar/session-42"}
	if err := runtime.AttachCortex(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AttachCortex(context.Background(), first); err != nil {
		t.Fatalf("idempotent correlation failed: %v", err)
	}
	if err := runtime.AttachCortex(context.Background(), CortexCorrelation{TaskID: first.TaskID, Revision: 2, Actor: first.Actor}); !errors.Is(err, ErrCorrelationConflict) {
		t.Fatalf("revision regression error = %v", err)
	}
	if err := runtime.AttachCortex(context.Background(), CortexCorrelation{TaskID: "task_2", Revision: 4, Actor: first.Actor}); !errors.Is(err, ErrCorrelationConflict) {
		t.Fatalf("task replacement error = %v", err)
	}
	updated := first
	updated.Revision = 4
	if err := runtime.AttachCortex(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	if got := mustSnapshot(t, runtime).Cortex; got != updated {
		t.Fatalf("cortex correlation = %#v", got)
	}
}

func TestSnapshotRoundTripIsolationAndForgeryRejection(t *testing.T) {
	runtime, clock := testRuntime(t, BudgetLimits{MaxContinuationTurns: 3})
	recordAdmittedTurn(t, runtime, AdmissionInitial, productiveTurn("initial", 2))
	if _, err := runtime.BeginContinuation(context.Background(), "unknown"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecoverPendingContinuation(context.Background(), PendingRecovery{
		TurnID: "unknown", Kind: PendingOutcomeUnknown, Reason: "lost receipt",
		Evidence: "transport closed", OutcomeRef: "exec_9",
	}); err != nil {
		t.Fatal(err)
	}
	resolution := BlockResolution{
		Reference: "exec_9", Reason: "inspected ledger", Reconciled: true,
		Evidence: "no dispatch event exists", Reconciliation: testReconciliationReceipt(),
	}
	resolved, err := ApplyVerifiedReconciliation(context.Background(), mustSnapshot(t, runtime), resolution, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = Restore(resolved, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}

	snapshot := mustSnapshot(t, runtime)
	if snapshot.LastBlockResolution == nil || snapshot.LastPendingRecovery == nil {
		t.Fatalf("missing recovery records: %#v", snapshot)
	}
	snapshot.LastBlockResolution.Resolution.Evidence = "forged"
	snapshot.LastPendingRecovery.Recovery.Evidence = "forged"
	again := mustSnapshot(t, runtime)
	if again.LastBlockResolution.Resolution.Evidence == "forged" || again.LastPendingRecovery.Recovery.Evidence == "forged" {
		t.Fatal("returned recovery records alias runtime state")
	}

	encoded, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(decoded, WithClock(clock)); err != nil {
		t.Fatalf("valid JSON round trip failed: %v", err)
	}

	forgedReference := cloneSnapshot(again)
	forgedReference.LastBlockResolution.Resolution.Reference = "other"
	if _, err := Restore(forgedReference, WithClock(clock)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged resolution reference error = %v", err)
	}
	forgedEvidence := cloneSnapshot(again)
	forgedEvidence.LastBlockResolution.Resolution.Evidence = ""
	if _, err := Restore(forgedEvidence, WithClock(clock)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing reconciliation evidence error = %v", err)
	}
	forgedReceipt := cloneSnapshot(again)
	forgedReceipt.LastBlockResolution.Resolution.Reconciliation.ResolutionSetSHA256 = strings.Repeat("0", 63) + "z"
	if _, err := Restore(forgedReceipt, WithClock(clock)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged typed reconciliation receipt error = %v", err)
	}
	forgedTime := cloneSnapshot(again)
	forgedTime.LastBlockResolution.ResolvedAt = forgedTime.UpdatedAt.Add(time.Second)
	if _, err := Restore(forgedTime, WithClock(clock)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged resolution time error = %v", err)
	}
}

func TestRestoreRejectsLegacyOutcomeProseWithoutDurableReceipt(t *testing.T) {
	runtime, clock := testRuntime(t, BudgetLimits{})
	if _, err := runtime.BeginTurn(context.Background(), "legacy_unknown", AdmissionInitial); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecoverPendingContinuation(context.Background(), PendingRecovery{
		TurnID: "legacy_unknown", Kind: PendingOutcomeUnknown,
		Reason: "legacy process lost", Evidence: "legacy host text", OutcomeRef: "exec_legacy",
	}); err != nil {
		t.Fatal(err)
	}
	legacy := mustSnapshot(t, runtime)
	legacy.Version = LegacySnapshotVersion
	legacy.Blocker = nil
	legacy.State = StatePaused
	legacy.StateReason = "legacy block resolved"
	legacy.LastBlockResolution = &BlockResolutionRecord{
		Blocker:    Blocker{Kind: BlockOutcomeUnknown, Reference: "exec_legacy", Reason: "legacy process lost", BlockedAt: legacy.UpdatedAt},
		Resolution: BlockResolution{Reference: "exec_legacy", Reason: "legacy inspection", Reconciled: true, Evidence: "unbound prose"},
		ResolvedAt: legacy.UpdatedAt,
	}
	if _, err := Restore(legacy, WithClock(clock)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no durable control receipt") {
		t.Fatalf("legacy unbound reconciliation error = %v", err)
	}
}

func TestCancelledContextDoesNotMutateRuntime(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{})
	before := mustSnapshot(t, runtime)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtime.RecordTurn(ctx, productiveTurn("cancelled", 100)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context error = %v", err)
	}
	after := mustSnapshot(t, runtime)
	if before.Usage != after.Usage || before.State != after.State || after.LastTurn != nil {
		t.Fatalf("cancelled operation mutated runtime: before=%#v after=%#v", before, after)
	}
}

func TestNilContextFailsClosed(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{})
	if _, err := runtime.Snapshot(nil); !errors.Is(err, ErrInvalid) { //nolint:staticcheck // intentional fail-closed API boundary test
		t.Fatalf("nil Snapshot context error = %v", err)
	}
	if _, err := runtime.CanAutoContinue(nil); !errors.Is(err, ErrInvalid) { //nolint:staticcheck // intentional fail-closed API boundary test
		t.Fatalf("nil continuation context error = %v", err)
	}
	if err := runtime.Pause(nil, "pause"); !errors.Is(err, ErrInvalid) { //nolint:staticcheck // intentional fail-closed API boundary test
		t.Fatalf("nil Pause context error = %v", err)
	}
}

func TestConcurrentReadersAndCorrelationUpdates(t *testing.T) {
	runtime, _ := testRuntime(t, BudgetLimits{})
	recordAdmittedTurn(t, runtime, AdmissionInitial, productiveTurn("initial", 1))
	correlation := CortexCorrelation{TaskID: "task_1", Actor: "actor_1"}
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for revision := int64(0); revision < 20; revision++ {
				_, _ = runtime.Snapshot(context.Background())
				_, _ = runtime.CanAutoContinue(context.Background())
				candidate := correlation
				candidate.Revision = revision
				_ = runtime.AttachCortex(context.Background(), candidate)
			}
		}()
	}
	wait.Wait()
	snapshot := mustSnapshot(t, runtime)
	if snapshot.Cortex.TaskID != correlation.TaskID || snapshot.Cortex.Actor != correlation.Actor || snapshot.Cortex.Revision < 0 {
		t.Fatalf("concurrent correlation = %#v", snapshot.Cortex)
	}
}
