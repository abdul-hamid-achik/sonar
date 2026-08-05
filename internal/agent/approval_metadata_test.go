package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

func TestMCPApprovalConsequenceExplainsWhyEffectfulCallsRemainGated(t *testing.T) {
	tests := []struct {
		name string
		meta llm.ToolBehavior
		want string
	}{
		{name: "unknown indirect call", want: "no effect metadata"},
		{name: "additive durable call", meta: llm.ToolBehavior{Declared: true}, want: "durable state"},
		{name: "destructive external call", meta: llm.ToolBehavior{Declared: true, Destructive: true, OpenWorld: true}, want: "destructive changes"},
		{name: "contradictory read destructive call", meta: llm.ToolBehavior{Declared: true, ReadOnly: true, Destructive: true}, want: "declares this read-only"},
		{name: "open-world read", meta: llm.ToolBehavior{Declared: true, ReadOnly: true, OpenWorld: true}, want: "external systems"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mcpApprovalConsequence(tt.meta); !strings.Contains(got, tt.want) {
				t.Fatalf("consequence = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCortexStartTaskApprovalConsequenceIsConservative(t *testing.T) {
	got := mcpApprovalConsequence(llm.ToolBehavior{
		Declared: true, ReadOnly: false, Destructive: false, OpenWorld: false,
	})
	for _, want := range []string{"server metadata", "durable state"} {
		if !strings.Contains(strings.ToLower(got), want) {
			t.Fatalf("cortex_start_task consequence = %q, want %q", got, want)
		}
	}
}

func TestBashApprovalExplainsWhyTheCommandStillNeedsADecision(t *testing.T) {
	ag := New(nil, nil, 0)
	preview := ag.buildApprovalPreview(context.Background(), AuthorityNormal, llm.ToolCall{
		Name: "bash", Arguments: map[string]any{"command": "rm -rf build"},
	}, "command-hash")
	if preview.Kind != permission.PreviewCommand || preview.Command != "rm -rf build" {
		t.Fatalf("bash preview = %#v", preview)
	}
	for _, want := range []string{"did not pre-authorize", "current turn", "change files", "external systems"} {
		if !strings.Contains(preview.Consequence, want) {
			t.Fatalf("bash consequence omitted %q: %q", want, preview.Consequence)
		}
	}
}

func TestAutoBashApprovalPreviewCarriesTheRuleThatTripped(t *testing.T) {
	workspace := t.TempDir()
	ag := New(nil, nil, 0)
	ag.SetWorkDir(workspace)

	tests := []struct {
		name       string
		mode       AuthorityMode
		command    string
		wantReason string
	}{
		{name: "dynamic expansion", mode: AuthorityAutoScoped, command: "echo $?", wantReason: "dynamic shell syntax ($?)"},
		{name: "workspace escape", mode: AuthorityAutoScoped, command: "cat " + filepath.Join(workspace, "..", "secret.txt"), wantReason: "operand outside the workspace"},
		{name: "uncatalogued executable", mode: AuthorityAutoScoped, command: "definitely-not-a-command --flag", wantReason: "executable outside the host catalog"},
		{name: "admitted command has no reason", mode: AuthorityAutoScoped, command: "go test ./...", wantReason: ""},
		{name: "normal mode never labels", mode: AuthorityNormal, command: "echo $?", wantReason: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preview := ag.buildApprovalPreview(context.Background(), tt.mode, llm.ToolCall{
				Name: "bash", Arguments: map[string]any{"command": tt.command},
			}, "request-hash")
			if preview.Reason != tt.wantReason {
				t.Fatalf("bash approval reason = %q, want %q (preview %#v)", preview.Reason, tt.wantReason, preview)
			}
		})
	}
}

func TestBuiltinApprovalPreviewsUseExactFilesystemVerbsAndConsequences(t *testing.T) {
	ag := New(nil, nil, 0)
	ag.SetWorkDir(t.TempDir())

	tests := []struct {
		name            string
		tool            string
		args            map[string]any
		wantAction      string
		wantConsequence []string
	}{
		{
			name:            "copy",
			tool:            "copy",
			args:            map[string]any{"source": "source.txt", "destination": "copy.txt"},
			wantAction:      "Copy file",
			wantConsequence: []string{"destination file", "source remains unchanged"},
		},
		{
			name:            "move",
			tool:            "move",
			args:            map[string]any{"source": "source.txt", "destination": "moved.txt"},
			wantAction:      "Move path",
			wantConsequence: []string{"source to the destination", "no longer exist"},
		},
		{
			name:            "remove recursively",
			tool:            "remove",
			args:            map[string]any{"path": "tree", "recursive": true},
			wantAction:      "Remove path",
			wantConsequence: []string{"target and its descendants"},
		},
		{
			name:            "create directory",
			tool:            "mkdir",
			args:            map[string]any{"path": "nested/dir"},
			wantAction:      "Create directory",
			wantConsequence: []string{"missing parent directories"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preview := ag.buildApprovalPreview(context.Background(), AuthorityNormal, llm.ToolCall{
				Name: tt.tool, Arguments: tt.args,
			}, "request-hash")
			if preview.Kind != permission.PreviewFilesystem || preview.ActionLabel != tt.wantAction {
				t.Fatalf("preview identity = %#v", preview)
			}
			for _, want := range tt.wantConsequence {
				if !strings.Contains(preview.Consequence, want) {
					t.Fatalf("consequence omitted %q: %q", want, preview.Consequence)
				}
			}
		})
	}
}

func TestBoundApprovalLabelIsUTF8SafeAndBounded(t *testing.T) {
	got := boundApprovalLabel(strings.Repeat("界", 100))
	if len(got) > 160 || !strings.HasSuffix(got, "...") {
		t.Fatalf("bounded label bytes=%d value=%q", len(got), got)
	}
}
