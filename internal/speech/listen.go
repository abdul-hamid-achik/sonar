package speech

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	// level is guarded separately from the process handles, because the UI reads
	// it on every frame and Stop holds mu across a subprocess Wait. Sharing one
	// mutex would stall the render loop for as long as ffmpeg takes to finalize
	// a header.
	levelMu sync.Mutex
	// loudness is the most recent momentary reading in LUFS, and heardAt is when
	// the microphone last carried something louder than a quiet room. Both are
	// zero-valued until the recorder reports, which is what "no reading yet"
	// has to look like — distinct from "reported silence".
	loudness  float64
	reported  bool
	heardAt   time.Time
	startedAt time.Time
	// floor and ceiling calibrate the meter to THIS room and THIS microphone.
	//
	// A fixed scale cannot work, and the first version proved it: anchored at
	// -60 LUFS on the assumption that a quiet input reads near digital silence,
	// it turned out that only the frames before the device delivers audio read
	// that low. Measured on a real machine, an empty room sits between -36 and
	// -21 — which the fixed scale rendered at four fifths of full, so the meter
	// was pinned near the top before anyone spoke and had almost no range left
	// for a voice. Every microphone and every room differ by more than the
	// distance between silence and speech, so the scale has to come from what
	// this one is actually reporting.
	floor      float64
	ceiling    float64
	calibrated bool
	// epoch identifies the recording these readings belong to.
	//
	// Stop waits for ffmpeg but not for the goroutine reading its diagnostics,
	// and os/exec closes that pipe when the process is reaped — so a reader can
	// still be holding a buffered line when the NEXT recording starts. The
	// mutex stops a data race; it does not stop one recording's levels being
	// written into another's calibration.
	epoch uint64
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
	// The recorder's own diagnostics are the only live signal there is about
	// whether the microphone is picking anything up. Without them an open
	// microphone and a muted one are the same screen, which is the state
	// somebody sits in talking to a harness that is recording silence.
	diagnostics, err := command.StderrPipe()
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("speech: open recorder diagnostics: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("speech: start recorder: %w", err)
	}
	l.command, l.wavPath = command, path
	epoch := l.resetLevel()
	go l.readLevels(epoch, diagnostics)
	return nil
}

// loudnessPattern matches the momentary loudness ffmpeg's ebur128 filter prints
// while it passes the audio through untouched.
//
// The reading arrives as "M:-120.7" during silence and "M: -22.4" once someone
// speaks, so the space is optional and the sign is not.
var loudnessPattern = regexp.MustCompile(`M:\s*(-?\d+(?:\.\d+)?)`)

// readLevels follows the recorder's diagnostics until it exits.
//
// It reads to the end even after the numbers stop being wanted: a stderr pipe
// nobody drains fills, and a full pipe blocks the recorder mid-utterance.
func (l *Listener) readLevels(epoch uint64, diagnostics io.ReadCloser) {
	// Whatever happens to the parsing, the pipe keeps being drained: a reader
	// that stops reading lets the recorder fill its stderr buffer and block
	// mid-utterance, which would look like the microphone freezing. Measured on
	// this ffmpeg, a two-minute recording holds its longest unbroken line to
	// about 8 KB against the scanner's 64 KB — so the bound is not reachable
	// today. It is drained anyway, because "not reachable today" is a property
	// of one program's logging and not of this code.
	defer func() { _, _ = io.Copy(io.Discard, diagnostics) }()
	scanner := bufio.NewScanner(diagnostics)
	for scanner.Scan() {
		match := loudnessPattern.FindStringSubmatch(scanner.Text())
		if match == nil {
			continue
		}
		loudness, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		l.noteLoudness(epoch, loudness)
	}
}

func (l *Listener) noteLoudness(epoch uint64, loudness float64) {
	l.levelMu.Lock()
	defer l.levelMu.Unlock()
	if epoch != l.epoch {
		// A leftover line from a recording that has already ended.
		return
	}
	if loudness < noAudioLUFS {
		// The device has not started delivering audio yet. These frames read
		// near digital silence and are not a quiet room — calibrating on one
		// would put the room's own tone at the top of the scale, which is
		// exactly what the fixed scale did.
		return
	}
	l.loudness, l.reported = loudness, true
	if !l.calibrated {
		l.floor, l.ceiling, l.calibrated = loudness, loudness+speechRangeDB, true
	}
	if loudness < l.floor {
		l.floor = loudness
		l.ceiling = max(l.ceiling, l.floor+speechRangeDB)
	}
	if loudness > l.ceiling {
		l.ceiling = loudness
	}
	if loudness >= l.floor+speechMarginDB {
		l.heardAt = time.Now()
	}
}

