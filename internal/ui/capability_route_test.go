package ui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/goaladvisor"
)

func TestNewConversationClearsPriorTurnDiagnostics(t *testing.T) {
	m := newTestModel(t)
	route := agent.CapabilityRoute{
		Phase: "research", Status: agent.CapabilityRouteResolved,
		Server: "hitspec", Tool: "hitspec_capture_webpage",
	}
	m.capabilityRoute = &route
	m.lastCapabilityRoute = &route
	m.promptTokens = 4_096
	m.evalCount = 256
	m.turnPromptTotal = 4_096
	m.turnEvalTotal = 256
	m.footerNotice = &footerNotice{text: "✓ Done", severity: noticeSuccess}
	m.lastTurnDuration = 3 * time.Second

	updated, _ := m.Update(ctrlKey('n'))
	m = updated.(*Model)

	if m.capabilityRoute != nil || m.lastCapabilityRoute != nil {
		t.Fatalf("new conversation retained contextual route: active=%#v last=%#v", m.capabilityRoute, m.lastCapabilityRoute)
	}
	if m.promptTokens != 0 || m.evalCount != 0 || m.turnPromptTotal != 0 || m.turnEvalTotal != 0 {
		t.Fatalf("new conversation retained token diagnostics: prompt=%d eval=%d turn_prompt=%d turn_eval=%d",
			m.promptTokens, m.evalCount, m.turnPromptTotal, m.turnEvalTotal)
	}
	if m.footerNotice != nil || m.lastTurnDuration != 0 {
		t.Fatalf("new conversation retained completion receipt: notice=%#v duration=%s", m.footerNotice, m.lastTurnDuration)
	}
	runtimeStatus := m.buildRuntimeStatusContent(58)
	if strings.Contains(runtimeStatus, "Last MCP route") || strings.Contains(runtimeStatus, "~4.1k") {
		t.Fatalf("new conversation rendered stale runtime diagnostics:\n%s", runtimeStatus)
	}
}

func TestCapabilityRouteIsEphemeralAdvisoryFooterState(t *testing.T) {
	m := newTestModel(t)
	m.width = 100
	m.height = 24
	m.state = StateWaiting
	entriesBefore := len(m.entries)
	toolsBefore := len(m.toolEntries)
	pendingBefore := m.toolsPending

	updated, _ := m.Update(CapabilityRouteMsg{Route: agent.CapabilityRoute{
		Phase: "research", Status: agent.CapabilityRouteResolved,
		Freshness: agent.CapabilityRouteFresh, Server: "hitspec", Tool: "hitspec_capture_webpage",
	}})
	m = updated.(*Model)
	if len(m.entries) != entriesBefore || len(m.toolEntries) != toolsBefore || m.toolsPending != pendingBefore {
		t.Fatalf("route mutated transcript/tool state: entries=%d tools=%d pending=%d", len(m.entries), len(m.toolEntries), m.toolsPending)
	}
	line := m.renderWorkingLine()
	for _, expected := range []string{"Suggested MCP", "research", "Hitspec", "capture webpage"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("footer missing %q: %q", expected, line)
		}
	}
	for _, forbidden := range []string{"✓", "success", "succeeded", "executed"} {
		if strings.Contains(strings.ToLower(line), forbidden) {
			t.Fatalf("advisory footer implied success: %q", line)
		}
	}
	state, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(state, "hitspec_capture_webpage") || strings.Contains(state, "Suggested MCP") {
		t.Fatalf("ephemeral route entered session state: %s", state)
	}

	updated, _ = m.Update(AgentDoneMsg{})
	m = updated.(*Model)
	if m.capabilityRoute != nil {
		t.Fatalf("completed turn retained route: %#v", m.capabilityRoute)
	}
	if m.lastCapabilityRoute == nil {
		t.Fatal("completed turn discarded the inspectable ephemeral route")
	}
	runtimeStatus := m.buildRuntimeStatusContent(58)
	for _, want := range []string{"Last MCP route", "Hitspec", "capture webpage", "advisory only"} {
		if !strings.Contains(runtimeStatus, want) {
			t.Fatalf("Runtime omitted last route %q:\n%s", want, runtimeStatus)
		}
	}
}

