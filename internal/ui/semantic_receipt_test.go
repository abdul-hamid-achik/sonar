package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

func TestSemanticToolReceiptDoesNotPaintBobDriftGreen(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(ToolCallStartMsg{
		ID: "bob-1", Name: "bob__bob_check", Args: map[string]any{}, StartTime: time.Now(),
	})
	m = updated.(*Model)
	projection := ecosystem.ProjectReceipt(ecosystem.ProjectToolCall("bob__bob_check", nil), ecosystem.RawReceipt{
		Structured: []byte(`{"schema_version":1,"ok":true,"workspace":"/repo","authority":{"mode":"exact_allowlist","default_workspace":"/repo","selected_workspace":"/repo","allowed_workspace_count":1},"plan_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","clean":false,"lock_changed":false,"conflict_count":0,"counts":{"create":0,"update":1,"adopt":0,"unchanged":0,"conflict":0},"warnings":[],"next_actions":[]}`),
	})
	updated, _ = m.Update(ToolCallResultMsg{
		ID: "bob-1", Name: "bob__bob_check", Result: `{"schema_version":1,"ok":true,"clean":false}`,
		Duration: 5 * time.Millisecond, Projection: projection,
	})
	m = updated.(*Model)

	if len(m.toolEntries) != 1 || m.toolEntries[0].Status != ToolStatusDone || m.toolEntries[0].IsError {
		t.Fatalf("transport state was collapsed into an error: %#v", m.toolEntries)
	}
	card := testProjectedToolCard(t, m, 0)
	if card.State != ToolCardAttention {
		t.Fatalf("Bob drift card = %#v", card)
	}
	plain := ansi.Strip(card.View(72))
	for _, want := range []string{"!", "Repository needs convergence", "Drift reported"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("attention receipt missing %q:\n%s", want, plain)
		}
	}
}

func TestStrictToolProjectionKeepsDomainUnknownOutOfSuccessState(t *testing.T) {
	m := newTestModel(t)
	m.toolEntries = []ToolEntry{{
		ID: "cortex-status-1", Name: "cortex__cortex_status", Status: ToolStatusDone,
		Collapsed: true,
		Projection: ecosystem.ToolProjection{
			Specialist: "cortex", Operation: "cortex_status", Role: ecosystem.RoleCoordination,
			Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainUnknown,
		},
	}}

	var rendered strings.Builder
	m.renderToolGroup(&rendered, testToolChatEntry(0))
	plain := ansi.Strip(rendered.String())
	if !strings.Contains(plain, "?") || !strings.Contains(plain, "Needs attention") {
		t.Fatalf("domain-unknown receipt lost its attention state:\n%s", plain)
	}
	for _, forbidden := range []string{"✓", "Checked Cortex"} {
		if strings.Contains(plain, forbidden) {
			t.Fatalf("domain-unknown receipt claimed success with %q:\n%s", forbidden, plain)
		}
	}
}

func TestHitspecSearchRendersSuccessfulCandidateEvidence(t *testing.T) {
	projection := ecosystem.ProjectReceipt(
		ecosystem.ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
			"server": "hitspec", "tool": "hitspec_search_web",
		}),
		ecosystem.RawReceipt{Text: `{"kind":"discovery","query":"current docs","results":[{"title":"Docs","url":"https://docs.sonar.dev/","domain":"docs.sonar.dev","snippet":"candidate","citation_id":"source-01"}],"truncated":false}`},
	)
	card := NewToolCard("mcphub__mcphub_call_tool", ToolCardGeneric, true, defaultThemeID)
	card.State = toolCardStateFromProjection(projection)
	card.Projection = projection
	card.Result = ecosystem.SafeReceiptText(projection)

	if card.State != ToolCardSuccess {
		t.Fatalf("Hitspec search card state = %v, want success for a completed candidate search", card.State)
	}
	collapsed := ansi.Strip(card.View(76))
	for _, expected := range []string{"Found web candidates", "1 candidate source", "docs.sonar.dev"} {
		if !strings.Contains(collapsed, expected) {
			t.Fatalf("collapsed Hitspec search receipt missing %q:\n%s", expected, collapsed)
		}
	}
	card.Expanded = true
	expanded := ansi.Strip(card.View(76))
	for _, expected := range []string{"specialist: Hitspec · discovery", "result: succeeded", "evidence: candidate"} {
		if !strings.Contains(expanded, expected) {
			t.Fatalf("expanded Hitspec search receipt missing %q:\n%s", expected, expanded)
		}
	}
	for _, forbidden := range []string{"evidence: verified", "Outcome needs interpretation"} {
		if strings.Contains(expanded, forbidden) {
			t.Fatalf("Hitspec candidate receipt claimed %q:\n%s", forbidden, expanded)
		}
	}
}

