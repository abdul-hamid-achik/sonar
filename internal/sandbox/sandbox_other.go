//go:build !darwin && !linux

package sandbox

// Available reports false everywhere sonar has no confinement primitive it has
// actually verified. It is deliberately not a best-effort guess: a stub that
// returns true and confines nothing would let a caller widen its own policy on
// a promise the host cannot keep.
func Available() bool { return false }

func networkNamespaceAvailable() bool { return false }

func wrapCommand(Policy, string, []string) (string, []string, error) {
	return "", nil, ErrUnsupported
}
