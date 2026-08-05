package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/catalog"
)

// AnthropicClient is a streaming chat adapter for the Anthropic Messages API
// (https://api.anthropic.com/v1/messages) and its wire-compatible proxies.
// Four catalog providers share this exact contract — see NewAnthropicProviderClient.
//
// The Messages API differs from the OpenAI chat-completions shape in ways
// that break a naive OpenAI-compatible client:
//
//   - The system prompt is a top-level "system" field, not a message with
//     role "system".
//   - Message content is an array of typed content blocks ("text",
//     "tool_use", "tool_result", "image"), not a plain string.
//   - Tool use arrives as tool_use content blocks; results are sent back as
//     tool_result content blocks inside a user-role message.
//   - Streaming uses a distinct SSE event vocabulary (message_start,
//     content_block_start/delta/stop, message_delta, message_stop) instead of
//     OpenAI's choices[].delta shape.
//   - Auth is the x-api-key header plus a required anthropic-version header,
//     not "Authorization: Bearer".
//   - max_tokens is required on every request; OpenAI treats it as optional.
//
// Credentials are supplied by the process environment only — never from
// config files.
type AnthropicClient struct {
	httpClient *http.Client
	base       *url.URL
	baseURL    string
	model      string
	apiKey     string
	// defaultMaxTokens is sent as max_tokens whenever ChatOptions.MaxEvalTokens
	// is unset (commonly true for ordinary, unbudgeted AUTO turns). Anthropic
	// rejects a request with no max_tokens at all.
	defaultMaxTokens int
	mu               sync.RWMutex
}

// AnthropicOptions constructs a remote Anthropic Messages API client.
type AnthropicOptions struct {
	BaseURL string
	Model   string
	APIKey  string
	// MaxTokens is the max_tokens value sent when ChatOptions.MaxEvalTokens is
	// zero. Defaults to AnthropicDefaultMaxOutputTokens when zero or negative.
	MaxTokens int
}

const (
	// AnthropicAPIVersion is the Messages API protocol version. It is
	// independent of model generation — every request, regardless of which
	// Claude (or Claude-compatible) model is targeted, sends this same value.
	AnthropicAPIVersion = "2023-06-01"

	// AnthropicDefaultMaxOutputTokens is the max_tokens fallback used when
	// neither the request nor the catalog's per-model default supplies one.
	// It is a conservative floor, not a target — NewAnthropicProviderClient
	// prefers the catalog's default_max_tokens for the resolved model.
	AnthropicDefaultMaxOutputTokens = 8192

	// AnthropicBaseURL is the real Anthropic API endpoint. The catalog's
	// "anthropic" entry names its api_endpoint as the template
	// "$ANTHROPIC_API_ENDPOINT" (an optional self-hosted-proxy override), not
	// a literal URL, so this is the fallback used whenever no override is
	// configured.
	AnthropicBaseURL = "https://api.anthropic.com"

	// AnthropicAPIKeyEnv names the environment variable holding the official
	// Anthropic API key. Only the name is ever configured; the value is read
	// from the process environment so a secret can never land in YAML.
	AnthropicAPIKeyEnv = "ANTHROPIC_API_KEY"

	// KimiCodingBaseURL, MiniMaxBaseURL, and MiniMaxChinaBaseURL are the three
	// Anthropic-Messages-API-compatible proxies the catalog lists with wire
	// type "anthropic". Unlike Anthropic's own template, the catalog gives
	// each of these a literal endpoint; they are reproduced here as
	// doc-verified constants (internal/catalog/providers.json) rather than
	// re-reading the catalog on every client build.
	KimiCodingBaseURL   = "https://api.kimi.com/coding"
	MiniMaxBaseURL      = "https://api.minimax.io/anthropic"
	MiniMaxChinaBaseURL = "https://api.minimaxi.com/anthropic"
)