func TestCapabilityRouteRendersTypedNonSuccessStatesWithoutTranscript(t *testing.T) {
	for _, test := range []struct {
		name  string
		route agent.CapabilityRoute
		want  []string
	}{
		{
			name:  "ambiguous cached",
			route: agent.CapabilityRoute{Phase: "research", Status: agent.CapabilityRouteAmbiguous, Freshness: agent.CapabilityRouteCached, CandidateCount: 3},
			want:  []string{"research", "ambiguous", "3 candidates", "cached"},
		},
		{
			name:  "no match reconsidered",
			route: agent.CapabilityRoute{Phase: "verification", Status: agent.CapabilityRouteNoMatch, Reconsidered: true},
			want:  []string{"verification", "No capability match", "reconsidered"},
		},
		{
			name:  "unavailable",
			route: agent.CapabilityRoute{Phase: "planning", Status: agent.CapabilityRouteUnavailable},
			want:  []string{"planning", "Routing unavailable"},
		},
		{
			name:  "invalid",
			route: agent.CapabilityRoute{Phase: "implementation", Status: agent.CapabilityRouteInvalid},
			want:  []string{"implementation", "Routing response invalid"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.width = 120
			m.state = StateWaiting
			entriesBefore := len(m.entries)
			updated, _ := m.Update(CapabilityRouteMsg{Route: test.route})
			m = updated.(*Model)
			line := m.renderWorkingLine()
			for _, want := range test.want {
				if !strings.Contains(line, want) {
					t.Fatalf("typed route missing %q: %q", want, line)
				}
			}
			if len(m.entries) != entriesBefore || strings.Contains(strings.ToLower(line), "success") {
				t.Fatalf("typed advisory mutated transcript or implied success: entries=%d line=%q", len(m.entries), line)
			}
		})
	}
}

func TestCapabilityRouteFooterRemainsResponsiveAndSanitized(t *testing.T) {
	m := newTestModel(t)
	m.width = 30
	m.height = 12
	m.state = StateWaiting
	updated, _ := m.Update(CapabilityRouteMsg{Route: agent.CapabilityRoute{
		Phase: "research", Server: "hitspec", Tool: strings.Repeat("capture_", 20),
	}})
	m = updated.(*Model)
	if m.capabilityRoute == nil {
		t.Fatal("valid resolved route was discarded")
	}
	if compact := capabilityRouteCompactLabel(*m.capabilityRoute); compact != "MCP Hitspec" {
		t.Fatalf("compact route label = %q, route=%#v", compact, *m.capabilityRoute)
	}
	line := m.renderWorkingLine()
	if lipgloss.Width(line) > m.chatPaneWidth() {
		t.Fatalf("footer overflow: width=%d pane=%d line=%q", lipgloss.Width(line), m.chatPaneWidth(), line)
	}
	if !strings.Contains(line, "esc") || !strings.Contains(line, "queue") || !strings.Contains(line, "Hitspec") {
		t.Fatalf("narrow footer lost route identity or controls: %q", line)
	}

	updated, _ = m.Update(CapabilityRouteMsg{Route: agent.CapabilityRoute{
		Phase: "research\x1b[31m", Server: "hitspec\nspoof", Tool: "capture",
	}})
	m = updated.(*Model)
	if m.capabilityRoute != nil {
		t.Fatalf("control-bearing route was not rejected: %#v", m.capabilityRoute)
	}
}

func TestCapabilityRouteCompactFooterKeepsExecutionPrimaryAndControls(t *testing.T) {
	for _, test := range []struct {
		name   string
		status agent.CapabilityRouteStatus
	}{
		{name: "ambiguous", status: agent.CapabilityRouteAmbiguous},
		{name: "no match", status: agent.CapabilityRouteNoMatch},
		{name: "unavailable", status: agent.CapabilityRouteUnavailable},
		{name: "invalid", status: agent.CapabilityRouteInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.width = 30
			m.height = 12
			m.state = StateWaiting
			updated, _ := m.Update(CapabilityRouteMsg{Route: agent.CapabilityRoute{
				Phase: "verification", Status: test.status,
			}})
			m = updated.(*Model)
			line := m.renderWorkingLine()
			if !strings.Contains(line, "Running") || !strings.Contains(line, "esc") || !strings.Contains(line, "queue") {
				t.Fatalf("compact footer lost execution identity or controls: %q", line)
			}
			if lipgloss.Width(line) > m.chatPaneWidth() {
				t.Fatalf("compact footer overflow: width=%d pane=%d line=%q", lipgloss.Width(line), m.chatPaneWidth(), line)
			}
		})
	}
}

func TestGoalCapabilityPhaseUsesOnlyTypedPhase(t *testing.T) {
	if got := goalCapabilityPhase(nil); got != "implementation" {
		t.Fatalf("nil phase = %q", got)
	}
	if got := goalCapabilityPhase(&goaladvisor.Advice{Phase: "verifying"}); got != "verification" {
		t.Fatalf("verifying phase = %q", got)
	}
}
