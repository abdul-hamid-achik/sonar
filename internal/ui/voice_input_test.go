package ui

import (
	"os"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/command"
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

// Dictation must be reachable without a key binding.
//
// alt+<letter> does not exist on a stock macOS terminal — Option composes a
// character and the app is sent nothing — and where it does exist a leader key
// or a multiplexer can claim it first. This is the way in that no terminal can
// intercept, so it has to actually dispatch and actually be listed.
func TestDictationIsReachableWithoutAKeyBinding(t *testing.T) {
	registry := command.NewRegistry()
	command.RegisterBuiltins(registry)

	for _, name := range []string{"voice", "listen", "mic"} {
		result := registry.Execute(&command.Context{}, name, nil)
		if result.Action != command.ActionVoiceInput {
			t.Fatalf("/%s dispatched %v (error %q), want ActionVoiceInput",
				name, result.Action, result.Error)
		}
	}

	m := newTestModel(t)
	m.cmdRegistry = registry
	if !strings.Contains(m.buildHelpContent(80), "/voice") {
		t.Fatal("the help overlay does not list /voice, so nobody whose terminal eats alt+v can find it")
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

// The hint has to name a stop that works in this terminal.
//
// It used to say only "alt+v to stop", which is exactly wrong for whoever
// needed it most: someone who opened the microphone with /voice because alt+v
// never reaches the app is then told to press alt+v to close it. Escape is the
// one key no terminal or leader can intercept, so it is named first.
func TestListeningHintNamesAStopThatAlwaysWorks(t *testing.T) {
	m := newTestModel(t)
	m.voiceInput = &voiceInputState{}
	activity, ok := m.currentWorkingActivity()
	if ok && activity.label == "Listening" {
		t.Fatal("precondition: a Listener-less state should not report listening")
	}

	// The copy itself is what this pins, since driving a real microphone in a
	// unit test is not available.
	source, err := os.ReadFile("activity.go")
	if err != nil {
		t.Fatalf("read activity.go: %v", err)
	}
	body := string(source)
	start := strings.Index(body, `label: "Listening"`)
	if start < 0 {
		t.Fatal("the Listening activity is gone; this test needs rewriting")
	}
	hint := body[start:min(start+200, len(body))]
	if !strings.Contains(hint, "esc") {
		t.Fatalf("the listening hint does not name escape: %q", hint)
	}
	if !strings.Contains(hint, "/voice") {
		t.Fatalf("the listening hint names no terminal-independent stop: %q", hint)
	}
}
