package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type countingClock struct {
	now   time.Time
	calls int
}

type steppingClock struct {
	now  time.Time
	step time.Duration
	call int
}

func (c *steppingClock) Now() time.Time {
	value := c.now.Add(time.Duration(c.call) * c.step)
	c.call++
	return value
}

type fixedEvaluationBases struct {
	basis EvaluationBasis
	err   error
}

func (r fixedEvaluationBases) EvaluationBasis(_ context.Context, _, _ string) (EvaluationBasis, error) {
	return r.basis, r.err
}

func (c *countingClock) Now() time.Time {
	c.calls++
	return c.now
}

func newRuntime(t *testing.T, linked bool, budget goal.BudgetLimits) *goal.Runtime {
	t.Helper()
	spec := goal.Spec{
		ID:        "goal_supervisor",
		SessionID: 42,
		Objective: "Ship a UI-independent supervisor",
		AcceptanceCriteria: []goal.AcceptanceCriterion{
			{ID: "safe", Description: "unsafe continuations stop"},
		},
		Budget: budget,
	}
	if linked {
		spec.Cortex = goal.CortexCorrelation{TaskID: "task_123", Revision: 1, Actor: "sonar"}
	}
	runtime, err := goal.New(spec, goal.WithClock(fixedClock{now: time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC)}))
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func authority() Observation {
	return Observation{LeaseOwned: true, PersistenceReady: true, AdvisorAvailable: true}
}

func recordProductive(t *testing.T, runtime *goal.Runtime, id string) {
	t.Helper()
	if _, err := runtime.BeginTurn(context.Background(), id, goal.AdmissionInitial); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordTurn(context.Background(), goal.TurnReport{
		TurnID: id, EvalTokens: 10, Productive: true, Summary: "one tested slice completed",
	}); err != nil {
		t.Fatal(err)
	}
}

func recordManualProductive(t *testing.T, runtime *goal.Runtime, id string) {
	t.Helper()
	if _, err := runtime.BeginTurn(context.Background(), id, goal.AdmissionManual); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RecordTurn(context.Background(), goal.TurnReport{
		TurnID: id, EvalTokens: 10, Productive: true, Summary: "one tested slice completed",
	}); err != nil {
		t.Fatal(err)
	}
}

func evaluationFixture(t *testing.T, runtime *goal.Runtime, status EvaluationStatus, baseline int64) (*EvaluationReceipt, EvaluationBasisReader) {
	t.Helper()
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LastTurn == nil {
		t.Fatal("evaluation fixture requires a settled turn")
	}
	receipt := &EvaluationReceipt{
		Status: status, TurnID: snapshot.LastTurn.TurnID,
		CortexTaskID: snapshot.Cortex.TaskID, Revision: snapshot.Cortex.Revision,
	}
	basis := fixedEvaluationBases{basis: EvaluationBasis{
		RecordID: "basis_" + snapshot.LastTurn.TurnID,
		GoalID:   snapshot.ID, TurnID: snapshot.LastTurn.TurnID,
		CortexTaskID: snapshot.Cortex.TaskID, CortexRevision: baseline,
		RecordedAt: snapshot.LastTurn.RecordedAt,
	}}
	return receipt, basis
}

