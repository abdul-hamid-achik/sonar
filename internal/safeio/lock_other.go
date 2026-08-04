//go:build windows || plan9 || js || wasip1

package safeio

import (
	"fmt"
	"os"
	"time"
)

// The common process-local gate remains effective on platforms without a
// portable interprocess advisory-lock primitive.
func withExclusiveFileLockPlatform(path string, _ time.Time, fn func() error) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlink, path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("lstat lock %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("fstat lock %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: lock %s (%s)", ErrNotRegular, path, info.Mode().Type())
	}
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure open lock %s: %w", path, err)
	}
	return fn()
}
