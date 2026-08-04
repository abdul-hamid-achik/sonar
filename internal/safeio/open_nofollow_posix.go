//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package safeio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openFileNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", ErrSymlink, path)
	}

	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open parent of %s: %w", path, err)
	}
	defer func() { _ = directory.Close() }()
	fd, err := unix.Openat(
		int(directory.Fd()),
		filepath.Base(path),
		// O_NONBLOCK prevents open itself from hanging on a FIFO before fstat
		// can reject it. It has no effect on ordinary disk-file reads.
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: %s", ErrSymlink, path)
		}
		return nil, fmt.Errorf("openat %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("openat %s returned an invalid descriptor", path)
	}
	return file, nil
}
