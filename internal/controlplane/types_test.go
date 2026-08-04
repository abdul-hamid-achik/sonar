package controlplane

import (
	"strings"
	"testing"
)

func TestMarshalDocumentAndValidation(t *testing.T) {
	document, digest, err := MarshalDocument(map[string]any{
		"answer": true,
		"source": "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Item{
		ItemID: "ctrl_1", IdempotencyKey: "idem_1",
		Kind:        KindCortexDecision,
		Identity:    Identity{SessionID: 7, WorkspaceID: "/workspace", GoalID: "goal_1"},
		Summary:     "Choose the recovery strategy",
		PayloadJSON: document, PayloadSHA256: digest,
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("valid item: %v", err)
	}

	item.PayloadSHA256 = HashText(`{"different":true}`)
	if err := item.Validate(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("hash mismatch error = %v", err)
	}
	if _, _, err := MarshalDocument([]string{"not", "an", "object"}); err == nil {
		t.Fatal("top-level array unexpectedly accepted")
	}
}

func TestKindSpecificValidationAndOutcomeCompatibility(t *testing.T) {
	document, digest, err := MarshalDocument(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	item := Item{
		ItemID: "ctrl_1", IdempotencyKey: "idem_1",
		Kind:     KindExecutionReconciliation,
		Identity: Identity{SessionID: 7, WorkspaceID: "/workspace"},
		Summary:  "Reconcile execution", PayloadJSON: document, PayloadSHA256: digest,
	}
	if err := item.Validate(); err == nil || !strings.Contains(err.Error(), "requires an execution id") {
		t.Fatalf("missing execution error = %v", err)
	}
	if OutcomeDismissed.ValidFor(KindExecutionReconciliation) {
		t.Fatal("execution reconciliation can be dismissed")
	}
	if !OutcomeReconciled.ValidFor(KindExecutionReconciliation) {
		t.Fatal("reconciled outcome rejected")
	}
	if !OutcomeApproved.ValidFor(KindDeferredApproval) || OutcomeAnswered.ValidFor(KindDeferredApproval) {
		t.Fatal("deferred approval compatibility is incorrect")
	}
}

func TestQueryIsAlwaysBoundedAndScoped(t *testing.T) {
	valid := Query{SessionID: 9, WorkspaceID: "/workspace", PendingOnly: true, Limit: MaxListLimit}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []Query{
		{SessionID: 0, WorkspaceID: "/workspace", Limit: 1},
		{SessionID: 9, WorkspaceID: "", Limit: 1},
		{SessionID: 9, WorkspaceID: "/workspace", Limit: 0},
		{SessionID: 9, WorkspaceID: "/workspace", Limit: MaxListLimit + 1},
		{SessionID: 9, WorkspaceID: "/workspace", Kind: Kind("unknown"), Limit: 1},
	} {
		if err := query.Validate(); err == nil {
			t.Fatalf("invalid query unexpectedly accepted: %#v", query)
		}
	}
}
