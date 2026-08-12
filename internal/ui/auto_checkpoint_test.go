package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

func TestGoalTurnAutoCheckpointSettlesThroughGoalRuntimeNotSupervisor(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	if _, err := m.goalRuntime.BeginTurn(context.Background(), "turn_goal_checkpoint", goal.AdmissionInitial); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return started.Add(time.Minute) }
	m.goalTurnID = "turn_goal_checkpoint"
	m.goalTurnToolCalls = 6
	m.goalTurnSuccesses = 5
	m.turnEvalTotal = 40
	m.state = StateStreaming
	// A live goal turn arms exactly this plain-AUTO supervisor state; the
	// checkpoint must still settle through the durable goal runtime.
	m.turnStartedAt = started
	m.turnLogicalID = "turn_goal_checkpoint"
	m.turnSegmentID = "turn_goal_checkpoint"
	m.turnAuthority = ModeAuto
	m.turnRunContext = context.Background()
	m.autoCheckpoints.reset("turn_goal_checkpoint", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)

	updated, _ := m.Update(AgentDoneMsg{
		TurnID: "turn_goal_checkpoint", SegmentTurnID: "turn_goal_checkpoint",
		Err: &agent.AutoIterationCheckpointError{
			TurnID: "turn_goal_checkpoint", Iterations: 40, ToolCalls: 6,
			SuccessfulToolCalls: 5, DistinctSuccessfulCalls: 5,
			ProgressDigest: "digest-goal-a", EvalTokens: 40,
		},
	})
	m = updated.(*Model)

	// A supervisor continuation would leave StateWaiting with a fresh segment
	// identity; goal settlement returns to idle and clears the segment.
	if m.state != StateIdle {
		t.Fatalf("goal checkpoint continued as plain AUTO: state=%v segment=%q", m.state, m.turnSegmentID)
	}
	if got := client.calls.Load(); got != 0 {
		t.Fatalf("goal checkpoint dispatched %d provider calls outside goal admission", got)
	}
	if m.goalTurnID != "" {
		t.Fatalf("goal turn did not settle: %q", m.goalTurnID)
	}
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	if snapshot.LastTurn == nil || !snapshot.LastTurn.Productive {
		t.Fatalf("productive AUTO checkpoint recorded as unproductive goal turn: %#v", snapshot.LastTurn)
	}
	if snapshot.PendingContinuation != nil {
		t.Fatalf("checkpoint settlement left the goal permit open: %#v", snapshot.PendingContinuation)
	}
	// Without linked Cortex evidence the runtime deliberately pauses a settled
	// productive turn for explicit review; it must never resume as plain AUTO.
	if snapshot.State != goal.StateActive && snapshot.State != goal.StatePaused {
		t.Fatalf("goal state after checkpoint settlement = %s", snapshot.State)
	}
}

func TestAutoCheckpointSupervisorRequiresNewBoundedProgress(t *testing.T) {
	started := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	var supervisor autoCheckpointSupervisor
	supervisor.reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)

	first := &agent.AutoIterationCheckpointError{ProgressDigest: "digest-a"}
	if err := supervisor.admit("turn-root", first, started.Add(time.Minute)); err != nil {
		t.Fatalf("first checkpoint: %v", err)
	}
	if err := supervisor.admit("turn-root", first, started.Add(2*time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "repeated") {
		t.Fatalf("repeated checkpoint error = %v", err)
	}
}

