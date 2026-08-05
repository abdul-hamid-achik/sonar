package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/netpolicy"
)

const (
	// pingTimeout bounds the startup/availability check so an unreachable or
	// hung Ollama server can never block application startup indefinitely.
	pingTimeout = 10 * time.Second

	// Ollama's own client uses an 8 MiB NDJSON token limit. Keep the same
	// compatibility bound while ensuring a malformed local server cannot grow
	// memory without limit. Non-streaming metadata has a separate total cap.
	maxOllamaStreamRecordBytes = 8 << 20
	maxOllamaResponseBytes     = 16 << 20
	maxOllamaErrorBytes        = 1 << 20
)

// streamIdleTimeout bounds the silence between stream records (and before
// the first one). It must stay generous: loading a multi-gigabyte model
// from disk and evaluating a large prompt both legitimately produce long
// gaps before the first token on constrained hardware. It exists so a hung
// server fails the turn instead of wedging a spinner forever. A variable
// only so tests can exercise the watchdog without waiting minutes.
var streamIdleTimeout = 5 * time.Minute

// ErrStreamIdle reports that an accepted provider stream stopped producing
// data for longer than the idle watchdog allows. It is retryable.
var ErrStreamIdle = errors.New("provider stream produced no data within the idle timeout")

// IsRetryableTransport reports whether err looks like a transient provider
// transport failure (connection loss, truncated stream, server-side 5xx, a
// request timeout, or an idle-stream watchdog) that a caller may retry
// immediately with the same request. Deliberate cancellation and admission
// deadlines are never retryable.
//
// A 429 (rate limit / quota exhaustion) is deliberately NOT included here,
// even though it is sometimes eventually retryable: unlike a 5xx or a
// dropped connection, resending immediately will not resolve it and only
// burns another billable request against an already-exhausted quota. Callers
// that want to retry a 429 must do so through IsRetryableProviderError, which
// forces them to consider ProviderRetryAfter (or their own backoff) instead
// of treating it the same as an ordinary transport hiccup.
func IsRetryableTransport(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrStreamIdle) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if status, ok := ProviderHTTPStatus(err); ok {
		return status >= http.StatusInternalServerError || status == http.StatusRequestTimeout
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// OllamaClient is a deliberately small HTTP adapter for the local Ollama API.
// Keeping the wire client here avoids linking Ollama's server/runtime graph
// (GPU runners, archives, SSH helpers, and registry code) into this CLI.
type OllamaClient struct {
	httpClient *http.Client
	base       *url.URL
	model      string
	numCtx     int
	baseURL    string
	authHeader string
}

type ollamaMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	Images     [][]byte         `json:"images,omitempty"`
	Thinking   string           `json:"thinking,omitempty"`
	ToolCalls  []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName   string           `json:"tool_name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type ollamaToolCall struct {
	ID       string                 `json:"id,omitempty"`
	Function ollamaToolCallFunction `json:"function"`
}

type ollamaToolCallFunction struct {
	Index     int            `json:"index"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ollamaTool struct {
	Type     string             `json:"type"`
	Function ollamaToolFunction `json:"function"`
}

type ollamaToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
	Think    *bool           `json:"think,omitempty"`
}

type ollamaChatResponse struct {
	Message         ollamaMessage `json:"message"`
	Done            bool          `json:"done"`
	DoneReason      string        `json:"done_reason,omitempty"`
	EvalCount       int           `json:"eval_count,omitempty"`
	PromptEvalCount int           `json:"prompt_eval_count,omitempty"`
	// Durations are nanoseconds, exactly as Ollama's /api/chat reports them.
	TotalDuration      int64  `json:"total_duration,omitempty"`
	LoadDuration       int64  `json:"load_duration,omitempty"`
	PromptEvalDuration int64  `json:"prompt_eval_duration,omitempty"`
	EvalDuration       int64  `json:"eval_duration,omitempty"`
	Error              string `json:"error,omitempty"`
}

