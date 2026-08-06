package sandbox

import (
	"os/exec"
)

// bubblewrapPath is the Linux confinement driver. Unlike macOS's sandbox-exec
// it is not part of the base system, so Available() reports false on a machine
// without it and the caller decides what that means.
const bubblewrapName = "bwrap"

func Available() bool {
	_, err := exec.LookPath(bubblewrapName)
	return err == nil
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
//   - Secret paths are covered with a tmpfs, which is what makes the file
//     unreadable — an empty directory is mounted over it, so there is nothing
//     left to read even for a process that knows the exact path.
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

	// A tmpfs over each secret leaves an empty directory where the file was.
	// This comes before the writable binds so a workspace-internal ignored
	// path cannot be re-exposed by the workspace bind that follows.
	for _, path := range policy.UnreadablePaths {
		wrapped = append(wrapped, "--tmpfs", path)
	}

	wrapped = append(wrapped, "--bind", policy.Workspace, policy.Workspace)
	for _, path := range policy.WritablePaths {
		if containedBy(path, policy.Workspace) {
			continue
		}
		wrapped = append(wrapped, "--bind", path, path)
	}

	// Re-cover the secrets after the writable binds. A path under the
	// workspace would otherwise have just been re-mounted read-write, which
	// would make the .env the policy hid both readable and writable again.
	for _, path := range policy.UnreadablePaths {
		if containedBy(path, policy.Workspace) {
			wrapped = append(wrapped, "--tmpfs", path)
		}
	}

	if !policy.AllowNetwork {
		wrapped = append(wrapped, "--unshare-net")
	}

	// Keep the working directory the caller chose. bubblewrap resets it to /
	// otherwise, which silently changes what a relative path in the command
	// means — the sort of difference that turns a confined command into a
	// different command.
	wrapped = append(wrapped, "--chdir", policy.Workspace)

	wrapped = append(wrapped, "--", name)
	return bubblewrapName, append(wrapped, args...), nil
}
