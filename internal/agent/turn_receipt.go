package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// TurnReceiptSchema identifies the versioned machine-readable summary of one
// headless turn. Exactly one receipt document is emitted on stdout per --json
// invocation; everything else stays on stderr.
const TurnReceiptSchema = "sonar.turn-receipt.v1"

// Turn receipt stop reasons form a closed catalog. External supervisors branch
// on these values, so new reasons are additive and existing ones never change
// meaning. Reasons shared with the goal supervisor keep its exact spelling.
const (
	StopReasonCompleted             = "completed"
	StopReasonCanceled              = "canceled"
	StopReasonOutcomeUnknown        = "outcome_unknown"
	StopReasonBudgetExhausted       = "budget_exhausted"
	StopReasonContextBudgetExceeded = "context_budget_exceeded"
	StopReasonEmptyTerminalResponse = "empty_terminal_response"
	StopReasonMalformedToolLoop     = "malformed_tool_loop"
	StopReasonHostRefusalLoop       = "host_refusal_loop"
	StopReasonError                 = "error"
)

// TurnReceiptSession identifies the durable session the turn executed under.
type TurnReceiptSession struct {
	ID        int64  `json:"id"`
	PublicID  string `json:"public_id,omitempty"`
	Workspace string `json:"workspace"`
}

// TurnReceiptModelOffload reports weights residency from the local runtime's
// process inventory. vram_bytes < total_bytes means part of the model ran on
// CPU — a fact that silently changes throughput and comparability.
type TurnReceiptModelOffload struct {
	VRAMBytes  int64 `json:"vram_bytes"`
	TotalBytes int64 `json:"total_bytes"`
}

// TurnReceiptModel records the inference identity the host resolved for the
// turn. Digest is present only when the provider inventory verified it, and
// Offload only when the runtime exposed residency for the model.
type TurnReceiptModel struct {
	Name     string                   `json:"name"`
	Digest   string                   `json:"digest,omitempty"`
	NumCtx   int                      `json:"num_ctx"`
	Provider string                   `json:"provider,omitempty"`
	Remote   bool                     `json:"remote"`
	Offload  *TurnReceiptModelOffload `json:"offload,omitempty"`
}

// TurnReceiptTiming aggregates provider-reported request timings across every
// iteration in the turn. ttft_ms is the first iteration's client-measured
// time to first token; the other fields are summed provider durations. A
// zero value means "not reported", never "instant".
type TurnReceiptTiming struct {
	TTFTMS       int64 `json:"ttft_ms"`
	LoadMS       int64 `json:"load_ms"`
	PromptEvalMS int64 `json:"prompt_eval_ms"`
	EvalMS       int64 `json:"eval_ms"`
	TotalMS      int64 `json:"total_ms"`
}

// TurnReceiptUsage accumulates provider-reported token accounting across every
// request in the turn, including conservative fail-closed reservations.
type TurnReceiptUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	EvalTokens   int64 `json:"eval_tokens"`
}

// TurnReceiptToolCall is one settled tool invocation. Raw arguments and
// results never enter the receipt; the execution ledger owns those hashes.
type TurnReceiptToolCall struct {
	CallID     string `json:"call_id,omitempty"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
}

// TurnReceiptDecision is the goal supervisor's verdict on whether another
// explicit invocation may dispatch. Action "stop" means a further `goal run`
// would not be admitted; Reason then uses the supervisor's own closed
// catalog, which shares spellings with the turn stop reasons where the two
// vocabularies overlap (completed, budget_exhausted, outcome_unknown).
type TurnReceiptDecision struct {
	Action   string   `json:"action"`
	Reason   string   `json:"reason,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	IssueIDs []string `json:"issue_ids,omitempty"`
}

// TurnReceipt is the complete document. RunID, TurnID, and Actor may carry
// externally minted identifiers; they are correlation labels and never grant
// authority. Status is one of settled, canceled, failed, or not_admitted —
// the last means the goal supervisor refused dispatch and no provider work
// happened.
type TurnReceipt struct {
	Schema           string                `json:"schema"`
	RunID            string                `json:"run_id,omitempty"`
	TurnID           string                `json:"turn_id,omitempty"`
	Actor            string                `json:"actor,omitempty"`
	Session          *TurnReceiptSession   `json:"session,omitempty"`
	Model            TurnReceiptModel      `json:"model"`
	Usage            TurnReceiptUsage      `json:"usage"`
	ToolCalls        []TurnReceiptToolCall `json:"tool_calls"`
	ToolCallsOmitted int                   `json:"tool_calls_omitted,omitempty"`
	Timing           *TurnReceiptTiming    `json:"timing,omitempty"`
	Status           string                `json:"status"`
	StopReason       string                `json:"stop_reason"`
	// Truncated is true when any provider response in the turn finished with
	// reason "length": the generation hit its token ceiling mid-thought.
	Truncated            bool                 `json:"truncated,omitempty"`
	Decision             *TurnReceiptDecision `json:"decision,omitempty"`
	Error                string               `json:"error,omitempty"`
	ExecutionCursor      int64                `json:"execution_cursor"`
	PendingRecoveryCount int                  `json:"pending_recovery_count"`
	Text                 string               `json:"text"`
}

// TurnOutcome maps a RunTurn error onto the receipt's closed status and
// stop-reason vocabulary. Status is "settled" for a clean turn, "canceled"
// for host/context cancellation, and "failed" otherwise.
func TurnOutcome(err error) (status, stopReason string) {
	var unresolved *UnresolvedExecutionError
	switch {
	case err == nil:
		return "settled", StopReasonCompleted
	case errors.As(err, &unresolved):
		return "failed", StopReasonOutcomeUnknown
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return "canceled", StopReasonCanceled
	case errors.Is(err, ErrTurnEvalBudgetExhausted):
		return "failed", StopReasonBudgetExhausted
	case errors.Is(err, ErrTurnContextBudgetExceeded):
		return "failed", StopReasonContextBudgetExceeded
	case errors.Is(err, ErrEmptyTerminalResponse):
		return "failed", StopReasonEmptyTerminalResponse
	case errors.Is(err, ErrMalformedToolLoop):
		return "failed", StopReasonMalformedToolLoop
	case errors.Is(err, ErrRepeatedHostRefusal):
		return "failed", StopReasonHostRefusalLoop
	default:
		return "failed", StopReasonError
	}
}

// WriteTurnReceipt emits exactly one newline-terminated JSON document. The
// schema field is stamped here so callers cannot ship an unversioned receipt.
func WriteTurnReceipt(w io.Writer, receipt TurnReceipt) error {
	receipt.Schema = TurnReceiptSchema
	if receipt.ToolCalls == nil {
		receipt.ToolCalls = []TurnReceiptToolCall{}
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode turn receipt: %w", err)
	}
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write turn receipt: %w", err)
	}
	return nil
}
