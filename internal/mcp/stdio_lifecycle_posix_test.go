//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

const (
	hangingStdioHelperEnv = "SONAR_TEST_HANGING_STDIO_HELPER"
	hangingStdioPIDFile   = "SONAR_TEST_HANGING_STDIO_PID_FILE"
)

// TestMCPHangingSTDIOHelperProcess is launched as a real STDIO MCP child by
// TestRegistryCloseReapsHangingSTDIOProcessGroup. It deliberately never reads
// initialization input and keeps a descendant alive with inherited pipes.
func TestMCPHangingSTDIOHelperProcess(t *testing.T) {
	if os.Getenv(hangingStdioHelperEnv) != "1" {
		return
	}
	descendant := exec.Command("/bin/sleep", "300")
	if err := descendant.Start(); err != nil {
		os.Exit(2)
	}
	pids := fmt.Sprintf("%d\n%d\n", os.Getpid(), descendant.Process.Pid)
	if err := os.WriteFile(os.Getenv(hangingStdioPIDFile), []byte(pids), 0o600); err != nil {
		_ = descendant.Process.Kill()
		os.Exit(3)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestRegistryCloseReapsHangingSTDIOProcessGroup(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "stdio-pids")
	r := NewRegistry()
	connectDone := make(chan error, 1)
	go func() {
		_, err := r.ConnectServer(context.Background(), config.ServerConfig{
			Name:    "hanging-stdio",
			Command: executable,
			Args:    []string{"-test.run=^TestMCPHangingSTDIOHelperProcess$"},
			Env: []string{
				hangingStdioHelperEnv + "=1",
				hangingStdioPIDFile + "=" + pidFile,
			},
			Transport: "stdio",
		})
		connectDone <- err
	}()

	helperPID, descendantPID := waitForStdioHelperPIDs(t, pidFile)
	t.Cleanup(func() {
		_ = syscall.Kill(-helperPID, syscall.SIGKILL)
		_ = syscall.Kill(descendantPID, syscall.SIGKILL)
	})

	closeDone := make(chan struct{})
	start := time.Now()
	go func() {
		r.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-helperPID, syscall.SIGKILL)
		t.Fatal("Registry.Close hung while cancelling a real STDIO MCP child")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Registry.Close took %v", elapsed)
	}

	select {
	case err := <-connectDone:
		if err == nil {
			t.Fatal("hanging STDIO connection unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("ConnectServer did not return before Registry.Close")
	}

	for _, pid := range []int{helperPID, descendantPID} {
		if !waitForProcessExit(pid, time.Second) {
			t.Fatalf("STDIO process %d survived Registry.Close", pid)
		}
	}
	if r.ServerCount() != 0 || r.ToolCount() != 0 {
		t.Fatalf("closed registry retained STDIO state: servers=%d tools=%d", r.ServerCount(), r.ToolCount())
	}
}

func waitForStdioHelperPIDs(t *testing.T, path string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 2 {
				helperPID, helperErr := strconv.Atoi(fields[0])
				descendantPID, descendantErr := strconv.Atoi(fields[1])
				if helperErr == nil && descendantErr == nil && helperPID > 0 && descendantPID > 0 {
					return helperPID, descendantPID
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read helper PID file: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("hanging STDIO helper did not report its process IDs")
	return 0, 0
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
