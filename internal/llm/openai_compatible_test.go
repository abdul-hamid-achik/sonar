package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAICompatibleRejectsSensitiveBaseURLWithoutEchoingIt(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
	}{
		{name: "userinfo", baseURL: "https://user:super-secret@example.com/v1"},
		{name: "query", baseURL: "https://example.com/v1?token=super-secret"},
		{name: "empty query", baseURL: "https://example.com/v1?"},
		{name: "fragment", baseURL: "https://example.com/v1#super-secret"},
		{name: "empty fragment", baseURL: "https://example.com/v1#"},
		{name: "invalid", baseURL: "://super-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
				BaseURL: test.baseURL,
				Model:   "test-model",
				APIKey:  "test-key",
			})
			if err == nil {
				t.Fatal("sensitive base URL was accepted")
			}
			if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), test.baseURL) {
				t.Fatalf("base URL leaked through client error: %v", err)
			}
		})
	}
}

func TestOpenAICompatibleRequiresTLSForRemoteHosts(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
	}{
		{name: "public hostname", baseURL: "http://api.example.test/v1"},
		{name: "private network address", baseURL: "http://192.168.1.10:8000/v1"},
		{name: "unsupported scheme", baseURL: "ftp://api.example.test/v1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
				BaseURL: test.baseURL,
				Model:   "test-model",
				APIKey:  "test-key",
			})
			if err == nil {
				t.Fatal("cleartext or unsupported remote base URL was accepted")
			}
			if strings.Contains(err.Error(), test.baseURL) {
				t.Fatalf("base URL leaked through validation error: %v", err)
			}
		})
	}

	for _, localURL := range []string{
		"http://localhost:8000/v1",
		"http://127.0.0.1:8000/v1",
		"http://[::1]:8000/v1",
	} {
		if _, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
			BaseURL: localURL,
			Model:   "local-model",
		}); err != nil {
			t.Fatalf("local cleartext base URL %q rejected: %v", localURL, err)
		}
	}
}

func TestOpenAICompatibleInferenceErrorsCarryRemoteProvenance(t *testing.T) {
	const providerSecret = "REMOTE_PROVIDER_SECRET /Users/remote/private.key"
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "http error body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"message": providerSecret},
				})
			},
		},
		{
			name: "sse error event",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				payload, _ := json.Marshal(map[string]any{
					"error": map[string]any{"message": providerSecret},
				})
				frame := append([]byte("data: "), payload...)
				frame = append(frame, '\n', '\n')
				_, _ = w.Write(frame)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()

			client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
				BaseURL: server.URL,
				Model:   "grok-test",
				APIKey:  "test-key",
			})
			if err != nil {
				t.Fatal(err)
			}
			err = client.ChatStream(context.Background(), ChatOptions{}, func(StreamChunk) error {
				return nil
			})
			if err == nil || !IsRemoteInferenceError(err) {
				t.Fatalf("ChatStream error = %v, want exact remote provenance", err)
			}
			if !strings.Contains(err.Error(), providerSecret) {
				t.Fatalf("transient llm error lost provider diagnostics: %v", err)
			}
		})
	}

	client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL: "http://127.0.0.1:1",
		Model:   "grok-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.ChatStream(context.Background(), ChatOptions{}, nil)
	if err == nil || IsRemoteInferenceError(err) {
		t.Fatalf("pre-dispatch callback validation acquired remote provenance: %v", err)
	}
}

func TestOpenAICompatibleChatStreamText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL: server.URL + "/v1",
		Model:   "grok-4.5",
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	if err := client.ChatStream(context.Background(), ChatOptions{}, func(chunk StreamChunk) error {
		text.WriteString(chunk.Text)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if text.String() != "hello" {
		t.Fatalf("text = %q, want hello", text.String())
	}
}

func TestOpenAICompatibleChatStreamToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]},"finish_reason":"tool_calls"}]}`,
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte("data: " + frame + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL: server.URL,
		Model:   "grok-4.5",
		APIKey:  "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls []ToolCall
	if err := client.ChatStream(context.Background(), ChatOptions{}, func(chunk StreamChunk) error {
		if len(chunk.ToolCalls) > 0 {
			calls = chunk.ToolCalls
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("tool calls = %#v", calls)
	}
	if path, _ := calls[0].Arguments["path"].(string); path != "a.go" {
		t.Fatalf("arguments = %#v", calls[0].Arguments)
	}
}

func TestOpenAICompatibleEncodesToolsAndSystem(t *testing.T) {
	var saw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&saw); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL: server.URL,
		Model:   "m",
		APIKey:  "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.ChatStream(context.Background(), ChatOptions{
		System: "sys",
		Messages: []Message{{
			Role:    "user",
			Content: "hi",
		}},
		Tools: []ToolDef{{
			Name:        "grep",
			Description: "search",
			Parameters:  map[string]any{"type": "object"},
		}},
	}, func(StreamChunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if saw["model"] != "m" {
		t.Fatalf("model = %#v", saw["model"])
	}
	messages, _ := saw["messages"].([]any)
	if len(messages) < 2 {
		t.Fatalf("messages = %#v", messages)
	}
	tools, _ := saw["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", tools)
	}
}

func TestOpenAICompatibleMissingKeyStillBuilds(t *testing.T) {
	// Local open servers may not need a key.
	client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL: "http://127.0.0.1:8000/v1",
		Model:   "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.Model() != "local" {
		t.Fatalf("model = %q", client.Model())
	}
}

func TestOpenAICompatibleChatStreamIdleWatchdogFailsHungStream(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release // then go silent without [DONE]
	}))
	defer server.Close()
	defer close(release)

	previous := streamIdleTimeout
	streamIdleTimeout = 100 * time.Millisecond
	defer func() { streamIdleTimeout = previous }()

	client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL: server.URL, Model: "m", APIKey: "k",
	})
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
	if !IsRemoteInferenceError(streamErr) {
		t.Fatalf("idle error from accepted stream should carry remote provenance: %v", streamErr)
	}
}

func TestOpenAICompatibleChatStreamIdleWatchdogAllowsSlowButLiveStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk%d\"}}]}\n\n", i)
			flusher.Flush()
			time.Sleep(60 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	previous := streamIdleTimeout
	streamIdleTimeout = 150 * time.Millisecond
	defer func() { streamIdleTimeout = previous }()

	client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL: server.URL, Model: "m", APIKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	var chunks int
	streamErr := client.ChatStream(context.Background(), ChatOptions{}, func(chunk StreamChunk) error {
		if chunk.Text != "" {
			chunks++
		}
		return nil
	})
	if streamErr != nil {
		t.Fatalf("slow-but-live stream error = %v, want nil", streamErr)
	}
	if chunks != 3 {
		t.Fatalf("chunks = %d, want 3", chunks)
	}
}
