package main

import (
	"path/filepath"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// goalAdvisorConfigured reports explicit Cortex authority from the same
// host-owned server list used to build the MCP registry. Conventional server
// names cover local HTTP configurations; exact executable basenames also
// cover trusted STDIO aliases without treating wrappers or lookalikes as an
// advisor configuration.
func goalAdvisorConfigured(servers []config.ServerConfig) bool {
	for _, server := range servers {
		name := strings.ToLower(strings.TrimSpace(server.Name))
		if name == "cortex" || name == "mcphub" {
			return true
		}

		if strings.TrimSpace(server.URL) != "" {
			continue
		}
		transport := strings.ToLower(strings.TrimSpace(server.Transport))
		if transport != "" && transport != "stdio" {
			continue
		}
		command := strings.TrimSpace(server.Command)
		if command == "" || command != server.Command {
			continue
		}
		switch strings.ToLower(filepath.Base(filepath.Clean(command))) {
		case "cortex", "mcphub":
			return true
		}
	}
	return false
}
