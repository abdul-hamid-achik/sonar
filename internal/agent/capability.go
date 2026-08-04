package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/capabilityadvisor"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

const (
	capabilityResolverTimeout    = 3 * time.Second
	maxCapabilityObjective       = 480
	maxCapabilityReroutesPerTurn = 1
)

// CapabilityActivity is host-owned, bounded context for one turn. ScopeID is
// a stable goal ID when a Goal Runtime exists and otherwise a session ID. No
// field may contain raw files, tool output, credentials, or secret values.
type CapabilityActivity struct {
	ScopeID             string
	Objective           string
	Phase               string
	CurrentActivity     string
	DesiredOutcome      string
	AvailableInputKinds []string
	IntentTags          []string
	CatalogRevision     string
	NonTrivial          bool
	Reconsider          bool
	cacheDiscriminator  [sha256.Size]byte
}

type capabilityAdviser interface {
	Advise(context.Context, capabilityadvisor.Request) capabilityadvisor.Result
}

type capabilityRetryKey [sha256.Size]byte

type capabilityToolRegistry interface {
	ResolveToolName(remoteName string) (string, bool)
	CallTool(ctx context.Context, exposedName string, args map[string]any) (*mcp.ToolResult, error)
}

// CapabilityRouteStatus is the bounded host conclusion from MCPHub. It is
// deliberately separate from transport, downstream domain outcome, and
// evidence state.
type CapabilityRouteStatus = capabilityadvisor.Status

const (
	CapabilityRouteResolved    CapabilityRouteStatus = capabilityadvisor.StatusResolved
	CapabilityRouteAmbiguous   CapabilityRouteStatus = capabilityadvisor.StatusAmbiguous
	CapabilityRouteNoMatch     CapabilityRouteStatus = capabilityadvisor.StatusNoMatch
	CapabilityRouteUnavailable CapabilityRouteStatus = capabilityadvisor.StatusUnavailable
	CapabilityRouteInvalid     CapabilityRouteStatus = capabilityadvisor.StatusInvalid
)

// CapabilityRouteFreshness states whether this caller dispatched a resolver
// request or reused an ephemeral result. Unknown covers pre-dispatch
// unavailability, such as a resolver outside the active MCP scope.
type CapabilityRouteFreshness string

const (
	CapabilityRouteFreshnessUnknown CapabilityRouteFreshness = "unknown"
	CapabilityRouteFresh            CapabilityRouteFreshness = "fresh"
	CapabilityRouteCached           CapabilityRouteFreshness = "cached"
)

// CapabilityRoute is an advisory host event. It contains only allowlisted,
// bounded identifiers and state. It is neither a tool execution nor evidence
// that any recommended operation succeeded. Server and Tool are populated
// only for an unambiguous resolved route.
type CapabilityRoute struct {
	Phase           string
	Status          CapabilityRouteStatus
	Freshness       CapabilityRouteFreshness
	Server          string
	Tool            string
	CandidateCount  int
	CatalogRevision string
	Reconsidered    bool
}

// CapabilityRouteOutput is optional so headless and test Output
// implementations do not need to grow transcript state for advisory events.
type CapabilityRouteOutput interface {
	CapabilityRoute(CapabilityRoute)
}

// CapabilityActivityFromPrompt creates a conservative activity projection for
// an ordinary turn. Multiline task descriptions are classified locally after
// whitespace normalization, but raw wording never crosses the resolver
// boundary. Code-fenced, JSON-document, or credential-like prompts still skip
// host-side resolution and remain available to the model-driven MCPHub path.
func CapabilityActivityFromPrompt(scopeID, prompt, phase string, workspaceAvailable bool) CapabilityActivity {
	material, ok := normalizedCapabilityPrompt(prompt)
	if !ok {
		return CapabilityActivity{}
	}
	signal := boundCapabilityText(material, maxCapabilityObjective)
	// File paths and names are inspected only by this local classifier. The
	// resolver receives the resulting allowlisted facets, never the material.
	inputs := capabilityInputKinds(material, workspaceAvailable)
	resolvedPhase := capabilityPhase(signal, phase)
	intentTags := capabilityIntentTags(material, resolvedPhase)
	objective, current, outcome, nonTrivial := projectCapabilityActivity(signal, resolvedPhase, inputs)
	if !nonTrivial {
		return CapabilityActivity{}
	}
	return CapabilityActivity{
		ScopeID:             strings.TrimSpace(scopeID),
		Objective:           objective,
		Phase:               resolvedPhase,
		CurrentActivity:     current,
		DesiredOutcome:      outcome,
		AvailableInputKinds: inputs,
		IntentTags:          intentTags,
		NonTrivial:          true,
		Reconsider:          asksToReconsiderCapabilities(signal),
		// The full validated prompt participates only in this local digest. The
		// resolver still receives the bounded host-authored projection above.
		cacheDiscriminator: capabilityMaterialFingerprint(material),
	}
}

