package ui

import (
	"testing"
)

func TestAgentPickerIncludesDefaultProfile(t *testing.T) {
	m := newTestModel(t)
	m.agentList = []string{"reviewer", "builder"}
	m.agentProfile = "reviewer"
	m.openAgentPicker()

	items := m.agentPickerState.List.Items()
	if len(items) != 3 {
		t.Fatalf("agent items = %d, want Default + 2 profiles", len(items))
	}
	first, ok := items[0].(agentItem)
	if !ok || !first.isDefault || first.display != "Default" {
		t.Fatalf("first agent item = %#v", items[0])
	}
}

func TestClearingAgentProfileRestoresDefaultAuthority(t *testing.T) {
	m := newTestModel(t)
	m.agentProfile = "restricted"
	m.profileSkills = nil
	m.baseLoadedContext = "project instructions"
	m.modelPinned = true
	m.agent.DenyAllMCPTools()

	if err := m.applyAgentProfile(""); err != nil {
		t.Fatal(err)
	}
	if m.agentProfile != "" || m.modelPinned {
		t.Fatalf("profile state = %q pinned=%v", m.agentProfile, m.modelPinned)
	}
	if _, restricted := m.agent.MCPServerScope(); restricted {
		t.Fatal("default profile did not restore the unrestricted configured MCP scope")
	}
	if got := m.agent.LoadedContext(); got != "project instructions" {
		t.Fatalf("loaded context = %q", got)
	}
}
