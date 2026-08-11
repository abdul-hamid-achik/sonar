package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Orienting with git is the most common thing an agent does before touching
// anything. Excluding it outright cost an interactive approval every time and,
// because dispatch preserves model order, stalled every read-only tool queued
// behind it — 2m15s in one measured session.
func TestGitReadSubcommandsAreAutoScoped(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--short"},
		{"log", "--oneline", "-3"},
		{"log", "-p", "--stat"},
		{"show", "HEAD"},
		{"show", "HEAD:internal/agent/agent.go"},
		{"diff", "--stat"},
		{"diff", "HEAD~1", "HEAD"},
		{"rev-parse", "--abbrev-ref", "HEAD"},
		{"ls-files"},
		{"blame", "internal/agent/agent.go"},
		{"describe", "--tags"},
		{"shortlog", "-sn"},
	} {
		if !autoScopedGitCommandAllowed(args) {
			t.Errorf("git %v should be auto-scoped", args)
		}
	}
}

// Each of these reads in one argument form and destroys in another. The verb is
// never catalogued, so the destructive form cannot ride in on the read's
// reputation.
//
// Four of them — branch, tag, stash, worktree — are additionally admitted by
// autoScopedGitListingAllowed in EXACT listing forms that carry no positional
// operand, which is how every mutating spelling below is reached. The bare and
// operand-bearing forms here must stay gated either way, and
// TestGitListingVerbsAreAdmittedOnlyInExactForms pins the other side.
func TestGitMutatingSubcommandsStayGated(t *testing.T) {
	// Bare `branch`/`tag` and `config --get` moved to the admitted side in the
	// session-4d01085 widening: the exact-form contract this file already
	// applies to listings — no positional operand, no mutation — covers them,
	// and refusing the most natural listing spelling cost real approvals.
	// Their operand-bearing spellings stay pinned refused below and in
	// TestGitOrientationWideningAdmitsNoMutation.
	for _, args := range [][]string{
		{"branch", "-D", "main"},
		{"tag", "v1.0.0"},
		{"config", "user.email", "x@y.z"},
		{"stash"}, // bare PUSHES the worktree
		{"stash", "push"},
		{"stash", "pop"},
		{"remote", "add", "origin", "git@example.com:x/y"},
		{"remote", "remove", "origin"},
		{"remote", "show", "origin"}, // reads, but contacts the remote
		{"checkout", "main"},
		{"switch", "-c", "feature"},
		{"restore", "."},
		{"reset", "--hard"},
		{"clean", "-fd"},
		{"commit", "-m", "x"},
		{"push"},
		{"pull"},
		{"fetch"},
		{"rebase", "-i", "HEAD~3"},
		{"merge", "main"},
		{"apply", "patch.diff"},
		{"rm", "file"},
		{"mv", "a", "b"},
		{"gc"},
		{"worktree", "add", "/tmp/wt"},
		{"submodule", "update", "--init"},
		{"difftool"},
		{"filter-branch"},
	} {
		if autoScopedGitCommandAllowed(args) {
			t.Errorf("git %v must stay approval-gated", args)
		}
	}
}

// A global option before the subcommand is the sharpest escalation available:
// `git -c diff.external=<program> diff` turns a read into arbitrary execution.
// Requiring args[0] to be a catalogued subcommand refuses all of them.
func TestGitGlobalOptionsBeforeSubcommandAreRefused(t *testing.T) {
	for _, args := range [][]string{
		{"-c", "diff.external=/tmp/evil", "diff"},
		{"-c", "core.pager=/tmp/evil", "log"},
		{"-C", "/etc", "status"},
		{"--git-dir=/other/.git", "log"},
		{"--work-tree=/", "status"},
		{"--exec-path=/tmp", "status"},
		{"--config-env=core.pager=EVIL", "log"},
		{"-p", "status"},
	} {
		if autoScopedGitCommandAllowed(args) {
			t.Errorf("git %v must stay approval-gated", args)
		}
	}
}

