package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"gopkg.in/yaml.v3"
)

const (
	MCPTrustGatewayMCPHub = "mcphub"

	maxMCPTrustContracts       = 256
	maxMCPTrustRouteBytes      = 193 // two 96-byte identifiers plus "__"
	maxMCPTrustIdentifierBytes = 96
)

// MCPTrustConfig is explicit host-owned authority for one local STDIO MCP
// process. Exact routes model the two reduced-friction classes the runtime can
// enforce safely. Server annotations never widen this policy UNLESS the
// operator delegates with `annotations: honor` — that flag is the single place
// the server's own read-only declarations gain authority, and it is a per-
// server decision, never a default.
//
// The coarser grants trade enumeration for a weaker promise: `all` (whole
// server) and `all_servers` (named downstream namespaces behind the MCPHub
// gateway) auto-approve under AUTO authority only, and their calls keep the
// conservative effect class — NORMAL still asks, and an unanswered call is
// still treated as an effect whose outcome is unknown. Only exact routes and
// honored annotations can claim read-only semantics.
type MCPTrustConfig struct {
	Disabled           bool     `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	LocalOwner         string   `yaml:"local_owner,omitempty" json:"local_owner,omitempty"`
	Gateway            string   `yaml:"gateway,omitempty" json:"gateway,omitempty"`
	Annotations        string   `yaml:"annotations,omitempty" json:"annotations,omitempty"`
	All                bool     `yaml:"all,omitempty" json:"all,omitempty"`
	AllServers         []string `yaml:"all_servers,omitempty" json:"all_servers,omitempty"`
	ReadOnly           []string `yaml:"read_only,omitempty" json:"read_only,omitempty"`
	WorkspaceEffectful []string `yaml:"workspace_effectful,omitempty" json:"workspace_effectful,omitempty"`
}

// MCPTrustAnnotationsHonor is the only accepted non-empty value for
// MCPTrustConfig.Annotations.
const MCPTrustAnnotationsHonor = "honor"

// UnmarshalYAML makes the entire server authority object strict. Without this
// boundary, `turst: {disabled: true}` would be silently ignored and an omitted
// policy could activate the embedded compatibility profile.
func (s *ServerConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("MCP server must be a mapping")
	}
	allowed := map[string]bool{
		"name": true, "command": true, "args": true, "env": true,
		"transport": true, "url": true, "trust": true,
	}
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index].Value, node.Content[index+1]
		if !allowed[key] {
			return fmt.Errorf("unknown MCP server field %q", key)
		}
		if key == "trust" && value.ShortTag() == "!!null" {
			return fmt.Errorf("MCP server trust cannot be null; omit it for compatibility or use disabled: true")
		}
	}
	type plain ServerConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*s = ServerConfig(decoded)
	return nil
}

// UnmarshalJSON applies the same strict parent boundary to shared mcp.json
// metadata and rejects null trust before it can become indistinguishable from
// omission.
func (s *ServerConfig) UnmarshalJSON(data []byte) error {
	allowed := map[string]bool{
		"name": true, "command": true, "args": true, "env": true,
		"transport": true, "url": true, "trust": true,
	}
	fields, err := strictJSONObjectFields(data, "MCP server", allowed)
	if err != nil {
		return err
	}
	for key, value := range fields {
		if key == "trust" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("MCP server trust cannot be null; omit it for compatibility or use disabled: true")
		}
	}
	type plain ServerConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded plain
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*s = ServerConfig(decoded)
	return nil
}

// UnmarshalYAML rejects misspelled authority fields instead of silently
// turning a security-sensitive configuration into a different policy.
func (c *MCPTrustConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("MCP trust must be a mapping")
	}
	allowed := map[string]bool{
		"disabled": true, "local_owner": true, "gateway": true,
		"annotations": true, "all": true, "all_servers": true,
		"read_only": true, "workspace_effectful": true,
	}
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index].Value
		if !allowed[key] {
			return fmt.Errorf("unknown MCP trust field %q", key)
		}
	}
	type plain MCPTrustConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = MCPTrustConfig(decoded)
	return nil
}

// UnmarshalJSON provides the same strict boundary for shared agents metadata.
func (c *MCPTrustConfig) UnmarshalJSON(data []byte) error {
	allowed := map[string]bool{
		"disabled": true, "local_owner": true, "gateway": true,
		"annotations": true, "all": true, "all_servers": true,
		"read_only": true, "workspace_effectful": true,
	}
	if _, err := strictJSONObjectFields(data, "MCP trust", allowed); err != nil {
		return err
	}
	type plain MCPTrustConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded plain
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("MCP trust contains trailing JSON values")
		}
		return fmt.Errorf("decode trailing MCP trust data: %w", err)
	}
	*c = MCPTrustConfig(decoded)
	return nil
}

func strictJSONObjectFields(data []byte, objectName string, allowed map[string]bool) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("%s must be a JSON object", objectName)
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("%s has a non-string field name", objectName)
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("%s field %q is duplicated", objectName, key)
		}
		if !allowed[key] {
			return nil, fmt.Errorf("unknown %s field %q", objectName, key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%s contains trailing JSON values", objectName)
		}
		return nil, fmt.Errorf("decode trailing %s data: %w", objectName, err)
	}
	return fields, nil
}

//go:embed default_mcp_trust.yaml
var defaultMCPTrustYAML []byte

type defaultMCPTrustFile struct {
	Profiles map[string]MCPTrustConfig `yaml:"profiles"`
}

var defaultMCPTrustProfiles = mustLoadDefaultMCPTrustProfiles()

func mustLoadDefaultMCPTrustProfiles() map[string]MCPTrustConfig {
	var document defaultMCPTrustFile
	if err := yaml.Unmarshal(defaultMCPTrustYAML, &document); err != nil {
		panic(fmt.Sprintf("decode built-in MCP trust profiles: %v", err))
	}
	if len(document.Profiles) == 0 {
		panic("built-in MCP trust profiles are empty")
	}
	profiles := make(map[string]MCPTrustConfig, len(document.Profiles))
	for executable, profile := range document.Profiles {
		server := ServerConfig{Name: executable, Command: executable, Trust: &profile}
		canonical, err := validateAndCanonicalizeMCPTrust(server, profile)
		if err != nil {
			panic(fmt.Sprintf("validate built-in MCP trust profile %q: %v", executable, err))
		}
		profiles[executable] = canonical
	}
	return profiles
}

// ResolveMCPTrust returns a defensive, canonical copy of the exact authority
// configured for server. An omitted policy receives the build-owned legacy
// profile only when the existing local-STDIO/basename safety floor holds.
// Explicit policy always replaces that compatibility profile.
func ResolveMCPTrust(server ServerConfig) (*MCPTrustConfig, error) {
	var trust MCPTrustConfig
	switch {
	case server.Trust != nil:
		trust = cloneMCPTrust(*server.Trust)
	case eligibleForDefaultMCPTrust(server):
		profile, ok := defaultMCPTrustProfiles[filepath.Base(filepath.Clean(server.Command))]
		if !ok {
			return nil, nil
		}
		trust = cloneMCPTrust(profile)
	default:
		return nil, nil
	}
	canonical, err := validateAndCanonicalizeMCPTrust(server, trust)
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

func eligibleForDefaultMCPTrust(server ServerConfig) bool {
	if !validMCPTrustNamespace(server.Name) || server.Command == "" || strings.TrimSpace(server.Command) != server.Command || server.URL != "" {
		return false
	}
	transport := server.Transport
	if transport != "" && transport != "stdio" {
		return false
	}
	_, ok := defaultMCPTrustProfiles[filepath.Base(filepath.Clean(server.Command))]
	return ok
}

func validateAndCanonicalizeMCPTrust(server ServerConfig, trust MCPTrustConfig) (MCPTrustConfig, error) {
	if trust.Disabled {
		if trust.LocalOwner != "" || trust.Gateway != "" || trust.Annotations != "" || trust.All ||
			len(trust.AllServers) != 0 || len(trust.ReadOnly) != 0 || len(trust.WorkspaceEffectful) != 0 {
			return MCPTrustConfig{}, fmt.Errorf("disabled MCP trust cannot declare owner, gateway, or contracts")
		}
		return MCPTrustConfig{Disabled: true}, nil
	}
	if trust.Annotations != "" && trust.Annotations != MCPTrustAnnotationsHonor {
		return MCPTrustConfig{}, fmt.Errorf("MCP trust annotations must be %q or omitted, got %q", MCPTrustAnnotationsHonor, trust.Annotations)
	}
	if !validMCPTrustNamespace(server.Name) {
		return MCPTrustConfig{}, fmt.Errorf("MCP trust requires an exact valid server namespace")
	}
	if server.URL != "" || (server.Transport != "" && server.Transport != "stdio") {
		return MCPTrustConfig{}, fmt.Errorf("MCP trust requires local stdio transport")
	}
	if server.Command == "" || strings.TrimSpace(server.Command) != server.Command {
		return MCPTrustConfig{}, fmt.Errorf("MCP trust requires an exact non-empty command")
	}
	if !validMCPTrustIdentifier(trust.LocalOwner) {
		return MCPTrustConfig{}, fmt.Errorf("MCP trust local_owner %q is not a canonical identifier", trust.LocalOwner)
	}
	basename := filepath.Base(filepath.Clean(server.Command))
	if basename != trust.LocalOwner {
		return MCPTrustConfig{}, fmt.Errorf("MCP trust local_owner %q does not match command basename %q", trust.LocalOwner, basename)
	}
	if trust.Gateway != "" && trust.Gateway != MCPTrustGatewayMCPHub {
		return MCPTrustConfig{}, fmt.Errorf("MCP trust gateway %q is unsupported", trust.Gateway)
	}
	if trust.Gateway == MCPTrustGatewayMCPHub && trust.LocalOwner != MCPTrustGatewayMCPHub {
		return MCPTrustConfig{}, fmt.Errorf("MCPHub gateway trust requires local_owner %q", MCPTrustGatewayMCPHub)
	}
	if len(trust.AllServers) != 0 && trust.Gateway != MCPTrustGatewayMCPHub {
		return MCPTrustConfig{}, fmt.Errorf("MCP trust all_servers names downstream namespaces and requires gateway %q", MCPTrustGatewayMCPHub)
	}
	seenDownstream := make(map[string]struct{}, len(trust.AllServers))
	for _, downstream := range trust.AllServers {
		if !validMCPTrustIdentifier(downstream) {
			return MCPTrustConfig{}, fmt.Errorf("MCP trust all_servers entry %q is not a canonical identifier", downstream)
		}
		if _, duplicate := seenDownstream[downstream]; duplicate {
			return MCPTrustConfig{}, fmt.Errorf("MCP trust all_servers entry %q is duplicated", downstream)
		}
		seenDownstream[downstream] = struct{}{}
	}
	total := len(trust.ReadOnly) + len(trust.WorkspaceEffectful) + len(trust.AllServers)
	if total == 0 && !trust.All && trust.Annotations != MCPTrustAnnotationsHonor {
		return MCPTrustConfig{}, fmt.Errorf("MCP trust must declare at least one exact contract, all, all_servers, annotations: honor, or disabled: true")
	}
	if total > maxMCPTrustContracts {
		return MCPTrustConfig{}, fmt.Errorf("MCP trust declares %d contracts; maximum is %d", total, maxMCPTrustContracts)
	}
	seen := make(map[string]string, total)
	validateRoutes := func(class string, routes []string) error {
		for _, route := range routes {
			if err := validateMCPTrustRoute(server.Name, trust.Gateway, route); err != nil {
				return fmt.Errorf("%s route %q: %w", class, route, err)
			}
			if previous, duplicate := seen[route]; duplicate {
				return fmt.Errorf("route %q is duplicated in %s and %s", route, previous, class)
			}
			seen[route] = class
		}
		return nil
	}
	if err := validateRoutes("read_only", trust.ReadOnly); err != nil {
		return MCPTrustConfig{}, err
	}
	if err := validateRoutes("workspace_effectful", trust.WorkspaceEffectful); err != nil {
		return MCPTrustConfig{}, err
	}
	canonical := cloneMCPTrust(trust)
	sort.Strings(canonical.ReadOnly)
	sort.Strings(canonical.WorkspaceEffectful)
	sort.Strings(canonical.AllServers)
	return canonical, nil
}

func validateMCPTrustRoute(serverName, gateway, route string) error {
	if route == "" || len(route) > maxMCPTrustRouteBytes || !utf8.ValidString(route) || strings.TrimSpace(route) != route {
		return fmt.Errorf("route is empty, oversized, non-UTF-8, or whitespace-padded")
	}
	parts := strings.Split(route, "__")
	if gateway == "" && len(parts) != 1 {
		return fmt.Errorf("direct routes cannot contain the reserved delimiter __")
	}
	if gateway == MCPTrustGatewayMCPHub && len(parts) > 2 {
		return fmt.Errorf("MCPHub routes may contain at most one downstream delimiter __")
	}
	for _, part := range parts {
		if !validMCPTrustIdentifier(part) {
			return fmt.Errorf("route segment %q is not a canonical identifier", part)
		}
	}
	if gateway == MCPTrustGatewayMCPHub && len(parts) == 1 && parts[0] == "mcphub_call_tool" {
		return fmt.Errorf("mcphub_call_tool authority must come from an exact downstream route")
	}
	if len(serverName)+2+len(route) > execution.MaxToolNameBytes {
		return fmt.Errorf("exposed tool name exceeds %d bytes", execution.MaxToolNameBytes)
	}
	return nil
}

func validMCPTrustIdentifier(value string) bool {
	if value == "" || len(value) > maxMCPTrustIdentifierBytes || !utf8.ValidString(value) || strings.Contains(value, "__") {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || (index > 0 && (char == '_' || char == '-')) {
			continue
		}
		return false
	}
	return true
}

func validMCPTrustNamespace(value string) bool {
	return value != "" && len(value) <= execution.MaxToolNameBytes && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && !strings.Contains(value, "__") &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func cloneMCPTrust(trust MCPTrustConfig) MCPTrustConfig {
	trust.ReadOnly = append([]string(nil), trust.ReadOnly...)
	trust.WorkspaceEffectful = append([]string(nil), trust.WorkspaceEffectful...)
	trust.AllServers = append([]string(nil), trust.AllServers...)
	return trust
}
