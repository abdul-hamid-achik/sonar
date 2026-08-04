package agent

import (
	"fmt"
	"time"
)

// TurnLimits are hard, per-turn provider limits supplied by a host scheduler.
// Zero leaves a dimension unlimited. Goal Runtime passes only its remaining
// budget, so each provider iteration and every later tool dispatch fail closed.
type TurnLimits struct {
	MaxEvalTokens int64
	// Deadline is the immutable host admission deadline. Hosts that own an
	// absolute wall budget should set this instead of converting the deadline to
	// a duration before routing, persistence, or command scheduling work.
	Deadline time.Time
	// MaxWallTime is retained for callers that only have a relative per-turn
	// timeout. When both fields are set, the earlier deadline wins.
	MaxWallTime time.Duration
}

// TurnOptions binds hard limits and one optional host-owned capability
// activity to the same admitted turn. Keeping this data per-call avoids a
// mutable "next turn" setter surviving a cancelled preflight.
type TurnOptions struct {
	Limits       TurnLimits
	Capability   CapabilityActivity
	Continuation *ContinuationContext
}

func (limits TurnLimits) validate() error {
	if limits.MaxEvalTokens < 0 || limits.MaxWallTime < 0 {
		return fmt.Errorf("turn limits must not be negative")
	}
	return nil
}

func (limits TurnLimits) bounded() bool {
	return limits.MaxEvalTokens > 0 || !limits.Deadline.IsZero() || limits.MaxWallTime > 0
}

func (limits TurnLimits) effectiveDeadline(now time.Time) (time.Time, bool) {
	deadline := limits.Deadline
	if limits.MaxWallTime > 0 {
		relative := now.Add(limits.MaxWallTime)
		if deadline.IsZero() || relative.Before(deadline) {
			deadline = relative
		}
	}
	return deadline, !deadline.IsZero()
}
