package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

const (
	bobWorkspaceManifest        = "bob.yaml"
	maxBobStoredAdmissions      = 4
	bobWorkspaceBootstrapSource = "bob_workspace_bootstrap"
)

// BobWorkspaceContextState is the complete bounded UI projection for the
// current Agent lifetime. Digest is nil when a newer generation invalidated
// the previous context. It never contains the workspace or raw Bob content.
type BobWorkspaceContextState struct {
	Generation uint64
	Digest     *ecosystem.ReceiptDigest
}

// BobWorkspaceContextOutput is optional. Headless callers do not need to
// implement it; interactive hosts receive only the bounded semantic digest.
type BobWorkspaceContextOutput interface {
	BobWorkspaceContext(BobWorkspaceContextState)
}

type bobWorkspaceCandidate struct {
	workspace         string
	rootInfo          os.FileInfo
	manifestInfo      os.FileInfo
	filesystemVersion uint64
}

type bobWorkspaceContextCache struct {
	candidate bobWorkspaceCandidate
	domain    ecosystem.DomainState
	digest    ecosystem.ReceiptDigest
}

type bobBootstrapPlan struct {
	candidate      bobWorkspaceCandidate
	call           llm.ToolCall
	definition     llm.ToolDef
	schemaDigest   string
	behaviorDigest string
}

type bobBootstrapClaim struct {
	candidate  bobWorkspaceCandidate
	generation uint64
}

type bobContextAdmission struct {
	candidate  bobWorkspaceCandidate
	generation uint64
	valid      bool
}

func (a *Agent) probeBobWorkspaceCandidate() (bobWorkspaceCandidate, bool) {
	if a == nil {
		return bobWorkspaceCandidate{}, false
	}
	workspace, err := a.openWorkspaceRoot()
	if err != nil {
		return bobWorkspaceCandidate{}, false
	}
	defer func() { _ = workspace.Close() }()
	info, err := workspace.root.Lstat(bobWorkspaceManifest)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || workspace.validate() != nil {
		return bobWorkspaceCandidate{}, false
	}
	a.mu.RLock()
	version := a.filesystemVersion
	a.mu.RUnlock()
	return bobWorkspaceCandidate{
		workspace: workspace.path, rootInfo: workspace.info, manifestInfo: info, filesystemVersion: version,
	}, true
}

func bobWorkspaceCandidatesEqual(left, right bobWorkspaceCandidate) bool {
	return left.workspace != "" && left.workspace == right.workspace &&
		left.filesystemVersion == right.filesystemVersion && left.rootInfo != nil && right.rootInfo != nil &&
		left.manifestInfo != nil && right.manifestInfo != nil &&
		os.SameFile(left.rootInfo, right.rootInfo) && os.SameFile(left.manifestInfo, right.manifestInfo) &&
		left.manifestInfo.Mode() == right.manifestInfo.Mode() && left.manifestInfo.Size() == right.manifestInfo.Size() &&
		left.manifestInfo.ModTime().Equal(right.manifestInfo.ModTime())
}

func (a *Agent) bobWorkspaceCandidateCurrent(candidate bobWorkspaceCandidate) bool {
	current, ok := a.probeBobWorkspaceCandidate()
	return ok && bobWorkspaceCandidatesEqual(candidate, current)
}

func cloneBobDigest(source *ecosystem.ReceiptDigest) *ecosystem.ReceiptDigest {
	if source == nil {
		return nil
	}
	clone := *source
	clone.Items = append([]string(nil), source.Items...)
	clone.Required = append([]string(nil), source.Required...)
	return &clone
}

func (a *Agent) bobWorkspaceStateLocked() BobWorkspaceContextState {
	state := BobWorkspaceContextState{Generation: a.bobWorkspaceGeneration}
	if a.bobWorkspaceContext != nil {
		state.Digest = cloneBobDigest(&a.bobWorkspaceContext.digest)
	}
	return state
}

func emitBobWorkspaceContext(out Output, state BobWorkspaceContextState) {
	if consumer, ok := out.(BobWorkspaceContextOutput); ok {
		consumer.BobWorkspaceContext(state)
	}
}

func (a *Agent) advanceBobWorkspaceGenerationLocked() {
	a.bobWorkspaceGeneration++
	if a.bobWorkspaceGeneration == 0 {
		a.bobWorkspaceGeneration = 1
	}
}

func (a *Agent) invalidateBobWorkspaceContextLocked() {
	a.bobWorkspaceContext = nil
	a.bobStoredAdmissions = nil
	a.advanceBobWorkspaceGenerationLocked()
}