// DurableGoalCapabilityActivity preserves the real goal ID while keeping
// current activity and desired outcome host-authored. The objective must still
// pass the same privacy boundary as an ordinary prompt.
func DurableGoalCapabilityActivity(goalID, objective, phase, activityDiscriminator string) CapabilityActivity {
	signal, ok := capabilityPromptObjective(objective)
	if !ok || strings.TrimSpace(goalID) == "" {
		return CapabilityActivity{}
	}
	inputs := capabilityInputKinds(signal, true)
	resolvedPhase := normalizeHostCapabilityPhase(phase)
	intentTags := capabilityIntentTags(signal, resolvedPhase)
	projectedObjective, _, _, matched := projectCapabilityActivity(signal, resolvedPhase, inputs)
	if !matched {
		projectedObjective = "Advance a durable workspace goal"
	}
	activity := CapabilityActivity{
		ScopeID: strings.TrimSpace(goalID), Objective: projectedObjective, Phase: resolvedPhase,
		AvailableInputKinds: inputs, IntentTags: intentTags, NonTrivial: true,
		Reconsider:         asksToReconsiderCapabilities(signal),
		cacheDiscriminator: capabilityMaterialFingerprint(activityDiscriminator),
	}
	activity.CurrentActivity, activity.DesiredOutcome = durableCapabilityWork(resolvedPhase)
	return activity
}

func durableCapabilityWork(phase string) (current, outcome string) {
	switch phase {
	case "research":
		return "Investigate the next bounded part of the durable goal", "A source-backed finding tied to the goal criteria"
	case "planning":
		return "Design the next repository changes before editing", "A verifiable plan tied to the goal criteria"
	case "verification":
		return "Verify the current workspace state against the goal criteria", "A reproducible verification result"
	case "handoff":
		return "Prepare the verified durable-goal result for handoff", "A bounded durable completion record"
	case "decision":
		return "Clarify the blocked durable-goal decision", "A bounded decision that can safely unblock work"
	default:
		return "Implement the next concrete slice of the durable goal", "Verified progress toward the goal criteria"
	}
}

func (activity CapabilityActivity) request(reconsider bool) capabilityadvisor.Request {
	return capabilityadvisor.Request{
		GoalID:             activity.ScopeID,
		NonTrivial:         activity.NonTrivial,
		Reconsider:         activity.Reconsider || reconsider,
		CatalogRevision:    activity.CatalogRevision,
		CacheDiscriminator: activity.cacheDiscriminator,
		Activity: capabilityadvisor.Activity{
			Objective:           activity.Objective,
			Phase:               activity.Phase,
			CurrentActivity:     activity.CurrentActivity,
			DesiredOutcome:      activity.DesiredOutcome,
			AvailableInputKinds: append([]string(nil), activity.AvailableInputKinds...),
			IntentTags:          append([]string(nil), activity.IntentTags...),
		},
	}
}

// scopedCapabilityRegistry prevents the host-side resolver from escaping the
// current profile's MCP scope or an explicit permission deny. It additionally
// requires the resolved namespace to be the host-pinned local MCPHub binary;
// server-authored names and descriptions can never establish this trust. It
// never changes MCPServerScope and exposes no downstream call surface.
type scopedCapabilityRegistry struct {
	agent   *Agent
	backend capabilityToolRegistry
}

// CapabilityRoutingHostState is the host-owned availability of contextual
// routing. It says nothing about a recommendation, downstream execution,
// domain outcome, or evidence.
type CapabilityRoutingHostState uint8

const (
	// CapabilityRoutingHostUnavailable means trust, active scope, an explicit
	// deny, or the resolver catalog prevents the host from exposing MCPHub.
	CapabilityRoutingHostUnavailable CapabilityRoutingHostState = iota
	// CapabilityRoutingHostServerUnavailable means the trusted resolver remains
	// catalogued, but the registry's current non-blocking snapshot says its MCP
	// namespace is disconnected (or no longer has a live status row).
	CapabilityRoutingHostServerUnavailable
	// CapabilityRoutingHostReady means only that the trusted, allowed resolver's
	// MCP namespace is currently connected.
	CapabilityRoutingHostReady
)

// CapabilityRoutingState combines the resolver's trust/scope/deny projection
// with the registry's cached connection snapshot. It performs no network call.
func (a *Agent) CapabilityRoutingState() CapabilityRoutingHostState {
	if a == nil || a.registry == nil {
		return CapabilityRoutingHostUnavailable
	}
	return capabilityRoutingHostState(a, a.registry, a.registry.ConnectionStatuses())
}

// CapabilityRoutingAvailable reports whether the active scope exposes the
// host-trusted local MCPHub resolver and its namespace is currently connected.
// It performs no network call and conveys neither a route nor downstream
// success.
func (a *Agent) CapabilityRoutingAvailable() bool {
	return a.CapabilityRoutingState() == CapabilityRoutingHostReady
}

func capabilityRoutingHostState(agent *Agent, backend capabilityToolRegistry, statuses []mcp.ConnectionStatus) CapabilityRoutingHostState {
	exposed, ok := (scopedCapabilityRegistry{agent: agent, backend: backend}).ResolveToolName("mcphub_resolve_tool")
	if !ok {
		return CapabilityRoutingHostUnavailable
	}
	namespace, _, namespaced := strings.Cut(exposed, "__")
	if !namespaced || namespace == "" {
		return CapabilityRoutingHostUnavailable
	}
	for _, status := range statuses {
		if status.Name != namespace {
			continue
		}
		if status.Connected {
			return CapabilityRoutingHostReady
		}
		return CapabilityRoutingHostServerUnavailable
	}
	// A retained resolver route without a current connection row is not enough
	// to claim readiness. This also fails closed across disconnect races.
	return CapabilityRoutingHostServerUnavailable
}

