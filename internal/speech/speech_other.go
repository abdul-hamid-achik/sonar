//go:build !darwin

package speech

import (
	"os"
	"os/exec"
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

func synthesizerCommand(string, int) (string, []string, error) {
	return "", nil, ErrUnavailable
}

func signalSynthesizer(*exec.Cmd) {}

// captureCommand records from ALSA, which is the one recorder present on a
// stock Linux without a desktop audio stack. It is unverified on a real host —
// this project has no Linux machine to try it on — so CaptureAvailable's
// ffmpeg probe is what actually gates it, and a wrong device name here fails
// loudly at record time rather than silently producing nothing.
func captureCommand(destination string) (string, []string) {
	return "ffmpeg", []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "alsa", "-i", "default",
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
