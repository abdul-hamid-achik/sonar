package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/charmbracelet/x/ansi"
)

type runtimeStatusExpertConsultant struct{ profiles int }

func (consultant runtimeStatusExpertConsultant) Consult(context.Context, expertteam.Request) (expertteam.Result, error) {
	return expertteam.Result{}, nil
}

func (consultant runtimeStatusExpertConsultant) ProfileCount() int { return consultant.profiles }

func TestRuntimeStatusBoundsFailuresAndScrollsToFinalEntry(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	for i := 1; i <= 8; i++ {
		m.failedServers = append(m.failedServers, FailedServer{
			Name:   fmt.Sprintf("server-%02d", i),
			Reason: "connection refused after a deliberately long local transport diagnostic",
		})
	}
	m.openSettingsPicker()
	m.openSettingsChild(m.openRuntimeStatus)

	top := m.renderRuntimeStatus()
	assertRenderedLinesFit(t, top, 40)
	assertRenderedHeightFits(t, top, 20)
	if integrated := ansi.Strip(m.View().Content); !strings.Contains(integrated, "╰") {
		t.Fatalf("integrated Runtime overlay clipped its bottom border:\n%s", integrated)
	}
	plainTop := ansi.Strip(top)
	if !strings.Contains(plainTop, "esc/q back") || !strings.Contains(plainTop, "↓") {
		t.Fatalf("runtime footer is not persistently actionable:\n%s", top)
	}

	updated, _ = m.Update(charKey('G'))
	m = updated.(*Model)
	bottom := m.renderRuntimeStatus()
	if !strings.Contains(bottom, "server-08") {
		t.Fatalf("scrolling did not reach final failure:\n%s", bottom)
	}
	assertRenderedLinesFit(t, bottom, 40)
	assertRenderedHeightFits(t, bottom, 20)
}

func TestRuntimeStatusPreservesScrollAcrossResize(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < 10; i++ {
		m.failedServers = append(m.failedServers, FailedServer{Name: fmt.Sprintf("failed-%d", i), Reason: strings.Repeat("detail ", 8)})
	}
	m.openRuntimeStatus()
	m.runtimeStatusState.Viewport.GotoBottom()

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m = updated.(*Model)
	if m.runtimeStatusState == nil || m.runtimeStatusState.Viewport.YOffset() == 0 {
		t.Fatal("runtime resize discarded the scroll position")
	}
	assertRenderedLinesFit(t, m.renderRuntimeStatus(), 60)
	assertRenderedHeightFits(t, m.renderRuntimeStatus(), 20)
}