func (registry scopedCapabilityRegistry) ResolveToolName(remoteName string) (string, bool) {
	agent := registry.agent
	backend := registry.backend
	if agent == nil || backend == nil || remoteName == "" || strings.TrimSpace(remoteName) != remoteName || strings.Contains(remoteName, "__") {
		return "", false
	}
	// Host-side registry is limited to MCPHub management tools. Downstream
	// specialists stay model- or continuation-owned.
	if !hostMCPHubManagementTool(remoteName) {
		return "", false
	}

	// Resolve exact routes only after projecting the host-trusted MCPHub
	// namespaces. A different, untrusted server may legitimately expose the same
	// protocol-level tool name; it must not make the trusted host integration
	// globally ambiguous. Multiple eligible trusted MCPHub routes still fail
	// closed instead of selecting one by map or configuration order.
	agent.mu.RLock()
	trustedNamespaces := make([]string, 0, len(agent.trustedMCP))
	for namespace, server := range agent.trustedMCP {
		contract, trusted := server.contracts[remoteName]
		if server.gateway == config.MCPTrustGatewayMCPHub && trusted && contract.auto && contract.effect == executionpkg.EffectReadOnly {
			trustedNamespaces = append(trustedNamespaces, namespace)
		}
	}
	agent.mu.RUnlock()
	sort.Strings(trustedNamespaces)

	match := ""
	for _, namespace := range trustedNamespaces {
		candidate := namespace + "__" + remoteName
		exposed, ok := backend.ResolveToolName(candidate)
		if !ok || exposed != candidate || !agent.allowsMCPTool(candidate) || agent.authorityPermissionDenied(candidate) {
			continue
		}
		if match != "" {
			return "", false
		}
		match = candidate
	}
	return match, match != ""
}

func (registry scopedCapabilityRegistry) CallTool(ctx context.Context, exposedName string, args map[string]any) (*mcp.ToolResult, error) {
	if exposedName == "" || strings.Contains(exposedName, "\x00") {
		return nil, fmt.Errorf("capability host tool is outside the active MCP scope")
	}
	namespace, remote, ok := strings.Cut(exposedName, "__")
	if !ok || namespace == "" || !hostMCPHubManagementTool(remote) || strings.Contains(remote, "__") {
		return nil, fmt.Errorf("capability host tool is outside the active MCP scope")
	}
	expected, ok := registry.ResolveToolName(remote)
	if !ok || exposedName != expected {
		return nil, fmt.Errorf("capability host tool is outside the active MCP scope")
	}
	return registry.backend.CallTool(ctx, exposedName, args)
}

func (a *Agent) resolveTurnCapability(ctx context.Context, out Output, activity CapabilityActivity) (string, *capabilityadvisor.Hint) {
	_, policy := a.modeContext()
	return a.resolveTurnCapabilityWithPolicy(ctx, out, activity, policy.AllowMCP)
}

func (a *Agent) resolveTurnCapabilityWithPolicy(ctx context.Context, out Output, activity CapabilityActivity, allowMCP bool) (string, *capabilityadvisor.Hint) {
	if a == nil || a.capabilityAdvisor == nil || !activity.NonTrivial || !allowMCP {
		// Without MCP authority the model cannot follow gateway playbooks; skip
		// host orientation so PLAN/ASK turns stay free of unusable tool advice.
		return "", nil
	}
	activity.ScopeID = strings.TrimSpace(activity.ScopeID)
	if activity.ScopeID == "" {
		return "", nil
	}

	a.syncCapabilityCatalogEpoch()
	if activity.CatalogRevision == "" {
		activity.CatalogRevision = a.capabilityCatalogRevisionSnapshot()
	}

	retryKey := capabilityActivityRetryKey(activity)
	a.mu.Lock()
	_, retry := a.capabilityRetries[retryKey]
	delete(a.capabilityRetries, retryKey)
	a.mu.Unlock()

	resolveCtx, cancel := context.WithTimeout(ctx, capabilityResolverTimeout)
	reconsidered := activity.Reconsider || retry
	result := a.capabilityAdvisor.Advise(resolveCtx, activity.request(retry || activity.Reconsider))
	cancel()
	status := normalizedCapabilityRouteStatus(result)
	a.recordCapabilityMetric(status)
	if result.CatalogRevision != "" {
		a.noteCapabilityCatalogRevision(result.CatalogRevision)
	}
	emitCapabilityRoute(out, capabilityRouteFromResult(activity, result, status, reconsidered))
	if retry && (status == capabilityadvisor.StatusUnavailable || status == capabilityadvisor.StatusInvalid) {
		// Preserve a failed-route refresh for a later turn when the resolver was
		// temporarily unavailable or returned an invalid bounded contract.
		a.mu.Lock()
		a.capabilityRetries[retryKey] = struct{}{}
		a.mu.Unlock()
	}
	if (status != capabilityadvisor.StatusResolved && status != capabilityadvisor.StatusAmbiguous) || result.Hint == nil {
		// No confident route: still emit playbook + optional stats so the model
		// knows the lazy gateway workflow for this phase.
		return a.enrichCapabilityAdvisory(ctx, activity, nil, "", allowMCP), nil
	}
	hint := result.Hint
	base := formatCapabilityHint(activity, *hint, allowMCP)
	// When the host can pre-fetch describe, avoid ordering the model to re-describe.
	if allowMCP && !hint.Ambiguous {
		registry := scopedCapabilityRegistry{agent: a, backend: a.registry}
		if _, ok := registry.ResolveToolName("mcphub_describe_tool"); ok {
			base = formatCapabilityHintWithOptions(activity, *hint, allowMCP, capabilityHintOptions{HostMayPrefetch: true})
		}
	}
	return a.enrichCapabilityAdvisory(ctx, activity, hint, base, allowMCP), hint
}

