package ui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Steering by voice, bounded to what cannot cost anything.
//
// This is the safe half of talking to the harness, and the split is the whole
// design. "Show me the diff" is READ-ONLY: a mis-transcription costs a screen
// nobody wanted. "Approve it" is not: the same mis-transcription costs a
// command nobody wanted. Same microphone, same `base` Whisper model, same
// far-field audio — opposite stakes. So navigation is built and approval is
// not, and the matcher below is shaped so that adding approval later would be
// an entry in a table rather than a second architecture.
//
// Three rules make it safe enough to be worth having:
//
//  1. Closed vocabulary. A transcription is a command only if it matches a
//     known phrase, never if it merely contains one.
//  2. Whole utterance. "Mostrame el diff" is a command; "mostrame el diff y
//     arreglá el bug" is dictation, because the second half is a request and
//     swallowing it would drop what somebody asked for.
//  3. Nothing here sends a prompt, answers an approval, cancels a turn, or
//     changes any setting that survives the session. Every action is a view
//     change or a repeat of something already said.
//
// Cancelling a turn is deliberately absent even though it is tempting. A
// mis-heard "stop" that kills a two-hour AUTO run is expensive, and Escape is
// already the stop that works without a microphone.

type voiceCommand int

const (
	voiceCommandNone voiceCommand = iota
	// voiceCommandRepeat is the one that justifies the feature: the natural
	// thing to say when you missed what was said is "again", and until now the
	// only way to get it was to go and read.
	voiceCommandRepeat
	voiceCommandQuiet
	voiceCommandDiff
	voiceCommandOutput
	voiceCommandStage
	voiceCommandTranscript
)

// voiceCommandPhrases is the closed vocabulary, in both languages the detector
// can produce.
//
// Every entry is a whole utterance somebody would actually say, not a keyword.
// They are matched after case-folding and stripping trailing punctuation, and
// nothing else — no stemming, no fuzzy distance. A near-miss is dictation, and
// dictation lands in the composer where it can be read before it does anything.
var voiceCommandPhrases = map[string]voiceCommand{
	// Repeat.
	"otra vez": voiceCommandRepeat, "repetí": voiceCommandRepeat,
	"repeti": voiceCommandRepeat, "repetilo": voiceCommandRepeat,
	"de nuevo": voiceCommandRepeat, "repeat": voiceCommandRepeat,
	"say that again": voiceCommandRepeat, "again": voiceCommandRepeat,

	// Silence — and honestly, a narrow window. Opening the microphone with
	// ctrl+g silences speech before it records, so most of the time this phrase
	// has nothing left to stop. What it does cover is speech that STARTS while
	// the microphone is already open: an alert jumps the queue and ignores
	// focus, so it can arrive mid-utterance. Kept for that, not for the case
	// the phrase sounds like it covers.
	"callate": voiceCommandQuiet, "cállate": voiceCommandQuiet,
	"silencio": voiceCommandQuiet, "pará": voiceCommandQuiet,
	"quiet": voiceCommandQuiet, "stop talking": voiceCommandQuiet,
	"be quiet": voiceCommandQuiet,

	// The diff.
	"diff": voiceCommandDiff, "el diff": voiceCommandDiff,
	"mostrame el diff": voiceCommandDiff, "muéstrame el diff": voiceCommandDiff,
	"muestrame el diff": voiceCommandDiff, "show the diff": voiceCommandDiff,
	"show me the diff": voiceCommandDiff,

	// The output of the inspected receipt.
	"output": voiceCommandOutput, "la salida": voiceCommandOutput,
	"mostrame la salida": voiceCommandOutput, "show the output": voiceCommandOutput,
	"show me the output": voiceCommandOutput,

	// The two screens.
	"panel": voiceCommandStage, "el panel": voiceCommandStage,
	"stage": voiceCommandStage, "show the panel": voiceCommandStage,
	"transcript": voiceCommandTranscript, "el transcript": voiceCommandTranscript,
	"volver": voiceCommandTranscript, "atrás": voiceCommandTranscript,
	"atras": voiceCommandTranscript, "back": voiceCommandTranscript,
	"go back": voiceCommandTranscript,
}

// voiceCommandFor matches a whole transcription against the vocabulary.
//
// Returns voiceCommandNone for anything else, which is the common case and the
// safe one: unmatched speech is dictation, and dictation is inserted rather
// than acted on.
func voiceCommandFor(transcript string) voiceCommand {
	normalized := strings.ToLower(strings.TrimSpace(transcript))
	normalized = strings.Trim(normalized, ".,;:!?¡¿\"'")
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		return voiceCommandNone
	}
	return voiceCommandPhrases[normalized]
}

// runVoiceCommand performs one, and reports what it did.
//
// It says what happened in the footer rather than out loud. Somebody who asked
// to be SHOWN something is about to look at the screen, so speaking the
// confirmation would talk over the thing they asked for — except for the two
// commands that are themselves audible, where the audio is the confirmation.
func (m *Model) runVoiceCommand(command voiceCommand) tea.Cmd {
	switch command {
	case voiceCommandRepeat:
		if !m.voiceActive() {
			return m.setFooterNotice(noticeInfo, "Spoken output is off — nothing to repeat.", 3*time.Second)
		}
		digest := strings.TrimSpace(m.voiceLastDigest)
		if digest == "" {
			return m.setFooterNotice(noticeInfo, "Nothing has been read out yet.", 3*time.Second)
		}
		// The backlog goes first. "Again" means the listener missed the one line
		// that mattered, and queueing the repeat behind several minutes of AUTO
		// narration answers a different question than the one they asked.
		m.voice.speaker.DropPending()
		// Read in the language it was READ in, not the one the session has
		// drifted to since. A digest from three turns ago is a Spanish sentence
		// however English the current turn is.
		language := m.voiceLastDigestLanguage
		if language == "" {
			language = m.voice.spokenLanguageNow()
		}
		sentences, remainder := spokenSentences(spokenText(digest))
		for _, sentence := range sentences {
			m.say(language, sentence)
		}
		if remainder != "" {
			m.say(language, remainder)
		}
		m.voice.speaker.Finish()
		return nil

	case voiceCommandQuiet:
		m.silenceVoice()
		return nil

	case voiceCommandDiff:
		return m.dispatchInspectedToolAction(toolOpenDiffActionID)

	case voiceCommandOutput:
		return m.dispatchInspectedToolAction(toolOpenOutputActionID)

	case voiceCommandStage:
		if m.voiceStageActive() {
			return nil
		}
		return m.toggleVoiceStage()

	case voiceCommandTranscript:
		// "Back" means one step out of wherever you are, and the stage yields to
		// viewers — so after "show me the diff", the stage is already inactive
		// and this used to do nothing at all. The detour closes first; the
		// second "back" then leaves the stage.
		if m.viewerModalActive() {
			return m.closeTopViewer()
		}
		if m.overlay != OverlayNone {
			m.overlay = OverlayNone
			m.refreshTranscript()
			return nil
		}
		if !m.voiceStageActive() {
			return nil
		}
		m.voiceStage = false
		m.refreshTranscript()
		return nil
	}
	return nil
}
