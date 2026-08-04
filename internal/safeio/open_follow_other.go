//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris

package safeio

import (
	"os"
)

func openFileFollow(path string) (*os.File, error) {
	return os.Open(path)
}
