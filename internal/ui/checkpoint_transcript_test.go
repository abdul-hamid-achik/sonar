package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestCheckpointTranscriptRestoresCausalToolReceiptWithoutRawPayload(t *testing.T) {
	const rawSecret = `{"structuredContent":{"secret":"CHECKPOINT_RAW_SECRET"}}`
	entries, tools := checkpointTranscriptFromMessages([]llm.Message{
		{Role: "user", Content: "inspect"},
		{Role: "assistant", Content: "I will inspect it", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{Role: "tool", ToolName: "read", ToolCallID: "call-1", Content: rawSecret},
		{Role: "assistant", Content: "Inspection complete."},
	})
	if len(entries) != 4 || len(tools) != 1 {
		t.Fatalf("checkpoint projection entries/tools = %d/%d: %#v %#v", len(entries), len(tools), entries, tools)
	}
	if entries[2].Kind != "tool_group" || entries[2].ToolIndex != 0 ||
		entries[3].Kind != "assistant" {
		t.Fatalf("causal tool placement was lost: %#v", entries)
	}
	tool := tools[0]
	if tool.Result != "" || tool.Args != "" || tool.RawArgs != nil ||
		strings.Contains(tool.Summary, "CHECKPOINT_RAW_SECRET") {
		t.Fatalf("checkpoint tool retained raw content: %#v", tool)
	}
	if tool.Projection.Transport != ecosystem.TransportSucceeded ||
		tool.Projection.Domain != ecosystem.DomainUnknown ||
		tool.Projection.Evidence != ecosystem.EvidenceNone ||
		tool.Projection.DomainTyped {
		t.Fatalf("checkpoint tool invented a domain/evidence outcome: %#v", tool.Projection)
	}

	m := newTestModel(t)
	m.entries, m.toolEntries = entries, tools
	rendered := ansi.Strip(m.renderEntries())
	if strings.Contains(rendered, "CHECKPOINT_RAW_SECRET") {
		t.Fatalf("rendered checkpoint leaked raw payload:\n%s", rendered)
	}
	card := testProjectedToolCard(t, m, 0)
	if card.State != ToolCardAttention {
		t.Fatalf("restored untyped receipt was not attention: %#v", card)
	}
}

func TestCheckpointTranscriptSanitizesUnsafeToolIdentity(t *testing.T) {
	entries, tools := checkpointTranscriptFromMessages([]llm.Message{{
		Role: "tool", ToolName: "read\x1b[31m\nforged", ToolCallID: " bad\nid ", Content: "ignored",
	}})
	if len(entries) != 1 || len(tools) != 1 {
		t.Fatalf("checkpoint projection = %#v %#v", entries, tools)
	}
	if tools[0].ID != "checkpoint-tool-0" ||
		strings.ContainsAny(tools[0].Name, "\x1b\r\n") {
		t.Fatalf("unsafe tool identity survived: %#v", tools[0])
	}
}
