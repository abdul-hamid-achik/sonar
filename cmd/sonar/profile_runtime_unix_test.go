//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func TestBuildBaseLoadedContextRejectsFIFOWithoutLegacyFallback(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "AGENTS.md")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("must not silently fall back"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldReader := projectInstructionsReader
	oldTimeout := projectInstructionsReadTimeout
	projectInstructionsReader = safeio.NewReader()
	projectInstructionsReadTimeout = 30 * time.Millisecond
	t.Cleanup(func() {
		projectInstructionsReader = oldReader
		projectInstructionsReadTimeout = oldTimeout
	})

	contextText, err := buildBaseLoadedContextAt(nil, dir)
	if err == nil || !errors.Is(err, safeio.ErrNotRegular) {
		t.Fatalf("FIFO instructions result = %q, %v", contextText, err)
	}
	if strings.Contains(contextText, "silently fall back") {
		t.Fatalf("unsafe AGENTS.md was suppressed by legacy fallback: %q", contextText)
	}

	// The nonblocking no-follow open lets fstat reject the FIFO immediately,
	// so the bounded reader slot is available for the next critical read.
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, readErr := projectInstructionsReader.ReadRegularFile(probe, 16, time.Second)
	if readErr != nil || string(data) != "ok" {
		t.Fatalf("reader recovery data = %q, err = %v", data, readErr)
	}
}
