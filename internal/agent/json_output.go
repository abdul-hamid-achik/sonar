package agent

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
)

// maxReceiptToolCalls bounds the receipt document. Long AUTO turns can settle
// hundreds of calls; the ledger keeps the complete record, the receipt keeps a
// bounded projection plus an omitted counter.
const maxReceiptToolCalls = 200

// JSONOutput implements Output for --json headless runs. Stdout stays silent
// while the turn streams so the process can end with exactly one
// machine-readable turn-receipt document; human-readable diagnostics keep
// flowing to stderr exactly as in plain headless mode.
type JSONOutput struct {
	inner *HeadlessOutput

	mu               sync.Mutex
	promptTokens     int64
	evalTokens       int64
	toolCalls        []TurnReceiptToolCall
	toolCallsOmitted int
	timing           llm.ProviderTiming
	timingSeen       bool
	truncated        bool
}

// NewJSONOutput creates a JSONOutput with diagnostics on os.Stderr.
func NewJSONOutput() *JSONOutput {
	return newJSONOutput(os.Stderr)
}

// newJSONOutput creates a JSONOutput with a custom stderr (for testing).
func newJSONOutput(stderr io.Writer) *JSONOutput {
	return &JSONOutput{inner: newHeadlessOutput(io.Discard, stderr)}
}

// StreamText accumulates the final answer without touching stdout.
func (j *JSONOutput) StreamText(text string) { j.inner.StreamText(text) }

// StreamReasoning is omitted from receipts; reasoning is provider-transient.
func (j *JSONOutput) StreamReasoning(string) {}

// StreamDone accumulates provider usage across every request in the turn,
// including conservative fail-closed reservations reported by the host.
func (j *JSONOutput) StreamDone(evalCount, promptTokens int) {
	j.mu.Lock()
	if evalCount > 0 {
		j.evalTokens += int64(evalCount)
	}
	if promptTokens > 0 {
		j.promptTokens += int64(promptTokens)
	}
	j.mu.Unlock()
	j.inner.StreamDone(evalCount, promptTokens)
}

// ToolCallStart forwards the stderr progress notice.
func (j *JSONOutput) ToolCallStart(callID, name string, args map[string]any) {
	j.inner.ToolCallStart(callID, name, args)
}

// ToolCallResult records one bounded settled-call entry for the receipt.
func (j *JSONOutput) ToolCallResult(callID, name string, result string, isError bool, duration time.Duration) {
	status := "ok"
	if isError {
		status = "error"
	}
	j.mu.Lock()
	if len(j.toolCalls) < maxReceiptToolCalls {
		j.toolCalls = append(j.toolCalls, TurnReceiptToolCall{
			CallID:     callID,
			Name:       name,
			Kind:       toolKindForName(name),
			Status:     status,
			DurationMS: duration.Milliseconds(),
		})
	} else {
		j.toolCallsOmitted++
	}
	j.mu.Unlock()
	j.inner.ToolCallResult(callID, name, result, isError, duration)
}

// SystemMessage forwards to stderr.
func (j *JSONOutput) SystemMessage(msg string) { j.inner.SystemMessage(msg) }

// Error forwards to stderr; terminal errors also reach the receipt through
// the caller's TurnOutcome mapping.
func (j *JSONOutput) Error(msg string) { j.inner.Error(msg) }

// CapabilityRoute keeps the advisory diagnostic on stderr.
func (j *JSONOutput) CapabilityRoute(route CapabilityRoute) { j.inner.CapabilityRoute(route) }

// GoalTurnStats preserves the headless goal settlement projection.
func (j *JSONOutput) GoalTurnStats() (summary string, evalTokens int64, productive bool) {
	return j.inner.GoalTurnStats()
}

// TokenUsage returns the accumulated provider token accounting.
func (j *JSONOutput) TokenUsage() (promptTokens, evalTokens int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.promptTokens, j.evalTokens
}

// ProviderReceipt accumulates terminal provider receipts across the turn's
// iterations: the first measured time-to-first-token, summed durations, and
// whether any generation was truncated at its token ceiling.
func (j *JSONOutput) ProviderReceipt(finishReason string, timing *llm.ProviderTiming) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if finishReason == "length" {
		j.truncated = true
	}
	if timing == nil {
		return
	}
	j.timingSeen = true
	if j.timing.TimeToFirstToken == 0 && timing.TimeToFirstToken > 0 {
		j.timing.TimeToFirstToken = timing.TimeToFirstToken
	}
	j.timing.TotalDuration += timing.TotalDuration
	j.timing.LoadDuration += timing.LoadDuration
	j.timing.PromptEvalDuration += timing.PromptEvalDuration
	j.timing.EvalDuration += timing.EvalDuration
}

// ComposeReceipt merges the streamed observations (usage, tool calls, final
// text) into a caller-assembled receipt frame.
func (j *JSONOutput) ComposeReceipt(base TurnReceipt) TurnReceipt {
	j.mu.Lock()
	base.Usage = TurnReceiptUsage{PromptTokens: j.promptTokens, EvalTokens: j.evalTokens}
	base.ToolCalls = append([]TurnReceiptToolCall(nil), j.toolCalls...)
	base.ToolCallsOmitted = j.toolCallsOmitted
	base.Truncated = j.truncated
	if j.timingSeen {
		base.Timing = &TurnReceiptTiming{
			TTFTMS:       j.timing.TimeToFirstToken.Milliseconds(),
			LoadMS:       j.timing.LoadDuration.Milliseconds(),
			PromptEvalMS: j.timing.PromptEvalDuration.Milliseconds(),
			EvalMS:       j.timing.EvalDuration.Milliseconds(),
			TotalMS:      j.timing.TotalDuration.Milliseconds(),
		}
	}
	j.mu.Unlock()
	base.Text = strings.TrimRight(j.inner.text.String(), "\n")
	return base
}

// toolKindForName classifies a tool by the host's naming contract: memory
// built-ins have exact reserved names, MCP tools are namespaced server__tool,
// and everything else the agent dispatches is a local built-in.
func toolKindForName(name string) string {
	switch {
	case memory.IsBuiltinTool(name):
		return "memory"
	case strings.Contains(name, "__"):
		return "mcp"
	default:
		return "builtin"
	}
}
