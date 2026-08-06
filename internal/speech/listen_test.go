package speech

import (
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The recorder carries its own stop time.
//
// Every other bound on an utterance lives in the caller, and a caller that was
// killed outright runs no timers. Without this flag an orphaned ffmpeg holds the
// microphone open and writes to a temp file for as long as the machine stays up
// — which is a privacy failure, not a leaked process.
func TestCaptureStopsItselfWithoutTheCaller(t *testing.T) {
	_, args := captureCommand("/tmp/example.wav", MaxUtterance)

	index := slices.Index(args, "-t")
	if index < 0 || index+1 >= len(args) {
		t.Fatalf("the recorder has no duration limit: %v", args)
	}
	seconds, err := strconv.Atoi(args[index+1])
	if err != nil {
		t.Fatalf("-t %q is not a number of seconds ffmpeg accepts", args[index+1])
	}
	if want := int(MaxUtterance.Seconds()); seconds != want {
		t.Fatalf("-t = %ds, want MaxUtterance (%ds)", seconds, want)
	}

	// The destination must stay last: ffmpeg reads everything before it as
	// options belonging to the output, and an argument inserted after it is
	// parsed as a second output file.
	if args[len(args)-1] != "/tmp/example.wav" {
		t.Fatalf("the destination is no longer the final argument: %v", args)
	}
}

// A caller that asks for no limit still gets one. "Unbounded" is not an option
// this package offers, because the process that would honour it is the one that
// may not be there.
func TestCaptureLimitRefusesToBeUnbounded(t *testing.T) {
	for _, limit := range []time.Duration{0, -time.Second} {
		if got := captureLimitSeconds(limit); got != strconv.Itoa(int(MaxUtterance.Seconds())) {
			t.Fatalf("captureLimitSeconds(%v) = %q, want the default bound", limit, got)
		}
	}
}

// Homebrew's whisper-cpp installs exactly one file and it is a fixture.
//
// `for-tests-ggml-tiny.bin` is 575 KB built to make whisper.cpp's own suite
// run, and it turns real speech into noise. Accepting it is worse than finding
// nothing: "dictation is broken" sends someone to look at their microphone,
// while "no model" names the thing to fix. This is the exact shape of a fresh
// install, not an edge case.
func TestModelDiscoveryRejectsTheFixtureHomebrewShips(t *testing.T) {
	for _, testCase := range []struct {
		name string
		want bool
	}{
		{"ggml-base.bin", true},
		{"ggml-large-v3-turbo.bin", true},
		{"ggml-tiny.en.bin", true},
		{"for-tests-ggml-tiny.bin", false},
		{"jfk.wav", false},
		{"ggml-base.bin.tmp", false},
		{"README.md", false},
	} {
		if got := usableWhisperModel(testCase.name); got != testCase.want {
			t.Errorf("usableWhisperModel(%q) = %v, want %v", testCase.name, got, testCase.want)
		}
	}
	// The size floor is the real guard, because the fixture's NAME is a
	// convention nobody promised to keep. Tiny, the smallest real model, is
	// about 75 MB.
	if minimumWhisperModelBytes >= 75<<20 {
		t.Fatalf("the size floor (%d) would reject the tiny model", minimumWhisperModelBytes)
	}
	if minimumWhisperModelBytes <= 1<<20 {
		t.Fatalf("the size floor (%d) would accept the 575 KB fixture", minimumWhisperModelBytes)
	}
}

// A missing model and a missing binary are different problems with different
// commands, and the wrong one wastes an afternoon: "brew install whisper-cpp"
// is advice to reinstall something already present.
func TestMissingModelIsNotMissingTranscriber(t *testing.T) {
	if ErrNoModel == ErrNoTranscriber {
		t.Fatal("the two failures are indistinguishable")
	}
	// A configured path that does not exist is a missing MODEL, not a missing
	// transcriber, and must not be passed to whisper to fail on its own.
	transcriber := LocalTranscriber{Model: filepath.Join(t.TempDir(), "absent.bin")}
	if transcriber.resolveModel() != "" {
		t.Fatal("a configured path that does not exist was passed through")
	}
	if _, err := exec.LookPath(whisperBinary); err == nil {
		if err := transcriber.Ready(); !errors.Is(err, ErrNoModel) {
			t.Fatalf("Ready() = %v, want ErrNoModel", err)
		}
	}
}

// The report names what is missing AND the command that fixes it. A diagnostic
// that says "Model: missing" and stops is where this started.
func TestDiagnosticsNameTheFixNotJustTheFault(t *testing.T) {
	report := Diagnose(filepath.Join(t.TempDir(), "absent.bin"), nil)
	var model *Diagnostic
	for index := range report {
		if report[index].Component == "Model" {
			model = &report[index]
		}
	}
	if model == nil {
		t.Fatal("the report has no Model line")
	}
	if model.OK {
		t.Fatal("a missing model reported OK")
	}
	if !strings.Contains(model.Detail, "absent.bin") {
		t.Fatalf("the report did not name the configured path: %q", model.Detail)
	}
	if !strings.Contains(model.Fix, "curl") || !strings.Contains(model.Fix, "ggml-base.bin") {
		t.Fatalf("the fix is not a command anyone can run: %q", model.Fix)
	}
}