func TestAutoCheckpointSupervisorBoundsSegmentsAndElapsedTime(t *testing.T) {
	started := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	var supervisor autoCheckpointSupervisor
	supervisor.reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)
	for index := 0; index < config.DefaultAutoMaxSegments; index++ {
		checkpoint := &agent.AutoIterationCheckpointError{ProgressDigest: string(rune('a' + index))}
		if err := supervisor.admit("turn-root", checkpoint, started.Add(time.Duration(index+1)*time.Minute)); err != nil {
			t.Fatalf("checkpoint %d: %v", index, err)
		}
	}
	if err := supervisor.admit("turn-root", &agent.AutoIterationCheckpointError{ProgressDigest: "overflow"}, started.Add(20*time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "segment") {
		t.Fatalf("segment ceiling error = %v", err)
	}

	supervisor.reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)
	if err := supervisor.admit("turn-root", &agent.AutoIterationCheckpointError{ProgressDigest: "late"}, started.Add(config.DefaultAutoMaxWallTime)); err == nil ||
		!strings.Contains(err.Error(), "time budget") {
		t.Fatalf("elapsed ceiling error = %v", err)
	}
}

func TestDefaultPlainAutoTurnLimitsBoundsOnlyUnboundedAuto(t *testing.T) {
	unbounded := agent.TurnLimits{}
	if got := defaultPlainAutoTurnLimits(unbounded, ModeAuto, config.DefaultAutoMaxWallTime); got.MaxWallTime != config.DefaultAutoMaxWallTime {
		t.Fatalf("plain AUTO wall ceiling = %v, want %v", got.MaxWallTime, config.DefaultAutoMaxWallTime)
	}
	if got := defaultPlainAutoTurnLimits(unbounded, ModeNormal, config.DefaultAutoMaxWallTime); got != (agent.TurnLimits{}) {
		t.Fatalf("interactive NORMAL turn was bounded: %#v", got)
	}
	if got := defaultPlainAutoTurnLimits(unbounded, ModePlan, config.DefaultAutoMaxWallTime); got != (agent.TurnLimits{}) {
		t.Fatalf("interactive PLAN turn was bounded: %#v", got)
	}
	goalBounded := agent.TurnLimits{MaxEvalTokens: 500}
	if got := defaultPlainAutoTurnLimits(goalBounded, ModeAuto, config.DefaultAutoMaxWallTime); got != goalBounded {
		t.Fatalf("already-bounded AUTO turn was rewritten: %#v", got)
	}
	deadlineBounded := agent.TurnLimits{Deadline: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)}
	if got := defaultPlainAutoTurnLimits(deadlineBounded, ModeAuto, config.DefaultAutoMaxWallTime); got != deadlineBounded {
		t.Fatalf("deadline-bounded AUTO turn was rewritten: %#v", got)
	}
}

func TestAutoCheckpointSupervisorAllowsRepeatedEffectfulWork(t *testing.T) {
	started := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	var supervisor autoCheckpointSupervisor
	supervisor.reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)

	first := &agent.AutoIterationCheckpointError{ProgressDigest: "build-test-set", EffectfulSuccessfulCalls: 2}
	if err := supervisor.admit("turn-root", first, started.Add(time.Minute)); err != nil {
		t.Fatalf("first segment refused: %v", err)
	}
	// The same work set including verified effects (for example a repeated
	// build/test cycle) may legitimately recur inside the bounded budgets.
	repeatedEffectful := &agent.AutoIterationCheckpointError{ProgressDigest: "build-test-set", EffectfulSuccessfulCalls: 1}
	if err := supervisor.admit("turn-root", repeatedEffectful, started.Add(2*time.Minute)); err != nil {
		t.Fatalf("repeated effectful segment refused: %v", err)
	}
	// A read-only replay of the same set remains a stall.
	repeatedReadOnly := &agent.AutoIterationCheckpointError{ProgressDigest: "build-test-set"}
	if err := supervisor.admit("turn-root", repeatedReadOnly, started.Add(3*time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "repeated without new progress") {
		t.Fatalf("read-only replay admitted: %v", err)
	}
}

func TestNormalizeLogicalTurnLimitsDoesNotRebaseAcrossSegments(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	limits := normalizeLogicalTurnLimits(agent.TurnLimits{MaxWallTime: 5 * time.Minute}, now)
	if limits.MaxWallTime != 0 || !limits.Deadline.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("normalized limits = %#v", limits)
	}
}