func TestDecideRequiresLeaseAndPersistenceBeforeWork(t *testing.T) {
	runtime := newRuntime(t, true, goal.BudgetLimits{})
	decision, err := Decide(context.Background(), runtime, Observation{
		PersistenceReady: true,
		Issues:           []Issue{{ID: "outcome", Kind: IssueOutcomeUnknown, Summary: "unknown effect"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != ActionStop || decision.Reason != StopLeaseRequired {
		t.Fatalf("lease decision = %#v", decision)
	}
	decision, err = Decide(context.Background(), runtime, Observation{
		LeaseOwned: true,
		Issues:     []Issue{{ID: "decision", Kind: IssueDecision, Summary: "choose"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Reason != StopPersistenceUnavailable {
		t.Fatalf("persistence decision = %#v", decision)
	}
}

func TestDecideEstablishesAuthorityBeforeRefreshingGoalRuntime(t *testing.T) {
	clock := &countingClock{now: time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC)}
	runtime, err := goal.New(goal.Spec{
		ID: "authority_order", SessionID: 42, Objective: "do not mutate before authority",
		AcceptanceCriteria: []goal.AcceptanceCriterion{{ID: "safe", Description: "state stays durable"}},
		Budget:             goal.BudgetLimits{MaxWallTime: time.Minute},
	}, goal.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Hour)
	before := clock.calls
	decision, err := Decide(context.Background(), runtime, Observation{PersistenceReady: true})
	if err != nil || decision.Reason != StopLeaseRequired {
		t.Fatalf("lease decision = %#v, %v", decision, err)
	}
	if clock.calls != before {
		t.Fatalf("lease-less decision refreshed Goal Runtime: calls %d -> %d", before, clock.calls)
	}
	decision, err = Decide(context.Background(), runtime, Observation{LeaseOwned: true})
	if err != nil || decision.Reason != StopPersistenceUnavailable {
		t.Fatalf("persistence decision = %#v, %v", decision, err)
	}
	if clock.calls != before {
		t.Fatalf("unpersistable decision refreshed Goal Runtime: calls %d -> %d", before, clock.calls)
	}
}

func TestDecideInitialManualAndRunningTurns(t *testing.T) {
	runtime := newRuntime(t, true, goal.BudgetLimits{MaxContinuationTurns: 3})
	decision, err := Decide(context.Background(), runtime, authority())
	if err != nil || decision.Action != ActionDispatchInitial {
		t.Fatalf("initial decision = %#v, %v", decision, err)
	}

	observation := authority()
	observation.RunningTurnID = "initial"
	decision, err = Decide(context.Background(), runtime, observation)
	if err != nil || decision.Action != ActionWaitTurn {
		t.Fatalf("running initial decision = %#v, %v", decision, err)
	}
	recordProductive(t, runtime, "initial")
	observation = authority()
	observation.Manual = true
	decision, err = Decide(context.Background(), runtime, observation)
	if err != nil || decision.Action != ActionEvaluate {
		t.Fatalf("linked manual preflight = %#v, %v", decision, err)
	}
	observation.Evaluation, observation.EvaluationBases = evaluationFixture(t, runtime, EvaluationNoProgress, 1)
	decision, err = Decide(context.Background(), runtime, observation)
	if err != nil || decision.Action != ActionDispatchManual {
		t.Fatalf("evaluated manual decision = %#v, %v", decision, err)
	}
}

func TestDecideEvaluatesThenContinuesOnlyAfterProgress(t *testing.T) {
	runtime := newRuntime(t, true, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordProductive(t, runtime, "initial")
	decision, err := Decide(context.Background(), runtime, authority())
	if err != nil || decision.Action != ActionEvaluate {
		t.Fatalf("evaluation decision = %#v, %v", decision, err)
	}

	observation := authority()
	observation.Evaluation, observation.EvaluationBases = evaluationFixture(t, runtime, EvaluationNoProgress, 1)
	decision, err = Decide(context.Background(), runtime, observation)
	if err != nil || decision.Reason != StopNoProgress {
		t.Fatalf("no-progress decision = %#v, %v", decision, err)
	}

	if err := runtime.AttachCortex(context.Background(), goal.CortexCorrelation{
		TaskID: "task_123", Revision: 2, Actor: "sonar",
	}); err != nil {
		t.Fatal(err)
	}
	observation.Evaluation, observation.EvaluationBases = evaluationFixture(t, runtime, EvaluationProgressed, 1)
	decision, err = Decide(context.Background(), runtime, observation)
	if err != nil || decision.Action != ActionDispatchContinuation {
		t.Fatalf("progressed decision = %#v, %v", decision, err)
	}
}

func TestDecideRequiresEvaluationBoundToLastTurnAndDurableRevision(t *testing.T) {
	runtime := newRuntime(t, true, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordProductive(t, runtime, "turn_1")

	base := authority()
	base.Evaluation = &EvaluationReceipt{
		Status: EvaluationProgressed, TurnID: "turn_1", CortexTaskID: "task_123",
		Revision: 1,
	}
	_, base.EvaluationBases = evaluationFixture(t, runtime, EvaluationProgressed, 1)
	if _, err := Decide(context.Background(), runtime, base); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-advancing progress error = %v", err)
	}

	if err := runtime.AttachCortex(context.Background(), goal.CortexCorrelation{
		TaskID: "task_123", Revision: 2, Actor: "sonar",
	}); err != nil {
		t.Fatal(err)
	}
	valid := authority()
	valid.Evaluation, valid.EvaluationBases = evaluationFixture(t, runtime, EvaluationProgressed, 1)
	decision, err := Decide(context.Background(), runtime, valid)
	if err != nil || decision.Action != ActionDispatchContinuation {
		t.Fatalf("bound evaluation decision = %#v, %v", decision, err)
	}

	wrongTurn := *valid.Evaluation
	wrongTurn.TurnID = "turn_other"
	observation := authority()
	observation.Evaluation = &wrongTurn
	observation.EvaluationBases = valid.EvaluationBases
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong-turn evaluation error = %v", err)
	}
	wrongTask := *valid.Evaluation
	wrongTask.CortexTaskID = "task_other"
	observation.Evaluation = &wrongTask
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong-task evaluation error = %v", err)
	}
	undurable := *valid.Evaluation
	undurable.Revision = 3
	observation.Evaluation = &undurable
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("undurable revision error = %v", err)
	}

	recordManualProductive(t, runtime, "turn_2")
	observation.Evaluation = valid.Evaluation
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale last-turn evaluation error = %v", err)
	}
}

func TestDecideStopsWithRefreshedGoalWhenContinuationObservationChanges(t *testing.T) {
	clock := &steppingClock{
		now: time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC), step: time.Minute,
	}
	runtime, err := goal.New(goal.Spec{
		ID: "stale_observation", SessionID: 42, Objective: "return the refreshed state",
		AcceptanceCriteria: []goal.AcceptanceCriterion{{ID: "fresh", Description: "decision uses current state"}},
		// Universal turn admission samples the goal clock once before the
		// receipt. Six steps keep exhaustion at the Decide refresh boundary.
		Budget: goal.BudgetLimits{MaxContinuationTurns: 3, MaxWallTime: 6 * time.Minute},
		Cortex: goal.CortexCorrelation{TaskID: "task_123", Revision: 1, Actor: "sonar"},
	}, goal.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	recordProductive(t, runtime, "turn_1")
	if err := runtime.AttachCortex(context.Background(), goal.CortexCorrelation{
		TaskID: "task_123", Revision: 2, Actor: "sonar",
	}); err != nil {
		t.Fatal(err)
	}
	observation := authority()
	observation.Evaluation, observation.EvaluationBases = evaluationFixture(t, runtime, EvaluationProgressed, 1)
	decision, err := Decide(context.Background(), runtime, observation)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != ActionStop || decision.Reason != StopObservationStale || decision.Goal.State != goal.StateExhausted {
		t.Fatalf("stale observation decision = %#v", decision)
	}
}

func TestDecideRejectsMissingOrForgedDurableEvaluationBasis(t *testing.T) {
	runtime := newRuntime(t, true, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordProductive(t, runtime, "turn_1")
	if err := runtime.AttachCortex(context.Background(), goal.CortexCorrelation{
		TaskID: "task_123", Revision: 2, Actor: "sonar",
	}); err != nil {
		t.Fatal(err)
	}
	receipt, reader := evaluationFixture(t, runtime, EvaluationProgressed, 1)
	observation := authority()
	observation.Evaluation = receipt
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing durable basis error = %v", err)
	}

	forged := reader.(fixedEvaluationBases)
	forged.basis.GoalID = "goal_other"
	observation.EvaluationBases = forged
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-goal basis error = %v", err)
	}
	forged = reader.(fixedEvaluationBases)
	forged.basis.RecordedAt = forged.basis.RecordedAt.Add(time.Second)
	observation.EvaluationBases = forged
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("post-turn basis error = %v", err)
	}
	observation.EvaluationBases = fixedEvaluationBases{err: errors.New("basis persistence unavailable")}
	if _, err := Decide(context.Background(), runtime, observation); err == nil || !strings.Contains(err.Error(), "persistence unavailable") {
		t.Fatalf("basis read error = %v", err)
	}
}

func TestDecideRefusesUnlinkedAutomaticContinuation(t *testing.T) {
	runtime := newRuntime(t, false, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordProductive(t, runtime, "initial")
	decision, err := Decide(context.Background(), runtime, authority())
	if err != nil || decision.Reason != StopCortexUnavailable {
		t.Fatalf("unlinked decision = %#v, %v", decision, err)
	}
	observation := authority()
	observation.Manual = true
	decision, err = Decide(context.Background(), runtime, observation)
	if err != nil || decision.Action != ActionDispatchManual {
		t.Fatalf("unlinked manual decision = %#v, %v", decision, err)
	}
}

func TestDecideOrphanedAndMismatchedPermitsRequireRecovery(t *testing.T) {
	runtime := newRuntime(t, true, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordProductive(t, runtime, "initial")
	if _, err := runtime.BeginContinuation(context.Background(), "continuation"); err != nil {
		t.Fatal(err)
	}
	decision, err := Decide(context.Background(), runtime, authority())
	if err != nil || decision.Reason != StopRecoveryRequired {
		t.Fatalf("orphan decision = %#v, %v", decision, err)
	}

	observation := authority()
	observation.RunningTurnID = "other"
	decision, err = Decide(context.Background(), runtime, observation)
	if err != nil || decision.Reason != StopTurnIdentityConflict {
		t.Fatalf("identity decision = %#v, %v", decision, err)
	}
	observation.RunningTurnID = "continuation"
	decision, err = Decide(context.Background(), runtime, observation)
	if err != nil || decision.Action != ActionWaitTurn {
		t.Fatalf("matched turn decision = %#v, %v", decision, err)
	}
}

func TestDecideControlPlaneIssuesUseConservativePriority(t *testing.T) {
	runtime := newRuntime(t, true, goal.BudgetLimits{})
	observation := authority()
	observation.Issues = []Issue{
		{ID: "dependency", Kind: IssueDependency, Summary: "waiting for input"},
		{ID: "approval", Kind: IssueApproval, Summary: "permission required"},
		{ID: "outcome", Kind: IssueOutcomeUnknown, Summary: "backend receipt missing"},
		{ID: "decision", Kind: IssueDecision, Summary: "choose a migration"},
	}
	decision, err := Decide(context.Background(), runtime, observation)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Reason != StopOutcomeUnknown || decision.Detail != "backend receipt missing" || len(decision.IssueIDs) != 4 {
		t.Fatalf("issue decision = %#v", decision)
	}
}

func TestDecideMapsGoalTerminalAndBlockedStates(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*goal.Runtime) error
		want   StopReason
	}{
		{name: "paused", mutate: func(runtime *goal.Runtime) error { return runtime.Pause(context.Background(), "review") }, want: StopPaused},
		{name: "dropped", mutate: func(runtime *goal.Runtime) error { return runtime.Drop(context.Background(), "obsolete") }, want: StopDropped},
		{name: "decision", mutate: func(runtime *goal.Runtime) error {
			return runtime.Block(context.Background(), goal.Blocker{Kind: goal.BlockDecision, Reference: "decision_1", Reason: "choose"})
		}, want: StopDecision},
		{name: "dependency", mutate: func(runtime *goal.Runtime) error {
			return runtime.Block(context.Background(), goal.Blocker{Kind: goal.BlockDependency, Reference: "dependency_1", Reason: "wait"})
		}, want: StopDependency},
		{name: "outcome", mutate: func(runtime *goal.Runtime) error {
			return runtime.Block(context.Background(), goal.Blocker{Kind: goal.BlockOutcomeUnknown, Reference: "execution_1", Reason: "unknown"})
		}, want: StopOutcomeUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := newRuntime(t, true, goal.BudgetLimits{})
			if err := test.mutate(runtime); err != nil {
				t.Fatal(err)
			}
			decision, err := Decide(context.Background(), runtime, authority())
			if err != nil || decision.Reason != test.want {
				t.Fatalf("decision = %#v, %v; want %s", decision, err, test.want)
			}
		})
	}
}

func TestDecideMapsUnproductiveAndBudgetStops(t *testing.T) {
	unproductive := newRuntime(t, true, goal.BudgetLimits{MaxContinuationTurns: 3})
	if _, err := unproductive.BeginTurn(context.Background(), "yield", goal.AdmissionInitial); err != nil {
		t.Fatal(err)
	}
	if err := unproductive.RecordTurn(context.Background(), goal.TurnReport{
		TurnID: "yield", Summary: "no concrete receipt",
	}); err != nil {
		t.Fatal(err)
	}
	observation := authority()
	decision, err := Decide(context.Background(), unproductive, observation)
	if err != nil || decision.Reason != StopUnproductive {
		t.Fatalf("unproductive decision = %#v, %v", decision, err)
	}

	exhausted := newRuntime(t, true, goal.BudgetLimits{MaxEvalTokens: 10})
	recordProductive(t, exhausted, "exact-budget")
	decision, err = Decide(context.Background(), exhausted, observation)
	if err != nil || decision.Reason != StopExhausted {
		t.Fatalf("exhausted decision = %#v, %v", decision, err)
	}
}

func TestDecideRejectsInvalidAndCancelledObservations(t *testing.T) {
	runtime := newRuntime(t, true, goal.BudgetLimits{})
	//nolint:staticcheck // Deliberately exercise the public nil-context rejection contract.
	if _, err := Decide(nil, runtime, authority()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil context error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Decide(ctx, runtime, authority()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context error = %v", err)
	}
	observation := authority()
	observation.Issues = []Issue{{ID: "bad", Kind: "mystery"}}
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid issue error = %v", err)
	}
	observation = authority()
	observation.RunningTurnID = "  "
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("whitespace running turn error = %v", err)
	}
	observation = authority()
	observation.RunningTurnID = string([]byte{0xff})
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid UTF-8 running turn error = %v", err)
	}
	observation = authority()
	observation.Issues = []Issue{
		{ID: "duplicate", Kind: IssueDecision, Summary: "one"},
		{ID: "duplicate", Kind: IssueApproval, Summary: "two"},
	}
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate issue error = %v", err)
	}
	observation = authority()
	observation.Issues = []Issue{{ID: "blank", Kind: IssueDecision, Summary: " \t "}}
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank issue summary error = %v", err)
	}
	observation = authority()
	observation.Evaluation = &EvaluationReceipt{
		Status: "forged", TurnID: "turn", CortexTaskID: "task_123", Revision: 1,
	}
	if _, err := Decide(context.Background(), runtime, observation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid evaluation error = %v", err)
	}
}
