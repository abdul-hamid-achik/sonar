// Package integration provides end-to-end integration tests for sonar.
// These tests verify the full flow from TUI initialization through agent execution.
// Run with: go test -tags=integration ./internal/integration/...
//go:build integration
// +build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/abdul-hamid-achik/sonar/internal/ui"
)

// skipIfNoOllama skips the test if Ollama is not running.
func skipIfNoOllama(t *testing.T) {
	if os.Getenv("OLLAMA_HOST") == "" {
		// Try default host
		os.Setenv("OLLAMA_HOST", "http://localhost:11434")
	}

	client := llm.NewModelManager(os.Getenv("OLLAMA_HOST"), 262144)
	if err := client.SetCurrentModel("qwen3.5:2b"); err != nil {
		t.Skipf("failed to initialize ollama client: %v", err)
	}

	if err := client.Ping(); err != nil {
		t.Skip("Ollama not available: skipping integration test")
	}
}

// TestTUI_Initialization verifies the TUI can initialize successfully
func TestTUI_Initialization(t *testing.T) {
	skipIfNoOllama(t)

	reg := command.NewRegistry()
	command.RegisterBuiltins(reg)

	cfg := config.DefaultModelConfig()
	router := config.NewRouter(&cfg)

	modelManager := llm.NewModelManager("http://localhost:11434", 262144)
	modelManager.SetCurrentModel("qwen3.5:2b")

	ag := agent.New(modelManager, mcp.NewRegistry(), 262144)
	ag.SetRouter(router)

	completer := ui.NewCompleter(reg, []string{"qwen3.5:2b"}, nil, nil, nil)
	m := ui.New(ag, reg, nil, completer, modelManager, router, nil)

	// Simulate window size
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*ui.Model)

	if !m.Ready() {
		t.Error("TUI should be ready after WindowSizeMsg")
	}
}

// TestTUI_ScrollAnchorDuringStreaming verifies scroll anchor works during streaming
func TestTUI_ScrollAnchorDuringStreaming(t *testing.T) {
	skipIfNoOllama(t)

	reg := command.NewRegistry()
	command.RegisterBuiltins(reg)

	cfg := config.DefaultModelConfig()
	router := config.NewRouter(&cfg)

	modelManager := llm.NewModelManager("http://localhost:11434", 262144)
	ag := agent.New(modelManager, mcp.NewRegistry(), 262144)
	ag.SetRouter(router)

	completer := ui.NewCompleter(reg, []string{"qwen3.5:2b"}, nil, nil, nil)
	m := ui.New(ag, reg, nil, completer, modelManager, router, nil)

	// Initialize
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*ui.Model)

	// Verify anchor is active initially
	if !m.AnchorActive() {
		t.Error("anchorActive should be true after initialization")
	}

	// Simulate streaming
	updated, _ = m.Update(ui.StreamTextMsg{Text: "Hello"})
	m = updated.(*ui.Model)

	// Anchor should still be active
	if !m.AnchorActive() {
		t.Error("anchorActive should remain true during streaming")
	}
}

// TestTUI_OverlayRendering verifies overlays render without crashing
func TestTUI_OverlayRendering(t *testing.T) {
	reg := command.NewRegistry()
	command.RegisterBuiltins(reg)

	cfg := config.DefaultModelConfig()
	router := config.NewRouter(&cfg)

	modelManager := llm.NewModelManager("http://localhost:11434", 262144)
	ag := agent.New(modelManager, mcp.NewRegistry(), 262144)
	ag.SetRouter(router)

	completer := ui.NewCompleter(reg, []string{"qwen3.5:2b"}, nil, nil, nil)
	m := ui.New(ag, reg, nil, completer, modelManager, router, nil)

	// Initialize
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*ui.Model)

	// Test help overlay
	updated, _ = m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	m = updated.(*ui.Model)

	view := m.View()
	if view.Content == "" {
		t.Error("View should not be empty")
	}

	// Close overlay
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(*ui.Model)
}

