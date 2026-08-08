package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// A hosted synthesizer, driven the same way the host one is.
//
// It exists because of a measurement, not a preference. The same sentence —
// "Hice el merge del package, limpié el cache y el deploy con git ya quedó." —
// was generated through four engines and transcribed back with the local
// Whisper. `say` with a Spanish voice returned "el merch … el caché … confit".
// OpenAI's gpt-4o-mini-tts returned every technical term intact, with the
// Spanish around them intact too. That is the gap this closes: a bilingual
// answer read as one language rather than as two badly.
//
// # Why it is off by default anyway
//
// It costs money per turn, it needs a second API key, and it puts a network
// round trip between a sentence and its audio — measured at about 1.9 seconds
// against `say`, which starts immediately and is free. The spoken digest makes
// that one round trip per turn instead of twenty, which is what makes it
// tolerable at all; it does not make it free.
//
// # Needs nothing
//
// The measurement that justified this driver also decided its Needs. Every
// engine TOLD the language applied that language's letter-to-sound rules to the
// English vocabulary — xAI given es-MX failed exactly the way `say` does — and
// every engine left to detect it handled the mixture. So this one is never told
// a language and never receives the respelling table, and `Model.forDriver`
// enforces both from the other side.
//
// # One player per run, not per sentence
//
// MP3 frames concatenate: measured, a 5.904s file followed by a 5.808s file
// plays as 11.712s, and ffplay reads the whole thing from a pipe. So a run is
// one ffplay process fed each sentence's audio as it arrives, which is the same
// shape `say` has — continuous speech, no fork between sentences, and a Close
// that lets the buffer drain. afplay cannot do this: it takes a file path and
// has no pipe mode at all.

const (
	// hostedEndpoint is OpenAI's speech endpoint. The provider is named in
	// config rather than detected, for the same reason the inference dialect is
	// chosen from provider identity: an endpoint that happens to accept the
	// shape is not the same as one that means it.
	hostedEndpoint = "https://api.openai.com/v1/audio/speech"
	hostedModel    = "gpt-4o-mini-tts"
	hostedVoiceID  = "nova"
	// hostedRequestTimeout bounds one sentence. Measured at 1.9s for a short
	// one; anything past this is a stall rather than a long sentence, and a
	// stalled request must not hold the queue behind it.
	hostedRequestTimeout = 30 * time.Second
)

// hostedDriver turns sentences into audio over HTTP and plays them locally.
type hostedDriver struct {
	endpoint string
	key      string
	model    string
	voice    string
	client   *http.Client
}

// newHostedDriver returns a driver, or an error naming what is missing.
//
// Both halves are checked before anything is spoken, the same way the dictation
// pipeline refuses before opening a microphone: a driver that authenticates on
// the first sentence reports its failure at the moment somebody was waiting to
// hear something.
func newHostedDriver(voice, model, endpoint string) (Driver, error) {
	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		return nil, fmt.Errorf("speech: hosted voice needs OPENAI_API_KEY")
	}
	if _, err := exec.LookPath("ffplay"); err != nil {
		// ffplay ships with the ffmpeg this project already requires for
		// dictation, so this is usually already satisfied — but it is a separate
		// binary and some packagers split it out.
		return nil, fmt.Errorf("speech: hosted voice needs ffplay (it ships with ffmpeg)")
	}
	if voice = strings.TrimSpace(voice); voice == "" {
		voice = hostedVoiceID
	}
	if model = strings.TrimSpace(model); model == "" {
		model = hostedModel
	}
	if endpoint = strings.TrimSpace(endpoint); endpoint == "" {
		endpoint = hostedEndpoint
	}
	return &hostedDriver{
		endpoint: endpoint,
		key:      key,
		model:    model,
		voice:    voice,
		client:   &http.Client{Timeout: hostedRequestTimeout},
	}, nil
}

// Needs nothing. See the package comment above: telling one of these engines
// the language is what makes it read English vocabulary with Spanish rules.
func (d *hostedDriver) Needs() Needs { return Needs{} }

