package goal

import (
	"regexp"
	"testing"
)

// A goal ID keys the durable goal record that survives a restart, and it was
// the one exported function in this package with no test anywhere in the
// repository. A collision would merge two goals' budgets, criteria and
// receipts into one row.
//
// The audit that found it also corrected its own premise: goal has a low
// test-to-code ratio, which suggested a thin package, but checking the
// exported surface directly turned up exactly this one gap. The ratio pointed
// at the right place for the wrong reason.
func TestGoalIDsAreDistinctAndWellFormed(t *testing.T) {
	shape := regexp.MustCompile(`^goal_[0-9a-f]{32}$`)
	seen := make(map[string]struct{}, 512)
	for i := 0; i < 512; i++ {
		id, err := NewGoalID()
		if err != nil {
			t.Fatalf("generate goal id: %v", err)
		}
		if !shape.MatchString(id) {
			t.Fatalf("goal id %q does not match goal_ + 32 hex characters", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("generated %q twice in 512 draws", id)
		}
		seen[id] = struct{}{}
	}
}
