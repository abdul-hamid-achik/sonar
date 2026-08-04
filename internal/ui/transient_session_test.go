package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestHeadlessSessionStatePersistsDurableToolReceiptNotTransientPage(t *testing.T) {
	const (
		secret  = "SECRET_TRANSIENT_RESULT_PAGE"
		receipt = "MCPHub page 0-128 · payload omitted from persistent context"
	)
	raw, err := EncodeHeadlessSessionState([]llm.Message{{
		Role: "tool", Content: secret, DurableContent: receipt,
		ToolName: "mcphub__mcphub_get_result", ToolCallID: "call-1",
	}}, "qwen3.5:4b", "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, secret) || !strings.Contains(raw, receipt) || strings.Contains(raw, "DurableContent") {
		t.Fatalf("session state crossed transient boundary: %s", raw)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) != 1 || state.Messages[0].Content != receipt || state.Messages[0].DurableContent != "" {
		t.Fatalf("restored messages = %#v", state.Messages)
	}
}

func TestSnapshotExecutionCursorHashesDurableTransientReplacement(t *testing.T) {
	m, _, terminal := modelWithCompletedExecution(t)
	m.agent.ReplaceMessages([]llm.Message{{
		Role: "tool", ToolCallID: terminal.Identity.CanonicalCallID, ToolName: terminal.Identity.ToolName,
		Content: "transient provider-only payload", DurableContent: "done",
	}})

	cursor, err := m.snapshotExecutionCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cursor != terminal.ID {
		t.Fatalf("snapshot cursor = %d, want terminal event %d", cursor, terminal.ID)
	}
}
