package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

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
//
// # Two clocks, and the one that used to be mistaken for the other
//
// A provider SEGMENT is not a TURN. The ReAct loop ends a segment at every
// model response — once per tool round, once per AUTO continuation, and once
// more when a capped request has to charge an unaccounted reservation — while
// the turn a listener perceives is the whole thing from their message to the
// harness going quiet. Everything scoped per segment lives with the stream
// buffer it indexes into; everything scoped per turn is seeded at the top of
// this file's turn hook and survives every segment inside it.
//
// Both were segment-scoped once, and that is where the reported behaviour came
// from: the language verdict was thrown away at every tool call, so a short
// segment carrying no evidence ("Voy a revisar los archivos.") fell back to the
// host default and read Spanish in an English voice.
type voiceState struct {
	speaker *speech.Speaker
	config  config.VoiceConfig

	// spoken counts the complete sentences of the current SEGMENT already said.
	//
	// A COUNT, not the text. The earlier version tracked a byte offset into the
	// projection, which assumed that projecting a prefix of the answer yields a
	// prefix of the projection. It does not: an inline span or a fence that is
	// still open projects one way now and a shorter way once it closes, so the
	// offset slid — mid-rune on accented text — and the channel either repeated
	// a clause or went silent for the rest of the turn. A count only ever moves
	// forward, and the worst a re-projection can now cost is one skipped
	// sentence instead of a corrupted stream.
	//
	// It indexes into the live stream buffer, so it is reset by the one function
	// that empties that buffer and by nothing else. Resetting it anywhere else
	// is how an answer gets read out twice.
	spoken int
	// answerLen is how much raw answer the projection has already looked at.
	answerLen int
	// spokenReasoning is the reasoning block already read out, for the same
	// reason as spokenRemainder below: the reasoning channel speaks a settled
	// block at a segment end, and a segment can end twice on one block.
	spokenReasoning string
	// spokenRemainder is the incomplete tail a segment end already flushed.
	//
	// It exists because a segment can end twice on the same text: a capped
	// request that comes back without trustworthy accounting charges the
	// unaccounted reservation through a second terminal receipt, and the tail is
	// the one part a sentence count cannot protect.
	spokenRemainder string
	// lastActivity is the last activity line said aloud, so a turn that reads
	// four files in a row does not say "reading" four times.
	lastActivity string
	// language is the turn's language once the answer itself has settled it, and
	// languageKnown is what distinguishes "decided on nothing yet" from "decided
	// it is English". Turn-scoped: a segment boundary must not clear it.
	language      string
	languageKnown bool
	// digestSpoken is the summary already read out this turn, so a segment that
	// ends twice on the same text does not say it twice.
	digestSpoken string
	// tables holds the compiled respellings per language, built on first use.
	// See voice_pronunciation.go: an English word read by a Spanish voice comes
	// out as a different word, not as an accent.
	tables map[string]*pronunciationTable
	// seed is what to read in until the answer proves otherwise — the language
	// of the user's own message, which is evidence the harness has before a
	// single token arrives and is right nearly every time. It survives a turn
	// that says nothing new, so a session that has been Spanish for ten
	// exchanges does not flip to English on a prompt of "dale".
	seed string
}

// StartVoice installs a speaker when the operator asked for one and the host
// has a synthesizer, and returns a notice to print when voice was requested and
// cannot be delivered.
//
// A request the host cannot honour is reported rather than silently dropped:
// someone who enabled voice and hears nothing should be told which of the two
// halves is missing.
func (m *Model) StartVoice(cfg config.VoiceConfig) string {
	// Kept whether or not it is on, because it is what /voice on has to build
	// from: the voice map, the rate and the pronunciation corrections are the
	// operator's settings, not the switch. Before this, starting with voice off
	// meant those settings did not exist in the session at all, so turning it on
	// at runtime could only have produced host defaults.
	m.voiceConfig = cfg
	if !cfg.Enabled {
		return ""
	}
	if notice, ok := m.openVoice(); !ok {
		return notice
	}
	return ""
}

