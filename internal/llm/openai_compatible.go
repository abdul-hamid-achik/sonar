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
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/netpolicy"
)

const (
	maxOpenAIStreamRecordBytes = 8 << 20
	maxOpenAIResponseBytes     = 16 << 20
	maxOpenAIErrorBytes        = 1 << 20
)

// OpenAICompatibleClient is a streaming chat adapter for OpenAI-compatible
// HTTP APIs (xAI, OpenAI, OpenRouter, local vLLM, etc.). Credentials are
// supplied by the process environment only — never from config files.
type OpenAICompatibleClient struct {
	httpClient *http.Client
	base       *url.URL
	baseURL    string
	model      string
	apiKey     string
	dialect    string
	thinking   bool
	effort     string
	mu         sync.RWMutex
}

// OpenAICompatibleOptions constructs a remote OpenAI-style client.
type OpenAICompatibleOptions struct {
	BaseURL string
	Model   string
	APIKey  string
	// Dialect selects provider-specific request extensions. Empty is plain
	// OpenAI. DialectDeepSeek adds the `thinking` toggle and the
	// `reasoning_content` round-trip that DeepSeek's tool-call turns require.
	Dialect string
	// Thinking requests chain-of-thought for dialects that expose a toggle.
	// Ignored by dialects without one.
	Thinking bool
	// ReasoningEffort grades thinking depth ("high", "max") when Thinking is on.
	ReasoningEffort string
}

// DialectDeepSeek marks the DeepSeek flavor of the OpenAI chat contract.
const DialectDeepSeek = "deepseek"

// parseProviderBaseURL validates and normalizes a base URL shared by every
// OpenAI- and Anthropic-family dialect: reject embedded credentials or a
// query/fragment (a validation error must never echo the raw input back), and
// require TLS for every non-local host. name identifies the caller in error
// text (e.g. "openai-compatible", "anthropic") without ever including the
// URL itself.
func parseProviderBaseURL(name, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s base_url is empty", name)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid %s base url", name)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("%s base url must not contain user information, a query, or a fragment", name)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%s base url must use http or https", name)
	}
	if scheme == "http" && !netpolicy.IsLocalHost(u.Hostname()) {
		return nil, fmt.Errorf("%s remote base url requires https", name)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// NewOpenAICompatibleClient builds a client against baseURL (for example
// https://api.x.ai/v1). The API key may be empty only for local open servers.
func NewOpenAICompatibleClient(opts OpenAICompatibleOptions) (*OpenAICompatibleClient, error) {
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		return nil, errors.New("openai-compatible model is empty")
	}
	u, err := parseProviderBaseURL("openai-compatible", opts.BaseURL)
	if err != nil {
		return nil, err
	}
	client := newOpenAIHTTPClient(u)
	return &OpenAICompatibleClient{
		httpClient: client,
		base:       u,
		baseURL:    strings.TrimSuffix(u.String(), "/"),
		model:      model,
		apiKey:     strings.TrimSpace(opts.APIKey),
		dialect:    strings.ToLower(strings.TrimSpace(opts.Dialect)),
		thinking:   opts.Thinking,
		effort:     strings.ToLower(strings.TrimSpace(opts.ReasoningEffort)),
	}, nil
}

func newOpenAIHTTPClient(base *url.URL) *http.Client {
	dialer := &net.Dialer{}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	}
	if netpolicy.IsLocalHost(base.Hostname()) {
		transport.Proxy = nil
		transport.DialContext = netpolicy.LocalOnlyDialContext(net.DefaultResolver, dialer.DialContext, "OpenAI-compatible")
	}
	originScheme := strings.ToLower(base.Scheme)
	originHost := strings.ToLower(base.Host)
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if strings.ToLower(req.URL.Scheme) != originScheme || strings.ToLower(req.URL.Host) != originHost {
				return fmt.Errorf("refusing cross-origin provider redirect to %s", req.URL.Redacted())
			}
			return nil
		},
	}
}

func (c *OpenAICompatibleClient) Model() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.model
}

// SetModel updates the model id for subsequent requests.
func (c *OpenAICompatibleClient) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("openai-compatible model is empty")
	}
	c.mu.Lock()
	c.model = model
	c.mu.Unlock()
	return nil
}

