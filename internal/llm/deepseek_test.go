package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureDeepSeekRequest runs one ChatStream against a stub and returns the
// decoded request body the client actually put on the wire.
func captureDeepSeekRequest(t *testing.T, dialect string, opts ChatOptions) map[string]any {
	t.Helper()
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	client, err := NewOpenAICompatibleClient(OpenAICompatibleOptions{
		BaseURL:         server.URL,
		Model:           DeepSeekFlashModel,
		APIKey:          "test-key",
		Dialect:         dialect,
		Thinking:        true,
		ReasoningEffort: DeepSeekDefaultEffort,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.ChatStream(context.Background(), opts, func(StreamChunk) error { return nil }); err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if body == nil {
		t.Fatal("stub captured no request body")
		return nil
	}
	return body
}

// DeepSeek toggles chain-of-thought with `thinking`, and defaults it to
// enabled. reasoning_effort only grades depth once thinking is on, so a harness
// that relies on it alone silently pays for reasoning on every turn.
func TestDeepSeekSendsThinkingToggle(t *testing.T) {
	body := captureDeepSeekRequest(t, DialectDeepSeek, ChatOptions{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})

	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("request omitted the thinking toggle: %v", body)
	}
	if thinking["type"] != "enabled" {
		t.Errorf("thinking type = %v, want enabled", thinking["type"])
	}
	if body["reasoning_effort"] != DeepSeekDefaultEffort {
		t.Errorf("reasoning_effort = %v, want %q", body["reasoning_effort"], DeepSeekDefaultEffort)
	}
}

func TestDeepSeekDisableReasoningTurnsThinkingOff(t *testing.T) {
	body := captureDeepSeekRequest(t, DialectDeepSeek, ChatOptions{
		Messages:         []Message{{Role: "user", Content: "hi"}},
		DisableReasoning: true,
	})

	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("request omitted the thinking toggle: %v", body)
	}
	if thinking["type"] != "disabled" {
		t.Errorf("thinking type = %v, want disabled", thinking["type"])
	}
	// "none" is not a value DeepSeek accepts, and sending it instead of the
	// toggle would leave thinking on.
	if effort, present := body["reasoning_effort"]; present && effort == "none" {
		t.Error("sent reasoning_effort=none instead of disabling thinking")
	}
}

// An assistant message that requested tools must carry its own reasoning back,
// or DeepSeek rejects the next agent iteration with HTTP 400.
func TestDeepSeekEchoesReasoningOnToolCallTurns(t *testing.T) {
	body := captureDeepSeekRequest(t, DialectDeepSeek, ChatOptions{
		Messages: []Message{
			{Role: "user", Content: "read the file"},
			{
				Role:             "assistant",
				Content:          "",
				ReasoningContent: "the user wants a file read",
				ToolCalls:        []ToolCall{{ID: "call_1", Name: "read", Arguments: map[string]any{"path": "a.txt"}}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "contents"},
		},
	})

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("unexpected messages payload: %v", body["messages"])
	}
	assistant, ok := messages[1].(map[string]any)
	if !ok {
		t.Fatalf("assistant message is not an object: %v", messages[1])
	}
	if assistant["reasoning_content"] != "the user wants a file read" {
		t.Errorf("reasoning_content = %v, want it echoed back", assistant["reasoning_content"])
	}
}

// The key must be present even when the reasoning text is empty — a resumed
// session can hold a tool-call message whose reasoning was never persisted.
func TestDeepSeekEmitsEmptyReasoningKeyForToolCalls(t *testing.T) {
	body := captureDeepSeekRequest(t, DialectDeepSeek, ChatOptions{
		Messages: []Message{{
			Role:      "assistant",
			ToolCalls: []ToolCall{{ID: "call_1", Name: "read", Arguments: map[string]any{}}},
		}},
	})

	messages := body["messages"].([]any)
	assistant := messages[0].(map[string]any)
	value, present := assistant["reasoning_content"]
	if !present {
		t.Fatal("tool-call message dropped the reasoning_content key")
	}
	if value != "" {
		t.Errorf("reasoning_content = %v, want empty string", value)
	}
}

// A plain answer carries no tool calls, so echoing reasoning would only widen
// how far private chain-of-thought travels for no protocol benefit.
func TestDeepSeekOmitsReasoningOnPlainAssistantTurns(t *testing.T) {
	body := captureDeepSeekRequest(t, DialectDeepSeek, ChatOptions{
		Messages: []Message{{
			Role:             "assistant",
			Content:          "the answer is 4",
			ReasoningContent: "2+2",
		}},
	})

	messages := body["messages"].([]any)
	assistant := messages[0].(map[string]any)
	if _, present := assistant["reasoning_content"]; present {
		t.Error("plain assistant turn echoed reasoning_content")
	}
}

