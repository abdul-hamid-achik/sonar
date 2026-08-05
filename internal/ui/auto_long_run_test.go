package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// An unattended AUTO job of several hours could not be configured at any value
// of tools.auto_max_iterations, because that setting bounds one provider
// segment while the whole-turn ceiling — 8 segments and 90 minutes — was a host
// constant that silently overrode it. Both dimensions are now settings, and
// this asserts a long ceiling is actually honoured rather than merely accepted
// by the config loader.
func TestAutoSupervisorHonoursALongConfiguredCeiling(t *testing.T) {
	started := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	var supervisor autoCheckpointSupervisor
	supervisor.reset("turn-long", started, 40, 6*time.Hour)

	// Well past the former 8-segment and 90-minute constants.
	for index := 0; index < 40; index++ {
		checkpoint := &agent.AutoIterationCheckpointError{
			ProgressDigest:           "segment-" + string(rune('a'+index%26)) + string(rune('a'+index/26)),
			EffectfulSuccessfulCalls: 1,
		}
		at := started.Add(time.Duration(index+1) * 5 * time.Minute)
		if err := supervisor.admit("turn-long", checkpoint, at); err != nil {
			t.Fatalf("segment %d at %v was refused under a 6h/40-segment ceiling: %v", index, at.Sub(started), err)
		}
	}

	// The ceiling still ends the run; a raised budget is not an absent one.
	overflow := &agent.AutoIterationCheckpointError{ProgressDigest: "one-too-many", EffectfulSuccessfulCalls: 1}
	if err := supervisor.admit("turn-long", overflow, started.Add(4*time.Hour)); err == nil ||
		!strings.Contains(err.Error(), "segment") {
		t.Fatalf("the configured segment ceiling did not end the run: %v", err)
	}
}

// Raising the ceiling must not weaken the stall guard. A model repeating the
// same read-only work set is the failure a long budget would otherwise let run
// for hours, and it is the reason the budget cannot simply be removed.
func TestALongCeilingStillStopsAStalledRun(t *testing.T) {
	started := time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC)
	var supervisor autoCheckpointSupervisor
	supervisor.reset("turn-long", started, 400, 12*time.Hour)

	first := &agent.AutoIterationCheckpointError{ProgressDigest: "same-work"}
	if err := supervisor.admit("turn-long", first, started.Add(time.Minute)); err != nil {
		t.Fatalf("first segment refused: %v", err)
	}
	repeat := &agent.AutoIterationCheckpointError{ProgressDigest: "same-work"}
	if err := supervisor.admit("turn-long", repeat, started.Add(2*time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "without new progress") {
		t.Fatalf("a stalled run survived a long ceiling: %v", err)
	}
}

// An unconfigured harness must behave exactly as it did before these became
// settings, or this change is a silent cost increase for every existing user.
func TestUnconfiguredAutoCeilingsMatchTheFormerConstants(t *testing.T) {
	if config.DefaultAutoMaxSegments != 8 {
		t.Errorf("default segments = %d, want the former constant 8", config.DefaultAutoMaxSegments)
	}
	if config.DefaultAutoMaxWallTime != 90*time.Minute {
		t.Errorf("default wall time = %v, want the former constant 90m", config.DefaultAutoMaxWallTime)
	}

	var zero autoCheckpointSupervisor
	if got := zero.segmentCeiling(); got != config.DefaultAutoMaxSegments {
		t.Errorf("a zero-valued supervisor allows %d segments, want the default %d", got, config.DefaultAutoMaxSegments)
	}
	if got := zero.elapsedCeiling(); got != config.DefaultAutoMaxWallTime {
		t.Errorf("a zero-valued supervisor allows %v, want the default %v", got, config.DefaultAutoMaxWallTime)
	}

	// reset must also fall back, so a caller that cannot resolve config does
	// not accidentally hand the supervisor an unbounded turn.
	var fallback autoCheckpointSupervisor
	fallback.reset("turn", time.Now(), 0, 0)
	if fallback.segmentCeiling() != config.DefaultAutoMaxSegments || fallback.elapsedCeiling() != config.DefaultAutoMaxWallTime {
		t.Error("reset with zero ceilings did not fall back to the defaults")
	}
}

// The configured wall time must reach the turn limits too. Bounding the
// supervisor alone would let the segment chain continue while the agent's own
// deadline cancelled the turn underneath it.
func TestConfiguredWallTimeReachesThePlainAutoTurnLimit(t *testing.T) {
	got := defaultPlainAutoTurnLimits(agent.TurnLimits{}, ModeAuto, 6*time.Hour)
	if got.MaxWallTime != 6*time.Hour {
		t.Fatalf("plain AUTO wall ceiling = %v, want the configured 6h", got.MaxWallTime)
	}
	// An explicitly bounded caller (a goal) keeps its own budget.
	bounded := agent.TurnLimits{MaxWallTime: time.Minute}
	if got := defaultPlainAutoTurnLimits(bounded, ModeAuto, 6*time.Hour); got.MaxWallTime != time.Minute {
		t.Fatalf("an explicit caller budget was overridden: %v", got.MaxWallTime)
	}
}