// Provider identities for the four catalog entries whose wire type is
// "anthropic". NewProviderClient selects the Anthropic dialect by checking a
// provider's identity against this set, not by reading the catalog's wire
// type string — see NewProviderClient's dialect-selection comment in
// deepseek.go for why that distinction is load-bearing.
const (
	AnthropicProviderID    = "anthropic"
	KimiCodingProviderID   = "kimi-coding"
	MiniMaxProviderID      = "minimax"
	MiniMaxChinaProviderID = "minimax-china"
)

// IsAnthropicFamilyProvider reports whether providerType (already normalized
// by config.NormalizedProviderType) is one of the four catalog providers that
// speak the Anthropic Messages API.
func IsAnthropicFamilyProvider(providerType string) bool {
	switch providerType {
	case AnthropicProviderID, KimiCodingProviderID, MiniMaxProviderID, MiniMaxChinaProviderID:
		return true
	default:
		return false
	}
}

// NewAnthropicProviderClient builds the Anthropic Messages API client for one
// of the four anthropic-family catalog providers. model must already be
// resolved (see ResolveProviderModel); apiKey must already be resolved from
// the process environment by the caller.
//
// baseURL is whatever the caller's configuration resolved. When it is empty,
// or is an unresolved catalog template (Catwalk's "anthropic" entry names its
// endpoint as the literal string "$ANTHROPIC_API_ENDPOINT" for callers that
// substitute environment overrides — sonar's config layer does not, so an
// unset override would otherwise reach here verbatim), this falls back to the
// provider's real, doc-verified endpoint instead of guessing.
func NewAnthropicProviderClient(providerID, baseURL, model, apiKey string) (*AnthropicClient, error) {
	if strings.TrimSpace(apiKey) == "" {
		envName := catalog.APIKeyEnv(catalog.ProviderID(providerID))
		if envName == "" {
			envName = "the provider's API key"
		}
		return nil, fmt.Errorf(
			"%s is unset or empty; sonar requires an API key for this provider (export %s, or inject it at launch)",
			envName, envName,
		)
	}
	maxTokens := AnthropicDefaultMaxOutputTokens
	if catalogModel, ok := catalog.LookupModel(catalog.ProviderID(providerID), model); ok {
		if catalogModel.DefaultMaxTokens > 0 && catalogModel.DefaultMaxTokens <= anthropicMaxTokensCeiling {
			maxTokens = int(catalogModel.DefaultMaxTokens)
		}
	}
	return NewAnthropicClient(AnthropicOptions{
		BaseURL:   anthropicFamilyBaseURL(providerID, baseURL),
		Model:     model,
		APIKey:    apiKey,
		MaxTokens: maxTokens,
	})
}

// anthropicMaxTokensCeiling bounds a catalog-sourced default_max_tokens
// before the int64-to-int conversion, mirroring config.maxProviderContextSize's
// defensive bound for the same class of catalog-derived integer.
const anthropicMaxTokensCeiling = 1 << 24

func anthropicFamilyBaseURL(providerID, baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed != "" && !strings.HasPrefix(trimmed, "$") {
		return trimmed
	}
	switch providerID {
	case AnthropicProviderID:
		return AnthropicBaseURL
	case KimiCodingProviderID:
		return KimiCodingBaseURL
	case MiniMaxProviderID:
		return MiniMaxBaseURL
	case MiniMaxChinaProviderID:
		return MiniMaxChinaBaseURL
	default:
		return trimmed
	}
}

// NewAnthropicClient builds a client against baseURL (for example
// https://api.anthropic.com). It performs no provider-identity resolution —
// callers that know a catalog provider identity should prefer
// NewAnthropicProviderClient, which also supplies doc-verified endpoint
// fallbacks and a catalog-derived default max_tokens.
func NewAnthropicClient(opts AnthropicOptions) (*AnthropicClient, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		return nil, errors.New("anthropic model is empty")
	}
	u, err := parseProviderBaseURL("anthropic", opts.BaseURL)
	if err != nil {
		return nil, err
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = AnthropicDefaultMaxOutputTokens
	}
	// Reuse the OpenAI-compatible dialect's transport hardening verbatim:
	// local-only dialing when the host is loopback, and refusal of any
	// cross-origin redirect. Nothing about it is OpenAI-specific — it is
	// generic per-origin HTTP client hardening keyed only on the parsed base
	// URL.
	client := newOpenAIHTTPClient(u)
	return &AnthropicClient{
		httpClient:       client,
		base:             u,
		baseURL:          strings.TrimSuffix(u.String(), "/"),
		model:            model,
		apiKey:           strings.TrimSpace(opts.APIKey),
		defaultMaxTokens: maxTokens,
	}, nil
}