// openVoice builds the speaker from the session's voice settings.
//
// Returns a notice rather than logging one, because the two callers report it
// differently: at startup it is a line printed before the TUI takes the screen,
// and at runtime it is a footer notice answering a command somebody just typed.
func (m *Model) openVoice() (string, bool) {
	if m.voice != nil {
		return "", true
	}
	if !speech.Available() {
		return "voice: this host has no synthesizer; nothing would be spoken", false
	}
	cfg := m.voiceConfig
	speaker, err := speech.NewWithProvider(cfg.Provider, cfg.Voice, cfg.Rate, cfg.Voices)
	if err != nil {
		return "voice: " + err.Error(), false
	}
	// Enabled is what the switch means, so an installed speaker always carries
	// it — otherwise /voice status would report the feature off while it speaks.
	cfg.Enabled = true
	m.voice = &voiceState{speaker: speaker, config: cfg}
	return "", true
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

// beginVoiceTurn opens a turn and decides what language to read it in before
// the model has said anything.
//
// The user's own message is the evidence. Waiting for the answer to prove its
// language means the first sentences are read in the host default, and they are
// exactly the sentences that carry no function words to go on — "Listo.",
// "Ya está.", "Voy a revisar." decide nothing and are most of what a coding
// agent opens with. The prompt has already been written by then, it is free to
// look at, and it agrees with the answer nearly every time.
//
// This is per TURN, not per segment: it runs where a user message is
// dispatched, and an AUTO continuation does not come through here.
func (m *Model) beginVoiceTurn(prompt string) {
	if !m.voiceActive() {
		return
	}
	m.voice.language, m.voice.languageKnown = "", false
	m.voice.lastActivity, m.voice.digestSpoken = "", ""
	if seed := spokenLanguage(prompt); seed != "" {
		m.voice.seed = seed
	}
	// A prompt with no evidence keeps the previous turn's seed rather than
	// clearing it. "sí", "dale" and "ok" are the shortest messages people send
	// and the least diagnostic, and a session does not change language because
	// one of them was sent.
}

// forgetSpokenAnswer starts the answer position over.
//
// Called from the one function that empties the stream buffer, because the
// position is an index into that buffer and means nothing without it. It used
// to be reset at the end of every provider segment instead, which is a
// different moment: the buffer survives a segment end until the transcript
// flush, so any further text — a second terminal receipt, an AUTO segment that
// continues before the flush — was measured against a position of zero and read
// the whole answer out again.
func (m *Model) forgetSpokenAnswer() {
	if !m.voiceActive() {
		return
	}
	m.voice.spoken, m.voice.answerLen = 0, 0
	m.voice.spokenRemainder, m.voice.spokenReasoning = "", ""
}

// speakAnswerDelta offers newly streamed answer text to the answer channel.
//
// It is called with the WHOLE answer so far rather than the delta, because the
// projection has to see complete markdown — a fence that is still open cannot
// be recognized from its opening line alone.
func (m *Model) speakAnswerDelta(answerSoFar string) {
	if !m.voiceActive() || !m.voice.config.Answer || !m.voiceMayNarrate() {
		return
	}
	if !m.answerMayHaveFinishedASentence(answerSoFar) {
		return
	}
	projected := spokenStreamingText(answerSoFar)
	sentences, _ := spokenSentences(projected)
	m.sayFrom(sentences, m.turnLanguage(projected))
}

// turnLanguage decides what language the answer is in, at most once per turn.
//
// Once, because the verdict gets steadier as more of the answer arrives rather
// than truer, and because a voice that changes twice in one answer sounds
// broken even when each choice was defensible. Until it is decided the seed
// answers — so the opening sentences are read in the language the user wrote
// in, not in the host default.
//
// The switch from seed to verdict is now safe to make mid-answer: the speaker
// queues, so a new voice starts after the current sentence finishes rather than
// on top of it.
func (m *Model) turnLanguage(projected string) string {
	if m.voice.languageKnown {
		return m.voice.language
	}
	language := spokenLanguage(projected)
	if language == "" {
		// Not yet decided. Thin evidence is a reason to look again with more
		// text, not a verdict — so this deliberately does not latch.
		return m.voice.seed
	}
	m.voice.language, m.voice.languageKnown = language, true
	return language
}

// spokenLanguageNow is the turn's language as currently understood, for the
// channels that do not get a vote in deciding it.
//
// The answer owns the verdict. A tool label is one word of English UI text and
// a reasoning block is often in a different language from the answer it
// precedes; neither is evidence, and neither is worth starting a second
// synthesizer for.
func (v *voiceState) spokenLanguageNow() string {
	if v.languageKnown {
		return v.language
	}
	return v.seed
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
func (m *Model) sayFrom(sentences []string, language string) {
	for index := m.voice.spoken; index < len(sentences); index++ {
		m.say(language, sentences[index])
	}
	if len(sentences) > m.voice.spoken {
		m.voice.spoken = len(sentences)
	}
}

// speakSegmentEnd flushes whatever a provider segment ended on.
//
// A segment rarely ends on a period, and a final clause held back forever is
// the one sentence a listener most wanted. The end of a model response is the
// boundary that makes it complete.
//
// What it does NOT do is treat that as the end of the turn. It used to clear
// the language verdict and the answer position here, which is why a turn with
// tool calls re-decided its language at every round and could read its own
// answer a second time. The speaker is told to drain — that part is per segment
// and always was, because it is what lets "is it still talking?" be answered —
// and draining no longer costs anything, since a later sentence waits for it
// instead of killing it.
func (m *Model) speakSegmentEnd(answer string) {
	if !m.voiceActive() {
		return
	}
	if m.voice.config.Answer {
		// The digest is recorded whether or not it is spoken, because it is not
		// only for the ear: it is the caption on the listening stage. Under
		// speak_when: unfocused with the terminal in front of you, narration is
		// held back — and the panel you are looking at was showing a sentence
		// from some earlier turn, because the only place that remembered one
		// sat behind this gate.
		digest := spokenDigest(answer)
		if digest != "" {
			m.voiceLastDigest = digest
			m.voiceLastDigestLanguage = m.voice.spokenLanguageNow()
		}
		if !m.voiceMayNarrate() {
			m.voice.speaker.Finish()
			return
		}
		if digest != "" {
			m.speakDigest(digest)
			m.voice.speaker.Finish()
			return
		}
		projected := spokenText(answer)
		language := m.turnLanguage(projected)
		sentences, remainder := spokenSentences(projected)
		m.sayFrom(sentences, language)
		if remainder != "" && remainder != m.voice.spokenRemainder {
			m.voice.spokenRemainder = remainder
			m.say(language, remainder)
		}
	}
	m.voice.speaker.Finish()
}

// speakDigest reads the model's own closing line instead of the answer.
//
// Speech is slower than an agent works, and no amount of queue management fixes
// that: a turn with twelve tool calls produces more narration than anyone wants
// to hear, and the listener ends up several minutes behind the work. The
// staleness bound in internal/speech stops that becoming unbounded; this is the
// other half, and it is the half that makes the result worth hearing rather
// than merely current.
//
// Asking the model is what makes it cheap. An extra request at the end of a
// turn would cost a round trip and a second provider failure surface at exactly
// the moment the listener is waiting for the outcome — and DeepSeek runs with
// thinking on by default, so it is not a small request. A line the model
// already knows how to write costs about fifty output tokens inline, and when
// the model ignores the instruction the absence is detected for free and
// everything falls back to reading the answer.
//
// What is already queued is dropped, not cut. The sentence being read finishes;
// cutting a person off mid-word remains something only they may ask for.
func (m *Model) speakDigest(digest string) {
	if digest == m.voice.digestSpoken {
		return
	}
	m.voice.digestSpoken = digest
	// Projected BEFORE anything is dropped. The projection is lossy by design —
	// a digest that is nothing but an inline span or a path reduces to nothing —
	// and dropping the backlog first meant a listener got silence instead of the
	// narration that was already queued and would have been fine.
	projected := spokenText(digest)
	if projected == "" {
		return
	}
	language := m.turnLanguage(projected)
	// Kept on the Model rather than in the turn state: the listening stage shows
	// the last thing said, and it has to still say it after the turn that said
	// it has ended. The language goes with it, because "again" three turns later
	// would otherwise read it in whatever language the CURRENT turn is in.
	m.voiceLastDigest, m.voiceLastDigestLanguage = digest, language
	m.voice.speaker.DropPending()
	sentences, remainder := spokenSentences(projected)
	for _, sentence := range sentences {
		m.say(language, sentence)
	}
	if remainder != "" {
		m.say(language, remainder)
	}
}

// voiceAnswerHint is the instruction that asks for that closing line.
//
// It names the container, the length, the language and the exclusions, because
// each of those is a way the request goes wrong: an unmarked summary cannot be
// found, a long one defeats the purpose, one written in the wrong language is
// read by the wrong voice, and one carrying a path produces the spelled-out
// noise the whole projection exists to remove.
const voiceAnswerHint = "Spoken output is on: this reply may be HEARD rather than read. " +
	"End your final message with an HTML comment of the form " +
	"<!--spoken: ... --> containing one to three short sentences, in the same " +
	"language as your reply, saying what changed and what needs the user. " +
	"Write it to be listened to: no paths, no code, no URLs, no command names. " +
	"It is read aloud instead of the rest of the reply, so it must stand alone."

// voiceTurnHint is the hint to install for the turn about to be dispatched, or
// "" when nothing is listening.
//
// Evaluated per dispatch rather than at startup, so switching the answer
// channel off with /voice stops asking for a digest on the very next turn.
func (m *Model) voiceTurnHint() string {
	if !m.voiceActive() || !m.voice.config.Answer {
		return ""
	}
	return voiceAnswerHint
}

// speakReasoning offers a settled reasoning block to the reasoning channel.
//
// Off by default and spoken only once settled, not streamed. Reasoning is
// exploratory by nature — it contradicts itself, abandons paths, and is far
// longer than the answer — so interleaving it sentence by sentence with the
// answer would produce two voices arguing. Whoever turns this on is asking to
// hear the thinking, and hears it as one block.
func (m *Model) speakReasoning(reasoning string) {
	if !m.voiceActive() || !m.voice.config.Reasoning || !m.voiceMayNarrate() {
		return
	}
	projected := spokenText(reasoning)
	if projected == "" || projected == m.voice.spokenReasoning {
		return
	}
	m.voice.spokenReasoning = projected
	// Reasoning gets its own reading because a model often thinks in one
	// language and answers in another, and this block is long enough to be
	// worth a voice of its own. It does not get a vote in the turn's verdict.
	language := spokenLanguage(projected)
	if language == "" {
		language = m.voice.spokenLanguageNow()
	}
	sentences, remainder := spokenSentences(projected)
	for _, sentence := range sentences {
		m.say(language, sentence)
	}
	if remainder != "" {
		m.say(language, remainder)
	}
}

// speakActivity offers one tool receipt to the activity channel.
//
// It repeats nothing: a turn that reads four files says "reading" once, because
// the transcript's own collapsed run already decided that four reads are one
// event worth reporting.
func (m *Model) speakActivity(label, summary string) {
	if !m.voiceActive() || !m.voice.config.Activity || !m.voiceMayNarrate() {
		return
	}
	line := spokenActivity(label, summary)
	if line == "" || line == m.voice.lastActivity {
		return
	}
	m.voice.lastActivity = line
	// Spoken in the turn's language rather than in none. Asking for no language
	// used to mean "whatever voice is running", which was true only while one
	// was — and a tool call arriving in the gap after a segment drained started
	// the host default instead, which then read the rest of the answer.
	m.say(m.voice.spokenLanguageNow(), line)
}

// voiceMayNarrate reports whether the three ambient channels may speak now.
//
// Under speak_when: unfocused they yield to a person who is looking at the
// transcript. That is the setting that makes a voice in a coding tool worth
// having: while you are reading, a synthesizer reading the same words to you is
// slower than your eyes and in the way; the moment you switch to another window
// it is the only channel you have left. Focus reporting is what separates the
// two, and nothing else in the harness can.
//
// The alert channel deliberately does not consult this. It exists for the
// person who is not there, and a listener who IS there loses four words.
//
// Fails toward speaking. Not every terminal reports focus and tmux has to be
// told to, so a host that has never reported is treated as "cannot tell" rather
// than as focused — a setting whose unsupported case is silence is
// indistinguishable from the feature being broken, which is the failure this
// whole surface keeps running into.
func (m *Model) voiceMayNarrate() bool {
	if m.voice.config.SpeaksWhileFocused() {
		return true
	}
	return !m.terminalFocusReported || !m.terminalFocused
}

// noteTerminalFocus records a focus change and stops speech on the way back.
//
// Returning to the terminal is a statement that you are reading again, and it
// is exactly the interruption silenceVoice was written for: whatever is being
// read out is about to be read faster off the screen. It is the same gesture as
// typing, arriving through a different event.
func (m *Model) noteTerminalFocus(focused bool) {
	m.terminalFocusReported = true
	m.terminalFocused = focused
	if focused && m.voiceActive() && !m.voice.config.SpeaksWhileFocused() {
		m.silenceVoice()
	}
}

// say hands one sentence to the synthesizer, respelled for the voice reading it.
//
// Every channel goes through here rather than calling the speaker directly.
// That is the point: a respelling applied at four call sites out of five is a
// harness that pronounces the same word two ways in one turn, and the fifth
// call site is always the one added next.
func (m *Model) say(language, sentence string) {
	language, sentence = m.forDriver(language, sentence)
	m.voice.speaker.SayIn(language, sentence)
}

// sayNext is say for the alert channel, which goes ahead of the backlog.
func (m *Model) sayNext(language, sentence string) {
	language, sentence = m.forDriver(language, sentence)
	m.voice.speaker.SayNext(language, sentence)
}

// forDriver applies only what the synthesizer actually needs.
//
// Both of these were unconditional, and a measurement is why they are not. The
// same sentence read by four engines, transcribed back with the local Whisper:
// every engine TOLD the language read the English vocabulary with that
// language's rules, and every engine left to detect it handled the mixture.
// `say` needs both because it binds a monolingual voice per process. An engine
// that sorts out mixed text itself needs neither — and giving it the
// respellings made it worse, turning "guit" into "gitad".
//
// So the caller asks. A driver that does not need a language does not get the
// caller's guess about one, because that guess is exactly what breaks it.
func (m *Model) forDriver(language, sentence string) (string, string) {
	needs := m.voice.speaker.Needs()
	if needs.Respelling {
		sentence = m.voice.pronounce(language, sentence)
	}
	if !needs.Language {
		language = ""
	}
	return language, sentence
}

// speakingAloud reports whether the synthesizer is still reading something out.
func (m *Model) speakingAloud() bool {
	return m.voiceActive() && m.voice.speaker.Speaking()
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

// speechNavigationKeys are the keys that move the reader without displacing the
// speaker.
//
// An allowlist, not a denylist, and deliberately small: the default for an
// unrecognised key is to silence, because a key nobody classified is far more
// likely to be someone typing than someone scrolling. Getting it wrong in this
// direction costs a sentence; the other direction talks over a person.
var speechNavigationKeys = map[string]bool{
	"up": true, "down": true,
	"pgup": true, "pgdown": true,
	"home": true, "end": true,
	"ctrl+u": true, "ctrl+d": true,
	"shift+up": true, "shift+down": true,
}

// keyInterruptsSpeech reports whether a key press means the listener has
// stopped listening.
func keyInterruptsSpeech(msg tea.KeyPressMsg) bool {
	return !speechNavigationKeys[msg.String()]
}

func (m *Model) closeVoice() {
	if !m.voiceActive() {
		return
	}
	m.voice.speaker.Close()
	m.voice = nil
}
