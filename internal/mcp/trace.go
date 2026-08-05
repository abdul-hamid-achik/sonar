package mcp

import (
	"context"
	"time"

	"github.com/charmbracelet/log"
)

// This file is the session log's MCP trace.
//
// MCP dispatch produced no log output at all, which made a whole class of
// session question unanswerable after the fact: which server actually served a
// tool, whether a call reached the server or died locally on a deadline, and
// whether a call that "worked" returned a domain error.
//
// The trace deliberately reports transport, domain, and evidence as separate
// facts, because they are independent and conflating them is the standard way
// to misread an MCP session. A call can complete over the wire (transport ok)
// and still carry IsError (domain failure); it can carry neither and still
// return no structured payload to verify anything against. A single "ok" field
// would erase exactly the distinction a reader needs.
//
// Nothing here prints tool arguments or result content. Argument maps carry
// whatever the model chose to send, and result content is remote text; both
// belong behind the parser boundary in internal/ecosystem, not in a log line.
// Sizes and flags describe the shape of a payload without reproducing it.

type callCorrelationKey struct{}

// WithCallCorrelation tags a dispatch with the host's execution identity so an
// MCP line can be joined to the agent's tool lifecycle trace. Without it the
// two logs can only be lined up by tool name and timing, which stops working
// exactly when it matters — concurrent health checks, retries, or the same
// tool called twice in one turn.
func WithCallCorrelation(ctx context.Context, executionID string) context.Context {
	if ctx == nil || executionID == "" {
		return ctx
	}
	return context.WithValue(ctx, callCorrelationKey{}, executionID)
}

func callCorrelation(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(callCorrelationKey{}).(string)
	return id
}

// SetLogger installs the session logger. Safe to leave unset; every trace call
// is nil-guarded, and an embedding without a logger behaves identically.
func (r *Registry) SetLogger(logger *log.Logger) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.logger = logger
	r.mu.Unlock()
}

func (r *Registry) traceLogger() *log.Logger {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.logger
}

// traceCallOutcome writes one dispatched MCP call.
//
// transportErr is a failure to complete the exchange: the call may or may not
// have run on the server, which is why a dispatched mutation in this state is
// outcome-unknown rather than safely retryable. A result with IsError set is
// the opposite — the server was reached, understood the call, and refused it.
func traceCallOutcome(
	logger *log.Logger,
	executionID, name, server, remoteName string,
	elapsed time.Duration,
	result *ToolResult,
	transportErr error,
) {
	if logger == nil {
		return
	}
	fields := []any{
		"tool", name,
		"server", server,
		"remote", remoteName,
		"ms", elapsed.Milliseconds(),
	}
	if executionID != "" {
		fields = append(fields, "execution", executionID)
	}

	if transportErr != nil {
		logger.Error("mcp call did not complete",
			append(fields, "transport", "failed", "err", transportErr)...)
		return
	}
	if result == nil {
		// A nil result with no error is not a shape any caller can act on, and
		// silently logging it as success would hide a client bug.
		logger.Error("mcp call returned neither a result nor an error", fields...)
		return
	}

	fields = append(fields,
		"transport", "ok",
		"content_bytes", len(result.Content),
		"structured", len(result.Structured) > 0,
	)
	if result.IsError {
		logger.Warn("mcp call refused by the server", append(fields, "domain", "error")...)
		return
	}
	logger.Debug("mcp call completed", append(fields, "domain", "ok")...)
}
