package agent

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	mcpPkg "github.com/abdul-hamid-achik/sonar/internal/mcp"
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
	// annotationsHonored delegates read-only classification to the server's
	// own tool annotations — explicit per-server opt-in, never a default.
	annotationsHonored bool
	// allTools / allDownstream are approval-only grants: AUTO runs the call
	// unattended, but the effect class stays effectful, so NORMAL still asks
	// and an unanswered call still has an unknown outcome.
	allTools      bool
	allDownstream map[string]struct{}
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
		honored := trust.Annotations == config.MCPTrustAnnotationsHonor
		var allDownstream map[string]struct{}
		if len(trust.AllServers) != 0 {
			allDownstream = make(map[string]struct{}, len(trust.AllServers))
			for _, downstream := range trust.AllServers {
				allDownstream[downstream] = struct{}{}
			}
		}
		if len(contracts) == 0 && !honored && !trust.All && allDownstream == nil {
			continue
		}
		trusted[server.Name] = trustedMCPServer{
			localOwner: trust.LocalOwner, gateway: trust.Gateway, contracts: contracts,
			annotationsHonored: honored, allTools: trust.All, allDownstream: allDownstream,
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

// trustedMCPContract resolves host-configured direct or MCPHub routes. Suffix
// matching is forbidden: `evil__cortex_status` must never gain authority merely
// by resembling a configured operation. MCP annotations and descriptions remain
// presentation metadata with exactly one exception: a server the operator
// marked `annotations: honor` has its own read-only declarations honored — an
// explicit per-server delegation, resolved after exact routes and never for
// the lazy call_tool wrapper, whose annotation describes the proxy rather than
// the downstream target.
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
	downstream := ""
	lazyWrapper := false
	switch {
	case server.gateway == "" && len(parts) == 2:
		route = parts[1]
	case server.gateway == config.MCPTrustGatewayMCPHub && len(parts) == 2:
		if parts[1] == "mcphub_call_tool" {
			lazyWrapper = true
			target, tool, exact := exactLazyMCPHubTarget(call.Arguments)
			if !exact {
				return mcpAuthorityContract{}, false
			}
			downstream = target
			route = target + "__" + tool
		} else {
			route = parts[1]
		}
	case server.gateway == config.MCPTrustGatewayMCPHub && len(parts) == 3:
		downstream = parts[1]
		route = parts[1] + "__" + parts[2]
	default:
		return mcpAuthorityContract{}, false
	}
	if contract, found := server.contracts[route]; found {
		return contract, true
	}
	if server.annotationsHonored && !lazyWrapper {
		if contract, honored := annotatedReadOnlyContract(a.mcpTools(), call.Name); honored {
			return contract, true
		}
	}
	if server.allTools {
		return serverTrustApprovalContract(), true
	}
	if downstream != "" {
		if _, granted := server.allDownstream[downstream]; granted {
			return serverTrustApprovalContract(), true
		}
	}
	return mcpAuthorityContract{}, false
}

// mcpServerNamespaceForCall names the call's effective server namespace — the
// subject a whole-server grant binds to. For a trusted MCPHub gateway the
// namespace is the downstream target (from the eager three-part name or the
// exact lazy arguments; an inexact lazy target yields "", so no server grant
// can cover a call whose destination the host cannot pin). For everything
// else it is the connected server itself. Returns "" for non-MCP names.
func (a *Agent) mcpServerNamespaceForCall(tc llm.ToolCall) string {
	if tc.Name == "" || strings.TrimSpace(tc.Name) != tc.Name {
		return ""
	}
	parts := strings.Split(tc.Name, "__")
	if len(parts) < 2 {
		return ""
	}
	head := parts[0]
	server, trusted := a.trustedMCPServer(head)
	if !trusted || server.gateway != config.MCPTrustGatewayMCPHub {
		if len(parts) == 2 {
			return head
		}
		return ""
	}
	switch {
	case len(parts) == 2 && parts[1] == "mcphub_call_tool":
		downstream, _, exact := exactLazyMCPHubTarget(tc.Arguments)
		if !exact {
			return ""
		}
		return downstream
	case len(parts) == 3:
		return parts[1]
	case len(parts) == 2:
		return head
	default:
		return ""
	}
}