// Hosts that are not DeepSeek must never receive its extensions: an unknown
// field can be a hard 400 on a strict OpenAI-compatible endpoint.
func TestNonDeepSeekDialectSendsNoDeepSeekExtensions(t *testing.T) {
	body := captureDeepSeekRequest(t, "", ChatOptions{
		Messages: []Message{{
			Role:             "assistant",
			ReasoningContent: "private",
			ToolCalls:        []ToolCall{{ID: "call_1", Name: "read", Arguments: map[string]any{}}},
		}},
	})

	if _, present := body["thinking"]; present {
		t.Error("plain OpenAI dialect sent the DeepSeek thinking toggle")
	}
	messages := body["messages"].([]any)
	assistant := messages[0].(map[string]any)
	if _, present := assistant["reasoning_content"]; present {
		t.Error("plain OpenAI dialect echoed reasoning_content")
	}
}

func TestResolveProviderModel(t *testing.T) {
	// An empty model resolves to the provider's cheap default.
	got, err := ResolveProviderModel("deepseek", "")
	if err != nil {
		t.Fatalf("empty model error = %v", err)
	}
	if got != DeepSeekFlashModel {
		t.Errorf("default model = %q, want %q", got, DeepSeekFlashModel)
	}

	// sonar runs many models; a second model from the same provider is valid.
	if got, err := ResolveProviderModel("deepseek", "deepseek-v4-pro"); err != nil || got != "deepseek-v4-pro" {
		t.Errorf("pro model = %q, err = %v; want it accepted", got, err)
	}

	// The catalog is a pinned snapshot. An id it has not seen yet must still
	// run, or the harness breaks the day a provider ships a new model.
	if got, err := ResolveProviderModel("deepseek", "deepseek-v5-future"); err != nil || got != "deepseek-v5-future" {
		t.Errorf("unlisted model = %q, err = %v; want it accepted", got, err)
	}

	// A private endpoint has no catalog entry to default from.
	if got, err := ResolveProviderModel("openai_compatible", "my-model"); err != nil || got != "my-model" {
		t.Errorf("private endpoint model = %q, err = %v", got, err)
	}
}

func TestNewDeepSeekClientRequiresAKey(t *testing.T) {
	_, err := NewDeepSeekClient(DeepSeekOptions{})
	if err == nil {
		t.Fatal("client was built with no API key")
	}
	if !strings.Contains(err.Error(), DeepSeekAPIKeyEnv) {
		t.Errorf("error does not name the key variable: %v", err)
	}
}

func TestNewDeepSeekClientDefaults(t *testing.T) {
	client, err := NewDeepSeekClient(DeepSeekOptions{APIKey: "test-key", Thinking: true})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if client.Model() != DeepSeekFlashModel {
		t.Errorf("model = %q, want %q", client.Model(), DeepSeekFlashModel)
	}
	if client.BaseURL() != DeepSeekBaseURL {
		t.Errorf("base URL = %q, want %q", client.BaseURL(), DeepSeekBaseURL)
	}
}

func TestEstimateDeepSeekCostUSD(t *testing.T) {
	// A cache hit is ~50x cheaper than a miss, which is why prompt-prefix
	// stability dominates cost in a long session.
	miss := EstimateDeepSeekCostUSD(1_000_000, 0, 0)
	hit := EstimateDeepSeekCostUSD(1_000_000, 1_000_000, 0)
	if miss <= hit {
		t.Fatalf("cache miss (%f) must cost more than a hit (%f)", miss, hit)
	}
	if got, want := miss, DeepSeekInputCacheMissUSDPerMTok; got != want {
		t.Errorf("1M miss tokens = %f, want %f", got, want)
	}
	if got, want := EstimateDeepSeekCostUSD(0, 0, 1_000_000), DeepSeekOutputUSDPerMTok; got != want {
		t.Errorf("1M output tokens = %f, want %f", got, want)
	}
	// A cached count larger than the prompt must clamp, not go negative.
	if got := EstimateDeepSeekCostUSD(10, 999, 0); got < 0 {
		t.Errorf("over-reported cache hits produced a negative cost: %f", got)
	}
}
