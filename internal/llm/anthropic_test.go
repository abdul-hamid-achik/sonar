package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// captureAnthropicRequest runs one ChatStream against a stub server and
// returns the decoded request body the client actually put on the wire, plus
// the request headers the server observed.
func captureAnthropicRequest(t *testing.T, opts ChatOptions) (map[string]any, http.Header) {
	t.Helper()
	var body map[string]any
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1}}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	client, err := NewAnthropicClient(AnthropicOptions{
		BaseURL: server.URL,
		Model:   "claude-sonnet-5",
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.ChatStream(context.Background(), opts, func(StreamChunk) error { return nil }); err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if body == nil {
		t.Fatal("stub captured no request body")
	}
	return body, headers
}

// The system prompt is a top-level "system" field, not a message with role
// "system" — the single most load-bearing shape difference from the OpenAI
// dialect.
func TestAnthropicSystemPromptIsTopLevelField(t *testing.T) {
	body, _ := captureAnthropicRequest(t, ChatOptions{
		System:   "You are a careful coding agent.",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})

	if body["system"] != "You are a careful coding agent." {
		t.Fatalf("system = %#v, want top-level system field", body["system"])
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %#v", body["messages"])
	}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("message entry is not an object: %v", raw)
		}
		if message["role"] == "system" {
			t.Fatalf("system prompt leaked into messages array: %v", messages)
		}
	}
}

// A host llm.Message{Role: "system"} appearing mid-history (agent/compact.go
// and agent/agent.go both append durable-recovery-context this way) must also
// fold into the top-level system field, not become a mid-array role:"system"
// message — the base Messages API only accepts "user"/"assistant" there, and
// not every anthropic-wire-family provider is guaranteed to support the newer
// mid-conversation system message capability.
func TestAnthropicFoldsMidHistorySystemMessageIntoTopLevelField(t *testing.T) {
	body, _ := captureAnthropicRequest(t, ChatOptions{
		System: "base",
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "system", Content: "recovered context"},
			{Role: "assistant", Content: "hello"},
		},
	})

	system, _ := body["system"].(string)
	if !strings.Contains(system, "base") || !strings.Contains(system, "recovered context") {
		t.Fatalf("system = %q, want it to contain both parts", system)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want the system-role entry folded out", messages)
	}
}

// Required headers: x-api-key (not Authorization: Bearer) and
// anthropic-version. Also confirms max_tokens is present when the caller
// never set MaxEvalTokens — Anthropic rejects a request with no max_tokens.
func TestAnthropicSendsAuthHeadersAndDefaultsMaxTokens(t *testing.T) {
	body, headers := captureAnthropicRequest(t, ChatOptions{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})

	if got := headers.Get("x-api-key"); got != "test-key" {
		t.Errorf("x-api-key = %q, want test-key", got)
	}
	if got := headers.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty — anthropic authenticates via x-api-key", got)
	}
	if got := headers.Get("anthropic-version"); got != AnthropicAPIVersion {
		t.Errorf("anthropic-version = %q, want %q", got, AnthropicAPIVersion)
	}
	maxTokens, ok := body["max_tokens"].(float64)
	if !ok || maxTokens <= 0 {
		t.Fatalf("max_tokens = %#v, want a positive default", body["max_tokens"])
	}
}

// When the caller does set MaxEvalTokens, it must override the client's
// default rather than being ignored.
func TestAnthropicHonorsRequestMaxEvalTokensOverDefault(t *testing.T) {
	body, _ := captureAnthropicRequest(t, ChatOptions{
		Messages:      []Message{{Role: "user", Content: "hi"}},
		MaxEvalTokens: 4096,
	})
	if got, ok := body["max_tokens"].(float64); !ok || int(got) != 4096 {
		t.Fatalf("max_tokens = %#v, want 4096", body["max_tokens"])
	}
}

// Tool results go back as tool_result content blocks inside a single
// user-role message — Anthropic requires strict user/assistant alternation,
// so several consecutive "tool" role host messages must coalesce into one
// wire message rather than violate that alternation.
func TestAnthropicEncodesToolResultsAsToolResultBlocksInUserMessage(t *testing.T) {
	body, _ := captureAnthropicRequest(t, ChatOptions{
		Messages: []Message{
			{Role: "user", Content: "read two files"},
			{
				Role:    "assistant",
				Content: "",
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "read", Arguments: map[string]any{"path": "a.txt"}},
					{ID: "call_2", Name: "read", Arguments: map[string]any{"path": "b.txt"}},
				},
			},
			{Role: "tool", ToolCallID: "call_1", Content: "contents of a"},
			{Role: "tool", ToolCallID: "call_2", Content: "contents of b"},
		},
	})

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("messages = %#v, want [user, assistant, user(tool_results)]", body["messages"])
	}

	assistant, _ := messages[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("messages[1].role = %v, want assistant", assistant["role"])
	}
	assistantBlocks, _ := assistant["content"].([]any)
	toolUseCount := 0
	for _, raw := range assistantBlocks {
		block, _ := raw.(map[string]any)
		if block["type"] == "tool_use" {
			toolUseCount++
		}
	}
	if toolUseCount != 2 {
		t.Fatalf("assistant tool_use blocks = %d, want 2: %v", toolUseCount, assistantBlocks)
	}

	toolResults, ok := messages[2].(map[string]any)
	if !ok || toolResults["role"] != "user" {
		t.Fatalf("messages[2] = %#v, want a single user-role message carrying both tool results", messages[2])
	}
	blocks, ok := toolResults["content"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("tool_result blocks = %#v, want 2 coalesced into one message", toolResults["content"])
	}
	for i, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			t.Fatalf("block %d = %#v, want type tool_result", i, raw)
		}
		if _, present := block["content"]; !present {
			t.Fatalf("block %d dropped the content key", i)
		}
	}
	if blocks[0].(map[string]any)["tool_use_id"] != "call_1" || blocks[1].(map[string]any)["tool_use_id"] != "call_2" {
		t.Fatalf("tool_use_id mismatch: %v", blocks)
	}
}

