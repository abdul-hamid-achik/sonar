package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return written, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, err := b.buf.Write(p)
	return written, err
}

func (b *cappedBuffer) Len() int { return b.buf.Len() }

func (b *cappedBuffer) String() string {
	value := b.buf.String()
	if b.truncated {
		value += "\n... (subprocess output truncated by host)"
	}
	return value
}

func (a *Agent) handleBash(parent context.Context, args map[string]any) (string, bool) {
	command, _ := args["command"].(string)
	if command == "" {
		return "error: command is required", true
	}

	timeout := a.getArgInt(args, "timeout", int(a.ToolTimeout().Seconds()))
	maxTimeoutSecs := int(a.ToolTimeout().Seconds())
	if maxTimeoutSecs > 120 {
		maxTimeoutSecs = 120
	}
	if timeout > maxTimeoutSecs {
		timeout = maxTimeoutSecs
	}
	if timeout < 1 {
		timeout = 1
	}

	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	configureCommandProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = a.activeWorkDir()
	// Do not leak the parent process environment (which may hold API keys,
	// tokens, DB passwords) to LLM-generated shell commands. Pass only a
	// curated allowlist of variables a shell legitimately needs.
	cmd.Env = sanitizedEnv()
	cmd.Stdin = nil

	stdout := cappedBuffer{limit: maxToolCaptureBytes}
	stderr := cappedBuffer{limit: maxToolCaptureBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += "STDERR:\n" + stderr.String()
	}

	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf(outcomeUnknownReceiptPrefix+" command timed out after %d seconds; its process group was terminated, but effects completed before termination may have occurred", timeout), true
	}
	if ctx.Err() == context.Canceled {
		return outcomeUnknownReceiptPrefix + " command was cancelled after dispatch; its process group was terminated, but effects completed before termination may have occurred", true
	}

	if err != nil {
		if output == "" {
			return fmt.Sprintf("error: %v", err), true
		}
		return fmt.Sprintf("Command exited with error:\n%s", output), true
	}

	if output == "" {
		return "Command completed successfully (no output)", false
	}

	return output, false
}

// envAllowlist names the environment variables a shell command legitimately
// needs. Everything else (secrets, tokens, provider keys) is withheld from
// LLM-generated commands.
var envAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
	"TERM", "TMPDIR", "PWD", "TZ",
	// Common toolchain roots that are paths, not secrets. Version-manager
	// shims (asdf, mise) silently fail with exit 126 without their data dirs,
	// which breaks every toolchain command an AUTO turn runs on such machines.
	"GOPATH", "GOROOT", "GOCACHE", "GOMODCACHE",
	"NVM_DIR", "PYENV_ROOT", "RBENV_ROOT", "CARGO_HOME", "RUSTUP_HOME",
	"ASDF_DIR", "ASDF_DATA_DIR", "MISE_DATA_DIR", "MISE_CACHE_DIR",
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
}

// sanitizedEnv returns a minimal environment for subprocesses, copying only
// the allowlisted variables that are actually set in the parent.
func sanitizedEnv() []string {
	env := make([]string, 0, len(envAllowlist))
	for _, k := range envAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
