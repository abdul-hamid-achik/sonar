package agent

import (
	"strings"
	"unicode/utf8"

	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/charmbracelet/log"
)

// This file is the session log's tool trace.
//
// Before it, a whole session produced a handful of lines — mode switches and
// the odd checkpoint — so answering "why did AUTO stop and ask for approval"
// or "why did that edit fail" meant opening the SQLite ledger and writing
// queries by hand. The information was always recorded; it just was not
// readable.
//
// The trace hangs off appendExecutionEvent rather than being sprinkled through
// dispatch, for one reason: that function is the single place every tool
// lifecycle transition already passes through on its way to the durable
// ledger. Logging there cannot drift out of agreement with the durable record,
// and no future call site can forget to log.
//
// What it may print is bounded by the same contract the ledger holds. Event
// carries identity, a type, an approval disposition, argument and result
// digests, and host-authored Detail text. It does not carry arguments, file
// contents, command text, or provider prose, so nothing here needs its own
// redaction rule — the boundary is upstream and this stays on the safe side of
// it by construction.

// traceLevel maps a lifecycle event to the level that makes an ordinary
// session readable at a glance. A completed read is noise when you are chasing
// a denial; a denial or an unknown outcome is never noise.
func traceLevel(eventType executionpkg.EventType) log.Level {
	switch eventType {
	case executionpkg.EventFailed, executionpkg.EventOutcomeUnknown:
		return log.ErrorLevel
	case executionpkg.EventDenied, executionpkg.EventApprovalRequested:
		return log.WarnLevel
	case executionpkg.EventCompleted, executionpkg.EventCancelled:
		return log.InfoLevel
	default:
		return log.DebugLevel
	}
}

// traceExecutionEvent writes one lifecycle transition to the session log.
//
// The identity fields are the correlation keys: turn groups a logical turn,
// execution groups the attempts of one tool call, and iteration/ordinal give
// the position within the turn. Grepping any one of them reconstructs a slice
// of the session without touching the database.
func traceExecutionEvent(logger *log.Logger, event executionpkg.Event, err error) {
	if logger == nil {
		return
	}
	identity := event.Identity
	fields := []any{
		"turn", identity.TurnID,
		"execution", identity.ExecutionID,
		"tool", identity.ToolName,
		"kind", identity.Kind,
		"effect", identity.EffectClass,
		"iter", identity.Iteration,
		"ordinal", identity.Ordinal,
	}
	// ApprovalNotApplicable is the resting value for events that carry no
	// decision; printing it on every line would bury the ones that do.
	if event.Approval != "" && event.Approval != executionpkg.ApprovalNotApplicable {
		fields = append(fields, "approval", event.Approval)
	}
	// The digest is what proves two events describe the same arguments. It is
	// the value that settled whether a classifier had really seen the command
	// on screen, so a short prefix earns its place; the full hash stays in the
	// ledger.
	if len(event.ArgumentsSHA256) >= 12 {
		fields = append(fields, "args", event.ArgumentsSHA256[:12])
	}
	if event.Detail != "" {
		fields = append(fields, "detail", event.Detail)
	}
	// On a failure the receipt is the answer to "why", and its absence is what
	// made `error=true` the least useful line in the file. It is already bound
	// and already persisted by executionEvent, so this narrows an existing
	// durable value rather than exposing a new one — and only for the event
	// where reading it is the point.
	if event.Type == executionpkg.EventFailed && event.ResultReceipt != "" {
		fields = append(fields, "result", traceSnippet(event.ResultReceipt))
	}
	// A ledger write that failed is the more urgent fact: the durable record
	// and this log now disagree, and that is exactly the state that latches an
	// unresolved execution.
	if err != nil {
		logger.Error("tool "+string(event.Type)+" (not durably recorded)", append(fields, "err", err)...)
		return
	}
	logger.Log(traceLevel(event.Type), "tool "+string(event.Type), fields...)
}

// maxTraceSnippetBytes keeps one lifecycle line readable. A log line that
// wraps for a screenful is a line nobody scans, which defeats the purpose of
// having the trace at all; the ledger keeps the full receipt.
const maxTraceSnippetBytes = 240

// traceSnippet bounds a receipt to one line: newlines and carriage returns
// become spaces so a multi-line tool error cannot break the log's one-record-
// per-line shape, and the result is truncated on a rune boundary.
func traceSnippet(text string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, strings.TrimSpace(text))
	if len(cleaned) <= maxTraceSnippetBytes {
		return cleaned
	}
	truncated := cleaned[:maxTraceSnippetBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated + "…"
}
