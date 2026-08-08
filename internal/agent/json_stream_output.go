package agent

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// maxStreamEventBytes bounds one event's text or message payload. Chunks are
// ordinarily far smaller; the cap keeps a pathological provider line from
// turning the stream into a memory problem for whoever tails it.
const maxStreamEventBytes = 16 * 1024

// streamEvent is one JSONL progress line of a --json-stream run. Every line
// carries `event`; the final line of the process is the ordinary turn receipt
// (distinguished by its `schema` field), so a consumer that only wants the
// settled outcome reads the last line and gets exactly the --json document.
//
// Tool events carry the same bounded projection as the receipt — name, kind,
// status, duration — and deliberately no arguments or results: the stream
// crosses a process boundary and stays presentation data, never authority.
type streamEvent struct {
	Event        string `json:"event"`
	Text         string `json:"text,omitempty"`
	Message      string `json:"message,omitempty"`
	CallID       string `json:"call_id,omitempty"`
	Name         string `json:"name,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Status       string `json:"status,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	EvalTokens   int64  `json:"eval_tokens,omitempty"`
	PromptTokens int64  `json:"prompt_tokens,omitempty"`
}

// StreamJSONOutput implements Output for --json-stream headless runs: JSONL
// progress events on stdout while the turn runs, sharing JSONOutput's receipt
// accumulator so the process still ends with the exact --json receipt line.
// Stderr diagnostics keep flowing exactly as in plain headless mode.
type StreamJSONOutput struct {
	*JSONOutput

	emitMu sync.Mutex
	events io.Writer
}

// NewStreamJSONOutput creates a StreamJSONOutput emitting events to w and
// diagnostics to os.Stderr.
func NewStreamJSONOutput(w io.Writer) *StreamJSONOutput {
	return &StreamJSONOutput{JSONOutput: newJSONOutput(os.Stderr), events: w}
}

// newStreamJSONOutput creates a StreamJSONOutput with custom sinks (tests).
func newStreamJSONOutput(events, stderr io.Writer) *StreamJSONOutput {
	return &StreamJSONOutput{JSONOutput: newJSONOutput(stderr), events: events}
}

// emit writes one newline-terminated event. A write failure is swallowed on
// purpose: the tailing parent may be gone, and a headless run must finish its
// work (and durable persistence) whether or not anyone is still watching.
func (s *StreamJSONOutput) emit(event streamEvent) {
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	s.emitMu.Lock()
	_, _ = s.events.Write(append(encoded, '\n'))
	s.emitMu.Unlock()
}

func (s *StreamJSONOutput) StreamText(text string) {
	s.JSONOutput.StreamText(text)
	if text == "" {
		return
	}
	s.emit(streamEvent{Event: "text", Text: truncateApprovalUTF8(text, maxStreamEventBytes)})
}

func (s *StreamJSONOutput) StreamDone(evalCount, promptTokens int) {
	s.JSONOutput.StreamDone(evalCount, promptTokens)
	s.emit(streamEvent{Event: "usage", EvalTokens: int64(evalCount), PromptTokens: int64(promptTokens)})
}

func (s *StreamJSONOutput) ToolCallStart(callID, name string, args map[string]any) {
	s.JSONOutput.ToolCallStart(callID, name, args)
	s.emit(streamEvent{Event: "tool_start", CallID: callID, Name: name, Kind: toolKindForName(name)})
}

func (s *StreamJSONOutput) ToolCallResult(callID, name string, result string, isError bool, duration time.Duration) {
	s.JSONOutput.ToolCallResult(callID, name, result, isError, duration)
	status := "ok"
	if isError {
		status = "error"
	}
	s.emit(streamEvent{
		Event: "tool_result", CallID: callID, Name: name, Kind: toolKindForName(name),
		Status: status, DurationMS: duration.Milliseconds(),
	})
}

func (s *StreamJSONOutput) SystemMessage(msg string) {
	s.JSONOutput.SystemMessage(msg)
	s.emit(streamEvent{Event: "system", Message: truncateApprovalUTF8(msg, maxStreamEventBytes)})
}

func (s *StreamJSONOutput) Error(msg string) {
	s.JSONOutput.Error(msg)
	s.emit(streamEvent{Event: "error", Message: truncateApprovalUTF8(msg, maxStreamEventBytes)})
}
