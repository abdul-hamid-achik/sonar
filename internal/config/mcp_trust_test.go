package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolveMCPTrustUsesLegacyProfileOnlyAsOmittedCompatibility(t *testing.T) {
	server := ServerConfig{Name: "gateway", Command: "/opt/homebrew/bin/mcphub", Transport: "stdio"}
	trust, err := ResolveMCPTrust(server)
	if err != nil {
		t.Fatal(err)
	}
	if trust == nil || trust.LocalOwner != "mcphub" || trust.Gateway != MCPTrustGatewayMCPHub ||
		!containsMCPTrustRoute(trust.ReadOnly, "bob__bob_plan") ||
		!containsMCPTrustRoute(trust.WorkspaceEffectful, "cortex__cortex_plan") {
		t.Fatalf("legacy MCPHub profile = %#v", trust)
	}

	server.Trust = &MCPTrustConfig{LocalOwner: "mcphub", Gateway: MCPTrustGatewayMCPHub, ReadOnly: []string{"bob__bob_plan"}}
	trust, err = ResolveMCPTrust(server)
	if err != nil {
		t.Fatal(err)
	}
	if len(trust.ReadOnly) != 1 || trust.ReadOnly[0] != "bob__bob_plan" || len(trust.WorkspaceEffectful) != 0 {
		t.Fatalf("explicit trust was merged with legacy profile: %#v", trust)
	}

	server.Trust = &MCPTrustConfig{Disabled: true}
	trust, err = ResolveMCPTrust(server)
	if err != nil || trust == nil || !trust.Disabled {
		t.Fatalf("disabled trust = %#v, %v", trust, err)
	}
}

func TestResolveMCPTrustValidatesExactLocalContracts(t *testing.T) {
	validDirect := ServerConfig{
		Name: "custom-alias", Command: "/usr/local/bin/acme", Transport: "stdio",
		Trust: &MCPTrustConfig{LocalOwner: "acme", ReadOnly: []string{"inspect"}, WorkspaceEffectful: []string{"converge"}},
	}
	validGateway := ServerConfig{
		Name: "gateway", Command: "mcphub",
		Trust: &MCPTrustConfig{LocalOwner: "mcphub", Gateway: MCPTrustGatewayMCPHub, ReadOnly: []string{"acme__inspect", "mcphub_resolve_tool"}},
	}
	for _, server := range []ServerConfig{validDirect, validGateway} {
		if trust, err := ResolveMCPTrust(server); err != nil || trust == nil {
			t.Fatalf("valid trust %#v = %#v, %v", server.Trust, trust, err)
		}
	}

	tests := []struct {
		name   string
		server ServerConfig
	}{
		{name: "remote", server: withMCPTrust(validDirect, func(server *ServerConfig) {
			server.Transport, server.URL = "streamable-http", "https://example.test/mcp"
		})},
		{name: "wrapper", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Command = "env" })},
		{name: "lookalike", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Command = "acme-helper" })},
		{name: "transport whitespace", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Transport = " stdio " })},
		{name: "unknown gateway", server: withMCPTrust(validGateway, func(server *ServerConfig) { server.Trust.Gateway = "other" })},
		{name: "gateway owner mismatch", server: withMCPTrust(validGateway, func(server *ServerConfig) { server.Command, server.Trust.LocalOwner = "acme", "acme" })},
		{name: "direct delimiter", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Trust.ReadOnly = []string{"acme__inspect"} })},
		{name: "nested gateway delimiter", server: withMCPTrust(validGateway, func(server *ServerConfig) { server.Trust.ReadOnly = []string{"acme__other__inspect"} })},
		{name: "lazy router authority", server: withMCPTrust(validGateway, func(server *ServerConfig) { server.Trust.ReadOnly = []string{"mcphub_call_tool"} })},
		{name: "wildcard", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Trust.ReadOnly = []string{"inspect*"} })},
		{name: "whitespace route", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Trust.ReadOnly = []string{" inspect"} })},
		{name: "duplicate", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Trust.ReadOnly = []string{"inspect", "inspect"} })},
		{name: "cross-list duplicate", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Trust.WorkspaceEffectful = []string{"inspect"} })},
		{name: "empty", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Trust.ReadOnly, server.Trust.WorkspaceEffectful = nil, nil })},
		{name: "disabled mixed", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Trust.Disabled = true })},
		{name: "bad namespace", server: withMCPTrust(validDirect, func(server *ServerConfig) { server.Name = "bad__namespace" })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if trust, err := ResolveMCPTrust(test.server); err == nil || trust != nil {
				t.Fatalf("invalid trust resolved as %#v, %v", trust, err)
			}
		})
	}
}

