package agent

import "github.com/abdul-hamid-achik/sonar/internal/mcp"

// MCPConnectionStatuses exposes the registry's cached, non-blocking status to
// host UIs. It deliberately does not run a health check from the render path.
func (a *Agent) MCPConnectionStatuses() []mcp.ConnectionStatus {
	if a == nil || a.registry == nil {
		return nil
	}
	return a.registry.ConnectionStatuses()
}
