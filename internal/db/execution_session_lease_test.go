//go:build darwin || linux

package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestExecutionSessionLeaseExcludesStoresAndReusesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease.db")
	first, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExecutionTestStore(t, first)
	second, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	cleanupExecutionTestStore(t, second)
	workspaceID := "/workspace/lease"
	session := createExecutionTestSession(t, first, workspaceID)

	lease, err := first.AcquireExecutionSessionLease(context.Background(), session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if _, err := second.AcquireExecutionSessionLease(context.Background(), session.ID, workspaceID); !errors.Is(err, ErrExecutionSessionBusy) {
		t.Fatalf("second Store lease error = %v, want ErrExecutionSessionBusy", err)
	}

	dirInfo, err := os.Stat(first.executionLeaseRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("lease directory mode = %04o, want 0700", got)
	}
	lockPath := filepath.Join(first.executionLeaseRoot, "session-"+strconv.FormatInt(session.ID, 10)+".lock")
	fileInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("lease file mode = %04o, want 0600", got)
	}

	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("idempotent lease close: %v", err)
	}
	reacquired, err := second.AcquireExecutionSessionLease(context.Background(), session.ID, workspaceID)
	if err != nil {
		t.Fatalf("acquire after close: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("reusable lock file was removed: %v", err)
	}
}

func TestExecutionSessionLeaseValidatesWorkspaceAndContext(t *testing.T) {
	store := testStore(t)
	workspaceID := "/workspace/lease-scope"
	session := createExecutionTestSession(t, store, workspaceID)

	if _, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, "/workspace/other"); !errors.Is(err, ErrExecutionWorkspaceMismatch) {
		t.Fatalf("cross-workspace lease error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.AcquireExecutionSessionLease(cancelled, session.ID, workspaceID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lease error = %v", err)
	}
}

func TestExecutionSessionLeaseOutlivesStoreClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store-close.db")
	first, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenPath(path)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	cleanupExecutionTestStore(t, second)
	workspaceID := "/workspace/store-close"
	session := createExecutionTestSession(t, first, workspaceID)
	lease, err := first.AcquireExecutionSessionLease(context.Background(), session.ID, workspaceID)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.AcquireExecutionSessionLease(context.Background(), session.ID, workspaceID); !errors.Is(err, ErrExecutionSessionBusy) {
		t.Fatalf("Store.Close bypassed external lease: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	reacquired, err := second.AcquireExecutionSessionLease(context.Background(), session.ID, workspaceID)
	if err != nil {
		t.Fatalf("lease remained held after explicit close: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatal(err)
	}
}
