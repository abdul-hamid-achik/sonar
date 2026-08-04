package agent

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	permissionpkg "github.com/abdul-hamid-achik/sonar/internal/permission"
)

// AuthorityMode is the host-owned authority granted to one conversational
// turn. It is deliberately separate from the model-facing mode prompt and the
// advertised tool set: changing prose or untrusted MCP annotations must never
// widen execution authority.
type AuthorityMode uint8

const (
	// AuthorityNormal keeps every risky operation on the configured permission
	// path. Read-only built-ins retain their existing implicit authorization.
	AuthorityNormal AuthorityMode = iota
	// AuthorityPlan is the typed companion to the read-only planning tool
	// policy. It never grants an automatic mutation by itself.
	AuthorityPlan
	// AuthorityAutoScoped permits only host-catalogued, workspace-scoped
	// operations to bypass an interactive modal. Ordinary local development
	// commands are included; destructive, externally effectful, dynamic shell,
	// and non-catalogued MCP calls still use the normal permission path.
	AuthorityAutoScoped
)

// ApprovalPosture is process-local approval UX policy on the Agent. It does
// not replace permission deny policies or the host skip-approvals posture, and
// it is never persisted.
type ApprovalPosture uint8

const (
	// ApprovalPosturePrompted is the default: approval-gated tools prompt
	// unless another authority path (AUTO scope, session grant) applies.
	ApprovalPosturePrompted ApprovalPosture = iota
	// ApprovalPostureAcceptWorkspaceEdits auto-approves write/edit/mkdir when
	// the target resolves inside the workspace or an explicit write grant,
	// including under AuthorityNormal. bash, remove, MCP, and memory stay gated.
	ApprovalPostureAcceptWorkspaceEdits
)

// Valid reports whether posture is a supported process-local approval posture.
func (p ApprovalPosture) Valid() bool {
	switch p {
	case ApprovalPosturePrompted, ApprovalPostureAcceptWorkspaceEdits:
		return true
	default:
		return false
	}
}

// SetApprovalPosture installs process-local approval UX policy. Invalid values
// fail closed to prompted. Explicit tool denies still win.
func (a *Agent) SetApprovalPosture(posture ApprovalPosture) {
	if a == nil {
		return
	}
	if !posture.Valid() {
		posture = ApprovalPosturePrompted
	}
	a.mu.Lock()
	a.approvalPosture = posture
	a.mu.Unlock()
}

