package supervisor

import (
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

// AgentTurnLimits converts a goal's remaining durable budget into hard
// host-owned turn limits. Every controller that dispatches a goal turn — TUI
// or headless — must pass these limits so a single turn cannot overrun the
// goal's evaluation-token or wall-time budget and only settle the overrun
// after the fact.
func AgentTurnLimits(snapshot goal.Snapshot, now time.Time) (agent.TurnLimits, error) {
	limits := agent.TurnLimits{}
	if snapshot.Budget.MaxEvalTokens > 0 {
		remaining := snapshot.Budget.MaxEvalTokens - snapshot.Usage.EvalTokens
		if remaining <= 0 {
			return limits, goal.ErrBudgetExhausted
		}
		limits.MaxEvalTokens = remaining
	}
	if snapshot.Budget.MaxWallTime > 0 {
		deadline := snapshot.CreatedAt.Add(snapshot.Budget.MaxWallTime)
		if !now.Before(deadline) {
			return limits, goal.ErrBudgetExhausted
		}
		limits.Deadline = deadline
	}
	return limits, nil
}
