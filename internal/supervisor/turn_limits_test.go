package supervisor

import (
	"errors"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

func TestAgentTurnLimitsBoundsRemainingBudget(t *testing.T) {
	createdAt := time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC)
	snapshot := goal.Snapshot{
		CreatedAt: createdAt,
		Budget:    goal.BudgetLimits{MaxEvalTokens: 12_000, MaxWallTime: 30 * time.Minute},
		Usage:     goal.BudgetUsage{EvalTokens: 4_500},
	}
	limits, err := AgentTurnLimits(snapshot, createdAt.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxEvalTokens != 7_500 {
		t.Fatalf("MaxEvalTokens = %d, want the remaining 7500", limits.MaxEvalTokens)
	}
	if want := createdAt.Add(30 * time.Minute); !limits.Deadline.Equal(want) {
		t.Fatalf("Deadline = %s, want the immutable goal deadline %s", limits.Deadline, want)
	}
}

func TestAgentTurnLimitsRejectsExhaustedBudgets(t *testing.T) {
	createdAt := time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC)
	evalExhausted := goal.Snapshot{
		CreatedAt: createdAt,
		Budget:    goal.BudgetLimits{MaxEvalTokens: 1_000},
		Usage:     goal.BudgetUsage{EvalTokens: 1_000},
	}
	if _, err := AgentTurnLimits(evalExhausted, createdAt); !errors.Is(err, goal.ErrBudgetExhausted) {
		t.Fatalf("eval exhaustion error = %v, want goal.ErrBudgetExhausted", err)
	}
	wallExhausted := goal.Snapshot{
		CreatedAt: createdAt,
		Budget:    goal.BudgetLimits{MaxWallTime: 10 * time.Minute},
	}
	if _, err := AgentTurnLimits(wallExhausted, createdAt.Add(10*time.Minute)); !errors.Is(err, goal.ErrBudgetExhausted) {
		t.Fatalf("wall exhaustion error = %v, want goal.ErrBudgetExhausted", err)
	}
}

func TestAgentTurnLimitsUnboundedGoalYieldsZeroLimits(t *testing.T) {
	limits, err := AgentTurnLimits(goal.Snapshot{CreatedAt: time.Now()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxEvalTokens != 0 || !limits.Deadline.IsZero() || limits.MaxWallTime != 0 {
		t.Fatalf("unbounded goal produced limits %+v", limits)
	}
}
