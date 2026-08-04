package ui

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

// ToolType represents the category of a tool for rendering.
type ToolType int

const (
	ToolTypeDefault ToolType = iota
	ToolTypeBash
	ToolTypeFileRead
	ToolTypeFileWrite
	ToolTypeSearch
	ToolTypeWeb
	ToolTypeMemory
)

var exactToolTypes = map[string]ToolType{
	// sonar built-ins and exact compatibility aliases.
	"bash":         ToolTypeBash,
	"exec":         ToolTypeBash,
	"exec_command": ToolTypeBash,
	"execute_bash": ToolTypeBash,
	"run_command":  ToolTypeBash,
	"shell":        ToolTypeBash,
	"shell_exec":   ToolTypeBash,

	"read":      ToolTypeFileRead,
	"read_file": ToolTypeFileRead,
	"file_view": ToolTypeFileRead,
	"view_file": ToolTypeFileRead,
	"cat_file":  ToolTypeFileRead,

	"write":       ToolTypeFileWrite,
	"write_file":  ToolTypeFileWrite,
	"edit":        ToolTypeFileWrite,
	"edit_file":   ToolTypeFileWrite,
	"create_file": ToolTypeFileWrite,
	"apply_patch": ToolTypeFileWrite,
	"patch":       ToolTypeFileWrite,

	"grep":                ToolTypeSearch,
	"search":              ToolTypeSearch,
	"find":                ToolTypeSearch,
	"glob":                ToolTypeSearch,
	"rg":                  ToolTypeSearch,
	"ripgrep":             ToolTypeSearch,
	"vecgrep_search":      ToolTypeSearch,
	"veclite_search":      ToolTypeSearch,
	"hitspec_search_web":  ToolTypeSearch,
	"mcphub_search_tools": ToolTypeSearch,

	"web_search":  ToolTypeWeb,
	"fetch":       ToolTypeWeb,
	"fetch_url":   ToolTypeWeb,
	"http_get":    ToolTypeWeb,
	"curl":        ToolTypeWeb,
	"browse_page": ToolTypeWeb,

	"memory_store":  ToolTypeMemory,
	"memory_save":   ToolTypeMemory,
	"remember_fact": ToolTypeMemory,
	"forget_key":    ToolTypeMemory,
}

// classifyTool returns the ToolType for an exact, local operation name. It
// deliberately rejects namespaced calls: their visible name is
// provider-controlled and may only acquire semantics through a normalized
// ecosystem route.
func classifyTool(name string) ToolType {
	if strings.Contains(name, "__") {
		return ToolTypeDefault
	}
	return classifyExactToolOperation(name)
}

// classifyProjectedTool admits a namespaced operation only when the
// ecosystem parser produced an exact normalized route for it. Unknown
// discovery/build/general tools stay generic rather than inheriting behavior
// from suggestive substrings.
func classifyProjectedTool(name string, projection ecosystem.ToolProjection) ToolType {
	if !strings.Contains(name, "__") {
		return classifyTool(name)
	}
	projection = projection.Normalize()
	if projection.Route.Server == "" || projection.Route.Tool == "" ||
		projection.Operation != projection.Route.Tool {
		return ToolTypeDefault
	}
	return classifyExactToolOperation(projection.Route.Tool)
}

func classifyExactToolOperation(operation string) ToolType {
	operation = strings.ToLower(strings.TrimSpace(operation))
	operation = strings.ReplaceAll(operation, "-", "_")
	if toolType, ok := exactToolTypes[operation]; ok {
		return toolType
	}
	return ToolTypeDefault
}

func toolCardKindForProjectedTool(name string, projection ecosystem.ToolProjection) ToolCardKind {
	switch classifyProjectedTool(name, projection) {
	case ToolTypeFileRead, ToolTypeFileWrite:
		return ToolCardFile
	case ToolTypeBash:
		return ToolCardBash
	case ToolTypeSearch:
		return ToolCardSearch
	default:
		return ToolCardGeneric
	}
}

