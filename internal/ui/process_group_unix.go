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
func configureTUICommandProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
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
