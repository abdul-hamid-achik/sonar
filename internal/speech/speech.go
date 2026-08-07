// Package speech reads text aloud through a host synthesizer.
//
// It follows the shape internal/sandbox established for an optional external
// capability: probe for a driver, report honestly when one was requested and is
// not there, and wrap a subprocess rather than link a library. That last part
// is not a preference. Both harnesses build with CGO_ENABLED=0 in goreleaser
// and in CI, and every Go audio binding is cgo — adopting one would flip the
// release pipeline for both repositories and break cross-compilation from a
// single runner. A subprocess costs a fork and keeps the static binary.
//
// # What this package is not
//
// It does not decide WHAT to say. Turning a coding-agent transcript into
// something worth hearing is a projection problem — file paths, diffs and URLs
// read aloud are noise, and the same sentence measures 12.9s spoken raw against
// 6.1s spoken projected — and that projection belongs to the UI, which is where
// the semantic labels already live. This package receives finished sentences.
package speech

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable reports that this host has no synthesizer sonar can drive.
var ErrUnavailable = errors.New("speech: no supported synthesizer on this host")

// maxQueuedUtterances bounds the backlog.
//
// A runaway guard, not a policy: at ordinary speaking rates this is upwards of
// twenty minutes of audio, and a listener that far behind the transcript
// stopped listening long ago. A finish marker is never dropped, because
// Speaking answers from the queue and a lost marker would leave the rail
// claiming the harness is still talking.
const maxQueuedUtterances = 256

// utterance is one queued thing to do: say a sentence in a language, or end the
// current run so the synthesizer reads out what it already has.
type utterance struct {
	language string
	text     string
	finish   bool
	// enqueuedAt is when this was handed over, and sticky exempts it from going
	// stale. See staleUtteranceAfter.
	enqueuedAt time.Time
	sticky     bool
}

// staleUtteranceAfter is how long a sentence may wait before it stops being
// worth saying.
//
// Speech is slower than an agent works. On a long run with the answer channel
// on, the queue grows behind real time and the listener ends up hearing
// narration of work that finished minutes ago — which is worse than silence,
// because it describes a present that is not the present. The 256-utterance cap
// does not help: it is a runaway guard at roughly twenty minutes of audio, not
// a policy about staleness.
//
// Thirty seconds is six to ten sentences at ordinary speaking rate. A listener
// that far behind has lost the thread; everything nearer than that still plays
// in order. Dropping also shortens the drain a language change waits on, which
// is a second complaint fixed for free.
//
// No configuration knob. This is the same class of bound as maxQueuedUtterances
// — a guard against a runaway, not a preference — and a knob here would be a
// setting nobody tunes.
const staleUtteranceAfter = 30 * time.Second

// drainDeadline bounds how long a finished run may take to stop making sound.
//
// Not a guess at how long speech takes — it is deliberately longer than any
// segment's audio, because exceeding it means the synthesizer is wedged rather
// than slow. Its only job is to turn "parked forever" into "parked once".
const drainDeadline = 5 * time.Minute

// Speaker owns the synthesizer and a queue of what to say through it.
//
// The queue is not a buffer for its own sake. A synthesizer voice is built for
// one language, and it is chosen when the PROCESS starts — so reading a Spanish
// answer that opened with an English clause means starting a second process,
// and doing that the moment the decision lands cuts whatever is currently being
// read. With a queue, the switch happens at the seam between two utterances:
// the old voice finishes its sentence, exits, and the new one starts. Nothing
// is ever cut except by Stop, which is the one interruption a user asked for.
//
// It also takes the writes off the caller's goroutine. Writing to a
// synthesizer's stdin blocks once its pipe fills, and the caller here is Bubble
// Tea's Update — a long answer used to be able to stall the frame loop on an
// audio device.
//
// A Speaker is safe for concurrent use. It owns no clock and schedules no work
// of its own: the caller hands it a sentence when a sentence exists.
type Speaker struct {
	mu    sync.Mutex
	ready sync.Cond

	// Set once by New and never mutated, so the worker reads it unlocked —
	// opening a run can cost a subprocess, and holding the mutex across it
	// would block Stop and Speaking on the UI goroutine.
	driver Driver
	// now is the clock, indirect so a test can age the queue without waiting on
	// one. Same rationale as the Driver seam: the property being pinned is
	// not observable in real time.
	now func() time.Time

	queue []utterance
	// busy is an utterance taken off the queue and not yet delivered. Speaking
	// has to count it, or the rail reports silence during the gap between
	// dequeue and the first byte written.
	busy bool
	// generation invalidates work in flight. Stop bumps it so an utterance the
	// worker already took is discarded rather than spoken after the key press
	// that asked for silence.
	generation int
	closed     bool

	// live is the run currently producing sound, and liveLanguage is what it was
	// opened for — kept beside it because a Voice does not carry its own
	// language and the queue has to know when the next sentence needs a
	// different one.
	live         Voice
	liveLanguage string
}

