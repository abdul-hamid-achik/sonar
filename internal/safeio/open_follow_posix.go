//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package safeio

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openFileFollow(path string) (*os.File, error) {
	// Explicit imports may follow a symlink, but O_NONBLOCK is still required
	// so a FIFO cannot strand the bounded reader before fstat rejects it.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open %s returned an invalid descriptor", path)
	}
	return file, nil
}