func TestResolveMCPTrustBoundsAndCanonicalizesRoutes(t *testing.T) {
	server := ServerConfig{
		Name: "acme", Command: "acme",
		Trust: &MCPTrustConfig{LocalOwner: "acme", ReadOnly: []string{"zeta", "alpha"}},
	}
	trust, err := ResolveMCPTrust(server)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(trust.ReadOnly, ",") != "alpha,zeta" {
		t.Fatalf("canonical routes = %#v", trust.ReadOnly)
	}
	server.Trust.ReadOnly[0] = strings.Repeat("a", maxMCPTrustIdentifierBytes+1)
	if _, err := ResolveMCPTrust(server); err == nil {
		t.Fatal("oversized route was accepted")
	}
	routes := make([]string, maxMCPTrustContracts+1)
	for index := range routes {
		routes[index] = "tool" + strings.Repeat("x", index/maxMCPTrustIdentifierBytes) + string(rune('a'+index%26))
	}
	server.Trust.ReadOnly = routes
	if _, err := ResolveMCPTrust(server); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("oversized catalog error = %v", err)
	}
}

func TestMCPTrustDecodingRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	for _, document := range []string{
		"trust:\n  local_owner: acme\n  read_ony: [inspect]\n",
		"trust: local-owner\n",
	} {
		var wrapper struct {
			Trust *MCPTrustConfig `yaml:"trust"`
		}
		if err := yaml.Unmarshal([]byte(document), &wrapper); err == nil {
			t.Fatalf("invalid YAML trust decoded: %s", document)
		}
	}
	for _, document := range []string{
		`{"local_owner":"acme","read_ony":["inspect"]}`,
		`{"local_owner":"acme","local_owner":"other","read_only":["inspect"]}`,
		`{"local_owner":"acme","read_only":["inspect"]} {}`,
	} {
		var trust MCPTrustConfig
		if err := json.Unmarshal([]byte(document), &trust); err == nil {
			t.Fatalf("invalid JSON trust decoded: %s", document)
		}
	}
}

func TestMCPServerDecodingRejectsTrustTyposAndNull(t *testing.T) {
	for _, document := range []string{
		"name: mcphub\ncommand: mcphub\nturst:\n  disabled: true\n",
		"name: mcphub\ncommand: mcphub\ntrust: null\n",
	} {
		var server ServerConfig
		if err := yaml.Unmarshal([]byte(document), &server); err == nil {
			t.Fatalf("invalid YAML server decoded and could activate defaults: %s", document)
		}
	}
	for _, document := range []string{
		`{"name":"mcphub","command":"mcphub","turst":{"disabled":true}}`,
		`{"name":"mcphub","command":"mcphub","trust":null}`,
		`{"name":"mcphub","command":"mcphub","trust":{"disabled":true},"trust":{"local_owner":"mcphub","read_only":["mcphub_list_servers"]}}`,
	} {
		var server ServerConfig
		if err := json.Unmarshal([]byte(document), &server); err == nil {
			t.Fatalf("invalid JSON server decoded and could activate defaults: %s", document)
		}
	}
}

func withMCPTrust(server ServerConfig, mutate func(*ServerConfig)) ServerConfig {
	if server.Trust != nil {
		copy := cloneMCPTrust(*server.Trust)
		server.Trust = &copy
	}
	mutate(&server)
	return server
}

func containsMCPTrustRoute(routes []string, want string) bool {
	for _, route := range routes {
		if route == want {
			return true
		}
	}
	return false
}
