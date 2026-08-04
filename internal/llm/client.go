package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"path"
	"strings"
	"time"
	"unicode"
)

var (
	// ErrNoModelSelected is a local preflight rejection. No provider request or
	// generation can have started when a Client returns this sentinel.
	ErrNoModelSelected = errors.New("no model selected")

	// ErrInferenceNotStarted identifies a host-side rejection that happened
	// before the provider's inference dispatch. Provider ChatStream errors are
	// intentionally not wrapped with this sentinel because dispatch may already
	// have happened by then.
	ErrInferenceNotStarted = errors.New("inference not started")
)

func inferenceNotStarted(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInferenceNotStarted, err)
}

// remoteInferenceError marks an error produced after a non-Ollama provider
// request crossed the dispatch boundary. Its text remains available to the
// agent for transient diagnostics, while IsRemoteInferenceError lets the agent
// replace that text before crossing a transcript or durable-session boundary.
// The concrete type is private so arbitrary clients cannot forge remote
// provenance and accidentally hide useful local Ollama diagnostics.
type remoteInferenceError struct {
	cause error
}

func (err *remoteInferenceError) Error() string {
	return err.cause.Error()
}

func (err *remoteInferenceError) Unwrap() error {
	return err.cause
}

func markRemoteInferenceError(err error) error {
	if err == nil || IsRemoteInferenceError(err) {
		return err
	}
	return &remoteInferenceError{cause: err}
}

// IsRemoteInferenceError reports whether err carries exact provenance from a
// dispatched OpenAI-compatible inference request. It deliberately does not
// classify Ollama or host-side preflight failures by matching error strings.
func IsRemoteInferenceError(err error) bool {
	var remoteErr *remoteInferenceError
	return errors.As(err, &remoteErr)
}

// Client is the interface for LLM providers.
type Client interface {
	// ChatStream sends messages to the LLM and streams the response.
	// The callback is called for each chunk. Return a non-nil error to abort.
	ChatStream(ctx context.Context, opts ChatOptions, fn func(StreamChunk) error) error

	// Ping checks if the LLM is reachable and the model is available.
	Ping() error

	// Model returns the current model name.
	Model() string

	// Embed generates embeddings for the given texts using the specified model.
	Embed(ctx context.Context, model string, texts []string) ([][]float32, error)
}

// ChatOptions holds parameters for a chat request.
type ChatOptions struct {
	Messages      []Message
	Tools         []ToolDef
	System        string
	MaxEvalTokens int // zero leaves provider generation uncapped
	// DisableReasoning asks providers with a native control to emit only the
	// visible answer. It is used for bounded child-expert reports where private
	// reasoning is neither a valid result nor safe durable content. Providers
	// without such a control may ignore it; callers must still reject a terminal
	// response that contains no visible text.
	DisableReasoning bool
	// NumThread is a host-only local inference cap. Zero leaves the provider
	// default unchanged; positive values are sent as Ollama num_thread.
	NumThread int
	// ExpectedContext pins a host-side context budget to the request. Provider
	// managers use it to reject a turn whose model policy changed after the
	// agent took its budget snapshot. Direct clients may ignore it.
	ExpectedContext int
	// ExpectedModel pins the tokenizer/model identity paired with the prompt
	// estimate. ModelManager validates it atomically before dispatch; direct
	// clients may ignore it.
	ExpectedModel string
}

// Message represents a conversation message.
type Message struct {
	Role    string `json:"role"` // system, user, assistant, tool
	Content string `json:"content"`
	// Images carry path-free, content-addressed metadata across a durable
	// session boundary. Raw Data remains provider-only and is never serialized.
	// Persistence writers must use agent.SanitizeMessagesForPersistence, which
	// drops transient images without a complete durable reference.
	Images []ImageData `json:"images,omitempty"`
	// DurableContent is the bounded replacement for transient tool content when
	// history crosses a persistence or compaction boundary. It is host-only:
	// providers receive Content, while JSON/session/checkpoint writers must call
	// agent.SanitizeMessagesForPersistence before serialization.
	DurableContent string `json:"-"`
	// ReasoningContent is the provider's chain-of-thought for this assistant
	// message. DeepSeek requires it echoed back verbatim on every later request
	// whenever the same message also carries ToolCalls; dropping it makes the
	// API reject the next agent iteration with HTTP 400. Host-only (`json:"-"`)
	// so private reasoning never crosses a session, transcript, or checkpoint
	// boundary.
	ReasoningContent string     `json:"-"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolName         string     `json:"tool_name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	// HostOwned marks a message whose exact contents were validated and
	// authored by the local host. It is deliberately not persisted or sent on
	// the wire: restore code must re-derive the marker from durable state, so a
	// user-authored message cannot forge host authority through JSON history.
	HostOwned bool `json:"-"`
}

