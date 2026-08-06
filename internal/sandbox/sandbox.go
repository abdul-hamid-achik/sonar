// Package sandbox confines a subprocess with the operating system's own
// primitives, so that a boundary sonar states is one the kernel enforces
// rather than one a catalog promises.
//
// It exists because the shell-admission catalog in internal/agent can only
// answer questions about argv. That is enough to keep `curl` and `rm -rf /`
// out of an unattended turn, and it is structurally unable to say anything
// about what a program does once it starts: `go test ./...` and `npm test`
// run workspace-defined code, and a workspace script can open a socket or
// read a file the catalog never saw a name for. Those two layers answer
// different questions and neither replaces the other.
//
// # What is enforced
//
//   - Reads of host secret paths and of anything the workspace ignore policy
//     excludes are denied.
//   - Writes are denied everywhere except the workspace and the toolchain
//     caches a build legitimately needs.
//   - Network access is denied unless the policy grants it.
//
// # What is not
//
// The workspace is writable, so a confined `rm -rf .` still destroys
// uncommitted work. The sandbox protects everything outside the workspace
// from the command; it cannot protect the workspace from itself, and the
// approval layer remains the only thing that can. Deleting the catalog
// because a sandbox exists would trade a broad, verified boundary for a
// narrower one.
//
// # Why the profile denies rather than allows
//
// The reference implementation this was measured against (Anthropic's
// sandbox-runtime) opens with `(deny default)` and then re-allows a long,
// curated list of Mach services, sysctls and IOKit classes derived from
// Chrome's sandbox policy — 611 lines on macOS alone, plus vendored seccomp
// binaries on Linux. That is the price of a default-deny posture, and it is
// a maintenance surface that Apple can invalidate.
//
// This profile starts from `(allow default)` and denies the three operation
// classes that carry the threat: file reads of named secrets, file writes
// outside the workspace, and network. A deny rule in SBPL is enforced by the
// kernel regardless of the default, so those three are as strong here as
// they are there. Everything outside those classes — Mach lookups, sysctls,
// IPC — stays permitted, and that is the honest cost: this confines what a
// command can READ, WRITE and REACH, not everything it can do.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ErrUnsupported reports that this platform has no confinement sonar can
// apply. Callers must decide whether to run unconfined or refuse; the package
// never silently downgrades, because a sandbox that quietly does nothing is
// worse than none at all — it moves a decision from the operator to a stub.
var ErrUnsupported = errors.New("sandbox: no supported confinement on this platform")

// Policy is the confinement one command runs under. Every path is absolute and
// symlink-resolved by Normalize before it reaches a profile: macOS matches
// Seatbelt subpaths against the resolved path, so an unresolved /tmp or
// /var/folders rule silently matches nothing — which is how a permissive
// profile passes its own tests while protecting nothing.
type Policy struct {
	// Workspace is the single tree the command may write to.
	Workspace string

	// WritablePaths are the additional roots a toolchain needs in order to
	// work at all: the Go build and module caches, TMPDIR, /dev. Without them
	// `go build` fails on its work directory rather than on anything the
	// operator asked to protect.
	WritablePaths []string

	// UnreadablePaths are exact roots denied for reading.
	UnreadablePaths []string

	// UnreadableComponents deny any path having a component that matches, at
	// any depth. `.env` and `id_rsa` are the shape: they are conventions about
	// a NAME, not about a location, and a policy expressed as exact paths
	// would have to walk the workspace on every command to find them — which
	// is both slow and racy against a file created a second later.
	//
	// Platform support is not symmetric and pretending otherwise would be the
	// dangerous kind of tidy. macOS enforces these directly as Seatbelt
	// regexes. Linux mount namespaces cannot express "any path matching a
	// pattern", so bubblewrap covers UnreadablePaths only and a component rule
	// there protects nothing — the catalog remains the layer that stops
	// `cat .env` on Linux, exactly as it does today.
	UnreadableComponents []string

	// ReadableLeaves are exact names admitted despite matching an unreadable
	// component, and only when they are the FINAL component of a path. The
	// public .env templates are the case: a repository may read .env.example,
	// and a directory carrying that name still hides its descendants.
	ReadableLeaves []string

	// AllowNetwork lifts the network denial for this one command.
	//
	// It is per-command rather than global on purpose. sonar's own provider
	// calls happen in the sonar process, never in a confined child, so a shell
	// that cannot reach the network costs inference nothing. What it does cost
	// is `npm install` and `go mod download`, which are exactly the commands
	// the catalog already sends through an approval — so the grant that lets
	// one run can carry its network with it.
	AllowNetwork bool
}