func (c *AnthropicClient) Model() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.model
}

// SetModel updates the model id for subsequent requests.
func (c *AnthropicClient) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("anthropic model is empty")
	}
	c.mu.Lock()
	c.model = model
	c.mu.Unlock()
	return nil
}

// BaseURL returns the configured Anthropic base URL for display.
func (c *AnthropicClient) BaseURL() string { return c.baseURL }

// Embed is not implemented for the Anthropic Messages API adapter: Anthropic
// serves no embeddings endpoint. ICE and local embeddings continue to use
// Ollama when enabled.
func (c *AnthropicClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, errors.New("anthropic provider does not implement embeddings; keep ICE on Ollama or disable it")
}

// Ping checks that the API key and model are usable.
func (c *AnthropicClient) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	return c.PingContext(ctx)
}

// PingContext prefers GET /v1/models when available, then falls back to a
// minimal non-streaming Messages request so proxies without a models list
// still work.
func (c *AnthropicClient) PingContext(ctx context.Context) error {
	if err := c.pingModels(ctx); err == nil {
		return nil
	}
	return c.pingChat(ctx)
}

func (c *AnthropicClient) pingModels(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("provider models: %w", err)
	}
	defer func() {
		// The bounded read below determines the operation result. Closing a
		// fully consumed response body cannot make that response unsuccessful.
		_ = resp.Body.Close()
	}()
	body, err := readBoundedBody(resp.Body, maxOpenAIResponseBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return anthropicStatusError(resp, body)
	}
	return nil
}

func (c *AnthropicClient) pingChat(ctx context.Context) error {
	content := "ping"
	payload := anthropicMessagesRequest{
		Model:     c.Model(),
		MaxTokens: 1,
		Messages: []anthropicMessage{{
			Role:    "user",
			Content: []anthropicContentBlock{{Type: "text", Text: &content}},
		}},
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/messages", payload, nil); err != nil {
		return fmt.Errorf("model %q not available: %w", c.Model(), err)
	}
	return nil
}