// New returns a Speaker, or ErrUnavailable when the host has no driver.
//
// No synthesizer process starts here — the first sentence starts one, so a
// session with voice enabled and nothing to say costs no process at all. The
// goroutine that will own it does start, because speech has to be serialized
// somewhere and the alternative is serializing it on the caller.
func New(voice string, rate int, voices map[string]string) (*Speaker, error) {
	return NewWithProvider("", voice, rate, voices)
}

// NewWithProvider returns a Speaker driven by the named provider.
//
// An unknown name is an error rather than a silent fallback to the host
// synthesizer. Somebody who asked for a hosted voice and got the local one
// would hear the exact failure they were trying to fix, with nothing saying so.
func NewWithProvider(provider, voice string, rate int, voices map[string]string) (*Speaker, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "say", "host", "local":
	case "openai":
		driver, err := newHostedDriver(voice)
		if err != nil {
			return nil, err
		}
		return newSpeaker(driver), nil
	default:
		return nil, fmt.Errorf("speech: unknown voice provider %q (say, openai)", provider)
	}
	if !Available() {
		return nil, ErrUnavailable
	}
	// The map is copied rather than aliased. Nothing mutates the caller's copy
	// today, but a driver reads it from a worker goroutine while the caller
	// holds the only other reference — and "nothing mutates it today" is a fact
	// about this month's callers, not a property of the type.
	owned := make(map[string]string, len(voices))
	for language, name := range voices {
		owned[language] = name
	}
	return newSpeaker(&sayDriver{rate: rate, voice: strings.TrimSpace(voice), voices: owned}), nil
}

// Needs reports what this speaker's driver requires from its caller. See
// driver.go: the answer decides whether the caller must detect a language and
// respell foreign vocabulary, or hand the text over as written.
func (s *Speaker) Needs() Needs {
	if s == nil || s.driver == nil {
		return Needs{}
	}
	return s.driver.Needs()
}

// newSpeaker is New without the host probe, so the queue can be exercised on a
// platform that has no synthesizer at all — and with a driver that records
// instead of speaking, which is the only way the ordering contracts here are
// observable at all.
func newSpeaker(driver Driver) *Speaker {
	speaker := &Speaker{driver: driver, now: time.Now}
	speaker.ready.L = &speaker.mu
	go speaker.run()
	return speaker
}

// Say queues one finished sentence in whatever voice is already running.
func (s *Speaker) Say(sentence string) { s.SayIn("", sentence) }

// SayIn queues one finished sentence to be read in a given language.
//
// It never blocks and never cuts what is currently being read. A sentence whose
// language differs from the live process waits for that process to finish and
// exit before the new voice starts — the cost is the tail of a sentence's worth
// of latency at a language change, against a cut word on every switch.
func (s *Speaker) SayIn(language, sentence string) {
	if s == nil {
		return
	}
	if sentence = strings.TrimSpace(sentence); sentence == "" {
		return
	}
	s.enqueue(utterance{language: language, text: sentence})
}

// SayNext queues a sentence ahead of everything still waiting.
//
// For the one thing that stops being true if it arrives late: a run blocked on
// an approval. Speech is slower than the model streams, so an answer channel
// left running builds a backlog — ten sentences is half a minute — and an alert
// behind it announces a prompt that has been waiting the whole time.
//
// Ahead of the queue, NOT ahead of the sentence being read. Cutting is what
// Stop is for, and a listener who loses the word they were following to hear an
// alert has been told two things badly instead of one thing well.
func (s *Speaker) SayNext(language, sentence string) {
	if s == nil {
		return
	}
	if sentence = strings.TrimSpace(sentence); sentence == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	// Sticky: an alert stays true however late it plays, and it jumps the queue
	// anyway, so it is never meaningfully stale. "Waiting for your approval" is
	// worth hearing thirty seconds late; a sentence about a file read is not.
	s.queue = append([]utterance{{
		language:   language,
		text:       sentence,
		enqueuedAt: s.now(),
		sticky:     true,
	}}, s.queue...)
	s.ready.Signal()
}

