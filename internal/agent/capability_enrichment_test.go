package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/capabilityadvisor"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestSoftFillWorkspaceArgsOnlySafeFields(t *testing.T) {
	hint := capabilityadvisor.Hint{
		RequiredFields: []string{"workspace", "url", "path", "workspace_path"},
	}
	got := softFillWorkspaceArgs(hint)
	if !strings.Contains(got, `workspace="."`) || !strings.Contains(got, `workspace_path="."`) {
		t.Fatalf("missing workspace soft-fill: %q", got)
	}
	if strings.Contains(got, "url") || strings.Contains(got, ` path="`) || strings.HasPrefix(got, `path="`) {
		t.Fatalf("unsafe fields soft-filled: %q", got)
	}
	// Bare "path" must not appear as its own assignment (workspace_path is ok).
	if strings.Contains(got, ", path=") || strings.Contains(got, ": path=") {
		t.Fatalf("bare path soft-filled: %q", got)
	}
}

func TestCapabilityPlaybookPlanningMentionsBob(t *testing.T) {
	text := capabilityPlaybookText(CapabilityActivity{
		NonTrivial: true, Phase: "planning", IntentTags: []string{"repository"},
	}, &capabilityadvisor.Hint{Server: "bob", Tool: "bob_plan"})
	for _, expected := range []string{"Host specialist playbook", "bob_context", "bob_plan", "bob__bob_plan"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("playbook missing %q:\n%s", expected, text)
		}
	}
}

func TestCapabilityPlaybookCodeIntentsPreferCodemapVecgrep(t *testing.T) {
	text := capabilityPlaybookText(CapabilityActivity{
		NonTrivial: true, Phase: "research",
		IntentTags: []string{"symbols", "structure"},
	}, nil)
	if !strings.Contains(text, "codemap") || !strings.Contains(text, "vecgrep") {
		t.Fatalf("code playbook missing specialists:\n%s", text)
	}
}

func TestLazyMCPHubSystemGuidanceIsCompact(t *testing.T) {
	text := lazyMCPHubSystemGuidance()
	if !strings.Contains(text, "mcphub_call_tool") || !strings.Contains(text, "Never invent") {
		t.Fatalf("lazy guidance incomplete: %q", text)
	}
	if len(text) > 1200 {
		t.Fatalf("lazy guidance too long for small models: %d", len(text))
	}
}

func TestHasLazyMCPHubTools(t *testing.T) {
	if hasLazyMCPHubTools(nil) {
		t.Fatal("empty tools reported lazy gateway")
	}
	if !hasLazyMCPHubTools([]llm.ToolDef{{Name: "mcphub__mcphub_call_tool"}}) {
		t.Fatal("call_tool not detected")
	}
	if hasLazyMCPHubTools([]llm.ToolDef{{Name: "read"}, {Name: "bash"}}) {
		t.Fatal("native tools misclassified as lazy gateway")
	}
}

func TestFormatCapabilityHintSoftFillAndPrefetchWording(t *testing.T) {
	activity := CapabilityActivity{Phase: "planning"}
	hint := capabilityadvisor.Hint{
		Namespaced: "bob__bob_plan", Server: "bob", Tool: "bob_plan",
		RequiredFields: []string{"workspace"},
	}
	got := formatCapabilityHintWithOptions(activity, hint, true, capabilityHintOptions{HostMayPrefetch: true})
	for _, expected := range []string{
		"Host capability advisory", "bob__bob_plan", `workspace="."`,
		"Prefer the host pre-fetched contract", "mcphub_call_tool", "Known Bob contract",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("missing %q:\n%s", expected, got)
		}
	}
	if strings.Contains(got, "Call the visible mcphub_describe_tool before constructing arguments") {
		t.Fatalf("prefetch wording still forced a model describe hop:\n%s", got)
	}
}

func TestEnrichCapabilityAdvisoryAddsPlaybookWithoutHint(t *testing.T) {
	agent := New(nil, nil, 0)
	text := agent.enrichCapabilityAdvisory(context.Background(), CapabilityActivity{
		NonTrivial: true, Phase: "research",
	}, nil, "", true)
	if !strings.Contains(text, "Host specialist playbook") {
		t.Fatalf("expected playbook for research phase:\n%s", text)
	}
	if strings.Contains(text, "MCPHub recommends") {
		t.Fatalf("unexpected selected route:\n%s", text)
	}
}

func TestCapabilityMetricsRecordStatuses(t *testing.T) {
	agent := New(nil, nil, 0)
	agent.recordCapabilityMetric(capabilityadvisor.StatusResolved)
	agent.recordCapabilityMetric(capabilityadvisor.StatusAmbiguous)
	agent.recordCapabilityPrefetch(true)
	agent.recordCapabilityPrefetch(false)
	snap := agent.CapabilityMetricsSnapshot()
	if snap.TurnsNonTrivial != 2 || snap.Resolved != 1 || snap.Ambiguous != 1 ||
		snap.HostPrefetchOK != 1 || snap.HostPrefetchFail != 1 {
		t.Fatalf("metrics = %#v", snap)
	}
}
