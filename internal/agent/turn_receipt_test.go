package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestTurnOutcomeMapsTerminalErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus string
		wantReason string
	}{
		{"settled", nil, "settled", StopReasonCompleted},
		{"canceled", context.Canceled, "canceled", StopReasonCanceled},
		{"deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), "canceled", StopReasonCanceled},
		{"eval budget", fmt.Errorf("wrap: %w", ErrTurnEvalBudgetExhausted), "failed", StopReasonBudgetExhausted},
		{"context budget", &TurnContextBudgetError{EstimatedPromptTokens: 9000, ContextWindowTokens: 8192}, "failed", StopReasonContextBudgetExceeded},
		{"empty terminal", ErrEmptyTerminalResponse, "failed", StopReasonEmptyTerminalResponse},
		{"malformed loop", ErrMalformedToolLoop, "failed", StopReasonMalformedToolLoop},
		{"host refusal", &RepeatedHostRefusalError{ToolName: "bash", Attempts: 2}, "failed", StopReasonHostRefusalLoop},
		{"unresolved effect", &UnresolvedExecutionError{}, "failed", StopReasonOutcomeUnknown},
		{"other", errors.New("boom"), "failed", StopReasonError},
	} {
		status, reason := TurnOutcome(test.err)
		if status != test.wantStatus || reason != test.wantReason {
			t.Fatalf("%s: TurnOutcome = (%q, %q), want (%q, %q)", test.name, status, reason, test.wantStatus, test.wantReason)
		}
	}
}

func TestWriteTurnReceiptEmitsOneVersionedDocument(t *testing.T) {
	var buf bytes.Buffer
	err := WriteTurnReceipt(&buf, TurnReceipt{
		Schema: "attacker-chosen", // must be overwritten by the writer
		TurnID: "turn_x",
		Status: "settled", StopReason: StopReasonCompleted,
	})
	if err != nil {
		t.Fatalf("WriteTurnReceipt: %v", err)
	}
	raw := buf.String()
	if !strings.HasSuffix(raw, "\n") || strings.Count(raw, "\n") != 1 {
		t.Fatalf("receipt must be exactly one newline-terminated line, got %q", raw)
	}
	var decoded TurnReceipt
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("receipt is not valid JSON: %v", err)
	}
	if decoded.Schema != TurnReceiptSchema {
		t.Fatalf("schema = %q, want %q", decoded.Schema, TurnReceiptSchema)
	}
	if decoded.ToolCalls == nil {
		t.Fatal("tool_calls must serialize as an empty array, not null")
	}
}

func TestJSONOutputCollectsObservationsWithoutStdout(t *testing.T) {
	var stderr bytes.Buffer
	out := newJSONOutput(&stderr)

	out.StreamText("the answer")
	out.StreamDone(120, 900)
	out.StreamDone(30, 400)
	out.ToolCallStart("call-1", "read", map[string]any{"path": "main.go"})
	out.ToolCallResult("call-1", "read", "content", false, 25*time.Millisecond)
	out.ToolCallResult("call-2", "mcphub__mcphub_call_tool", "fail", true, 5*time.Millisecond)
	out.ToolCallResult("call-3", "memory_save", "ok", false, time.Millisecond)

	receipt := out.ComposeReceipt(TurnReceipt{TurnID: "turn_x", Status: "settled", StopReason: StopReasonCompleted})
	if receipt.Usage.EvalTokens != 150 || receipt.Usage.PromptTokens != 1300 {
		t.Fatalf("usage = %+v, want eval 150 prompt 1300", receipt.Usage)
	}
	if receipt.Text != "the answer" {
		t.Fatalf("text = %q", receipt.Text)
	}
	if len(receipt.ToolCalls) != 3 {
		t.Fatalf("tool calls = %d, want 3", len(receipt.ToolCalls))
	}
	wantKinds := map[string]string{"read": "builtin", "mcphub__mcphub_call_tool": "mcp", "memory_save": "memory"}
	for _, call := range receipt.ToolCalls {
		if call.Kind != wantKinds[call.Name] {
			t.Fatalf("tool %q kind = %q, want %q", call.Name, call.Kind, wantKinds[call.Name])
		}
	}
	if receipt.ToolCalls[1].Status != "error" {
		t.Fatalf("failed call status = %q, want error", receipt.ToolCalls[1].Status)
	}
	if stderr.Len() == 0 {
		t.Fatal("diagnostics should still flow to stderr")
	}
}