func TestRuntimeStatusSeparatesLocalToolsFromMCPServers(t *testing.T) {
	m := newTestModel(t)
	home := t.TempDir()
	workspace := filepath.Join(home, "src", "project")
	t.Setenv("HOME", home)
	m.agent.SetWorkDir(workspace)
	m.toolCount = 19
	m.serverCount = 0

	content := m.buildRuntimeStatusContent(52)
	searchable := strings.Join(strings.Fields(content), " ")
	availability := m.agent.ToolAvailability()
	for _, want := range []string{
		"Workspace", "~/src/project", "Tools", fmt.Sprintf("%d ready", availability.Ready()),
		fmt.Sprintf("%d local", availability.Local),
		"MCP", "not configured", "0 servers", "Context route", "not exposed · policy/catalog", "Experts", "disabled in configuration",
	} {
		if !strings.Contains(searchable, want) {
			t.Fatalf("runtime status missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "across 0 MCP servers") {
		t.Fatalf("runtime status still conflates local tools with MCP servers:\n%s", content)
	}
}

func TestRuntimeStatusKeepsOllamaRecoveryConditionalAndActionable(t *testing.T) {
	m := newTestModel(t)
	m.ollamaOffline = true
	content := strings.Join(strings.Fields(ansi.Strip(m.buildRuntimeStatusContent(58))), " ")
	for _, want := range []string{
		"Ollama setup needed",
		"ctrl+o select or install a model",
		"run ollama serve if the host is unavailable",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("Runtime omitted Ollama recovery %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "host offline") {
		t.Fatalf("Runtime promoted an unverified host diagnosis:\n%s", content)
	}
}

func TestRuntimeStatusPluralizesVisibleCounters(t *testing.T) {
	m := newTestModel(t)
	m.sessionTurnCount = 1
	m.sessionEvalTotal = 12
	m.iceEnabled = true
	m.iceConversations = 1
	m.applyMCPStatusSnapshot([]MCPServerStatus{{Name: "one", Connected: true, ToolCount: 1}})

	content := strings.Join(strings.Fields(ansi.Strip(m.buildRuntimeStatusContent(58))), " ")
	for _, want := range []string{"1 turn", "1 conversation", "1 tool"} {
		if !strings.Contains(content, want) {
			t.Fatalf("Runtime omitted natural singular %q:\n%s", want, content)
		}
	}
	for _, stale := range []string{"1 turns", "1 conversations", "1 tools"} {
		if strings.Contains(content, stale) {
			t.Fatalf("Runtime retained mechanical plural %q:\n%s", stale, content)
		}
	}
}

func TestRuntimeStatusProjectsSessionIdentityAfterCreation(t *testing.T) {
	m := newTestModel(t)
	unsaved := strings.Join(strings.Fields(ansi.Strip(m.buildRuntimeStatusContent(58))), " ")
	if strings.Contains(unsaved, "Session S") {
		t.Fatalf("Runtime invented a durable session before the first turn:\n%s", unsaved)
	}

	m.sessionID = 7

	m.sessionPublicID = "aaaaaa7"
	m.activeSessionTitle = "Polish composer wrapping"
	saved := strings.Join(strings.Fields(ansi.Strip(m.buildRuntimeStatusContent(58))), " ")
	if !strings.Contains(saved, "Session aaaaaa7 · Polish composer wrapping") {
		t.Fatalf("Runtime omitted saved session identity:\n%s", saved)
	}
}

func TestRuntimeStatusRetainsExpertStartupFailure(t *testing.T) {
	m := newTestModel(t)
	m.SetExpertRuntimeSetupFailed()
	content := m.buildRuntimeStatusContent(58)
	// Styling is applied independently to wrapped continuation lines. Strip it
	// before collapsing layout whitespace so this assertion checks the copy,
	// not whether the terminal emitted an ANSI reset at the wrap boundary.
	searchable := strings.Join(strings.Fields(ansi.Strip(content)), " ")
	for _, want := range []string{"Experts", "setup failed", "review experts config/profiles; restart"} {
		if !strings.Contains(searchable, want) {
			t.Fatalf("expert startup status omitted %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "see diagnostics") {
		t.Fatalf("expert startup status retained non-actionable recovery copy:\n%s", content)
	}
}

func TestRuntimeStatusKeepsReadOnlyExpertBoundaryWithProfiles(t *testing.T) {
	m := newTestModel(t)
	m.agent.SetExpertConsultant(runtimeStatusExpertConsultant{profiles: 3})
	content := strings.Join(strings.Fields(ansi.Strip(m.buildRuntimeStatusContent(58))), " ")
	for _, want := range []string{"Experts", "ready", "3 profiles", "read-only", "adaptive"} {
		if !strings.Contains(content, want) {
			t.Fatalf("expert runtime status omitted %q:\n%s", want, content)
		}
	}
}

func TestRuntimeStatusUsesNaturalSingularAuthorityLabels(t *testing.T) {
	m := newTestModel(t)
	workspace := t.TempDir()
	external := t.TempDir()
	m.agent.SetWorkDir(workspace)
	if _, err := m.agent.AddReadRoot(external); err != nil {
		t.Fatal(err)
	}

	content := strings.Join(strings.Fields(ansi.Strip(m.buildRuntimeStatusContent(58))), " ")
	if !strings.Contains(content, "Read scope 1 temporary external grant") || strings.Contains(content, "grant(s)") {
		t.Fatalf("Runtime retained mechanical singular copy:\n%s", content)
	}
}

func TestRuntimeStatusDistinguishesTurnBoundTypedWriteFromShell(t *testing.T) {
	m := newTestModel(t)
	workspace := t.TempDir()
	external := t.TempDir()
	m.agent.SetWorkDir(workspace)

	inspection, err := m.agent.InspectWritePath(external)
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Release()
	if _, err := m.agent.AddInspectedWriteGrant(inspection.Grant()); err != nil {
		t.Fatal(err)
	}

	content := strings.Join(strings.Fields(ansi.Strip(m.buildRuntimeStatusContent(72))), " ")
	for _, want := range []string{
		"Turn-bound typed-write access",
		"Directory",
		"Built-in typed operations only",
		"expires when this turn settles",
		"raw shell remains confined to the startup workspace",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("Runtime omitted typed-write boundary %q:\n%s", want, content)
		}
	}
}

func TestRuntimeStatusRowsSanitizeConfigurableLabels(t *testing.T) {
	m := newTestModel(t)
	m.agentProfile = "reviewer\nApproval · disabled\x1b]52;c;payload\a\u202e"
	content := ansi.Strip(m.buildRuntimeStatusContent(58))
	for _, forbidden := range []string{"\nApproval · disabled", "\x1b]52", "\a", "\u202e"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("Runtime retained untrusted profile control %q:\n%s", forbidden, content)
		}
	}
	if !strings.Contains(content, "reviewer Approval · disabled") {
		t.Fatalf("Runtime lost the safe profile projection:\n%s", content)
	}
	if m.agentProfile != "reviewer\nApproval · disabled\x1b]52;c;payload\a\u202e" {
		t.Fatal("Runtime display sanitization mutated the profile identity")
	}
}

func TestRuntimeStatusUsesPresentedAuthorityAndPrioritizesReadScope(t *testing.T) {
	m := newTestModel(t)
	m.model = "qwen-cloud:latest"
	m.modelPinned = true
	m.ollamaModels = []OllamaModelDescriptor{{Name: m.model, Source: OllamaModelCloud}}
	m.mode = ModePlan
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})

	content := strings.Join(strings.Fields(ansi.Strip(m.buildRuntimeStatusContent(80))), " ")
	for _, want := range []string{
		"Model Pinned · CLOUD · remote prompts · qwen-cloud:latest",
		"Mode AUTO · Goal Runtime",
		"Approval",
		"Read scope workspace only",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("Runtime omitted presented authority %q:\n%s", want, content)
		}
	}
	approval := strings.Index(content, "Approval")
	readScope := strings.Index(content, "Read scope")
	tools := strings.Index(content, "Tools")
	if approval < 0 || readScope <= approval || tools <= readScope {
		t.Fatalf("authority rows are not ordered Approval -> Read scope -> Tools:\n%s", content)
	}
}

