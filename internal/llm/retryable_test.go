package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestIsRetryableTransportClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"wrapped cancelled", &url.Error{Op: "Post", URL: "http://x", Err: context.Canceled}, false},
		{"stream idle", fmt.Errorf("read Ollama stream: %w", ErrStreamIdle), true},
		{"unexpected eof", fmt.Errorf("read Ollama stream: %w", io.ErrUnexpectedEOF), true},
		{"eof", io.EOF, true},
		{"server 500", &ollamaHTTPError{StatusCode: http.StatusInternalServerError, Status: "500"}, true},
		{"server 503", &ollamaHTTPError{StatusCode: http.StatusServiceUnavailable, Status: "503"}, true},
		{"throttled 429", &ollamaHTTPError{StatusCode: http.StatusTooManyRequests, Status: "429"}, true},
		{"client 404", &ollamaHTTPError{StatusCode: http.StatusNotFound, Status: "404"}, false},
		{"client 400", &ollamaHTTPError{StatusCode: http.StatusBadRequest, Status: "400"}, false},
		{"net op error", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, true},
		{"plain provider error", errors.New("model requires more system memory"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRetryableTransport(tt.err); got != tt.want {
				t.Fatalf("IsRetryableTransport(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestOllamaChatStreamIdleWatchdogFailsHungStream(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"partial"},"done":false}`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	previous := streamIdleTimeout
	streamIdleTimeout = 100 * time.Millisecond
	defer func() { streamIdleTimeout = previous }()

	client, err := NewOllamaClient(server.URL, "qwen", 4096)
	if err != nil {
		t.Fatal(err)
	}
	sawText := false
	streamErr := client.ChatStream(context.Background(), ChatOptions{}, func(chunk StreamChunk) error {
		if chunk.Text != "" {
			sawText = true
		}
		return nil
	})
	if !sawText {
		t.Fatal("stream callback never observed the delivered chunk")
	}
	if !errors.Is(streamErr, ErrStreamIdle) {
		t.Fatalf("hung stream error = %v, want ErrStreamIdle", streamErr)
	}
	if !IsRetryableTransport(streamErr) {
		t.Fatalf("idle watchdog error must be retryable: %v", streamErr)
	}
}

func TestOllamaChatStreamIdleWatchdogAllowsSlowButLiveStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"t"},"done":false}`)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(60 * time.Millisecond)
		}
		_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":""},"done":true,"eval_count":3,"prompt_eval_count":1}`)
	}))
	defer server.Close()

	previous := streamIdleTimeout
	streamIdleTimeout = 150 * time.Millisecond
	defer func() { streamIdleTimeout = previous }()

	client, err := NewOllamaClient(server.URL, "qwen", 4096)
	if err != nil {
		t.Fatal(err)
	}
	chunks := 0
	if err := client.ChatStream(context.Background(), ChatOptions{}, func(StreamChunk) error {
		chunks++
		return nil
	}); err != nil {
		t.Fatalf("live-but-slow stream failed: %v", err)
	}
	if chunks < 4 {
		t.Fatalf("streamed chunks = %d, want at least 4", chunks)
	}
}
