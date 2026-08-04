package safeio

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

// ErrLockTimeout reports that a bounded exclusive-file-lock acquisition could
// not complete before its deadline.
var ErrLockTimeout = errors.New("exclusive file lock timed out")

type localPathLock struct {
	slot chan struct{}
}

var localPathLocks sync.Map

// WithExclusiveFileLock serializes fn with other writers using lockPath. On
// POSIX systems the lock is interprocess; every platform also uses a process-
// local gate so separate Store instances in one process cannot race.
func WithExclusiveFileLock(lockPath string, timeout time.Duration, fn func() error) error {
	if lockPath == "" {
		return fmt.Errorf("exclusive lock path is required")
	}
	if timeout <= 0 {
		return fmt.Errorf("invalid exclusive lock timeout %s", timeout)
	}
	if fn == nil {
		return fmt.Errorf("exclusive lock callback is required")
	}

	key := filepath.Clean(lockPath)
	if absolute, err := filepath.Abs(key); err == nil {
		key = absolute
	}
	value, _ := localPathLocks.LoadOrStore(key, &localPathLock{slot: make(chan struct{}, 1)})
	gate := value.(*localPathLock)
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case gate.slot <- struct{}{}:
		defer func() { <-gate.slot }()
	case <-timer.C:
		return fmt.Errorf("%w after %s: %s", ErrLockTimeout, timeout, lockPath)
	}

	return withExclusiveFileLockPlatform(lockPath, deadline, fn)
}