// ChatStream streams a chat completion via the Anthropic Messages SSE
// protocol.
func (c *AnthropicClient) ChatStream(ctx context.Context, opts ChatOptions, fn func(StreamChunk) error) error {
	if fn == nil {
		return errors.New("anthropic stream callback is nil")
	}
	messages, system, err := convertAnthropicMessages(opts)
	if err != nil {
		return inferenceNotStarted(err)
	}
	maxTokens := opts.MaxEvalTokens
	if maxTokens <= 0 {
		c.mu.RLock()
		maxTokens = c.defaultMaxTokens
		c.mu.RUnlock()
	}
	payload := anthropicMessagesRequest{
		Model:     c.Model(),
		MaxTokens: maxTokens,
		Messages:  messages,
		Tools:     convertAnthropicTools(opts.Tools),
		Stream:    true,
	}
	if system != "" {
		payload.System = system
	}

	req, err := c.newRequest(ctx, http.MethodPost, "/v1/messages", payload)
	if err != nil {
		return inferenceNotStarted(err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return markRemoteInferenceError(fmt.Errorf("provider chat stream: %w", err))
	}
	defer func() {
		// Stream completion/error is established while reading; a later close
		// error carries no additional provider outcome information.
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := readBoundedBody(resp.Body, maxOpenAIErrorBytes)
		return markRemoteInferenceError(anthropicStatusError(resp, body))
	}

	return markRemoteInferenceError(c.consumeSSE(ctx, resp.Body, fn))
}

// consumeSSE parses the Anthropic Messages SSE event vocabulary
// (message_start, content_block_start/delta/stop, message_delta,
// message_stop, ping, error). It shares the OpenAI-compatible dialect's idle
// watchdog pattern verbatim: a timer closes the body on silence so a blocked
// ReadString returns instead of holding the turn open forever.
func (c *AnthropicClient) consumeSSE(ctx context.Context, body io.ReadCloser, fn func(StreamChunk) error) error {
	streamCtx, cancelStream := context.WithCancelCause(ctx)
	defer cancelStream(nil)
	idle := time.AfterFunc(streamIdleTimeout, func() {
		cancelStream(fmt.Errorf("%w (%s)", ErrStreamIdle, streamIdleTimeout))
		_ = body.Close()
	})
	defer idle.Stop()

	reader := bufio.NewReaderSize(body, 64*1024)
	toolAccum := map[int]*toolCallBuilder{}
	blockKind := map[int]string{}
	promptTokens := 0
	completionTokens := 0
	finish := ""
	sawStop := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if cause := context.Cause(streamCtx); errors.Is(cause, ErrStreamIdle) && ctx.Err() == nil {
				return cause
			}
			if errors.Is(err, io.EOF) {
				if !sawStop {
					// Some proxies close without a terminal message_stop event.
					chunk := StreamChunk{
						Done:            true,
						ToolCalls:       finalizeToolCalls(toolAccum),
						FinishReason:    finish,
						EvalCount:       completionTokens,
						PromptEvalCount: promptTokens,
					}
					if err := fn(chunk); err != nil {
						return err
					}
				}
				return nil
			}
			return fmt.Errorf("read provider stream: %w", err)
		}
		idle.Reset(streamIdleTimeout)
		line = strings.TrimRight(line, "\r\n")
		if line == "" || !strings.HasPrefix(line, "data:") {
			// Blank lines separate frames; "event:" lines are redundant with
			// the "type" field already present in every data payload below.
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if len(data) > maxOpenAIStreamRecordBytes {
			return fmt.Errorf("provider stream record exceeds %d-byte limit", maxOpenAIStreamRecordBytes)
		}
		var event anthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("decode provider stream: %w", err)
		}
		switch event.Type {
		case "message_start":
			if event.Message != nil && event.Message.Usage != nil {
				promptTokens = event.Message.Usage.InputTokens
			}
		case "content_block_start":
			if event.ContentBlock != nil {
				blockKind[event.Index] = event.ContentBlock.Type
				if event.ContentBlock.Type == "tool_use" {
					toolAccum[event.Index] = &toolCallBuilder{
						id:   event.ContentBlock.ID,
						name: event.ContentBlock.Name,
					}
				}
			}
		case "content_block_delta":
			if event.Delta == nil {
				continue
			}
			switch blockKind[event.Index] {
			case "tool_use":
				if builder, ok := toolAccum[event.Index]; ok && event.Delta.PartialJSON != "" {
					builder.arguments.WriteString(event.Delta.PartialJSON)
				}
			case "thinking":
				// Reasoning is host-only (StreamChunk.Reasoning is never
				// persisted) and must never cross a session, transcript, or
				// checkpoint boundary — see llm.Message.ReasoningContent.
				if event.Delta.Thinking != "" {
					if err := fn(StreamChunk{Reasoning: event.Delta.Thinking}); err != nil {
						return err
					}
				}
			default: // "text" or an unrecognized future block type
				if event.Delta.Text != "" {
					if err := fn(StreamChunk{Text: event.Delta.Text}); err != nil {
						return err
					}
				}
			}
		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				finish = anthropicFinishReason(event.Delta.StopReason)
			}
			if event.Usage != nil && event.Usage.OutputTokens > 0 {
				completionTokens = event.Usage.OutputTokens
			}
		case "message_stop":
			sawStop = true
			chunk := StreamChunk{
				Done:            true,
				ToolCalls:       finalizeToolCalls(toolAccum),
				FinishReason:    finish,
				EvalCount:       completionTokens,
				PromptEvalCount: promptTokens,
			}
			return fn(chunk)
		case "error":
			if event.Error != nil && event.Error.Message != "" {
				return errors.New(event.Error.Message)
			}
			return errors.New("provider stream error")
		default:
			// "ping", "content_block_stop", and any future event type carry
			// nothing this loop needs.
		}
	}
}

