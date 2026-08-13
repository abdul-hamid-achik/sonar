//go:build darwin

package hostenv

import (
	"os"

	"golang.org/x/sys/unix"
)

// FilterMallocDiagnostics redirects fd 2 through a line filter that drops
// libmalloc's stack-logging chatter for the life of the returned restore
// function. Go's os.Stderr is fd 2; libmalloc writes to fd 2 directly, so
// swapping the file descriptor is what actually catches the leak. A failure
// to install the filter is silent: the TUI still starts, and the diagnostic
// is the same one the filter exists to hide.
func FilterMallocDiagnostics() func() {
	reader, writer, err := os.Pipe()
	if err != nil {
		return func() {}
	}
	saved, err := unix.Dup(unix.Stderr)
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return func() {}
	}
	if err := unix.Dup2(int(writer.Fd()), unix.Stderr); err != nil {
		_ = unix.Close(saved)
		_ = reader.Close()
		_ = writer.Close()
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = reader.Close() }()
		copyFilteredMallocDiagnostics(reader, fdWriter(saved))
	}()
	return func() {
		_ = writer.Close()
		<-done
		_ = unix.Dup2(saved, unix.Stderr)
		_ = unix.Close(saved)
	}
}

type fdWriter int

func (fd fdWriter) Write(p []byte) (int, error) {
	return unix.Write(int(fd), p)
}
