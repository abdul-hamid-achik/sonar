package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

func TestAutoSegmentStateRequiresNewBoundedProgress(t *testing.T) {
	started := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	var state AutoSegmentState
	state.Reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)

	first := &AutoIterationCheckpointError{ProgressDigest: "digest-a"}
	if err := state.Admit("turn-root", first, started.Add(time.Minute)); err != nil {
		t.Fatalf("first checkpoint: %v", err)
	}
	if err := state.Admit("turn-root", first, started.Add(2*time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "repeated") {
		t.Fatalf("repeated checkpoint error = %v", err)
	}
}

func TestAutoSegmentStateBoundsSegmentsAndElapsedTime(t *testing.T) {
	started := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	var state AutoSegmentState
	state.Reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)
	for index := 0; index < config.DefaultAutoMaxSegments; index++ {
		checkpoint := &AutoIterationCheckpointError{ProgressDigest: string(rune('a' + index))}
		if err := state.Admit("turn-root", checkpoint, started.Add(time.Duration(index+1)*time.Minute)); err != nil {
			t.Fatalf("checkpoint %d: %v", index, err)
		}
	}
	if err := state.Admit("turn-root", &AutoIterationCheckpointError{ProgressDigest: "overflow"}, started.Add(20*time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "segment") {
		t.Fatalf("segment ceiling error = %v", err)
	}

	state.Reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)
	if err := state.Admit("turn-root", &AutoIterationCheckpointError{ProgressDigest: "late"}, started.Add(config.DefaultAutoMaxWallTime)); err == nil ||
		!strings.Contains(err.Error(), "time budget") {
		t.Fatalf("elapsed ceiling error = %v", err)
	}
}

func TestAutoSegmentStateAllowsRepeatedEffectfulWork(t *testing.T) {
	started := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	var state AutoSegmentState
	state.Reset("turn-root", started, config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime)

	first := &AutoIterationCheckpointError{ProgressDigest: "build-test-set", EffectfulSuccessfulCalls: 2}
	if err := state.Admit("turn-root", first, started.Add(time.Minute)); err != nil {
		t.Fatalf("first segment refused: %v", err)
	}
	if err := state.Admit("turn-root", first, started.Add(2*time.Minute)); err != nil {
		t.Fatalf("repeated effectful set refused: %v", err)
	}
}

func TestApplyDefaultAutoTurnLimitsBoundsOnlyUnboundedAuto(t *testing.T) {
	unbounded := TurnLimits{}
	if got := ApplyDefaultAutoTurnLimits(unbounded, true, config.DefaultAutoMaxWallTime); got.MaxWallTime != config.DefaultAutoMaxWallTime {
		t.Fatalf("plain AUTO wall ceiling = %v, want %v", got.MaxWallTime, config.DefaultAutoMaxWallTime)
	}
	if got := ApplyDefaultAutoTurnLimits(unbounded, false, config.DefaultAutoMaxWallTime); got != (TurnLimits{}) {
		t.Fatalf("non-AUTO turn was bounded: %#v", got)
	}
	goalBounded := TurnLimits{MaxEvalTokens: 500}
	if got := ApplyDefaultAutoTurnLimits(goalBounded, true, config.DefaultAutoMaxWallTime); got != goalBounded {
		t.Fatalf("already-bounded AUTO turn was rewritten: %#v", got)
	}
}

func TestNormalizeLogicalTurnLimitsDoesNotRebase(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	limits := NormalizeLogicalTurnLimits(TurnLimits{MaxWallTime: 5 * time.Minute}, now)
	if limits.Deadline != now.Add(5*time.Minute) || limits.MaxWallTime != 0 {
		t.Fatalf("normalized limits = %#v", limits)
	}
	again := NormalizeLogicalTurnLimits(limits, now.Add(time.Minute))
	if again.Deadline != limits.Deadline {
		t.Fatalf("deadline rebased across segments: %v -> %v", limits.Deadline, again.Deadline)
	}
}

func TestContinueAutoLimitsPreservesRemainingEvalBudget(t *testing.T) {
	next, err := ContinueAutoLimits(TurnLimits{MaxEvalTokens: 100}, &AutoIterationCheckpointError{EvalTokens: 40})
	if err != nil || next.MaxEvalTokens != 60 {
		t.Fatalf("remaining = %#v err=%v", next, err)
	}
	if _, err := ContinueAutoLimits(TurnLimits{MaxEvalTokens: 40}, &AutoIterationCheckpointError{EvalTokens: 40}); err == nil {
		t.Fatal("exhausted eval budget was admitted")
	}
}