func (c *OpenAICompatibleClient) BaseURL() string { return c.baseURL }

// Ping checks that the API key and model are usable.
func (c *OpenAICompatibleClient) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	return c.PingContext(ctx)
}

// PingContext prefers GET /models when available, then falls back to a minimal
// non-streaming chat completion so APIs without a models list still work.
func (c *OpenAICompatibleClient) PingContext(ctx context.Context) error {
	if err := c.pingModels(ctx); err == nil {
		return nil
	}
	return c.pingChat(ctx)
}

func (c *OpenAICompatibleClient) pingModels(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/models", nil)
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
		return openAIStatusError(resp, body)
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return fmt.Errorf("decode provider models: %w", err)
	}
	model := c.Model()
	if len(listing.Data) == 0 {
		return nil // some hosts return empty lists; chat ping will catch hard failures
	}
	for _, item := range listing.Data {
		if item.ID == model {
			return nil
		}
	}
	// List succeeded but model was absent — still allow; hosts vary.
	return nil
}

func (c *OpenAICompatibleClient) pingChat(ctx context.Context) error {
	payload := openAIChatRequest{
		Model: c.Model(),
		Messages: []openAIMessage{{
			Role:    "user",
			Content: "ping",
		}},
		Stream:      false,
		MaxTokens:   1,
		Temperature: floatPtr(0),
	}
	var response openAIChatCompletion
	if err := c.doJSON(ctx, http.MethodPost, "/chat/completions", payload, &response); err != nil {
		return fmt.Errorf("model %q not available: %w", c.Model(), err)
	}
	return nil
}

// Embed is not implemented for the generic OpenAI-compatible chat adapter.
// ICE and local embeddings continue to use Ollama when enabled.
func (c *OpenAICompatibleClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, errors.New("openai-compatible provider does not implement embeddings; keep ICE on Ollama or disable it")
}

// ChatStream streams a chat completion via the OpenAI SSE protocol.
func (c *OpenAICompatibleClient) ChatStream(ctx context.Context, opts ChatOptions, fn func(StreamChunk) error) error {
	if fn == nil {
		return errors.New("openai-compatible stream callback is nil")
	}
	deepseek := c.dialect == DialectDeepSeek
	messages, err := convertOpenAIMessages(opts, deepseek)
	if err != nil {
		return inferenceNotStarted(err)
	}
	payload := openAIChatRequest{
		Model:    c.Model(),
		Messages: messages,
		Stream:   true,
		// Only meaningful on a streaming request, and every request here is
		// one. Without it a spec-strict provider sends no usage at all.
		StreamOptions: &openAIStreamOptions{IncludeUsage: true},
		Tools:         convertOpenAITools(opts.Tools),
	}
	if opts.MaxEvalTokens > 0 {
		payload.MaxTokens = opts.MaxEvalTokens
	}
	switch {
	case deepseek:
		// DeepSeek defaults thinking to enabled, and reasoning_effort only
		// grades depth once it is on — it cannot switch it off. Always state
		// the toggle explicitly so DisableReasoning is honored rather than
		// silently billed as a thinking turn.
		thinking := c.thinking && !opts.DisableReasoning
		if thinking {
			payload.Thinking = &thinkingConfig{Type: "enabled"}
			payload.ReasoningEffort = c.effort
		} else {
			payload.Thinking = &thinkingConfig{Type: "disabled"}
		}
	case opts.DisableReasoning:
		// Best-effort for plain OpenAI hosts; many ignore unknown fields.
		payload.ReasoningEffort = "none"
	}

	req, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", payload)
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
		return markRemoteInferenceError(openAIStatusError(resp, body))
	}

	return markRemoteInferenceError(c.consumeSSE(ctx, resp.Body, fn))
}

