package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestParseRetryAfterHeaderFormats exercises the RFC 9110 delta-seconds and
// HTTP-date forms directly, independent of any transport.
func TestParseRetryAfterHeaderFormats(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		want   time.Duration
		wantOK bool
	}{
		{"empty", "", 0, false},
		{"whitespace only", "   ", 0, false},
		{"delta seconds", "120", 120 * time.Second, true},
		{"delta seconds zero", "0", 0, true},
		{"delta seconds negative", "-5", 0, false},
		{"garbage", "not-a-value", 0, false},
		{"http-date future", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{"http-date past", now.Add(-90 * time.Second).Format(http.TimeFormat), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tt.header, now)
			if ok != tt.wantOK {
				t.Fatalf("parseRetryAfter(%q) ok = %v, want %v", tt.header, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

// TestProviderStatusClassificationOverHTTP drives real httptest servers
// through the OpenAI-compatible dialect (which Anthropic's dialect shares via
// the same openAIHTTPError type) and asserts the full bounded-status
// contract: the status code is recoverable, Retry-After is honored when
// present, and the two retry predicates disagree exactly where they should —
// a 429 must never be classified as an immediate transport retry, but it
// must still be classified as eventually retryable so a caller can apply
// ProviderRetryAfter or its own backoff.
func TestProviderStatusClassificationOverHTTP(t *testing.T) {
	tests := []struct {
		name               string
		status             int
		retryAfterHeader   string
		wantRetryTransport bool
		wantRetryProvider  bool
		wantRetryAfter     time.Duration
		wantRetryAfterOK   bool
	}{
		{
			name:               "429 rate limited with retry-after seconds",
			status:             http.StatusTooManyRequests,
			retryAfterHeader:   "2",
			wantRetryTransport: false,
			wantRetryProvider:  true,
			wantRetryAfter:     2 * time.Second,
			wantRetryAfterOK:   true,
		},
		{
			name:               "429 rate limited without retry-after header",
			status:             http.StatusTooManyRequests,
			wantRetryTransport: false,
			wantRetryProvider:  true,
		},
		{
			name:               "401 unauthorized",
			status:             http.StatusUnauthorized,
			wantRetryTransport: false,
			wantRetryProvider:  false,
		},
		{
			name:               "403 forbidden",
			status:             http.StatusForbidden,
			wantRetryTransport: false,
			wantRetryProvider:  false,
		},
		{
			name:               "404 not found",
			status:             http.StatusNotFound,
			wantRetryTransport: false,
			wantRetryProvider:  false,
		},
		{
			name:               "400 bad request",
			status:             http.StatusBadRequest,
			wantRetryTransport: false,
			wantRetryProvider:  false,
		},
		{
			name:               "408 request timeout",
			status:             http.StatusRequestTimeout,
			wantRetryTransport: true,
			wantRetryProvider:  true,
		},
		{
			name:               "500 internal server error",
			status:             http.StatusInternalServerError,
			wantRetryTransport: true,
			wantRetryProvider:  true,
		},
		{
			name:               "502 bad gateway",
			status:             http.StatusBadGateway,
			wantRetryTransport: true,
			wantRetryProvider:  true,
		},
		{
			name:               "503 service unavailable with retry-after",
			status:             http.StatusServiceUnavailable,
			retryAfterHeader:   "5",
			wantRetryTransport: true,
			wantRetryProvider:  true,
			wantRetryAfter:     5 * time.Second,
			wantRetryAfterOK:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfterHeader != "" {
					w.Header().Set("Retry-After", tt.retryAfterHeader)
				}
				w.WriteHeader(tt.status)
				// The body intentionally carries provider-authored prose. This
				// test never asserts that text reaches the caller — only the
				// bounded status/retry-after facts extracted from it. Whether
				// that prose is permitted past the transcript boundary is the
				// agent layer's sanitization contract, not this package's.
				_, _ = w.Write([]byte(`{"error":{"message":"synthetic provider failure detail"}}`))
			}))
			defer server.Close()

			client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
				BaseURL: server.URL,
				Model:   "test-model",
				APIKey:  "test-key",
			})
			if err != nil {
				t.Fatal(err)
			}
			streamErr := client.ChatStream(context.Background(), ChatOptions{
				Messages: []Message{{Role: "user", Content: "hi"}},
			}, func(StreamChunk) error { return nil })
			if streamErr == nil {
				t.Fatal("expected an error from the non-2xx response")
			}

			if got := IsRetryableTransport(streamErr); got != tt.wantRetryTransport {
				t.Fatalf("IsRetryableTransport = %v, want %v", got, tt.wantRetryTransport)
			}
			if got := IsRetryableProviderError(streamErr); got != tt.wantRetryProvider {
				t.Fatalf("IsRetryableProviderError = %v, want %v", got, tt.wantRetryProvider)
			}
			status, ok := ProviderHTTPStatus(streamErr)
			if !ok || status != tt.status {
				t.Fatalf("ProviderHTTPStatus = (%d, %v), want (%d, true)", status, ok, tt.status)
			}
			gotRetryAfter, gotOK := ProviderRetryAfter(streamErr)
			if gotOK != tt.wantRetryAfterOK || gotRetryAfter != tt.wantRetryAfter {
				t.Fatalf("ProviderRetryAfter = (%v, %v), want (%v, %v)", gotRetryAfter, gotOK, tt.wantRetryAfter, tt.wantRetryAfterOK)
			}
		})
	}
}

