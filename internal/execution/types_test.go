package execution

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRandomIDsArePrefixedAndDistinct(t *testing.T) {
	tests := []struct {
		prefix string
		makeID func() (string, error)
	}{
		{"run_", NewRunID},
		{"turn_", NewTurnID},
		{"exec_", NewExecutionID},
		{"idem_", NewIdempotencyKey},
	}
	for _, test := range tests {
		first, err := test.makeID()
		if err != nil {
			t.Fatal(err)
		}
		second, err := test.makeID()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(first, test.prefix) || len(first) != len(test.prefix)+32 {
			t.Fatalf("id %q does not have expected %q + 128-bit form", first, test.prefix)
		}
		if first == second {
			t.Fatalf("generated duplicate %s IDs", test.prefix)
		}
	}
}

func TestHashCanonicalArgumentsIgnoresMapOrder(t *testing.T) {
	left, err := HashCanonicalArguments(map[string]any{
		"path":    "notes.md",
		"options": map[string]any{"force": true, "mode": float64(2)},
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := HashCanonicalArguments(map[string]any{
		"options": map[string]any{"mode": float64(2), "force": true},
		"path":    "notes.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("canonical hashes differ: %q != %q", left, right)
	}
	if _, err := HashCanonicalArguments(map[string]any{"bad": math.Inf(1)}); err == nil {
		t.Fatal("non-JSON argument unexpectedly hashed")
	}
}

func TestBoundTextPreservesUTF8AndLimits(t *testing.T) {
	receipt := BoundResultReceipt(strings.Repeat("界", MaxResultReceiptBytes))
	if len(receipt) > MaxResultReceiptBytes || !utf8.ValidString(receipt) {
		t.Fatalf("bounded receipt bytes=%d valid=%v", len(receipt), utf8.ValidString(receipt))
	}
	detail := BoundDetail(strings.Repeat("x", MaxDetailBytes+100))
	if len(detail) > MaxDetailBytes || !strings.HasSuffix(detail, "...[truncated]") {
		t.Fatalf("bounded detail bytes=%d suffix=%q", len(detail), detail[len(detail)-20:])
	}
}

func TestEventValidationRejectsRawShapeViolations(t *testing.T) {
	identity := Identity{
		SessionID: 1, WorkspaceID: "/tmp/work", RunID: "run-test", TurnID: "turn-test",
		ExecutionID: "exec-test", IdempotencyKey: "idem-test",
		CanonicalCallID: "call-1", ToolName: "read", Iteration: 1,
		Ordinal: 1, Kind: KindBuiltin, EffectClass: EffectReadOnly,
	}
	event := Event{
		Identity: identity, Type: EventRequested,
		Approval: ApprovalNotApplicable, ArgumentsSHA256: HashText("{}"),
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	event.ArgumentsSHA256 = strings.ToUpper(event.ArgumentsSHA256)
	if err := event.Validate(); err == nil {
		t.Fatal("uppercase digest unexpectedly accepted")
	}
	event.ArgumentsSHA256 = HashText("{}")
	event.ResultReceipt = strings.Repeat("x", MaxResultReceiptBytes+1)
	if err := event.Validate(); err == nil {
		t.Fatal("oversized receipt unexpectedly accepted")
	}
}