// Finish says that no more sentences are coming, and lets the synthesizer read
// out what it already has.
//
// This is what makes Speaking answer a question worth asking. While stdin stays
// open `say` waits for more input and stays alive whether or not any audio is
// playing, so "a process exists" means only "the speaker was used at some point
// this turn". Closing stdin without signalling lets it drain and exit on its
// own — measured, a sentence that takes 6.4 seconds to read keeps the process
// alive for 6.40 seconds.
//
// It is queued rather than applied, so it lands AFTER everything already said,
// not in the middle of it. Distinct from Stop, which cancels. This one waits.
func (s *Speaker) Finish() {
	if s == nil {
		return
	}
	s.enqueue(utterance{finish: true})
}

// DropPending discards the backlog without touching what is being read.
//
// The difference from Stop is the whole reason it exists. Stop cancels because
// a person asked for silence, so it cuts mid-sentence. This is the harness
// deciding that a queue of narration has been superseded — by a summary written
// for the ear, say — where cutting the sentence already playing would be the
// harness talking over itself to correct itself. So the generation is left
// alone, the live process keeps its input, and only what has not started is
// dropped.
func (s *Speaker) DropPending() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Finish markers survive: Speaking answers from the queue, and dropping one
	// would leave the rail reporting audio that is not coming.
	kept := s.queue[:0]
	for _, queued := range s.queue {
		if queued.finish {
			kept = append(kept, queued)
		}
	}
	s.queue = kept
}

// Stop ends speech immediately and discards anything not yet spoken.
//
// Immediately is the whole contract, and this is the only method that breaks
// the no-cutting rule the rest of the file is built on. Every voice-interface
// source agrees that interruption cancels rather than queues: a user who types
// while the harness is talking has stopped listening, and audio that keeps
// going is talking over them. So this drops the backlog and sends SIGTERM,
// rather than closing stdin and waiting — draining finishes the backlog first,
// which is exactly the behaviour being cancelled.
func (s *Speaker) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = nil
	s.generation++
	s.silenceLocked()
}

// Close stops speech and refuses further sentences.
func (s *Speaker) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.queue = nil
	s.generation++
	s.silenceLocked()
	s.ready.Broadcast()
}

// Speaking reports whether anything is still on its way to the speakers.
//
// That is the queue, the utterance in flight, and a live process. After Finish
// the process half is exactly "audio is still playing"; before it, while stdin
// is open for more sentences, a live process means the synthesizer is running
// and may or may not have anything left to read — the host cannot tell, because
// no synthesizer this package drives reports it.
func (s *Speaker) Speaking() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// A closed speaker will never produce sound again, whatever the worker
		// is still holding. Without this it reported true for the utterance that
		// happened to be off the queue when Close landed — one the generation
		// check was about to discard — so shutdown briefly claimed audio that
		// was never coming.
		return false
	}
	if len(s.queue) > 0 || s.busy {
		return true
	}
	if s.live == nil {
		return false
	}
	select {
	case <-s.live.Done():
		return false
	default:
		return true
	}
}

// SpeakingLanguage reports the language the live synthesizer was started for.
func (s *Speaker) SpeakingLanguage() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveLanguage
}

func (s *Speaker) enqueue(next utterance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if !next.finish && len(s.queue) >= maxQueuedUtterances {
		return
	}
	if next.enqueuedAt.IsZero() {
		next.enqueuedAt = s.now()
	}
	s.queue = append(s.queue, next)
	s.ready.Signal()
}

// run delivers queued utterances one at a time, forever, until Close.
func (s *Speaker) run() {
	for {
		next, generation, ok := s.take()
		if !ok {
			return
		}
		s.deliver(next, generation)
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}
}

func (s *Speaker) take() (utterance, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		for len(s.queue) == 0 && !s.closed {
			s.ready.Wait()
		}
		if s.closed {
			return utterance{}, 0, false
		}
		next := s.queue[0]
		s.queue = s.queue[1:]
		if s.stale(next) {
			// Dropped here rather than at enqueue, because whether a sentence is
			// too late to say is only knowable at the moment it would be said.
			continue
		}
		s.busy = true
		return next, s.generation, true
	}
}