func (c *OpenAICompatibleClient) consumeSSE(ctx context.Context, body io.ReadCloser, fn func(StreamChunk) error) error {
	// Mirror the Ollama watchdog: a timer closes the body on idle silence so
	// a blocked ReadString returns instead of holding the turn open forever.
	streamCtx, cancelStream := context.WithCancelCause(ctx)
	defer cancelStream(nil)
	idle := time.AfterFunc(streamIdleTimeout, func() {
		cancelStream(fmt.Errorf("%w (%s)", ErrStreamIdle, streamIdleTimeout))
		_ = body.Close()
	})
	defer idle.Stop()

	reader := bufio.NewReaderSize(body, 64*1024)
	toolAccum := map[int]*toolCallBuilder{}
	sawDone := false

	// ProviderTiming documents itself as "provider-reported request timings
	// plus the client-measured time to first streamed token", and this dialect
	// reported none at all — so the --json receipt's timing block was empty for
	// every hosted provider, including DeepSeek, the default. Only the
	// inherited Ollama client filled it, which is why a spec driving a fake
	// local daemon was the one place it looked like it worked.
	//
	// Time-to-first-token and total duration need no provider cooperation.
	// LoadDuration and the per-phase durations do, so they stay zero, which the
	// contract already defines as "not reported" rather than "instant".
	streamStart := time.Now()
	var firstToken time.Duration
	timing := func() *ProviderTiming {
		return &ProviderTiming{
			TimeToFirstToken: firstToken,
			TotalDuration:    time.Since(streamStart),
		}
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if cause := context.Cause(streamCtx); errors.Is(cause, ErrStreamIdle) && ctx.Err() == nil {
				return cause
			}
			if errors.Is(err, io.EOF) {
				if !sawDone {
					// Some servers close without a terminal chunk after the last delta.
					chunk := StreamChunk{Done: true, ToolCalls: finalizeToolCalls(toolAccum), Timing: timing()}
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
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			chunk := StreamChunk{Done: true, ToolCalls: finalizeToolCalls(toolAccum), Timing: timing()}
			return fn(chunk)
		}
		if len(data) > maxOpenAIStreamRecordBytes {
			return fmt.Errorf("provider stream record exceeds %d-byte limit", maxOpenAIStreamRecordBytes)
		}
		var event openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("decode provider stream: %w", err)
		}
		if event.Error != nil && event.Error.Message != "" {
			return errors.New(event.Error.Message)
		}
		if len(event.Choices) == 0 {
			continue
		}
		choice := event.Choices[0]
		delta := choice.Delta
		if firstToken == 0 && (delta.Content != "" || delta.ReasoningContent != "" || delta.Reasoning != "") {
			firstToken = time.Since(streamStart)
		}
		chunk := StreamChunk{
			Text:      delta.Content,
			Reasoning: firstNonEmpty(delta.ReasoningContent, delta.Reasoning),
		}
		for _, call := range delta.ToolCalls {
			idx := call.Index
			builder, ok := toolAccum[idx]
			if !ok {
				builder = &toolCallBuilder{}
				toolAccum[idx] = builder
			}
			if call.ID != "" {
				builder.id = call.ID
			}
			if call.Function.Name != "" {
				builder.name += call.Function.Name
			}
			if call.Function.Arguments != "" {
				builder.arguments.WriteString(call.Function.Arguments)
			}
		}
		finish := choice.FinishReason
		if finish == "stop" || finish == "tool_calls" || finish == "length" {
			chunk.Done = true
			chunk.FinishReason = finish
			chunk.ToolCalls = finalizeToolCalls(toolAccum)
			sawDone = true
			chunk.Timing = timing()
			if event.Usage != nil {
				chunk.EvalCount = event.Usage.CompletionTokens
				chunk.PromptEvalCount = event.Usage.PromptTokens
			}
		}
		if err := fn(chunk); err != nil {
			return err
		}
		if chunk.Done {
			return nil
		}
	}
}

type toolCallBuilder struct {
	id        string
	name      string
	arguments strings.Builder
}

func finalizeToolCalls(builders map[int]*toolCallBuilder) []ToolCall {
	if len(builders) == 0 {
		return nil
	}
	// Preserve index order.
	max := -1
	for idx := range builders {
		if idx > max {
			max = idx
		}
	}
	out := make([]ToolCall, 0, len(builders))
	for i := 0; i <= max; i++ {
		builder, ok := builders[i]
		if !ok {
			continue
		}
		args := map[string]any{}
		raw := builder.arguments.String()
		if strings.TrimSpace(raw) != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				// Keep a single string field so the host still sees the payload.
				args = map[string]any{"_raw": raw}
			}
		}
		id := builder.id
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		out = append(out, ToolCall{
			ID:        id,
			Name:      builder.name,
			Arguments: args,
		})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func floatPtr(v float64) *float64 { return &v }

type openAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	// StreamOptions asks for the terminal usage receipt. The OpenAI streaming
	// contract omits usage unless include_usage is set, and sonar's budgets are
	// not advisory: a turn that ends without a trustworthy receipt charges its
	// reservation fail-closed, so a provider that follows the spec strictly
	// would exhaust a goal's budget on its first turn.
	//
	// DeepSeek sends usage regardless, which is why this went unnoticed until
	// Ollama Cloud — which does not — became a supported provider.
	StreamOptions   *openAIStreamOptions `json:"stream_options,omitempty"`
	Tools           []openAITool         `json:"tools,omitempty"`
	MaxTokens       int                  `json:"max_tokens,omitempty"`
	Temperature     *float64             `json:"temperature,omitempty"`
	ReasoningEffort string               `json:"reasoning_effort,omitempty"`
	// Thinking is DeepSeek's explicit chain-of-thought toggle. It is a distinct
	// control from ReasoningEffort, which only grades effort once thinking is
	// on; reasoning_effort alone cannot switch thinking off.
	Thinking *thinkingConfig `json:"thinking,omitempty"`
}

// openAIStreamOptions carries the one streaming option sonar needs.
type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// thinkingConfig is DeepSeek's `{"thinking": {"type": "enabled"|"disabled"}}`.
type thinkingConfig struct {
	Type string `json:"type"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content,omitempty"`
	// ReasoningContent is a pointer so an assistant message that carries tool
	// calls emits the key even when the reasoning text is empty. DeepSeek
	// rejects a tool-call turn whose assistant message drops the field, and an
	// omitempty string could not express "present but empty".
	ReasoningContent *string          `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	Name             string           `json:"name,omitempty"`
}

type openAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type openAIToolCall struct {
	Index    int                    `json:"index,omitempty"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function openAIToolCallFunction `json:"function"`
}

type openAIToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openAIStreamChunk struct {
	Choices []openAIStreamChoice `json:"choices"`
	Usage   *openAIUsage         `json:"usage,omitempty"`
	Error   *openAIErrorBody     `json:"error,omitempty"`
}

type openAIStreamChoice struct {
	Delta        openAIStreamDelta `json:"delta"`
	FinishReason string            `json:"finish_reason"`
}

type openAIStreamDelta struct {
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	Reasoning        string           `json:"reasoning,omitempty"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIChatCompletion struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
	Error *openAIErrorBody `json:"error,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitempty"`
}

type openAIHTTPError struct {
	StatusCode int
	Status     string
	Message    string
	// RetryAfter is the provider's Retry-After response header, parsed to a
	// positive duration. Zero covers three cases that all mean "no extra
	// wait is mandated by the header": no header was sent, the header asked
	// for zero seconds, or an HTTP-date header had already elapsed. See
	// IsRetryableProviderError and ProviderRetryAfter — zero is never a
	// license to retry a 429 immediately on its own; it only means this
	// particular signal supplied no additional delay.
	RetryAfter time.Duration
}

// ProviderHTTPStatus reports the HTTP status a provider returned, when the
// error carries one. It recognizes both HTTP-error shapes this package
// produces: the OpenAI/Anthropic-dialect openAIHTTPError and Ollama's own
// ollamaHTTPError, so callers get one status lookup regardless of which
// client dispatched the request.
//
// The status code alone is a bounded, host-safe fact: unlike the response body
// it cannot contain provider prose, an endpoint, or a credential. That makes it
// the one detail a startup diagnostic can surface without weakening the
// transcript sanitization that ProviderFailureCopy exists to enforce.
func ProviderHTTPStatus(err error) (int, bool) {
	var httpErr *openAIHTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode > 0 {
		return httpErr.StatusCode, true
	}
	var ollamaErr *ollamaHTTPError
	if errors.As(err, &ollamaErr) && ollamaErr.StatusCode > 0 {
		return ollamaErr.StatusCode, true
	}
	return 0, false
}

// ProviderStatusHint maps a provider HTTP status to an actionable cause. The
// text is host-authored, never the provider's.
func ProviderStatusHint(status int) string {
	switch {
	case status == 401:
		return "the API key was rejected"
	case status == 403:
		return "the key is valid but the request was refused — usually exhausted credits, a spending limit, or a region restriction"
	case status == 404:
		return "the model or endpoint does not exist for this provider"
	case status == 429:
		return "rate limited or the account's quota is exhausted"
	case status >= 500:
		return "the provider is failing on its side"
	default:
		return "the provider rejected the request"
	}
}

func (e *openAIHTTPError) Error() string {
	if e.Message == "" {
		return e.Status
	}
	return fmt.Sprintf("%s: %s", e.Status, e.Message)
}

func openAIStatusError(resp *http.Response, body []byte) error {
	message := strings.TrimSpace(string(body))
	var envelope struct {
		Error openAIErrorBody `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	retryAfter, _ := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	return &openAIHTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Message:    message,
		RetryAfter: retryAfter,
	}
}

