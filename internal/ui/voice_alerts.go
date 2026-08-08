package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

// voiceTurnDoneThreshold is how long a turn has to run before its ending is
// worth announcing.
//
// Under it, the answer is its own announcement: it is already on screen and
// read before "done" finishes being spoken. Over it, the person who asked has
// had time to go and do something else, which is the only case where a spoken
// completion tells anyone anything.
const voiceTurnDoneThreshold = 20 * time.Second

// The alert channel: the few moments that need a person.
//
// The other three channels report what the harness is SAYING or DOING. This one
// reports what it is WAITING FOR, and that is the difference between a voice
// you turn off after a day and one you leave on. Reading an answer aloud
// competes with reading it off the screen and loses — you read faster than any
// synthesizer. An approval blocking on someone who is not looking at the
// terminal has no competing channel at all: the run is stopped until a person
// happens to glance over, and nothing else in the harness can tell them.
//
// So this channel is the one that is on by default, and it stays audible even
// under speak_when: unfocused — the whole point is to reach someone who is not
// there.
//
// # Why there is a phrase table here
//
// sonar's interface is in English. The turn's language is not: the harness
// already reads Spanish answers in a Spanish voice, and handing an English
// label to that voice produces the mangled reading the language detection
// exists to avoid. So the four sentences this channel can say are written out
// in both languages the detector can return.
//
// It is deliberately a table of four phrases and not the beginning of an i18n
// layer. Nothing else in sonar is translated, adding a framework for this would
// be a much larger decision than the feature warrants, and the table stops
// being maintainable at roughly the size it already is. If a fifth language
// ever appears in spokenLanguage, it belongs here or the phrase falls back to
// English, which is what an unknown key already does.

// voiceAlert names one thing worth interrupting someone for.
type voiceAlert int

const (
	// alertApprovalNeeded is the one that justifies the channel: a tool is
	// waiting and the run is stopped until someone answers.
	alertApprovalNeeded voiceAlert = iota
	// alertTurnDone closes the loop for someone who walked away.
	alertTurnDone
	// alertTurnFailed is worth its own phrase because "done" and "failed" are
	// the same event from the rail's point of view and opposite ones to hear.
	alertTurnFailed
	// alertTurnCancelled follows an interruption the user asked for, so it is
	// confirmation rather than news.
	alertTurnCancelled
	// alertApprovalNeededFor is the same alert with the action named.
	alertApprovalNeededFor
	// alertContextPressure is opt-in (voice.context_alert). On screen the
	// filling window is ambient state with a meter; from another room it is
	// the difference between "still working" and "about to compact what we
	// said". Off by default because most sessions cross the line routinely
	// and compaction handles it without anyone needing to come back.
	alertContextPressure
)

// alertPhrases maps an alert to what to say, per language.
//
// The English text is the fallback for any language with no entry, which is
// every language the detector cannot name.
var alertPhrases = map[voiceAlert]map[string]string{
	alertApprovalNeeded: {
		"en": "Waiting for your approval.",
		"es": "Espero tu aprobación.",
	},
	alertApprovalNeededFor: {
		// One slot, filled with an action the host already bounded. See
		// speakApprovalNeeded for why this one names its subject when no other
		// alert does.
		"en": "Waiting for your approval to %s.",
		"es": "Espero tu aprobación para %s.",
	},
	alertTurnDone: {
		"en": "Done.",
		"es": "Listo.",
	},
	alertTurnFailed: {
		"en": "The turn failed.",
		"es": "El turno falló.",
	},
	alertTurnCancelled: {
		"en": "Stopped.",
		"es": "Detenido.",
	},
	alertContextPressure: {
		"en": "The context window is past three quarters full.",
		"es": "La ventana de contexto pasó los tres cuartos.",
	},
}

func alertPhrase(alert voiceAlert, language string) string {
	phrases := alertPhrases[alert]
	if phrase := phrases[language]; phrase != "" {
		return phrase
	}
	return phrases["en"]
}

// speakAlert says one of them, in the language the turn is being read in.
//
// It never says WHAT is waiting. An approval names a command, and a command
// read aloud is the noise the whole projection exists to remove — worse here
// than anywhere, because the sentence has to survive being heard from another
// room. "Waiting for your approval" sends someone to the screen, which is where
// the decision has to be made anyway.
func (m *Model) speakAlert(alert voiceAlert) {
	if !m.voiceActive() || !m.voice.config.Alerts {
		return
	}
	// Deliberately not gated on focus. The other channels yield to a person who
	// is looking at the transcript; this one exists for the person who is not,
	// and a listener who IS looking loses four words.
	language := m.voice.spokenLanguageNow()
	// Ahead of whatever the answer channel has queued. Speech runs slower than
	// the model streams, so an answer left running builds a backlog that an
	// alert would sit behind — announcing an approval that has been blocking the
	// run for the whole length of it. It still waits for the sentence being
	// read: interrupting is Stop's job and nothing else's.
	phrase := alertPhrase(alert, language)
	// Recorded as heard, for the stage: somebody who caught only that the
	// harness said SOMETHING can read what it was.
	m.voiceLastAlert = phrase
	m.sayNext(language, phrase)
	m.voice.speaker.Finish()
}

