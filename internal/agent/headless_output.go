package agent

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// HeadlessOutput implements the Output interface for non-interactive / pipe mode.
// Text is written to stdout; tool calls, system messages, and errors go to stderr.
type HeadlessOutput struct {
	stdout        io.Writer
	stderr        io.Writer
	text          strings.Builder
	evalTokens    int64
	promptTokens  int64
	toolCalls     int64
	toolSuccesses int64
}

// NewHeadlessOutput creates a HeadlessOutput that writes text to os.Stdout
// and diagnostics to os.Stderr.
func NewHeadlessOutput() *HeadlessOutput {
	return &HeadlessOutput{
		stdout: os.Stdout,
		stderr: os.Stderr,
	}
}

// newHeadlessOutput creates a HeadlessOutput with custom writers (for testing).
func newHeadlessOutput(stdout, stderr io.Writer) *HeadlessOutput {
	return &HeadlessOutput{
		stdout: stdout,
		stderr: stderr,
	}
}

// StreamText writes incremental text content to stdout.
func (h *HeadlessOutput) StreamText(text string) {
	h.text.WriteString(text)
	_, _ = fmt.Fprint(h.stdout, text)
}

// StreamReasoning is intentionally omitted from pipe output; stdout remains a
// clean final answer suitable for scripts.
func (h *HeadlessOutput) StreamReasoning(string) {}

// StreamDone writes a trailing newline to ensure output is terminated.
func (h *HeadlessOutput) StreamDone(evalCount, promptTokens int) {
	if evalCount > 0 {
		h.evalTokens += int64(evalCount)
	}
	if promptTokens > 0 {
		h.promptTokens += int64(promptTokens)
	}
	_, _ = fmt.Fprintln(h.stdout)
}

// TokenUsage returns the accumulated provider token accounting so headless
// runs can persist per-turn usage exactly like the TUI.
func (h *HeadlessOutput) TokenUsage() (promptTokens, evalTokens int64) {
	return h.promptTokens, h.evalTokens
}

// ToolCallStart writes a brief tool invocation notice to stderr.
func (h *HeadlessOutput) ToolCallStart(_ string, name string, args map[string]any) {
	h.toolCalls++
	_, _ = fmt.Fprintf(h.stderr, "→ %s %s\n", name, FormatToolArgsForTool(name, args))
}

// GoalTurnStats returns the bounded facts needed to settle a headless goal
// turn. Raw tool results never enter this projection.
func (h *HeadlessOutput) GoalTurnStats() (summary string, evalTokens int64, productive bool) {
	switch {
	case h.toolSuccesses > 0:
		summary = fmt.Sprintf("settled %d tool call(s), %d successful", h.toolCalls, h.toolSuccesses)
	case h.toolCalls > 0:
		summary = fmt.Sprintf("settled %d tool call(s) without a successful receipt", h.toolCalls)
	case strings.TrimSpace(h.text.String()) != "":
		summary = "assistant yielded without a concrete tool receipt"
	default:
		summary = "turn yielded without concrete progress"
	}
	return summary, h.evalTokens, h.toolSuccesses > 0
}

// ToolCallResult writes the tool result summary to stderr.
func (h *HeadlessOutput) ToolCallResult(_ string, name string, result string, isError bool, duration time.Duration) {
	status := "ok"
	if isError {
		status = "ERROR"
	} else {
		h.toolSuccesses++
	}
	// Truncate long results for stderr display.
	display := result
	if len(display) > 200 {
		display = display[:197] + "..."
	}
	_, _ = fmt.Fprintf(h.stderr, "← %s [%s %s] %s\n", name, status, duration.Round(time.Millisecond), display)
}

// SystemMessage writes a system message to stderr.
func (h *HeadlessOutput) SystemMessage(msg string) {
	_, _ = fmt.Fprintf(h.stderr, "[system] %s\n", msg)
}

// CapabilityRoute writes an advisory diagnostic without contaminating stdout.
func (h *HeadlessOutput) CapabilityRoute(route CapabilityRoute) {
	freshness := route.Freshness
	if freshness == "" {
		freshness = CapabilityRouteFreshnessUnknown
	}
	reconsidered := ""
	if route.Reconsidered {
		reconsidered = ", reconsidered"
	}
	switch route.Status {
	case CapabilityRouteResolved:
		_, _ = fmt.Fprintf(
			h.stderr, "[capability] %s resolved -> %s__%s (%s%s advisory)\n",
			route.Phase, route.Server, route.Tool, freshness, reconsidered,
		)
	case CapabilityRouteAmbiguous:
		_, _ = fmt.Fprintf(
			h.stderr, "[capability] %s ambiguous (%d candidates; %s%s advisory)\n",
			route.Phase, route.CandidateCount, freshness, reconsidered,
		)
	default:
		_, _ = fmt.Fprintf(
			h.stderr, "[capability] %s %s (%s%s advisory)\n",
			route.Phase, route.Status, freshness, reconsidered,
		)
	}
}

// Error writes an error message to stderr.
func (h *HeadlessOutput) Error(msg string) {
	_, _ = fmt.Fprintf(h.stderr, "[error] %s\n", msg)
}
