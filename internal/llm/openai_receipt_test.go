package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Two facts a turn receipt depends on, neither of which was asserted anywhere
// and both of which were broken for every remote provider.
//
// sonar's budgets are not advisory: a turn that ends without a
// trustworthy usage receipt charges its reservation fail-closed. The OpenAI
// streaming contract omits usage unless include_usage is set, so a
// spec-strict provider could exhaust a goal's budget on its first turn. And
// ProviderTiming describes itself as carrying "the client-measured time to
// first streamed token", which this dialect never measured — so the --json
// receipt's timing block was empty unless the native Ollama client produced it.
func TestStreamRequestsUsageAndReportsClientTiming(t *testing.T) {
	const firstTokenDelay = 20 * time.Millisecond

	var asked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		asked = request.StreamOptions != nil && request.StreamOptions.IncludeUsage

		w.Header().Set("Content-Type", "text/event-stream")
		// Flush the headers before pausing. The client starts its clock when
		// headers arrive, so sleeping first would move the clock too and leave
		// time-to-first-token at zero on loopback.
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(firstTokenDelay)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL: server.URL, Model: "test-model", APIKey: "test-key",
	})
	if err != nil {
		t.Fatal(err)
	}

	var final StreamChunk
	err = client.ChatStream(context.Background(),
		ChatOptions{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(chunk StreamChunk) error {
			if chunk.Done {
				final = chunk
			}
			return nil
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if !asked {
		t.Error("request omitted stream_options.include_usage, so a spec-strict provider would report no usage at all")
	}
	if final.EvalCount != 7 || final.PromptEvalCount != 11 {
		t.Errorf("usage = %d/%d, want 7/11", final.EvalCount, final.PromptEvalCount)
	}
	if final.Timing == nil {
		t.Fatal("terminal chunk carries no timing; the --json receipt's timing block would be empty")
	}
	// A floor, not the exact pause. The client starts its clock when response
	// headers arrive, which is marginally after the server flushed them, so the
	// measurement lands just under the server's sleep — CI caught this at
	// 19.22ms against 20ms. Half the pause still separates a real measurement
	// from an unset field, which is the property under test.
	if final.Timing.TimeToFirstToken < firstTokenDelay/2 {
		t.Errorf("time to first token = %v, want a real measurement near the server's %v pause",
			final.Timing.TimeToFirstToken, firstTokenDelay)
	}
	if final.Timing.TotalDuration < final.Timing.TimeToFirstToken {
		t.Errorf("total %v is shorter than time to first token %v",
			final.Timing.TotalDuration, final.Timing.TimeToFirstToken)
	}
	// Per-phase durations need provider cooperation and stay zero, which the
	// contract defines as "not reported" rather than "instant".
	if final.Timing.LoadDuration != 0 || final.Timing.EvalDuration != 0 {
		t.Errorf("client-measured timing invented provider-only durations: %+v", final.Timing)
	}
}
