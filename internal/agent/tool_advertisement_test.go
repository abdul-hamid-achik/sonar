package agent

import (
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

// advertisementFixture builds an agent whose vecgrep namespace trusts one read
// route, mirroring the real ~/.agents/mcp.json shape.
func advertisementFixture(t *testing.T, skipApprovals bool, withCallback bool) *Agent {
	t.Helper()
	ag := New(nil, nil, 8192)
	ag.SetPermissionChecker(permission.NewChecker(nil, skipApprovals))
	if withCallback {
		ag.SetApprovalCallback(func(permission.ApprovalRequest) {})
	}
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{
		Name: "vecgrep", Command: "vecgrep", Args: []string{"serve", "--mcp"},
		Trust: &config.MCPTrustConfig{
			LocalOwner: "vecgrep",
			ReadOnly:   []string{"vecgrep_search"},
		},
	}})
	return ag
}

func advertisedNames(defs []llm.ToolDef) map[string]bool {
	out := make(map[string]bool, len(defs))
	for _, d := range defs {
		out[d.Name] = true
	}
	return out
}

func candidateDefs() []llm.ToolDef {
	return []llm.ToolDef{
		{Name: "read"},                     // builtin, argument-dependent
		{Name: "bash"},                     // builtin, argument-dependent
		{Name: "vecgrep__vecgrep_search"},  // trusted read route
		{Name: "vecgrep__vecgrep_clean"},   // untrusted, needs approval
		{Name: "vecgrep__vecgrep_index"},   // untrusted, needs approval
		{Name: "mcphub__mcphub_call_tool"}, // target lives in the arguments
	}
}

// Headless with no approval surface: the write half of the server can only be
// refused at dispatch, so it must not be offered. Everything whose verdict
// depends on arguments stays.
func TestUnapprovableMCPToolsAreNotAdvertisedWithoutAnApprovalSurface(t *testing.T) {
	ag := advertisementFixture(t, false, false)

	got := advertisedNames(ag.dropUnapprovableTools(candidateDefs()))

	for _, want := range []string{"read", "bash", "vecgrep__vecgrep_search", "mcphub__mcphub_call_tool"} {
		if !got[want] {
			t.Errorf("%s was dropped but can still run", want)
		}
	}
	for _, unwanted := range []string{"vecgrep__vecgrep_clean", "vecgrep__vecgrep_index"} {
		if got[unwanted] {
			t.Errorf("%s was advertised but could only ever be refused", unwanted)
		}
	}
}

// The TUI can answer a prompt, so hiding anything there would remove a tool the
// user could have approved.
func TestApprovalCallbackKeepsTheFullCatalog(t *testing.T) {
	ag := advertisementFixture(t, false, true)

	if got := ag.dropUnapprovableTools(candidateDefs()); len(got) != len(candidateDefs()) {
		t.Fatalf("advertised %d of %d tools with an approval surface present", len(got), len(candidateDefs()))
	}
}

// skip-approvals lives inside the checker, so a tool that would have asked now
// resolves to allow. Dropping it would break the documented posture.
func TestSkipApprovalsKeepsTheFullCatalog(t *testing.T) {
	ag := advertisementFixture(t, true, false)

	if got := ag.dropUnapprovableTools(candidateDefs()); len(got) != len(candidateDefs()) {
		t.Fatalf("advertised %d of %d tools under skip-approvals", len(got), len(candidateDefs()))
	}
}

// A missing checker means there is no approval gate to reason about. Guessing
// there would silently shrink the catalog for embedders and tests.
func TestNoCheckerLeavesTheCatalogAlone(t *testing.T) {
	ag := New(nil, nil, 8192)
	if got := ag.dropUnapprovableTools(candidateDefs()); len(got) != len(candidateDefs()) {
		t.Fatalf("advertised %d of %d tools with no permission checker", len(got), len(candidateDefs()))
	}
}

func TestDropUnapprovableToolsHandlesEmptyInput(t *testing.T) {
	ag := advertisementFixture(t, false, false)
	if got := ag.dropUnapprovableTools(nil); len(got) != 0 {
		t.Errorf("nil input produced %d tools", len(got))
	}
}
