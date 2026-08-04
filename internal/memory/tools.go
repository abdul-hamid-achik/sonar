package memory

import "github.com/abdul-hamid-achik/sonar/internal/llm"

// BuiltinToolDefs returns the tool definitions for memory operations.
func BuiltinToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Name:        "memory_save",
			Description: "Save an important fact, user preference, or piece of context to persistent memory. Use this proactively when the user shares information worth remembering across sessions.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "The fact or information to remember.",
					},
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional tags for categorization (e.g., 'preference', 'project', 'name').",
					},
				},
				"required": []string{"content"},
			},
		},
		{
			Name:        "memory_recall",
			Description: "Search persistent memory for previously saved facts. Use this when you need to recall user preferences, project details, or other saved context.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search query to find relevant memories.",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "memory_delete",
			Description: "Delete a memory by its ID. Use memory_recall or memory_list first to find the ID of the memory you want to delete.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "integer",
						"description": "The ID of the memory to delete (use memory_list or memory_recall to find IDs).",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "memory_update",
			Description: "Update an existing memory's content or tags. Use memory_recall or memory_list first to find the ID.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "integer",
						"description": "The ID of the memory to update (use memory_list or memory_recall to find IDs).",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "New content for the memory.",
					},
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "New tags for the memory.",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "memory_list",
			Description: "List all stored memories with their IDs, content, and tags. Use this to see what has been saved.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of memories to return (default: 20).",
					},
				},
			},
		},
	}
}

// IsBuiltinTool returns true if the given tool name is a built-in memory tool.
func IsBuiltinTool(name string) bool {
	switch name {
	case "memory_save", "memory_recall", "memory_delete", "memory_update", "memory_list":
		return true
	default:
		return false
	}
}
