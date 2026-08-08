package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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

	if anthropicSystemText(t, body) != "You are a careful coding agent." {
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

	system := anthropicSystemText(t, body)
	if !strings.Contains(system, "base") || !strings.Contains(system, "recovered context") {
		t.Fatalf("system = %q, want it to contain both parts", system)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want the system-role entry folded out", messages)
	}
}

// anthropicSystemText reads the top-level system field in its block-array
// form — the shape prompt caching requires, since only a block can carry
// cache_control.
func anthropicSystemText(t *testing.T, body map[string]any) string {
	t.Helper()
	blocks, ok := body["system"].([]any)
	if !ok {
		t.Fatalf("system = %#v, want a block array", body["system"])
	}
	var parts []string
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "text" {
			t.Fatalf("system block = %#v, want text blocks", raw)
		}
		text, _ := block["text"].(string)
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n")
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

// The outbound half of tool calling: what the host's []ToolDef becomes in the
// request body. Four catalog providers reach the model through this one
// function, and nothing exercised it — every existing tool test asserted the
// inbound direction (tool_use blocks decoding into llm.ToolCall) or the
// tool_result round-trip, never the schema that makes a model able to call a
// tool in the first place.
//
// Anthropic names the field "input_schema", not OpenAI's
// "function.parameters", and puts the tool at the top level of the request
// rather than under a {"type":"function"} wrapper. A regression here does not
// look like a crash: the request succeeds and the model simply never calls a
// tool.
func TestAnthropicConvertsToolsOntoTheWire(t *testing.T) {
	readFileSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "workspace-relative path"},
		},
		"required":             []any{"path"},
		"additionalProperties": false,
	}

	tests := []struct {
		name  string
		tools []ToolDef
		// want is the exact decoded "tools" array. A nil want means the key
		// must be absent from the request entirely.
		want []map[string]any
	}{
		{
			name:  "no tools omits the key",
			tools: nil,
		},
		{
			// omitempty on a nil slice and on an allocated empty slice must
			// agree: an agent turn with tools disabled sends no tools array,
			// and Anthropic rejects "tools": [].
			name:  "an allocated but empty tool set also omits the key",
			tools: []ToolDef{},
		},
		{
			name: "a full definition maps name, description, and schema",
			tools: []ToolDef{{
				Name:        "read_file",
				Description: "Read a file from the workspace.",
				Parameters:  readFileSchema,
			}},
			want: []map[string]any{{
				"name":         "read_file",
				"description":  "Read a file from the workspace.",
				"input_schema": readFileSchema,
			}},
		},
		{
			// A tool with no schema at all still needs a schema on the wire:
			// input_schema has no omitempty, and Anthropic requires an object
			// schema. This branch, not the caller, supplies it.
			name:  "a nil schema becomes an empty object schema",
			tools: []ToolDef{{Name: "list_workspace", Description: "List the workspace."}},
			want: []map[string]any{{
				"name":         "list_workspace",
				"description":  "List the workspace.",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
			}},
		},
		{
			// Empty and nil are different inputs and must stay different
			// outputs. Only a nil schema is missing; an allocated empty map is
			// a schema the caller authored, and the dialect forwards a host
			// schema verbatim rather than rewriting it. convertOpenAITools
			// splits on exactly the same condition, so an MCP server's schema
			// reaches both dialects identically.
			name:  "an empty but non-nil schema is forwarded verbatim",
			tools: []ToolDef{{Name: "now", Parameters: map[string]any{}}},
			want: []map[string]any{{
				"name":         "now",
				"input_schema": map[string]any{},
			}},
		},
		{
			name:  "a missing description is omitted rather than sent empty",
			tools: []ToolDef{{Name: "now", Parameters: map[string]any{"type": "object"}}},
			want: []map[string]any{{
				"name":         "now",
				"input_schema": map[string]any{"type": "object"},
			}},
		},
		{
			// Tool order is the order the host offered them in; the model's
			// tool choice and the host's ordered dispatch both read it.
			name: "several tools keep their order",
			tools: []ToolDef{
				{Name: "first", Description: "a", Parameters: map[string]any{"type": "object"}},
				{Name: "second", Description: "b", Parameters: map[string]any{"type": "object"}},
				{Name: "third", Description: "c", Parameters: map[string]any{"type": "object"}},
			},
			want: []map[string]any{
				{"name": "first", "description": "a", "input_schema": map[string]any{"type": "object"}},
				{"name": "second", "description": "b", "input_schema": map[string]any{"type": "object"}},
				{"name": "third", "description": "c", "input_schema": map[string]any{"type": "object"}},
			},
		},
		{
			// DisplayName and Behavior are host-only MCP presentation metadata
			// (llm.ToolDef marks both `json:"-"`). A provider must never see
			// them: untrusted server annotations have no business shaping a
			// model's view of a tool.
			name: "host-only MCP presentation metadata never reaches the provider",
			tools: []ToolDef{{
				Name:        "mcp_search",
				Description: "Search.",
				Parameters:  map[string]any{"type": "object"},
				DisplayName: "Search (Acme)",
				Behavior:    ToolBehavior{Declared: true, ReadOnly: true, OpenWorld: true},
			}},
			want: []map[string]any{{
				"name":         "mcp_search",
				"description":  "Search.",
				"input_schema": map[string]any{"type": "object"},
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, _ := captureAnthropicRequest(t, ChatOptions{
				Messages: []Message{{Role: "user", Content: "hi"}},
				Tools:    test.tools,
			})

			raw, present := body["tools"]
			if test.want == nil {
				if present {
					t.Fatalf("tools = %#v, want the key absent", raw)
				}
				return
			}
			if !present {
				t.Fatalf("request carried no tools array: %#v", body)
			}
			got, ok := raw.([]any)
			if !ok {
				t.Fatalf("tools = %#v, want an array", raw)
			}
			if len(got) != len(test.want) {
				t.Fatalf("tools = %#v, want %d entries", got, len(test.want))
			}
			for i, wantTool := range test.want {
				gotTool, ok := got[i].(map[string]any)
				if !ok {
					t.Fatalf("tools[%d] = %#v, want an object", i, got[i])
				}
				// The last tool carries the prompt-cache breakpoint; every
				// other tool must not. Strip it before the shape comparison
				// so this table keeps pinning the tool conversion itself.
				if i == len(test.want)-1 {
					if !reflect.DeepEqual(gotTool["cache_control"], map[string]any{"type": "ephemeral"}) {
						t.Errorf("tools[%d] (last) missing cache breakpoint: %#v", i, gotTool)
					}
					delete(gotTool, "cache_control")
				} else if _, stray := gotTool["cache_control"]; stray {
					t.Errorf("tools[%d] carries a stray cache breakpoint: %#v", i, gotTool)
				}
				if !reflect.DeepEqual(gotTool, normalizeJSON(t, wantTool)) {
					t.Errorf("tools[%d] = %#v, want %#v", i, gotTool, wantTool)
				}
			}
		})
	}
}

