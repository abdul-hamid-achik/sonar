package main

import (
	"context"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/supervisor"
)

// headlessSupervisorIssues projects the session's pending durable control
// items into the supervisor's scheduling vocabulary. The bounded store query
// and the adapter's validation both fail closed: an unreadable control plane
// stops dispatch instead of running past an unresolved approval or effect.
func headlessSupervisorIssues(ctx context.Context, store *db.Store, sessionID int64, workspaceID string) ([]supervisor.Issue, error) {
	states, err := store.ListControlStates(ctx, controlplane.Query{
		SessionID:   sessionID,
		WorkspaceID: workspaceID,
		PendingOnly: true,
		Limit:       supervisor.MaxIssues,
	})
	if err != nil {
		return nil, err
	}
	return supervisor.IssuesFromControlStates(states)
}

// receiptDecision projects a supervisor decision into the bounded receipt
// field. The goal snapshot inside the decision stays out of the receipt; the
// goal store owns that state.
func receiptDecision(decision supervisor.Decision) *agent.TurnReceiptDecision {
	return &agent.TurnReceiptDecision{
		Action:   string(decision.Action),
		Reason:   string(decision.Reason),
		Detail:   decision.Detail,
		IssueIDs: append([]string(nil), decision.IssueIDs...),
	}
}
