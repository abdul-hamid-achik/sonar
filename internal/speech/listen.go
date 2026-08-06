package speech

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Listening: capture a spoken utterance and transcribe it locally.
//
// Two subprocesses, both probed before use and both absent from a stock
// machine, so the capability reports honestly rather than failing at the moment
// someone speaks. Capture is ffmpeg because it is already how this project
// converts audio and because it is the one recorder present on both platforms
// worth targeting; transcription is whisper.cpp because it is MIT, ships as a
// small compiled binary, and does not drag a Python runtime into a Go project.
//
// # Why local only, for now
//
// A hosted transcriber would be faster and nearly free — Groq bills $0.04 an
// hour — but sending audio off the machine is a different decision from sending
// text, and not one this package should make on an operator's behalf. Voice
// carries who is in the room and what else was said. The interface below takes
// a Transcriber, so a hosted driver is a second implementation and not a
// rewrite, but nothing here reaches the network.
//
// # What is not tested
//
// Capture needs a microphone and, on macOS, a permission grant that only a
// human can give. The parts that can be tested are: whether the tools are
// present, how the pipeline is assembled, and what happens when either is
// missing. The recording itself is verified by using it.

// ErrNoCapture and ErrNoTranscriber name which half of the pipeline is absent,
// because "voice input unavailable" sends someone looking in the wrong place.
var (
	ErrNoCapture     = errors.New("speech: no audio recorder found (install ffmpeg)")
	ErrNoTranscriber = errors.New("speech: no local transcriber found (install whisper-cpp)")
	// ErrNoModel is separate from ErrNoTranscriber because the two are fixed by
	// different commands and the wrong one wastes an afternoon. Homebrew's
	// whisper-cpp installs the binary and no usable model at all, so this is
	// not an edge case — it is what a fresh install looks like.
	ErrNoModel = errors.New("speech: no whisper model found")
)

// Transcriber turns a recorded WAV file into text. It exists as an interface so
// a hosted driver can be added without touching capture, and so the local one
// can be replaced in a test without a microphone.
type Transcriber interface {
	Transcribe(ctx context.Context, wavPath string) (string, error)
	// Ready reports whether transcription can run right now, naming what is
	// missing when it cannot.
	//
	// It exists so the refusal can happen BEFORE the microphone opens. Checking
	// only at transcription time means recording someone speaking and then
	// telling them it was never going to work, which is the worst ordering
	// available — and is what shipped, because the availability check looked for
	// the binary and the model was checked three function calls later.
	Ready() error
}

// Listener records one utterance at a time.
//
// Toggle rather than hold: terminals only report key release under the Kitty
// keyboard protocol, so a hold-to-talk binding would work in Ghostty and do
// nothing in Terminal.app. A toggle behaves the same everywhere.
type Listener struct {
	mu          sync.Mutex
	command     *exec.Cmd
	wavPath     string
	transcriber Transcriber
}

// NewListener returns a Listener, or an error naming the missing half.
func NewListener(transcriber Transcriber) (*Listener, error) {
	if !CaptureAvailable() {
		return nil, ErrNoCapture
	}
	if transcriber == nil {
		transcriber = LocalTranscriber{}
	}
	return &Listener{transcriber: transcriber}, nil
}

// CaptureAvailable reports whether this host can record audio.
func CaptureAvailable() bool {
	path, err := exec.LookPath("ffmpeg")
	return err == nil && path != ""
}

