//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package safeio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// OpenWithinNoFollow opens a workspace-relative path without following a
// symlink in any component. Each component is opened relative to the verified
// parent descriptor, so renaming a directory during traversal cannot redirect
// the final open outside root. The caller owns the returned descriptor and
// must validate its file type with Stat before reading it.
func OpenWithinNoFollow(root, relative string) (*os.File, error) {
	root, components, err := withinPathComponents(root, relative)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root %s: %w", root, err)
	}

	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("open workspace root %s: %w", root, err)
	}

	for index, component := range components {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if index < len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, openErr := unix.Openat(fd, component, flags, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			if errors.Is(openErr, unix.ELOOP) {
				return nil, fmt.Errorf("%w: %s", ErrSymlink, filepath.Join(root, filepath.Join(components[:index+1]...)))
			}
			return nil, fmt.Errorf("open workspace path component %q: %w", component, openErr)
		}
		fd = next
	}

	path := filepath.Join(append([]string{root}, components...)...)
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open workspace path %s returned an invalid descriptor", path)
	}
	return file, nil
}
