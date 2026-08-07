package speech

import (
	"context"
	"errors"
	"os"
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

// The meter calibrates to the room, because a fixed scale cannot.
//
// Measured on a real machine, an empty room reports between -36 and -21 LUFS —
// nowhere near the digital silence a fixed floor assumed. Anchored at -60, room
// tone alone rendered at four fifths of full, so the meter was pinned near the
// top before anyone spoke and had no range left for a voice. That is exactly
// what it looked like: always lit, barely moving.
func TestTheLevelMeterCalibratesToTheRoom(t *testing.T) {
	listener := &Listener{}
	// resetLevel is what a real Start does, and Hearing reports nothing without
	// it: "nothing yet" and "nothing for a while" are answered from the moment
	// recording began.
	listener.resetLevel()

	// Frames from before the device delivers audio are not a quiet room, and
	// calibrating on one would put the room's own tone back at the ceiling.
	listener.noteLoudness(listener.epoch, -120.7)
	if got := listener.Level(); got != 0 {
		t.Fatalf("a pre-audio frame produced a reading: %v", got)
	}

	// A noisy room settles at zero once it is the floor.
	for range 5 {
		listener.noteLoudness(listener.epoch, -25)
	}
	if got := listener.Level(); got > 0.01 {
		t.Fatalf("room tone did not settle to the bottom of the scale: %v", got)
	}
	if heard, _ := listener.Hearing(); heard {
		t.Fatal("room tone counted as somebody speaking")
	}

	// A voice well above that floor uses the scale.
	listener.noteLoudness(listener.epoch, -8)
	if got := listener.Level(); got < 0.5 {
		t.Fatalf("speech over room tone barely moved the meter: %v", got)
	}
	if heard, _ := listener.Hearing(); !heard {
		t.Fatal("speech over room tone did not count as heard")
	}

	// And a QUIET room reaches the same place from the other side, which is the
	// property a fixed scale cannot have: the two differ by more than the
	// distance between silence and speech.
	quiet := &Listener{}
	quiet.resetLevel()
	for range 5 {
		quiet.noteLoudness(quiet.epoch, -55)
	}
	if got := quiet.Level(); got > 0.01 {
		t.Fatalf("a quiet room did not settle to the bottom either: %v", got)
	}
	quiet.noteLoudness(quiet.epoch, -38)
	if got := quiet.Level(); got < 0.5 {
		t.Fatalf("speech in a quiet room barely moved the meter: %v", got)
	}
}

// A recording's levels may not leak into the next one.
//
// Stop waits for the recorder but not for the goroutine reading its
// diagnostics, and os/exec closes that pipe when the process is reaped — so a
// reader can still hold a buffered line when the NEXT recording has already
// started. The mutex prevents a data race; it does not prevent one recording's
// levels calibrating another's meter.
func TestAnOldReaderCannotCalibrateANewRecording(t *testing.T) {
	listener := &Listener{}
	stale := listener.resetLevel()
	for range 4 {
		listener.noteLoudness(stale, -25) // a noisy room
	}
	if !listener.calibrated {
		t.Fatal("precondition: the first recording did not calibrate")
	}

	// A new recording begins in a quiet room.
	fresh := listener.resetLevel()
	listener.noteLoudness(fresh, -55)

	// The old reader finally drains a line it had buffered.
	listener.noteLoudness(stale, -8)

	if heard, _ := listener.Hearing(); heard {
		t.Fatal("a line from the previous recording counted as speech in this one")
	}
	if got := listener.Level(); got > 0.01 {
		t.Fatalf("a stale reading moved the new recording's meter: %v", got)
	}
}

// The transcript sidecar is removed even when transcription fails.
//
// whisper writes its output beside the recording, so a run that errors or is
// cancelled can leave that file behind — and it is not a stray temporary, it is
// what somebody said, left in a shared directory with nothing to remove it.
func TestAFailedTranscriptionLeavesNothingBehind(t *testing.T) {
	directory := t.TempDir()
	wav := filepath.Join(directory, "utterance.wav")
	if err := os.WriteFile(wav, []byte("not audio"), 0o600); err != nil {
		t.Fatalf("stage a recording: %v", err)
	}
	// Stand in for what whisper would have written before failing.
	sidecar := wav + ".txt"
	if err := os.WriteFile(sidecar, []byte("lo que dijo la persona"), 0o600); err != nil {
		t.Fatalf("stage a transcript: %v", err)
	}

	// A cancelled context makes the run fail after the file exists, which is
	// the exact shape of the leak.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	transcriber := LocalTranscriber{}
	if transcriber.Ready() != nil {
		t.Skip("no local transcriber on this host")
	}
	_, _ = transcriber.Transcribe(ctx, wav)

	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatal("a failed transcription left the transcript on disk")
	}
}
