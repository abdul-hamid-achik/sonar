package tools

import "github.com/abdul-hamid-achik/sonar/internal/llm"

func GrepToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "grep",
		Description: "Search for a pattern in files. Use this to find code, text, or values across multiple files.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "The regex pattern to search for.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path to search in (defaults to current directory).",
				},
				"include": map[string]any{
					"type":        "string",
					"description": "File pattern to include (e.g., '*.go', '*.ts').",
				},
				"context": map[string]any{
					"type":        "integer",
					"description": "Number of lines of context to show around matches (default: 3).",
				},
			},
			"required": []string{"pattern"},
		},
	}
}

func ReadToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "read",
		Description: "Read the contents of a file. Use this to view source code, configuration files, or any text file.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to read.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of lines to read (optional).",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Line number to start reading from (optional, 1-indexed).",
				},
			},
			"required": []string{"path"},
		},
	}
}

func WriteToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "write",
		Description: "Atomically write complete content to one file. Use this to create or overwrite a file; parent directories are created as needed. Submit the complete intended file in one call—do not split content merely to fit an approval display.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to write.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Content to write to the file.",
				},
			},
			"required": []string{"path", "content"},
		},
	}
}

func GlobToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "glob",
		Description: "Find files matching a pattern. Use this to discover files by name patterns like '*.go', '**/*.ts', etc.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Glob pattern to match (e.g., '**/*.go', 'src/**/*.ts').",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory to search in (defaults to current directory).",
				},
			},
			"required": []string{"pattern"},
		},
	}
}

func BashToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "bash",
		Description: "Execute a shell command. Use this to run git, npm, go, or other command-line tools. Output is returned after completion.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to execute.",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Timeout in seconds (default: 30, max: 120).",
				},
			},
			"required": []string{"command"},
		},
	}
}

func LsToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "ls",
		Description: "List files and directories. Use this to see what's in a directory.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory path to list (defaults to current directory).",
				},
			},
		},
	}
}

func FindToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "find",
		Description: "Find files or directories by name. Use this to locate specific files when you know all or part of the filename.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Name or pattern to search for (supports * and ? wildcards).",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory to search in (defaults to current directory).",
				},
				"type": map[string]any{
					"type":        "string",
					"description": "Type to find: 'f' for files, 'd' for directories (default: both).",
				},
			},
			"required": []string{"name"},
		},
	}
}

func DiffToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "diff",
		Description: "Show the differences between the current file content and new content. Use this to preview changes before writing.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to diff.",
				},
				"new_content": map[string]any{
					"type":        "string",
					"description": "The new content to compare against the current file.",
				},
			},
			"required": []string{"path", "new_content"},
		},
	}
}

func EditToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "edit",
		Description: "Atomically apply a complete unified-diff patch to one file. Use this for targeted edits without overwriting the entire file. Submit all related hunks together—do not split a coherent patch merely to fit an approval display. Format: @@ -start,count +new_start,new_count @@ followed by - (remove), + (add), or space (context) lines.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to edit.",
				},
				"patch": map[string]any{
					"type":        "string",
					"description": "Unified diff patch to apply. Format: @@ -start,count +new_start,new_count @@ followed by -line (remove), +line (add), or context line.",
				},
			},
			"required": []string{"path", "patch"},
		},
	}
}

func MkdirToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "mkdir",
		Description: "Create one or more directories. Creates parent directories as needed.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the directory to create.",
				},
			},
			"required": []string{"path"},
		},
	}
}

func RemoveToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "remove",
		Description: "Remove files or directories. Use with caution - this permanently deletes files.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to remove (file or directory).",
				},
				"recursive": map[string]any{
					"type":        "boolean",
					"description": "Remove directories recursively (default: false).",
				},
				"force": map[string]any{
					"type":        "boolean",
					"description": "Ignore nonexistent files (default: false).",
				},
			},
			"required": []string{"path"},
		},
	}
}

func CopyToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "copy",
		Description: "Copy a file from source to destination.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"source": map[string]any{
					"type":        "string",
					"description": "Source path to copy from.",
				},
				"destination": map[string]any{
					"type":        "string",
					"description": "Destination path to copy to.",
				},
			},
			"required": []string{"source", "destination"},
		},
	}
}

func MoveToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "move",
		Description: "Move or rename a file or directory.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"source": map[string]any{
					"type":        "string",
					"description": "Source path to move from.",
				},
				"destination": map[string]any{
					"type":        "string",
					"description": "Destination path to move to.",
				},
			},
			"required": []string{"source", "destination"},
		},
	}
}

func ExistsToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "exists",
		Description: "Check if a file or directory exists and get information about it.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to check.",
				},
			},
			"required": []string{"path"},
		},
	}
}

// LoadSkillToolDef describes progressive, model-selected skill loading. The
// host resolves only an exact name from its already-discovered catalog.
func LoadSkillToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "load_skill",
		Description: "Load the body of one available skill by its exact catalog name before acting when that skill clearly matches the task. This is read-only and does not activate the skill globally.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Exact skill name from the Available Skills catalog.",
				},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
	}
}

// ConsultExpertsToolDef describes application-level advisory fan-out. The
// host supplies no tools to child experts and retains all effect authority in
// the parent Agent.
func ConsultExpertsToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "consult_experts",
		Description: "Run a bounded read-only team, swarm, or application-level MoE of tool-free profiles. Guaranteed built-ins are architect, critic, explorer, generalist, and verifier; the host may expose more. Use for explicit expert requests or hard decisions needing distinct perspectives; omit experts for automatic selection unless exact profile names were supplied, never invent personas, and have the parent verify claims.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"strategy": map[string]any{
					"type":        "string",
					"enum":        []string{"team", "swarm", "moe"},
					"description": "team uses a stable group; swarm favors diversity; moe selects best matches.",
				},
				"objective": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   32768,
					"description": "Bounded question every selected expert analyzes.",
				},
				"experts": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
					"maxItems":    16,
					"description": "Optional exact host-provided profile names: team exact, swarm seeds, moe fallback. Guaranteed built-ins are architect, critic, explorer, generalist, and verifier; the host may expose more. Omit this field for automatic selection; never invent profile names or role descriptions.",
				},
				"model": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   256,
					"description": "Optional exact model from the active Ollama inventory, not a provider profile or arbitrary remote model; host inventory, consent, and resource policy still apply. Omit to inherit profile/current-model routing.",
				},
				"model_overrides": map[string]any{
					"type":     "array",
					"maxItems": 16,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"expert": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
							"model":  map[string]any{"type": "string", "minLength": 1, "maxLength": 256},
						},
						"required":             []string{"expert", "model"},
						"additionalProperties": false,
					},
					"description": "Optional exact per-profile models; host policy still applies.",
				},
			},
			"required":             []string{"strategy", "objective"},
			"additionalProperties": false,
		},
	}
}
