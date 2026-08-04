package ui

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

func TestFormatToolResultSanitizesFallbackOutput(t *testing.T) {
	result := "safe\x1b]52;c;TOOL_SECRET\x07\n\x1b[2Jnext\u202e\u2066"
	formatted := formatToolResult(result, 20, 80)
	if strings.Contains(formatted, "TOOL_SECRET") || !strings.Contains(formatted, "safe\nnext") {
		t.Fatalf("sanitized fallback result = %q", formatted)
	}
	for _, character := range formatted {
		if character == '\n' || character == '\t' {
			continue
		}
		if unicode.IsControl(character) || isBidiControl(character) {
			t.Fatalf("unsafe rune %U survived fallback result: %q", character, formatted)
		}
	}
}

func TestFormatToolResultTruncatesUnicodeByDisplayWidth(t *testing.T) {
	formatted := formatToolResult("界界界", 20, 5)
	if !strings.Contains(formatted, "…") || !strings.HasPrefix(formatted, "界界") {
		t.Fatalf("unicode truncation = %q", formatted)
	}
	if !utf8.ValidString(formatted) {
		t.Fatalf("unicode truncation produced invalid UTF-8: %q", formatted)
	}
}

func TestClassifyTool(t *testing.T) {
	tests := []struct {
		name     string
		expected ToolType
	}{
		{"bash", ToolTypeBash},
		{"execute_bash", ToolTypeBash},
		{"shell_exec", ToolTypeBash},
		{"run_command", ToolTypeBash},
		{"read_file", ToolTypeFileRead},
		{"file_view", ToolTypeFileRead},
		{"cat_file", ToolTypeFileRead},
		{"write_file", ToolTypeFileWrite},
		{"edit_file", ToolTypeFileWrite},
		{"create_file", ToolTypeFileWrite},
		{"apply_patch", ToolTypeFileWrite},
		{"web_search", ToolTypeWeb},
		{"fetch_url", ToolTypeWeb},
		{"http_get", ToolTypeWeb},
		{"curl", ToolTypeWeb},
		{"browse_page", ToolTypeWeb},
		{"memory_store", ToolTypeMemory},
		{"remember_fact", ToolTypeMemory},
		{"forget_key", ToolTypeMemory},
		{"list_tools", ToolTypeDefault},
		{"unknown", ToolTypeDefault},
		{"search", ToolTypeSearch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyTool(tt.name)
			if got != tt.expected {
				t.Errorf("classifyTool(%q) = %d, want %d", tt.name, got, tt.expected)
			}
		})
	}
}

func TestClassifyToolRejectsSuggestiveLookalikesAndUnprojectedRoutes(t *testing.T) {
	for _, name := range []string{
		"command_status",
		"readiness_probe",
		"fetcher",
		"findings",
		"writeup",
		"memory_pressure",
		"evil__read_file",
		"server__exec_command",
	} {
		if got := classifyTool(name); got != ToolTypeDefault {
			t.Fatalf("classifyTool(%q) = %d, want conservative default", name, got)
		}
	}
}

func TestClassifyProjectedToolUsesOnlyExactNormalizedRoute(t *testing.T) {
	readRoute := ecosystem.ProjectToolCall("files__read-file", nil)
	if got := classifyProjectedTool("files__read-file", readRoute); got != ToolTypeFileRead {
		t.Fatalf("exact projected read route = %d, want %d", got, ToolTypeFileRead)
	}

	lazyExec := ecosystem.ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "runner",
		"tool":   "exec_command",
	})
	if got := classifyProjectedTool("mcphub__mcphub_call_tool", lazyExec); got != ToolTypeBash {
		t.Fatalf("exact lazy exec route = %d, want %d", got, ToolTypeBash)
	}

	untyped := ecosystem.ToolProjection{Operation: "read_file"}.Normalize()
	if got := classifyProjectedTool("files__read_file", untyped); got != ToolTypeDefault {
		t.Fatalf("unrouted namespaced read = %d, want conservative default", got)
	}

	lookalike := ecosystem.ProjectToolCall("files__readiness_probe", nil)
	if got := classifyProjectedTool("files__readiness_probe", lookalike); got != ToolTypeDefault {
		t.Fatalf("projected lookalike = %d, want conservative default", got)
	}
}

