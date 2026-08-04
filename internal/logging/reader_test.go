package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// helper creates n temp log files in dir with distinct mod times.
func createFakeLogs(t *testing.T, dir string, n int) []string {
	t.Helper()
	var paths []string
	for i := range n {
		name := filepath.Join(dir, "2025-01-01_00-00-0"+string(rune('0'+i))+".log")
		if err := os.WriteFile(name, []byte("line "+string(rune('0'+i))+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Stagger mod times so ordering is deterministic.
		ts := time.Now().Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(name, ts, ts); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	return paths
}

func TestListLogs(t *testing.T) {
	dir := t.TempDir()
	createFakeLogs(t, dir, 5)

	logs, err := listLogsIn(dir, 3)
	if err != nil {
		t.Fatalf("listLogsIn error: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(logs))
	}

	// Verify newest-first ordering.
	for i := 1; i < len(logs); i++ {
		if logs[i].ModTime.After(logs[i-1].ModTime) {
			t.Errorf("entry %d (%v) is newer than entry %d (%v)", i, logs[i].ModTime, i-1, logs[i-1].ModTime)
		}
	}
}

func TestListLogs_All(t *testing.T) {
	dir := t.TempDir()
	createFakeLogs(t, dir, 4)

	logs, err := listLogsIn(dir, 0)
	if err != nil {
		t.Fatalf("listLogsIn error: %v", err)
	}
	if len(logs) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(logs))
	}
}

func TestListLogs_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	logs, err := listLogsIn(dir, 5)
	if err != nil {
		t.Fatalf("listLogsIn error: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(logs))
	}
}

func TestListLogs_MissingDir(t *testing.T) {
	logs, err := listLogsIn(filepath.Join(t.TempDir(), "missing"), 5)
	if err != nil {
		t.Fatalf("listLogsIn error: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(logs))
	}
}

func TestTailLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	lines, err := TailLog(path, 3)
	if err != nil {
		t.Fatalf("TailLog error: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "line3" {
		t.Errorf("expected 'line3', got %q", lines[0])
	}
	if lines[2] != "line5" {
		t.Errorf("expected 'line5', got %q", lines[2])
	}
}

func TestTailLog_FewerLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.log")
	if err := os.WriteFile(path, []byte("only\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lines, err := TailLog(path, 100)
	if err != nil {
		t.Fatalf("TailLog error: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
}

func TestTailLog_MissingFile(t *testing.T) {
	_, err := TailLog("/tmp/nonexistent-file-test-xyz.log", 10)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLatestLogPath(t *testing.T) {
	dir := t.TempDir()
	paths := createFakeLogs(t, dir, 3)

	latest, err := latestLogPathIn(dir)
	if err != nil {
		t.Fatalf("latestLogPathIn error: %v", err)
	}
	// The last created file has the newest mod time.
	expected := paths[len(paths)-1]
	if latest != expected {
		t.Errorf("expected %q, got %q", expected, latest)
	}
}

func TestLatestLogPath_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	_, err := latestLogPathIn(dir)
	if err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestLogDir(t *testing.T) {
	dir := LogDir()
	if dir == "" {
		t.Fatal("LogDir should not be empty")
	}
	if filepath.Base(dir) != "logs" {
		t.Errorf("expected dir to end in 'logs', got %q", dir)
	}
}
