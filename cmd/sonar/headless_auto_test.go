package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/config"
)

type scriptedAutoRunner struct {
	ceilings int
	wall     time.Duration
	results  []error
	calls    []string
}

func (r *scriptedAutoRunner) AutoTurnCeilings() (int, time.Duration) {
	if r.ceilings == 0 {
		return config.DefaultAutoMaxSegments, config.DefaultAutoMaxWallTime
	}
	return r.ceilings, r.wall
}

func (r *scriptedAutoRunner) RunTurnWithOptions(_ context.Context, _ agent.Output, turnID string, _ agent.TurnOptions) error {
	r.calls = append(r.calls, turnID)
	if len(r.results) == 0 {
		return nil
	}
	err := r.results[0]
	r.results = r.results[1:]
	return err
}

func TestRunHeadlessAutoChainContinuesPastOneSegment(t *testing.T) {
	runner := &scriptedAutoRunner{
		ceilings: 2,
		wall:     time.Hour,
		results: []error{
			&agent.AutoIterationCheckpointError{
				ProgressDigest: "seg-1", SuccessfulToolCalls: 2, DistinctSuccessfulCalls: 2, ToolCalls: 2,
			},
			nil,
		},
	}
	out := agent.NewHeadlessOutput()
	finalID, err := runHeadlessAutoChain(
		context.Background(), runner, out, "turn_logical", agent.TurnOptions{}, true, time.Now(), nil,
	)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("segments run = %d, want 2: %#v", len(runner.calls), runner.calls)
	}
	if runner.calls[0] != "turn_logical" {
		t.Fatalf("first segment = %q", runner.calls[0])
	}
	if finalID == "" || finalID == "turn_logical" {
		t.Fatalf("final segment reused the logical turn id: %q", finalID)
	}
}

func TestRunHeadlessAutoChainDoesNotContinueWhenDisabled(t *testing.T) {
	runner := &scriptedAutoRunner{
		results: []error{&agent.AutoIterationCheckpointError{ProgressDigest: "seg-1"}},
	}
	_, err := runHeadlessAutoChain(
		context.Background(), runner, agent.NewHeadlessOutput(), "turn_one", agent.TurnOptions{}, false, time.Now(), nil,
	)
	if err == nil || !strings.Contains(err.Error(), "AUTO iteration checkpoint") {
		t.Fatalf("disabled chain error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("disabled chain ran %d segments", len(runner.calls))
	}
}

func TestRunHeadlessAutoChainStopsAtSegmentCeiling(t *testing.T) {
	checkpoint := &agent.AutoIterationCheckpointError{
		ProgressDigest: "same-read", SuccessfulToolCalls: 1, DistinctSuccessfulCalls: 1, ToolCalls: 1,
	}
	runner := &scriptedAutoRunner{
		ceilings: 1,
		wall:     time.Hour,
		results:  []error{checkpoint, checkpoint},
	}
	_, err := runHeadlessAutoChain(
		context.Background(), runner, agent.NewHeadlessOutput(), "turn_cap", agent.TurnOptions{}, true, time.Now(), nil,
	)
	if err == nil || !strings.Contains(err.Error(), "segment") {
		t.Fatalf("ceiling error = %v", err)
	}
}