// serverTrustApprovalContract is the approval-only grant behind `all` and
// `all_servers`: auto under AUTO authority, effectful class. authorityAutoApproves
// reaches its non-read branch, which requires AuthorityAutoScoped — so NORMAL
// still asks — and an unanswered call keeps the conservative outcome handling.
func serverTrustApprovalContract() mcpAuthorityContract {
	return mcpAuthorityContract{effect: executionpkg.Effectful, auto: true}
}

// annotatedReadOnlyContract grants read-only authority from the server's own
// tool declaration. Reached only under the explicit `annotations: honor`
// delegation; an absent definition or an undeclared hint fails closed.
func annotatedReadOnlyContract(defs []llm.ToolDef, name string) (mcpAuthorityContract, bool) {
	for _, def := range defs {
		if def.Name != name {
			continue
		}
		if def.Behavior.Declared && def.Behavior.ReadOnly {
			return mcpAuthorityContract{effect: executionpkg.EffectReadOnly, auto: true}, true
		}
		return mcpAuthorityContract{}, false
	}
	return mcpAuthorityContract{}, false
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
		case "copy", "move":
			return a.workspaceTransferAutoApproved(call)
		case "remove":
			return a.workspaceRemoveAutoApproved(call)
		case "bash":
			command, ok := call.Arguments["command"].(string)
			return ok && a.autoScopedCommandAllowed(command)
		default:
			return false
		}
	case executionpkg.KindMemory:
		// The memory store is workspace-scoped, owner-only JSON, so mutating
		// it is the same risk class as an in-workspace write — which AUTO
		// already runs unattended. Named explicitly rather than blanket-true
		// so a future memory tool does not auto-approve by omission.
		switch call.Name {
		case "memory_save", "memory_update", "memory_delete":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

// workspaceTransferAutoApproved reports whether a copy/move keeps its mutation
// inside the active workspace proper. Write grants are deliberately excluded:
// additionalWriteAllowsTool defines a grant as write/edit/mkdir authority, and
// auto-approving copy against it would widen a grant the user already scoped.
// copy reads its source through the read path, which never needs approval, so
// only its destination is load-bearing; move renames, so both endpoints are.
func (a *Agent) workspaceTransferAutoApproved(call llm.ToolCall) bool {
	destination, ok := call.Arguments["destination"].(string)
	if !ok || strings.TrimSpace(destination) == "" {
		return false
	}
	if _, err := a.resolveWorkspacePath(destination); err != nil {
		return false
	}
	if call.Name != "move" {
		return true
	}
	source, ok := call.Arguments["source"].(string)
	if !ok || strings.TrimSpace(source) == "" {
		return false
	}
	_, err := a.resolveWorkspacePath(source)
	return err == nil
}

// workspaceRemoveAutoApproved admits only the non-recursive form: removing one
// file or an empty directory in the workspace is the risk class of the write
// clobber AUTO already runs, while recursive removal is the rm -rf class that
// stays prompted everywhere else — even confined bash refuses it. recursive is
// read through getArgBool so authorization and execution parse it identically.
func (a *Agent) workspaceRemoveAutoApproved(call llm.ToolCall) bool {
	if a.getArgBool(call.Arguments, "recursive", false) {
		return false
	}
	path, ok := call.Arguments["path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return false
	}
	_, err := a.resolveWorkspacePath(path)
	return err == nil
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

// workspaceArgumentKey is the exact argument name host-catalogued
// workspace-effectful MCP routes use to scope their effect to one repository.
const workspaceArgumentKey = "workspace"

// pinCataloguedWorkspaceArgument binds the harness's active workspace to a
// host-catalogued workspace-effectful MCP call whose advertised input schema
// declares a `workspace` property that the model left out. It returns the
// pinned call and true only when the pin is what authorizes the call.
//
// This narrows authority; it never widens it. Downstream servers declare
// `workspace` as optional and default it to their own working directory — a
// directory the harness never resolved, never contained, and cannot name in an
// approval prompt. Omitting the argument therefore does not mean "no
// workspace", it means "some workspace chosen by the server". Before this pin,
// mcpWorkspaceWithinAuthority had nothing to check and fell through to the
// prompt, so a route the operator explicitly listed under workspace_effectful
// still interrupted them, and approving it authorized an unnamed target. After
// it, the effect is pinned to the workspace the host already owns and the
// existing containment check verifies a real path.
//
// Every one of the following must hold, or the call is returned untouched:
//
//   - the turn holds AuthorityAutoScoped and the call dispatches as MCP, so
//     NORMAL and PLAN turns are unaffected;
//   - the exact host trust catalogue resolves the route to an automatic,
//     workspace-scoped contract; read-only and uncatalogued routes are never
//     rewritten, and neither is a route under an explicit permission deny;
//   - the harness has an active workspace;
//   - the route's own advertised schema in this turn's registry snapshot
//     declares `workspace`, so the harness never invents an argument a server
//     did not say it accepts;
//   - the model supplied no value — a model-supplied workspace is left alone
//     for mcpWorkspaceWithinAuthority to contain or reject;
//   - the pinned call classifies to the same durable kind and effect, and
//     actually passes containment.
//
// The lazy MCPHub `mcphub_call_tool` shape is deliberately excluded. Its
// advertised schema describes the gateway's own `server`/`tool`/`arguments`
// envelope, not the downstream target's inputs, so no advertised schema can
// satisfy the rule above for the nested map. The only downstream schemas the
// harness ever holds are the turn-scoped continuation contracts, which are
// gateway-supplied, model-context-only state kept for rejecting malformed
// continuation arguments; promoting them into an authorization input would let
// remote describe output decide what the host injects. Nested gateway calls
// therefore keep requiring an explicit workspace and keep prompting.
func (a *Agent) pinCataloguedWorkspaceArgument(
	mode AuthorityMode, call llm.ToolCall, kind executionpkg.Kind, snapshot mcpPkg.ToolSnapshot,
) (llm.ToolCall, bool) {
	if a == nil || mode != AuthorityAutoScoped || kind != executionpkg.KindMCP {
		return call, false
	}
	if a.isTrustedLazyMCPHubCall(call.Name) {
		return call, false
	}
	workspace := strings.TrimSpace(a.activeWorkDir())
	if workspace == "" {
		return call, false
	}
	contract, trusted := a.trustedMCPContract(call)
	if !trusted || !contract.auto || !contract.workspaceScoped ||
		contract.effect == executionpkg.EffectReadOnly {
		return call, false
	}
	if a.authorityPermissionDeniedForCall(call) {
		return call, false
	}
	if raw, present := call.Arguments[workspaceArgumentKey]; present && raw != nil {
		return call, false
	}
	if !mcpSchemaDeclaresWorkspace(snapshot, call.Name) {
		return call, false
	}

	pinned := call
	pinned.Arguments = cloneApprovalArguments(call.Arguments)
	if pinned.Arguments == nil {
		pinned.Arguments = make(map[string]any, 1)
	}
	pinned.Arguments[workspaceArgumentKey] = workspace

	// Route resolution for the direct shapes never reads arguments, so pinning
	// cannot move the call to a different contract. Restate that as a check
	// rather than a comment: a future route rule that did read arguments would
	// otherwise silently let this rewrite reclassify a durable effect.
	pinnedKind, pinnedEffect := a.executionKindForCall(pinned)
	originalKind, originalEffect := a.executionKindForCall(call)
	if pinnedKind != originalKind || pinnedEffect != originalEffect {
		return call, false
	}
	// Mutate the request only when the mutation is what grants authority. If
	// the pinned path is somehow not containable, leave the call exactly as the
	// model wrote it so the operator approves the real request.
	if !a.mcpWorkspaceWithinAuthority(pinned) {
		return call, false
	}
	return pinned, true
}

// mcpSchemaDeclaresWorkspace reports whether the exact advertised definition for
// name in this turn's registry snapshot declares a `workspace` property that an
// absolute path string can satisfy. A declared type other than string is
// refused: injecting a value the server's own schema rejects would turn a
// prompt into a failed call.
func mcpSchemaDeclaresWorkspace(snapshot mcpPkg.ToolSnapshot, name string) bool {
	for _, definition := range snapshot.Tools {
		if definition.Name != name {
			continue
		}
		properties, ok := definition.Parameters["properties"].(map[string]any)
		if !ok {
			return false
		}
		declared, present := properties[workspaceArgumentKey]
		if !present {
			return false
		}
		schema, ok := declared.(map[string]any)
		if !ok {
			return false
		}
		switch declaredType := schema["type"].(type) {
		case nil:
			// cortex and friends describe `workspace` with a description and no
			// type. An untyped property still declares the argument exists.
			return true
		case string:
			return declaredType == "string"
		case []any:
			for _, entry := range declaredType {
				if text, isText := entry.(string); isText && text == "string" {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
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
