package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestMCPStatusSnapshotRefreshesEverySurfaceWithoutPersistingRawError(t *testing.T) {
	m := newTestModel(t)
	m.openRuntimeStatus()
	raw := "\x1b[31mconnection refused\nprivate transport detail"
	entriesBefore := len(m.entries)

	updated, _ := m.Update(MCPStatusSnapshotMsg{Servers: []MCPServerStatus{
		{Name: "hitspec", Connected: true, ToolCount: 3},
		{Name: "mcphub", Detail: raw},
	}})
	m = updated.(*Model)
	if len(m.entries) != entriesBefore {
		t.Fatal("reactive MCP snapshot entered persisted transcript")
	}
	if m.serverCount != 1 || len(m.failedServers) != 1 || len(m.mcpServers) != 2 {
		t.Fatalf("MCP projection = connected:%d failed:%#v all:%#v", m.serverCount, m.failedServers, m.mcpServers)
	}
	if detail := m.failedServers[0].Reason; strings.ContainsAny(detail, "\x1b\n\r") || len(detail) > 160 {
		t.Fatalf("raw MCP error survived bounded projection: %q", detail)
	}

	runtime := ansi.Strip(m.renderRuntimeStatus())
	for _, want := range []string{"Hitspec", "connected", "MCPHub", "unavailable"} {
		if !strings.Contains(runtime, want) {
			t.Fatalf("open Runtime did not refresh %q:\n%s", want, runtime)
		}
	}

	m.closeRuntimeStatus()
	m.openSettingsPicker()
	settings := ansi.Strip(m.renderSettingsPicker())
	for _, want := range []string{"2 servers", "1 connected", "1 unavailable"} {
		if !strings.Contains(settings, want) {
			t.Fatalf("Settings did not reflect %q:\n%s", want, settings)
		}
	}
	m.closeSettingsPicker()

	result := m.cmdRegistry.Execute(m.buildCommandContext(), "servers", nil)
	for _, want := range []string{"hitspec · connected · 3 tools", "mcphub · unavailable", "MCP tools available: 3"} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("/servers missing %q:\n%s", want, result.Text)
		}
	}
	if strings.Contains(result.Text, "private transport detail") {
		t.Fatalf("/servers persisted raw MCP error: %q", result.Text)
	}

	m.entries = append(m.entries, ChatEntry{Kind: "user", Content: "hello"})
	if footer := ansi.Strip(m.renderStatusLine()); !strings.Contains(footer, "1 MCP server unavailable") {
		t.Fatalf("footer did not expose degraded MCP state: %q", footer)
	}
	updated, _ = m.Update(MCPStatusSnapshotMsg{Servers: []MCPServerStatus{
		{Name: "hitspec", Connected: true, ToolCount: 3},
		{Name: "mcphub", Connected: true, ToolCount: 4},
	}})
	m = updated.(*Model)
	if len(m.failedServers) != 0 || strings.Contains(ansi.Strip(m.renderStatusLine()), "MCP server unavailable") {
		t.Fatal("recovery snapshot retained stale failure state")
	}
}

func TestInitCompleteFailureKeepsRawMCPErrorOutOfTranscript(t *testing.T) {
	m := newTestModel(t)
	raw := "dial failed\nsecret transport payload"
	updated, _ := m.Update(InitCompleteMsg{
		Model: "local", FailedServers: []FailedServer{{Name: "mcphub", Reason: raw}},
	})
	m = updated.(*Model)
	transcript := entryText(m.entries)
	if !strings.Contains(transcript, "MCP unavailable: mcphub") || strings.Contains(transcript, "secret transport payload") {
		t.Fatalf("InitComplete transcript retained raw failure: %q", transcript)
	}
}

func TestMCPStatusSnapshotNoColorAndNarrowRuntime(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })
	m := newTestModel(t)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m.applyMCPStatusSnapshot([]MCPServerStatus{{Name: "mcphub", Detail: "connection refused"}})
	m.openRuntimeStatus()
	rendered := m.renderRuntimeStatus()
	if hasANSIColor(rendered) {
		t.Fatalf("NO_COLOR Runtime emitted ANSI color: %q", rendered)
	}
	assertRenderedLinesFit(t, rendered, minTerminalWidth)
	assertRenderedHeightFits(t, rendered, minTerminalHeight)
}
