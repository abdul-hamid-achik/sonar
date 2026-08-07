package speech

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// The host-synthesizer driver: one subprocess per voice run.
//
// This is the driver the rest of the package was originally written around, and
// it is the one with Needs on both fields. `say` binds a voice when the process
// starts and applies that voice's language rules to everything, so the caller
// has to decide the language and respell foreign vocabulary before handing it
// over. See driver.go for the measurement that separated those two concessions
// from the problem itself.

// sayDriver drives the host synthesizer named by synthesizerCommand.
type sayDriver struct {
	// Immutable after construction, so a run can read them without the
	// Speaker's lock — resolving a voice name can cost a subprocess.
	rate   int
	voice  string
	voices map[string]string
}

// Needs is both, and permanently. It is a property of driving a monolingual
// synthesizer, not a limitation waiting to be lifted.
func (d *sayDriver) Needs() Needs { return Needs{Language: true, Respelling: true} }

func (d *sayDriver) Open(language string) (Voice, error) {
	name, args, err := synthesizerCommand(d.voiceFor(language), d.rate)
	if err != nil {
		return nil, err
	}
	command := exec.Command(name, args...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("speech: open synthesizer input: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("speech: start synthesizer: %w", err)
	}
	run := &sayVoice{command: command, stdin: stdin, exited: make(chan struct{})}
	// One reaper per process, started with it, so Wait is called exactly once
	// and always: a synthesizer nobody waits on is a zombie for the rest of the
	// session, and closing exited is what a voice change waits on.
	go func() {
		_ = command.Wait()
		close(run.exited)
	}()
	return run, nil
}

// voiceFor resolves a language to a voice name, most specific first: a
// per-language setting, then a single configured voice for everything, then
// whatever the host has for that language.
func (d *sayDriver) voiceFor(language string) string {
	if configured := strings.TrimSpace(d.voices[language]); configured != "" {
		return configured
	}
	return VoiceForLanguage(language, d.voice)
}

// sayVoice is one running `say`.
type sayVoice struct {
	mu      sync.Mutex
	command *exec.Cmd
	stdin   io.WriteCloser
	exited  chan struct{}
}

func (v *sayVoice) Say(sentence string) error {
	v.mu.Lock()
	stdin := v.stdin
	v.mu.Unlock()
	if stdin == nil {
		return io.ErrClosedPipe
	}
	// Escape first, then add our own command. Reversing the two would hand the
	// control channel to the text being escaped — `say` obeys [[...]] from
	// stdin, and every word here came from a model.
	_, err := io.WriteString(stdin, sentencePause()+escapeSynthesizerCommands(sentence)+"\n")
	return err
}

// Close ends the run without signalling, which is what lets `say` drain.
//
// Measured: a sentence that takes 6.4 seconds to read keeps the process alive
// for 6.40 seconds after its input closes. That is what makes Done mean "the
// audio has stopped" rather than "the handle was released".
func (v *sayVoice) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stdin == nil {
		return nil
	}
	err := v.stdin.Close()
	v.stdin = nil
	return err
}

func (v *sayVoice) Cancel() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stdin != nil {
		_ = v.stdin.Close()
		v.stdin = nil
	}
	if v.command != nil && v.command.Process != nil {
		// SIGTERM rather than SIGKILL: `say` releases the audio device on TERM,
		// and killed outright it can leave the output route wedged until
		// another process claims it.
		signalSynthesizer(v.command)
	}
}

func (v *sayVoice) Done() <-chan struct{} { return v.exited }