func TestJSONOutputBoundsToolCallEntries(t *testing.T) {
	out := newJSONOutput(&bytes.Buffer{})
	for i := 0; i < maxReceiptToolCalls+7; i++ {
		out.ToolCallResult(fmt.Sprintf("call-%d", i), "read", "ok", false, time.Millisecond)
	}
	receipt := out.ComposeReceipt(TurnReceipt{})
	if len(receipt.ToolCalls) != maxReceiptToolCalls {
		t.Fatalf("tool calls = %d, want bounded at %d", len(receipt.ToolCalls), maxReceiptToolCalls)
	}
	if receipt.ToolCallsOmitted != 7 {
		t.Fatalf("omitted = %d, want 7", receipt.ToolCallsOmitted)
	}
}

func TestJSONOutputAggregatesProviderReceipts(t *testing.T) {
	out := newJSONOutput(&bytes.Buffer{})
	out.ProviderReceipt("stop", &llm.ProviderTiming{
		TimeToFirstToken:   400 * time.Millisecond,
		TotalDuration:      2 * time.Second,
		LoadDuration:       100 * time.Millisecond,
		PromptEvalDuration: 200 * time.Millisecond,
		EvalDuration:       1500 * time.Millisecond,
	})
	out.ProviderReceipt("length", &llm.ProviderTiming{
		TimeToFirstToken: 90 * time.Millisecond,
		TotalDuration:    1 * time.Second,
		EvalDuration:     800 * time.Millisecond,
	})

	receipt := out.ComposeReceipt(TurnReceipt{})
	if receipt.Timing == nil {
		t.Fatal("receipt should carry aggregated timing")
	}
	if receipt.Timing.TTFTMS != 400 {
		t.Fatalf("ttft_ms = %d, want the first iteration's 400", receipt.Timing.TTFTMS)
	}
	if receipt.Timing.TotalMS != 3000 || receipt.Timing.EvalMS != 2300 ||
		receipt.Timing.LoadMS != 100 || receipt.Timing.PromptEvalMS != 200 {
		t.Fatalf("timing = %+v", receipt.Timing)
	}
	if !receipt.Truncated {
		t.Fatal("a length finish reason must mark the receipt truncated")
	}

	bare := newJSONOutput(&bytes.Buffer{})
	bare.ProviderReceipt("stop", nil)
	if got := bare.ComposeReceipt(TurnReceipt{}); got.Timing != nil || got.Truncated {
		t.Fatalf("no timings and a clean stop should leave receipt untouched: %+v", got)
	}
}

func TestSetExecutionRunIDImportsLabelWithoutAuthority(t *testing.T) {
	ag := New(nil, nil, 0)
	if err := ag.SetExecutionRunID("chalupa_run_01J"); err != nil {
		t.Fatalf("SetExecutionRunID: %v", err)
	}
	if got := ag.ExecutionRunID(); got != "chalupa_run_01J" {
		t.Fatalf("ExecutionRunID = %q", got)
	}
	if err := ag.SetExecutionRunID("  "); err == nil {
		t.Fatal("blank run identity must be rejected")
	}
	if err := ag.SetExecutionRunID(strings.Repeat("x", 200)); err == nil {
		t.Fatal("oversized run identity must be rejected")
	}
	if err := ag.SetExecutionRunID("bad\xff"); err == nil {
		t.Fatal("non-UTF-8 run identity must be rejected")
	}
}
