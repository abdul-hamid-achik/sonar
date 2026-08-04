package mcp

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// ServerInfo holds metadata about a connected MCP server.
type ServerInfo struct {
	Name      string
	ToolCount int
}

// ServerInstruction is bounded usage guidance supplied by a connected MCP
// server during protocol initialization.
type ServerInstruction struct {
	Name string
	Text string
}

const maxServerInstructionBytes = 4 * 1024

const serverInstructionTruncatedMarker = "\n... [MCP server guidance truncated]"

func boundServerInstruction(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	text = strings.TrimSpace(strings.ToValidUTF8(text, "�"))
	if len(text) <= maxBytes {
		return text
	}

	marker := serverInstructionTruncatedMarker
	if len(marker) >= maxBytes {
		marker = ""
	}
	limit := maxBytes - len(marker)
	for limit > 0 && !utf8.ValidString(text[:limit]) {
		limit--
	}
	return strings.TrimSpace(text[:limit]) + marker
}

// ToolResult is the outcome of a tool call.
type ToolResult struct {
	Content    string
	Structured json.RawMessage
	ErrorMeta  json.RawMessage
	IsError    bool
}

// ToLLMToolDef converts MCP tool schema to the LLM's tool definition format.
func ToLLMToolDef(name, description string, inputSchema any) llm.ToolDef {
	return toLLMToolDef(name, "", description, inputSchema, nil)
}

// ToLLMToolDefFromMCP preserves bounded, host-only presentation metadata from
// the MCP server. Annotations remain untrusted and are never authorization or
// durable-effect input; missing hints retain conservative display defaults.
func ToLLMToolDefFromMCP(tool *mcp.Tool) llm.ToolDef {
	if tool == nil {
		return ToLLMToolDef("", "", nil)
	}
	return toLLMToolDef(tool.Name, tool.Title, tool.Description, tool.InputSchema, tool.Annotations)
}

func toLLMToolDef(name, title, description string, inputSchema any, annotations *mcp.ToolAnnotations) llm.ToolDef {
	params, _ := inputSchema.(map[string]any)
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	def := llm.ToolDef{
		Name:        name,
		Description: description,
		Parameters:  params,
	}
	if annotations == nil {
		def.DisplayName = strings.TrimSpace(title)
		return def
	}
	if strings.TrimSpace(title) == "" {
		title = annotations.Title
	}
	destructive := !annotations.ReadOnlyHint
	if !annotations.ReadOnlyHint && annotations.DestructiveHint != nil {
		destructive = *annotations.DestructiveHint
	}
	openWorld := true
	if annotations.OpenWorldHint != nil {
		openWorld = *annotations.OpenWorldHint
	}
	def.DisplayName = strings.TrimSpace(title)
	def.Behavior = llm.ToolBehavior{
		Declared:    true,
		ReadOnly:    annotations.ReadOnlyHint,
		Destructive: destructive,
		Idempotent:  annotations.IdempotentHint,
		OpenWorld:   openWorld,
	}
	return def
}