// Normalize resolves and de-duplicates every path, dropping the ones that do
// not resolve. It is idempotent and safe to call more than once.
func (p Policy) Normalize() Policy {
	normalized := Policy{AllowNetwork: p.AllowNetwork}
	normalized.Workspace = resolvePath(p.Workspace)
	normalized.WritablePaths = resolvePaths(p.WritablePaths)
	normalized.UnreadablePaths = resolvePaths(p.UnreadablePaths)
	normalized.UnreadableComponents = append([]string(nil), p.UnreadableComponents...)
	sort.Strings(normalized.UnreadableComponents)
	normalized.ReadableLeaves = append([]string(nil), p.ReadableLeaves...)
	sort.Strings(normalized.ReadableLeaves)
	return normalized
}

// Valid reports whether the policy can be enforced. A policy with no
// workspace would confine a command to writing nowhere, which is not a
// sandbox but a broken one, and it fails closed here rather than at exec.
func (p Policy) Valid() bool {
	return strings.TrimSpace(p.Workspace) != "" && filepath.IsAbs(p.Workspace)
}

func resolvePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute)
}

// resolvePaths resolves, drops empties, and sorts. Sorting makes a generated
// profile deterministic, which is what lets a test assert one.
func resolvePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		normalized := resolvePath(path)
		if normalized == "" {
			continue
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		resolved = append(resolved, normalized)
	}
	sort.Strings(resolved)
	return resolved
}

// WorkspacePolicy builds the policy an ordinary confined command runs under:
// the workspace is writable, the toolchain's caches are writable, the named
// secrets are unreadable, and the network is denied.
//
// The cache list is not a convenience. A profile that allows only the
// workspace makes `go build` fail on its own work directory, `npm` fail on its
// cache, and every one of those failures looks like a broken toolchain rather
// than a policy decision — which is how an operator concludes the sandbox is
// unusable and turns it off. Reading them from the environment rather than
// shelling out to `go env` keeps this on the per-command path.
func WorkspacePolicy(workspace string, unreadable, unreadableComponents, readableLeaves []string, allowNetwork bool) Policy {
	writable := []string{
		os.TempDir(),
		envOrDefault("GOCACHE", userCacheSubdir("go-build")),
		envOrDefault("GOMODCACHE", filepath.Join(envOrDefault("GOPATH", homeSubdir("go")), "pkg", "mod")),
		envOrDefault("XDG_CACHE_HOME", homeSubdir(".cache")),
		envOrDefault("CARGO_HOME", homeSubdir(".cargo")),
		homeSubdir(".npm"),
		homeSubdir(".bun/install/cache"),
	}
	return Policy{
		Workspace:            workspace,
		WritablePaths:        writable,
		UnreadablePaths:      unreadable,
		UnreadableComponents: unreadableComponents,
		ReadableLeaves:       readableLeaves,
		AllowNetwork:         allowNetwork,
	}.Normalize()
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func homeSubdir(elements ...string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(append([]string{home}, elements...)...)
}

func userCacheSubdir(name string) string {
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		return ""
	}
	return filepath.Join(cache, name)
}

// Apply confines an already-prepared command by rewriting only what it
// executes.
//
// Rewriting in place rather than building a replacement is the whole point of
// this signature. A *exec.Cmd carries state the caller established and some of
// which cannot be copied at all: a command built by exec.CommandContext holds
// an unexported context, and assigning its Cancel onto a fresh exec.Command
// makes Go refuse to start with "command with a non-nil Cancel was not created
// with CommandContext". The first version of this package did exactly that,
// and the resulting failure was invisible in the tests that mattered — every
// denial assertion passed because nothing ran at all.
//
// So confinement changes Path and Args and touches nothing else. Directory,
// environment, streams, process group, wait delay and cancellation stay
// exactly as the caller set them.
func Apply(policy Policy, cmd *exec.Cmd) error {
	if cmd == nil {
		return fmt.Errorf("sandbox: no command to confine")
	}
	name, args, err := resolve(policy, cmd.Path, cmd.Args[1:])
	if err != nil {
		return err
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("sandbox: confinement driver %q is unavailable: %w", name, err)
	}
	cmd.Path = path
	cmd.Args = append([]string{name}, args...)
	return nil
}

// Wrap returns a fresh command that runs name+args under the policy. It suits
// a caller that owns no prior command state; anything holding a prepared
// *exec.Cmd wants Apply instead.
func Wrap(policy Policy, name string, args []string) (*exec.Cmd, error) {
	wrappedName, wrappedArgs, err := resolve(policy, name, args)
	if err != nil {
		return nil, err
	}
	return exec.Command(wrappedName, wrappedArgs...), nil
}

func resolve(policy Policy, name string, args []string) (string, []string, error) {
	policy = policy.Normalize()
	if !policy.Valid() {
		return "", nil, fmt.Errorf("sandbox: policy has no workspace root")
	}
	if !Available() {
		return "", nil, ErrUnsupported
	}
	return wrapCommand(policy, name, args)
}

// containedBy reports whether path is root or sits beneath it. Both are
// expected to be resolved already.
func containedBy(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
