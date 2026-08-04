//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package mcp

import "os/exec"

// configureProcessCancellation retains exec.CommandContext's direct-process
// cancellation on platforms without POSIX process groups.
func configureProcessCancellation(*exec.Cmd) {}
