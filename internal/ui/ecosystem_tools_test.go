package ui

import (
	"strings"
	"testing"
)

func TestEcosystemToolSummaryUnderstandsDirectAndLazyCalls(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want []string
	}{
		{
			name: "cortex__cortex_status",
			args: map[string]any{"taskId": "task_123", "workspace": "/private/repo"},
			want: []string{"task task_123"},
		},
		{
			name: "bob__bob_validate_manifest",
			args: map[string]any{"manifest_yaml": "product:\n  token: secret"},
			want: []string{"inline manifest"},
		},
		{
			name: "monitor__monitor_analyze",
			args: map[string]any{"pid": 42, "window_seconds": 10},
			want: []string{"PID 42", "10s window"},
		},
		{
			name: "mcphub__mcphub_call_tool",
			args: map[string]any{
				"server": "cortex", "tool": "cortex_status",
				"arguments": map[string]any{"taskId": "task_456", "api_token": "do-not-render"},
			},
			want: []string{"Cortex", "status"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ecosystemToolSummary(tt.name, tt.args)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("summary %q missing %q", got, want)
				}
			}
			for _, secret := range []string{"secret", "do-not-render", "api_token"} {
				if strings.Contains(got, secret) {
					t.Fatalf("summary exposed non-allowlisted payload: %q", got)
				}
			}
		})
	}
}

func TestToolSummaryUsesEcosystemProjectionBeforeGenericFallback(t *testing.T) {
	entry := ToolEntry{
		Name: "mcphub__mcphub_call_tool",
		RawArgs: map[string]any{
			"tool":      "monitor__monitor_processes",
			"arguments": map[string]any{"filter": "sonar", "sort_by": "rss"},
		},
	}
	got := toolSummary(ToolTypeDefault, entry)
	for _, want := range []string{"Monitor", "processes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("tool summary %q missing %q", got, want)
		}
	}
	for _, private := range []string{"sonar", "rss", "filter"} {
		if strings.Contains(got, private) {
			t.Fatalf("tool summary %q exposed nested MCP argument %q", got, private)
		}
	}
}

func TestExpertConsultationSummaryShowsStrategyAndCountWithoutObjectiveOrNames(t *testing.T) {
	args := map[string]any{
		"strategy":  "SWARM",
		"objective": "secret incident details",
		"experts":   []any{"private-security-name", "private-reviewer-name"},
	}
	got := ecosystemToolSummary("consult_experts", args)
	if got != "swarm · 2 requested" {
		t.Fatalf("expert summary = %q", got)
	}
	for _, private := range []string{"secret", "incident", "private-security-name", "private-reviewer-name"} {
		if strings.Contains(got, private) {
			t.Fatalf("expert summary exposed %q: %q", private, got)
		}
	}
	if got := ecosystemToolSummary("consult_experts", map[string]any{"strategy": "moe", "objective": "review"}); got != "moe · auto-selected" {
		t.Fatalf("auto-selected expert summary = %q", got)
	}
}

func TestCompactToolFailureExtractsJSONAndOffersRecovery(t *testing.T) {
	tests := []struct {
		name   string
		result string
		want   []string
		not    []string
	}{
		{
			name:   "mcphub__mcphub_call_tool",
			result: `{"error":{"message":"connection refused"},"arguments":{"token":"secret"}}`,
			want:   []string{"MCPHub", "Connection unavailable", "Runtime", "reconnect"},
			not:    []string{"secret", "arguments"},
		},
		{
			name:   "cortex__cortex_verify",
			result: `{"ok":false,"error":{"code":"invalid","message":"taskId is required"}}`,
			want:   []string{"Cortex", "taskId is required"},
			not:    []string{"\"ok\"", "\"code\""},
		},
		{
			name:   "bob__bob_check",
			result: "MCP tool ended without a receipt: deadline exceeded",
			want:   []string{"Bob", "Outcome unknown", "inspect"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compactToolFailure(tt.name, tt.result)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("failure %q missing %q", got, want)
				}
			}
			for _, unwanted := range tt.not {
				if strings.Contains(got, unwanted) {
					t.Fatalf("failure %q contains %q", got, unwanted)
				}
			}
		})
	}
}

func TestToolCardKeepsCompactErrorAndExpandedRawDetail(t *testing.T) {
	card := NewToolCard("cortex__cortex_status", ToolCardGeneric, true, defaultThemeID)
	card.State = ToolCardError
	card.Result = `{"error":{"message":"connection refused"},"debug":"raw detail"}`

	collapsed := card.View(80)
	if !strings.Contains(collapsed, "Connection unavailable") || strings.Contains(collapsed, "raw detail") {
		t.Fatalf("collapsed error is not compact:\n%s", collapsed)
	}

	card.Expanded = true
	expanded := card.View(80)
	for _, want := range []string{"Connection unavailable", "details:", "raw detail"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded error missing %q:\n%s", want, expanded)
		}
	}
}