// Start opens the microphone and begins recording to a temporary file.
//
// A real file rather than a pipe, deliberately: a WAV header carries the data
// size in a field that must be backpatched when the stream ends, which needs a
// seekable destination. Recording to stdout produces a header claiming a length
// nobody wrote, and every downstream tool reads it as a truncated or empty
// recording.
func (l *Listener) Start() error {
	if l == nil {
		return ErrNoCapture
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.command != nil {
		return nil
	}
	file, err := os.CreateTemp("", "sonar-utterance-*.wav")
	if err != nil {
		return fmt.Errorf("speech: create recording: %w", err)
	}
	path := file.Name()
	_ = file.Close()

	name, args := captureCommand(path, MaxUtterance)
	command := exec.Command(name, args...)
	if err := command.Start(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("speech: start recorder: %w", err)
	}
	l.command, l.wavPath = command, path
	return nil
}

// Stop ends the recording and returns the transcription.
//
// The recorder is signalled rather than killed, for the same reason everything
// else in this codebase is: ffmpeg finalizes the WAV header on SIGINT and
// leaves an unreadable file when killed outright. Waiting for it to exit is
// what makes the file safe to read.
func (l *Listener) Stop(ctx context.Context) (string, error) {
	if l == nil {
		return "", ErrNoCapture
	}
	l.mu.Lock()
	command, path := l.command, l.wavPath
	l.command, l.wavPath = nil, ""
	l.mu.Unlock()
	if command == nil {
		return "", nil
	}
	defer func() { _ = os.Remove(path) }()

	interruptRecorder(command)
	_ = command.Wait()

	info, err := os.Stat(path)
	if err != nil || info.Size() < minimumUtteranceBytes {
		// A recording too short to hold speech is a slip of the key, not an
		// utterance. Returning empty lets the caller drop it silently instead
		// of reporting a transcription failure nobody caused.
		return "", nil
	}
	return l.transcriber.Transcribe(ctx, path)
}

// Recording reports whether the microphone is currently open.
func (l *Listener) Recording() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.command != nil
}

// Cancel abandons a recording without transcribing it.
func (l *Listener) Cancel() {
	if l == nil {
		return
	}
	l.mu.Lock()
	command, path := l.command, l.wavPath
	l.command, l.wavPath = nil, ""
	l.mu.Unlock()
	if command == nil {
		return
	}
	interruptRecorder(command)
	_ = command.Wait()
	_ = os.Remove(path)
}

// minimumUtteranceBytes is roughly a fifth of a second of 16kHz mono 16-bit
// audio plus a header. Below this there is nothing to transcribe.
const minimumUtteranceBytes = 6 * 1024

// MaxUtterance is how long the RECORDER will run before stopping itself.
//
// The caller already bounds an utterance, and that bound is the one a user
// meets. This one exists for the case the caller cannot cover: a harness killed
// outright leaves no code to run a timer, and an orphaned recorder holds the
// microphone open and fills a disk for as long as the machine is up. A process
// that cannot be told to stop has to know when to.
//
// Longer than the caller's timeout on purpose. In a live session the caller
// always closes the microphone first and says why; reaching this one means
// nothing was left to ask.
const MaxUtterance = 3 * time.Minute

// captureLimitSeconds renders a duration for ffmpeg's -t, which takes seconds.
func captureLimitSeconds(limit time.Duration) string {
	if limit <= 0 {
		limit = MaxUtterance
	}
	return strconv.FormatInt(int64(limit.Seconds()), 10)
}

// LocalTranscriber runs whisper.cpp on the recorded file.
type LocalTranscriber struct {
	// Model is the ggml model path. Empty uses the conventional location a
	// whisper.cpp install writes to.
	Model string
	// Language is an ISO code. Empty lets whisper detect, which costs a little
	// latency and is right for a bilingual user.
	Language string
}

// TranscriberAvailable reports whether the default local transcriber can run.
func TranscriberAvailable() bool {
	return LocalTranscriber{}.Ready() == nil
}

// Ready checks both halves: the binary, and a model it can load.
func (t LocalTranscriber) Ready() error {
	if _, err := exec.LookPath(whisperBinary); err != nil {
		return ErrNoTranscriber
	}
	if t.resolveModel() == "" {
		return ErrNoModel
	}
	return nil
}

// resolveModel is the configured model if it names a readable file, otherwise
// whatever the host has. A configured path that does not exist returns empty
// rather than being passed through: whisper fails with its own message about a
// file, and the caller can say something more useful first.
func (t LocalTranscriber) resolveModel() string {
	if model := strings.TrimSpace(t.Model); model != "" {
		if info, err := os.Stat(model); err == nil && info.Mode().IsRegular() {
			return model
		}
		return ""
	}
	return defaultWhisperModel()
}