func normalizedCapabilityRouteStatus(result capabilityadvisor.Result) capabilityadvisor.Status {
	switch result.Status {
	case capabilityadvisor.StatusResolved:
		if result.Hint == nil {
			return capabilityadvisor.StatusInvalid
		}
		if result.Hint.Ambiguous {
			return capabilityadvisor.StatusAmbiguous
		}
		return capabilityadvisor.StatusResolved
	case capabilityadvisor.StatusAmbiguous:
		if result.Hint == nil || !result.Hint.Ambiguous {
			return capabilityadvisor.StatusInvalid
		}
		return capabilityadvisor.StatusAmbiguous
	case capabilityadvisor.StatusNoMatch, capabilityadvisor.StatusUnavailable, capabilityadvisor.StatusInvalid, capabilityadvisor.StatusSkipped:
		return result.Status
	default:
		return capabilityadvisor.StatusInvalid
	}
}

func capabilityRouteFromResult(activity CapabilityActivity, result capabilityadvisor.Result, status capabilityadvisor.Status, reconsidered bool) CapabilityRoute {
	freshness := CapabilityRouteFreshnessUnknown
	if result.Cached {
		freshness = CapabilityRouteCached
	} else if result.Attempted {
		freshness = CapabilityRouteFresh
	}
	route := CapabilityRoute{
		Phase: activity.Phase, Status: status, Freshness: freshness,
		CatalogRevision: result.CatalogRevision, Reconsidered: reconsidered,
	}
	if route.CatalogRevision == "" {
		route.CatalogRevision = activity.CatalogRevision
	}
	if result.Hint == nil {
		return route
	}
	switch status {
	case capabilityadvisor.StatusResolved:
		route.Server = result.Hint.Server
		route.Tool = result.Hint.Tool
		route.CandidateCount = 1
	case capabilityadvisor.StatusAmbiguous:
		route.CandidateCount = 1 + len(result.Hint.Alternatives)
	}
	return route
}

func emitCapabilityRoute(out Output, route CapabilityRoute) {
	if route.Status == capabilityadvisor.StatusSkipped {
		return
	}
	if routeOut, ok := out.(CapabilityRouteOutput); ok {
		routeOut.CapabilityRoute(route)
	}
}

func composeCapabilityContext(advisory, base string) string {
	advisory = strings.TrimSpace(advisory)
	if advisory == "" {
		return base
	}
	if base == "" {
		return advisory
	}
	return advisory + "\n\n" + base
}

func (a *Agent) formatCapabilityHint(activity CapabilityActivity, hint capabilityadvisor.Hint) string {
	_, policy := a.modeContext()
	return formatCapabilityHint(activity, hint, policy.AllowMCP)
}

type capabilityHintOptions struct {
	// HostMayPrefetch signals that the host will (or already can) call
	// mcphub_describe_tool for this route, so the advisory should not force a
	// redundant model-authored describe hop.
	HostMayPrefetch bool
}

func formatCapabilityHint(activity CapabilityActivity, hint capabilityadvisor.Hint, allowMCP bool) string {
	return formatCapabilityHintWithOptions(activity, hint, allowMCP, capabilityHintOptions{})
}

func formatCapabilityHintWithOptions(activity CapabilityActivity, hint capabilityadvisor.Hint, allowMCP bool, options capabilityHintOptions) string {
	var builder strings.Builder
	builder.WriteString("## Host capability advisory\n")
	builder.WriteString("This bounded MCPHub recommendation is advisory only. It is not a tool execution, result, success receipt, evidence, or additional authority.\n")
	if hint.Ambiguous {
		builder.WriteString("MCPHub reported an ambiguous route. No capability has been selected. Candidate identifiers: ")
		candidates := append([]string{hint.Namespaced}, hint.Alternatives...)
		builder.WriteString(strings.Join(candidates, ", "))
		appendKnownAmbiguousCapabilityContracts(&builder, activity, hint)
		if allowMCP {
			builder.WriteString(". Use the visible mcphub_search_tools resolver companion before choosing a downstream tool.\n")
		} else {
			builder.WriteString(". The current turn policy does not expose MCP downstream tools, so do not treat any candidate as executable in this turn.\n")
		}
	} else {
		fmt.Fprintf(&builder, "MCPHub recommends %s (server %s, tool %s).\n", hint.Namespaced, hint.Server, hint.Tool)
	}
	if len(hint.RequiredFields) > 0 {
		fmt.Fprintf(&builder, "Required argument fields: %s.\n", strings.Join(hint.RequiredFields, ", "))
	}
	if soft := softFillWorkspaceArgs(hint); soft != "" {
		builder.WriteString(soft)
		builder.WriteByte('\n')
	}
	if !hint.Ambiguous && allowMCP {
		if options.HostMayPrefetch {
			if hint.NeedsDescription() {
				builder.WriteString("The resolver's argument summary was truncated; prefer the host pre-fetched contract below when present. ")
			} else {
				builder.WriteString("Prefer the host pre-fetched contract below when present. ")
			}
			builder.WriteString("Only call mcphub_describe_tool again if that contract is missing or incomplete. Required fields are a routing summary and may omit mutually exclusive inputs.\n")
		} else {
			if hint.NeedsDescription() {
				builder.WriteString("The resolver's argument summary was truncated. ")
			}
			builder.WriteString("Call the visible mcphub_describe_tool before constructing arguments. Required fields are only a routing summary and may omit runtime relationships such as mutually exclusive inputs.\n")
		}
	}
	appendKnownCapabilityContract(&builder, activity, hint)
	if !hint.Ambiguous && allowMCP {
		builder.WriteString("After inspecting the bounded contract metadata, if this route still matches the work, fill the arguments and invoke it through the visible mcphub_call_tool gateway. The downstream call must pass through normal scope, privacy, approval, and execution-ledger policy.\n")
	} else if !hint.Ambiguous {
		builder.WriteString("The current turn policy does not expose MCP downstream tools. Keep this as routing advice only and do not claim the recommendation was executed.\n")
	}
	return strings.TrimSpace(builder.String())
}

