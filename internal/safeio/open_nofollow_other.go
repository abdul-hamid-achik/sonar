//go:build windows || plan9 || js || wasip1

package safeio

import (
	"fmt"
	"os"
)

// Platforms without POSIX openat/O_NOFOLLOW use the strongest available
// lexical check, followed by the shared post-open fstat validation.
func openFileNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", ErrSymlink, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return file, nil
}
