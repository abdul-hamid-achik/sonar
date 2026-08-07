package speech

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The hosted driver is told nothing about language, and that is the finding it
// was built on rather than an omission.
//
// Measured through four engines and transcribed back: every one TOLD the
// language read the English vocabulary with that language's rules — xAI given
// es-MX produced "Merch", "Catch", "Diploi", "Geet", exactly the way `say`
// does — and every one left to detect it handled the mixture. So Needs is empty
// in both fields, and Model.forDriver enforces the other half.
func TestTheHostedDriverIsToldNothing(t *testing.T) {
	driver := &hostedDriver{}
	if needs := driver.Needs(); needs.Language || needs.Respelling {
		t.Fatalf("the hosted driver asks for what makes it worse: %+v", needs)
	}
}

// It refuses before it speaks, naming which half is missing.
//
// The same ordering the dictation pipeline uses: a driver that authenticates on
// its first sentence reports the failure at the moment somebody was waiting to
// hear something.
func TestTheHostedDriverRefusesBeforeItSpeaks(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := newHostedDriver(""); err == nil {
		t.Fatal("a hosted driver was built with no credential")
	} else if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("the refusal does not name what is missing: %v", err)
	}
}

// An unknown provider is an error, never a quiet fallback to the local voice.
//
// Somebody who asked for the hosted engine and silently got `say` would hear
// the exact mispronunciation they were trying to fix, with nothing saying why.
func TestAnUnknownProviderIsRefused(t *testing.T) {
	if _, err := NewWithProvider("elevenlabs", "", 0, nil); err == nil {
		t.Fatal("an unknown provider fell back instead of failing")
	} else if !strings.Contains(err.Error(), "elevenlabs") {
		t.Fatalf("the error does not name the provider asked for: %v", err)
	}
}

// The request carries the sentence and nothing else, and a refusal is reported
// with the reason the service gave.
func TestTheHostedRequestAsksForWhatItPlays(t *testing.T) {
	var body []byte
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAllLimited(r)
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("fake-mp3-bytes"))
	}))
	defer server.Close()

	driver := &hostedDriver{
		endpoint: server.URL, key: "k", model: hostedModel,
		voice: hostedVoiceID, client: server.Client(),
	}
	audio, err := driver.synthesize(context.Background(), "Ya quedó todo listo.")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if string(audio) != "fake-mp3-bytes" {
		t.Fatalf("the audio was not returned verbatim: %q", audio)
	}
	if auth != "Bearer k" {
		t.Fatalf("the credential did not reach the request: %q", auth)
	}
	for _, want := range []string{"Ya quedó todo listo.", hostedModel, "mp3"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the request does not carry %q: %s", want, body)
		}
	}
	// No language field: the whole point is that it detects rather than be told.
	if strings.Contains(string(body), "language") {
		t.Errorf("the request tells the engine a language: %s", body)
	}

	// A refusal quotes the service's own reason — no credits, bad key, rate
	// limit — because those are fixed in four different places.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"You have no credits remaining."}}`))
	}))
	defer failing.Close()
	driver.endpoint, driver.client = failing.URL, failing.Client()
	if _, err := driver.synthesize(context.Background(), "Hola."); err == nil {
		t.Fatal("a rejected request reported success")
	} else if !strings.Contains(err.Error(), "no credits") {
		t.Fatalf("the failure lost the reason the service gave: %v", err)
	}
}

func readAllLimited(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(io.LimitReader(r.Body, 4096))
}

// An oversized response is refused, not truncated.
//
// A LimitReader alone returns a short read with a nil error, and the cut lands
// wherever it lands — mid-frame, into a player that would render whatever the
// fragment happened to decode to.
func TestAnOversizedResponseIsRefused(t *testing.T) {
	flood := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 1<<20)
		for range 6 {
			_, _ = w.Write(chunk)
		}
	}))
	defer flood.Close()

	driver := &hostedDriver{
		endpoint: flood.URL, key: "k", model: hostedModel,
		voice: hostedVoiceID, client: flood.Client(),
	}
	if _, err := driver.synthesize(context.Background(), "Hola."); err == nil {
		t.Fatal("an oversized response was accepted and truncated")
	}
}

// Cancelling a run stops a request that is still in flight, rather than leaving
// the Speaker's only worker waiting out the full timeout for audio that has
// nowhere left to go.
func TestCancellingAVoiceStopsARequestInFlight(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)

	driver := &hostedDriver{
		endpoint: slow.URL, key: "k", model: hostedModel,
		voice: hostedVoiceID, client: slow.Client(),
	}
	ctx, abort := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := driver.synthesize(ctx, "Hola."); done <- err }()

	abort()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled request reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the run did not stop the request in flight")
	}
}