// convertOpenAIMessages renders host messages onto the wire. echoReasoning is
// set only for providers that require an assistant tool-call turn to carry its
// own chain-of-thought back (DeepSeek); hosts that reject the unknown field
// must never receive it.
func convertOpenAIMessages(opts ChatOptions, echoReasoning bool) ([]openAIMessage, error) {
	out := make([]openAIMessage, 0, len(opts.Messages)+1)
	if opts.System != "" {
		out = append(out, openAIMessage{Role: "system", Content: opts.System})
	}
	for i, message := range opts.Messages {
		converted := openAIMessage{
			Role:       message.Role,
			ToolCallID: message.ToolCallID,
			Name:       message.ToolName,
		}
		// Echo reasoning only where it is contractually required: an assistant
		// message that requested tools. Sending it on a plain answer is ignored
		// by the API and would leak reasoning into more requests than needed.
		if echoReasoning && message.Role == "assistant" && len(message.ToolCalls) > 0 {
			reasoning := message.ReasoningContent
			converted.ReasoningContent = &reasoning
		}
		if len(message.ToolCalls) > 0 {
			for index, call := range message.ToolCalls {
				args, err := json.Marshal(call.Arguments)
				if err != nil {
					return nil, fmt.Errorf("message %d tool call %d: encode arguments: %w", i, index, err)
				}
				id := call.ID
				if id == "" {
					id = fmt.Sprintf("call_%d", index)
				}
				converted.ToolCalls = append(converted.ToolCalls, openAIToolCall{
					Index: index,
					ID:    id,
					Type:  "function",
					Function: openAIToolCallFunction{
						Name:      call.Name,
						Arguments: string(args),
					},
				})
			}
		}
		if len(message.Images) > 0 {
			parts := make([]openAIContentPart, 0, len(message.Images)+1)
			if message.Content != "" {
				parts = append(parts, openAIContentPart{Type: "text", Text: message.Content})
			}
			for imageIndex, image := range message.Images {
				if err := image.Validate(); err != nil {
					return nil, fmt.Errorf("message %d image %d: %w", i, imageIndex, err)
				}
				dataURL := "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
				parts = append(parts, openAIContentPart{
					Type:     "image_url",
					ImageURL: &openAIImageURL{URL: dataURL},
				})
			}
			converted.Content = parts
		} else {
			converted.Content = message.Content
		}
		// OpenAI tool role messages need content string.
		if message.Role == "tool" && converted.Content == nil {
			converted.Content = ""
		}
		out = append(out, converted)
	}
	return out, nil
}

func convertOpenAITools(tools []ToolDef) []openAITool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openAITool, 0, len(tools))
	for _, tool := range tools {
		params := tool.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, openAITool{
			Type: "function",
			Function: openAIToolFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

func (c *OpenAICompatibleClient) doJSON(ctx context.Context, method, route string, payload, output any) error {
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
		return openAIStatusError(resp, body)
	}
	if output == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("decode provider response: %w", err)
	}
	return nil
}

func (c *OpenAICompatibleClient) newRequest(ctx context.Context, method, route string, payload any) (*http.Request, error) {
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
	req.Header.Set("User-Agent", "sonar/openai-compatible")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return req, nil
}

func joinURLPath(basePath, route string) string {
	basePath = strings.TrimRight(basePath, "/")
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	if basePath == "" {
		return route
	}
	return basePath + route
}
