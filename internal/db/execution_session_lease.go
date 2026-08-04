package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrExecutionSessionBusy means another process or Store instance holds the
// exclusive execution lease for this persisted session.
var ErrExecutionSessionBusy = errors.New("execution session is busy")

// ExecutionSessionLease holds one kernel-backed, cross-process session lock.
// It is independent of Store.Close and is released automatically by the kernel
// when the owning process exits. The reusable lock file is never unlinked.
type ExecutionSessionLease struct {
	mu          sync.Mutex
	file        *os.File
	sessionID   int64
	workspaceID string
	leaseRoot   string
	closed      bool
}

// Close releases the execution lease. Repeated and concurrent calls are safe
// and return nil after the first close attempt has completed.
func (l *ExecutionSessionLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	file := l.file
	l.file = nil
	if file == nil {
		return nil
	}
	unlockErr := unlockExecutionLeaseFile(file)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

// AcquireExecutionSessionLease acquires a nonblocking exclusive lease for one
// session/workspace scope. The caller must hold the returned lease across
// recovery inspection, agent execution, and snapshot cursor persistence.
func (s *Store) AcquireExecutionSessionLease(ctx context.Context, sessionID int64, workspaceID string) (*ExecutionSessionLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateExecutionSessionScope(ctx, s.db, sessionID, workspaceID); err != nil {
		return nil, err
	}
	if s.executionLeaseRoot == "" {
		return nil, fmt.Errorf("execution session leases require a file-backed database")
	}
	if err := validateExecutionLeasePlatform(); err != nil {
		return nil, err
	}
	if err := ensurePrivateExecutionLeaseDirectory(s.executionLeaseRoot); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(s.executionLeaseRoot, fmt.Sprintf("session-%d.lock", sessionID))
	file, err := openExecutionLeaseFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("open execution session lease: %w", err)
	}
	locked, err := tryLockExecutionLeaseFile(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock execution session %d: %w", sessionID, err)
	}
	if !locked {
		_ = file.Close()
		return nil, fmt.Errorf("%w: session %d", ErrExecutionSessionBusy, sessionID)
	}
	lease := &ExecutionSessionLease{
		file: file, sessionID: sessionID, workspaceID: workspaceID,
		leaseRoot: s.executionLeaseRoot,
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	return lease, nil
}

func ensurePrivateExecutionLeaseDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create execution lease directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect execution lease directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("execution lease path %q is not a private directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure execution lease directory: %w", err)
	}
	return nil
}
