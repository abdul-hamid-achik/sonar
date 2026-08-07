package ui

import (
	"testing"
)

// The vocabulary is closed and matched whole, and both halves of that are what
// make voice steering safe enough to build at all.
//
// Read-only is the other half: a mis-transcription here costs a screen nobody
// wanted. The same microphone answering an approval would cost a command nobody
// wanted — same `base` Whisper model, same far-field audio, opposite stakes.
func TestVoiceSteeringOnlyMatchesAWholeKnownPhrase(t *testing.T) {
	for phrase, want := range map[string]voiceCommand{
		"mostrame el diff":   voiceCommandDiff,
		"Mostrame el diff.":  voiceCommandDiff, // case and punctuation are noise
		"  otra   vez  ":     voiceCommandRepeat,
		"callate":            voiceCommandQuiet,
		"show me the output": voiceCommandOutput,
		"volver":             voiceCommandTranscript,
	} {
		if got := voiceCommandFor(phrase); got != want {
			t.Errorf("voiceCommandFor(%q) = %d, want %d", phrase, got, want)
		}
	}

	// Containing a command is not being one. The second half of each of these
	// is a request, and swallowing it would drop what somebody asked for.
	for _, dictation := range []string{
		"mostrame el diff y arreglá el bug",
		"repetí el test que falló",
		"no, callate un segundo y después seguí con el deploy",
		"el diff está mal, revisalo",
		"",
		"cualquier otra cosa que alguien diga",
	} {
		if got := voiceCommandFor(dictation); got != voiceCommandNone {
			t.Errorf("dictation was taken as command %d: %q", got, dictation)
		}
	}
}

// Nothing in the vocabulary may reach an action that costs something.
//
// A table test cannot prove that by itself, so this pins the boundary the
// design depends on: the set of commands is exactly the read-only ones, and a
// new entry has to be added here deliberately to exist at all.
func TestVoiceSteeringReachesNothingDestructive(t *testing.T) {
	allowed := map[voiceCommand]bool{
		voiceCommandRepeat: true, voiceCommandQuiet: true,
		voiceCommandDiff: true, voiceCommandOutput: true,
		voiceCommandStage: true, voiceCommandTranscript: true,
	}
	for phrase, command := range voiceCommandPhrases {
		if !allowed[command] {
			t.Errorf("%q reaches command %d, which is not in the read-only set", phrase, command)
		}
	}
	// And the inert model runs every one of them without touching anything it
	// should not: no speaker, no receipt, no stage.
	m := newTestModel(t)
	for command := range allowed {
		m.runVoiceCommand(command)
	}
}

// A spoken command is performed instead of being typed into the composer, and
// dictation still lands there.
func TestASpokenCommandDoesNotBecomeADraft(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.voiceInput = &voiceInputState{}
	m.voiceInput.token = 1
	m.voiceLastDigest = "Ya quedó."

	m.handleVoiceTranscript(VoiceTranscriptMsg{Token: 1, Text: "otra vez"})
	if draft := m.input.Value(); draft != "" {
		t.Fatalf("a command was typed into the composer: %q", draft)
	}

	m.voiceInput.token = 2
	m.handleVoiceTranscript(VoiceTranscriptMsg{Token: 2, Text: "arreglá el canal de voz"})
	if draft := m.input.Value(); draft == "" {
		t.Fatal("dictation did not reach the composer")
	}
}

// A digest that projects to nothing must not take the narration with it.
//
// The projection is lossy on purpose — a line that is only an inline span or a
// path reduces to nothing — and the queue was being dropped BEFORE that was
// known. The listener got silence instead of the narration that was already
// queued and would have been fine.
func TestAnEmptyDigestDoesNotBuySilence(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.speakAnswerDelta("Primera frase. Segunda frase.")
	spokenBefore := m.voice.spoken
	if spokenBefore == 0 {
		t.Fatal("precondition: nothing was queued to lose")
	}

	// A digest whose entire content is a path: it projects to nothing.
	m.speakSegmentEnd("Listo.\n\n<!--spoken: ./...-->")
	if m.voice.digestSpoken != "" && m.voiceLastDigest != "" && m.voice.spoken != spokenBefore {
		t.Fatalf("an unspeakable digest disturbed the narration: %d -> %d", spokenBefore, m.voice.spoken)
	}
}

// "Back" means one step out of wherever you are.
//
// The stage yields to viewers, so after "show me the diff" the stage is already
// inactive and "back" used to do nothing at all — the one word most likely to
// be said next after opening a detour.
func TestBackLeavesTheDetourBeforeTheStage(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.ready, m.width, m.height = true, 100, 30
	m.voiceStage = true

	// With an overlay open, back closes the overlay and leaves the stage up.
	m.overlay = OverlayHelp
	m.runVoiceCommand(voiceCommandTranscript)
	if m.overlay != OverlayNone {
		t.Fatal("back did not close the overlay")
	}
	if !m.voiceStageActive() {
		t.Fatal("back left the stage as well as the overlay")
	}

	// The second back leaves the stage.
	m.runVoiceCommand(voiceCommandTranscript)
	if m.voiceStageActive() {
		t.Fatal("back did not leave the stage")
	}
}

// A repeated line is read in the language it was read in, not the one the
// session has drifted to since.
func TestRepeatKeepsTheLanguageItWasSaidIn(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.voiceLastDigest = "Ya quedó todo listo."
	m.voiceLastDigestLanguage = "es"
	// The session has moved on to English since.
	m.beginVoiceTurn("Now check the tests and report back to me please.")

	m.runVoiceCommand(voiceCommandRepeat)
	if m.voiceLastDigestLanguage != "es" {
		t.Fatalf("the digest lost the language it was said in: %q", m.voiceLastDigestLanguage)
	}
}
