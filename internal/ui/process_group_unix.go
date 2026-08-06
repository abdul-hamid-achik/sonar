//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package ui

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const tuiProcessGroupCleanupTimeout = 100 * time.Millisecond

// configureTUICommandProcessGroup ensures cancelling an owned non-interactive
// TUI effect terminates the process and every descendant it spawned. This is
// intentionally not used for tea.ExecProcess: Bubble Tea owns interactive
// editor execution and terminal restoration synchronously.
// configureTUICommandProcessGroup gives a host-owned subprocess its own group
// and cancels that group politely first.
//
// SIGTERM before SIGKILL is not manners, it is correctness for the one command
// this path exists to run. `git commit` takes .git/index.lock before it writes
// and removes it on the way out, and SIGKILL cannot be caught — so a cancelled
// /commit left a zero-byte lock behind that blocks every later git command in
// the repository until a human deletes it. It happened four times in one
// session before anyone connected the two.
//
// Escalation is not skipped, only delayed: cmd.WaitDelay already arms the
// uncatchable kill, and cleanupTUICommandProcessGroup repeats it afterwards for
// any descendant that was forking during cancellation. A process that ignores
// SIGTERM still dies; one that handles it gets to leave the repository usable.
func configureTUICommandProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

// cleanupTUICommandProcessGroup runs only after the group leader has been
// waited and therefore cannot fork another child. Repeat the uncatchable kill
// briefly so a descendant that was concurrently forking during cancellation
// cannot escape a one-shot process-group signal.
func cleanupTUICommandProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	deadline := time.Now().Add(tuiProcessGroupCleanupTimeout)
	for {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil || time.Now().After(deadline) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}
