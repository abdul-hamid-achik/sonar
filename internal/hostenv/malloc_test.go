package hostenv

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestScrubDarwinMallocDiagnosticsUnsetsKnownKeys(t *testing.T) {
	t.Setenv("MallocStackLogging", "1")
	t.Setenv("MallocStackLoggingNoCompact", "1")
	t.Setenv("MallocLogFile", "/tmp/malloc.log")
	t.Setenv("PATH", "/usr/bin")

	removed := ScrubDarwinMallocDiagnostics()
	for _, key := range []string{"MallocStackLogging", "MallocStackLoggingNoCompact", "MallocLogFile"} {
		if !slices.Contains(removed, key) {
			t.Errorf("scrub did not report %s", key)
		}
		if _, ok := os.LookupEnv(key); ok {
			t.Errorf("%s still set after scrub", key)
		}
	}
	if got := os.Getenv("PATH"); got != "/usr/bin" {
		t.Fatalf("PATH = %q, want kept", got)
	}
}

func TestMallocDiagnosticLineMatchIsNarrow(t *testing.T) {
	if !isMallocDiagnosticLine("sonar(10489) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.") {
		t.Fatal("diagnostic line was not recognized")
	}
	if isMallocDiagnosticLine("error: file not found") {
		t.Fatal("ordinary stderr was treated as a malloc diagnostic")
	}
}

func TestDarwinMallocDiagnosticKeyMatchIsPrefix(t *testing.T) {
	if !isDarwinMallocDiagnosticKey("MallocStackLoggingNoCompact") {
		t.Fatal("prefixed stack-logging key was not recognized")
	}
	if isDarwinMallocDiagnosticKey("MALLOC_CHECK_") {
		t.Fatal("glibc malloc key was treated as Darwin diagnostic")
	}
}

func TestCopyFilteredMallocDiagnosticsDropsOnlyThatLine(t *testing.T) {
	input := strings.Join([]string{
		"real error: boom",
		"sonar(10489) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.",
		"still here",
	}, "\n") + "\n"
	var out bytes.Buffer
	copyFilteredMallocDiagnostics(strings.NewReader(input), &out)
	got := out.String()
	if strings.Contains(got, "MallocStackLogging:") {
		t.Fatalf("diagnostic leaked:\n%s", got)
	}
	if !strings.Contains(got, "real error: boom") || !strings.Contains(got, "still here") {
		t.Fatalf("kept lines missing:\n%s", got)
	}
}