func TestExpandedSemanticReceiptShowsDownstreamRouteAndEvidence(t *testing.T) {
	card := NewToolCard("mcphub__cortex__cortex_investigate", ToolCardGeneric, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Expanded = true
	card.Duration = time.Millisecond
	card.Projection = ecosystem.ToolProjection{
		Specialist: "cortex", Operation: "cortex_investigate", Role: ecosystem.RoleCoordination,
		Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainSucceeded,
		Evidence: ecosystem.EvidenceSupported,
		Route:    ecosystem.ToolRoute{Gateway: "mcphub", Server: "cortex", Tool: "cortex_investigate", Lazy: true},
	}
	plain := ansi.Strip(card.View(76))
	for _, want := range []string{"Investigated", "specialist: Cortex · coordination", "route: sonar → MCPHub → Cortex · lazy", "call: completed", "result: succeeded", "evidence: supported"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("expanded semantic receipt missing %q:\n%s", want, plain)
		}
	}
	assertToolCardLinesFit(t, card.View(76), 76)
}

func TestSemanticAttentionUsesDomainBeforeArbitraryResultText(t *testing.T) {
	card := NewToolCard("bob__bob_check", ToolCardGeneric, true, defaultThemeID)
	card.State = ToolCardAttention
	card.Expanded = true
	card.Duration = time.Millisecond
	card.Result = "Everything completed successfully\nserver supplied detail"
	card.Projection = ecosystem.ToolProjection{
		Specialist: "bob", Operation: "bob_check", Role: ecosystem.RoleBuild,
		Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainConflict,
	}

	plain := ansi.Strip(card.View(76))
	canonical := strings.Index(plain, "Conflict reported")
	arbitrary := strings.Index(plain, "Everything completed successfully")
	if canonical < 0 || arbitrary < 0 || canonical > arbitrary {
		t.Fatalf("authoritative domain state did not precede arbitrary detail:\n%s", plain)
	}
	if header := strings.Split(plain, "\n")[0]; strings.Contains(header, "Checked repository contract") {
		t.Fatalf("attention header reused success copy:\n%s", plain)
	}
}

func TestDeferredMCPHubReceiptShowsStoredResultAndFetchPath(t *testing.T) {
	projection := ecosystem.ToolProjection{
		Specialist: "cortex", Operation: "cortex_start_task", Role: ecosystem.RoleCoordination,
		Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainAttention,
		Route: ecosystem.ToolRoute{
			Gateway: "mcphub", Server: "cortex", Tool: "cortex_start_task",
			CallID: "call-123", Lazy: true,
		},
	}
	card := NewToolCard("mcphub__mcphub_call_tool", ToolCardGeneric, true, defaultThemeID)
	card.State = toolCardStateFromProjection(projection)
	card.Expanded = true
	card.Duration = 2 * time.Millisecond
	card.Projection = projection

	plain := ansi.Strip(card.View(76))
	for _, want := range []string{
		"Result stored", "fetch call-123", "call: completed",
		"result: needs attention", "evidence: none", "stored result: call-123",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("stored MCPHub receipt missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "Started case") {
		t.Fatalf("stored MCPHub receipt retained a success verb:\n%s", plain)
	}
	for _, line := range strings.Split(plain, "\n") {
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "│")) == "result: call-123" {
			t.Fatalf("stored MCPHub receipt used an ambiguous result label:\n%s", plain)
		}
	}
	for _, width := range []int{4, 12, 28, 52, 76} {
		assertToolCardLinesFit(t, card.View(width), width)
	}
}