func TestAgentDoneProductiveAutoCheckpointContinuesWithoutSettlement(t *testing.T) {
	m := newTestModel(t)
	started := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return started.Add(time.Minute) }
	m.state = StateStreaming
	m.turnStartedAt = started
	m.turnCheckpointSet = true
	m.turnLogicalID = "turn-root"
	m.turnSegmentID = "turn-root"
	m.turnAuthority = ModeAuto
	m.turnRunContext = context.Background()
	m.turnRunOptions = agent.TurnOptions{Limits: agent.TurnLimits{MaxEvalTokens: 100}}
	m.autoCheckpoints.reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)
	m.sessionTurnCount = 4
	cancelled := false
	m.cancel = func() { cancelled = true }

	updated, command := m.Update(AgentDoneMsg{
		TurnID: "turn-root", SegmentTurnID: "turn-root",
		Err: &agent.AutoIterationCheckpointError{
			ProgressDigest: "progress-a", EvalTokens: 25,
		},
	})
	m = updated.(*Model)
	if command == nil {
		t.Fatal("productive checkpoint did not schedule the next segment")
	}
	if m.state != StateWaiting || m.turnSegmentID == "turn-root" || m.turnLogicalID != "turn-root" {
		t.Fatalf("continuation identity/state = state %v logical %q segment %q", m.state, m.turnLogicalID, m.turnSegmentID)
	}
	if m.sessionTurnCount != 4 || !m.turnCheckpointSet || cancelled {
		t.Fatalf("checkpoint settled logical turn: count=%d checkpoint=%v cancelled=%v", m.sessionTurnCount, m.turnCheckpointSet, cancelled)
	}
	if got := m.turnRunOptions.Limits.MaxEvalTokens; got != 75 {
		t.Fatalf("remaining eval budget = %d, want 75", got)
	}
	for _, entry := range m.entries {
		if entry.Kind == "error" {
			t.Fatalf("productive checkpoint rendered as error: %#v", m.entries)
		}
	}
	receipt := false
	for _, entry := range m.entries {
		if entry.Kind == "system" && strings.Contains(entry.Content, "continuing segment 2") {
			receipt = true
			if strings.Contains(entry.Content, "/") && !strings.Contains(entry.Content, "tools ok") {
				t.Fatalf("segment receipt lost its counters grammar: %q", entry.Content)
			}
		}
	}
	if !receipt {
		t.Fatalf("continuation left no bounded segment receipt: %#v", m.entries)
	}
}

func TestAgentDoneRepeatedAutoCheckpointStopsAndSettles(t *testing.T) {
	m := newTestModel(t)
	started := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return started.Add(time.Minute) }
	m.state = StateStreaming
	m.turnStartedAt = started
	m.turnCheckpointSet = true
	m.turnLogicalID = "turn-root"
	m.turnSegmentID = "turn-segment"
	m.turnAuthority = ModeAuto
	m.turnRunContext = context.Background()
	m.autoCheckpoints.reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)
	m.autoCheckpoints.LastDigest = "same-progress"
	m.autoCheckpoints.SegmentsContinued = 1

	updated, command := m.Update(AgentDoneMsg{
		TurnID: "turn-root", SegmentTurnID: "turn-segment",
		Err: &agent.AutoIterationCheckpointError{ProgressDigest: "same-progress"},
	})
	m = updated.(*Model)
	if command != nil {
		t.Fatal("repeated checkpoint scheduled another segment")
	}
	if m.state != StateIdle || m.turnCheckpointSet {
		t.Fatalf("repeated checkpoint did not settle: state=%v checkpoint=%v", m.state, m.turnCheckpointSet)
	}
	found := false
	for _, entry := range m.entries {
		if entry.Kind == "error" && strings.Contains(entry.Content, "repeated without new progress") {
			found = true
		}
	}
	if !found {
		t.Fatalf("safe stop receipt missing: %#v", m.entries)
	}
}