func (d *hostedDriver) Open(string) (Voice, error) {
	// -autoexit so closing the pipe ends the process, -nodisp because there is
	// no window to draw, and pipe:0 because the audio arrives a sentence at a
	// time rather than as a file.
	player := exec.Command("ffplay", "-nodisp", "-autoexit", "-loglevel", "error", "-i", "pipe:0")
	stdin, err := player.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("speech: open player input: %w", err)
	}
	if err := player.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("speech: start player: %w", err)
	}
	ctx, abort := context.WithCancel(context.Background())
	run := &hostedVoice{
		driver: d, player: player, stdin: stdin,
		exited: make(chan struct{}), ctx: ctx, abort: abort,
	}
	go func() {
		_ = player.Wait()
		close(run.exited)
	}()
	return run, nil
}

// hostedVoice is one run: one player process, fed sentence by sentence.
type hostedVoice struct {
	driver *hostedDriver
	mu     sync.Mutex
	player *exec.Cmd
	stdin  io.WriteCloser
	exited chan struct{}
	// abort cancels a request that is still in flight.
	//
	// Cancelling the player is not cancelling the run: a slow endpoint left Say
	// blocked for the full request timeout after the audio had already been
	// killed, holding the Speaker's only worker on a sentence nobody would
	// hear. The context ties the two together.
	abort context.CancelFunc
	ctx   context.Context
}

// Say fetches one sentence's audio and hands it to the player.
//
// Synchronous on purpose. The Speaker calls this from its own worker, which is
// exactly the goroutine that should wait — and the wait overlaps with playback
// for free, because the player is still working through what it was given. A
// second queue inside the driver would buy nothing that ffplay's buffer does
// not already provide.
func (v *hostedVoice) Say(sentence string) error {
	v.mu.Lock()
	stdin := v.stdin
	v.mu.Unlock()
	if stdin == nil {
		return io.ErrClosedPipe
	}
	audio, err := v.driver.synthesize(v.ctx, sentence)
	if err != nil {
		return err
	}
	_, err = stdin.Write(audio)
	return err
}

func (d *hostedDriver) synthesize(parent context.Context, sentence string) ([]byte, error) {
	body, err := json.Marshal(map[string]string{
		"model": d.model,
		"input": sentence,
		"voice": d.voice,
		// mp3 because its frames concatenate, which is what lets one player
		// read a whole run. A headerless format would need the sample rate
		// carried out of band and a container would break at every join.
		"response_format": "mp3",
	})
	if err != nil {
		return nil, fmt.Errorf("speech: encode request: %w", err)
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, hostedRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("speech: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+d.key)
	request.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("speech: hosted voice: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		// The body carries the reason — no credits, bad key, rate limit — and
		// it is bounded before being quoted, because it is remote text.
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, fmt.Errorf("speech: hosted voice returned %d: %s",
			response.StatusCode, strings.TrimSpace(string(detail)))
	}
	// One byte past the bound, so an oversized response is REFUSED rather than
	// truncated. A LimitReader alone returns a short read with a nil error, and
	// the cut lands wherever it lands — mid-frame, into a player that would then
	// render whatever the fragment happened to decode to.
	audio, err := io.ReadAll(io.LimitReader(response.Body, hostedMaxAudioBytes+1))
	if err != nil {
		return nil, fmt.Errorf("speech: read hosted audio: %w", err)
	}
	if len(audio) > hostedMaxAudioBytes {
		return nil, fmt.Errorf("speech: hosted voice returned more than %d bytes for one sentence", hostedMaxAudioBytes)
	}
	return audio, nil
}

// hostedMaxAudioBytes bounds one sentence's audio. At 24kHz/128kbps a minute is
// about 960 KB, and no single projected sentence is a minute long — so this is
// a runaway guard on a remote response, not a limit anybody meets.
const hostedMaxAudioBytes = 4 << 20

func (v *hostedVoice) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stdin == nil {
		return nil
	}
	// The run is over, so nothing new should be fetched for it. Anything
	// already written still plays: closing the pipe is what lets it drain.
	if v.abort != nil {
		v.abort()
	}
	err := v.stdin.Close()
	v.stdin = nil
	return err
}

func (v *hostedVoice) Cancel() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.abort != nil {
		// Before the player, so a Say waiting on the network returns instead of
		// finishing a request whose audio has nowhere left to go.
		v.abort()
	}
	if v.stdin != nil {
		_ = v.stdin.Close()
		v.stdin = nil
	}
	if v.player != nil && v.player.Process != nil {
		// Same signal and the same reason as the host synthesizer: a player
		// killed outright can leave the output route claimed.
		signalSynthesizer(v.player)
	}
}

func (v *hostedVoice) Done() <-chan struct{} { return v.exited }
