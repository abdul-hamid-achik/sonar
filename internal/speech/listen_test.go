package speech

import (
	"slices"
	"strconv"
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