// speakApprovalNeeded says an approval is waiting, and says what for.
//
// Every other alert deliberately withholds its subject: a command read aloud is
// the noise the projection exists to remove, and "go to the screen" is the only
// safe instruction when the decision has to be made there anyway.
//
// This one is the exception, and the rationale that made the rule is what
// breaks it. "Go to the screen" assumes the listener will come; the alert
// channel exists precisely for the person who is not there, and telling them
// only that SOMETHING is waiting makes the trip mandatory to learn whether it
// was worth making. Naming the action — "to run a command", "to write to auto
// command dot go" — is the difference between an interruption and information.
//
// What it says is the host's own bounded label plus the projection, never the
// command text. The projection is the enforcement: a path becomes its filename
// and everything else is dropped, so this cannot leak a shell line into a room.
func (m *Model) speakApprovalNeeded(action string) {
	if !m.voiceActive() || !m.voice.config.Alerts {
		return
	}
	if action = strings.TrimSpace(action); action == "" {
		m.speakAlert(alertApprovalNeeded)
		return
	}
	language := m.voice.spokenLanguageNow()
	phrase := fmt.Sprintf(alertPhrase(alertApprovalNeededFor, language), action)
	m.voiceLastAlert = phrase
	m.sayNext(language, phrase)
	m.voice.speaker.Finish()
}

// spokenApprovalAction turns an approval preview into something worth hearing.
//
// The host already wrote a bounded label for every request — the same one the
// prompt shows — so this reuses it rather than inventing a second vocabulary
// that could describe the same request differently from the screen. The path,
// when there is one, goes through the ordinary projection.
func spokenApprovalAction(preview permission.ApprovalPreview) string {
	action := strings.TrimSpace(sanitizeTerminalSingleLine(preview.ActionLabel))
	if action == "" {
		return ""
	}
	action = strings.ToLower(action)
	if path := strings.TrimSpace(preview.Path); path != "" {
		if spoken := spokenPath(path); spoken != "" {
			action += " " + spoken
		}
	}
	return action
}

// speakContextPressure says the window crossed the pressure line, once per
// crossing. It rides the alert channel's rules — ahead of the backlog,
// indifferent to focus — but behind its own opt-in, because most sessions
// cross the line routinely and compaction handles it unattended.
func (m *Model) speakContextPressure() {
	if !m.voiceActive() || !m.voice.config.ContextAlert {
		return
	}
	if !m.contextPressureHigh() {
		// Re-arm once compaction (or a new conversation) brings usage back
		// under the line, so the next crossing is news again.
		m.voiceContextAlerted = false
		return
	}
	if m.voiceContextAlerted {
		return
	}
	m.voiceContextAlerted = true
	m.speakAlert(alertContextPressure)
}

// speakTurnOutcome reports how a turn ended, once, to whoever is not watching.
//
// Bounded by how long the turn ran. A question answered in four seconds does
// not need to be announced — the answer itself is the announcement, and by the
// time "done" is spoken the reader is already past it. What earns a word is the
// run that took long enough for someone to have left.
func (m *Model) speakTurnOutcome(elapsed time.Duration, failed, cancelled bool) {
	if !m.voiceActive() || !m.voice.config.Alerts {
		return
	}
	if alert, worth := turnOutcomeAlert(elapsed, failed, cancelled); worth {
		m.speakAlert(alert)
	}
}

// turnOutcomeAlert decides which ending is worth a word, if any.
//
// Split from the speaking so the rule can be read and tested as a rule. Failure
// and cancellation always earn one — a failure nobody heard is a run someone
// still believes is going, and a cancellation is the confirmation the person
// who asked is waiting for. Only success is rationed.
func turnOutcomeAlert(elapsed time.Duration, failed, cancelled bool) (voiceAlert, bool) {
	switch {
	case cancelled:
		return alertTurnCancelled, true
	case failed:
		return alertTurnFailed, true
	case elapsed >= voiceTurnDoneThreshold:
		return alertTurnDone, true
	default:
		return alertTurnDone, false
	}
}
