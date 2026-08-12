package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
)

type headlessTurnRunner interface {
	RunTurnWithOptions(ctx context.Context, out agent.Output, turnID string, options agent.TurnOptions) error
	AutoTurnCeilings() (segments int, wallTime time.Duration)
}

// runHeadlessAutoChain runs one headless turn. When chain is true it honors
// tools.auto_max_segments and tools.auto_max_wall_time the same way the TUI
// AUTO supervisor does. Goal turns pass chain=false: Goal Runtime owns
// their continuation.
func runHeadlessAutoChain(
	ctx context.Context,
	runner headlessTurnRunner,
	out agent.Output,
	turnID string,
	options agent.TurnOptions,
	chain bool,
	now time.Time,
	afterSegment func(segmentTurnID string, checkpoint *agent.AutoIterationCheckpointError) error,
) (string, error) {
	if !chain {
		return turnID, runner.RunTurnWithOptions(ctx, out, turnID, options)
	}
	segments, wall := runner.AutoTurnCeilings()
	options.Limits = agent.ApplyDefaultAutoTurnLimits(options.Limits, true, wall)
	options.Limits = agent.NormalizeLogicalTurnLimits(options.Limits, now)
	var state agent.AutoSegmentState
	state.Reset(turnID, now, segments, wall)
	logicalID := turnID
	currentID := turnID
	for {
		err := runner.RunTurnWithOptions(ctx, out, currentID, options)
		var checkpoint *agent.AutoIterationCheckpointError
		if !errors.As(err, &checkpoint) {
			return currentID, err
		}
		now = time.Now()
		if admitErr := state.Admit(logicalID, checkpoint, now); admitErr != nil {
			return currentID, fmt.Errorf("AUTO continuation stopped: %w", admitErr)
		}
		nextLimits, limitsErr := agent.ContinueAutoLimits(options.Limits, checkpoint)
		if limitsErr != nil {
			return currentID, fmt.Errorf("AUTO continuation stopped: %w", limitsErr)
		}
		if afterSegment != nil {
			if hookErr := afterSegment(currentID, checkpoint); hookErr != nil {
				return currentID, fmt.Errorf("AUTO continuation stopped: %w", hookErr)
			}
		}
		nextID, idErr := executionpkg.NewTurnID()
		if idErr != nil {
			return currentID, fmt.Errorf("AUTO continuation identity: %w", idErr)
		}
		options.Limits = nextLimits
		options.Continuation = nil
		currentID = nextID
	}
}