// stale reports whether an utterance waited long enough to stop being worth
// saying.
//
// A finish marker is never stale: Speaking answers from the queue, and dropping
// one would leave the rail reporting audio that is not coming. Neither is a
// sticky one.
func (s *Speaker) stale(next utterance) bool {
	if next.finish || next.sticky || next.enqueuedAt.IsZero() {
		return false
	}
	return s.now().Sub(next.enqueuedAt) > staleUtteranceAfter
}

func (s *Speaker) deliver(next utterance, generation int) {
	if next.finish {
		s.drain(generation)
		return
	}
	voice := s.voiceFor(next.language, generation)
	if voice == nil {
		return
	}
	// The driver escapes its own control channel and adds its own prosody.
	// Neither belongs here: `say` obeys [[...]] and xAI obeys [pause] and
	// <whisper>, and a queue that knew about either would be wrong for the
	// next driver.
	if err := voice.Say(next.text); err != nil {
		// A run that ended under us is not an error worth surfacing to a user
		// mid-turn; the next sentence opens a fresh one.
		s.silence(generation)
	}
}

// voiceFor returns a run in the right language, opening one if there is none,
// or nil when the work was superseded.
func (s *Speaker) voiceFor(language string, generation int) Voice {
	if !s.driver.Needs().Language {
		// A driver that sorts out mixed text itself must not be handed the
		// caller's guess. Measured: telling one the language is exactly what
		// made it read English vocabulary with Spanish rules.
		language = ""
	}
	s.mu.Lock()
	if s.closed || generation != s.generation {
		s.mu.Unlock()
		return nil
	}
	if s.live != nil && s.liveLanguage == language {
		live := s.live
		s.mu.Unlock()
		return live
	}
	superseded := s.live != nil
	s.mu.Unlock()

	if superseded {
		// Either a different voice is needed, or this run was already finished.
		// The live run is left to say everything it was given: replacing a voice
		// by cancelling the run reading it is the cut this queue exists to
		// prevent.
		s.drain(generation)
	}
	return s.open(language, generation)
}

func (s *Speaker) open(language string, generation int) Voice {
	s.mu.Lock()
	superseded := s.closed || generation != s.generation
	s.mu.Unlock()
	if superseded {
		// A cancel landed while the previous voice was draining. Checked before
		// the driver does any work, so silencing does not cost a run that is
		// opened only to be ended.
		return nil
	}
	live, err := s.driver.Open(language)
	if err != nil {
		return nil
	}

	s.mu.Lock()
	if s.closed || generation != s.generation {
		// Stop landed while this was opening. Nothing has been said yet, so
		// ending it here costs no audio at all.
		s.mu.Unlock()
		live.Cancel()
		return nil
	}
	s.live, s.liveLanguage = live, language
	s.mu.Unlock()
	return live
}

// drain ends the run and waits for it to finish producing sound.
func (s *Speaker) drain(generation int) {
	s.mu.Lock()
	if generation != s.generation {
		s.mu.Unlock()
		return
	}
	live := s.live
	if live == nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	_ = live.Close()

	// Blocking here is the point: it is the audio still playing. Stop and Close
	// cancel the run, so a cancel does not have to wait for it.
	//
	// Bounded anyway. A synthesizer that never exits — a wedged audio device is
	// the realistic way — would park this worker forever, and with it the queue,
	// the language switch behind it and every later sentence. A key press
	// recovers it because Stop signals the process, but the whole point of this
	// feature is the session where nobody is at the keyboard. The deadline is
	// far longer than any run's audio so it can only ever catch a wedge, and
	// past it the run is cancelled rather than waited on.
	select {
	case <-live.Done():
	case <-time.After(drainDeadline):
		live.Cancel()
		<-live.Done()
	}

	s.mu.Lock()
	if s.live == live {
		s.live, s.liveLanguage = nil, ""
	}
	s.mu.Unlock()
}

func (s *Speaker) silence(generation int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == s.generation {
		s.silenceLocked()
	}
}

func (s *Speaker) silenceLocked() {
	if s.live == nil {
		return
	}
	s.live.Cancel()
	s.live, s.liveLanguage = nil, ""
}