// appendKnownCapabilityContract adds host-owned composition facts for exact,
// versioned downstream surfaces. Resolver metadata remains advisory and cannot
// invent persistence. These notes describe only what sonar can safely
// conclude before a call; they never mark an operation successful.
func appendKnownCapabilityContract(builder *strings.Builder, activity CapabilityActivity, hint capabilityadvisor.Hint) {
	if builder == nil || hint.Ambiguous {
		return
	}
	server := strings.ToLower(hint.Server)
	tool := strings.ToLower(hint.Tool)
	switch server {
	case "hitspec", "hitspec_web":
		switch {
		case isHitspecCapabilityTool(tool, "fetch"):
			builder.WriteString("Known Hitspec fetch contract: hitspec_fetch returns bounded content inline and does not create a workspace file or durable artifact.\n")
			if isDurableCapabilityOutcome(activity.DesiredOutcome) {
				builder.WriteString("To satisfy this durable outcome, review the inline result, write the accepted content to a workspace file through a separately authorized host action, then describe and call fcheap_save separately. Fetch, file write, and artifact save are distinct effect and approval boundaries.\n")
			}
		case isHitspecCapabilityTool(tool, "capture_webpage"):
			builder.WriteString("Known Hitspec v2.18 capture contract: when this optional tool is exposed, it persists rendered Markdown as a durable file.cheap stash and returns a compact artifact receipt rather than the page body. Indexing is requested and reported separately.\n")
		case isHitspecCapabilityTool(tool, "search_web"):
			builder.WriteString("Known Hitspec search contract: hitspec_search_web returns non-persisted discovery candidates, not verified evidence.\n")
		}
	case "bob":
		switch tool {
		case "bob_context", "bob_plan", "bob_path", "bob_playbook", "bob_inspect", "bob_check":
			builder.WriteString("Known Bob contract: Bob reports repository/contract state only (EvidenceNone). Clean/plan success is not application verification.\n")
		}
	case "cortex":
		switch tool {
		case "cortex_status", "cortex_investigate", "cortex_plan", "cortex_verify", "cortex_open_task", "cortex_handoff", "cortex_remember":
			builder.WriteString("Known Cortex contract: use for evidence-guided orientation and criteria; sonar still owns approval, budgets, and tool dispatch.\n")
		}
	case "codemap":
		builder.WriteString("Known Codemap contract: structural graph/impact support; treat stale index signals as non-negative absence of proof.\n")
	case "vecgrep":
		builder.WriteString("Known Vecgrep contract: semantic discovery candidates only — open exact files with native tools before editing.\n")
	case "fcheap":
		builder.WriteString("Known file.cheap contract: artifact storage preserves bytes; it does not create truth by itself.\n")
	case "minerva":
		builder.WriteString("Known Minerva contract: skills/profiles/stack analytics for self-improvement after the primary task is grounded.\n")
	case "monitor":
		builder.WriteString("Known Monitor contract: operational process/resource support, not application verification evidence.\n")
	case "obsidian":
		builder.WriteString("Known Obsidian contract: vault note operations remain approval-gated and workspace/privacy scoped.\n")
	}
}

// appendKnownAmbiguousCapabilityContracts keeps MCPHub's ambiguity intact while
// supplying exact host-owned distinctions for the versioned Hitspec web
// surfaces. This helps the model compare task fit without turning a candidate
// into a selected route or granting any execution authority.
func appendKnownAmbiguousCapabilityContracts(builder *strings.Builder, activity CapabilityActivity, hint capabilityadvisor.Hint) {
	if builder == nil || !hint.Ambiguous {
		return
	}
	candidates := append([]string{hint.Namespaced}, hint.Alternatives...)
	hasCandidate := func(target string) bool {
		for _, candidate := range candidates {
			clean := strings.TrimPrefix(target, "hitspec_")
			if strings.EqualFold(candidate, "hitspec__"+target) ||
				strings.EqualFold(candidate, "hitspec__"+clean) {
				return true
			}
		}
		return false
	}
	if !hasCandidate("hitspec_capture_webpage") && !hasCandidate("hitspec_fetch") && !hasCandidate("hitspec_search_web") {
		return
	}
	builder.WriteString(". Host-known Hitspec contract distinctions: ")
	parts := make([]string, 0, 3)
	if hasCandidate("hitspec_capture_webpage") {
		parts = append(parts, "hitspec_capture_webpage persists rendered Markdown as a durable file.cheap stash, returns a compact artifact receipt, and reports indexing separately")
	}
	if hasCandidate("hitspec_fetch") {
		parts = append(parts, "hitspec_fetch returns bounded content inline and does not persist it")
	}
	if hasCandidate("hitspec_search_web") {
		parts = append(parts, "hitspec_search_web returns non-persisted discovery candidates, not verified evidence")
	}
	builder.WriteString(strings.Join(parts, "; "))
	if isDurableCapabilityOutcome(activity.DesiredOutcome) {
		builder.WriteString(". The requested outcome is durable, but MCPHub's route remains ambiguous until the candidate contracts are compared")
	}
}

func isHitspecCapabilityTool(tool, public string) bool {
	return tool == public || tool == "hitspec_"+public
}

func isDurableCapabilityOutcome(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "durable") || strings.Contains(value, "artifact") || strings.Contains(value, "artefacto")
}

