package ui

import (
	"sort"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/command"
)

// applyInitialMCPStatus accepts the complete snapshot when available while
// retaining compatibility with embeddings that only populate FailedServers.
func (m *Model) applyInitialMCPStatus(servers []MCPServerStatus, failed []FailedServer) {
	if len(servers) > 0 {
		m.applyMCPStatusSnapshot(servers)
		return
	}
	if len(failed) == 0 {
		m.mcpServers = nil
		m.failedServers = nil
		return
	}
	servers = make([]MCPServerStatus, 0, len(failed))
	for _, failure := range failed {
		servers = append(servers, MCPServerStatus{
			Name: failure.Name, Detail: failure.Reason,
		})
	}
	m.applyMCPStatusSnapshot(servers)
}

// applyMCPStatusSnapshot replaces the transient MCP projection atomically on
// the Bubble Tea update goroutine. Raw transport errors are reduced to a
// bounded, terminal-safe detail and are never appended to ChatEntry.
func (m *Model) applyMCPStatusSnapshot(servers []MCPServerStatus) {
	byName := make(map[string]MCPServerStatus, len(servers))
	for _, server := range servers {
		name := sanitizeTerminalSingleLine(server.Name)
		name = truncateDisplay(strings.TrimSpace(name), 80)
		if name == "" {
			continue
		}
		server.Name = name
		if server.ToolCount < 0 {
			server.ToolCount = 0
		}
		if server.Connected {
			server.Detail = ""
		} else {
			server.Detail = compactConnectionFailure(server.Detail)
		}
		byName[strings.ToLower(name)] = server
	}

	keys := make([]string, 0, len(byName))
	for key := range byName {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	normalized := make([]MCPServerStatus, 0, len(keys))
	failed := make([]FailedServer, 0, len(keys))
	connected := 0
	for _, key := range keys {
		server := byName[key]
		normalized = append(normalized, server)
		if server.Connected {
			connected++
		} else {
			failed = append(failed, FailedServer{Name: server.Name, Reason: server.Detail})
		}
	}
	m.mcpServers = normalized
	m.failedServers = failed
	m.serverCount = connected
	if m.agent != nil {
		m.toolCount = m.agent.ToolCount()
	}

	if m.settingsPickerState != nil {
		m.refreshSettingsPicker()
	}
	if m.runtimeStatusState != nil {
		m.refreshRuntimeStatus(true)
	}
	if m.ready && !m.initializing {
		m.recalcViewportHeight()
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
	}
}

func (m *Model) mcpStatusCounts() (connected, unavailable, tools int) {
	for _, server := range m.mcpServers {
		if server.Connected {
			connected++
			tools += server.ToolCount
		} else {
			unavailable++
		}
	}
	return connected, unavailable, tools
}

func (m *Model) mcpRuntimeProjection() ([]string, []FailedServer, map[string]int) {
	connected := make([]string, 0, len(m.mcpServers))
	failed := make([]FailedServer, 0, len(m.mcpServers))
	toolCounts := make(map[string]int, len(m.mcpServers))
	for _, server := range m.mcpServers {
		key := strings.ToLower(server.Name)
		if server.Connected {
			connected = append(connected, server.Name)
			toolCounts[key] = server.ToolCount
		} else {
			failed = append(failed, FailedServer{Name: server.Name, Reason: server.Detail})
		}
	}
	if len(m.mcpServers) == 0 && len(m.failedServers) > 0 {
		failed = append(failed, m.failedServers...)
	}
	return connected, failed, toolCounts
}

func (m *Model) commandMCPServers() []command.ServerInfo {
	servers := make([]command.ServerInfo, 0, len(m.mcpServers))
	for _, server := range m.mcpServers {
		servers = append(servers, command.ServerInfo{
			Name: server.Name, Connected: server.Connected, ToolCount: server.ToolCount,
			Detail: server.Detail,
		})
	}
	return servers
}