type ollamaListResponse struct {
	Models []ollamaListModel `json:"models"`
}

type ollamaListModel struct {
	Name         string             `json:"name"`
	Model        string             `json:"model"`
	RemoteModel  string             `json:"remote_model,omitempty"`
	RemoteHost   string             `json:"remote_host,omitempty"`
	Size         int64              `json:"size"`
	Digest       string             `json:"digest,omitempty"`
	ModifiedAt   time.Time          `json:"modified_at,omitempty"`
	Details      ollamaModelDetails `json:"details,omitempty"`
	Capabilities []string           `json:"capabilities,omitempty"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

type ollamaErrorResponse struct {
	Error string `json:"error"`
}

type ollamaHTTPError struct {
	StatusCode int
	Status     string
	Message    string
	// RetryAfter is the provider's Retry-After response header, parsed to a
	// duration. See openAIHTTPError.RetryAfter for the exact zero-value
	// contract.
	RetryAfter time.Duration
}

func (e *ollamaHTTPError) Error() string {
	if e.Message == "" {
		return e.Status
	}
	return fmt.Sprintf("%s: %s", e.Status, e.Message)
}

// NewOllamaClient creates a new Ollama client without mutating process-wide
// environment state. baseURL wins over OLLAMA_HOST; otherwise the loopback
// default used by Ollama is selected.
func NewOllamaClient(baseURL, model string, numCtx int) (*OllamaClient, error) {
	resolved := strings.TrimSpace(baseURL)
	if resolved == "" {
		resolved = strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	}
	if resolved == "" {
		resolved = "http://127.0.0.1:11434"
	}

	u, err := normalizeOllamaURL(resolved)
	if err != nil {
		return nil, fmt.Errorf("invalid ollama base url %q: %w", resolved, err)
	}
	client := newOllamaHTTPClient(u)
	authHeader := ""
	if u.Scheme == "https" && !isLocalOllamaHost(u.Hostname()) {
		if apiKey := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY")); apiKey != "" {
			authHeader = "Bearer " + apiKey
		}
	}
	return &OllamaClient{
		httpClient: client,
		base:       u,
		model:      model,
		numCtx:     numCtx,
		baseURL:    strings.TrimSuffix(u.String(), "/"),
		authHeader: authHeader,
	}, nil
}

func newOllamaHTTPClient(base *url.URL) *http.Client {
	dialer := &net.Dialer{}
	return newOllamaHTTPClientWithNetwork(base, net.DefaultResolver, dialer.DialContext)
}

func newOllamaHTTPClientWithNetwork(base *url.URL, resolver netpolicy.IPResolver, dial netpolicy.DialContextFunc) *http.Client {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	}
	// A loopback inference request must not be handed to HTTP_PROXY. This is
	// both faster and preserves the local-only boundary for localhost aliases.
	if isLocalOllamaHost(base.Hostname()) {
		transport.Proxy = nil
		transport.DialContext = netpolicy.LocalOnlyDialContext(resolver, dial, "Ollama")
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
				return fmt.Errorf("refusing cross-origin Ollama redirect to %s", req.URL.Redacted())
			}
			return nil
		},
	}
}

func isLocalOllamaHost(host string) bool {
	return netpolicy.IsLocalHost(host)
}

func (o *OllamaClient) Model() string { return o.model }

// Ping checks Ollama is running and the model exists.
func (o *OllamaClient) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	return o.PingContext(ctx)
}

// PingContext checks model availability within the caller's cancellation and
// deadline rather than creating an unrelated background operation.
func (o *OllamaClient) PingContext(ctx context.Context) error {
	var response struct{}
	if err := o.doJSON(ctx, http.MethodPost, "/api/show", map[string]any{"model": o.model}, &response); err != nil {
		return fmt.Errorf("model %q not available: %w", o.model, err)
	}
	return nil
}

// Unload asks Ollama to evict this model immediately. It is used before an
// exclusive profile switch so two chat-model weight sets do not overlap in
// unified memory.
func (o *OllamaClient) Unload(ctx context.Context) error {
	return o.doJSON(ctx, http.MethodPost, "/api/generate", map[string]any{
		"model":      o.model,
		"prompt":     "",
		"stream":     false,
		"keep_alive": "0s",
	}, nil)
}

// ChatStream sends a chat request and streams the response via callback.
func (o *OllamaClient) ChatStream(ctx context.Context, opts ChatOptions, fn func(StreamChunk) error) error {
	if fn == nil {
		return errors.New("ollama stream callback is nil")
	}
	if opts.NumThread < 0 {
		return errors.New("ollama num_thread cannot be negative")
	}

	messages := make([]ollamaMessage, 0, len(opts.Messages)+1)
	if opts.System != "" {
		messages = append(messages, ollamaMessage{Role: "system", Content: opts.System})
	}
	for _, message := range opts.Messages {
		converted := ollamaMessage{
			Role:       message.Role,
			Content:    message.Content,
			ToolName:   message.ToolName,
			ToolCallID: message.ToolCallID,
		}
		for imageIndex, image := range message.Images {
			if err := image.Validate(); err != nil {
				// Validation occurs before request construction, so downstream
				// execution state must not treat this as an uncertain dispatch.
				return inferenceNotStarted(fmt.Errorf("ollama message image %d: %w", imageIndex, err))
			}
			// Keep provider request ownership independent from the caller. JSON's
			// []byte encoding emits the base64 strings required by /api/chat.
			converted.Images = append(converted.Images, append([]byte(nil), image.Data...))
		}
		for index, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, ollamaToolCall{
				ID: call.ID,
				Function: ollamaToolCallFunction{
					Index:     index,
					Name:      call.Name,
					Arguments: call.Arguments,
				},
			})
		}
		messages = append(messages, converted)
	}

	options := make(map[string]any, 3)
	if o.numCtx > 0 {
		options["num_ctx"] = o.numCtx
	}
	if opts.MaxEvalTokens > 0 {
		options["num_predict"] = opts.MaxEvalTokens
	}
	if opts.NumThread > 0 {
		options["num_thread"] = opts.NumThread
	}
	request := ollamaChatRequest{
		Model:    o.model,
		Messages: messages,
		Stream:   true,
		Tools:    convertTools(opts.Tools),
		Options:  options,
	}
	if opts.DisableReasoning {
		disabled := false
		request.Think = &disabled
	}

	sawDone := false
	requestStart := time.Now()
	var firstTokenAt time.Time
	err := o.streamJSON(ctx, "/api/chat", request, func(record []byte) error {
		var response ollamaChatResponse
		if err := json.Unmarshal(record, &response); err != nil {
			return fmt.Errorf("decode Ollama chat response: %w", err)
		}
		if response.Error != "" {
			return errors.New(response.Error)
		}
		if response.Done {
			sawDone = true
		}
		if firstTokenAt.IsZero() && (response.Message.Content != "" || response.Message.Thinking != "") {
			firstTokenAt = time.Now()
		}

		chunk := StreamChunk{
			Text:      response.Message.Content,
			Reasoning: response.Message.Thinking,
			Done:      response.Done,
		}
		if response.Done {
			chunk.EvalCount = response.EvalCount
			chunk.PromptEvalCount = response.PromptEvalCount
			chunk.FinishReason = response.DoneReason
			timing := ProviderTiming{
				TotalDuration:      time.Duration(response.TotalDuration),
				LoadDuration:       time.Duration(response.LoadDuration),
				PromptEvalDuration: time.Duration(response.PromptEvalDuration),
				EvalDuration:       time.Duration(response.EvalDuration),
			}
			if !firstTokenAt.IsZero() {
				timing.TimeToFirstToken = firstTokenAt.Sub(requestStart)
			}
			if timing != (ProviderTiming{}) {
				chunk.Timing = &timing
			}
		}
		for _, call := range response.Message.ToolCalls {
			chunk.ToolCalls = append(chunk.ToolCalls, ToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
		return fn(chunk)
	})
	if err != nil {
		return o.markRemoteFailure(err)
	}
	if !sawDone {
		return o.markRemoteFailure(fmt.Errorf("ollama chat stream ended before done: %w", io.ErrUnexpectedEOF))
	}
	return nil
}

// markRemoteFailure applies the remote-inference redaction contract to a
// dispatched request that carried a credential.
//
// Ollama Cloud is a credentialed remote HTTPS provider that happens to be
// implemented as a variant of this client rather than as its own type, so
// classifying provenance by concrete client type left it on the local branch —
// the one that deliberately prints the raw error, which ollamaStatusError
// builds straight from the remote HTTP response body, into the transcript and
// from there into the durable session snapshot. The credential is the
// discriminator that survives that implementation detail: it is set only for
// an HTTPS host that is not local, which is exactly the population whose
// failure text is somebody else's server talking.
//
// A genuinely local Ollama sends no Authorization header and keeps its raw
// diagnostics, which are actionable and are meant to be shown.
//
// Only ChatStream needs this. It is the sole Ollama entry point whose error
// crosses a transcript or durable-session boundary; Ping, Embed, and the
// inventory calls report through host-authored copy that never carries the
// provider body.
func (o *OllamaClient) markRemoteFailure(err error) error {
	if o.authHeader == "" {
		return err
	}
	return markRemoteInferenceError(err)
}

// Embed generates embeddings for the given texts using the specified model.
func (o *OllamaClient) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	var response ollamaEmbedResponse
	if err := o.doJSON(ctx, http.MethodPost, "/api/embed", map[string]any{
		"model": model,
		"input": texts,
	}, &response); err != nil {
		return nil, fmt.Errorf("embedding failed: %w", err)
	}
	return response.Embeddings, nil
}

func (o *OllamaClient) listModels(ctx context.Context) ([]ollamaListModel, error) {
	var response ollamaListResponse
	if err := o.doJSON(ctx, http.MethodGet, "/api/tags", nil, &response); err != nil {
		return nil, err
	}
	return response.Models, nil
}

// convertTools transforms local tool definitions into Ollama's documented
// JSON wire shape. Parameters stay as generic JSON Schema, preserving nested
// items, anyOf, $defs, multi-type declarations, and future schema keywords.
func convertTools(defs []ToolDef) []ollamaTool {
	if len(defs) == 0 {
		return nil
	}

	tools := make([]ollamaTool, 0, len(defs))
	for _, definition := range defs {
		tools = append(tools, ollamaTool{
			Type: "function",
			Function: ollamaToolFunction{
				Name:        definition.Name,
				Description: definition.Description,
				Parameters:  convertToolParameters(definition.Parameters),
			},
		})
	}
	return tools
}

func convertToolParameters(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	parameters := make(map[string]any, len(schema)+2)
	for key, value := range schema {
		parameters[key] = value
	}
	if value, ok := parameters["type"].(string); !ok || value == "" {
		parameters["type"] = "object"
	}
	if _, ok := parameters["properties"]; !ok {
		parameters["properties"] = map[string]any{}
	}
	return parameters
}

func (o *OllamaClient) doJSON(ctx context.Context, method, route string, payload, output any) error {
	request, err := o.newRequest(ctx, method, route, payload, "application/json")
	if err != nil {
		return err
	}
	response, err := o.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer func() {
		// The bounded read below determines the operation result. Closing a
		// fully consumed response body cannot make that response unsuccessful.
		_ = response.Body.Close()
	}()

	limit := int64(maxOllamaResponseBytes)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		limit = maxOllamaErrorBytes
	}
	body, err := readBoundedBody(response.Body, limit)
	if err != nil {
		return fmt.Errorf("read Ollama response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return ollamaStatusError(response, body)
	}
	if output == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("decode Ollama response: %w", err)
	}
	return nil
}

func (o *OllamaClient) streamJSON(ctx context.Context, route string, payload any, fn func([]byte) error) error {
	// The idle watchdog turns a silently hung stream into a typed, retryable
	// failure. It cancels the request context, so the blocked read below
	// returns instead of holding the turn (and its spinner) open forever.
	streamCtx, cancelStream := context.WithCancelCause(ctx)
	defer cancelStream(nil)
	idle := time.AfterFunc(streamIdleTimeout, func() {
		cancelStream(fmt.Errorf("%w (%s)", ErrStreamIdle, streamIdleTimeout))
	})
	defer idle.Stop()
	streamCause := func(err error) error {
		if cause := context.Cause(streamCtx); errors.Is(cause, ErrStreamIdle) && ctx.Err() == nil {
			return cause
		}
		return err
	}

	request, err := o.newRequest(streamCtx, http.MethodPost, route, payload, "application/x-ndjson")
	if err != nil {
		return err
	}
	response, err := o.httpClient.Do(request)
	if err != nil {
		return streamCause(err)
	}
	defer func() {
		// Stream completion/error is established while reading; a later close
		// error carries no additional provider outcome information.
		_ = response.Body.Close()
	}()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, readErr := readBoundedBody(response.Body, maxOllamaErrorBytes)
		if readErr != nil {
			return fmt.Errorf("read Ollama error response: %w", streamCause(readErr))
		}
		return ollamaStatusError(response, body)
	}

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), maxOllamaStreamRecordBytes)
	for scanner.Scan() {
		idle.Reset(streamIdleTimeout)
		record := bytes.TrimSpace(scanner.Bytes())
		if len(record) == 0 {
			continue
		}
		var errorEnvelope ollamaErrorResponse
		if err := json.Unmarshal(record, &errorEnvelope); err != nil {
			return fmt.Errorf("decode Ollama stream record: %w", err)
		}
		if errorEnvelope.Error != "" {
			return errors.New(errorEnvelope.Error)
		}
		if err := fn(record); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Ollama stream: %w", streamCause(err))
	}
	return nil
}

func (o *OllamaClient) newRequest(ctx context.Context, method, route string, payload any, accept string) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode Ollama request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	endpoint := o.base.ResolveReference(&url.URL{Path: route})
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("User-Agent", "sonar/ollama-wire")
	if o.authHeader != "" {
		request.Header.Set("Authorization", o.authHeader)
	}
	return request, nil
}

func readBoundedBody(reader io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d-byte limit", maxBytes)
	}
	return data, nil
}

func ollamaStatusError(response *http.Response, body []byte) error {
	message := strings.TrimSpace(string(body))
	var envelope ollamaErrorResponse
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != "" {
		message = envelope.Error
	}
	retryAfter, _ := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
	return &ollamaHTTPError{
		StatusCode: response.StatusCode,
		Status:     response.Status,
		Message:    message,
		RetryAfter: retryAfter,
	}
}

// BaseURL returns the configured Ollama base URL for display.
func (o *OllamaClient) BaseURL() string { return o.baseURL }

// ParseBaseURL validates and normalizes an Ollama URL.
func ParseBaseURL(rawURL string) (*url.URL, error) { return normalizeOllamaURL(rawURL) }

// normalizeOllamaURL accepts the same lenient host forms as OLLAMA_HOST and
// returns a simple HTTP(S) origin. A missing scheme defaults to HTTP and a
// missing HTTP port to Ollama's default 11434.
func normalizeOllamaURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("missing host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("userinfo, query strings, and fragments are not allowed")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("base URL path is not allowed")
	}
	if strings.ContainsAny(u.Hostname(), " \t\r\n") {
		return nil, errors.New("invalid host")
	}
	if u.Scheme == "http" && u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "11434")
	}
	u.Path = ""
	return u, nil
}
