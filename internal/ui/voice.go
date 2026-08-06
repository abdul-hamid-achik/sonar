package ui

import (
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/speech"
)

// Voice output: three channels, separately switchable, one of them on.
//
// Answer, reasoning, and activity are three different things to hear, and a
// single on/off switch makes that choice for the listener badly. An AUTO turn
// produces eight tool receipts and several reasoning blocks; spoken together
// they bury the one sentence that was worth waiting for. So each is its own
// channel, and only the answer speaks by default.
//
// The projection is in voice_projection.go and is where the value is. This file
// owns only WHEN each channel speaks and what silences it.
type voiceState struct {
	speaker *speech.Speaker
	config  config.VoiceConfig

	// pending is the not-yet-complete tail of the answer. Streaming delivers
	// partial sentences, and a sentence is spoken only once it is whole.
	pending string
	// spokenActivity is the last activity line said aloud, so a turn that reads
	// four files in a row does not say "reading" four times.
	lastActivity string
}

// StartVoice installs a speaker when the operator asked for one and the host
// has a synthesizer, and returns a notice to print when voice was requested and
// cannot be delivered.
//
// A request the host cannot honour is reported rather than silently dropped:
// someone who enabled voice and hears nothing should be told which of the two
// halves is missing.
func (m *Model) StartVoice(cfg config.VoiceConfig) string {
	if !cfg.Enabled {
		return ""
	}
	if !speech.Available() {
		return "voice: enabled in config but this host has no synthesizer; nothing will be spoken"
	}
	speaker, err := speech.New(cfg.Voice, cfg.Rate)
	if err != nil {
		return "voice: " + err.Error()
	}
	m.voice = &voiceState{speaker: speaker, config: cfg}
	return ""
}

// voiceActive reports whether a voice policy is installed at all.
//
// It deliberately does not require a speaker. The channel policy and the
// synthesizer are separate concerns: every Speaker method is nil-safe, so the
// policy can be exercised — and tested — without spawning a process and
// claiming an audio device. In production StartVoice installs the policy only
// once a speaker exists, so the two are never apart outside a test.
func (m *Model) voiceActive() bool {
	return m != nil && m.voice != nil
}

// speakAnswerDelta offers newly streamed answer text to the answer channel.
//
// It is called with the WHOLE answer so far rather than the delta, because the
// projection has to see complete markdown — a fence that is still open cannot
// be recognized from its opening line alone. What has already been spoken is
// tracked by length, not by re-reading.
func (m *Model) speakAnswerDelta(answerSoFar string) {
	if !m.voiceActive() || !m.voice.config.Answer {
		return
	}
	projected := spokenText(answerSoFar)
	if len(projected) <= len(m.voice.pending) {
		return
	}
	sentences, remainder := spokenSentences(projected[len(m.voice.pending):])
	for _, sentence := range sentences {
		_ = m.voice.speaker.Say(sentence)
		m.voice.pending += sentence + " "
	}
	_ = remainder
}

// speakTurnEnd flushes whatever the turn ended on.
//
// A turn rarely ends on a period, and a final clause held back forever is the
// one sentence a listener most wanted. Ending the turn is the boundary that
// makes it complete.
func (m *Model) speakTurnEnd(answer string) {
	if !m.voiceActive() {
		return
	}
	if m.voice.config.Answer {
		projected := spokenText(answer)
		if tail := strings.TrimSpace(strings.TrimPrefix(projected, strings.TrimSpace(m.voice.pending))); tail != "" {
			_ = m.voice.speaker.Say(tail)
		}
	}
	m.voice.pending = ""
	m.voice.lastActivity = ""
}

// speakReasoning offers a settled reasoning block to the reasoning channel.
//
// Off by default and spoken only once settled, not streamed. Reasoning is
// exploratory by nature — it contradicts itself, abandons paths, and is far
// longer than the answer — so interleaving it sentence by sentence with the
// answer would produce two voices arguing. Whoever turns this on is asking to
// hear the thinking, and hears it as one block.
func (m *Model) speakReasoning(reasoning string) {
	if !m.voiceActive() || !m.voice.config.Reasoning {
		return
	}
	projected := spokenText(reasoning)
	if projected == "" {
		return
	}
	sentences, remainder := spokenSentences(projected)
	for _, sentence := range sentences {
		_ = m.voice.speaker.Say(sentence)
	}
	if remainder != "" {
		_ = m.voice.speaker.Say(remainder)
	}
}

// speakActivity offers one tool receipt to the activity channel.
//
// It repeats nothing: a turn that reads four files says "reading" once, because
// the transcript's own collapsed run already decided that four reads are one
// event worth reporting.
func (m *Model) speakActivity(label, summary string) {
	if !m.voiceActive() || !m.voice.config.Activity {
		return
	}
	line := spokenActivity(label, summary)
	if line == "" || line == m.voice.lastActivity {
		return
	}
	m.voice.lastActivity = line
	_ = m.voice.speaker.Say(line)
}

// silenceVoice cancels speech immediately and drops anything not yet said.
//
// Every voice-interface source agrees on this and it is the gap every existing
// implementation leaves open: interruption cancels, it never queues. A user who
// types while the harness is talking has stopped listening, and audio that
// keeps going is talking over them.
func (m *Model) silenceVoice() {
	if !m.voiceActive() {
		return
	}
	m.voice.speaker.Stop()
	m.voice.pending = ""
	m.voice.lastActivity = ""
}

func (m *Model) closeVoice() {
	if !m.voiceActive() {
		return
	}
	m.voice.speaker.Close()
	m.voice = nil
}