// ImageData is one image input for a multimodal model. Its durable fields form
// a path-free content-addressed reference; Data is transient provider input.
// Ollama's native API has no codec allowlist in its wire contract, so this layer
// validates a syntactic image/* media type without inventing a narrower set.
type ImageData struct {
	SHA256    string `json:"sha256,omitempty"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"mime_type,omitempty"`
	Size      int64  `json:"size_bytes,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Data      []byte `json:"-"`
}

// NewImageData validates and copies an image payload for provider transport.
func NewImageData(mediaType string, data []byte) (ImageData, error) {
	canonical, err := validateImageMediaType(mediaType)
	if err != nil {
		return ImageData{}, err
	}
	if len(data) == 0 {
		return ImageData{}, errors.New("image payload is empty")
	}
	return ImageData{
		MediaType: canonical,
		Data:      append([]byte(nil), data...),
	}, nil
}

// NewReferencedImageData creates a complete content-addressed reference and
// retains a defensive copy of the bytes for the current provider request.
// Callers are responsible for placing the same bytes in the resolver's backing
// store before persisting the returned reference.
func NewReferencedImageData(name, mediaType string, width, height int, data []byte) (ImageData, error) {
	image, err := NewImageData(mediaType, data)
	if err != nil {
		return ImageData{}, err
	}
	if width <= 0 || height <= 0 {
		return ImageData{}, errors.New("image dimensions must be positive")
	}
	digest := sha256.Sum256(image.Data)
	image.SHA256 = hex.EncodeToString(digest[:])
	image.Name = SanitizeImageName(name)
	image.Size = int64(len(image.Data))
	image.Width = width
	image.Height = height
	if err := image.ValidateReference(); err != nil {
		return ImageData{}, err
	}
	return image, nil
}

// Validate reports whether the image is safe to send through a provider
// adapter. Provider adapters call this even for struct literals so callers
// cannot bypass NewImageData's admission checks.
func (image ImageData) Validate() error {
	if _, err := validateImageMediaType(image.MediaType); err != nil {
		return err
	}
	if len(image.Data) == 0 {
		return errors.New("image payload is empty")
	}
	if image.hasReferenceMetadata() {
		if err := image.ValidateReference(); err != nil {
			return err
		}
		if err := image.validateReferenceData(); err != nil {
			return err
		}
	}
	return nil
}

// ValidateReference verifies the complete durable, path-free metadata. It does
// not require Data, allowing a restored session to validate before resolving
// the payload.
func (image ImageData) ValidateReference() error {
	canonicalMediaType, err := validateImageMediaType(image.MediaType)
	if err != nil {
		return err
	}
	if image.MediaType != canonicalMediaType {
		return errors.New("image reference media type is not canonical")
	}
	if !durableImageMediaType(image.MediaType) {
		return fmt.Errorf("image reference media type %q is unsupported", image.MediaType)
	}
	if len(image.SHA256) != sha256.Size*2 {
		return errors.New("image reference requires a full SHA-256 digest")
	}
	if _, err := hex.DecodeString(image.SHA256); err != nil || image.SHA256 != strings.ToLower(image.SHA256) {
		return errors.New("image reference SHA-256 must be lowercase hexadecimal")
	}
	if image.Name == "" || image.Name != SanitizeImageName(image.Name) {
		return errors.New("image reference name is not sanitized")
	}
	if image.Size <= 0 || image.Size > 20<<20 {
		return errors.New("image reference size is outside the durable bound")
	}
	if image.Width <= 0 || image.Height <= 0 || image.Width > 16_384 || image.Height > 16_384 {
		return errors.New("image reference dimensions are outside the durable bound")
	}
	if uint64(image.Width)*uint64(image.Height) > 24_000_000 {
		return errors.New("image reference pixel count is outside the durable bound")
	}
	return nil
}

// WithData hydrates a durable reference, verifying its content address and
// size before returning an independent provider-owned value.
func (image ImageData) WithData(data []byte) (ImageData, error) {
	if err := image.ValidateReference(); err != nil {
		return ImageData{}, err
	}
	image.Data = append([]byte(nil), data...)
	if err := image.validateReferenceData(); err != nil {
		return ImageData{}, err
	}
	return image, nil
}

func (image ImageData) validateReferenceData() error {
	if int64(len(image.Data)) != image.Size {
		return fmt.Errorf("resolved image size is %d bytes, want %d", len(image.Data), image.Size)
	}
	digest := sha256.Sum256(image.Data)
	if hex.EncodeToString(digest[:]) != image.SHA256 {
		return errors.New("resolved image does not match its SHA-256 reference")
	}
	return nil
}

func (image ImageData) hasReferenceMetadata() bool {
	return image.SHA256 != "" || image.Name != "" || image.Size != 0 || image.Width != 0 || image.Height != 0
}

// SanitizeImageName removes directory components, control characters, and
// bidirectional-format characters from display metadata. It retains ordinary
// Unicode and punctuation because the bounded name is never used as a path.
func SanitizeImageName(value string) string {
	value = strings.ReplaceAll(value, `\`, "/")
	value = path.Base(value)
	value = strings.TrimSpace(value)
	var sanitized strings.Builder
	for _, char := range value {
		if unsafeImageNameRune(char) {
			continue
		}
		sanitized.WriteRune(char)
	}
	name := strings.Join(strings.Fields(sanitized.String()), " ")
	runes := []rune(name)
	if len(runes) > 120 {
		name = string(runes[:120])
	}
	if name == "" || name == "." || name == ".." {
		return "image"
	}
	return name
}

func unsafeImageNameRune(char rune) bool {
	return char == unicode.ReplacementChar || unicode.IsControl(char) || unicode.In(char, unicode.Bidi_Control)
}

func durableImageMediaType(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/gif":
		return true
	default:
		return false
	}
}

func validateImageMediaType(value string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("invalid image media type: %w", err)
	}
	mediaType = strings.ToLower(mediaType)
	if !strings.HasPrefix(mediaType, "image/") || len(mediaType) == len("image/") {
		return "", fmt.Errorf("media type %q is not an image", value)
	}
	return mediaType, nil
}

// ProviderTiming carries the provider-reported request timings plus the
// client-measured time to first streamed token. Durations the provider did
// not report stay zero; consumers must treat zero as "not reported", never
// as "instant".
type ProviderTiming struct {
	TimeToFirstToken   time.Duration
	TotalDuration      time.Duration
	LoadDuration       time.Duration
	PromptEvalDuration time.Duration
	EvalDuration       time.Duration
}

// StreamChunk is a piece of a streaming response.
type StreamChunk struct {
	Text            string     // incremental text content
	Reasoning       string     // provider-native thinking/reasoning delta
	ToolCalls       []ToolCall // tool calls (usually in final chunk)
	Done            bool       // true on the last chunk
	EvalCount       int        // tokens generated (only on Done)
	PromptEvalCount int        // prompt tokens evaluated (only on Done)
	// FinishReason is the provider's terminal reason (only on Done): "stop",
	// "tool_calls", or "length" for a truncated generation. Empty when the
	// provider did not report one — a truncated response must therefore be
	// detected by "length", never by the absence of "stop".
	FinishReason string
	// Timing is attached to the terminal chunk when any timing fact is known.
	Timing *ProviderTiming
}

// ToolCall represents a tool invocation requested by the LLM.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// ToolDef defines a tool the LLM can call.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema
	// DisplayName and Behavior are host-only MCP presentation metadata.
	// They must never be sent to model providers as part of a tool schema.
	DisplayName string       `json:"-"`
	Behavior    ToolBehavior `json:"-"`
}

// ToolBehavior is the bounded presentation projection of standard MCP tool
// annotations. It is untrusted server metadata and must never by itself alter
// authorization, durable effect classification, or recovery semantics.
type ToolBehavior struct {
	Declared    bool
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	OpenWorld   bool
}
