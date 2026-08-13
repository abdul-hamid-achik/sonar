package agent

import "testing"

// A real session ran four bash_output calls against a still-running background
// command and every one of them was refused as a duplicate read. The
// duplicate-suppression guard treats a read-only built-in as a pure function of
// its arguments until some tool changes state — true for read, grep and ls, and
// false for a poll against a process running outside the tool loop, which is
// the entire purpose of bash_output.
//
// Asking the same question again is the only correct way to ask it.
func TestBackgroundPollIsExemptFromDuplicateSuppression(t *testing.T) {
	for _, name := range []string{"bash_output", "agent_output"} {
		if !builtinResultVariesOverTime(name) {
			t.Errorf("%s is still treated as a repeatable pure read", name)
		}
	}
}

// The exemption must stay narrow. Suppression is what stops a model
// re-reading the same file every iteration, and widening it would return that
// loop.
func TestOrdinaryReadsStayDeduplicated(t *testing.T) {
	for _, name := range []string{"read", "grep", "ls", "glob", "find", "diff", "bash", "write", "edit"} {
		if builtinResultVariesOverTime(name) {
			t.Errorf("%q was exempted from duplicate suppression", name)
		}
	}
}