// anthropicFinishReason maps Anthropic's stop_reason vocabulary onto the
// values StreamChunk.FinishReason already promises callers. The "length"
// mapping is load-bearing: agent/json_output.go detects a truncated
// generation by comparing FinishReason to the literal string "length", never
// by the absence of "stop".
func anthropicFinishReason(stopReason string) string {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		// "pause_turn", "refusal", or a future value: pass through rather than
		// collapse it into "stop" and lose the distinction.
		return stopReason
	}
}

// convertAnthropicMessages renders host messages onto the wire, returning the
// message array and the concatenated top-level system text. Anthropic's base
// Messages API has no message-role "system" — only "user" and "assistant" are
// valid roles in the array, and roles must strictly alternate. A host
// llm.Message with Role "system" (agent/compact.go and agent/agent.go both
// append durable-recovery-context messages this way) is therefore folded into
// the top-level system field rather than sent as a mid-history message: the
// newer per-model "mid-conversation system message" capability is not
// something every anthropic-wire-family provider (Kimi Coding, MiniMax) is
// guaranteed to support, and folding is correct everywhere.
//
// Anthropic also requires strict user/assistant alternation, so consecutive
// host messages that map to the same wire role (most commonly several
// sequential tool results) are coalesced into one message with multiple
// content blocks rather than emitted as separate messages.
func convertAnthropicMessages(opts ChatOptions) ([]anthropicMessage, string, error) {
	var systemParts []string
	if strings.TrimSpace(opts.System) != "" {
		systemParts = append(systemParts, opts.System)
	}
	out := make([]anthropicMessage, 0, len(opts.Messages))
	for i, message := range opts.Messages {
		if message.Role == "system" {
			if message.Content != "" {
				systemParts = append(systemParts, message.Content)
			}
			continue
		}
		role, err := anthropicRole(message.Role)
		if err != nil {
			return nil, "", fmt.Errorf("message %d: %w", i, err)
		}
		blocks, err := anthropicContentBlocksFor(message, i)
		if err != nil {
			return nil, "", err
		}
		if len(blocks) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}
		out = append(out, anthropicMessage{Role: role, Content: blocks})
	}
	return out, strings.Join(systemParts, "\n\n"), nil
}

func anthropicRole(role string) (string, error) {
	switch role {
	case "user", "tool":
		return "user", nil
	case "assistant":
		return "assistant", nil
	default:
		return "", fmt.Errorf("unsupported role %q for anthropic dialect", role)
	}
}

func anthropicContentBlocksFor(message Message, index int) ([]anthropicContentBlock, error) {
	var blocks []anthropicContentBlock
	switch message.Role {
	case "tool":
		// Tool results are sent back as tool_result blocks inside a
		// user-role message. Content is a pointer so the key is present even
		// when the tool's output was legitimately an empty string, matching
		// how convertOpenAIMessages guarantees the analogous OpenAI "tool"
		// role content key.
		content := message.Content
		blocks = append(blocks, anthropicContentBlock{
			Type:      "tool_result",
			ToolUseID: message.ToolCallID,
			Content:   &content,
		})
		return blocks, nil
	case "assistant":
		if message.Content != "" {
			text := message.Content
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: &text})
		}
		for callIndex, call := range message.ToolCalls {
			input := call.Arguments
			if input == nil {
				input = map[string]any{}
			}
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", index, callIndex)
			}
			blocks = append(blocks, anthropicContentBlock{
				Type:  "tool_use",
				ID:    id,
				Name:  call.Name,
				Input: input,
			})
		}
	default: // "user"
		if message.Content != "" {
			text := message.Content
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: &text})
		}
	}
	for imageIndex, image := range message.Images {
		if err := image.Validate(); err != nil {
			return nil, fmt.Errorf("message %d image %d: %w", index, imageIndex, err)
		}
		blocks = append(blocks, anthropicContentBlock{
			Type: "image",
			Source: &anthropicImageSource{
				Type:      "base64",
				MediaType: image.MediaType,
				Data:      base64.StdEncoding.EncodeToString(image.Data),
			},
		})
	}
	return blocks, nil
}