// ApprovalPosture returns the process-local approval UX policy.
func (a *Agent) ApprovalPosture() ApprovalPosture {
	if a == nil {
		return ApprovalPosturePrompted
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.approvalPosture
}

// Valid reports whether mode is a supported host authority.
func (mode AuthorityMode) Valid() bool {
	switch mode {
	case AuthorityNormal, AuthorityPlan, AuthorityAutoScoped:
		return true
	default:
		return false
	}
}

// SetAuthorityMode installs the authority to snapshot at the start of the next
// turn. Invalid values fail closed to NORMAL. A running turn keeps the value it
// captured at admission, so a concurrent UI mode change cannot widen it.
func (a *Agent) SetAuthorityMode(mode AuthorityMode) {
	if !mode.Valid() {
		mode = AuthorityNormal
	}
	a.mu.Lock()
	a.authorityMode = mode
	a.mu.Unlock()
}

// AuthorityMode returns the authority that the next turn will snapshot.
func (a *Agent) AuthorityMode() AuthorityMode {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.authorityMode.Valid() {
		return AuthorityNormal
	}
	return a.authorityMode
}

type mcpAuthorityContract struct {
	effect          executionpkg.EffectClass
	auto            bool
	workspaceScoped bool
}

type trustedMCPServer struct {
	localOwner string
	gateway    string
	contracts  map[string]mcpAuthorityContract
}

// SetTrustedLocalMCPServers derives namespace trust exclusively from the
// host-resolved configuration. Explicit exact-route trust replaces the legacy
// profile; an omitted policy may receive the build-owned compatibility profile.
// config.ResolveMCPTrust keeps local STDIO and exact executable basename as the
// safety floor, so remote transports, wrappers, and lookalikes fail closed.
// Call this once with the same server list used to connect the Registry, before
// starting turns.
func (a *Agent) SetTrustedLocalMCPServers(servers []config.ServerConfig) {
	trusted := make(map[string]trustedMCPServer)
	namespaceCounts := make(map[string]int, len(servers))
	for _, server := range servers {
		namespaceCounts[server.Name]++
	}
	for _, server := range servers {
		if namespaceCounts[server.Name] != 1 {
			continue
		}
		trust, err := config.ResolveMCPTrust(server)
		if err != nil || trust == nil || trust.Disabled {
			continue
		}
		contracts := make(map[string]mcpAuthorityContract, len(trust.ReadOnly)+len(trust.WorkspaceEffectful))
		for _, route := range trust.ReadOnly {
			contracts[route] = mcpAuthorityContract{effect: executionpkg.EffectReadOnly, auto: true}
		}
		for _, route := range trust.WorkspaceEffectful {
			contracts[route] = mcpAuthorityContract{
				effect: executionpkg.Effectful, auto: true, workspaceScoped: true,
			}
		}
		if len(contracts) == 0 {
			continue
		}
		trusted[server.Name] = trustedMCPServer{
			localOwner: trust.LocalOwner, gateway: trust.Gateway, contracts: contracts,
		}
	}
	a.mu.Lock()
	a.trustedMCP = trusted
	a.approvalHostVersion++
	a.mcpRouteVersion++
	a.mu.Unlock()
	a.clearContinuationContracts()
}

func (a *Agent) mcpRouteVersionSnapshot() uint64 {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mcpRouteVersion
}

func (a *Agent) trustedMCPServer(namespace string) (trustedMCPServer, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	server, ok := a.trustedMCP[namespace]
	return server, ok
}

func (a *Agent) isTrustedMCPHubNamespace(namespace string) bool {
	server, ok := a.trustedMCPServer(namespace)
	return ok && server.gateway == config.MCPTrustGatewayMCPHub
}

// trustedMCPHubNamespaces snapshots the host-configured MCPHub namespaces for
// one turn. Tool names and MCP descriptions are remote presentation data, so
// schema admission must never infer this authority from an operation suffix.
func (a *Agent) trustedMCPHubNamespaces() map[string]struct{} {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	trusted := make(map[string]struct{})
	for namespace, server := range a.trustedMCP {
		if server.gateway == config.MCPTrustGatewayMCPHub {
			trusted[namespace] = struct{}{}
		}
	}
	return trusted
}

// trustedMCPContract resolves only exact host-configured direct or MCPHub
// routes. Suffix matching is forbidden: `evil__cortex_status` must never gain
// authority merely by resembling a configured operation. MCP annotations and
// descriptions remain presentation metadata and are never consulted here.
func (a *Agent) trustedMCPContract(call llm.ToolCall) (mcpAuthorityContract, bool) {
	if call.Name == "" || strings.TrimSpace(call.Name) != call.Name {
		return mcpAuthorityContract{}, false
	}
	parts := strings.Split(call.Name, "__")
	if len(parts) < 2 {
		return mcpAuthorityContract{}, false
	}
	server, ok := a.trustedMCPServer(parts[0])
	if !ok {
		return mcpAuthorityContract{}, false
	}
	route := ""
	switch {
	case server.gateway == "" && len(parts) == 2:
		route = parts[1]
	case server.gateway == config.MCPTrustGatewayMCPHub && len(parts) == 2:
		if parts[1] == "mcphub_call_tool" {
			downstream, tool, exact := exactLazyMCPHubTarget(call.Arguments)
			if !exact {
				return mcpAuthorityContract{}, false
			}
			route = downstream + "__" + tool
		} else {
			route = parts[1]
		}
	case server.gateway == config.MCPTrustGatewayMCPHub && len(parts) == 3:
		route = parts[1] + "__" + parts[2]
	default:
		return mcpAuthorityContract{}, false
	}
	contract, found := server.contracts[route]
	return contract, found
}

// trustedMCPOutcomeContract recognizes exact, build-owned semantic contracts
// that remain approval-required but can prove a downstream terminal answer.
// It is deliberately separate from trustedMCPContract: this function never
// grants automatic execution authority. The compatibility entries cover both
// direct downstream names and MCPHub 0.20's clean public aliases.
func (a *Agent) trustedMCPOutcomeContract(call llm.ToolCall) bool {
	if _, ok := a.trustedMCPContract(call); ok {
		return true
	}
	parts := strings.Split(call.Name, "__")
	if len(parts) < 2 || !a.isTrustedMCPHubNamespace(parts[0]) {
		return false
	}
	server, tool := "", ""
	switch {
	case len(parts) == 3:
		server, tool = parts[1], parts[2]
	case len(parts) == 2 && parts[1] == "mcphub_call_tool":
		var exact bool
		server, tool, exact = exactLazyMCPHubTarget(call.Arguments)
		if !exact {
			return false
		}
	default:
		return false
	}
	if server != "hitspec" {
		return false
	}
	switch tool {
	case "search_web", "hitspec_search_web", "fetch", "hitspec_fetch",
		"capture_webpage", "hitspec_capture_webpage":
		return true
	default:
		return false
	}
}

func exactLazyMCPHubTarget(args map[string]any) (server, tool string, ok bool) {
	rawTool, toolOK := args["tool"].(string)
	if !toolOK || rawTool == "" || strings.TrimSpace(rawTool) != rawTool {
		return "", "", false
	}
	rawServer, hasServer := args["server"]
	if hasServer {
		server, ok = rawServer.(string)
		if !ok || strings.TrimSpace(server) != server || strings.Contains(server, "__") {
			return "", "", false
		}
	}
	if !hasServer || server == "" {
		var found bool
		server, tool, found = strings.Cut(rawTool, "__")
		if !found || server == "" || tool == "" {
			return "", "", false
		}
	} else {
		tool = strings.TrimPrefix(rawTool, server+"__")
	}
	if tool == "" || strings.Contains(tool, "__") {
		return "", "", false
	}
	return server, tool, true
}

func (a *Agent) authorityAutoApproves(mode AuthorityMode, call llm.ToolCall, kind executionpkg.Kind) bool {
	if a.authorityPermissionDeniedForCall(call) {
		return false
	}
	if kind == executionpkg.KindMCP {
		contract, ok := a.trustedMCPContract(call)
		if !ok || !contract.auto {
			return false
		}
		// A host-catalogued read has the same authority regardless of transport:
		// read/find/grep built-ins do not open mutation approval modals, so the
		// equivalent local MCP read must not become noisier merely because it is
		// routed through MCPHub. Explicit deny above still wins.
		if contract.effect == executionpkg.EffectReadOnly {
			return true
		}
		if mode != AuthorityAutoScoped {
			return false
		}
		return !contract.workspaceScoped || a.mcpWorkspaceWithinAuthority(call)
	}
	// Process-local "accept workspace edits" posture auto-approves only the
	// write/edit/mkdir built-ins when the path is workspace- or grant-scoped.
	// Deny policies above still win; bash/remove/MCP/memory never qualify.
	if a.ApprovalPosture() == ApprovalPostureAcceptWorkspaceEdits &&
		kind == executionpkg.KindBuiltin &&
		a.workspaceEditPathAutoApproved(call) {
		return true
	}
	if mode != AuthorityAutoScoped {
		return false
	}
	switch kind {
	case executionpkg.KindBuiltin:
		if strings.TrimSpace(a.activeWorkDir()) == "" {
			return false
		}
		switch call.Name {
		case "write", "edit", "mkdir":
			return a.workspaceEditPathAutoApproved(call)
		case "bash":
			command, ok := call.Arguments["command"].(string)
			return ok && a.autoScopedCommandAllowed(command)
		default:
			return false
		}
	default:
		return false
	}
}

// workspaceEditPathAutoApproved reports whether a write/edit/mkdir call targets
// a path that resolves inside the active workspace or an explicit write grant.
func (a *Agent) workspaceEditPathAutoApproved(call llm.ToolCall) bool {
	switch call.Name {
	case "write", "edit", "mkdir":
	default:
		return false
	}
	if strings.TrimSpace(a.activeWorkDir()) == "" {
		return false
	}
	path, ok := call.Arguments["path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return false
	}
	if _, err := a.resolveWorkspacePath(path); err == nil {
		return true
	}
	resolved, err := a.resolveAdditionalWritePath(path)
	return err == nil && a.additionalWriteAllowsTool(resolved, call.Name)
}

func (a *Agent) authorityPermissionDenied(toolName string) bool {
	checker := a.permissionChecker()
	return checker != nil && checker.ToCheckResult(toolName) == permissionpkg.CheckDeny
}

func (a *Agent) authorityPermissionDeniedForCall(call llm.ToolCall) bool {
	checker := a.permissionChecker()
	return checker != nil && a.permissionCheckResult(checker, call) == permissionpkg.CheckDeny
}

// permissionCheckResult preserves the policy result for the exposed call name
// but lets an exact deny on a canonical pinned MCPHub route also block the lazy
// call_tool spelling of that same downstream effect. Allows are not propagated
// across spellings, so this aliasing can only narrow authority.
func (a *Agent) permissionCheckResult(checker *permissionpkg.Checker, call llm.ToolCall) permissionpkg.CheckResult {
	result := checker.ToCheckResult(call.Name)
	canonical, ok := a.canonicalGatewayPermissionName(call)
	if ok && canonical != call.Name && checker.ToCheckResult(canonical) == permissionpkg.CheckDeny {
		return permissionpkg.CheckDeny
	}
	return result
}

func (a *Agent) canonicalGatewayPermissionName(call llm.ToolCall) (string, bool) {
	parts := strings.Split(call.Name, "__")
	if len(parts) < 2 || !a.isTrustedMCPHubNamespace(parts[0]) {
		return "", false
	}
	switch {
	case len(parts) == 3:
		return call.Name, true
	case len(parts) == 2 && parts[1] == "mcphub_call_tool":
		server, tool, ok := exactLazyMCPHubTarget(call.Arguments)
		if !ok {
			return "", false
		}
		canonical := parts[0] + "__" + server + "__" + tool
		if len(canonical) > executionpkg.MaxToolNameBytes || !utf8.ValidString(canonical) ||
			strings.IndexFunc(canonical, unicode.IsControl) >= 0 {
			return "", false
		}
		return canonical, true
	default:
		return "", false
	}
}

func (a *Agent) mcpWorkspaceWithinAuthority(call llm.ToolCall) bool {
	if strings.TrimSpace(a.activeWorkDir()) == "" {
		return false
	}
	args := call.Arguments
	if a.isTrustedLazyMCPHubCall(call.Name) {
		nested, present := args["arguments"]
		if !present || nested == nil {
			return false
		}
		var ok bool
		args, ok = nested.(map[string]any)
		if !ok {
			return false
		}
	}
	raw, present := args["workspace"]
	if !present || raw == nil {
		return false
	}
	workspace, ok := raw.(string)
	if !ok {
		return false
	}
	if strings.TrimSpace(workspace) == "" {
		return false
	}
	if _, err := a.resolveWorkspacePath(workspace); err == nil {
		return true
	}
	resolved, err := a.resolveAdditionalWritePath(workspace)
	return err == nil && a.additionalWriteAllowsWorkspace(resolved)
}

func (a *Agent) isTrustedLazyMCPHubCall(name string) bool {
	parts := strings.Split(name, "__")
	if len(parts) != 2 || parts[1] != "mcphub_call_tool" {
		return false
	}
	server, ok := a.trustedMCPServer(parts[0])
	return ok && server.gateway == config.MCPTrustGatewayMCPHub
}

// isGatewayRoutedMCPCall reports whether the call reaches its effect owner
// through a known gateway hop. A gateway's own reply proves only that the
// gateway answered, not that the downstream server did.
func (a *Agent) isGatewayRoutedMCPCall(name string) bool {
	parts := strings.Split(name, "__")
	if len(parts) < 2 {
		return false
	}
	server, ok := a.trustedMCPServer(parts[0])
	if !ok || server.gateway != config.MCPTrustGatewayMCPHub {
		return false
	}
	return len(parts) == 3 || (len(parts) == 2 && parts[1] == "mcphub_call_tool")
}

// gatewayDownstreamServer resolves which downstream server a gateway-routed
// call addresses, mirroring trustedMCPContract's exact name rules. Gateway
// management operations have no downstream and resolve to false.
func (a *Agent) gatewayDownstreamServer(call llm.ToolCall) (string, bool) {
	parts := strings.Split(call.Name, "__")
	if len(parts) < 2 {
		return "", false
	}
	server, ok := a.trustedMCPServer(parts[0])
	if !ok || server.gateway != config.MCPTrustGatewayMCPHub {
		return "", false
	}
	if len(parts) == 3 {
		return parts[1], true
	}
	if len(parts) == 2 && parts[1] == "mcphub_call_tool" {
		server, _, ok := exactLazyMCPHubTarget(call.Arguments)
		if !ok || server == "" {
			return "", false
		}
		return server, true
	}
	return "", false
}
