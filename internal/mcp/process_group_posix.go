//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package mcp

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// configureProcessCancellation gives each STDIO MCP server its own process
// group. exec.CommandContext invokes Cancel when the client/registry lifecycle
// ends; killing the group prevents grandchildren from surviving shutdown while
// retaining inherited pipes that can otherwise keep cmd.Wait blocked.
func configureProcessCancellation(cmd *exec.Cmd) {
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
	// This is a final os/exec safeguard if a platform leaves inherited pipes
	// open despite group termination. The SDK still owns the single cmd.Wait.
	cmd.WaitDelay = 2 * time.Second
}
