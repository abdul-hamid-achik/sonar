//go:build !darwin

package hostenv

// FilterMallocDiagnostics is a no-op off Darwin: libmalloc's diagnostic is
// a macOS fd-2 leak, and there is nothing to intercept.
func FilterMallocDiagnostics() func() {
	return func() {}
}
