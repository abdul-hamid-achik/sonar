package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/sandbox"
)

// newSandboxedAgent returns an agent confined to a fresh workspace holding one
// ordinary file and one secret.
func newSandboxedAgent(t *testing.T, allowNetwork bool) (*Agent, string) {
	t.Helper()
	workspace := t.TempDir()
	for name, content := range map[string]string{
		"app.go": "package app\n",
		".env":   "API_KEY=SECRET-VALUE\n",
	} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	ag.SetSandboxPosture(SandboxPosture{Enabled: true, AllowNetwork: allowNetwork})
	return ag, workspace
}

// runBash fails the test when the host refused to run the command at all.
//
// Without this guard every denial assertion below passes for the wrong reason.
// That is not hypothetical: the first version of confinement produced
// "command with a non-nil Cancel was not created with CommandContext" and
// nothing executed, so "the secret is unreadable", "the write is refused" and
// "the network is refused" all reported success while enforcing nothing. A
// denial test has to prove the command ran and was stopped, not that it never
// started.
func runBash(t *testing.T, ag *Agent, command string) string {
	t.Helper()
	output, _ := ag.handleBash(context.Background(), map[string]any{"command": command})
	for _, hostFailure := range []string{
		"could not be confined",
		"was not created with CommandContext",
		"executable file not found",
	} {
		if strings.Contains(output, hostFailure) {
			t.Fatalf("the host never ran the command, so nothing below is a real denial:\n%s", output)
		}
	}
	return output
}

// TestConfinedBashToolEnforcesTheWorkspaceBoundary drives the real tool rather
// than the sandbox package, because the boundary that matters is the one a
// model's command actually meets. Every field handleBash sets — directory,
// sanitized environment, process group, capped streams — has to survive being
// wrapped, and a test of the package alone proves none of that.
func TestConfinedBashToolEnforcesTheWorkspaceBoundary(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("no confinement driver on this platform")
	}
	ag, workspace := newSandboxedAgent(t, false)
	if !ag.SandboxActive() {
		t.Fatal("confinement was requested and is available, but reports inactive")
	}

	t.Run("ordinary work still runs", func(t *testing.T) {
		output := runBash(t, ag, "cat app.go && echo generated > out.txt")
		if !strings.Contains(output, "package app") {
			t.Fatalf("confined read of a workspace file failed:\n%s", output)
		}
		if _, err := os.Stat(filepath.Join(workspace, "out.txt")); err != nil {
			t.Fatalf("confined write inside the workspace failed: %v", err)
		}
	})

	t.Run("the secret the catalog hides is hidden from the kernel too", func(t *testing.T) {
		// The catalog already refuses `cat .env` by path authority. This is the
		// case it cannot reach: a workspace script opening the file itself, so
		// no argv anywhere names it.
		script := filepath.Join(workspace, "leak.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ncat .env\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		output := runBash(t, ag, "sh leak.sh")
		if strings.Contains(output, "SECRET-VALUE") {
			t.Fatalf("a workspace script read a secret the host policy denies:\n%s", output)
		}
	})

	t.Run("writes outside every writable root are refused", func(t *testing.T) {
		// Not t.TempDir(): that lives under TMPDIR, which the default policy
		// makes writable on purpose so `go build` can create its work
		// directory. A target there proves nothing, and asserting on one is
		// how this test passed while enforcing nothing during development.
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			t.Skip("no home directory to test the boundary against")
		}
		outside := filepath.Join(home, ".sonar-sandbox-escape-probe")
		t.Cleanup(func() { _ = os.Remove(outside) })
		runBash(t, ag, "echo escaped > "+outside)
		if _, err := os.Stat(outside); err == nil {
			t.Fatal("a confined command wrote outside every writable root")
		}
	})

	t.Run("the network is refused", func(t *testing.T) {
		output := runBash(t, ag, "curl -sS -m 5 http://example.com")
		if strings.Contains(strings.ToLower(output), "<html") {
			t.Fatalf("a confined command reached the network:\n%s", output)
		}
	})
}

// TestUnconfinedAgentIsUnchanged pins that the feature is genuinely off by
// default. A security layer that quietly changes behaviour before anyone opts
// in is one an operator debugs instead of adopts.
func TestUnconfinedAgentIsUnchanged(t *testing.T) {
	workspace := t.TempDir()
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	if ag.SandboxActive() {
		t.Fatal("confinement is active without being enabled")
	}
	outside := filepath.Join(t.TempDir(), "written.txt")
	runBash(t, ag, "echo written > "+outside)
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("the default posture confined a command it should not have: %v", err)
	}
}

// TestSandboxActiveRequiresBothRequestAndPlatform keeps the two halves of the
// answer together. A caller that checked only the posture would describe a
// boundary the platform is not enforcing.
func TestSandboxActiveRequiresBothRequestAndPlatform(t *testing.T) {
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())
	ag.SetSandboxPosture(SandboxPosture{Enabled: true})
	if got, want := ag.SandboxActive(), sandbox.Available(); got != want {
		t.Fatalf("SandboxActive() = %v with driver availability %v", got, want)
	}
	ag.SetSandboxPosture(SandboxPosture{})
	if ag.SandboxActive() {
		t.Fatal("a disabled posture still reports active")
	}
}

// TestConfinedPolicyTracksTheHostSecretList proves the derivation rather than a
// copy: the confinement handed to the kernel is built from the same
// internal/config list the workspace path checks evaluate.
func TestConfinedPolicyTracksTheHostSecretList(t *testing.T) {
	ag, workspace := newSandboxedAgent(t, false)
	policy := ag.sandboxPolicy()
	if policy.Workspace == "" || !strings.HasSuffix(policy.Workspace, filepath.Base(workspace)) {
		t.Fatalf("policy workspace does not track the agent's: %q", policy.Workspace)
	}
	if len(policy.UnreadableComponents) == 0 || len(policy.ReadableLeaves) == 0 {
		t.Fatalf("policy did not inherit the host secret policy: %#v", policy)
	}
	for _, component := range []string{".env", ".ssh", "id_rsa*"} {
		if !containsString(policy.UnreadableComponents, component) {
			t.Fatalf("host secret component %q is missing from the confinement: %#v",
				component, policy.UnreadableComponents)
		}
	}
	if policy.AllowNetwork {
		t.Fatal("a policy built from a network-denying posture allows the network")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
