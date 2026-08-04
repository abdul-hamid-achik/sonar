package ui

import (
	"strings"
	"testing"
)

func TestProjectEcosystemConnectionsUsesSharedHealthVocabulary(t *testing.T) {
	connections := projectEcosystemConnections(
		[]string{"monitor", "mcphub"},
		[]FailedServer{
			{Name: "cortex", Reason: "connection refused"},
			{Name: "mcphub", Reason: "stale startup failure"},
		},
	)
	if len(connections) != 3 {
		t.Fatalf("connections = %#v", connections)
	}
	if got := summarizeConnectionHealth(connections); got != "degraded · 2 connected · 1 unavailable" {
		t.Fatalf("health summary = %q", got)
	}

	states := make(map[string]capabilityHealth, len(connections))
	for _, connection := range connections {
		states[connection.Label] = connection.Health
	}
	if states["MCPHub"] != capabilityConnected || states["Monitor"] != capabilityConnected || states["Cortex"] != capabilityUnavailable {
		t.Fatalf("connection states = %#v", states)
	}
	for _, connection := range connections {
		if connection.Label == "MCPHub" && connection.Detail != "" {
			t.Fatalf("reconnected MCPHub retained stale failure: %#v", connection)
		}
		if connection.Label == "Cortex" && !strings.Contains(connection.Recovery, "sonar will reconnect") {
			t.Fatalf("Cortex recovery is not actionable: %#v", connection)
		}
	}
}

func TestProjectEcosystemConnectionsNamesKnownAndCustomServers(t *testing.T) {
	connections := projectEcosystemConnections(
		[]string{"bob-mcp", "hitspec", "private_search"},
		[]FailedServer{{Name: "mcp-hub", Reason: "executable file not found"}},
	)
	if len(connections) != 4 {
		t.Fatalf("connections = %#v", connections)
	}
	got := make(map[string]string, len(connections))
	for _, connection := range connections {
		got[connection.Label] = connection.Role
	}
	for label, role := range map[string]string{
		"MCPHub":         "gateway and discovery",
		"Bob":            "repository contracts",
		"Hitspec":        "bounded web and HTTP",
		"private_search": "MCP tools",
	} {
		if got[label] != role {
			t.Fatalf("%s role = %q, want %q; all=%#v", label, got[label], role, got)
		}
	}
}

func TestConnectionHealthEmptyStateIsExplicitAndQuiet(t *testing.T) {
	if got := summarizeConnectionHealth(nil); got != "not configured · 0 servers" {
		t.Fatalf("empty health summary = %q", got)
	}
}

func TestConnectionFailureSanitizationDropsTerminalControlsAndBounds(t *testing.T) {
	reason := "\x1b[31mconnection refused\x1b[0m\n" + strings.Repeat("detail ", 40)
	got := compactConnectionFailure(reason)
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\n") || !strings.Contains(got, "connection refused") {
		t.Fatalf("sanitized failure = %q", got)
	}
	if width := len([]rune(got)); width > 96 {
		t.Fatalf("failure width = %d, want <= 96: %q", width, got)
	}
}
