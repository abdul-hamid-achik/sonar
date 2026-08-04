//go:build windows || plan9 || js || wasip1

package agent

import "os/exec"

// Non-Unix platforms retain exec.CommandContext's single-process fallback.
func configureCommandProcessGroup(*exec.Cmd) {}
