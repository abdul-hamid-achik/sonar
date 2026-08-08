//go:build !darwin

package speech

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// espeakPath is the synthesizer this platform speaks through: espeak-ng, found
// on PATH rather than at a fixed location, because no Linux ships one in the
// base system the way macOS ships `say`.
//
// espeak-ng over Piper was a licensing call, not a quality one: Piper's
// upstream archived in October 2025 and its active fork is GPL-3, which is a
// decision about a distributed binary rather than an implementation detail.
// espeak-ng sounds robotic beside it — and beside macOS's compact voices, for
// that matter — but it is apt-installable, MIT-adjacent (GPL for the binary,
// invoked here as a subprocess), reads stdin, and honours a rate and a voice,
// which is every feature this package needs.
//
// Wired per its documented behaviour; unverified on a real Linux host, the
// same honesty captureCommand carries below. Available() gates on the binary
// actually being present, so a host without it is told so rather than left
// silent — and `voice.provider: openai` needs none of this.
const espeakPath = "espeak-ng"

// espeakDefaultRate is words per minute. espeak-ng defaults to 175; the same
// observation behind `say`'s 210 applies — a coding session is read while
// working, and every voice interface that gets used at all runs faster than
// its default.
const espeakDefaultRate = 190

func Available() bool {
	path, err := exec.LookPath(espeakPath)
	return err == nil && path != ""
}

func synthesizerName() string { return espeakPath }

func synthesizerCommand(voice string, rate int) (string, []string, error) {
	if !Available() {
		return "", nil, ErrUnavailable
	}
	if rate <= 0 {
		rate = espeakDefaultRate
	}
	args := []string{"-s", strconv.Itoa(rate)}
	if voice != "" {
		// espeak-ng voices are language identifiers ("es", "en-us"), so the
		// per-language voice map and this flag speak the same vocabulary.
		args = append(args, "-v", voice)
	}
	// No operand: espeak-ng then reads stdin until it closes, the same shape
	// `say` has — one process per run, sentences flowing in with no gap.
	return espeakPath, args, nil
}

// signalSynthesizer asks the synthesizer to stop and lets it exit. SIGTERM
// rather than SIGKILL for the same reason as on macOS: an uncatchable signal
// denies the process the chance to release the audio device.
func signalSynthesizer(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		_ = command.Process.Kill()
	}
}

// escapeSynthesizerCommands is the identity here, and that is a verified
// property rather than an omission: espeak-ng only interprets markup under
// -m (SSML mode), which synthesizerCommand never passes. In plain-text mode
// there is no control channel for a model's sentence to reach. If -m is ever
// added, this function must learn to neutralise SSML first — the same
// ordering rule `say`'s [[ ]] escaping documents.
func escapeSynthesizerCommands(text string) string { return text }

// sentencePause has no inline spelling without SSML mode, and turning SSML on
// to get one would open the control channel escapeSynthesizerCommands just
// declared closed. espeak-ng inserts its own gap at sentence boundaries;
// that has to be enough until a listener on a real host says otherwise.
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

// listHostVoices parses `espeak-ng --voices`.
//
// The format is columns: Pty Language Age/Gender VoiceName File Other. The
// language column doubles as the locale — espeak-ng names voices by language
// identifier — so both hostVoice fields come from one line without the
// column-guessing `say -v ?` needs.
func listHostVoices() []hostVoice {
	if !Available() {
		return nil
	}
	output, err := exec.Command(espeakPath, "--voices").Output()
	if err != nil {
		return nil
	}
	var voices []hostVoice
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	first := true
	for scanner.Scan() {
		if first {
			// Header row.
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		voices = append(voices, hostVoice{name: fields[3], locale: fields[1]})
	}
	return voices
}
