package agent

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
)

// memoryBuiltinToolDefs returns memory tool definitions for merging with MCP tools.
func (a *Agent) memoryBuiltinToolDefs() []llm.ToolDef {
	return memory.BuiltinToolDefs()
}

// isMemoryTool checks if a tool name is a built-in memory tool.
func (a *Agent) isMemoryTool(name string) bool {
	return memory.IsBuiltinTool(name)
}

// handleMemoryTool dispatches a memory tool call and returns the result.
func (a *Agent) handleMemoryTool(tc llm.ToolCall) (string, bool) {
	switch tc.Name {
	case "memory_save":
		return a.handleMemorySave(tc.Arguments)
	case "memory_recall":
		return a.handleMemoryRecall(tc.Arguments)
	case "memory_delete":
		return a.handleMemoryDelete(tc.Arguments)
	case "memory_update":
		return a.handleMemoryUpdate(tc.Arguments)
	case "memory_list":
		return a.handleMemoryList(tc.Arguments)
	default:
		return fmt.Sprintf("unknown memory tool: %s", tc.Name), true
	}
}

func (a *Agent) handleMemorySave(args map[string]any) (string, bool) {
	content, _ := args["content"].(string)
	if content == "" {
		return "error: content is required", true
	}

	var tags []string
	if rawTags, ok := args["tags"]; ok {
		switch v := rawTags.(type) {
		case []any:
			for _, t := range v {
				if s, ok := t.(string); ok {
					tags = append(tags, s)
				}
			}
		case []string:
			tags = v
		}
	}

	id, err := a.memoryStore.Save(content, tags)
	if err != nil {
		return fmt.Sprintf("error saving memory: %v", err), true
	}

	return fmt.Sprintf("Memory saved (id: %d)", id), false
}

func (a *Agent) handleMemoryRecall(args map[string]any) (string, bool) {
	query, _ := args["query"].(string)
	if query == "" {
		return "error: query is required", true
	}

	memories, err := a.memoryStore.Recall(query, 5)
	if err != nil {
		// Reporting this as "no matching memories" would hand the model an
		// authoritative absence built from a failure, and the loop would record
		// the call as a completed execution.
		return fmt.Sprintf("error: %v", err), true
	}
	if len(memories) == 0 {
		return "No matching memories found.", false
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Found %d matching memories:\n", len(memories))
	for _, mem := range memories {
		fmt.Fprintf(&b, "- [%d] %s", mem.ID, mem.Content)
		if len(mem.Tags) > 0 {
			fmt.Fprintf(&b, " (tags: %s)", strings.Join(mem.Tags, ", "))
		}
		b.WriteString("\n")
	}

	return b.String(), false
}

func (a *Agent) handleMemoryDelete(args map[string]any) (string, bool) {
	idVal, ok := args["id"]
	if !ok {
		return "error: id is required", true
	}

	var id int
	switch v := idVal.(type) {
	case float64:
		id = int(v)
	case int:
		id = v
	default:
		return "error: id must be a number", true
	}

	deleted, err := a.memoryStore.Delete(id)
	if err != nil {
		return fmt.Sprintf("error deleting memory: %v", err), true
	}
	if !deleted {
		return fmt.Sprintf("memory with id %d not found", id), true
	}
	return fmt.Sprintf("Memory %d deleted", id), false
}

func (a *Agent) handleMemoryUpdate(args map[string]any) (string, bool) {
	idVal, ok := args["id"]
	if !ok {
		return "error: id is required", true
	}

	var id int
	switch v := idVal.(type) {
	case float64:
		id = int(v)
	case int:
		id = v
	default:
		return "error: id must be a number", true
	}

	content, _ := args["content"].(string)
	var tags []string
	if rawTags, ok := args["tags"]; ok {
		switch v := rawTags.(type) {
		case []any:
			for _, t := range v {
				if s, ok := t.(string); ok {
					tags = append(tags, s)
				}
			}
		case []string:
			tags = v
		}
	}

	if content == "" && len(tags) == 0 {
		return "error: at least one of content or tags is required", true
	}

	updated, err := a.memoryStore.Update(id, content, tags)
	if err != nil {
		return fmt.Sprintf("error updating memory: %v", err), true
	}
	if !updated {
		return fmt.Sprintf("memory with id %d not found", id), true
	}
	return fmt.Sprintf("Memory %d updated", id), false
}

func (a *Agent) handleMemoryList(args map[string]any) (string, bool) {
	limit := 20
	if rawLimit, ok := args["limit"]; ok {
		switch v := rawLimit.(type) {
		case float64:
			limit = int(v)
		case int:
			limit = v
		}
	}

	memories, err := a.memoryStore.Recent(limit)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	if len(memories) == 0 {
		return "No memories stored.", false
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Stored memories (%d total):\n", a.memoryStore.Count())
	for _, mem := range memories {
		fmt.Fprintf(&b, "- [%d] %s", mem.ID, mem.Content)
		if len(mem.Tags) > 0 {
			fmt.Fprintf(&b, " (tags: %s)", strings.Join(mem.Tags, ", "))
		}
		b.WriteString("\n")
	}

	return b.String(), false
}