// Flags that make an otherwise-read subcommand write a file or run a program.
func TestGitWritingAndDelegatingFlagsAreRefused(t *testing.T) {
	for _, args := range [][]string{
		{"diff", "--output=/tmp/out"},
		{"diff", "--output", "/tmp/out"},
		{"show", "--output=/tmp/out"},
		{"diff", "--ext-diff"},
		// --textconv runs the config-named textconv filter. Caught by the
		// pre-existing suite, not by the first draft of this allowlist.
		{"diff", "--textconv"},
		{"log", "--textconv"},
		{"diff", "--textcon"},
		{"diff", "--extcmd=/tmp/evil"},
		{"diff", "--no-index", "/etc/passwd", "/etc/hosts"},
		{"diff", "-o", "/tmp/out"},
		{"diff", "-O", "/tmp/orderfile"},
		// Unique-prefix abbreviations must fail closed too.
		{"diff", "--out=/tmp/out"},
		{"diff", "--outp=/tmp/out"},
		{"diff", "--no-ind"},
	} {
		if autoScopedGitCommandAllowed(args) {
			t.Errorf("git %v must stay approval-gated", args)
		}
	}
}

// git accepts a short option's value attached to the letter, and the denylist
// above only ever recognized a denied option as its own token or =-joined.
// `git blame -S/etc/passwd` and `git log -O/etc/passwd` were therefore admitted:
// -S is blame's revs-file and -O is the diff/log/show order-file, both are
// really opened, and git echoes their content into diagnostics that become an
// unredacted tool result in the transcript and the durable session.
//
// One entry per short option any catalogued subcommand accepts with a value,
// plus the clustered forms that hide the letter behind a valueless one.
func TestGitAttachedShortOptionValuesAreRefused(t *testing.T) {
	for _, args := range [][]string{
		// The reproduced escapes.
		{"blame", "-S/etc/passwd", "--", "main.go"},
		{"log", "-O/etc/passwd"},
		// Every other value-taking short option, in attached form.
		{"diff", "-O/etc/passwd"},   // --output-indicator? no: order-file
		{"show", "-O/etc/passwd"},   // order-file
		{"diff", "-o/tmp/out"},      // --output: writes a file
		{"log", "-S/etc/passwd"},    // pickaxe string for log, revs-file for blame
		{"log", "-G/etc/passwd"},    // --find-object regex
		{"diff", "-I/etc/passwd"},   // --ignore-matching-lines regex
		{"log", "-L/etc/passwd"},    // line range
		{"blame", "-L/etc/passwd"},  // line range
		{"ls-files", "-X/etc/pass"}, // --exclude-from: a file
		{"ls-files", "-x/etc/pass"}, // --exclude pattern
		{"shortlog", "-w/etc/pass"}, // --wrap width
		{"status", "-u/etc/passwd"}, // --untracked-files mode
		{"diff", "-U/etc/passwd"},   // --unified
		{"blame", "-M/etc/passwd"},  // --find-renames
		{"blame", "-C/etc/passwd"},  // --find-copies
		{"diff", "-B/etc/passwd"},   // --break-rewrites
		{"diff", "-l/etc/passwd"},   // rename limit
		{"log", "-n/etc/passwd"},    // --max-count
		// The value hidden behind valueless letters in one POSIX cluster.
		{"blame", "-wS/etc/passwd"},
		{"log", "-psS/etc/passwd"},
		{"diff", "-wO/etc/passwd"},
		{"shortlog", "-snw/etc/passwd"},
		// A numeric-valued letter must not become a bridge to a path.
		{"diff", "-U3S/etc/passwd"},
		{"blame", "-M90%S/etc/passwd"},
		// A relative attached value is refused too. It cannot escape the
		// workspace, but -o would write and -O/-S would read a file the
		// operand loop never sees as an operand.
		{"blame", "-Srevs"},
		{"diff", "-oout"},
		// A traversal attached to the letter.
		{"blame", "-S../../etc/passwd"},
		{"log", "-O../../../etc/passwd"},
		// Fail closed after `--` as well: whether a later word is a pathspec
		// rather than an option is a per-subcommand claim about git's grammar,
		// and this guard exists because such claims have been wrong before.
		{"log", "--", "-S/etc/passwd"},
		// An unrecognized attached form is refused rather than guessed at.
		{"log", "-Zsomething"},
		{"status", "-uwhatever"},
	} {
		if autoScopedGitCommandAllowed(args) {
			t.Errorf("git %v must stay approval-gated", args)
		}
	}
}