// whisperBinary is whisper.cpp's CLI. The name changed from `main` to
// `whisper-cli` upstream; only the current one is looked for, because silently
// accepting a binary called `main` from PATH is how a project ends up running
// something unrelated.
const whisperBinary = "whisper-cli"

func (t LocalTranscriber) Transcribe(ctx context.Context, wavPath string) (string, error) {
	if err := t.Ready(); err != nil {
		return "", err
	}
	model := t.resolveModel()
	args := []string{"-m", model, "-f", wavPath, "--no-timestamps", "--no-prints", "-otxt", "-of", wavPath}
	if language := strings.TrimSpace(t.Language); language != "" {
		args = append(args, "-l", language)
	}
	command := exec.CommandContext(ctx, whisperBinary, args...)
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("speech: transcribe: %w", err)
	}
	transcript, err := os.ReadFile(wavPath + ".txt")
	defer func() { _ = os.Remove(wavPath + ".txt") }()
	if err != nil {
		return "", fmt.Errorf("speech: read transcript: %w", err)
	}
	return strings.TrimSpace(string(transcript)), nil
}

// defaultWhisperModel finds a usable model wherever whisper.cpp installs put
// them.
//
// It globs rather than naming four files. The previous version listed
// ggml-base, ggml-base.en, ggml-small and ggml-tiny, and on the machine this
// was written for the answer was none of them: Homebrew's whisper-cpp ships
// exactly one file, `for-tests-ggml-tiny.bin`, and someone who downloads
// ggml-large-v3-turbo.bin by hand had it ignored too. A directory of models
// should be read as a directory of models.
//
// Smaller is preferred. Dictation is re-read by the person who spoke it, so
// seconds of latency per utterance cost more than the accuracy they buy — and
// file size orders the whisper family correctly, tiny through large, without
// this needing to know their names.
func defaultWhisperModel() string {
	var best string
	var bestSize int64
	for _, root := range whisperModelRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !usableWhisperModel(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || info.Size() < minimumWhisperModelBytes {
				continue
			}
			if best == "" || info.Size() < bestSize {
				best, bestSize = filepath.Join(root, entry.Name()), info.Size()
			}
		}
	}
	return best
}

func whisperModelRoots() []string {
	roots := []string{"/opt/homebrew/share/whisper-cpp", "/usr/local/share/whisper-cpp"}
	home, err := os.UserHomeDir()
	if err != nil {
		return roots
	}
	return append([]string{
		filepath.Join(home, ".cache", "whisper.cpp"),
		filepath.Join(home, ".local", "share", "whisper.cpp"),
		filepath.Join(home, "Library", "Application Support", "whisper.cpp"),
		filepath.Join(home, "models"),
	}, roots...)
}

// usableWhisperModel rejects the one file Homebrew actually ships.
//
// `for-tests-ggml-tiny.bin` is 575 KB of fixture built to make whisper.cpp's
// own suite run, and it transcribes real speech into noise. Accepting it would
// be worse than finding nothing: "dictation is broken" sends someone looking at
// their microphone, while "no model" names the thing to fix.
func usableWhisperModel(name string) bool {
	return strings.HasPrefix(name, "ggml-") && strings.HasSuffix(name, ".bin")
}

// minimumWhisperModelBytes is below the smallest real model (tiny is ~75 MB)
// and above every fixture. Size is the check because the fixture's NAME is a
// convention nobody promised to keep.
const minimumWhisperModelBytes = 20 << 20

// ModelDownloadHint is the command that fixes ErrNoModel.
//
// It exists because the error is otherwise a dead end. Homebrew's whisper-cpp
// ships `for-tests-ggml-tiny.bin` and no downloader, whisper.cpp's own
// download script is in a repository the user does not have, and "install a
// model" leaves someone searching for a file name and a URL. The multilingual
// base model is the recommendation rather than base.en: this harness is used in
// more than one language, and the English-only variant silently mis-transcribes
// the others rather than failing.
func ModelDownloadHint() string {
	root := "~/.cache/whisper.cpp"
	return "mkdir -p " + root + " && curl -L -o " + root + "/ggml-base.bin " +
		"https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.bin"
}
