package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// ToolHook is an interceptor that runs around every tool execution. It is the
// in-process equivalent of iii's before/after tool-hook fanout: a single seam
// for audit logging, argument validation/redaction, and policy side effects,
// without the agent loop having to special-case any of them.
//
// Pre hooks and ordinary post hooks run in registration order. Host-owned
// context-only limiters are applied after every policy/redaction post hook so a
// bounded interactive store can synchronously admit the complete safe result.
// Hooks must be safe for concurrent use: MCP and memory tools execute in
// parallel goroutines.
type ToolHook interface {
	// Name identifies the hook in logs.
	Name() string

	// PreToolUse runs before a tool executes. It may mutate call.Arguments in
	// place (e.g. to redact or normalize). Returning block=true cancels the
	// call; reason becomes the synthetic tool result returned to the model.
	PreToolUse(ctx context.Context, call *llm.ToolCall) (block bool, reason string)

	// PostToolUse runs after a tool executes. It may rewrite the result in
	// place (e.g. to redact secrets or cap size). isErr reflects whether the
	// tool reported an error.
	PostToolUse(ctx context.Context, call llm.ToolCall, result *string, isErr bool)
}

// AddToolHook registers a tool hook. Not safe to call concurrently with Run.
func (a *Agent) AddToolHook(h ToolHook) {
	a.hooks = append(a.hooks, h)
}

// runPreHooks invokes every PreToolUse hook. If any blocks, it returns the
// block reason and the index is short-circuited (later hooks do not run).
func (a *Agent) runPreHooks(ctx context.Context, call *llm.ToolCall) (block bool, reason string) {
	for _, h := range a.hooks {
		if b, r := h.PreToolUse(ctx, call); b {
			if a.logger != nil {
				a.logger.Debug("tool hook block", "hook", h.Name(), "tool", call.Name, "reason", r)
			}
			return true, r
		}
	}
	return false, ""
}

// contextOnlyToolResultHook marks a hook that limits provider, ledger, and
// transcript context but must not destroy the complete post-redaction result
// before an interactive output host can admit a bounded ephemeral prefix.
//
// The unexported marker deliberately reserves this lifecycle distinction for
// host-owned hooks. Arbitrary third-party hooks remain redaction/policy hooks
// and therefore run before the ephemeral detail boundary.
type contextOnlyToolResultHook interface {
	contextOnlyToolResultHook()
}

// runPostHooks invokes policy/redaction hooks first, captures the complete safe
// result, and then applies context-only limiters. The returned string is
// ephemeral: callers may offer it only to a process-local bounded store, never
// to provider context, the execution ledger, transcript text, or session state.
func (a *Agent) runPostHooks(
	ctx context.Context,
	call llm.ToolCall,
	result *string,
	isErr bool,
) string {
	for _, h := range a.hooks {
		if _, contextOnly := h.(contextOnlyToolResultHook); contextOnly {
			continue
		}
		h.PostToolUse(ctx, call, result, isErr)
	}
	complete := ""
	if result != nil {
		complete = *result
	}
	for _, h := range a.hooks {
		if _, contextOnly := h.(contextOnlyToolResultHook); !contextOnly {
			continue
		}
		h.PostToolUse(ctx, call, result, isErr)
	}
	return complete
}

// SizeCapHook truncates oversized tool results so a single runaway tool (a
// huge file read, a noisy command) cannot blow the context-window budget — a
// real risk on the small local models this agent targets. A no-op PreToolUse.
type SizeCapHook struct {
	MaxBytes int
}

// NewSizeCapHook returns a size-cap hook. A non-positive max disables capping.
func NewSizeCapHook(maxBytes int) *SizeCapHook { return &SizeCapHook{MaxBytes: maxBytes} }

func (h *SizeCapHook) Name() string { return "size-cap" }

func (*SizeCapHook) contextOnlyToolResultHook() {}

func (h *SizeCapHook) PreToolUse(ctx context.Context, call *llm.ToolCall) (bool, string) {
	return false, ""
}

func (h *SizeCapHook) PostToolUse(ctx context.Context, call llm.ToolCall, result *string, isErr bool) {
	if h.MaxBytes <= 0 || result == nil {
		return
	}
	if len(*result) <= h.MaxBytes && utf8.ValidString(*result) {
		return
	}
	total := len(*result)
	prefix, consumed := boundedValidUTF8Prefix(*result, h.MaxBytes)
	if consumed == total {
		*result = prefix
		return
	}
	*result = prefix + fmt.Sprintf(
		"\n\n... [output truncated: %d of %d bytes shown]",
		consumed,
		total,
	)
}

// boundedValidUTF8Prefix converts invalid source bytes to the replacement rune
// while scanning no more source than can fit in the bounded result. It returns
// the number of original source bytes represented by the safe prefix, keeping
// the truncation receipt honest without an unbounded allocation.
func boundedValidUTF8Prefix(value string, maxBytes int) (string, int) {
	if value == "" || maxBytes <= 0 {
		return "", 0
	}
	var result strings.Builder
	result.Grow(min(maxBytes, len(value)))
	consumed := 0
	for consumed < len(value) {
		character, size := utf8.DecodeRuneInString(value[consumed:])
		if character == utf8.RuneError && size == 1 {
			character = '\uFFFD'
		}
		characterBytes := utf8.RuneLen(character)
		if characterBytes < 0 || result.Len()+characterBytes > maxBytes {
			break
		}
		result.WriteRune(character)
		consumed += size
	}
	return result.String(), consumed
}
