package speech

import (
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// sayPath is the macOS synthesizer. It is part of the base system, reads from
// stdin incrementally, and honours a rate and a voice — which is every feature
// this package needs and none it has to install.
const sayPath = "/usr/bin/say"

// defaultRate is words per minute. `say` defaults near 175; a coding session is
// read while working rather than listened to attentively, and every voice
// interface that gets used at all is run faster than its default.
const defaultRate = 210

func Available() bool {
	path, err := exec.LookPath(sayPath)
	return err == nil && path != ""
}

func synthesizerCommand(voice string, rate int) (string, []string, error) {
	if !Available() {
		return "", nil, ErrUnavailable
	}
	if rate <= 0 {
		rate = defaultRate
	}
	args := []string{"-r", strconv.Itoa(rate)}
	if voice != "" {
		args = append(args, "-v", voice)
	}
	// No operand: `say` then reads stdin until it closes, which is what lets
	// one process speak a whole turn's worth of sentences.
	return sayPath, args, nil
}

// signalSynthesizer asks the synthesizer to stop and lets it exit.
//
// SIGTERM rather than SIGKILL, for the reason a cancelled `git commit` taught
// this codebase: an uncatchable signal denies a process the chance to leave the
// machine tidy. `say` releases the audio device on TERM; killed outright it can
// leave the output route wedged until another process claims it.
func signalSynthesizer(command *exec.Cmd) {
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		_ = command.Process.Kill()
	}
}

// captureCommand records 16kHz mono 16-bit WAV from the default input.
//
// `:default` names the default audio input device in avfoundation's
// "video:audio" address form — the empty video half is required, and omitting
// the colon selects a camera instead. 16kHz mono is what every Whisper family
// model expects; feeding anything else costs a resample nobody sees.
func captureCommand(destination string, limit time.Duration) (string, []string) {
	return "ffmpeg", []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "avfoundation", "-i", ":default",
		"-t", captureLimitSeconds(limit),
		"-ar", "16000", "-ac", "1", "-sample_fmt", "s16",
		"-y", destination,
	}
}

// interruptRecorder asks ffmpeg to finish the file it is writing.
//
// SIGINT, not SIGKILL. A WAV header carries its data size in a field written
// when the stream closes; ffmpeg backpatches it on interrupt and cannot on a
// kill, which leaves a file every reader treats as empty. Same lesson as the
// git index lock, one signal further along.
func interruptRecorder(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	if err := command.Process.Signal(syscall.SIGINT); err != nil {
		_ = command.Process.Kill()
	}
}
