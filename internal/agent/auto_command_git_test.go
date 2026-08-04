package agent

import "testing"

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

// Each of these reads in one argument form and destroys in another. Admitting
// the verb at all would mean the destructive form rides in on the read's
// reputation, so none of them are catalogued.
func TestGitMutatingSubcommandsStayGated(t *testing.T) {
	for _, args := range [][]string{
		{"branch", "-D", "main"},
		{"branch"}, // bare lists, but the verb also deletes
		{"tag"},    // bare lists, but `tag <name>` creates
		{"tag", "v1.0.0"},
		{"config", "user.email", "x@y.z"},
		{"config", "--get", "user.email"}, // reads, but the verb also writes
		{"stash"},                         // bare PUSHES the worktree
		{"stash", "list"},
		{"remote", "-v"},
		{"remote", "add", "origin", "git@example.com:x/y"},
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
