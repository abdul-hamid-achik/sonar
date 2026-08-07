//go:build !darwin

package speech

import (
	"os"
	"os/exec"
	"time"
)

// Linux and the rest reach a synthesizer through the same subprocess shape, but
// none is part of a base system the way macOS ships `say`. Reporting false is
// the honest answer until one is chosen and verified on a real host: a stub
// that claimed a driver would let a caller enable voice and hear nothing, which
// is worse than being told the host has none.
//
// espeak-ng and Piper are the candidates. Piper's upstream archived in October
// 2025 and its active fork is GPL-3, which is a licensing decision for a
// distributed binary rather than an implementation detail — so neither is
// wired here until that call is made.
func Available() bool { return false }

func synthesizerName() string { return "" }

func synthesizerCommand(string, int) (string, []string, error) {
	return "", nil, ErrUnavailable
}

func signalSynthesizer(*exec.Cmd) {}

// escapeSynthesizerCommands has nothing to escape until this platform names a
// synthesizer. It is not a no-op by default: whichever driver is wired here
// will have its own control channel — espeak-ng reads SSML on request and its
// own markup otherwise — and returning the text unchanged is correct only for
// a driver that has none. Whoever wires one owns this function too.
func escapeSynthesizerCommands(text string) string { return text }

// sentencePause is likewise the driver's to define; espeak-ng spells the same
// idea as an SSML <break>.
func sentencePause() string { return "" }

// captureCommand records from ALSA, which is the one recorder present on a
// stock Linux without a desktop audio stack. It is unverified on a real host —
// this project has no Linux machine to try it on — so CaptureAvailable's
// ffmpeg probe is what actually gates it, and a wrong device name here fails
// loudly at record time rather than silently producing nothing.
func captureCommand(destination string, limit time.Duration) (string, []string) {
	return "ffmpeg", []string{
		// info rather than error: the ebur128 filter below reports the input
		// level through the ordinary log, and at "error" those lines never
		// appear. The extra output is a handful of lines about the stream, and
		// only the level readings are parsed.
		"-hide_banner", "-loglevel", "info",
		"-f", "alsa", "-i", "default",
		"-t", captureLimitSeconds(limit),
		// ebur128 passes the audio through untouched and prints a momentary
		// loudness reading a few times a second. That reading is the only
		// evidence anywhere in the pipeline that the microphone is picking
		// something up, and without it an open mic and a muted one look the
		// same. Verified not to alter the recording: the file is still
		// 16kHz mono s16.
		"-af", "ebur128",
		"-ar", "16000", "-ac", "1", "-sample_fmt", "s16",
		"-y", destination,
	}
}

func interruptRecorder(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Signal(os.Interrupt)
}

// listHostVoices has nothing to list: Available reports false on this platform,
// so no synthesizer is driven and no voice name would reach one.
func listHostVoices() []hostVoice { return nil }