func (l *Listener) resetLevel() uint64 {
	l.levelMu.Lock()
	defer l.levelMu.Unlock()
	l.loudness, l.reported, l.heardAt = 0, false, time.Time{}
	l.floor, l.ceiling, l.calibrated = 0, 0, false
	l.startedAt = time.Now()
	l.epoch++
	return l.epoch
}

// Level reports how loud the microphone is right now, from 0 to 1.
//
// Normalized from LUFS rather than reported raw, because the caller is drawing
// a meter and not writing a mastering tool. Digital silence reads around -120
// and ordinary speech lands between -30 and -15, so the scale is anchored there:
// below quietFloorLUFS is nothing, above loudCeilingLUFS is full.
func (l *Listener) Level() float64 {
	if l == nil {
		return 0
	}
	l.levelMu.Lock()
	defer l.levelMu.Unlock()
	if !l.reported || !l.calibrated || l.ceiling <= l.floor {
		return 0
	}
	level := (l.loudness - l.floor) / (l.ceiling - l.floor)
	return min(1, max(0, level))
}

// Hearing reports whether the microphone has carried anything but room noise
// recently, and whether it has had long enough to say so.
//
// Two answers, because "nothing yet" and "nothing for a while" call for
// different words. A meter that sat at zero for six seconds while somebody
// talked into it is reporting a muted input or the wrong device, and that is
// worth saying out loud — it is the whole complaint about dictation, and the
// harness has the evidence to answer it.
func (l *Listener) Hearing() (heard bool, longEnoughToTell bool) {
	if l == nil {
		return false, false
	}
	l.levelMu.Lock()
	defer l.levelMu.Unlock()
	if l.startedAt.IsZero() {
		return false, false
	}
	since := time.Since(l.startedAt)
	return !l.heardAt.IsZero(), since >= silencePatience
}

const (
	// noAudioLUFS separates "the device has not started" from "a quiet room".
	// Frames before the input delivers audio read around -120; no real room
	// does. Anything below this is discarded rather than calibrated on.
	noAudioLUFS = -70
	// speechRangeDB is how far above the room's own floor the meter runs to
	// full. Speech at a normal distance sits well above room tone, and pinning
	// the top to a fixed level would put a loud room permanently at the ceiling
	// for the same reason a fixed floor put a quiet one there.
	speechRangeDB = 18
	// speechMarginDB is how far above the floor counts as "something was said"
	// rather than as the room. Below it, the silence warning is what the rail
	// reports.
	speechMarginDB = 8
	// silencePatience is how long to wait before saying nothing is coming
	// through. Long enough to cover somebody collecting their thoughts, short
	// enough to catch them before they have said a whole paragraph to a muted
	// microphone.
	silencePatience = 4 * time.Second
)

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
	// Registered BEFORE the run, not after it. whisper writes its transcript
	// beside the recording, so a run that fails or is cancelled mid-way can
	// leave that file behind — and it is not a stray temporary, it is what the
	// person said, sitting in a world-readable directory with nothing left to
	// remove it. Same class as the orphaned recorder: a privacy failure wearing
	// a resource leak's clothes.
	defer func() { _ = os.Remove(wavPath + ".txt") }()
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("speech: transcribe: %w", err)
	}
	transcript, err := os.ReadFile(wavPath + ".txt")
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

// NewSilentListenerForTest returns a Listener that reports an open microphone
// which has been hearing nothing for longer than the harness waits.
//
// Exported for the UI package, which owns the sentence that gets said about it
// and has no way to produce the state otherwise: driving a real microphone in a
// unit test needs a permission grant only a human can give, and the whole point
// of the silence warning is the case where that grant is what is missing.
func NewSilentListenerForTest(startedAt time.Time) *Listener {
	listener := &Listener{transcriber: LocalTranscriber{}}
	listener.epoch = 1
	listener.startedAt = startedAt
	// Room tone and nothing else. Deliberately NOT digital silence: a real
	// microphone in a real room reports around -25, and the version of this
	// that used -120 described a state no recording produces.
	listener.noteLoudness(listener.epoch, -25)
	return listener
}
