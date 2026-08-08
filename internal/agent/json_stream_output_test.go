package agent

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// The stream is a per-callback JSONL projection that shares the receipt
// accumulator: every line parses alone, tool events carry the receipt's
// bounded fields and never arguments or results, and the accumulated receipt
// matches what plain --json would have reported.
func TestStreamJSONOutputEmitsParseableBoundedEvents(t *testing.T) {
	var events strings.Builder
	out := newStreamJSONOutput(&events, io.Discard)

	out.StreamText("Hola.")
	out.ToolCallStart("call-1", "read", map[string]any{"path": "secret.txt"})
	out.ToolCallResult("call-1", "read", "the entire file body", false, 40*time.Millisecond)
	out.ToolCallResult("call-2", "mcphub__mcphub_call_tool", "boom", true, time.Millisecond)
	out.StreamDone(9, 100)
	out.Error("provider hiccup")

	lines := []streamEvent{}
	for _, raw := range strings.Split(strings.TrimSpace(events.String()), "\n") {
		var event streamEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatalf("line does not parse alone: %q: %v", raw, err)
		}
		lines = append(lines, event)
	}
	if len(lines) != 6 {
		t.Fatalf("events = %d, want 6: %+v", len(lines), lines)
	}
	if lines[0].Event != "text" || lines[0].Text != "Hola." {
		t.Fatalf("text event = %+v", lines[0])
	}
	if lines[1].Event != "tool_start" || lines[1].Name != "read" || lines[1].Kind != "builtin" {
		t.Fatalf("tool_start = %+v", lines[1])
	}
	if strings.Contains(events.String(), "secret.txt") || strings.Contains(events.String(), "entire file body") {
		t.Fatal("stream leaked tool arguments or results across the process boundary")
	}
	if lines[2].Event != "tool_result" || lines[2].Status != "ok" || lines[2].DurationMS != 40 {
		t.Fatalf("tool_result = %+v", lines[2])
	}
	if lines[3].Status != "error" || lines[3].Kind != "mcp" {
		t.Fatalf("mcp error result = %+v", lines[3])
	}
	if lines[4].Event != "usage" || lines[4].EvalTokens != 9 || lines[4].PromptTokens != 100 {
		t.Fatalf("usage = %+v", lines[4])
	}
	if lines[5].Event != "error" || lines[5].Message != "provider hiccup" {
		t.Fatalf("error = %+v", lines[5])
	}

	// The shared accumulator still composes the exact --json receipt.
	receipt := out.ComposeReceipt(TurnReceipt{})
	if receipt.Usage.EvalTokens != 9 || len(receipt.ToolCalls) != 2 || receipt.Text != "Hola." {
		t.Fatalf("receipt diverged from stream: %+v", receipt)
	}
}