func (a *Agent) invalidateBobWorkspaceContext(out Output) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.invalidateBobWorkspaceContextLocked()
	state := a.bobWorkspaceStateLocked()
	a.mu.Unlock()
	emitBobWorkspaceContext(out, state)
}

// reconcileBobWorkspaceContext re-probes the root marker without reading it.
// A marker is only a candidate; cached semantic state still requires an exact
// typed Bob receipt bound to the same pinned file identity.
func (a *Agent) reconcileBobWorkspaceContext(out Output) (bobWorkspaceCandidate, bool) {
	candidate, present := a.probeBobWorkspaceCandidate()
	a.mu.Lock()
	if !present {
		if a.bobWorkspaceContext != nil {
			a.invalidateBobWorkspaceContextLocked()
		}
		state := a.bobWorkspaceStateLocked()
		a.mu.Unlock()
		// Always publish the current generation. Several legitimate session and
		// workspace boundaries invalidate the Agent cache without owning an
		// Output; the next turn must still clear a host that retained the prior
		// card.
		emitBobWorkspaceContext(out, state)
		return bobWorkspaceCandidate{}, false
	}
	if a.bobWorkspaceContext != nil && !bobWorkspaceCandidatesEqual(a.bobWorkspaceContext.candidate, candidate) {
		a.invalidateBobWorkspaceContextLocked()
	}
	cached := a.bobWorkspaceContext != nil
	state := a.bobWorkspaceStateLocked()
	a.mu.Unlock()
	emitBobWorkspaceContext(out, state)
	return candidate, cached
}

func (a *Agent) bobWorkspaceContextPrompt() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	cache := a.bobWorkspaceContext
	if cache == nil {
		a.mu.RUnlock()
		return ""
	}
	projection := ecosystem.ToolProjection{
		Specialist: "bob", Operation: "bob_context", Role: ecosystem.RoleBuild,
		Transport: ecosystem.TransportSucceeded, Domain: cache.domain, DomainTyped: true,
		Evidence: ecosystem.EvidenceNone, Digest: cloneBobDigest(&cache.digest),
	}.Normalize()
	a.mu.RUnlock()
	if projection.Digest == nil {
		return ""
	}
	return "Bob repository context (validated bounded contract state; convergence only, never behavioral verification):\n" +
		ecosystem.SafeReceiptText(projection)
}

func (a *Agent) planBobWorkspaceBootstrap(candidate bobWorkspaceCandidate, snapshot mcp.ToolSnapshot) (bobBootstrapPlan, bool) {
	if a == nil || a.registry == nil || candidate.workspace == "" || snapshot.Epoch == 0 ||
		a.registry.SnapshotTools().Epoch != snapshot.Epoch || !a.bobWorkspaceCandidateCurrent(candidate) {
		return bobBootstrapPlan{}, false
	}
	arguments := map[string]any{"workspace": candidate.workspace, "profile": "compact"}
	direct := make([]llm.ToolDef, 0, 1)
	pinned := make([]llm.ToolDef, 0, 1)
	for _, definition := range snapshot.Tools {
		parts := strings.Split(definition.Name, "__")
		if len(parts) < 2 || !snapshot.ServerAvailable(parts[0]) || !a.allowsMCPTool(definition.Name) {
			continue
		}
		call := llm.ToolCall{Name: definition.Name, Arguments: arguments}
		contract, trusted := a.trustedMCPContract(call)
		if !trusted || !contract.auto || contract.effect != executionpkg.EffectReadOnly ||
			a.authorityPermissionDeniedForCall(call) || !autoContinuationBehaviorEligible(definition.Behavior) {
			continue
		}
		if _, err := validateContinuationArguments(definition, arguments, nil); err != nil {
			continue
		}
		switch {
		case len(parts) == 2:
			server, ok := a.trustedMCPServer(parts[0])
			if ok && server.gateway == "" && server.localOwner == "bob" && parts[1] == "bob_context" {
				direct = append(direct, definition)
			}
		case len(parts) == 3:
			server, ok := a.trustedMCPServer(parts[0])
			if ok && server.gateway == config.MCPTrustGatewayMCPHub && parts[1] == "bob" && parts[2] == "bob_context" {
				pinned = append(pinned, definition)
			}
		}
	}
	var definition llm.ToolDef
	switch {
	case len(direct) == 1:
		definition = direct[0]
	case len(direct) > 1:
		return bobBootstrapPlan{}, false
	case len(pinned) == 1:
		definition = pinned[0]
	default:
		return bobBootstrapPlan{}, false
	}
	schemaDigest, ok := continuationSchemaDigest(definition)
	if !ok {
		return bobBootstrapPlan{}, false
	}
	return bobBootstrapPlan{
		candidate: candidate, call: llm.ToolCall{Name: definition.Name, Arguments: cloneApprovalArguments(arguments)},
		definition: definition, schemaDigest: schemaDigest, behaviorDigest: autoContinuationBehaviorDigest(definition),
	}, true
}

