package supervisor

import (
	"errors"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
)

func supervisorControlItem(t *testing.T, id, summary string, kind controlplane.Kind) controlplane.Item {
	t.Helper()
	payload, digest, err := controlplane.MarshalDocument(map[string]any{"source": "supervisor-test"})
	if err != nil {
		t.Fatal(err)
	}
	item := controlplane.Item{
		ItemID: id, IdempotencyKey: "idem_" + id, Kind: kind,
		Identity: controlplane.Identity{SessionID: 42, WorkspaceID: "/workspace"},
		Summary:  summary, PayloadJSON: payload, PayloadSHA256: digest,
	}
	if kind == controlplane.KindExecutionReconciliation {
		item.Identity.ExecutionID = "exec_" + id
	}
	return item
}

func supervisorControlResolution(t *testing.T, item controlplane.Item) *controlplane.Resolution {
	t.Helper()
	evidence, digest, err := controlplane.MarshalDocument(map[string]any{"receipt": "resolved"})
	if err != nil {
		t.Fatal(err)
	}
	return &controlplane.Resolution{
		ResolutionID:   "resolution_" + item.ItemID,
		IdempotencyKey: "resolution_idem_" + item.ItemID,
		ItemID:         item.ItemID, SessionID: item.Identity.SessionID,
		WorkspaceID: item.Identity.WorkspaceID, Outcome: controlplane.OutcomeAnswered,
		EvidenceJSON: evidence, EvidenceSHA256: digest, ResolvedBy: "supervisor-test",
	}
}

func TestIssuesFromControlStatesMapsPendingAndSkipsResolved(t *testing.T) {
	decision := supervisorControlItem(t, "decision_1", "Choose the migration", controlplane.KindCortexDecision)
	approval := supervisorControlItem(t, "approval_1", "Approve the write", controlplane.KindDeferredApproval)
	reconciliation := supervisorControlItem(t, "outcome_1", "Reconcile the backend receipt", controlplane.KindExecutionReconciliation)
	resolved := supervisorControlItem(t, "resolved_1", "Already answered", controlplane.KindCortexDecision)
	states := []controlplane.State{
		{Item: decision},
		{Item: resolved, Resolution: supervisorControlResolution(t, resolved)},
		{Item: approval},
		{Item: reconciliation},
	}

	issues, err := IssuesFromControlStates(states)
	if err != nil {
		t.Fatal(err)
	}
	want := []Issue{
		{ID: decision.ItemID, Kind: IssueDecision, Summary: decision.Summary},
		{ID: approval.ItemID, Kind: IssueApproval, Summary: approval.Summary},
		{ID: reconciliation.ItemID, Kind: IssueOutcomeUnknown, Summary: reconciliation.Summary},
	}
	if len(issues) != len(want) {
		t.Fatalf("issues = %#v, want %#v", issues, want)
	}
	for index := range want {
		if issues[index] != want[index] {
			t.Fatalf("issue %d = %#v, want %#v", index, issues[index], want[index])
		}
	}
}

func TestIssuesFromControlStatesRejectsForgedOrUnboundedProjection(t *testing.T) {
	item := supervisorControlItem(t, "decision_1", "Choose", controlplane.KindCortexDecision)
	oversized := make([]controlplane.State, MaxIssues+1)
	if _, err := IssuesFromControlStates(oversized); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized projection error = %v", err)
	}

	duplicate := []controlplane.State{{Item: item}, {Item: item}}
	if _, err := IssuesFromControlStates(duplicate); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate projection error = %v", err)
	}

	invalid := item
	invalid.PayloadSHA256 = controlplane.HashText(`{"different":true}`)
	if _, err := IssuesFromControlStates([]controlplane.State{{Item: invalid}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid item error = %v", err)
	}

	nonCanonical := item
	nonCanonical.ItemID = " decision_1 "
	if _, err := IssuesFromControlStates([]controlplane.State{{Item: nonCanonical}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-canonical id error = %v", err)
	}

	forgedResolved := controlplane.State{Item: item, Resolution: supervisorControlResolution(t, item)}
	forgedResolved.Resolution.ItemID = "different_item"
	if _, err := IssuesFromControlStates([]controlplane.State{forgedResolved}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged resolved projection error = %v", err)
	}
}