func (a *Agent) markCapabilityRouteFailed(activity CapabilityActivity, callName string, args map[string]any, hint *capabilityadvisor.Hint) bool {
	if a == nil || hint == nil || hint.Ambiguous || strings.TrimSpace(activity.ScopeID) == "" {
		return false
	}
	server, tool, ok := exactLazyMCPHubTarget(args)
	if !ok || !strings.HasSuffix(callName, "__mcphub_call_tool") || server != hint.Server || tool != hint.Tool {
		return false
	}
	a.mu.Lock()
	a.capabilityRetries[capabilityActivityRetryKey(activity)] = struct{}{}
	a.capabilityMetrics.RouteFailedRetry++
	a.mu.Unlock()
	return true
}

func capabilityActivityRetryKey(activity CapabilityActivity) capabilityRetryKey {
	values := []string{
		activity.ScopeID, activity.Objective, activity.Phase,
		activity.CurrentActivity, activity.DesiredOutcome,
	}
	kinds := append([]string(nil), activity.AvailableInputKinds...)
	sort.Strings(kinds)
	values = append(values, kinds...)
	intentTags := append([]string(nil), activity.IntentTags...)
	sort.Strings(intentTags)
	values = append(values, intentTags...)
	for index := range values {
		values[index] = strings.ToLower(strings.Join(strings.Fields(values[index]), " "))
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(strings.Join(values, "\x00")))
	_, _ = hash.Write(activity.cacheDiscriminator[:])
	var key capabilityRetryKey
	copy(key[:], hash.Sum(nil))
	return key
}

func capabilityRouteOutcomeFailed(projection ecosystem.ToolProjection, toolError bool) bool {
	if toolError || projection.Transport == ecosystem.TransportFailed {
		return true
	}
	switch projection.Domain {
	case ecosystem.DomainFailed, ecosystem.DomainBlocked, ecosystem.DomainConflict, ecosystem.DomainDrift:
		return true
	default:
		return false
	}
}

func capabilityPromptObjective(prompt string) (string, bool) {
	value, ok := normalizedCapabilityPrompt(prompt)
	if !ok {
		return "", false
	}
	return boundCapabilityText(value, maxCapabilityObjective), true
}

// normalizedCapabilityPrompt validates private task wording for local
// classification and fingerprinting. Callers must not put its return value in
// resolver queries, durable state, UI events, or transcripts authored by the
// host. Only the bounded semantic projection crosses the MCPHub boundary.
func normalizedCapabilityPrompt(prompt string) (string, bool) {
	if !utf8.ValidString(prompt) || strings.Contains(prompt, "```") {
		return "", false
	}
	value := strings.Join(strings.Fields(strings.TrimSpace(prompt)), " ")
	if value == "" || looksLikeRawJSON(value) || containsCredentialValue(value) {
		return "", false
	}
	return value, true
}

// projectCapabilityActivity converts private user wording into a small,
// host-authored semantic description. The original prompt is used only for
// local classification and never crosses the resolver boundary.
func projectCapabilityActivity(signal, phase string, inputKinds []string) (objective, current, outcome string, nonTrivial bool) {
	lower := " " + strings.ToLower(signal)
	hasInput := func(kind string) bool {
		for _, candidate := range inputKinds {
			if candidate == kind {
				return true
			}
		}
		return false
	}

	switch {
	case asksToReconsiderCapabilities(signal):
		return "Reconsider the capability route for the current task",
			"Ask MCPHub to reconsider available capabilities",
			"A fresh bounded capability recommendation", true
	case hasInput("url") && containsAnyWord(lower,
		" capture", " fetch", " download", " preserve", " markdown",
		" capturar", " descargar", " guardar", " conservar"):
		return "Preserve a public webpage as readable Markdown",
			"Capture and normalize content from a public URL",
			"A readable durable Markdown artifact", true
	case hasInput("url"):
		return "Use a public URL for a non-trivial task",
			"Inspect or retrieve content from a public URL",
			"Verifiable information derived from the URL", true
	case hasInput("video"):
		return "Analyze an external video input",
			"Inspect bounded video evidence without exposing its file path",
			"A verifiable finding derived from the video", true
	case hasInput("image"):
		return "Analyze an external image input",
			"Inspect bounded image evidence without exposing its file path",
			"A verifiable finding derived from the image", true
	case hasInput("audio"):
		return "Analyze an external audio input",
			"Inspect bounded audio evidence without exposing its file path",
			"A verifiable finding derived from the audio", true
	case hasInput("document"):
		return "Analyze an external document input",
			"Inspect bounded document evidence without exposing its file path",
			"A verifiable finding derived from the document", true
	case hasInput("artifact_id") || containsAnyWord(lower,
		" stored artifact", " saved artifact", " artefacto guardado", " artifact search"):
		return "Find a previously stored artifact",
			"Locate durable data by its artifact metadata",
			"A bounded reference to the stored artifact", true
	case containsAnyWord(lower,
		" web search", " search online", " search the web", " current information", " latest information",
		" búsqueda web", " buscar en línea", " buscar online", " información actual"):
		return "Search the live public web for current information",
			"Discover relevant public web sources",
			"A bounded set of candidate public sources", true
	case hasInput("database") && hasInput("cli"):
		return "Investigate code database and command-line evidence together",
			"Correlate evidence across multiple project sources",
			"A source-backed diagnosis", true
	case phase == "planning" && nonTrivialCapabilityPrompt(signal, inputKinds):
		return "Plan a repository feature before implementation",
			"Design repository changes before editing",
			"A verifiable implementation plan", true
	case phase == "verification" && nonTrivialCapabilityPrompt(signal, inputKinds):
		return "Verify a repository change",
			"Check the requested behavior against available evidence",
			"A reproducible verification result", true
	case phase == "research" && nonTrivialCapabilityPrompt(signal, inputKinds):
		return "Investigate the available project evidence",
			"Research the requested issue using available inputs",
			"A source-backed finding", true
	case phase == "implementation" && nonTrivialCapabilityPrompt(signal, inputKinds):
		return "Implement a repository change",
			"Make the requested workspace change",
			"A verified implementation", true
	case nonTrivialCapabilityPrompt(signal, inputKinds):
		return "Complete a non-trivial workspace task",
			"Work on the requested task using available inputs",
			"A verifiable task result", true
	default:
		return "", "", "", false
	}
}

