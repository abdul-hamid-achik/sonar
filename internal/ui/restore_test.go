package ui

import (
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestEntriesFromMessages(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		// tool-call-only assistant turn + its tool result: both dropped from the
		// visible transcript (the agent keeps them in context).
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{{Name: "ls"}}},
		{Role: "tool", ToolName: "ls", Content: "a.go b.go"},
		{Role: "assistant", Content: "there are two files"},
		{Role: "user", Content: ""}, // empty user message is skipped
	}

	entries := entriesFromMessages(msgs)

	want := []struct {
		kind, content string
	}{
		{"user", "first question"},
		{"assistant", "first answer"},
		{"assistant", "there are two files"},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		if entries[i].Kind != w.kind || entries[i].Content != w.content {
			t.Errorf("entry %d = {%q,%q}, want {%q,%q}", i, entries[i].Kind, entries[i].Content, w.kind, w.content)
		}
	}
}

func TestEntriesFromMessagesEmpty(t *testing.T) {
	if got := entriesFromMessages(nil); got != nil {
		t.Errorf("expected nil for no messages, got %+v", got)
	}
}
