package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
)

func newTestAgentWithMemory(t *testing.T) *Agent {
	t.Helper()
	agent, _ := newTestAgentWithMemoryAt(t)
	return agent
}

func newTestAgentWithMemoryAt(t *testing.T) (*Agent, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-memories.json")
	return &Agent{
		memoryStore: memory.NewStore(path),
		registry:    mcp.NewRegistry(),
	}, path
}

func TestHandleMemoryTool(t *testing.T) {
	tests := []struct {
		name       string
		toolCall   llm.ToolCall
		wantSubstr string
		wantErr    bool
	}{
		{
			name: "dispatch to save",
			toolCall: llm.ToolCall{
				Name:      "memory_save",
				Arguments: map[string]any{"content": "test fact", "tags": []any{"tag1"}},
			},
			wantSubstr: "Memory saved (id:",
			wantErr:    false,
		},
		{
			name: "dispatch to recall",
			toolCall: llm.ToolCall{
				Name:      "memory_recall",
				Arguments: map[string]any{"query": "test"},
			},
			wantSubstr: "No matching memories found.",
			wantErr:    false,
		},
		{
			name: "unknown tool",
			toolCall: llm.ToolCall{
				Name:      "unknown",
				Arguments: map[string]any{},
			},
			wantSubstr: "unknown memory tool: unknown",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ag := newTestAgentWithMemory(t)
			result, isErr := ag.handleMemoryTool(tt.toolCall)
			if isErr != tt.wantErr {
				t.Errorf("handleMemoryTool() isErr = %v, want %v", isErr, tt.wantErr)
			}
			if !strings.Contains(result, tt.wantSubstr) {
				t.Errorf("handleMemoryTool() = %q, want substring %q", result, tt.wantSubstr)
			}
		})
	}
}

func TestHandleMemorySave(t *testing.T) {
	tests := []struct {
		name       string
		args       map[string]any
		wantSubstr string
		wantErr    bool
	}{
		{
			name:       "valid with tags as []any",
			args:       map[string]any{"content": "test fact", "tags": []any{"tag1", "tag2"}},
			wantSubstr: "Memory saved (id:",
			wantErr:    false,
		},
		{
			name:       "valid without tags",
			args:       map[string]any{"content": "another fact"},
			wantSubstr: "Memory saved (id:",
			wantErr:    false,
		},
		{
			name:       "missing content",
			args:       map[string]any{},
			wantSubstr: "error: content is required",
			wantErr:    true,
		},
		{
			name:       "empty content",
			args:       map[string]any{"content": ""},
			wantSubstr: "error: content is required",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ag := newTestAgentWithMemory(t)
			result, isErr := ag.handleMemorySave(tt.args)
			if isErr != tt.wantErr {
				t.Errorf("handleMemorySave() isErr = %v, want %v", isErr, tt.wantErr)
			}
			if !strings.Contains(result, tt.wantSubstr) {
				t.Errorf("handleMemorySave() = %q, want substring %q", result, tt.wantSubstr)
			}
		})
	}
}

func TestHandleMemoryRecall(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(ag *Agent)
		args       map[string]any
		wantSubstr string
		wantErr    bool
	}{
		{
			name: "valid recall finds saved memory",
			setup: func(ag *Agent) {
				_, _ = ag.memoryStore.Save("user prefers Go", []string{"language"})
			},
			args:       map[string]any{"query": "Go"},
			wantSubstr: "Found 1 matching memories:",
			wantErr:    false,
		},
		{
			name:       "missing query",
			setup:      func(ag *Agent) {},
			args:       map[string]any{},
			wantSubstr: "error: query is required",
			wantErr:    true,
		},
		{
			name:       "no matches",
			setup:      func(ag *Agent) {},
			args:       map[string]any{"query": "nonexistent"},
			wantSubstr: "No matching memories found.",
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ag := newTestAgentWithMemory(t)
			tt.setup(ag)
			result, isErr := ag.handleMemoryRecall(tt.args)
			if isErr != tt.wantErr {
				t.Errorf("handleMemoryRecall() isErr = %v, want %v", isErr, tt.wantErr)
			}
			if !strings.Contains(result, tt.wantSubstr) {
				t.Errorf("handleMemoryRecall() = %q, want substring %q", result, tt.wantSubstr)
			}
		})
	}
}

// An unusable memory store must reach the model as an error, never as an
// authoritative "no matching memories" — the loop records tool results as
// completed executions, so a swallowed failure becomes a fact nobody revisits.
func TestMemoryToolsReportAnUnusableStoreInsteadOfAnAbsence(t *testing.T) {
	ag, storePath := newTestAgentWithMemoryAt(t)

	if _, err := ag.memoryStore.Save("a real memory", []string{"tag"}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if result, isErr := ag.handleMemoryRecall(map[string]any{"query": "real"}); isErr ||
		!strings.Contains(result, "a real memory") {
		t.Fatalf("baseline recall failed: %q err=%v", result, isErr)
	}

	// Corrupt the backing file so the next read fails closed.
	if err := os.WriteFile(storePath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt store: %v", err)
	}

	for name, run := range map[string]func() (string, bool){
		"memory_recall": func() (string, bool) {
			return ag.handleMemoryRecall(map[string]any{"query": "real"})
		},
		"memory_list": func() (string, bool) {
			return ag.handleMemoryList(map[string]any{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, isErr := run()
			if !isErr {
				t.Fatalf("a broken store was reported as a normal result: %q", result)
			}
			if strings.Contains(result, "No matching memories") || strings.Contains(result, "No memories stored") {
				t.Fatalf("a failure was reported as an absence: %q", result)
			}
		})
	}
}
