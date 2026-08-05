package agent

import (
	"strings"
	"testing"
)

// Four of six approval prompts in a measured AUTO session were ordinary
// searches — `grep -rl "ollama" internal/ui`, `find . -name '*.go'` — refused
// with "arguments outside the host catalog". That is true and useless: it names
// a rule without naming a way forward, so the model re-sends the same shell
// command and collects another prompt.
//
// The refusal is correct and stays. Raw recursive search bypasses the workspace
// ignore policy, which is exactly what the built-in grep, glob, ls and read
// tools enforce — and those tools take the same arguments the model was
// reaching for.
func TestSearchRefusalNamesTheToolThatWouldWork(t *testing.T) {
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())

	for _, command := range []string{
		`grep -rl "ollama" internal/ui`,
		`grep -rli "ollama" internal/ui --include='*.go'`,
		`find . -name '*.go'`,
		`rg --no-ignore pattern`,
		`tree -a .`,
		`ls -Ra .`,
		`du -sh .`,
	} {
		reason := ag.autoCommandApprovalReason(AuthorityAutoScoped, command)
		if reason == "" {
			t.Fatalf("raw recursive search was admitted, bypassing the ignore policy: %s", command)
		}
		if !strings.Contains(reason, "grep") || !strings.Contains(reason, "ignore policy") {
			t.Errorf("refusal for %q gives no remedy: %q", command, reason)
		}
	}
}

// The remedy must not leak onto refusals it does not apply to. A command
// refused for its arguments has no built-in equivalent to redirect to.
func TestOtherRefusalsKeepTheirOwnReason(t *testing.T) {
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())

	substitution := ag.autoCommandApprovalReason(AuthorityAutoScoped, "go build $(pwd)")
	if !strings.Contains(substitution, "dynamic shell syntax") {
		t.Errorf("a command substitution reported %q", substitution)
	}
	if strings.Contains(substitution, "ignore policy") {
		t.Errorf("the search remedy leaked onto a substitution refusal: %q", substitution)
	}

	uncatalogued := ag.autoCommandApprovalReason(AuthorityAutoScoped, "definitely-not-a-command --flag")
	if !strings.Contains(uncatalogued, "executable outside") {
		t.Errorf("an unknown executable reported %q", uncatalogued)
	}
}

// The label is operator-facing and lands in the approval modal and the durable
// ledger, so it stays one bounded line of host-authored text.
func TestRemedyLabelIsBoundedHostText(t *testing.T) {
	label := autoCommandReasonLabel(autoCommandReasonHostToolAvailable, "grep -r x .")
	if strings.ContainsAny(label, "\n\r\t") {
		t.Errorf("label spans lines: %q", label)
	}
	if len(label) > 160 {
		t.Errorf("label is %d bytes", len(label))
	}
	if strings.Contains(label, "ollama") || strings.Contains(label, "grep -r x") {
		t.Errorf("label echoed the command: %q", label)
	}
}
