//go:build windows || plan9 || js || wasip1

package safeio

import (
	"fmt"
	"os"
)

// OpenWithinNoFollow fails closed on platforms without descriptor-relative,
// no-follow traversal. An lstat followed by os.Open would leave a race in which
// a checked component could be replaced by a symlink or Windows reparse point.
func OpenWithinNoFollow(root, relative string) (*os.File, error) {
	root, _, err := withinPathComponents(root, relative)
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s", ErrNoFollowUnsupported, root)
}