// normalizeJSON round-trips a wanted value through JSON so it compares equal to
// a decoded request body (numbers become float64, []any stays []any).
func normalizeJSON(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode expectation: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decode expectation: %v", err)
	}
	return out
}

// anthropicPingStub answers the two routes PingContext may touch and records
// the order it touched them in.
type anthropicPingStub struct {
	modelsStatus    int
	messagesStatus  int
	routes          []string
	messagesPayload map[string]any
	versionHeaders  []string
}

func (s *anthropicPingStub) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		s.routes = append(s.routes, r.Method+" "+r.URL.Path)
		s.versionHeaders = append(s.versionHeaders, r.Header.Get("anthropic-version"))
		status := s.modelsStatus
		if r.URL.Path == "/v1/messages" {
			status = s.messagesStatus
			if err := json.NewDecoder(r.Body).Decode(&s.messagesPayload); err != nil {
				t.Errorf("decode ping payload: %v", err)
			}
		}
		if status == 0 {
			status = http.StatusOK
		}
		if status < 200 || status >= 300 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "authentication_error", "message": "invalid x-api-key"},
			})
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}
}

// PingContext prefers GET /v1/models and falls back to a minimal Messages
// request, so an Anthropic-wire proxy that serves no model list (Kimi Coding,
// MiniMax) still verifies as reachable instead of reading as misconfigured.
func TestAnthropicPingContextPrefersModelsThenFallsBack(t *testing.T) {
	tests := []struct {
		name           string
		modelsStatus   int
		messagesStatus int
		wantRoutes     []string
		wantErr        bool
	}{
		{
			name:         "a models list settles the ping on its own",
			modelsStatus: http.StatusOK,
			wantRoutes:   []string{"GET /v1/models"},
		},
		{
			name:           "a proxy without a models route falls back to messages",
			modelsStatus:   http.StatusNotFound,
			messagesStatus: http.StatusOK,
			wantRoutes:     []string{"GET /v1/models", "POST /v1/messages"},
		},
		{
			// The fallback is unconditional on the models route failing, not
			// conditional on a 404: a proxy may answer anything at all there.
			name:           "a server error on the models route still falls back",
			modelsStatus:   http.StatusInternalServerError,
			messagesStatus: http.StatusOK,
			wantRoutes:     []string{"GET /v1/models", "POST /v1/messages"},
		},
		{
			name:           "both routes failing is a ping failure",
			modelsStatus:   http.StatusUnauthorized,
			messagesStatus: http.StatusUnauthorized,
			wantRoutes:     []string{"GET /v1/models", "POST /v1/messages"},
			wantErr:        true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &anthropicPingStub{modelsStatus: test.modelsStatus, messagesStatus: test.messagesStatus}
			server := httptest.NewServer(stub.handler(t))
			defer server.Close()

			client, err := NewAnthropicClient(AnthropicOptions{
				BaseURL: server.URL,
				Model:   "claude-sonnet-5",
				APIKey:  "test-key",
			})
			if err != nil {
				t.Fatalf("new client: %v", err)
			}

			pingErr := client.PingContext(context.Background())
			if test.wantErr {
				if pingErr == nil {
					t.Fatal("ping succeeded against a provider that rejected both routes")
				}
				// The failure must name the model and carry the provider's own
				// diagnostic; "ping failed" alone is not actionable.
				if !strings.Contains(pingErr.Error(), "claude-sonnet-5") {
					t.Errorf("error does not name the model: %v", pingErr)
				}
				if !strings.Contains(pingErr.Error(), "invalid x-api-key") {
					t.Errorf("provider diagnostic lost: %v", pingErr)
				}
				if strings.Contains(pingErr.Error(), server.URL) {
					t.Errorf("error leaked the base URL: %v", pingErr)
				}
				if status, ok := ProviderHTTPStatus(pingErr); !ok || status != http.StatusUnauthorized {
					t.Errorf("ProviderHTTPStatus = (%d, %v), want (401, true)", status, ok)
				}
			} else if pingErr != nil {
				t.Fatalf("ping: %v", pingErr)
			}

			if !reflect.DeepEqual(stub.routes, test.wantRoutes) {
				t.Fatalf("routes = %v, want %v", stub.routes, test.wantRoutes)
			}
			for i, version := range stub.versionHeaders {
				if version != AnthropicAPIVersion {
					t.Errorf("request %d anthropic-version = %q, want %q", i, version, AnthropicAPIVersion)
				}
			}
		})
	}
}

