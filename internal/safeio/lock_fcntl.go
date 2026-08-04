//go:build aix

package safeio

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func withExclusiveFileLockPlatform(path string, deadline time.Time, fn func() error) error {
	file, err := openLockFileNoFollow(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	for {
		err = unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("lock %s: %w", path, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%w: %s", ErrLockTimeout, path)
		}
		pause := 10 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		time.Sleep(pause)
	}
	defer func() {
		lock.Type = unix.F_UNLCK
		_ = unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock)
	}()
	return fn()
}
