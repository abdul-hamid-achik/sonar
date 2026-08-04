package agent

import (
	"errors"
	"fmt"
)

var (
	ErrTurnEvalBudgetExhausted   = errors.New("turn evaluation-token budget exhausted")
	ErrTurnContextBudgetExceeded = errors.New("turn context budget exceeded")
	ErrEmptyTerminalResponse     = errors.New("provider returned an empty terminal assistant response")
	ErrMalformedToolLoop         = errors.New("model repeatedly returned malformed tool requests")
	ErrRepeatedHostRefusal       = errors.New("model repeatedly submitted an identical request refused by the approval host")
)

const maxIdenticalHostRefusals = 2

// RepeatedHostRefusalError stops an impossible approval loop without
// misreporting the host's refusal as a user denial.
type RepeatedHostRefusalError struct {
	ToolName      string
	ArgumentsHash string
	Code          string
	Attempts      int
}

func (e *RepeatedHostRefusalError) Error() string {
	if e == nil {
		return ErrRepeatedHostRefusal.Error()
	}
	return fmt.Sprintf("%v: tool=%q arguments=%s code=%q attempts=%d; change the request or approval renderer before retrying",
		ErrRepeatedHostRefusal, e.ToolName, e.ArgumentsHash, e.Code, e.Attempts)
}

func (e *RepeatedHostRefusalError) Unwrap() error { return ErrRepeatedHostRefusal }

// TurnContextBudgetError reports that a bounded turn cannot safely make its
// next provider request without exceeding the active model's context window.
// Bounded turns may not launch an unaccounted summarization generation, so the
// host must compact or replace history before retrying.
type TurnContextBudgetError struct {
	EstimatedPromptTokens int
	ContextWindowTokens   int
}

func (e *TurnContextBudgetError) Error() string {
	if e == nil {
		return ErrTurnContextBudgetExceeded.Error()
	}
	return fmt.Sprintf(
		"%v: estimated prompt uses %d of %d tokens; reduce prompt or tool context, compact history, or switch to a model with a larger context window, then retry",
		ErrTurnContextBudgetExceeded, e.EstimatedPromptTokens, e.ContextWindowTokens,
	)
}

func (e *TurnContextBudgetError) Unwrap() error {
	return ErrTurnContextBudgetExceeded
}