func looksLikeRawJSON(value string) bool {
	return strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")
}

func containsCredentialValue(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "-----begin ") || strings.Contains(lower, "sk-") || strings.Contains(lower, "ghp_") {
		return true
	}
	for _, name := range []string{"password", "passwd", "secret", "api key", "api_key", "access token", "access_token", "refresh token", "authorization", "cookie", "credential"} {
		for _, separator := range []string{"=", ":", " is ", " es "} {
			if strings.Contains(lower, name+separator) {
				return true
			}
		}
	}
	return false
}

func capabilityPhase(objective, fallback string) string {
	lower := strings.ToLower(objective)
	tests := []struct {
		phase string
		terms []string
	}{
		{phase: "verification", terms: []string{" verify", "validate", " test", "probar", "verificar", "validar"}},
		{phase: "planning", terms: []string{" plan", "design", "architecture", "planear", "diseñar", "arquitectura"}},
		{phase: "research", terms: []string{"research", "investigat", "search", "explore", "inspect", "fetch", "capture", "buscar", "investigar", "explorar", "revisar", "descargar", "capturar"}},
		{phase: "implementation", terms: []string{"implement", "build", "fix", "edit", "create", "update", "install", "configure", "implementar", "construir", "arreglar", "editar", "crear", "actualizar", "instalar", "configurar"}},
	}
	padded := " " + lower
	for _, test := range tests {
		for _, term := range test.terms {
			if strings.Contains(padded, term) {
				return test.phase
			}
		}
	}
	fallback = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(fallback)), "_"))
	if fallback == "" {
		return "working"
	}
	return boundCapabilityText(fallback, 48)
}

func normalizeHostCapabilityPhase(phase string) string {
	phase = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(phase)), "_"))
	if phase == "" {
		return "working"
	}
	return boundCapabilityText(phase, 48)
}

func capabilityMaterialFingerprint(value string) [sha256.Size]byte {
	var normalized strings.Builder
	space := true
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			normalized.WriteRune(r)
			space = false
			continue
		}
		if !space {
			normalized.WriteByte(' ')
			space = true
		}
	}
	return sha256.Sum256([]byte(strings.TrimSpace(normalized.String())))
}

func capabilityPhaseForAuthority(mode AuthorityMode) string {
	switch mode {
	case AuthorityPlan:
		return "planning"
	case AuthorityAutoScoped:
		return "implementation"
	default:
		return "working"
	}
}