// TestOllamaProviderStatusClassification proves the same bounded-status
// contract holds for Ollama's independent ollamaHTTPError shape, not just
// the OpenAI/Anthropic dialect's openAIHTTPError — ProviderHTTPStatus,
// IsRetryableTransport, and IsRetryableProviderError must all recognize both.
func TestOllamaProviderStatusClassification(t *testing.T) {
	tests := []struct {
		name               string
		status             int
		retryAfterHeader   string
		wantRetryTransport bool
		wantRetryProvider  bool
		wantRetryAfter     time.Duration
		wantRetryAfterOK   bool
	}{
		{
			name:               "429 with retry-after",
			status:             http.StatusTooManyRequests,
			retryAfterHeader:   "3",
			wantRetryTransport: false,
			wantRetryProvider:  true,
			wantRetryAfter:     3 * time.Second,
			wantRetryAfterOK:   true,
		},
		{
			name:               "401 unauthorized",
			status:             http.StatusUnauthorized,
			wantRetryTransport: false,
			wantRetryProvider:  false,
		},
		{
			name:               "503 service unavailable",
			status:             http.StatusServiceUnavailable,
			wantRetryTransport: true,
			wantRetryProvider:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfterHeader != "" {
					w.Header().Set("Retry-After", tt.retryAfterHeader)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":"synthetic ollama failure"}`))
			}))
			defer server.Close()

			client, err := NewOllamaClient(server.URL, "qwen", 4096)
			if err != nil {
				t.Fatal(err)
			}
			streamErr := client.ChatStream(context.Background(), ChatOptions{}, func(StreamChunk) error { return nil })
			if streamErr == nil {
				t.Fatal("expected an error from the non-2xx response")
			}

			if got := IsRetryableTransport(streamErr); got != tt.wantRetryTransport {
				t.Fatalf("IsRetryableTransport = %v, want %v", got, tt.wantRetryTransport)
			}
			if got := IsRetryableProviderError(streamErr); got != tt.wantRetryProvider {
				t.Fatalf("IsRetryableProviderError = %v, want %v", got, tt.wantRetryProvider)
			}
			status, ok := ProviderHTTPStatus(streamErr)
			if !ok || status != tt.status {
				t.Fatalf("ProviderHTTPStatus = (%d, %v), want (%d, true)", status, ok, tt.status)
			}
			gotRetryAfter, gotOK := ProviderRetryAfter(streamErr)
			if gotOK != tt.wantRetryAfterOK || gotRetryAfter != tt.wantRetryAfter {
				t.Fatalf("ProviderRetryAfter = (%v, %v), want (%v, %v)", gotRetryAfter, gotOK, tt.wantRetryAfter, tt.wantRetryAfterOK)
			}
		})
	}
}

// TestGenuineTransportErrorIsRetryableEverywhere proves a real connection
// failure (no HTTP response at all, so no status code is ever produced)
// remains retryable under both predicates, and carries no HTTP status or
// Retry-After — it is a transport hiccup, not a provider-classified failure.
func TestGenuineTransportErrorIsRetryableEverywhere(t *testing.T) {
	client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL: "http://127.0.0.1:1", // reserved port: connection refused/unreachable
		Model:   "test-model",
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	streamErr := client.ChatStream(context.Background(), ChatOptions{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(StreamChunk) error { return nil })
	if streamErr == nil {
		t.Fatal("expected a connection failure")
	}
	if !IsRetryableTransport(streamErr) {
		t.Fatalf("genuine transport error must be retryable: %v", streamErr)
	}
	if !IsRetryableProviderError(streamErr) {
		t.Fatalf("genuine transport error must be retryable under IsRetryableProviderError: %v", streamErr)
	}
	if _, ok := ProviderHTTPStatus(streamErr); ok {
		t.Fatalf("a connection failure must not carry an HTTP status: %v", streamErr)
	}
	if _, ok := ProviderRetryAfter(streamErr); ok {
		t.Fatalf("a connection failure must not carry a Retry-After hint: %v", streamErr)
	}
}

// TestCancellationAndDeadlineAreNeverRetryable guards the existing contract
// that deliberate cancellation is not a provider failure at all, so it must
// stay false under the new predicate exactly like the old one.
func TestCancellationAndDeadlineAreNeverRetryable(t *testing.T) {
	if IsRetryableProviderError(context.Canceled) {
		t.Fatal("context.Canceled must not be retryable")
	}
	if IsRetryableProviderError(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded must not be retryable")
	}
	if IsRetryableProviderError(nil) {
		t.Fatal("nil must not be retryable")
	}
}
