package mcp

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToLLMToolDef(t *testing.T) {
	tests := []struct {
		name        string
		toolName    string
		description string
		inputSchema any
		wantName    string
		wantDesc    string
		wantParams  bool // true = should have non-nil params
	}{
		{
			name:        "normal with valid schema",
			toolName:    "read_file",
			description: "Read a file",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
			},
			wantName:   "read_file",
			wantDesc:   "Read a file",
			wantParams: true,
		},
		{
			name:        "nil schema uses default",
			toolName:    "noop",
			description: "No-op tool",
			inputSchema: nil,
			wantName:    "noop",
			wantDesc:    "No-op tool",
			wantParams:  true,
		},
		{
			name:        "non-map schema uses default",
			toolName:    "bad_schema",
			description: "Bad schema tool",
			inputSchema: "not a map",
			wantName:    "bad_schema",
			wantDesc:    "Bad schema tool",
			wantParams:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ToLLMToolDef(tt.toolName, tt.description, tt.inputSchema)
			if result.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", result.Name, tt.wantName)
			}
			if result.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", result.Description, tt.wantDesc)
			}
			if tt.wantParams && result.Parameters == nil {
				t.Error("Parameters should not be nil")
			}
			// Nil and non-map schemas should get the default object schema.
			if tt.inputSchema == nil || func() bool { _, ok := tt.inputSchema.(map[string]any); return !ok }() {
				if result.Parameters["type"] != "object" {
					t.Errorf("default schema type = %v, want 'object'", result.Parameters["type"])
				}
			}
		})
	}
}

func TestToLLMToolDefFromMCPPreservesUntrustedPresentationBehavior(t *testing.T) {
	no := false
	tool := &sdkmcp.Tool{
		Name: "cortex_status", Title: "Inspect Cortex status",
		Description: "Read durable task status.",
		InputSchema: map[string]any{"type": "object"},
		Annotations: &sdkmcp.ToolAnnotations{
			Title: "Ignored annotation title", ReadOnlyHint: true,
			DestructiveHint: &no, OpenWorldHint: &no,
		},
	}
	def := ToLLMToolDefFromMCP(tool)
	if def.DisplayName != tool.Title {
		t.Fatalf("display name = %q, want %q", def.DisplayName, tool.Title)
	}
	if !def.Behavior.Declared || !def.Behavior.ReadOnly || def.Behavior.Destructive || def.Behavior.OpenWorld {
		t.Fatalf("presentation behavior = %#v", def.Behavior)
	}
}

func TestToLLMToolDefFromMCPKeepsConservativeMissingHintDefaults(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name            string
		tool            *sdkmcp.Tool
		wantDeclared    bool
		wantReadOnly    bool
		wantDestructive bool
		wantOpenWorld   bool
	}{
		{name: "missing annotations", tool: &sdkmcp.Tool{Name: "unknown"}},
		{name: "missing open-world hint", tool: &sdkmcp.Tool{Name: "read", Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &no}}, wantDeclared: true, wantReadOnly: true, wantOpenWorld: true},
		{name: "read-only ignores missing destructive hint", tool: &sdkmcp.Tool{Name: "read", Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &no}}, wantDeclared: true, wantReadOnly: true},
		{name: "read-only ignores contradictory destructive hint", tool: &sdkmcp.Tool{Name: "read", Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &yes, OpenWorldHint: &no}}, wantDeclared: true, wantReadOnly: true},
		{name: "open-world read", tool: &sdkmcp.Tool{Name: "search", Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &no, OpenWorldHint: &yes}}, wantDeclared: true, wantReadOnly: true, wantOpenWorld: true},
		{name: "effectful additive", tool: &sdkmcp.Tool{Name: "start", Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, OpenWorldHint: &no}}, wantDeclared: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			behavior := ToLLMToolDefFromMCP(tt.tool).Behavior
			if behavior.Declared != tt.wantDeclared || behavior.ReadOnly != tt.wantReadOnly ||
				behavior.Destructive != tt.wantDestructive || behavior.OpenWorld != tt.wantOpenWorld {
				t.Fatalf("behavior = %#v", behavior)
			}
		})
	}
}
