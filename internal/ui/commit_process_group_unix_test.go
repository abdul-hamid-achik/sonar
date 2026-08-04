//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommitCancellationKillsGitDescendants(t *testing.T) {
	workDir := t.TempDir()
	started := filepath.Join(workDir, "started")
	late := filepath.Join(workDir, "late")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "touch \"$STARTED\"; (sleep 1; touch \"$LATE\") & wait")
	cmd.Env = append(os.Environ(), "STARTED="+started, "LATE="+late)
	configureTUICommandProcessGroup(cmd)
	cmd.WaitDelay = 100 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- runOwnedTUICommand(cmd) }()

	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("git-like effect did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled git-like effect did not join")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(late); !os.IsNotExist(err) {
		t.Fatalf("git descendant mutated after cancellation: %v", err)
	}
}

func TestAutomatedCommitDisablesHooksAndSigning(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	workDir := t.TempDir()
	runGitForCommitTest(t, workDir, "init", "-q")
	runGitForCommitTest(t, workDir, "config", "user.name", "sonar Test")
	runGitForCommitTest(t, workDir, "config", "user.email", "sonar@example.invalid")

	tracked := filepath.Join(workDir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitForCommitTest(t, workDir, "add", "tracked.txt")

	hookStarted := filepath.Join(workDir, "hook-started")
	hookLate := filepath.Join(workDir, "hook-late")
	signerStarted := filepath.Join(workDir, "signer-started")
	t.Setenv("HOOK_STARTED", hookStarted)
	t.Setenv("HOOK_LATE", hookLate)
	t.Setenv("SIGNER_STARTED", signerStarted)

	hook := filepath.Join(workDir, ".git", "hooks", "pre-commit")
	hookScript := "#!/bin/sh\n" +
		"touch \"$HOOK_STARTED\"\n" +
		"nohup sh -c 'sleep 0.2; touch \"$HOOK_LATE\"' >/dev/null 2>&1 &\n" +
		"exit 0\n"
	if err := os.WriteFile(hook, []byte(hookScript), 0o700); err != nil {
		t.Fatal(err)
	}
	signer := filepath.Join(workDir, "fake-gpg")
	if err := os.WriteFile(signer, []byte("#!/bin/sh\ntouch \"$SIGNER_STARTED\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitForCommitTest(t, workDir, "config", "commit.gpgSign", "true")
	runGitForCommitTest(t, workDir, "config", "gpg.program", signer)

	if err := (commandCommitGit{dir: workDir}).Commit(context.Background(), "test: controlled commit"); err != nil {
		t.Fatalf("controlled commit: %v", err)
	}
	for _, marker := range []string{hookStarted, signerStarted} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("disabled external helper ran (%s): %v", marker, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(hookLate); !os.IsNotExist(err) {
		t.Fatalf("detached hook mutation ran after commit: %v", err)
	}
	if got := strings.TrimSpace(runGitForCommitTest(t, workDir, "rev-list", "--count", "HEAD")); got != "1" {
		t.Fatalf("commit count = %q, want 1", got)
	}
}

func runGitForCommitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}
