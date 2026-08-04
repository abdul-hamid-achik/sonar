//go:build darwin || linux

package db

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func validateExecutionLeasePlatform() error { return nil }

func openExecutionLeaseFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*os.File, error) {
		_ = unix.Close(fd)
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return closeOnError(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return closeOnError(fmt.Errorf("execution lease path is not a regular file"))
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return closeOnError(err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return closeOnError(fmt.Errorf("create execution lease file handle"))
	}
	return file, nil
}

func tryLockExecutionLeaseFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return false, err
}

func unlockExecutionLeaseFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