func previewModeForProjectedTool(
	name string,
	projection ecosystem.ToolProjection,
) ToolPreviewMode {
	operation := name
	if strings.Contains(name, "__") {
		normalized := projection.Normalize()
		if normalized.Route.Server == "" || normalized.Route.Tool == "" ||
			normalized.Operation != normalized.Route.Tool {
			return ToolPreviewGeneric
		}
		operation = normalized.Route.Tool
	}
	normalizedOperation := strings.ReplaceAll(
		strings.ToLower(strings.TrimSpace(operation)),
		"-",
		"_",
	)
	// A diff is read-only in the execution policy, but its result body still
	// benefits from the edit/diff preview budget.
	if normalizedOperation == "diff" {
		return ToolPreviewEdit
	}
	switch classifyExactToolOperation(operation) {
	case ToolTypeFileRead:
		return ToolPreviewRead
	case ToolTypeBash:
		return ToolPreviewExec
	case ToolTypeSearch:
		return ToolPreviewSearch
	case ToolTypeFileWrite:
		return ToolPreviewEdit
	default:
		return ToolPreviewGeneric
	}
}

// toolIcon returns a type-specific icon for the tool.
func toolIcon(tt ToolType, status ToolStatus) string {
	if status == ToolStatusCancelled {
		return "–"
	}
	if status == ToolStatusError {
		return "✗"
	}
	if status == ToolStatusDone {
		switch tt {
		case ToolTypeBash:
			return "$"
		case ToolTypeFileRead:
			return "◎"
		case ToolTypeFileWrite:
			return "✎"
		case ToolTypeWeb:
			return "◆"
		case ToolTypeMemory:
			return "◈"
		default:
			return "✓"
		}
	}
	// Running
	return "⚙"
}

// toolSummary extracts a key argument for display based on tool type.
func toolSummary(tt ToolType, te ToolEntry) string {
	if te.RawArgs == nil {
		return ""
	}
	if summary := ecosystemToolSummary(te.Name, te.RawArgs); summary != "" {
		return summary
	}
	switch tt {
	case ToolTypeBash:
		if cmd, ok := te.RawArgs["command"].(string); ok {
			return truncateDisplay(cmd, 60)
		}
	case ToolTypeFileRead, ToolTypeFileWrite:
		for _, key := range []string{"path", "file_path", "filename", "file"} {
			if p, ok := te.RawArgs[key].(string); ok {
				return p
			}
		}
	case ToolTypeWeb:
		for _, key := range []string{"url", "uri", "href"} {
			if u, ok := te.RawArgs[key].(string); ok {
				return truncateDisplay(u, 60)
			}
		}
	case ToolTypeMemory:
		if k, ok := te.RawArgs["key"].(string); ok {
			return k
		}
	}
	return ""
}

// detectCodeBlocks checks if the result contains markdown code blocks.
func detectCodeBlocks(text string) bool {
	return strings.Contains(text, "```") || strings.Contains(text, "~~~")
}

// formatToolResult formats a tool result for display with smart truncation.
// It preserves code blocks and adds expand/collapse hints.
func formatToolResult(result string, maxLines int, maxWidth int) string {
	result = sanitizeTerminalMultiline(result)
	if result == "" {
		return "(no output)"
	}

	lines := strings.Split(result, "\n")

	// Detect if result contains code blocks
	hasCodeBlocks := detectCodeBlocks(result)

	// Truncate by lines if too long
	if len(lines) > maxLines {
		var b strings.Builder
		for i := 0; i < maxLines; i++ {
			line := lines[i]
			// Truncate long lines
			line = truncateDisplay(line, maxWidth)
			b.WriteString(line)
			b.WriteString("\n")
		}
		remaining := len(lines) - maxLines
		b.WriteString("... ")
		if hasCodeBlocks {
			b.WriteString("(code blocks truncated)")
		} else {
			fmt.Fprintf(&b, "%d", remaining)
			b.WriteString(" more lines")
		}
		return b.String()
	}

	// Truncate long lines
	var b strings.Builder
	for i, line := range lines {
		line = truncateDisplay(line, maxWidth)
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}

	return b.String()
}