func bobWorkspaceBootstrapHint(plan bobBootstrapPlan) string {
	if plan.call.Name == "" || plan.candidate.workspace == "" {
		return ""
	}
	return fmt.Sprintf(
		"A root bob.yaml marks this workspace as a Bob contract candidate; the filename is not proof of validity. Use the exact registered read-only tool %s with workspace=%s and profile=compact to obtain validated bounded repository context.",
		strconv.Quote(plan.call.Name), strconv.Quote(plan.candidate.workspace),
	)
}

func (a *Agent) prepareBobWorkspaceBootstrap(
	plan bobBootstrapPlan,
	snapshot mcp.ToolSnapshot,
	mode AuthorityMode,
	state *autoContinuationState,
) *preparedAutoContinuation {
	if a == nil || state == nil || !a.bobWorkspaceCandidateCurrent(plan.candidate) {
		return nil
	}
	a.mu.Lock()
	if a.bobWorkspaceContext != nil {
		a.mu.Unlock()
		return nil
	}
	generation := a.bobWorkspaceGeneration
	a.mu.Unlock()
	argumentHash, err := executionpkg.HashCanonicalArguments(plan.call.Arguments)
	if err != nil {
		return nil
	}
	contextDigest := executionpkg.HashText(strings.Join([]string{
		plan.candidate.workspace, fmt.Sprint(plan.candidate.manifestInfo.Size()),
		plan.candidate.manifestInfo.ModTime().UTC().Format("20060102T150405.000000000Z07:00"), fmt.Sprint(generation),
	}, "\x00"))
	continuation := &ValidatedContinuation{
		Source: "local_agent", SourceOperation: bobWorkspaceBootstrapSource, Tool: "bob_context",
		Call: cloneContinuationToolCall(plan.call), ReasonCode: "workspace_context", Effect: executionpkg.EffectReadOnly,
		SourceDomain: ecosystem.DomainSucceeded, ContextDigest: contextDigest,
		SchemaDigest: plan.schemaDigest, BehaviorDigest: plan.behaviorDigest, WorkspaceRef: plan.candidate.workspace,
		hostBootstrap: &bobBootstrapClaim{candidate: plan.candidate, generation: generation},
	}
	continuation.Fingerprint = executionpkg.HashText(strings.Join([]string{
		continuation.Source, continuation.SourceOperation, continuation.ContextDigest,
		continuation.Call.Name, argumentHash, continuation.SchemaDigest, continuation.BehaviorDigest,
	}, "\x00"))
	freshness, ok := a.observeContinuationFreshness(continuation)
	if !ok {
		return nil
	}
	return a.prepareAndReserveAutoContinuation(continuation, snapshot, mode, freshness, state)
}