// TestTUI_ToolCardRendering verifies tool cards render correctly
func TestTUI_ToolCardRendering(t *testing.T) {
	reg := command.NewRegistry()
	command.RegisterBuiltins(reg)

	cfg := config.DefaultModelConfig()
	router := config.NewRouter(&cfg)

	modelManager := llm.NewModelManager("http://localhost:11434", 262144)
	ag := agent.New(modelManager, mcp.NewRegistry(), 262144)
	ag.SetRouter(router)

	completer := ui.NewCompleter(reg, []string{"qwen3.5:2b"}, nil, nil, nil)
	m := ui.New(ag, reg, nil, completer, modelManager, router, nil)

	// Initialize
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*ui.Model)

	// Simulate tool call
	startTime := time.Now()
	updated, _ = m.Update(ui.ToolCallStartMsg{
		Name:      "read_file",
		Args:      map[string]any{"path": "test.go"},
		StartTime: startTime,
	})
	m = updated.(*ui.Model)

	// Simulate tool result
	updated, _ = m.Update(ui.ToolCallResultMsg{
		Name:     "read_file",
		Result:   "file content",
		IsError:  false,
		Duration: 100 * time.Millisecond,
	})
	m = updated.(*ui.Model)

	// Verify view renders without panic
	view := m.View()
	if view.Content == "" {
		t.Error("View should not be empty after tool execution")
	}
}

// TestQwenRouter_Integration verifies Qwen router classification
func TestQwenRouter_Integration(t *testing.T) {
	cfg := config.DefaultModelConfig()
	router := config.NewQwenModelRouter(&cfg)

	tests := []struct {
		query         string
		mode          config.ModeContext
		expectSmaller string // model should be this or smaller
		expectLarger  string // model should be this or larger
	}{
		{"what is go?", config.ModeAskContext, "qwen3.5:2b", ""},
		{"design architecture", config.ModeBuildContext, "", "qwen3.5:4b"},
		{"plan the system", config.ModePlanContext, "", "qwen3.5:4b"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			model := router.SelectModelForMode(tt.query, tt.mode)

			if tt.expectSmaller != "" {
				// Verify model is not larger than expected
				if modelRank(model) > modelRank(tt.expectSmaller) {
					t.Errorf("model %s is larger than expected %s", model, tt.expectSmaller)
				}
			}

			if tt.expectLarger != "" {
				// Verify model is not smaller than expected
				if modelRank(model) < modelRank(tt.expectLarger) {
					t.Errorf("model %s is smaller than expected %s", model, tt.expectLarger)
				}
			}
		})
	}
}

// modelRank returns a numeric rank for model comparison (higher = larger)
func modelRank(model string) int {
	switch {
	case strings.Contains(model, "0.8b"):
		return 1
	case strings.Contains(model, "2b"):
		return 2
	case strings.Contains(model, "4b"):
		return 3
	case strings.Contains(model, "9b"):
		return 4
	default:
		return 0
	}
}

// TestFileOperations_Integration verifies file operations work end-to-end
func TestFileOperations_Integration(t *testing.T) {
	skipIfNoOllama(t)

	// Create temp directory for test
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	// Write test file
	if err := os.WriteFile(testFile, []byte("hello world"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Verify file exists
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("failed to read test file: %v", err)
	}

	if string(content) != "hello world" {
		t.Errorf("unexpected file content: %q", string(content))
	}
}

// BenchmarkTUI_Rendering benchmarks TUI rendering performance
func BenchmarkTUI_Render(b *testing.B) {
	reg := command.NewRegistry()
	command.RegisterBuiltins(reg)

	cfg := config.DefaultModelConfig()
	router := config.NewRouter(&cfg)

	modelManager := llm.NewModelManager("http://localhost:11434", 262144)
	ag := agent.New(modelManager, mcp.NewRegistry(), 262144)
	ag.SetRouter(router)

	completer := ui.NewCompleter(reg, []string{"qwen3.5:2b"}, nil, nil, nil)
	m := ui.New(ag, reg, nil, completer, modelManager, router, nil)

	// Initialize
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*ui.Model)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// BenchmarkQwenRouter_Classification benchmarks Qwen router performance
func BenchmarkQwenRouter_Classification(b *testing.B) {
	cfg := config.DefaultModelConfig()
	router := config.NewQwenModelRouter(&cfg)

	queries := []string{
		"what is go",
		"how do i create a file",
		"debug this nil pointer error",
		"design microservice architecture",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, q := range queries {
			_ = router.SelectModelForMode(q, config.ModeAskContext)
		}
	}
}
