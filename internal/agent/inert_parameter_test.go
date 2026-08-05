package agent

import "testing"

// In one AUTO session, half the approval prompts were dynamic-syntax refusals,
// and the commands behind them were ordinary verification runs — `go build
// ./... 2>&1 | head -30; echo "exit: $?"`. The other half were `$(…)`.
//
// `$?`, `$$`, `$#` and `$!` are fixed by POSIX to expand to a decimal integer.
// None can produce a command, a path, or a program name, which is the property
// the dynamic-syntax rule exists to enforce. Admitting them is a widening with
// a proof behind it, not a convenience list.
func TestInertParametersNoLongerDemandApproval(t *testing.T) {
	for _, command := range []string{
		`go build ./... 2>&1 | head -30; echo "exit: $?"`,
		"go test ./... ; echo $?",
		"echo $$",
		"echo $#",
		"echo $!",
		// The value can be adjacent to literal text and still be a number.
		`echo "status=$?"`,
		// Two in one command, and one inside quotes.
		`go vet ./... ; echo $? ; echo "pid $$"`,
	} {
		if hasDynamicShellSyntax(command) {
			t.Errorf("still refused as dynamic syntax: %s", command)
		}
	}
}

// The widening must not reach anything that can run a command or name a path.
// This is the half of the evidence that stays refused.
func TestCommandSubstitutionAndExpansionStayRefused(t *testing.T) {
	for _, command := range []string{
		// The other real command from the same session.
		"for d in $(go list ./...); do echo $d; done",
		"go build $(pwd)",
		"echo `date`",
		"cat ${HOME}/.ssh/config",
		"echo $HOME",
		"echo $PATH",
		"echo ${x:-$(whoami)}",
		// Indirect expansion is not a bare integer.
		"echo ${!x}",
		// A subshell is a subshell whatever precedes it.
		"echo $? ; (rm -rf /)",
		// The inert form must not become a bridge: `$` then `$(` is still a
		// substitution, and consuming `$$` must not swallow the opener.
		"echo $$(whoami)",
		`echo "$?$(whoami)"`,
		// A lone trailing `$` has nothing proven about it.
		"echo $",
	} {
		if !hasDynamicShellSyntax(command) {
			t.Errorf("admitted despite being able to run a command: %s", command)
		}
	}
}

// The refusal label must name the token that actually caused it. With `$?`
// admitted, a command carrying both would otherwise blame the harmless one and
// send the reader looking in the wrong place.
func TestRefusalNamesTheRealTrigger(t *testing.T) {
	token, found := firstDynamicShellSyntaxToken(`echo "exit: $?" && go build $(pwd)`)
	if !found {
		t.Fatal("a command substitution was not detected at all")
	}
	if token == "$?" {
		t.Errorf("the refusal blamed the admitted parameter instead of the substitution")
	}
	if token != "$" {
		t.Errorf("token = %q, want the substitution's $", token)
	}
}

// End to end: the measured command is admitted by the whole assessment, not
// just by the scanner, and the substitution is not.
func TestMeasuredCommandsAssessAsExpected(t *testing.T) {
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())

	admitted := `go build ./... 2>&1 | head -30; echo "exit: $?"`
	if reason := ag.autoCommandApprovalReason(AuthorityAutoScoped, admitted); reason == "dynamic shell syntax ($?)" {
		t.Errorf("the measured verification command is still refused for $?: %q", reason)
	}
	refused := "for d in $(go list ./...); do echo $d; done"
	if reason := ag.autoCommandApprovalReason(AuthorityAutoScoped, refused); reason == "" {
		t.Error("a command substitution was admitted under AUTO")
	}
}