func (a *Agent) bobBootstrapClaimCurrent(claim *bobBootstrapClaim) bool {
	if a == nil || claim == nil || !a.bobWorkspaceCandidateCurrent(claim.candidate) {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.bobWorkspaceContext == nil && claim.generation != 0 && claim.generation == a.bobWorkspaceGeneration
}

func bobContextCallArguments(call llm.ToolCall) (map[string]any, bool) {
	parts := strings.Split(call.Name, "__")
	switch {
	case len(parts) == 2 && parts[1] == "bob_context":
		return call.Arguments, true
	case len(parts) == 3 && parts[1] == "bob" && parts[2] == "bob_context":
		return call.Arguments, true
	default:
		return nil, false
	}
}

func (a *Agent) captureBobContextAdmission(call llm.ToolCall) bobContextAdmission {
	arguments, exact := bobContextCallArguments(call)
	contract, trusted := a.trustedMCPContract(call)
	workspace, workspaceOK := arguments["workspace"].(string)
	profile, profileOK := arguments["profile"].(string)
	profileSupported := profile == "compact" || profile == "standard" || profile == "full"
	snapshot := a.mcpToolSnapshot()
	definition, definitionOK := exactAutoContinuationDefinition(snapshot, call.Name)
	parts := strings.Split(call.Name, "__")
	exactBobRoute := false
	if len(parts) == 2 {
		server, ok := a.trustedMCPServer(parts[0])
		exactBobRoute = ok && server.gateway == "" && server.localOwner == "bob" && parts[1] == "bob_context"
	} else if len(parts) == 3 {
		server, ok := a.trustedMCPServer(parts[0])
		exactBobRoute = ok && server.gateway == config.MCPTrustGatewayMCPHub && parts[1] == "bob" && parts[2] == "bob_context"
	}
	if !exact || !exactBobRoute || !trusted || contract.effect != executionpkg.EffectReadOnly ||
		!definitionOK || !snapshot.ServerAvailable(parts[0]) || !autoContinuationBehaviorEligible(definition.Behavior) ||
		!workspaceOK || !profileOK || !profileSupported {
		return bobContextAdmission{}
	}
	if state, err := validateContinuationArguments(definition, arguments, nil); err != nil || state != continuationArgumentsReady {
		return bobContextAdmission{}
	}
	candidate, present := a.probeBobWorkspaceCandidate()
	if !present || workspace != candidate.workspace {
		return bobContextAdmission{}
	}
	a.mu.RLock()
	generation := a.bobWorkspaceGeneration
	a.mu.RUnlock()
	return bobContextAdmission{candidate: candidate, generation: generation, valid: true}
}

func (a *Agent) resolveBobContextAdmission(
	captured bobContextAdmission,
	projection ecosystem.ToolProjection,
	assembly ecosystem.MCPHubResultObservation,
) bobContextAdmission {
	projection = projection.Normalize()
	if captured.valid && projection.Digest != nil && projection.Digest.Kind == ecosystem.DigestMCPHubStored &&
		projection.Route.CallID != "" {
		a.mu.Lock()
		if a.bobStoredAdmissions == nil || len(a.bobStoredAdmissions) >= maxBobStoredAdmissions {
			a.bobStoredAdmissions = make(map[string]bobContextAdmission)
		}
		a.bobStoredAdmissions[projection.Route.CallID] = captured
		a.mu.Unlock()
		return bobContextAdmission{}
	}
	if assembly.Bound && assembly.Complete && projection.Route.CallID != "" {
		a.mu.Lock()
		admission := a.bobStoredAdmissions[projection.Route.CallID]
		delete(a.bobStoredAdmissions, projection.Route.CallID)
		a.mu.Unlock()
		return admission
	}
	return captured
}

func (a *Agent) clearBobStoredAdmissions() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.bobStoredAdmissions = nil
	a.mu.Unlock()
}

func (a *Agent) settleBobContextAdmission(
	out Output,
	admission bobContextAdmission,
	projection ecosystem.ToolProjection,
	receipt ecosystem.RawReceipt,
	assembly ecosystem.MCPHubResultObservation,
	terminal executionpkg.EventType,
) {
	acceptedTerminal := terminal == executionpkg.EventCompleted ||
		terminal == executionpkg.EventFailed && projection.Transport == ecosystem.TransportSucceeded
	if a == nil || !admission.valid || !acceptedTerminal {
		return
	}
	projection = projection.Normalize()
	workspace := assembly.Workspace
	if workspace == "" {
		workspace, _ = ecosystem.BobContextWorkspace(projection, receipt)
	}
	validDomain := projection.Domain == ecosystem.DomainSucceeded || projection.Domain == ecosystem.DomainDrift ||
		projection.Domain == ecosystem.DomainConflict
	valid := projection.Specialist == "bob" && projection.Operation == "bob_context" &&
		projection.Transport == ecosystem.TransportSucceeded && projection.DomainTyped && validDomain &&
		projection.Evidence == ecosystem.EvidenceNone && projection.Digest != nil &&
		projection.Digest.Kind == ecosystem.DigestBobContext && workspace == admission.candidate.workspace &&
		a.bobWorkspaceCandidateCurrent(admission.candidate)

	a.mu.Lock()
	if admission.generation != a.bobWorkspaceGeneration {
		a.mu.Unlock()
		return
	}
	if !valid {
		// A typed failed refresh invalidates an installed projection. When no
		// projection existed, retain the generation so LA-3's reservation ledger
		// suppresses the same unchanged candidate on later turns.
		if a.bobWorkspaceContext != nil {
			a.invalidateBobWorkspaceContextLocked()
		}
		state := a.bobWorkspaceStateLocked()
		a.mu.Unlock()
		emitBobWorkspaceContext(out, state)
		return
	}
	a.bobWorkspaceContext = &bobWorkspaceContextCache{
		candidate: admission.candidate, domain: projection.Domain, digest: *cloneBobDigest(projection.Digest),
	}
	a.advanceBobWorkspaceGenerationLocked()
	state := a.bobWorkspaceStateLocked()
	a.mu.Unlock()
	emitBobWorkspaceContext(out, state)
}
