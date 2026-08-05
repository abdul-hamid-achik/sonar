package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// authorizeToolCall applies one policy path to every risky built-in, memory,
// and MCP operation. The CLI always installs a checker; nil remains an
// explicit embedding opt-out for package users and unit tests.
func (a *Agent) authorizeToolCall(ctx context.Context, tc llm.ToolCall, out Output) bool {
	decision, err := a.decideToolAuthorization(ctx, a.AuthorityMode(), tc, nil)
	if err != nil || decision.cancelled {
		return false
	}
	if !decision.allowed {
		if decision.hostRefused {
			code := decision.refusalCode
			if code == "" {
				code = "host_refused"
			}
			a.failedToolCall(tc, out, fmt.Sprintf("tool request refused by host [%s]: %s", code, decision.reason), a.NumCtx())
			return false
		}
		a.deniedToolCall(tc, out, "tool call denied: "+decision.reason)
		return false
	}
	return ctx.Err() == nil
}

func (a *Agent) blockedToolCall(tc llm.ToolCall, out Output) {
	errMsg := fmt.Sprintf("tool call blocked in current mode: %s", tc.Name)
	out.ToolCallStart(tc.ID, tc.Name, tc.Arguments)
	out.ToolCallResult(tc.ID, tc.Name, errMsg, true, 0)
	a.AppendMessage(llm.Message{
		Role:       "tool",
		Content:    errMsg,
		ToolName:   tc.Name,
		ToolCallID: tc.ID,
	})
}

func (a *Agent) deniedToolCall(tc llm.ToolCall, out Output, reason string) {
	out.ToolCallStart(tc.ID, tc.Name, tc.Arguments)
	out.ToolCallResult(tc.ID, tc.Name, reason, true, 0)
	a.AppendMessage(llm.Message{
		Role:       "tool",
		Content:    reason,
		ToolName:   tc.Name,
		ToolCallID: tc.ID,
	})
}

func (a *Agent) failedToolCall(tc llm.ToolCall, out Output, reason string, numCtx int) {
	result := capToolResultForContext(reason, numCtx)
	out.ToolCallStart(tc.ID, tc.Name, tc.Arguments)
	out.ToolCallResult(tc.ID, tc.Name, result, true, 0)
	a.AppendMessage(llm.Message{
		Role:       "tool",
		Content:    result,
		ToolName:   tc.Name,
		ToolCallID: tc.ID,
	})
}

// cancelUndispatchedToolCalls closes every assistant tool-call edge that is
// still queued when a turn is cancelled. These operations never reached a
// backend, so the receipt deliberately distinguishes them from the
// OUTCOME UNKNOWN receipt used after dispatch.
func (a *Agent) cancelUndispatchedToolCalls(calls []llm.ToolCall, out Output, cause error) {
	for _, tc := range calls {
		result := fmt.Sprintf("CANCELLED — NOT DISPATCHED: tool %q did not start because the turn ended: %v", tc.Name, cause)
		out.ToolCallStart(tc.ID, tc.Name, tc.Arguments)
		out.ToolCallResult(tc.ID, tc.Name, result, true, 0)
		a.AppendMessage(llm.Message{
			Role:       "tool",
			Content:    result,
			ToolName:   tc.Name,
			ToolCallID: tc.ID,
		})
	}
}

func builtinToolRequiresApproval(name string) bool {
	switch name {
	case "write", "edit", "bash", "mkdir", "remove", "copy", "move":
		return true
	default:
		return false
	}
}

func memoryToolRequiresApproval(name string) bool {
	switch name {
	case "memory_save", "memory_delete", "memory_update":
		return true
	default:
		return false
	}
}

func mcpDispatchErrorReceipt(name string, err error) string {
	return fmt.Sprintf(outcomeUnknownReceiptPrefix+" tool %q ended without a result receipt and may have taken effect: %v", name, err)
}

func dispatchedEffectErrorReceipt(name, backendResult string, contextErr error) string {
	if contextErr != nil {
		return fmt.Sprintf(outcomeUnknownReceiptPrefix+" tool %q was cancelled after dispatch and may have taken effect: %v\nBackend result: %s", name, contextErr, backendResult)
	}
	return fmt.Sprintf(outcomeUnknownReceiptPrefix+" tool %q returned an error after dispatch and may have partially taken effect. Do not retry automatically; inspect state first.\nBackend result: %s", name, backendResult)
}

// isRetryableError returns true for transient LLM errors that are worth
// retrying: malformed JSON from small models, and transport failures such as
// connection loss, truncated streams, provider 5xx, or an idle-stream
// watchdog. Retry happens before any tool dispatch, so resending the same
// request cannot double-execute effects.
func isRetryableError(err error) bool {
	if llm.IsRetryableTransport(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "parse JSON") || strings.Contains(msg, "unexpected end of JSON")
}
