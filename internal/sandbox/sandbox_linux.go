package sandbox

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// bubblewrapPath is the Linux confinement driver. Unlike macOS's sandbox-exec
// it is not part of the base system, so Available() reports false on a machine
// without it and the caller decides what that means.
const bubblewrapName = "bwrap"

func Available() bool {
	path, err := exec.LookPath(bubblewrapName)
	if err != nil {
		return false
	}
	// LookPath alone is true once bubblewrap is installed, but a confined
	// command still has to START. Probe the filesystem-only shape: that is
	// enough for workspace binds and secret hides, and it is what GitHub
	// Actions can actually run. Network detachment is optional (see
	// networkNamespaceAvailable) and must not decide whether confinement
	// exists at all — otherwise Available is false on CI, every Linux test
	// skips, and the product claims a boundary nothing in the suite proves.
	cmd := exec.Command(path,
		"--die-with-parent",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--", "true",
	)
	return cmd.Run() == nil
}

// networkNamespaceAvailable reports whether --unshare-net can start a process.
//
// Detaching the network needs CAP_NET_ADMIN to bring loopback up inside the
// new namespace. Unprivileged hosts either grant that via a user-namespace
// uid-0 map, or refuse with RTM_NEWADDR: Operation not permitted (GitHub
// Actions). The mount confinement still holds without it; only the network
// half is dropped. Cached: wrapCommand asks on every command and the probe is
// a process start.
var (
	networkNamespaceOnce sync.Once
	networkNamespaceOK   bool
)

func networkNamespaceAvailable() bool {
	networkNamespaceOnce.Do(func() {
		path, err := exec.LookPath(bubblewrapName)
		if err != nil {
			return
		}
		// User-namespace uid 0 is the standard unprivileged shape for
		// CAP_NET_ADMIN inside the netns. Without it, --unshare-net fails on
		// hosts that created the namespace but cannot configure loopback.
		cmd := exec.Command(path,
			"--die-with-parent",
			"--unshare-user", "--uid", "0", "--gid", "0",
			"--ro-bind", "/", "/",
			"--dev", "/dev",
			"--proc", "/proc",
			"--unshare-net",
			"--", "true",
		)
		networkNamespaceOK = cmd.Run() == nil
	})
	return networkNamespaceOK
}

// wrapCommand builds a bubblewrap invocation.
//
// The construction is the inverse of the macOS profile and reaches the same
// three properties by a different route: rather than denying operations,
// bubblewrap builds a mount namespace in which the forbidden things are not
// present in a writable form.
//
//   - Everything is bind-mounted read-only, so writes fail by construction
//     rather than by rule.
//   - The workspace and the toolchain caches are re-bound read-write on top.
//   - Secret paths are covered: a directory by an empty tmpfs, a file by a
//     read-only bind of /dev/null. Either way there is nothing left to read,
//     even for a process that knows the exact path.
//   - --unshare-net removes the network namespace entirely. There is no
//     interface to reach, which is stronger than a filter and needs no proxy.
//
// --die-with-parent ties the sandbox to sonar's own lifetime, so a confined
// process cannot outlive the turn that started it.
func wrapCommand(policy Policy, name string, args []string) (string, []string, error) {
	wrapped := []string{
		"--die-with-parent",
		// Read-only view of the host, then the writable holes on top of it.
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/run",
	}

	// Every mount source must EXIST. bubblewrap aborts the whole sandbox on a
	// missing --bind source, where a Seatbelt rule for an absent path simply
	// never matches — so a policy that is merely redundant on macOS makes
	// nothing run at all here. WorkspacePolicy lists toolchain caches that a
	// given machine may not have (a container has no ~/.npm), which is exactly
	// how this reached Linux untested: on macOS those paths happened to exist.
	//
	// Dropping them is semantically free. A path that does not exist grants no
	// write and hides no secret.
	present := func(path string) bool {
		_, err := os.Lstat(path)
		return err == nil
	}

	// Hiding a secret takes a different mount depending on what it IS. tmpfs
	// is a directory mount and bubblewrap aborts the whole sandbox when asked
	// to lay one over a regular file — so a single secret FILE anywhere in the
	// policy meant nothing ran at all, and every denial assertion passed for
	// the emptiest of reasons. A file is covered by binding /dev/null over it,
	// which is the bubblewrap idiom: the path still resolves and reads as
	// empty, so the content is gone without the command failing in a way that
	// looks like a broken filesystem.
	hide := func(into []string, path string) []string {
		info, err := os.Lstat(path)
		if err != nil {
			return into
		}
		if info.IsDir() {
			return append(into, "--tmpfs", path)
		}
		return append(into, "--ro-bind", os.DevNull, path)
	}

	// A tmpfs over each secret leaves an empty directory where the file was.
	// This comes before the writable binds so a workspace-internal ignored
	// path cannot be re-exposed by the workspace bind that follows.
	secrets := append(append([]string(nil), policy.UnreadablePaths...),
		resolveSecretComponents(policy)...)
	for _, path := range secrets {
		wrapped = hide(wrapped, path)
	}

	wrapped = append(wrapped, "--bind", policy.Workspace, policy.Workspace)
	for _, path := range policy.WritablePaths {
		if containedBy(path, policy.Workspace) || !present(path) {
			continue
		}
		wrapped = append(wrapped, "--bind", path, path)
	}

	// Re-cover the secrets after the writable binds. A path under the
	// workspace would otherwise have just been re-mounted read-write, which
	// would make the .env the policy hid both readable and writable again.
	for _, path := range secrets {
		if containedBy(path, policy.Workspace) {
			wrapped = hide(wrapped, path)
		}
	}

	if !policy.AllowNetwork && networkNamespaceAvailable() {
		// --unshare-net creates an empty network namespace and bubblewrap then
		// brings loopback up inside it. That RTM_NEWADDR needs CAP_NET_ADMIN.
		// Mapping into a user namespace as uid 0 grants the cap only inside
		// the namespace — the host uid map still owns the files — which is the
		// standard unprivileged bubblewrap shape for a detached network.
		//
		// When the probe fails (GitHub Actions), mount confinement still
		// applies and the command still runs: dropping network isolation is
		// weaker than claiming Available false and skipping every Linux proof.
		wrapped = append(wrapped,
			"--unshare-user", "--uid", "0", "--gid", "0",
			"--unshare-net",
		)
	}

	// Keep the working directory the caller chose. bubblewrap resets it to /
	// otherwise, which silently changes what a relative path in the command
	// means — the sort of difference that turns a confined command into a
	// different command.
	wrapped = append(wrapped, "--chdir", policy.Workspace)

	wrapped = append(wrapped, "--", name)
	return bubblewrapName, append(wrapped, args...), nil
}