func convertAnthropicTools(tools []ToolDef) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, tool := range tools {
		schema := tool.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, anthropicTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	return out
}

func (c *AnthropicClient) doJSON(ctx context.Context, method, route string, payload, output any) error {
	req, err := c.newRequest(ctx, method, route, payload)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		// The bounded read below determines the operation result. Closing a
		// fully consumed response body cannot make that response unsuccessful.
		_ = resp.Body.Close()
	}()
	body, err := readBoundedBody(resp.Body, maxOpenAIResponseBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return anthropicStatusError(resp, body)
	}
	if output == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("decode provider response: %w", err)
	}
	return nil
}

func (c *AnthropicClient) newRequest(ctx context.Context, method, route string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode provider request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := c.base.ResolveReference(&url.URL{Path: joinURLPath(c.base.Path, route)})
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "sonar/anthropic")
	// Anthropic authenticates with x-api-key, not "Authorization: Bearer", and
	// requires the protocol version on every request regardless of model.
	req.Header.Set("anthropic-version", AnthropicAPIVersion)
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	return req, nil
}

// anthropicStatusError reuses openAIHTTPError so ProviderHTTPStatus and
// ProviderStatusHint (defined in openai_compatible.go) work uniformly across
// both dialects without change: the type carries nothing OpenAI-specific,
// only a status code and a provider-authored message.
func anthropicStatusError(resp *http.Response, body []byte) error {
	message := strings.TrimSpace(string(body))
	var envelope struct {
		Error anthropicErrorBody `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	return &openAIHTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Message:    message,
	}
}

// --- Wire types -------------------------------------------------------

type anthropicMessagesRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

// anthropicContentBlock is a flexible union across the block kinds this
// dialect emits (text, tool_use, tool_result, image). Fields irrelevant to a
// given block's Type are left zero and omitted from the wire payload.
type anthropicContentBlock struct {
	Type string `json:"type"`

	// text
	Text *string `json:"text,omitempty"`

	// tool_use
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`

	// tool_result. Content is a pointer so the key is present even when the
	// tool's output was an empty string — omitempty on a plain string would
	// silently drop the key rather than send "".
	ToolUseID string  `json:"tool_use_id,omitempty"`
	Content   *string `json:"content,omitempty"`
	IsError   bool    `json:"is_error,omitempty"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicStreamEvent is the union of every Messages API SSE event this
// dialect handles, dispatched on Type. The wire sends a matching "event:"
// line before each "data:" frame, but the payload's own "type" field is
// authoritative and sufficient to dispatch on, so consumeSSE never inspects
// the "event:" line.
type anthropicStreamEvent struct {
	Type string `json:"type"`

	// message_start
	Message *anthropicStreamMessage `json:"message,omitempty"`

	// content_block_start
	Index        int                          `json:"index"`
	ContentBlock *anthropicStreamContentBlock `json:"content_block,omitempty"`

	// content_block_delta / message_delta
	Delta *anthropicStreamDelta `json:"delta,omitempty"`

	// message_delta
	Usage *anthropicUsage `json:"usage,omitempty"`

	// error
	Error *anthropicErrorBody `json:"error,omitempty"`
}

type anthropicStreamMessage struct {
	ID    string          `json:"id"`
	Model string          `json:"model"`
	Usage *anthropicUsage `json:"usage,omitempty"`
}

type anthropicStreamContentBlock struct {
	Type string `json:"type"` // "text" | "tool_use" | "thinking"
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type anthropicStreamDelta struct {
	// content_block_delta
	Type        string `json:"type,omitempty"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`

	// message_delta
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

type anthropicErrorBody struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message"`
}
