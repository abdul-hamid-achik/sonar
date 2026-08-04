package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOllamaChatStreamCarriesTimingAndFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":"partial"},"done":false}`)
		_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":" answer"},"done":true,"done_reason":"length","eval_count":42,"prompt_eval_count":11,"total_duration":3810000000,"load_duration":50000000,"prompt_eval_duration":220000000,"eval_duration":3180000000}`)
	}))
	defer server.Close()

	client, err := NewOllamaClient(server.URL, "fixture-model", 8192)
	if err != nil {
		t.Fatal(err)
	}
	var terminal StreamChunk
	err = client.ChatStream(context.Background(), ChatOptions{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Done {
			terminal = chunk
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if terminal.FinishReason != "length" {
		t.Fatalf("FinishReason = %q, want the truncation reason to survive to the agent boundary", terminal.FinishReason)
	}
	if terminal.EvalCount != 42 || terminal.PromptEvalCount != 11 {
		t.Fatalf("usage = %d/%d", terminal.EvalCount, terminal.PromptEvalCount)
	}
	if terminal.Timing == nil {
		t.Fatal("terminal chunk should carry provider timings")
	}
	if terminal.Timing.TotalDuration != 3810*time.Millisecond ||
		terminal.Timing.LoadDuration != 50*time.Millisecond ||
		terminal.Timing.PromptEvalDuration != 220*time.Millisecond ||
		terminal.Timing.EvalDuration != 3180*time.Millisecond {
		t.Fatalf("timing = %+v", terminal.Timing)
	}
	if terminal.Timing.TimeToFirstToken <= 0 {
		t.Fatalf("client-measured TTFT should be positive, got %s", terminal.Timing.TimeToFirstToken)
	}
}

func TestOllamaChatStreamOmitsTimingWhenUnreported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A terminal receipt without content, timings, or reason: Timing must
		// stay nil so consumers cannot mistake absence for zero-cost.
		_, _ = fmt.Fprintln(w, `{"message":{"role":"assistant","content":""},"done":true,"eval_count":1,"prompt_eval_count":1}`)
	}))
	defer server.Close()

	client, err := NewOllamaClient(server.URL, "fixture-model", 8192)
	if err != nil {
		t.Fatal(err)
	}
	var terminal StreamChunk
	if err := client.ChatStream(context.Background(), ChatOptions{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Done {
			terminal = chunk
		}
		return nil
	}); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if terminal.Timing != nil {
		t.Fatalf("timing should be nil when the provider reported none, got %+v", terminal.Timing)
	}
	if terminal.FinishReason != "" {
		t.Fatalf("finish reason should stay empty when unreported, got %q", terminal.FinishReason)
	}
}
