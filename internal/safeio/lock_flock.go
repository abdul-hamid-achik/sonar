//go:build android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

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

	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
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
	defer func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }()
	return fn()
}