// maxSecretScanEntries bounds the workspace walk below. A repository with more
// entries than this is one where the walk would cost more per command than the
// confinement is worth; the scan stops and what it already found is still
// covered. The number is generous — sonar's own tree is far under it.
const maxSecretScanEntries = 50_000

// resolveSecretComponents turns component globs into concrete mount points.
//
// A mount namespace has no way to say "any path matching a pattern", which is
// the whole difficulty: macOS enforces `.env` at any depth with one Seatbelt
// regex and costs nothing, while bubblewrap needs a path per file. Leaving the
// gap open was the first version's answer, and it made the two platforms
// promise different things under one package doc — `cat .env` and
// `cat ~/.ssh/id_rsa` both succeeded on Linux.
//
// So the paths are found instead. Two sources, and both are needed:
//
//   - a bounded walk of the workspace, which is where a repository's own .env
//     lives and where a workspace script would reach for one;
//   - the literal components resolved against $HOME, which is where ~/.ssh,
//     ~/.aws, ~/.npmrc and ~/.netrc live. `--ro-bind / /` makes the whole host
//     readable, so without this they stay readable.
//
// The honest limit: this is a snapshot. A secret CREATED after the scan and
// before exec is not covered, where the macOS regex would still refuse it.
// Closing that would need seccomp — which is what the reference implementation
// vendors compiled binaries for — and it is a narrower gap than the one this
// closes.
func resolveSecretComponents(policy Policy) []string {
	if len(policy.UnreadableComponents) == 0 {
		return nil
	}
	leaves := make(map[string]struct{}, len(policy.ReadableLeaves))
	for _, leaf := range policy.ReadableLeaves {
		leaves[leaf] = struct{}{}
	}
	denied := func(name string, isLeaf bool) bool {
		for _, component := range policy.UnreadableComponents {
			if matched, _ := filepath.Match(component, name); !matched {
				continue
			}
			if _, public := leaves[name]; public && isLeaf {
				return false
			}
			return true
		}
		return false
	}

	var found []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, component := range policy.UnreadableComponents {
			// Only literal components can name a path without a walk; a glob
			// like *.pem is left to the workspace scan, since walking $HOME
			// would cost far more than the confinement is worth.
			if strings.ContainsAny(component, "*?[") {
				continue
			}
			found = append(found, filepath.Join(home, component))
		}
	}

	scanned := 0
	_ = filepath.WalkDir(policy.Workspace, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if scanned++; scanned > maxSecretScanEntries {
			return fs.SkipAll
		}
		if path == policy.Workspace {
			return nil
		}
		// A directory is a leaf only when nothing is under it, which is the
		// distinction the public-template exception turns on: `.env.example`
		// the FILE is readable, `.env.example/` the directory is not.
		if !denied(entry.Name(), !entry.IsDir()) {
			return nil
		}
		found = append(found, path)
		if entry.IsDir() {
			// Already covered wholesale; descending would only add redundant
			// mounts under a tmpfs that hides them anyway.
			return fs.SkipDir
		}
		return nil
	})
	return found
}
