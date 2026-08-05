//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package agent

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const commandProcessGroupCleanupTimeout = 100 * time.Millisecond

// configureCommandProcessGroup ensures cancellation terminates the shell and
// every descendant it spawned, preventing background mutations after Stop.
func configureCommandProcessGroup(cmd *exec.Cmd) {
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

// cleanupCommandProcessGroup runs only after the group leader has been waited
// and therefore cannot fork another child. Repeat the uncatchable kill briefly
// so a descendant that was concurrently forking during cancellation cannot
// escape a one-shot process-group signal.
func cleanupCommandProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	deadline := time.Now().Add(commandProcessGroupCleanupTimeout)
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