func capabilityInputKinds(objective string, workspaceAvailable bool) []string {
	lower := strings.ToLower(objective)
	set := make(map[string]struct{})
	if workspaceAvailable {
		set["workspace"] = struct{}{}
	}
	if strings.Contains(lower, "https://") || strings.Contains(lower, "http://") || strings.Contains(lower, "www.") {
		set["url"] = struct{}{}
	}
	if containsAnyWord(lower, "database", "postgres", "mysql", "sqlite", "mongodb", "base de datos") {
		set["database"] = struct{}{}
	}
	if containsAnyWord(lower, " cli", "command", "terminal", "shell", "comando", "terminal") {
		set["cli"] = struct{}{}
	}
	if containsAnyWord(lower, "artifact id", "artifact_id", "stash id", "stash_id", "artefacto") {
		set["artifact_id"] = struct{}{}
	}
	for _, kind := range externalFileInputKinds(objective) {
		set["external_file"] = struct{}{}
		set[kind] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// capabilityIntentTags converts private task wording into a closed vocabulary
// aligned with MCPHub's public use_when language. No prompt fragment is copied:
// every emitted value is a compile-time label and the resolver validates the
// labels again before including them in its bounded query.
func capabilityIntentTags(objective, phase string) []string {
	lower := " " + strings.ToLower(objective)
	set := make(map[string]struct{})
	add := func(values ...string) {
		for _, value := range values {
			set[value] = struct{}{}
		}
	}

	if containsAnyWord(lower,
		" symbol", " reference", " call graph", " blast radius", " dependency graph", " code structure",
		" símbolo", " referencia", " grafo de llamadas", " impacto", " estructura del código") {
		add("code", "symbols", "references", "structure")
	}
	if containsAnyWord(lower,
		" semantic search", "find code by meaning", "related code", "unknown symbol", " búsqueda semántica", "código relacionado") {
		add("code", "semantic", "meaning", "search")
	}
	if containsAnyWord(lower,
		" authentication", " authorization", " auth ", "middleware auth", " login", " seguridad", " autenticación", " autorización") {
		add("code", "security", "symbols", "references")
	}
	if containsAnyWord(lower,
		" logging", " log ", " logs", " telemetry", " tracing", " observability", " auditoría", " telemetría", " trazas") {
		add("observability", "audit", "telemetry")
	}
	if containsAnyWord(lower,
		" process", " port", " cpu", " memory usage", " machine health", " resource usage",
		" proceso", " puerto", " memoria", " salud de la máquina", " recursos") {
		add("processes", "ports", "resources", "health")
	}
	if containsAnyWord(lower,
		" incident", " service dependency", " audit trail", " service discovery", " dependency incident",
		" incidente", " dependencia de servicio", " rastro de auditoría") {
		add("services", "dependencies", "incidents", "audit")
	}
	if containsAnyWord(lower,
		" browser", " web ui", " webpage ui", " end-to-end", " e2e", " navegador", " interfaz web") {
		add("browser", "web_ui", "verification")
	}
	if containsAnyWord(lower,
		" terminal", " tui", " command line", " cli ", " consola", " línea de comandos") {
		add("terminal", "cli", "verification")
	}
	if containsAnyWord(lower,
		" http request", " api request", " .http", " hitspec", " petición http", " solicitud http") {
		add("http", "requests", "specification")
	}
	if containsAnyWord(lower,
		" stored artifact", " saved artifact", " artifact search", " artifact_id", " stash_id", " artefacto guardado") {
		add("artifact", "storage", "search")
	}
	if containsAnyWord(lower, ".mp4", ".mov", ".mkv", ".webm", ".avi") {
		add("video", "evidence")
	}
	if containsAnyWord(lower, ".png", ".jpg", ".jpeg", ".webp", ".heic") {
		add("image", "evidence")
	}
	if phase == "planning" {
		add("repository", "planning", "feature")
	}
	if containsAnyWord(lower,
		" skill", " skills", " profile", " profiles", "system prompt", "self-improvement", "self improvement",
		" habilidad", " habilidades", " perfil", " perfiles", " auto-mejora") {
		add("skills", "profiles", "self_improvement")
	}
	if containsAnyWord(lower,
		" note", " notes", " vault", "obsidian", " nota", " notas", " knowledge base", "base de conocimiento") {
		add("notes", "knowledge", "vault")
	}
	if containsAnyWord(lower,
		" codemap", " call graph", "impact analysis", "symbol graph", " grafo", "mapa de código") {
		add("code", "symbols", "structure", "references")
	}
	if containsAnyWord(lower,
		" vecgrep", "embedding search", "hybrid search", "vector search", " búsqueda vectorial") {
		add("code", "semantic", "meaning", "search")
	}

	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	if len(result) > 16 {
		result = result[:16]
	}
	return result
}

func externalFileInputKinds(value string) []string {
	sets := map[string]map[string]struct{}{
		"image": {
			".bmp": {}, ".gif": {}, ".heic": {}, ".heif": {}, ".jpeg": {}, ".jpg": {},
			".png": {}, ".svg": {}, ".tif": {}, ".tiff": {}, ".webp": {},
		},
		"video": {
			".avi": {}, ".m4v": {}, ".mkv": {}, ".mov": {}, ".mp4": {}, ".mpeg": {},
			".mpg": {}, ".webm": {},
		},
		"audio": {
			".aac": {}, ".flac": {}, ".m4a": {}, ".mp3": {}, ".ogg": {}, ".opus": {}, ".wav": {},
		},
		"document": {
			".csv": {}, ".doc": {}, ".docx": {}, ".epub": {}, ".odt": {}, ".pdf": {},
			".ppt": {}, ".pptx": {}, ".rtf": {}, ".txt": {}, ".xls": {}, ".xlsx": {},
		},
	}
	found := make(map[string]struct{})
	for _, token := range strings.Fields(value) {
		candidate := strings.Trim(token, "\"'`()[]{}<>,;:!?.")
		lower := strings.ToLower(candidate)
		if lower == "" || strings.Contains(lower, "://") {
			continue
		}
		extension := strings.ToLower(filepath.Ext(lower))
		for kind, extensions := range sets {
			if _, ok := extensions[extension]; ok {
				found[kind] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(found))
	for kind := range found {
		result = append(result, kind)
	}
	sort.Strings(result)
	return result
}

func nonTrivialCapabilityPrompt(objective string, inputKinds []string) bool {
	if len(inputKinds) > 1 {
		return true
	}
	lower := " " + strings.ToLower(objective)
	return containsAnyWord(lower,
		" implement", " build", " fix", " analyze", " research", " search", " fetch", " capture", " inspect", " read", " plan", " design", " test", " verify", " install", " configure", " diagnose", " create", " update", " review",
		" implementar", " construir", " arreglar", " analizar", " investigar", " buscar", " descargar", " capturar", " revisar", " leer", " planear", " diseñar", " probar", " verificar", " instalar", " configurar", " diagnosticar", " crear", " actualizar")
}

func asksToReconsiderCapabilities(objective string) bool {
	lower := strings.ToLower(objective)
	return (strings.Contains(lower, "reconsider") || strings.Contains(lower, "reconsidera")) &&
		(strings.Contains(lower, "capabilit") || strings.Contains(lower, "tool") || strings.Contains(lower, "herramient"))
}

func containsAnyWord(value string, terms ...string) bool {
	for _, term := range terms {
		if strings.Contains(value, term) {
			return true
		}
	}
	return false
}

func boundCapabilityText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit - len("…")
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut]) + "…"
}

func capabilityScopeID(sessionID int64, turnID string) string {
	if sessionID > 0 {
		return fmt.Sprintf("session_%d", sessionID)
	}
	var builder strings.Builder
	for _, r := range strings.TrimSpace(turnID) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-' {
			builder.WriteRune(r)
		}
	}
	if builder.Len() == 0 {
		return "turn"
	}
	return "turn_" + builder.String()
}
