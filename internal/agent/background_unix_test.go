//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package agent

import (
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCloseReapsTheWholeBackgroundProcessGroup proves the shutdown contract at
// the operating system: not only the shell the harness started but the child it
// spawned must be gone, so a backgrounded dev server cannot keep holding a port
// after the session ends.
func TestCloseReapsTheWholeBackgroundProcessGroup(t *testing.T) {
	ag := newBackgroundAgent(t)
	// The shell prints its child's pid and then waits, so the group has a
	// descendant that a single-process kill would leave behind.
	id, proc := startBackground(t, ag, "sleep 300 & echo $!; wait")
	childPID := 0
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && childPID == 0 {
		result := readBack(t, ag, id)
		childPID = firstPIDInOutput(result)
		if childPID == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if childPID == 0 {
		t.Fatal("background shell never reported its child pid")
	}
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child %d was not alive before shutdown: %v", childPID, err)
	}

	ag.Close()

	for _, pid := range []int{proc.pid, childPID} {
		if !processGone(pid) {
			t.Fatalf("process %d survived Agent.Close", pid)
		}
	}
}

func processGone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		// A reaped child of this process can linger as a zombie only until the
		// runtime waits it; every other error (EPERM on a recycled pid) means
		// the original process is gone as far as this session is concerned.
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// firstPIDInOutput returns the first line of captured output that is nothing
// but digits, which is the pid the background shell echoed.
func firstPIDInOutput(result string) int {
	for _, line := range strings.Split(result, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}