// The fallback must stay the cheapest request that still proves the model is
// usable: one token, one message, no streaming. It is issued on every provider
// switch and at startup, and the caller is billed for it.
func TestAnthropicPingChatFallbackSendsMinimalRequest(t *testing.T) {
	stub := &anthropicPingStub{modelsStatus: http.StatusNotFound, messagesStatus: http.StatusOK}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()

	client, err := NewAnthropicClient(AnthropicOptions{
		BaseURL:   server.URL,
		Model:     "claude-sonnet-5",
		APIKey:    "test-key",
		MaxTokens: 64000,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	payload := stub.messagesPayload
	if payload == nil {
		t.Fatal("the fallback sent no messages payload")
	}
	if payload["model"] != "claude-sonnet-5" {
		t.Errorf("model = %v, want the client's model", payload["model"])
	}
	// The client's configured max_tokens must not be reused here: a ping that
	// inherited a 64000-token budget would bill like a real turn.
	if maxTokens, ok := payload["max_tokens"].(float64); !ok || maxTokens != 1 {
		t.Errorf("max_tokens = %#v, want 1", payload["max_tokens"])
	}
	if stream, present := payload["stream"]; present && stream != false {
		t.Errorf("stream = %v, want the ping to be non-streaming", stream)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v, want exactly one", payload["messages"])
	}
	message, _ := messages[0].(map[string]any)
	if message["role"] != "user" {
		t.Errorf("role = %v, want user", message["role"])
	}
	blocks, ok := message["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("content = %#v, want one block", message["content"])
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "ping" {
		t.Errorf("content block = %#v, want a single text block", block)
	}
}

// Ping is the context-free wrapper the provider-status surfaces call; it must
// reach the same routes as PingContext rather than being a separate code path.
func TestAnthropicPingWrapsPingContext(t *testing.T) {
	stub := &anthropicPingStub{modelsStatus: http.StatusOK}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()

	client, err := NewAnthropicClient(AnthropicOptions{
		BaseURL: server.URL,
		Model:   "claude-sonnet-5",
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !reflect.DeepEqual(stub.routes, []string{"GET /v1/models"}) {
		t.Fatalf("routes = %v, want the same models-first order as PingContext", stub.routes)
	}
}

// A cancelled context must abort the ping rather than fall through to the
// messages fallback and issue a second doomed request.
func TestAnthropicPingContextHonorsCancellation(t *testing.T) {
	stub := &anthropicPingStub{modelsStatus: http.StatusOK}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()

	client, err := NewAnthropicClient(AnthropicOptions{
		BaseURL: server.URL,
		Model:   "claude-sonnet-5",
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.PingContext(ctx); err == nil {
		t.Fatal("a cancelled ping reported success")
	}
	if len(stub.routes) != 0 {
		t.Fatalf("a cancelled ping still reached the provider: %v", stub.routes)
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

// Three cache breakpoints, in serialization order: the last tool, the system
// block, and the final content block of the final message. The moving third
// one is what makes turn N+1 reuse everything turn N sent; the first two pin
// the prefixes that never change inside a session. Exactly three — the API
// allows four, and one stays in hand.
func TestAnthropicPlacesThreeCacheBreakpoints(t *testing.T) {
	body, _ := captureAnthropicRequest(t, ChatOptions{
		System: "base prompt",
		Tools: []ToolDef{
			{Name: "first"},
			{Name: "second"},
		},
		Messages: []Message{
			{Role: "user", Content: "hola"},
			{Role: "assistant", Content: "hola!"},
			{Role: "user", Content: "seguime contando"},
		},
	})

	marked := func(block map[string]any) bool {
		control, ok := block["cache_control"].(map[string]any)
		return ok && control["type"] == "ephemeral"
	}

	tools := body["tools"].([]any)
	if marked(tools[0].(map[string]any)) || !marked(tools[1].(map[string]any)) {
		t.Fatalf("tool breakpoints misplaced: %#v", tools)
	}
	system := body["system"].([]any)
	if !marked(system[len(system)-1].(map[string]any)) {
		t.Fatalf("system block missing its breakpoint: %#v", system)
	}
	messages := body["messages"].([]any)
	for index, raw := range messages {
		content := raw.(map[string]any)["content"].([]any)
		last := index == len(messages)-1
		for b, rawBlock := range content {
			block := rawBlock.(map[string]any)
			if last && b == len(content)-1 {
				if !marked(block) {
					t.Fatalf("final message block missing its breakpoint: %#v", block)
				}
			} else if marked(block) {
				t.Fatalf("stray breakpoint on message %d block %d: %#v", index, b, block)
			}
		}
	}
}

// Cached tokens still occupy the context window. input_tokens excludes both
// cache portions once breakpoints are sent, so the prompt count the harness
// reports has to be the sum — or every cache hit would render a near-empty
// context meter over a nearly full window.
func TestAnthropicPromptCountIncludesCachedTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":7,"cache_creation_input_tokens":100,"cache_read_input_tokens":9000}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
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
	var promptEvalCount int
	err = client.ChatStream(context.Background(), ChatOptions{}, func(chunk StreamChunk) error {
		if chunk.Done {
			promptEvalCount = chunk.PromptEvalCount
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if promptEvalCount != 9107 {
		t.Fatalf("prompt tokens = %d, want 7 + 100 + 9000", promptEvalCount)
	}
}