func TestContextRoutingRuntimeLabelDistinguishesServerAvailability(t *testing.T) {
	for _, test := range []struct {
		state agent.CapabilityRoutingHostState
		want  string
	}{
		{state: agent.CapabilityRoutingHostUnavailable, want: "not exposed · policy/catalog"},
		{state: agent.CapabilityRoutingHostServerUnavailable, want: "MCPHub server unavailable"},
		{state: agent.CapabilityRoutingHostReady, want: "ready · MCPHub advisory"},
	} {
		if got := contextRoutingRuntimeLabel(test.state); !strings.Contains(got, test.want) {
			t.Fatalf("routing label for state %v = %q, want %q", test.state, got, test.want)
		}
	}
}

func TestRuntimeStatusReconcilesReconnectedEcosystemServers(t *testing.T) {
	m := newTestModel(t)
	m.failedServers = []FailedServer{
		{Name: "mcphub", Reason: "stale failure"},
		{Name: "cortex", Reason: "connection refused"},
	}
	// Registry test seams are not needed here: project the same live/failure
	// inputs used by Runtime and assert the rendered vocabulary independently.
	connections := projectEcosystemConnections([]string{"mcphub", "monitor"}, m.failedServers)
	if got := summarizeConnectionHealth(connections); got != "degraded · 2 connected · 1 unavailable" {
		t.Fatalf("runtime projection = %q", got)
	}
	for _, connection := range connections {
		if connection.Label == "MCPHub" && connection.Health != capabilityConnected {
			t.Fatalf("stale MCPHub failure won over live connection: %#v", connections)
		}
	}
}

func TestCompactWorkspacePathPreservesRepositoryName(t *testing.T) {
	path := filepath.Join(string(filepath.Separator), "Users", "person", "projects", "sonar")
	if got, want := compactWorkspacePath(path, 22), "…/projects/sonar"; got != want {
		t.Fatalf("compact workspace = %q, want %q", got, want)
	}
	if got, want := compactWorkspacePath(path, 14), "…/sonar"; got != want {
		t.Fatalf("narrow workspace = %q, want %q", got, want)
	}
}

// The Ollama daemon version used to live in the model picker's title, where
// the specs used it as a proxy for "the picker is open". It is a runtime fact,
// so it must be reachable in the Runtime panel — the title is now constant and
// no longer surfaces it anywhere else.
func TestRuntimeStatusSurfacesTheOllamaVersion(t *testing.T) {
	m := newTestModel(t)
	m.ollamaVersion = "0.31.2-test"
	m.openRuntimeStatus()

	panel := ansi.Strip(m.renderRuntimeStatus())
	if !strings.Contains(panel, "0.31.2-test") {
		t.Fatalf("runtime panel does not report the Ollama version:\n%s", panel)
	}
	if title := ollamaModelPickerTitle("0.31.2-test"); title != "Ollama models" {
		t.Fatalf("model picker title still carries the version: %q", title)
	}
}
