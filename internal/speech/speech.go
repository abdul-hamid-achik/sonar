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
	"io"
	"os/exec"
	"strings"
	"sync"
)

// ErrUnavailable reports that this host has no synthesizer sonar can drive.
var ErrUnavailable = errors.New("speech: no supported synthesizer on this host")

// Speaker owns one synthesizer subprocess and the goroutine feeding it.
//
// A Speaker is safe for concurrent use by the UI goroutine and the command
// goroutine that a Bubble Tea program runs, because both reach it through the
// same mutex. It owns no clock and schedules no work of its own: the caller
// hands it a sentence when a sentence exists.
type Speaker struct {
	mu      sync.Mutex
	command *exec.Cmd
	stdin   io.WriteCloser
	rate    int
	voice   string
	// voices maps an ISO language code to a voice name, from configuration.
	voices map[string]string
	// language is what the LIVE process was started for, so a caller can tell
	// whether the next sentence needs a different one.
	language string
	closed   bool
}

// New returns a Speaker, or ErrUnavailable when the host has no driver.
//
// Nothing is started here. The synthesizer process begins on the first sentence
// and lives until Stop, so a session with voice enabled and nothing to say
// costs no process at all.
func New(voice string, rate int, voices map[string]string) (*Speaker, error) {
	if !Available() {
		return nil, ErrUnavailable
	}
	return &Speaker{voice: strings.TrimSpace(voice), rate: rate, voices: voices}, nil
}

// Say queues one finished sentence.
//
// The synthesizer reads from stdin, so consecutive sentences flow into a
// running process instead of paying a fork each time — which is also what makes
// speech continuous rather than gapped between sentences.
func (s *Speaker) Say(sentence string) error { return s.SayIn("", sentence) }

// SayIn queues one finished sentence to be read in a given language.
//
// The language is consulted when a synthesizer process STARTS, and not again
// while one is running. Switching voices mid-flight would mean killing a process
// that still has audio queued, which cuts a sentence in half — and since Finish
// runs at every turn boundary, each turn already gets a fresh decision. The cost
// is that an answer which changes language halfway through is read by one voice;
// the alternative costs a cut word on every switch.
func (s *Speaker) SayIn(language, sentence string) error {
	sentence = strings.TrimSpace(sentence)
	if s == nil || sentence == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.stdin == nil {
		if s.command != nil {
			// A previous turn's audio is still draining after Finish. New
			// content supersedes it: two synthesizers talking over each other is
			// worse than losing the tail of an answer already on screen.
			s.resetLocked()
		}
		if err := s.startLocked(language); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(s.stdin, sentence+"\n"); err != nil {
		// A synthesizer that exited under us is not an error worth surfacing to
		// a user mid-turn; the next sentence starts a fresh one.
		s.resetLocked()
		return nil
	}
	return nil
}

// Stop ends speech immediately and discards anything not yet spoken.
//
// Immediately is the whole contract. Every voice-interface source agrees that
// interruption cancels rather than queues: a user who types while the harness
// is talking has stopped listening, and audio that keeps going is talking over
// them. So this sends SIGTERM to the synthesizer rather than closing stdin and
// waiting for it to drain — closing stdin finishes the backlog first, which is
// exactly the behaviour being cancelled.
func (s *Speaker) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetLocked()
}

// Close stops speech and refuses further sentences.
func (s *Speaker) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.resetLocked()
}

// Finish says that no more sentences are coming, and lets the synthesizer read
// out what it already has.
//
// This is what makes Speaking answer a question worth asking. While stdin stays
// open `say` waits for more input and stays alive whether or not any audio is
// playing, so "a process exists" means only "the speaker was used at some point
// this turn". Closing stdin without signalling lets it drain and exit on its
// own — measured, a sentence that takes 6.4 seconds to read keeps the process
// alive for 6.40 seconds — so from here until the reaper clears it, a live
// process means audio is still coming out of the speakers.
//
// Distinct from Stop, which cancels. This one waits.
func (s *Speaker) Finish() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
}

// Speaking reports whether a synthesizer process is currently alive.
//
// After Finish that is exactly "audio is still playing". Before it, while stdin
// is still open for more sentences, it means the synthesizer is running and may
// or may not have anything left to read — the host cannot tell, because no
// synthesizer this package drives reports it.
func (s *Speaker) Speaking() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.command != nil
}

// SpeakingLanguage reports the language the live synthesizer was started for.
func (s *Speaker) SpeakingLanguage() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.language
}

// voiceFor resolves a language to a voice name, most specific first: a
// per-language setting, then a single configured voice for everything, then
// whatever the host has for that language.
func (s *Speaker) voiceFor(language string) string {
	if configured := strings.TrimSpace(s.voices[language]); configured != "" {
		return configured
	}
	return VoiceForLanguage(language, s.voice)
}

func (s *Speaker) startLocked(language string) error {
	name, args, err := synthesizerCommand(s.voiceFor(language), s.rate)
	if err != nil {
		return err
	}
	command := exec.Command(name, args...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("speech: open synthesizer input: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("speech: start synthesizer: %w", err)
	}
	s.command, s.stdin, s.language = command, stdin, language
	// Reaped in the background so a stopped synthesizer cannot become a zombie
	// and so Stop never blocks the UI goroutine on a Wait.
	// The reaper clears the handle when the synthesizer exits, which after
	// Finish is the moment the audio stops. Without that, Speaking would stay
	// true forever on a process nobody is waiting for.
	go func() {
		_ = command.Wait()
		s.mu.Lock()
		if s.command == command {
			s.command, s.language = nil, ""
			if s.stdin != nil {
				_ = s.stdin.Close()
				s.stdin = nil
			}
		}
		s.mu.Unlock()
	}()
	return nil
}

func (s *Speaker) resetLocked() {
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	if s.command != nil && s.command.Process != nil {
		signalSynthesizer(s.command)
	}
	s.command, s.language = nil, ""
}
