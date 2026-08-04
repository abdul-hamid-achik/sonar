package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/charmbracelet/x/ansi"
)

func TestRunningHitspecRoutesKeepSpecialistWithoutRepeatingAction(t *testing.T) {
	tests := []struct {
		tool      string
		wantLabel string
	}{
		{tool: "hitspec_search_web", wantLabel: "Searching the public web"},
		{tool: "hitspec_capture_webpage", wantLabel: "Capturing webpage artifact"},
	}

	for _, test := range tests {
		t.Run(test.tool, func(t *testing.T) {
			args := map[string]any{
				"server": "hitspec",
				"tool":   test.tool,
				// Nested values are deliberately private and must not enter the
				// ambient card even while the route remains recognizable.
				"arguments": map[string]any{"query": "private query", "token": "secret"},
			}
			card := NewToolCard("mcphub__mcphub_call_tool", ToolCardGeneric, true, defaultThemeID)
			card.Projection = ecosystem.ProjectToolCall(card.Name, args)
			card.SetSummary(ecosystemToolSummary(card.Name, args))

			plain := ansi.Strip(card.View(84))
			for _, want := range []string{test.wantLabel, "Hitspec"} {
				if !strings.Contains(plain, want) {
					t.Fatalf("running Hitspec receipt omitted %q:\n%s", want, plain)
				}
			}
			if strings.Contains(plain, "Hitspec · "+friendlyRemoteAction(test.tool)) {
				t.Fatalf("running Hitspec receipt repeated its semantic action:\n%s", plain)
			}
			for _, private := range []string{"private query", "secret", "token"} {
				if strings.Contains(plain, private) {
					t.Fatalf("running Hitspec receipt exposed %q:\n%s", private, plain)
				}
			}
			assertToolCardLinesFit(t, card.View(84), 84)
		})
	}
}

func TestCompletedHitspecCaptureShowsBoundedArtifactReceipt(t *testing.T) {
	args := map[string]any{
		"server": "hitspec", "tool": "hitspec_capture_webpage",
		"arguments": map[string]any{"url": "https://example.com/docs?token=secret"},
	}
	projection := ecosystem.ProjectReceipt(
		ecosystem.ProjectToolCall("mcphub__mcphub_call_tool", args),
		ecosystem.RawReceipt{Structured: json.RawMessage(`{
			"url":"https://example.com/docs",
			"final_url":"https://example.com/docs",
			"title":"private title","http_status":200,"content_type":"text/html","markdown_bytes":321,
			"stash":{"id":"stash-ui-123","status":"saved","created_at":"2026-07-14T12:00:00Z",
				"file_count":1,"total_size":321,"indexed":true,"index_requested":true}
		}`)},
	)

	card := NewToolCard("mcphub__mcphub_call_tool", ToolCardGeneric, true, defaultThemeID)
	card.State = toolCardStateFromProjection(projection)
	card.Projection = projection
	card.Expanded = true

	plain := ansi.Strip(card.View(84))
	for _, want := range []string{
		"Captured webpage artifact",
		"specialist: Hitspec · artifact",
		"call: completed",
		"result: succeeded",
		"evidence: supported",
		"fcheap://stash/stash-ui-123",
		"1 file",
		"321 bytes",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("completed Hitspec receipt omitted %q:\n%s", want, plain)
		}
	}
	for _, private := range []string{"example.com", "token", "private title"} {
		if strings.Contains(plain, private) {
			t.Fatalf("completed Hitspec receipt exposed parser-boundary field %q:\n%s", private, plain)
		}
	}
	assertToolCardLinesFit(t, card.View(84), 84)
}
