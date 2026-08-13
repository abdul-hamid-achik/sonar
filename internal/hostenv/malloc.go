package hostenv

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// ScrubDarwinMallocDiagnostics unsets macOS libmalloc diagnostic variables
// inherited from an IDE, Instruments, or a parent debugger.
//
// Those variables make every fork print
//
//	<binary>(pid) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
//
// onto fd 2. In the TUI that is the same tty as the alt-screen, so the line
// punches through the frame. Codex and Copilot CLI hit the same leak; the
// fix is to drop the variables before any subprocess is spawned. Harmless
// on Linux and Windows: the keys are simply absent.
func ScrubDarwinMallocDiagnostics() []string {
	var removed []string
	for _, kv := range os.Environ() {
		key, _, found := strings.Cut(kv, "=")
		if !found || !isDarwinMallocDiagnosticKey(key) {
			continue
		}
		if err := os.Unsetenv(key); err != nil {
			continue
		}
		removed = append(removed, key)
	}
	return removed
}

func isDarwinMallocDiagnosticKey(key string) bool {
	return strings.HasPrefix(key, "MallocStackLogging") ||
		strings.HasPrefix(key, "MallocLogFile")
}

func isMallocDiagnosticLine(line string) bool {
	return strings.Contains(line, "MallocStackLogging:")
}

func copyFilteredMallocDiagnostics(r io.Reader, dst io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if isMallocDiagnosticLine(line) {
			continue
		}
		_, _ = io.WriteString(dst, line+"\n")
	}
}
