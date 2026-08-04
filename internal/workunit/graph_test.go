package workunit

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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
	return &fakeClock{now: time.Date(2026, time.July, 12, 16, 0, 0, 0, time.UTC)}
}

func unit(id string, role Role, effect EffectPolicy, dependencies ...string) UnitSpec {
	return UnitSpec{
		ID:        id,
		Title:     "Specialist " + id,
		Role:      role,
		Effect:    effect,
		Prompt:    "Perform the bounded " + id + " responsibility.",
		DependsOn: dependencies,
	}
}

func testSpec() GraphSpec {
	explore := unit("explore", RoleExplorer, EffectReadOnly)
	explore.AcceptanceCriterionIDs = []string{"ux"}
	implement := unit("implement", RoleImplementer, EffectWriter, "explore")
	implement.AcceptanceCriterionIDs = []string{"ux"}
	implement.ProofExpectations = []string{"focused tests pass"}
	verify := unit("verify", RoleVerifier, EffectReadOnly, "implement")
	verify.AcceptanceCriterionIDs = []string{"ux"}
	verify.ProofExpectations = []string{"run the focused test suite"}
	return GraphSpec{
		ID:          "work_test",
		GoalID:      "goal_test",
		SessionID:   42,
		WorkspaceID: "/tmp/workspace",
		Units:       []UnitSpec{explore, implement, verify},
	}
}