func TestToolIcon(t *testing.T) {
	tests := []struct {
		tt       ToolType
		status   ToolStatus
		expected string
	}{
		// Error always returns ✗
		{ToolTypeBash, ToolStatusError, "✗"},
		{ToolTypeFileRead, ToolStatusError, "✗"},
		{ToolTypeDefault, ToolStatusError, "✗"},

		// Running always returns ⚙
		{ToolTypeBash, ToolStatusRunning, "⚙"},
		{ToolTypeFileRead, ToolStatusRunning, "⚙"},
		{ToolTypeDefault, ToolStatusRunning, "⚙"},

		// Done returns type-specific icons
		{ToolTypeBash, ToolStatusDone, "$"},
		{ToolTypeFileRead, ToolStatusDone, "◎"},
		{ToolTypeFileWrite, ToolStatusDone, "✎"},
		{ToolTypeWeb, ToolStatusDone, "◆"},
		{ToolTypeMemory, ToolStatusDone, "◈"},
		{ToolTypeDefault, ToolStatusDone, "✓"},
	}

	for _, tt := range tests {
		got := toolIcon(tt.tt, tt.status)
		if got != tt.expected {
			t.Errorf("toolIcon(%d, %d) = %q, want %q", tt.tt, tt.status, got, tt.expected)
		}
	}
}

func TestToolSummary(t *testing.T) {
	t.Run("bash_command", func(t *testing.T) {
		te := ToolEntry{
			RawArgs: map[string]any{"command": "ls -la"},
		}
		got := toolSummary(ToolTypeBash, te)
		if got != "ls -la" {
			t.Errorf("expected 'ls -la', got %q", got)
		}
	})

	t.Run("bash_long_command_truncated", func(t *testing.T) {
		longCmd := "this is a very long command that should be truncated because it is longer than sixty characters total"
		te := ToolEntry{
			RawArgs: map[string]any{"command": longCmd},
		}
		got := toolSummary(ToolTypeBash, te)
		if lipgloss.Width(got) > 60 {
			t.Errorf("expected truncated to 60 display cells, got %d", lipgloss.Width(got))
		}
		if !strings.HasSuffix(got, "…") {
			t.Error("truncated command should end with a Unicode ellipsis")
		}
	})

	t.Run("file_read_path", func(t *testing.T) {
		te := ToolEntry{
			RawArgs: map[string]any{"file_path": "/home/user/test.go"},
		}
		got := toolSummary(ToolTypeFileRead, te)
		if got != "/home/user/test.go" {
			t.Errorf("expected path, got %q", got)
		}
	})

	t.Run("file_write_path", func(t *testing.T) {
		te := ToolEntry{
			RawArgs: map[string]any{"path": "/tmp/output.txt"},
		}
		got := toolSummary(ToolTypeFileWrite, te)
		if got != "/tmp/output.txt" {
			t.Errorf("expected path, got %q", got)
		}
	})

	t.Run("web_url", func(t *testing.T) {
		te := ToolEntry{
			RawArgs: map[string]any{"url": "https://example.com"},
		}
		got := toolSummary(ToolTypeWeb, te)
		if got != "https://example.com" {
			t.Errorf("expected url, got %q", got)
		}
	})

	t.Run("memory_key", func(t *testing.T) {
		te := ToolEntry{
			RawArgs: map[string]any{"key": "user_pref"},
		}
		got := toolSummary(ToolTypeMemory, te)
		if got != "user_pref" {
			t.Errorf("expected 'user_pref', got %q", got)
		}
	})

	t.Run("nil_args", func(t *testing.T) {
		te := ToolEntry{RawArgs: nil}
		got := toolSummary(ToolTypeBash, te)
		if got != "" {
			t.Errorf("nil args should return empty, got %q", got)
		}
	})

	t.Run("default_type_returns_empty", func(t *testing.T) {
		te := ToolEntry{
			RawArgs: map[string]any{"foo": "bar"},
		}
		got := toolSummary(ToolTypeDefault, te)
		if got != "" {
			t.Errorf("default type should return empty, got %q", got)
		}
	})
}
