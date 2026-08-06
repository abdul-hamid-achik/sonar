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

	// spoken counts the complete sentences of the current answer already said.
	//
	// A COUNT, not the text. The earlier version tracked a byte offset into the
	// projection, which assumed that projecting a prefix of the answer yields a
	// prefix of the projection. It does not: an inline span or a fence that is
	// still open projects one way now and a shorter way once it closes, so the
	// offset slid — mid-rune on accented text — and the channel either repeated
	// a clause or went silent for the rest of the turn. A count only ever moves
	// forward, and the worst a re-projection can now cost is one skipped
	// sentence instead of a corrupted stream.
	spoken int
	// answerLen is how much raw answer the projection has already looked at.
	answerLen int
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
// be recognized from its opening line alone.
func (m *Model) speakAnswerDelta(answerSoFar string) {
	if !m.voiceActive() || !m.voice.config.Answer {
		return
	}
	if !m.answerMayHaveFinishedASentence(answerSoFar) {
		return
	}
	sentences, _ := spokenSentences(spokenStreamingText(answerSoFar))
	m.sayFrom(sentences)
}

// answerMayHaveFinishedASentence skips the projection for a chunk that cannot
// have completed one.
//
// The projection walks the whole answer and runs on every streamed chunk, so a
// turn costs O(n²). Measured on this machine: a 4 KB answer spends 40 ms across
// the turn, a 21 KB answer 190 ms, and a 63 KB answer 720 ms with its worst
// single chunk at 8 ms — half a frame, to produce exactly what the previous
// chunk produced. Most chunks are a few tokens carrying no terminator, and
// without a terminator no new sentence can be complete, so a scan of the bytes
// that just arrived stands in for six regexes over everything.
//
// Every byte is examined exactly once, because the mark advances on every call
// including the ones that skip.
func (m *Model) answerMayHaveFinishedASentence(answerSoFar string) bool {
	seen := m.voice.answerLen
	m.voice.answerLen = len(answerSoFar)
	if len(answerSoFar) < seen {
		// The buffer shrank, so this is a different answer. Never skip on a
		// shrink: the mark belongs to text that no longer exists.
		return true
	}
	return strings.ContainsAny(answerSoFar[seen:], ".!?")
}

// sayFrom speaks whatever is past the position already reached and advances it.
//
// The position never moves backwards. A projection can shrink between chunks —
// a closing fence removes everything it encloses — and treating that as "these
// sentences were never said" is what makes a channel repeat itself.
func (m *Model) sayFrom(sentences []string) {
	for index := m.voice.spoken; index < len(sentences); index++ {
		_ = m.voice.speaker.Say(sentences[index])
	}
	if len(sentences) > m.voice.spoken {
		m.voice.spoken = len(sentences)
	}
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
		sentences, remainder := spokenSentences(spokenText(answer))
		m.sayFrom(sentences)
		if remainder != "" {
			_ = m.voice.speaker.Say(remainder)
		}
	}
	// The next turn is a different answer, so the position starts over. This is
	// the only place it may go back to zero.
	m.voice.spoken = 0
	m.voice.answerLen = 0
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
//
// It cancels the audio and keeps the position. Dropping the position would mean
// the next streamed chunk re-speaks the answer from its first sentence, which
// is the opposite of what interrupting asked for. It also cannot mute the rest
// of the turn instead: this runs on EVERY key press, including the Enter that
// sends the message, so a turn-scoped mute would silence every answer there is.
func (m *Model) silenceVoice() {
	if !m.voiceActive() {
		return
	}
	m.voice.speaker.Stop()
	m.voice.lastActivity = ""
}

func (m *Model) closeVoice() {
	if !m.voiceActive() {
		return
	}
	m.voice.speaker.Close()
	m.voice = nil
}