func testGraph(t *testing.T) (*Graph, *fakeClock) {
	t.Helper()
	clock := testClock()
	graph, err := New(testSpec(), WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	return graph, clock
}

func snapshot(t *testing.T, graph *Graph) Snapshot {
	t.Helper()
	snapshot, err := graph.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func completion(summary string) CompletionReceipt {
	return CompletionReceipt{
		Attempt: 1,
		Summary: summary,
		Evidence: []Evidence{{
			Kind:    "test",
			Summary: "go test passed",
			Ref:     "go test ./internal/workunit",
		}},
	}
}

func TestNewValidatesScopeCapabilitiesAndGraph(t *testing.T) {
	tests := []struct {
		name string
		edit func(*GraphSpec)
	}{
		{name: "scope", edit: func(spec *GraphSpec) { spec.SessionID = 0 }},
		{name: "duplicate unit", edit: func(spec *GraphSpec) { spec.Units[1].ID = "explore" }},
		{name: "unknown dependency", edit: func(spec *GraphSpec) { spec.Units[1].DependsOn = []string{"missing"} }},
		{name: "cycle", edit: func(spec *GraphSpec) { spec.Units[0].DependsOn = []string{"implement"} }},
		{name: "explorer writer", edit: func(spec *GraphSpec) { spec.Units[0].Effect = EffectWriter }},
		{name: "verifier writer", edit: func(spec *GraphSpec) { spec.Units[2].Effect = EffectWriter }},
		{name: "verifier without dependency", edit: func(spec *GraphSpec) { spec.Units[2].DependsOn = nil }},
		{name: "verifier without criteria", edit: func(spec *GraphSpec) { spec.Units[2].AcceptanceCriterionIDs = nil }},
		{name: "verifier without proof", edit: func(spec *GraphSpec) { spec.Units[2].ProofExpectations = nil }},
		{name: "verifier criterion mismatch", edit: func(spec *GraphSpec) { spec.Units[2].AcceptanceCriterionIDs = []string{"other"} }},
		{name: "negative budget", edit: func(spec *GraphSpec) { spec.Units[0].Budget.MaxTurns = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := testSpec()
			test.edit(&spec)
			if _, err := New(spec); !errors.Is(err, ErrInvalid) {
				t.Fatalf("New() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestNewGeneratesIDAndSnapshotsAreIsolated(t *testing.T) {
	spec := testSpec()
	spec.ID = ""
	first, err := New(spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(spec)
	if err != nil {
		t.Fatal(err)
	}
	firstSnapshot := snapshot(t, first)
	secondSnapshot := snapshot(t, second)
	if !strings.HasPrefix(firstSnapshot.ID, "work_") || firstSnapshot.ID == secondSnapshot.ID {
		t.Fatalf("generated IDs = %q, %q", firstSnapshot.ID, secondSnapshot.ID)
	}

	firstSnapshot.Units[0].Spec.DependsOn = append(firstSnapshot.Units[0].Spec.DependsOn, "forged")
	firstSnapshot.Units[1].Spec.AcceptanceCriterionIDs[0] = "forged"
	if current := snapshot(t, first); current.Units[0].Spec.DependsOn != nil || current.Units[1].Spec.AcceptanceCriterionIDs[0] != "ux" {
		t.Fatalf("snapshot aliases graph state: %#v", current.Units)
	}
}

func TestDependencyChainAdmitsExplorerImplementerThenVerifier(t *testing.T) {
	graph, clock := testGraph(t)
	ready, err := graph.Ready(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ready.ReadOnly) != 1 || ready.ReadOnly[0].Spec.ID != "explore" || ready.Writer != nil {
		t.Fatalf("initial ready set = %#v", ready)
	}
	if decision, _ := graph.Readiness(context.Background(), "implement"); decision.Reason != ReadyDependencyPending || decision.Dependency != "explore" {
		t.Fatalf("implement readiness = %#v", decision)
	}

	firstAdmission, err := graph.Start(context.Background(), "explore")
	if err != nil {
		t.Fatal(err)
	}
	if !firstAdmission.Started || firstAdmission.Attempt != 1 {
		t.Fatalf("first admission = %#v", firstAdmission)
	}
	replayedAdmission, err := graph.Start(context.Background(), "explore")
	if err != nil {
		t.Fatalf("duplicate Start() was not idempotent: %v", err)
	}
	if replayedAdmission.Started || replayedAdmission.Attempt != firstAdmission.Attempt || !replayedAdmission.StartedAt.Equal(firstAdmission.StartedAt) {
		t.Fatalf("replayed admission could authorize redispatch: %#v", replayedAdmission)
	}
	clock.Advance(time.Second)
	if err := graph.Complete(context.Background(), "explore", completion("research complete")); err != nil {
		t.Fatal(err)
	}
	ready, _ = graph.Ready(context.Background())
	if ready.Writer == nil || ready.Writer.Spec.ID != "implement" || len(ready.ReadOnly) != 0 {
		t.Fatalf("post-exploration ready set = %#v", ready)
	}

	clock.Advance(time.Second)
	if _, err := graph.Start(context.Background(), "implement"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	implementationReceipt := completion("implementation complete")
	if err := graph.Complete(context.Background(), "implement", implementationReceipt); err != nil {
		t.Fatal(err)
	}
	if err := graph.Complete(context.Background(), "implement", implementationReceipt); err != nil {
		t.Fatalf("duplicate completion was not idempotent: %v", err)
	}
	if err := graph.Complete(context.Background(), "implement", completion("different")); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting completion error = %v", err)
	}
	ready, _ = graph.Ready(context.Background())
	if len(ready.ReadOnly) != 1 || ready.ReadOnly[0].Spec.ID != "verify" || ready.Writer != nil {
		t.Fatalf("verification ready set = %#v", ready)
	}

	clock.Advance(time.Second)
	if _, err := graph.Start(context.Background(), "verify"); err != nil {
		t.Fatal(err)
	}
	if err := graph.Complete(context.Background(), "verify", CompletionReceipt{Attempt: 1, Summary: "looks good"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("evidence-free verifier error = %v", err)
	}
	clock.Advance(time.Second)
	if err := graph.Complete(context.Background(), "verify", completion("independent proof complete")); err != nil {
		t.Fatal(err)
	}
	ready, _ = graph.Ready(context.Background())
	if len(ready.ReadOnly) != 0 || ready.Writer != nil {
		t.Fatalf("settled graph still has ready work: %#v", ready)
	}
}

func TestWriterLaneSerializesImplementersWhileReadersRemainParallel(t *testing.T) {
	first := unit("writer-a", RoleImplementer, EffectWriter)
	first.AcceptanceCriterionIDs = []string{"a"}
	second := unit("writer-b", RoleImplementer, EffectWriter)
	second.AcceptanceCriterionIDs = []string{"b"}
	explorer := unit("reader-a", RoleExplorer, EffectReadOnly)
	reader := unit("reader-b", RoleExplorer, EffectReadOnly)
	spec := testSpec()
	spec.Units = []UnitSpec{first, second, explorer, reader}
	graph, err := New(spec, WithClock(testClock()))
	if err != nil {
		t.Fatal(err)
	}
	ready, _ := graph.Ready(context.Background())
	if ready.Writer == nil || ready.Writer.Spec.ID != "writer-a" || len(ready.ReadOnly) != 2 {
		t.Fatalf("initial ready set = %#v", ready)
	}
	if _, err := graph.Start(context.Background(), "writer-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Start(context.Background(), "writer-b"); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("second writer error = %v, want ErrWriterBusy", err)
	}
	if decision, _ := graph.Readiness(context.Background(), "writer-b"); decision.Reason != ReadyWriterBusy || decision.Writer != "writer-a" {
		t.Fatalf("writer-b readiness = %#v", decision)
	}
	if _, err := graph.Start(context.Background(), "reader-a"); err != nil {
		t.Fatalf("reader could not run beside writer: %v", err)
	}
	if _, err := graph.Start(context.Background(), "reader-b"); err != nil {
		t.Fatalf("parallel reader could not start: %v", err)
	}
}

func TestFailedDependencyRequiresExplicitRetryAndProof(t *testing.T) {
	graph, clock := testGraph(t)
	if _, err := graph.Start(context.Background(), "explore"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	failure := FailureReceipt{Attempt: 1, Reason: "temporary provider failure", Retryable: true}
	if err := graph.Fail(context.Background(), "explore", failure); err != nil {
		t.Fatal(err)
	}
	if err := graph.Fail(context.Background(), "explore", failure); err != nil {
		t.Fatalf("duplicate failure was not idempotent: %v", err)
	}
	if decision, _ := graph.Readiness(context.Background(), "implement"); decision.Reason != ReadyDependencyUnsettled {
		t.Fatalf("dependent readiness = %#v", decision)
	}
	clock.Advance(time.Second)
	if err := graph.Retry(context.Background(), "explore", "provider recovered"); err != nil {
		t.Fatal(err)
	}
	if admission, err := graph.Start(context.Background(), "explore"); err != nil {
		t.Fatal(err)
	} else if !admission.Started || admission.Attempt != 2 {
		t.Fatalf("retry admission = %#v", admission)
	}
	if err := graph.Complete(context.Background(), "explore", completion("late attempt-one completion")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale completion error = %v, want ErrConflict", err)
	}
	if err := graph.Fail(context.Background(), "explore", failure); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale failure error = %v, want ErrConflict", err)
	}
	current := snapshot(t, graph)
	if current.Units[0].State != StateRunning || current.Units[0].Attempt != 2 || current.Units[0].Reason != "" || current.Units[0].Failure != nil {
		t.Fatalf("retried unit projection = %#v", current.Units[0])
	}
}

func TestBlockCancelAndNonRetryableFailureTransitions(t *testing.T) {
	graph, clock := testGraph(t)
	if err := graph.Block(context.Background(), "explore", "awaiting a user decision"); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Start(context.Background(), "explore"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("blocked start error = %v", err)
	}
	clock.Advance(time.Second)
	if err := graph.Retry(context.Background(), "explore", "decision recorded"); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Start(context.Background(), "explore"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := graph.Fail(context.Background(), "explore", FailureReceipt{Attempt: 1, Reason: "permanent failure"}); err != nil {
		t.Fatal(err)
	}
	if err := graph.Retry(context.Background(), "explore", "try anyway"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("non-retryable failure retry error = %v", err)
	}
	if err := graph.Cancel(context.Background(), "verify", "goal changed"); err != nil {
		t.Fatal(err)
	}
	if err := graph.Cancel(context.Background(), "verify", "goal changed"); err != nil {
		t.Fatalf("duplicate cancellation was not idempotent: %v", err)
	}
}

func TestSnapshotRoundTripAndForgeryRejection(t *testing.T) {
	graph, clock := testGraph(t)
	if _, err := graph.Start(context.Background(), "explore"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	original := snapshot(t, graph)
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(decoded, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, restored); got.ID != original.ID || got.Units[0].State != StateRunning {
		t.Fatalf("restored snapshot = %#v", got)
	}

	forged := original
	forged.Units = append([]Unit(nil), original.Units...)
	forged.Units[1].State = StateRunning
	forged.Units[1].Attempt = 1
	forged.Units[1].StartedAt = original.UpdatedAt
	if _, err := Restore(forged, WithClock(clock)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("multiple-writer/dependency forgery error = %v", err)
	}

	forged = original
	forged.Version++
	if _, err := Restore(forged, WithClock(clock)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("version forgery error = %v", err)
	}

	forged = cloneSnapshot(original)
	forged.Units[0].StartedAt = forged.UpdatedAt.Add(time.Nanosecond)
	if _, err := Restore(forged, WithClock(clock)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future unit timestamp forgery error = %v", err)
	}

	forged = cloneSnapshot(original)
	forged.Units[0].Reason = "forged running explanation"
	if _, err := Restore(forged, WithClock(clock)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("running reason forgery error = %v", err)
	}

	behind := &fakeClock{now: original.UpdatedAt.Add(-time.Nanosecond)}
	if _, err := Restore(original, WithClock(behind)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future snapshot forgery error = %v", err)
	}
}

func TestRestoreRejectsForgedRetryProjectionAndAttemptOverflow(t *testing.T) {
	graph, clock := testGraph(t)
	original := snapshot(t, graph)

	forged := cloneSnapshot(original)
	forged.Units[0].Attempt = 1
	if _, err := Restore(forged, WithClock(clock)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing retry reason forgery error = %v", err)
	}

	forged = cloneSnapshot(original)
	forged.Units[0].Attempt = math.MaxInt64
	forged.Units[0].Reason = "recovered an extreme durable retry projection"
	restored, err := Restore(forged, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Start(context.Background(), "explore"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("attempt overflow error = %v", err)
	}
	if current := snapshot(t, restored).Units[0]; current.Attempt != math.MaxInt64 || current.State != StateQueued {
		t.Fatalf("attempt overflow mutated projection: %#v", current)
	}
}

func TestCancelledAndNilContextsFailClosed(t *testing.T) {
	graph, _ := testGraph(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := graph.Start(ctx, "explore"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Start() error = %v", err)
	}
	//nolint:staticcheck // Deliberately exercise the public nil-context rejection contract.
	if _, err := graph.Snapshot(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Snapshot() error = %v", err)
	}
	if state := snapshot(t, graph).Units[0].State; state != StateQueued {
		t.Fatalf("cancelled context mutated state to %s", state)
	}
}

func TestContextCancelledWhileWaitingForGraphLockDoesNotMutate(t *testing.T) {
	graph, _ := testGraph(t)
	graph.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := graph.Start(ctx, "explore")
		result <- err
	}()
	cancel()
	graph.mu.Unlock()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() after lock-wait cancellation error = %v", err)
	}
	if state := snapshot(t, graph).Units[0].State; state != StateQueued {
		t.Fatalf("cancelled lock waiter mutated state to %s", state)
	}
}

func TestConcurrentReadinessAndSnapshots(t *testing.T) {
	graph, _ := testGraph(t)
	var group sync.WaitGroup
	for range 32 {
		group.Add(2)
		go func() {
			defer group.Done()
			if _, err := graph.Ready(context.Background()); err != nil {
				t.Errorf("Ready() error = %v", err)
			}
		}()
		go func() {
			defer group.Done()
			if _, err := graph.Snapshot(context.Background()); err != nil {
				t.Errorf("Snapshot() error = %v", err)
			}
		}()
	}
	group.Wait()
}

func TestConcurrentAdmissionIssuesOneDispatchAuthorityAndOneWriter(t *testing.T) {
	graph, _ := testGraph(t)
	const callers = 32
	results := make(chan Admission, callers)
	errorsSeen := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			admission, err := graph.Start(context.Background(), "explore")
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- admission
		}()
	}
	group.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent duplicate admission error = %v", err)
	}
	started := 0
	for admission := range results {
		if admission.Attempt != 1 {
			t.Fatalf("concurrent admission = %#v", admission)
		}
		if admission.Started {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("fresh dispatch authorities = %d, want exactly 1", started)
	}

	first := unit("writer-a", RoleImplementer, EffectWriter)
	second := unit("writer-b", RoleImplementer, EffectWriter)
	spec := testSpec()
	spec.Units = []UnitSpec{first, second}
	writers, err := New(spec, WithClock(testClock()))
	if err != nil {
		t.Fatal(err)
	}
	type writerResult struct {
		admission Admission
		err       error
	}
	writerResults := make(chan writerResult, 2)
	for _, id := range []string{"writer-a", "writer-b"} {
		group.Add(1)
		go func() {
			defer group.Done()
			admission, startErr := writers.Start(context.Background(), id)
			writerResults <- writerResult{admission: admission, err: startErr}
		}()
	}
	group.Wait()
	close(writerResults)
	writerStarted := 0
	writerBusy := 0
	for result := range writerResults {
		switch {
		case result.err == nil && result.admission.Started:
			writerStarted++
		case errors.Is(result.err, ErrWriterBusy):
			writerBusy++
		default:
			t.Fatalf("unexpected concurrent writer result = %#v", result)
		}
	}
	if writerStarted != 1 || writerBusy != 1 {
		t.Fatalf("concurrent writers started=%d busy=%d", writerStarted, writerBusy)
	}
}
