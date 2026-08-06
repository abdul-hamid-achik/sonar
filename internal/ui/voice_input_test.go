package ui

import (
	"os"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/speech"
)

// Two bounds close the microphone, and the one a user meets must be the one
// that can explain itself.
//
// The UI's timeout says "closed after two minutes" in the footer. The
// recorder's is a backstop for a harness that was killed and left no code to
// run a timer. If the recorder's were the shorter of the two, recording would
// stop mid-sentence with nothing on screen saying why.
func TestTheUITimeoutClosesTheMicrophoneFirst(t *testing.T) {
	if voiceListenTimeout >= speech.MaxUtterance {
		t.Fatalf("the recorder's backstop (%v) fires at or before the UI's timeout (%v), so a recording would end without a message",
			speech.MaxUtterance, voiceListenTimeout)
	}
}

// The microphone must not open while the synthesizer is still talking, because
// the recorder would transcribe the harness reading its own answer back.
//
// This is a source scan for the same reason internal/ui already uses one
// elsewhere: nothing fails at the moment the order is wrong. The key handler
// silences speech before routing any key, so both orderings behave identically
// through every path a test can drive, and the bug is invisible until a caller
// arrives that is not a key press. The order shipped wrong once already.
func TestMicrophoneOpensOnlyAfterSpeechIsSilenced(t *testing.T) {
	source, err := os.ReadFile("voice_input.go")
	if err != nil {
		t.Fatalf("read voice_input.go: %v", err)
	}
	body := string(source)
	const marker = "func (m *Model) startVoiceInput()"
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("startVoiceInput is gone; this test needs rewriting for its replacement")
	}
	body = body[start:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}

	silence := strings.Index(body, "m.silenceVoice()")
	open := strings.Index(body, "listener.Start()")
	if silence < 0 || open < 0 {
		t.Fatalf("startVoiceInput no longer both silences and opens: silence=%d open=%d", silence, open)
	}
	if silence > open {
		t.Fatal("the microphone opens before speech is silenced; the recorder will capture the synthesizer")
	}
}