// Gating all of git would be a regression: these are the ordinary read forms,
// including the attached ones that provably carry no filesystem path.
func TestGitSafeShortOptionFormsStayAutoScoped(t *testing.T) {
	for _, args := range [][]string{
		{"log", "--oneline", "-3"},   // bare commit count
		{"log", "-10"},               // two digits, still a count
		{"shortlog", "-sn"},          // cluster of valueless letters
		{"shortlog", "-sne"},         // -n is valueless here, -e follows it
		{"status", "-sb"},            // short + branch
		{"status", "-uno"},           // fixed untracked-files enum
		{"status", "-uall"},          //
		{"status", "-unormal"},       //
		{"status", "-suno"},          // the enum after a valueless letter
		{"diff", "-U3"},              // numeric context
		{"diff", "--stat", "-U10"},   //
		{"blame", "-M90%", "f.go"},   // similarity percentage
		{"diff", "-C75"},             //
		{"diff", "-B20"},             //
		{"log", "-n20"},              // attached max-count
		{"diff", "-l100"},            // attached rename limit
		{"blame", "-wp", "f.go"},     // ignore-whitespace + porcelain
		{"blame", "-l", "f.go"},      // -l is boolean for blame
		{"log", "-p", "--stat"},      //
		{"log", "-S", "needle"},      // detached pickaxe value
		{"blame", "-L", "1,10", "f"}, // detached line range
		{"ls-files", "-z"},           //
		{"diff", "-w", "--", "f.go"}, //
	} {
		if !autoScopedGitCommandAllowed(args) {
			t.Errorf("git %v should be auto-scoped", args)
		}
	}
}

// The helper decides the argument shape; this pins the whole admission path,
// because the escape needed both halves: the denylist missed the attached
// option AND the generic operand loop read `-S/etc/passwd` as a single relative
// word, joined it under the workspace, and reported it as inside.
func TestGitAttachedShortOptionValuesCannotEscapeTheWorkspaceInAuto(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	hostBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostBin, "git"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("install host git: %v", err)
	}
	t.Setenv("PATH", hostBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)

	for _, command := range []string{
		"git blame -S/etc/passwd -- main.go",
		"git log -O/etc/passwd",
		"git diff -O" + filepath.Join(os.Getenv("HOME"), ".ssh", "config"),
		"git blame -S /etc/passwd -- main.go",
		"git diff -O=/etc/passwd",
	} {
		t.Run(command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("command escaped the workspace under AUTO: %q", command)
			}
		})
	}
	for _, command := range []string{
		"git status -sb",
		"git log --oneline -3",
		"git diff -U3 -- internal",
		"git blame -wp main.go",
		"git status -uno",
	} {
		t.Run(command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); !assessment.admitted() {
				t.Fatalf("routine git read was not admitted in AUTO: %q", command)
			}
		})
	}
}

func TestGitEmptyAndUnknownArgsAreRefused(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{},
		{""},
		{"not-a-subcommand"},
		{"STATUS"}, // git subcommands are case-sensitive
	} {
		if autoScopedGitCommandAllowed(args) {
			t.Errorf("git %v must stay approval-gated", args)
		}
	}
}

// Session 4d01085 prompted for `git branch` and `git merge-base` — pure
// orientation reads. These pin the widened exact forms, and the refusal half
// pins that the widening admitted no operand-carrying (mutating) spelling.
func TestGitOrientationFormsAreAutoScoped(t *testing.T) {
	for _, args := range [][]string{
		{"merge-base", "main", "HEAD"},
		{"merge-base", "--is-ancestor", "main", "HEAD"},
		{"merge-base", "--fork-point", "origin/main"},
		{"branch"},
		{"tag"},
		{"branch", "--show-current"},
		{"config", "--list"},
		{"config", "--get", "user.name"},
		{"remote", "get-url", "origin"},
	} {
		if !autoScopedGitCommandAllowed(args) {
			t.Errorf("git %v should be auto-scoped", args)
		}
	}
}

func TestGitOrientationWideningAdmitsNoMutation(t *testing.T) {
	for _, args := range [][]string{
		// config writes through positional value or scoped flags.
		{"config", "user.name", "evil"},
		{"config", "--get", "--global", "user.name"},
		{"config", "--global", "--get", "user.name"},
		{"config", "--unset", "user.name"},
		// remote spellings that mutate or contact the network.
		{"remote", "get-url"},
		{"remote", "set-url", "origin", "https://example.com/x.git"},
		{"remote", "get-url", "--push"},
		// bare-form widening must not leak into operand forms.
		{"branch", "newthing"},
		{"tag", "v9.9"},
		{"branch", "--show-current", "extra"},
	} {
		if autoScopedGitCommandAllowed(args) {
			t.Errorf("git %v must stay approval-gated", args)
		}
	}
}
