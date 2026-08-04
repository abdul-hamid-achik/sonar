package safeio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidatePublishPath rejects symlinks and non-directory components before an
// atomic file publish. Managed application paths below the canonical home are
// checked component-by-component; arbitrary test/library paths still validate
// their final parent and destination. Call it again immediately before rename
// because validation is intentionally a point-in-time guard. A same-UID actor
// that can replace an owner-only real directory between the final lstat and the
// publish syscall remains an operating-system containment boundary; closing
// that final pathname race requires platform-specific dirfd-relative rename.
func ValidatePublishPath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve publish path %s: %w", path, err)
	}
	paths := []string{filepath.Dir(absolute), absolute}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		if home, homeErr = filepath.Abs(home); homeErr == nil {
			if resolved, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil {
				home = resolved
			}
			if rel, relErr := filepath.Rel(home, absolute); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				paths = []string{home}
				current := home
				for _, component := range strings.Split(rel, string(filepath.Separator)) {
					if component == "" || component == "." {
						continue
					}
					current = filepath.Join(current, component)
					paths = append(paths, current)
				}
			}
		}
	}

	for index, component := range paths {
		info, lstatErr := os.Lstat(component)
		if errors.Is(lstatErr, os.ErrNotExist) {
			return nil
		}
		if lstatErr != nil {
			return fmt.Errorf("inspect publish path component %s: %w", component, lstatErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w in publish path: %s", ErrSymlink, component)
		}
		if index < len(paths)-1 && !info.IsDir() {
			return fmt.Errorf("%w: publish path parent %s (%s)", ErrNotRegular, component, info.Mode().Type())
		}
		if index == len(paths)-1 && !info.Mode().IsRegular() {
			return fmt.Errorf("%w: publish destination %s (%s)", ErrNotRegular, component, info.Mode().Type())
		}
	}
	return nil
}
