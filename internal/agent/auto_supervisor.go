package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// AutoSegmentState is the host-owned ceiling on a complete AUTO conversational
// turn. The agent loop emits ErrAutoIterationCheckpoint at the per-segment
// watchdog; this state decides whether a fresh segment may start.
type AutoSegmentState struct {
	LogicalTurnID     string
	StartedAt         time.Time
	SegmentsContinued int
	LastDigest        string
	MaxSegments       int
	MaxElapsed        time.Duration
}

func (s *AutoSegmentState) Reset(logicalTurnID string, startedAt time.Time, maxSegments int, maxElapsed time.Duration) {
	if maxSegments <= 0 {
		maxSegments = config.DefaultAutoMaxSegments
	}
	if maxElapsed <= 0 {
		maxElapsed = config.DefaultAutoMaxWallTime
	}
	*s = AutoSegmentState{
		LogicalTurnID: strings.TrimSpace(logicalTurnID),
		StartedAt:     startedAt,
		MaxSegments:   maxSegments,
		MaxElapsed:    maxElapsed,
	}
}

func (s *AutoSegmentState) SegmentCeiling() int {
	if s == nil || s.MaxSegments <= 0 {
		return config.DefaultAutoMaxSegments
	}
	return s.MaxSegments
}

func (s *AutoSegmentState) ElapsedCeiling() time.Duration {
	if s == nil || s.MaxElapsed <= 0 {
		return config.DefaultAutoMaxWallTime
	}
	return s.MaxElapsed
}

func (s *AutoSegmentState) Clear() {
	*s = AutoSegmentState{}
}

func (s *AutoSegmentState) Admit(
	logicalTurnID string,
	checkpoint *AutoIterationCheckpointError,
	now time.Time,
) error {
	if s == nil || checkpoint == nil {
		return errors.New("checkpoint receipt is unavailable")
	}
	if strings.TrimSpace(logicalTurnID) == "" || logicalTurnID != s.LogicalTurnID {
		return errors.New("checkpoint does not belong to the active turn")
	}
	digest := strings.TrimSpace(checkpoint.ProgressDigest)
	if digest == "" {
		return errors.New("checkpoint has no progress identity")
	}
	if digest == s.LastDigest && checkpoint.EffectfulSuccessfulCalls == 0 {
		return errors.New("the last AUTO segment repeated without new progress")
	}
	if s.SegmentsContinued >= s.SegmentCeiling() {
		return fmt.Errorf("the %d-segment AUTO continuation budget was exhausted", s.SegmentCeiling())
	}
	if !s.StartedAt.IsZero() && now.Sub(s.StartedAt) >= s.ElapsedCeiling() {
		return fmt.Errorf("the %s AUTO continuation time budget was exhausted", s.ElapsedCeiling())
	}
	s.SegmentsContinued++
	s.LastDigest = digest
	return nil
}

// ApplyDefaultAutoTurnLimits gives an otherwise-unbounded AUTO turn the same
// wall ceiling the segment supervisor already enforces across segments.
func ApplyDefaultAutoTurnLimits(limits TurnLimits, auto bool, wallTime time.Duration) TurnLimits {
	if !auto {
		return limits
	}
	if limits.MaxEvalTokens > 0 || !limits.Deadline.IsZero() || limits.MaxWallTime > 0 {
		return limits
	}
	if wallTime <= 0 {
		wallTime = config.DefaultAutoMaxWallTime
	}
	limits.MaxWallTime = wallTime
	return limits
}

// NormalizeLogicalTurnLimits converts a relative wall limit into an absolute
// deadline so AUTO segments do not restart the clock.
func NormalizeLogicalTurnLimits(limits TurnLimits, now time.Time) TurnLimits {
	if limits.MaxWallTime <= 0 {
		return limits
	}
	deadline := now.Add(limits.MaxWallTime)
	if limits.Deadline.IsZero() || deadline.Before(limits.Deadline) {
		limits.Deadline = deadline
	}
	limits.MaxWallTime = 0
	return limits
}

// ContinueAutoLimits subtracts a finished segment's eval receipt from the
// logical turn budget. A non-positive remainder stops the chain.
func ContinueAutoLimits(limits TurnLimits, checkpoint *AutoIterationCheckpointError) (TurnLimits, error) {
	if checkpoint == nil {
		return limits, errors.New("checkpoint receipt is unavailable")
	}
	if remaining := limits.MaxEvalTokens; remaining > 0 {
		remaining -= checkpoint.EvalTokens
		if remaining <= 0 {
			return limits, errors.New("the logical turn evaluation-token budget was exhausted")
		}
		limits.MaxEvalTokens = remaining
	}
	return limits, nil
}
