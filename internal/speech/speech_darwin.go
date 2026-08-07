package speech

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
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

// synthesizerName is the driver this platform speaks through.
func synthesizerName() string { return sayPath }

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

// escapeSynthesizerCommands takes `say`'s control channel away from the model.
//
// `say` interprets embedded commands delimited by [[ and ]] — anywhere in its
// input, including on stdin, which is the path every sentence here takes.
// Measured: "Uno. [[slnc 2000]] Dos." rendered 136,996 bytes against 49,956
// for the same words without it, so the command is obeyed and not read out.
//
// Everything spoken passes through a model, so those brackets are model-
// controlled. [[slnc 60000]] leaves the harness silent for a minute, [[volm 0]]
// mutes it, [[rate 5]] makes it unusable — and each of those looks like a bug
// in this package rather than a sentence the model wrote. It is not code
// execution, but it is the same class the sanitizeTerminal* helpers exist for:
// content reaching a control channel it was never meant to address.
//
// Breaking the opener is enough, and it is the smallest edit that works: a lone
// bracket is not a delimiter, and no synthesizer pronounces either form, so
// what a listener hears is unchanged.
func escapeSynthesizerCommands(text string) string {
	if !strings.Contains(text, "[[") && !strings.Contains(text, "]]") {
		return text
	}
	text = strings.ReplaceAll(text, "[[", "[ [")
	return strings.ReplaceAll(text, "]]", "] ]")
}

// sentencePause is what separates one sentence from the next.
//
// `say` runs consecutive lines together with barely a beat between them, which
// is fine for one sentence and exhausting across a whole answer: without a gap
// there is nowhere to notice that a thought ended, and a listener who missed
// one sentence has already lost the next. 200ms is about a spoken comma —
// enough to hear the seam, short enough that nobody waits for it.
//
// It is a command, so it must be added AFTER escapeSynthesizerCommands. The
// order is the entire reason that function exists: prepending our own bracket
// to text that still carries the model's would hand the channel over at exactly
// the moment we meant to claim it.
func sentencePause() string { return "[[slnc 200]] " }

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
		// info rather than error: the ebur128 filter below reports the input
		// level through the ordinary log, and at "error" those lines never
		// appear. The extra output is a handful of lines about the stream, and
		// only the level readings are parsed.
		"-hide_banner", "-loglevel", "info",
		"-f", "avfoundation", "-i", ":default",
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

// loadHostVoices parses `say -v '?'`.
//
// The format is name, then locale, then "# example", with the name padded by
// spaces — and names contain spaces themselves ("Grandma (Spanish (Spain))"),
// so the locale is found by looking for the field that LOOKS like one rather
// than by counting columns.
func listHostVoices() []hostVoice {
	if !Available() {
		return nil
	}
	output, err := exec.Command(sayPath, "-v", "?").Output()
	if err != nil {
		return nil
	}
	var voices []hostVoice
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line, _, _ := strings.Cut(scanner.Text(), "#")
		fields := strings.Fields(line)
		for index := len(fields) - 1; index >= 1; index-- {
			if !looksLikeLocale(fields[index]) {
				continue
			}
			name := strings.TrimSpace(strings.Join(fields[:index], " "))
			if name != "" {
				voices = append(voices, hostVoice{name: name, locale: fields[index]})
			}
			break
		}
	}
	return voices
}
