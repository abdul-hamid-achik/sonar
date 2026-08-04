package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractDescription(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "first non-header non-empty line",
			content: "# Title\n\nThis is the description.\nMore text.",
			want:    "This is the description.",
		},
		{
			name:    "header only content",
			content: "# Title\n## Subtitle\n### Another",
			want:    "",
		},
		{
			name:    "empty content",
			content: "",
			want:    "",
		},
		{
			name:    "whitespace around description",
			content: "# Title\n\n  Indented description  \n",
			want:    "Indented description",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractDescription(tt.content)
			if got != tt.want {
				t.Errorf("extractDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want int // expected number of lines
	}{
		{name: "normal lines", s: "a\nb\nc", want: 3},
		{name: "empty string", s: "", want: 1},
		{name: "trailing newline", s: "a\nb\n", want: 3},
		{name: "single line", s: "hello", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitLines(tt.s)
			if len(got) != tt.want {
				t.Errorf("splitLines(%q) returned %d lines, want %d (lines: %v)", tt.s, len(got), tt.want, got)
			}
		})
	}
}

func TestTrimWhitespace(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want string
	}{
		{name: "tabs", s: "\thello\t", want: "hello"},
		{name: "spaces", s: "  hello  ", want: "hello"},
		{name: "mixed", s: "\t hello \t", want: "hello"},
		{name: "already trimmed", s: "hello", want: "hello"},
		{name: "empty", s: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trimWhitespace(tt.s)
			if got != tt.want {
				t.Errorf("trimWhitespace(%q) = %q, want %q", tt.s, got, tt.want)
			}
		})
	}
}

func TestStartsWith(t *testing.T) {
	tests := []struct {
		name   string
		s      string
		prefix string
		want   bool
	}{
		{name: "match", s: "hello world", prefix: "hello", want: true},
		{name: "no match", s: "hello world", prefix: "world", want: false},
		{name: "empty prefix", s: "hello", prefix: "", want: true},
		{name: "longer prefix", s: "hi", prefix: "hello", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := startsWith(tt.s, tt.prefix)
			if got != tt.want {
				t.Errorf("startsWith(%q, %q) = %v, want %v", tt.s, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestLoadAgentsDir(t *testing.T) {
	t.Run("valid temp structure with agent", func(t *testing.T) {
		tmp := t.TempDir()

		// Create agents/test-agent/agent.yaml
		agentDir := filepath.Join(tmp, "agents", "test-agent")
		if err := os.MkdirAll(agentDir, 0755); err != nil {
			t.Fatal(err)
		}
		agentYAML := `name: test-agent
description: A test agent
model: qwen3.5:0.8b
`
		if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(agentYAML), 0644); err != nil {
			t.Fatal(err)
		}

		dir, err := LoadAgentsDir(tmp)
		if err != nil {
			t.Fatalf("LoadAgentsDir() error: %v", err)
		}
		if dir.Path != tmp {
			t.Errorf("Path = %q, want %q", dir.Path, tmp)
		}
		if len(dir.Agents) != 1 {
			t.Errorf("expected 1 agent, got %d", len(dir.Agents))
		}
		agent, ok := dir.Agents["test-agent"]
		if !ok {
			t.Fatal("expected agent 'test-agent' to exist")
		}
		if agent.Description != "A test agent" {
			t.Errorf("agent description = %q, want %q", agent.Description, "A test agent")
		}
	})

	t.Run("empty path uses FindAgentsDir", func(t *testing.T) {
		dir, err := LoadAgentsDir("")
		if err != nil {
			t.Fatalf("LoadAgentsDir('') error: %v", err)
		}
		// Should return a valid AgentsDir (possibly with no agents)
		if dir == nil {
			t.Fatal("expected non-nil AgentsDir")
		}
		if dir.Agents == nil {
			t.Error("expected Agents map to be initialized")
		}
	})

	t.Run("nonexistent subdirs dont error", func(t *testing.T) {
		tmp := t.TempDir()
		// Empty temp dir — no agents/, skills/, mcp.json, etc.
		dir, err := LoadAgentsDir(tmp)
		if err != nil {
			t.Fatalf("LoadAgentsDir() error: %v", err)
		}
		if len(dir.Agents) != 0 {
			t.Errorf("expected 0 agents, got %d", len(dir.Agents))
		}
	})

	t.Run("loads legacy mcp_servers key", func(t *testing.T) {
		tmp := t.TempDir()
		mcpJSON := `{"mcp_servers":[{"name":"cortex","command":"cortex","args":["serve"]}]}`
		if err := os.WriteFile(filepath.Join(tmp, "mcp.json"), []byte(mcpJSON), 0644); err != nil {
			t.Fatal(err)
		}

		dir, err := LoadAgentsDir(tmp)
		if err != nil {
			t.Fatalf("LoadAgentsDir() error: %v", err)
		}
		if len(dir.MCPServers) != 1 || dir.MCPServers[0].Name != "cortex" {
			t.Fatalf("MCPServers = %#v, want cortex", dir.MCPServers)
		}
	})

	t.Run("servers key wins when both keys are present", func(t *testing.T) {
		tmp := t.TempDir()
		mcpJSON := `{"servers":[{"name":"canonical"}],"mcp_servers":[{"name":"legacy"}]}`
		if err := os.WriteFile(filepath.Join(tmp, "mcp.json"), []byte(mcpJSON), 0644); err != nil {
			t.Fatal(err)
		}

		dir, err := LoadAgentsDir(tmp)
		if err != nil {
			t.Fatalf("LoadAgentsDir() error: %v", err)
		}
		if len(dir.MCPServers) != 1 || dir.MCPServers[0].Name != "canonical" {
			t.Fatalf("MCPServers = %#v, want canonical server", dir.MCPServers)
		}
	})
}
