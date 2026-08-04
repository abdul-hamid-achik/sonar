package supervisor

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
)

// MaxIssues matches the bounded durable control-plane projection. A caller
// cannot bypass that read bound by constructing an Observation directly.
const MaxIssues = controlplane.MaxListLimit

// IssuesFromControlStates adapts the dependency-free durable control-plane
// projection to the smaller scheduling vocabulary. Resolved items confer no
// pending stop condition and are deliberately omitted. Input order is
// preserved so Decision.IssueIDs remains a stable projection of the bounded
// store query.
func IssuesFromControlStates(states []controlplane.State) ([]Issue, error) {
	if len(states) > MaxIssues {
		return nil, fmt.Errorf("%w: control-plane projection exceeds %d items", ErrInvalid, MaxIssues)
	}
	issues := make([]Issue, 0, len(states))
	seen := make(map[string]struct{}, len(states))
	for index, state := range states {
		if err := state.Item.Validate(); err != nil {
			return nil, fmt.Errorf("%w: control-plane item %d: %v", ErrInvalid, index, err)
		}
		if strings.TrimSpace(state.Item.ItemID) != state.Item.ItemID {
			return nil, fmt.Errorf("%w: control-plane item %d has a non-canonical id", ErrInvalid, index)
		}
		if _, exists := seen[state.Item.ItemID]; exists {
			return nil, fmt.Errorf("%w: duplicate control-plane item id %q", ErrInvalid, state.Item.ItemID)
		}
		seen[state.Item.ItemID] = struct{}{}
		if !state.Pending() {
			if err := state.Resolution.Validate(); err != nil {
				return nil, fmt.Errorf("%w: control-plane resolution %d: %v", ErrInvalid, index, err)
			}
			if state.Resolution.ItemID != state.Item.ItemID ||
				state.Resolution.SessionID != state.Item.Identity.SessionID ||
				state.Resolution.WorkspaceID != state.Item.Identity.WorkspaceID ||
				!state.Resolution.Outcome.ValidFor(state.Item.Kind) {
				return nil, fmt.Errorf("%w: control-plane resolution %d does not match its item", ErrInvalid, index)
			}
			continue
		}

		var kind IssueKind
		switch state.Item.Kind {
		case controlplane.KindCortexDecision:
			kind = IssueDecision
		case controlplane.KindDeferredApproval:
			kind = IssueApproval
		case controlplane.KindExecutionReconciliation:
			kind = IssueOutcomeUnknown
		default:
			return nil, fmt.Errorf("%w: unsupported control-plane item kind %q", ErrInvalid, state.Item.Kind)
		}
		issues = append(issues, Issue{
			ID: state.Item.ItemID, Kind: kind, Summary: state.Item.Summary,
		})
	}
	return issues, nil
}
