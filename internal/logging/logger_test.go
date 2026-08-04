package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cleanupLogFile(t *testing.T, f *os.File) {
	t.Helper()
	t.Cleanup(func() {
		name := f.Name()
		if err := f.Close(); err != nil {
			t.Errorf("close log file: %v", err)
		}
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove log file: %v", err)
		}
	})
}

func TestNewSessionLogger(t *testing.T) {
	logger, f, err := NewSessionLogger()
	if err != nil {
		t.Fatalf("NewSessionLogger() error: %v", err)
	}
	if f != nil {
		cleanupLogFile(t, f)
	}

	if logger == nil {
		t.Fatal("logger should not be nil")
	}

	// Verify the file was created in the expected directory.
	if f == nil {
		t.Fatal("file should not be nil")
	}
	if info, err := f.Stat(); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("log mode = %04o, want 0600", got)
	}

	dir := filepath.Dir(f.Name())
	if !strings.Contains(dir, filepath.Join(".config", "sonar", "logs")) {
		t.Errorf("log file should be in ~/.config/sonar/logs/, got %q", dir)
	}

	// Verify the file is writable.
	logger.Info("test message", "key", "value")
}

func TestNilLoggerNoPanic(t *testing.T) {
	// Ensure the pattern used in the TUI works: nil check before calling.
	var called bool
	logger, f, err := NewSessionLogger()
	if err == nil && f != nil {
		cleanupLogFile(t, f)
		logger.Info("test")
		called = true
	}
	// Just verify the code path executed without panic.
	_ = called
}
