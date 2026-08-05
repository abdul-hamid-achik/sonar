// Package openaiwire writes the OpenAI-compatible streaming shapes the
// Glyphrun fixtures need, so each fixture carries only its own scripted
// conversation.
//
// It exists because sonar stopped speaking a second protocol. Every fixture
// used to fake Ollama's native /api/chat, and each carried its own copy of the
// NDJSON writers; when `ollama` became a hosted OpenAI-compatible provider,
// all of those copies became unreachable at once. Converting them meant either
// writing the SSE frames four more times or writing them here.
package openaiwire

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// FirstTokenDelay is the pause between flushing response headers and emitting
// the first token.
//
// Both halves matter. sonar measures time-to-first-token from the moment
// response headers arrive, so a fixture that slept before flushing would delay
// the clock too and report zero. On loopback the whole exchange is otherwise
// sub-millisecond, and a receipt assertion could not tell a real measurement
// from an unset field.
const FirstTokenDelay = 15 * time.Millisecond

// ChatRequest is the subset of an OpenAI chat request a fixture inspects.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Tools    []Tool    `json:"tools"`
	// ReasoningEffort is how the plain OpenAI dialect switches thinking off:
	// sonar sends "none" for a request that must not reason. DeepSeek uses a
	// separate `thinking` object, which no fixture here needs to inspect.
	ReasoningEffort string `json:"reasoning_effort"`
}

// Tool is an offered function, as a fixture sees it in the request.
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// Message is one conversation entry, including the tool receipts a fixture
// asserts on.
type Message struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	Name       string `json:"name"`
	ToolCallID string `json:"tool_call_id"`
}

// ToolName is the tool a tool-result message answers. OpenAI carries it in
// `name`; the native Ollama protocol these fixtures used to speak had a
// dedicated field, and the accessor keeps the call sites reading the same.
func (m Message) ToolName() string { return m.Name }

// ReasoningDisabled reports whether the request asked for no reasoning.
func (r ChatRequest) ReasoningDisabled() bool { return r.ReasoningEffort == "none" }

// WriteModels answers GET /v1/models with a single served model.
func WriteModels(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"data":[{"id":%q}]}`, model)
}

// beginStream sets the streaming headers and flushes them, starting the
// client's time-to-first-token clock before the deliberate pause.
func beginStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	time.Sleep(FirstTokenDelay)
}

func writeFrame(w http.ResponseWriter, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// finish emits the terminal frame carrying the stop reason and the usage
// receipt, then closes the stream. sonar charges a reservation fail-closed
// when a turn ends without usage, so a fixture that omitted it would exercise
// the failure path rather than the one it means to test.
func finish(w http.ResponseWriter, reason string, promptTokens, completionTokens int) {
	writeFrame(w, map[string]any{
		"choices": []map[string]any{{
			"index": 0, "delta": map[string]any{}, "finish_reason": reason,
		}},
		"usage": map[string]any{
			"prompt_tokens": promptTokens, "completion_tokens": completionTokens,
		},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

// WriteText streams one assistant answer and settles the turn.
func WriteText(w http.ResponseWriter, text string) {
	beginStream(w)
	writeFrame(w, map[string]any{
		"choices": []map[string]any{{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": text},
		}},
	})
	finish(w, "stop", 8, 5)
}

// WriteSessionTitle answers the session-title request with a fixed title.
func WriteSessionTitle(w http.ResponseWriter) {
	beginStream(w)
	writeFrame(w, map[string]any{
		"choices": []map[string]any{{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": "Fixture session"},
		}},
	})
	finish(w, "stop", 2, 2)
}

// WriteToolCall streams a single tool call and settles the turn with the
// tool_calls stop reason.
//
// Arguments are a JSON string, not an object: that is the OpenAI contract, and
// it is the one shape difference from Ollama's native protocol that a
// mechanical port gets wrong.
func WriteToolCall(w http.ResponseWriter, id, name string, arguments map[string]any) {
	encodedArgs, err := json.Marshal(arguments)
	if err != nil {
		encodedArgs = []byte("{}")
	}
	beginStream(w)
	writeFrame(w, map[string]any{
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"index": 0, "id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": string(encodedArgs)},
				}},
			},
		}},
	})
	finish(w, "tool_calls", 7, 5)
}

// WriteBashCall is the common case: one bash tool call.
func WriteBashCall(w http.ResponseWriter, id, command string) {
	WriteToolCall(w, id, "bash", map[string]any{"command": command})
}

// WriteError fails the request the way a provider would.
func WriteError(w http.ResponseWriter, message string) {
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"server_error"}}`, message)
}

// HasSuccessfulToolReceipt reports whether the conversation's most recent tool
// message is a successful receipt for the expected call.
//
// The whole conversation arrives on every request, so requiring the expected
// identity to be the *last* tool message is what stops an earlier receipt from
// satisfying a later follow-up.
func HasSuccessfulToolReceipt(request ChatRequest, expectedID, expectedName string) bool {
	lastToolMessage := -1
	matched := -1
	for _, message := range request.Messages {
		if message.Role != "tool" || message.Content == "" {
			continue
		}
		lastToolMessage++
		if message.ToolCallID != expectedID {
			continue
		}
		// OpenAI tool messages carry the call id; the name is optional and
		// omitted by some clients, so it is checked only when present.
		if message.Name != "" && message.Name != expectedName {
			continue
		}
		lower := strings.ToLower(message.Content)
		if !strings.Contains(lower, "denied") && !strings.Contains(lower, "error") &&
			!strings.Contains(lower, "exit status") {
			matched = lastToolMessage
		}
	}
	return lastToolMessage >= 0 && matched == lastToolMessage
}

// HasAnySuccessfulToolReceipt reports whether the conversation's most recent
// tool message is a success, without pinning which call it answers. Use it
// where a fixture only cares that the previous tool settled.
func HasAnySuccessfulToolReceipt(request ChatRequest) bool {
	for index := len(request.Messages) - 1; index >= 0; index-- {
		message := request.Messages[index]
		if message.Role != "tool" || message.Content == "" {
			continue
		}
		lower := strings.ToLower(message.Content)
		return !strings.Contains(lower, "denied") && !strings.Contains(lower, "error") &&
			!strings.Contains(lower, "exit status")
	}
	return false
}

// Stream is an in-progress response for a fixture that needs to emit frames in
// sequence — reasoning, then a pause, then a tool call — rather than one
// canned answer.
type Stream struct{ w http.ResponseWriter }

// Begin flushes the streaming headers, starting the client's
// time-to-first-token clock, and pauses so that clock measures something.
func Begin(w http.ResponseWriter) *Stream {
	beginStream(w)
	return &Stream{w: w}
}

// Reasoning emits a provider-native thinking delta.
func (s *Stream) Reasoning(text string) {
	writeFrame(s.w, map[string]any{
		"choices": []map[string]any{{
			"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": text},
		}},
	})
}

// Text emits an assistant content delta.
func (s *Stream) Text(text string) {
	writeFrame(s.w, map[string]any{
		"choices": []map[string]any{{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": text},
		}},
	})
}

// ToolCall emits a tool call delta without settling the turn.
func (s *Stream) ToolCall(id, name string, arguments map[string]any) {
	encodedArgs, err := json.Marshal(arguments)
	if err != nil {
		encodedArgs = []byte("{}")
	}
	writeFrame(s.w, map[string]any{
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"index": 0, "id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": string(encodedArgs)},
				}},
			},
		}},
	})
}

// Finish settles the turn with a stop reason and the usage receipt.
func (s *Stream) Finish(reason string) { finish(s.w, reason, 7, 5) }
