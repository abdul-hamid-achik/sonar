package main

import (
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

func TestGoalAdvisorConfiguredRequiresExplicitCortexOrMCPHubConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		servers []config.ServerConfig
		want    bool
	}{
		{name: "none"},
		{name: "unrelated", servers: []config.ServerConfig{{Name: "search", Command: "vecgrep"}}},
		{name: "named cortex http", servers: []config.ServerConfig{{Name: "cortex", Transport: "streamable-http", URL: "http://127.0.0.1:8080/mcp"}}, want: true},
		{name: "named mcphub http", servers: []config.ServerConfig{{Name: "mcphub", Transport: "sse", URL: "http://127.0.0.1:8081/sse"}}, want: true},
		{name: "stdio mcphub alias", servers: []config.ServerConfig{{Name: "gateway", Command: "/opt/homebrew/bin/mcphub"}}, want: true},
		{name: "stdio cortex alias", servers: []config.ServerConfig{{Name: "advisor", Command: "cortex", Transport: "stdio"}}, want: true},
		{name: "remote command lookalike", servers: []config.ServerConfig{{Name: "gateway", Command: "mcphub-helper"}}},
		{name: "wrapper", servers: []config.ServerConfig{{Name: "gateway", Command: "env", Args: []string{"mcphub"}}}},
		{name: "non stdio alias", servers: []config.ServerConfig{{Name: "gateway", Command: "mcphub", Transport: "streamable-http", URL: "http://127.0.0.1:8082/mcp"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := goalAdvisorConfigured(test.servers); got != test.want {
				t.Fatalf("goalAdvisorConfigured() = %v, want %v", got, test.want)
			}
		})
	}
}