// tool_use content blocks in the response stream decode into llm.ToolCall,
// with input_json_delta fragments reassembled into structured Arguments.
func TestAnthropicDecodesToolUseIntoToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"pa"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"th\":\"a.go\"}"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}`,
			`{"type":"message_stop"}`,
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte("data: " + frame + "\n\n"))
		}
	}))
	defer server.Close()

	client, err := NewAnthropicClient(AnthropicOptions{BaseURL: server.URL, Model: "claude-sonnet-5", APIKey: "k"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	var calls []ToolCall
	var finish string
	var evalCount, promptEvalCount int
	err = client.ChatStream(context.Background(), ChatOptions{}, func(chunk StreamChunk) error {
		if len(chunk.ToolCalls) > 0 {
			calls = chunk.ToolCalls
		}
		if chunk.Done {
			finish = chunk.FinishReason
			evalCount = chunk.EvalCount
			promptEvalCount = chunk.PromptEvalCount
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "read_file" || calls[0].ID != "toolu_1" {
		t.Fatalf("tool calls = %#v", calls)
	}
	if path, _ := calls[0].Arguments["path"].(string); path != "a.go" {
		t.Fatalf("arguments = %#v", calls[0].Arguments)
	}
	if finish != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls (mapped from stop_reason=tool_use)", finish)
	}
	if evalCount != 8 || promptEvalCount != 10 {
		t.Errorf("usage = eval:%d prompt:%d, want eval:8 prompt:10", evalCount, promptEvalCount)
	}
}

// The terminal chunk must carry both the mapped finish reason and usage —
// this is the one place a truncated generation ("max_tokens" -> "length") is
// distinguishable from a normal completion downstream.
func TestAnthropicTerminalChunkMapsMaxTokensStopReasonToLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":5}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":16}}`,
			`{"type":"message_stop"}`,
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte("data: " + frame + "\n\n"))
		}
	}))
	defer server.Close()

	client, err := NewAnthropicClient(AnthropicOptions{BaseURL: server.URL, Model: "claude-sonnet-5", APIKey: "k"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	var finish string
	var doneSeen bool
	err = client.ChatStream(context.Background(), ChatOptions{}, func(chunk StreamChunk) error {
		if chunk.Done {
			doneSeen = true
			finish = chunk.FinishReason
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if !doneSeen {
		t.Fatal("stream never produced a terminal chunk")
	}
	if finish != "length" {
		t.Errorf("finish reason = %q, want length (mapped from stop_reason=max_tokens)", finish)
	}
}

// An HTTP error must surface the provider's own message without ever
// echoing the base URL — sensitive base URLs are rejected before any
// request is ever built, and the error text is host-authored, never url.Error
// text that could carry the endpoint.
func TestAnthropicRejectsSensitiveBaseURLWithoutEchoingIt(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
	}{
		{name: "userinfo", baseURL: "https://user:super-secret@example.com"},
		{name: "query", baseURL: "https://example.com?token=super-secret"},
		{name: "fragment", baseURL: "https://example.com#super-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewAnthropicClient(AnthropicOptions{
				BaseURL: test.baseURL,
				Model:   "claude-sonnet-5",
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

// The HTTP error path itself: the provider's status/message reach the caller
// (via the shared openAIHTTPError type, so ProviderHTTPStatus/ProviderStatusHint
// keep working across both dialects), and the error text is built solely from
// the response status and body — never from the client's configured base URL.
func TestAnthropicHTTPErrorSurfacesProviderMessageWithoutBaseURL(t *testing.T) {
	const providerMessage = "overloaded_error: the server is temporarily overloaded"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "overloaded_error", "message": providerMessage},
		})
	}))
	defer server.Close()

	client, err := NewAnthropicClient(AnthropicOptions{
		BaseURL: server.URL,
		Model:   "claude-sonnet-5",
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	streamErr := client.ChatStream(context.Background(), ChatOptions{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(StreamChunk) error { return nil })
	if streamErr == nil || !IsRemoteInferenceError(streamErr) {
		t.Fatalf("ChatStream error = %v, want a remote-provenance error", streamErr)
	}
	if !strings.Contains(streamErr.Error(), providerMessage) {
		t.Fatalf("provider diagnostic lost: %v", streamErr)
	}
	if strings.Contains(streamErr.Error(), server.URL) {
		t.Fatalf("error leaked the base URL: %v", streamErr)
	}
	if status, ok := ProviderHTTPStatus(streamErr); !ok || status != http.StatusServiceUnavailable {
		t.Fatalf("ProviderHTTPStatus = (%d, %v), want (503, true)", status, ok)
	}
}

// NewAnthropicProviderClient requires a resolved API key, mirroring
// NewDeepSeekClient's fail-fast behavior for the pinned provider.
func TestNewAnthropicProviderClientRequiresAKey(t *testing.T) {
	_, err := NewAnthropicProviderClient(AnthropicProviderID, "", "claude-sonnet-5", "")
	if err == nil {
		t.Fatal("client was built with no API key")
	}
	if !strings.Contains(err.Error(), AnthropicAPIKeyEnv) {
		t.Errorf("error does not name the key variable: %v", err)
	}
}

// An empty base URL falls back to each provider's real, doc-verified
// endpoint rather than guessing.
func TestNewAnthropicProviderClientFallsBackToDocVerifiedEndpoints(t *testing.T) {
	tests := []struct {
		providerID string
		wantURL    string
	}{
		{AnthropicProviderID, AnthropicBaseURL},
		{KimiCodingProviderID, KimiCodingBaseURL},
		{MiniMaxProviderID, MiniMaxBaseURL},
		{MiniMaxChinaProviderID, MiniMaxChinaBaseURL},
	}
	for _, test := range tests {
		t.Run(test.providerID, func(t *testing.T) {
			client, err := NewAnthropicProviderClient(test.providerID, "", "claude-sonnet-5", "test-key")
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			if got := client.BaseURL(); got != test.wantURL {
				t.Errorf("base url = %q, want %q", got, test.wantURL)
			}
		})
	}
}

// Catwalk's "anthropic" catalog entry names its endpoint as the literal,
// unresolved template "$ANTHROPIC_API_ENDPOINT" when no override env var is
// configured (see internal/catalog/providers.json). sonar's config layer does
// not substitute that template, so it can reach this constructor verbatim —
// it must be treated the same as an empty base URL, not passed through.
func TestNewAnthropicProviderClientTreatsUnresolvedCatalogTemplateAsEmpty(t *testing.T) {
	client, err := NewAnthropicProviderClient(AnthropicProviderID, "$ANTHROPIC_API_ENDPOINT", "claude-sonnet-5", "test-key")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if got := client.BaseURL(); got != AnthropicBaseURL {
		t.Errorf("base url = %q, want the doc-verified fallback %q", got, AnthropicBaseURL)
	}
}

// A caller-supplied base URL is never overridden.
func TestNewAnthropicProviderClientHonorsExplicitBaseURL(t *testing.T) {
	client, err := NewAnthropicProviderClient(AnthropicProviderID, "https://proxy.internal.example", "claude-sonnet-5", "test-key")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if got := client.BaseURL(); got != "https://proxy.internal.example" {
		t.Errorf("base url = %q, want the explicit override preserved", got)
	}
}

// NewProviderClient must select the Anthropic dialect for all four
// anthropic-wire-family identities, by identity rather than by reading the
// catalog's wire-type string.
func TestNewProviderClientSelectsAnthropicDialectByIdentity(t *testing.T) {
	for _, providerType := range []string{AnthropicProviderID, KimiCodingProviderID, MiniMaxProviderID, MiniMaxChinaProviderID} {
		t.Run(providerType, func(t *testing.T) {
			client, err := NewProviderClient(providerType, "", "", "test-key")
			if err != nil {
				t.Fatalf("new provider client: %v", err)
			}
			if _, ok := client.(*AnthropicClient); !ok {
				t.Fatalf("provider %q selected %T, want *AnthropicClient", providerType, client)
			}
		})
	}
}

// The DeepSeek and generic OpenAI-compatible branches must still return a
// RemoteChatClient (interface widening did not silently change which
// concrete dialect a non-anthropic provider selects).
func TestNewProviderClientStillSelectsNonAnthropicDialects(t *testing.T) {
	deepseekClient, err := NewProviderClient(config.ProviderTypeDeepSeek, "", "", "test-key")
	if err != nil {
		t.Fatalf("deepseek: %v", err)
	}
	if _, ok := deepseekClient.(*OpenAICompatibleClient); !ok {
		t.Fatalf("deepseek selected %T, want *OpenAICompatibleClient", deepseekClient)
	}

	xaiClient, err := NewProviderClient(config.ProviderTypeXAI, "https://api.x.ai/v1", "grok-4.5", "test-key")
	if err != nil {
		t.Fatalf("xai: %v", err)
	}
	if _, ok := xaiClient.(*OpenAICompatibleClient); !ok {
		t.Fatalf("xai selected %T, want *OpenAICompatibleClient", xaiClient)
	}
}
