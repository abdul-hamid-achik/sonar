package ui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

func TestPresentToolUsesStateSpecificActionLabels(t *testing.T) {
	tests := []struct {
		name  string
		kind  ToolCardKind
		state ToolCardState
		want  string
	}{
		{name: "read_file", kind: ToolCardFile, state: ToolCardRunning, want: "Reading"},
		{name: "read_file", kind: ToolCardFile, state: ToolCardSuccess, want: "Read"},
		{name: "read_file", kind: ToolCardFile, state: ToolCardError, want: "Read failed"},
		{name: "apply_patch", kind: ToolCardFile, state: ToolCardRunning, want: "Patching"},
		{name: "bash", kind: ToolCardBash, state: ToolCardSuccess, want: "Ran"},
		{name: "grep", kind: ToolCardSearch, state: ToolCardError, want: "Search failed"},
		{name: "memory_save", kind: ToolCardGeneric, state: ToolCardSuccess, want: "Saved memory"},
		{name: "consult_experts", kind: ToolCardGeneric, state: ToolCardRunning, want: "Consulting experts"},
		{name: "consult_experts", kind: ToolCardGeneric, state: ToolCardSuccess, want: "Consulted experts"},
		{name: "consult_experts", kind: ToolCardGeneric, state: ToolCardAttention, want: "Expert consultation is partial"},
		{name: "consult_experts", kind: ToolCardGeneric, state: ToolCardError, want: "Expert consultation failed"},
		{name: "load_skill", kind: ToolCardGeneric, state: ToolCardRunning, want: "Loading skill"},
		{name: "load_skill", kind: ToolCardGeneric, state: ToolCardSuccess, want: "Loaded skill"},
		{name: "hitspec_capture_webpage", kind: ToolCardGeneric, state: ToolCardSuccess, want: "Captured webpage artifact"},
		{name: "server__read-file", kind: ToolCardGeneric, state: ToolCardSuccess, want: "Read"},
		{name: "", kind: ToolCardSearch, state: ToolCardRunning, want: "Searching"},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/"+tt.want, func(t *testing.T) {
			got := presentTool(tt.name, tt.kind, tt.state)
			if got.label != tt.want {
				t.Fatalf("presentTool(%q, %v, %v) label = %q, want %q", tt.name, tt.kind, tt.state, got.label, tt.want)
			}
		})
	}
}

func TestPresentToolAttentionNeverFallsBackToSuccessCopy(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "read_file", want: "Needs attention"},
		{name: "apply_patch", want: "Needs attention"},
		{name: "fcheap_save", want: "Artifact save needs review"},
		{name: "hitspec_capture_webpage", want: "Webpage artifact needs review"},
		{name: "vault_get_secret", want: "Secret access needs attention"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels, ok := registeredToolLabels(tt.name)
			if !ok {
				t.Fatalf("tool %q is not registered", tt.name)
			}
			got := presentTool(tt.name, ToolCardGeneric, ToolCardAttention).label
			if got != tt.want {
				t.Fatalf("attention label = %q, want %q", got, tt.want)
			}
			if got == labels.success {
				t.Fatalf("attention label reused success copy %q", got)
			}
		})
	}
}

func TestPresentToolHumanizesUnknownIdentifiersSafely(t *testing.T) {
	tests := []struct {
		name  string
		kind  ToolCardKind
		state ToolCardState
		want  string
	}{
		{name: "custom_deploy-tool", kind: ToolCardGeneric, state: ToolCardRunning, want: "Running custom deploy tool"},
		{name: "custom-file-reader", kind: ToolCardFile, state: ToolCardSuccess, want: "Accessed custom file reader"},
		{name: "repository_lookup", kind: ToolCardSearch, state: ToolCardRunning, want: "Searching with repository lookup"},
		{name: "remote-sync", kind: ToolCardGeneric, state: ToolCardError, want: "Remote sync failed"},
		{name: "MCP_sync", kind: ToolCardGeneric, state: ToolCardRunning, want: "Running MCP sync"},
		{name: "\x1b[31mcustom_部署🙂-sync\x1b[0m", kind: ToolCardGeneric, state: ToolCardRunning, want: "Running custom 部署 sync"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := presentTool(tt.name, tt.kind, tt.state)
			if got.label != tt.want {
				t.Fatalf("label = %q, want %q", got.label, tt.want)
			}
			if !utf8.ValidString(got.label) {
				t.Fatalf("label is invalid UTF-8: %q", got.label)
			}
			if strings.Contains(got.label, "\x1b") || strings.Contains(got.label, "🙂") {
				t.Fatalf("label retained terminal control or emoji content: %q", got.label)
			}
		})
	}
}

func TestPresentToolBoundsLongUnicodeLabels(t *testing.T) {
	presentation := presentTool(strings.Repeat("部署_🙂-remote_sync_", 30), ToolCardGeneric, ToolCardRunning)
	if !utf8.ValidString(presentation.label) {
		t.Fatalf("label is invalid UTF-8: %q", presentation.label)
	}
	if got := lipgloss.Width(presentation.label); got > maxToolPresentationWidth {
		t.Fatalf("label width = %d, want <= %d: %q", got, maxToolPresentationWidth, presentation.label)
	}
	if strings.Contains(presentation.label, "🙂") {
		t.Fatalf("label retained emoji content: %q", presentation.label)
	}
}

func TestToolCardPresentationPreservesRawNameAndShowsItWhenExpanded(t *testing.T) {
	card := NewToolCard("write_file", ToolCardFile, true, defaultThemeID)
	originalName := card.Name
	card.State = ToolCardSuccess
	card.Duration = 250 * time.Millisecond
	card.Result = "saved"

	collapsed := card.View(48)
	firstLine := strings.Split(collapsed, "\n")[0]
	if !strings.Contains(firstLine, "Wrote") || strings.Contains(firstLine, "write_file") {
		t.Fatalf("collapsed header did not use the friendly receipt label:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "tool: write_file") {
		t.Fatalf("collapsed receipt exposed implementation detail:\n%s", collapsed)
	}

	card.Expanded = true
	expanded := card.View(48)
	if !strings.Contains(expanded, "tool: write_file") {
		t.Fatalf("expanded receipt omitted the raw tool identifier:\n%s", expanded)
	}
	if card.Name != originalName {
		t.Fatalf("rendering changed correlation name from %q to %q", originalName, card.Name)
	}

	card.State = ToolCardError
	card.Result = "permission denied"
	failed := card.View(48)
	if !strings.Contains(failed, "Write failed") || !strings.Contains(failed, "tool: write_file") {
		t.Fatalf("expanded failure omitted its friendly label or raw identifier:\n%s", failed)
	}
	for _, width := range []int{4, 12, 24, 48} {
		assertToolCardLinesFit(t, card.View(width), width)
	}
}
