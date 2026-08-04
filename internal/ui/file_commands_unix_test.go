//go:build darwin || linux

package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"golang.org/x/sys/unix"
)

func TestLoadAndExportRejectFIFOWithoutBlocking(t *testing.T) {
	workDir := t.TempDir()
	fifo := filepath.Join(workDir, "blocked")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	load := m.handleCommandAction(command.Result{Action: command.ActionLoadContext, Data: fifo})
	result := awaitCommandMessage[ContextLoadResultMsg](t, commandMessages(load), 500*time.Millisecond)
	if result.Err == nil {
		t.Fatal("FIFO context load succeeded")
	}

	exportDone := make(chan error, 1)
	go func() {
		_, err := writeConversationExport(workDir, fifo, []byte("data"), true)
		exportDone <- err
	}()
	select {
	case err := <-exportDone:
		if err == nil || !strings.Contains(err.Error(), "non-regular") {
			t.Fatalf("FIFO export error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("FIFO export blocked")
	}

	info, err := os.Lstat(fifo)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO was changed: info=%v err=%v", info, err)
	}
}
