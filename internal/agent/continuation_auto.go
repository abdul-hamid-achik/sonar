package agent

import (
	"sort"
	"strings"
	"sync"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

const (
	hardMaxAutoContinuationSteps   = 2
	maxAutoContinuationSourceState = 32
)

// autoContinuationState is one explicitly enabled auto_read_only chain. It is
// intentionally separate from suggestion history: showing a proposal neither
// spends the automatic step budget nor reserves execution authority.
type autoContinuationState struct {
	mu            sync.Mutex
	registryEpoch uint64
	maxSteps      int
	steps         int
	seen          map[string]struct{}
}

func newAutoContinuationState(registryEpoch uint64, requestedSteps int) *autoContinuationState {
	state := &autoContinuationState{}
	state.reset(registryEpoch, requestedSteps)
	return state
}

// reset starts a new host-authorized chain. Callers must use it only at a real
// chain/session boundary; model retries must retain the existing state.
func (state *autoContinuationState) reset(registryEpoch uint64, requestedSteps int) {
	if state == nil {
		return
	}
	if requestedSteps < 0 {
		requestedSteps = 0
	}
	if requestedSteps > hardMaxAutoContinuationSteps {
		requestedSteps = hardMaxAutoContinuationSteps
	}
	state.mu.Lock()
	state.registryEpoch = registryEpoch
	state.maxSteps = requestedSteps
	state.steps = 0
	state.seen = make(map[string]struct{}, requestedSteps)
	state.mu.Unlock()
}

// preparedAutoContinuation is detached, bounded scheduling input. It contains
// no downstream prose or raw StructuredContent. The call arguments are cloned
// both when prepared and when handed to the queue.
type preparedAutoContinuation struct {
	continuation      ValidatedContinuation
	registryEpoch     uint64
	authorityVersion  uint64
	freshnessSequence uint64
}

func (prepared *preparedAutoContinuation) detachedCall() llm.ToolCall {
	if prepared == nil {
		return llm.ToolCall{}
	}
	call := prepared.continuation.Call
	call.Arguments = cloneApprovalArguments(call.Arguments)
	return call
}

// selectAutoReadOnlyContinuation is the in-loop LA-3 seam. It returns a call
// only when the exact successful source supplied exactly one fully specified,
// unblocked action and the current host/catalog contract proves the target is
// a closed-world, idempotent read. A successful return atomically spends one
// chain step and reserves the source fingerprint across Agent turns.
func (a *Agent) selectAutoReadOnlyContinuation(
	sourceCall llm.ToolCall,
	projection ecosystem.ToolProjection,
	candidates []ecosystem.ContinuationAction,
	snapshot mcp.ToolSnapshot,
	sourceAuthorized bool,
	sourceRouteVersion uint64,
	authorityMode AuthorityMode,
	state *autoContinuationState,
) *preparedAutoContinuation {
	if a == nil || state == nil || !sourceAuthorized || len(candidates) != 1 ||
		sourceRouteVersion != a.mcpRouteVersionSnapshot() ||
		projection.Transport != ecosystem.TransportSucceeded || projection.Domain != ecosystem.DomainSucceeded ||
		!projection.DomainTyped || !isContinuationSourceProjection(projection) ||
		len(candidates[0].Inputs) != 0 || len(candidates[0].BlockedBy) != 0 {
		return nil
	}
	validated, ok := a.validateContinuation(sourceCall, projection, candidates[0], snapshot)
	if !ok {
		return nil
	}
	freshnessSequence, ok := a.observeContinuationFreshness(validated)
	if !ok || sourceRouteVersion != a.mcpRouteVersionSnapshot() {
		return nil
	}
	return a.prepareAndReserveAutoContinuation(validated, snapshot, authorityMode, freshnessSequence, state)
}

// claimAutoReadOnlyContinuationContext atomically converts the initial opaque
// Goal/host continuation into a scheduled read. A nil state represents config
// that is not auto_read_only. Every failed eligibility or reservation check
// leaves the one-shot context untouched so suggestion mode can still consume
// it through continuationContextText.
func (a *Agent) claimAutoReadOnlyContinuationContextWithSnapshot(
	context *ContinuationContext,
	snapshot mcp.ToolSnapshot,
	authorityMode AuthorityMode,
	state *autoContinuationState,
) *preparedAutoContinuation {
	if a == nil || context == nil || context.owner != a || state == nil {
		return nil
	}
	context.mu.Lock()
	defer context.mu.Unlock()
	if context.consumed || context.registryEpoch == 0 || context.registryEpoch != snapshot.Epoch ||
		!a.continuationContextSourceStillAuthorized(context) ||
		!a.continuationFreshnessCurrent(&context.continuation, context.issueSequence) {
		return nil
	}
	prepared := a.prepareAndReserveAutoContinuation(
		&context.continuation, snapshot, authorityMode, context.issueSequence, state,
	)
	if prepared == nil {
		return nil
	}
	context.consumed = true
	return prepared
}

func (a *Agent) prepareAndReserveAutoContinuation(
	continuation *ValidatedContinuation,
	snapshot mcp.ToolSnapshot,
	authorityMode AuthorityMode,
	freshnessSequence uint64,
	state *autoContinuationState,
) *preparedAutoContinuation {
	authorityVersion := a.approvalStateSnapshot().hostVersion
	if !a.continuationFreshnessCurrent(continuation, freshnessSequence) ||
		!a.autoReadOnlyContinuationEligible(continuation, snapshot, authorityMode) {
		return nil
	}
	if a.approvalStateSnapshot().hostVersion != authorityVersion ||
		!a.continuationFreshnessCurrent(continuation, freshnessSequence) {
		return nil
	}
	prepared := &preparedAutoContinuation{
		continuation:      cloneValidatedContinuation(continuation),
		registryEpoch:     snapshot.Epoch,
		authorityVersion:  authorityVersion,
		freshnessSequence: freshnessSequence,
	}
	if !a.reserveAutoContinuation(prepared, state) {
		return nil
	}
	return prepared
}

func (a *Agent) autoReadOnlyContinuationEligible(
	continuation *ValidatedContinuation,
	snapshot mcp.ToolSnapshot,
	authorityMode AuthorityMode,
) bool {
	if a == nil || a.registry == nil || continuation == nil || authorityMode != AuthorityAutoScoped ||
		continuation.SourceDomain != ecosystem.DomainSucceeded || len(continuation.Inputs) != 0 ||
		len(continuation.BlockedBy) != 0 || continuation.Effect != executionpkg.EffectReadOnly ||
		continuation.Fingerprint == "" || continuation.SchemaDigest == "" || continuation.BehaviorDigest == "" ||
		snapshot.Epoch == 0 || a.registry.SnapshotTools().Epoch != snapshot.Epoch ||
		!a.continuationWorkspaceMatches(continuation.WorkspaceRef) ||
		!a.allowsMCPTool(continuation.Call.Name) || a.authorityPermissionDeniedForCall(continuation.Call) ||
		!exactPinnedAutoContinuationTarget(a, continuation.Call) {
		return false
	}
	if continuation.hostBootstrap != nil {
		if continuation.Source != "local_agent" || continuation.SourceOperation != "bob_workspace_bootstrap" ||
			continuation.Tool != "bob_context" || !a.bobBootstrapClaimCurrent(continuation.hostBootstrap) {
			return false
		}
	} else if continuation.Source != "bob" && continuation.Source != "cortex" {
		// Exact Bob and Cortex parsers are the only downstream authorities that
		// may issue LA-3 continuations. Other sources cannot gain read authority by
		// constructing a lookalike normalized value.
		return false
	}

	kind, effect := a.executionKindForCall(continuation.Call)
	contract, trusted := a.trustedMCPContract(continuation.Call)
	if kind != executionpkg.KindMCP || effect != executionpkg.EffectReadOnly || !trusted || !contract.auto ||
		contract.effect != executionpkg.EffectReadOnly || !a.authorityAutoApproves(authorityMode, continuation.Call, kind) {
		return false
	}
	definition, ok := exactAutoContinuationDefinition(snapshot, continuation.Call.Name)
	if !ok || !snapshot.ServerAvailable(autoContinuationNamespace(continuation.Call.Name)) {
		return false
	}
	schemaDigest, ok := continuationSchemaDigest(definition)
	if !ok || schemaDigest != continuation.SchemaDigest ||
		autoContinuationBehaviorDigest(definition) != continuation.BehaviorDigest ||
		!autoContinuationBehaviorEligible(definition.Behavior) {
		return false
	}
	return true
}

func exactPinnedAutoContinuationTarget(a *Agent, call llm.ToolCall) bool {
	if a == nil || call.Name == "" || strings.TrimSpace(call.Name) != call.Name {
		return false
	}
	parts := strings.Split(call.Name, "__")
	switch len(parts) {
	case 2:
		server, ok := a.trustedMCPServer(parts[0])
		return ok && server.gateway == "" && parts[1] != "mcphub_call_tool"
	case 3:
		server, ok := a.trustedMCPServer(parts[0])
		return ok && server.gateway != "" && parts[1] != "" && parts[2] != ""
	default:
		return false
	}
}

func autoContinuationNamespace(name string) string {
	namespace, _, _ := strings.Cut(name, "__")
	return namespace
}

func exactAutoContinuationDefinition(snapshot mcp.ToolSnapshot, name string) (llm.ToolDef, bool) {
	var match llm.ToolDef
	found := false
	for _, definition := range snapshot.Tools {
		if definition.Name != name {
			continue
		}
		if found {
			return llm.ToolDef{}, false
		}
		match, found = definition, true
	}
	return match, found
}

func autoContinuationBehaviorEligible(behavior llm.ToolBehavior) bool {
	return behavior.Declared && behavior.ReadOnly && !behavior.Destructive &&
		behavior.Idempotent && !behavior.OpenWorld
}

func autoContinuationBehaviorDigest(definition llm.ToolDef) string {
	fields := []string{
		definition.Name,
		boolToken(definition.Behavior.Declared),
		boolToken(definition.Behavior.ReadOnly),
		boolToken(definition.Behavior.Destructive),
		boolToken(definition.Behavior.Idempotent),
		boolToken(definition.Behavior.OpenWorld),
	}
	return executionpkg.HashText(strings.Join(fields, "\x00"))
}

func boolToken(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func cloneValidatedContinuation(source *ValidatedContinuation) ValidatedContinuation {
	if source == nil {
		return ValidatedContinuation{}
	}
	clone := *source
	clone.Call.Arguments = cloneApprovalArguments(source.Call.Arguments)
	clone.Inputs = append([]string(nil), source.Inputs...)
	clone.BlockedBy = append([]string(nil), source.BlockedBy...)
	if source.hostBootstrap != nil {
		claim := *source.hostBootstrap
		clone.hostBootstrap = &claim
	}
	return clone
}

type autoContinuationHistory struct {
	records map[string]autoContinuationRecord
	order   []string
}

type autoContinuationRecord struct {
	revision      uint64
	contextDigest string
	fingerprint   string
	retired       []string
}

func newAutoContinuationHistory() *autoContinuationHistory {
	return &autoContinuationHistory{records: make(map[string]autoContinuationRecord)}
}

func (history *autoContinuationHistory) canReserve(continuation *ValidatedContinuation) bool {
	if history == nil || continuation == nil || continuation.Fingerprint == "" {
		return false
	}
	key := continuationSourceLifecycleKey(continuation)
	prior, exists := history.records[key]
	if !exists {
		return true
	}
	if continuation.SourceTask != "" {
		// Cortex task revisions are immutable and monotonic across operations.
		return continuation.SourceRevision > prior.revision
	}
	if continuation.ContextDigest == "" || continuation.ContextDigest == prior.contextDigest {
		return false
	}
	for _, retired := range prior.retired {
		if continuation.ContextDigest == retired {
			return false
		}
	}
	return continuation.Fingerprint != prior.fingerprint
}

func (history *autoContinuationHistory) reserve(continuation *ValidatedContinuation) {
	key := continuationSourceLifecycleKey(continuation)
	prior, exists := history.records[key]
	if !exists {
		history.order = append(history.order, key)
	}
	record := autoContinuationRecord{
		revision: continuation.SourceRevision, contextDigest: continuation.ContextDigest,
		fingerprint: continuation.Fingerprint,
	}
	if exists && continuation.SourceTask == "" && prior.contextDigest != "" && prior.contextDigest != continuation.ContextDigest {
		record.retired = append(append([]string(nil), prior.retired...), prior.contextDigest)
		if len(record.retired) > maxAutoContinuationSourceState {
			record.retired = record.retired[len(record.retired)-maxAutoContinuationSourceState:]
		}
	}
	history.records[key] = record
	for len(history.order) > maxAutoContinuationSourceState {
		oldest := history.order[0]
		history.order = history.order[1:]
		delete(history.records, oldest)
	}
}

func (a *Agent) reserveAutoContinuation(prepared *preparedAutoContinuation, state *autoContinuationState) bool {
	if a == nil || prepared == nil || state == nil || prepared.registryEpoch == 0 {
		return false
	}
	continuation := &prepared.continuation
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.registryEpoch != prepared.registryEpoch || state.maxSteps <= 0 || state.steps >= state.maxSteps {
		return false
	}
	if _, duplicate := state.seen[continuation.Fingerprint]; duplicate {
		return false
	}
	if a.registry == nil || a.registry.SnapshotTools().Epoch != prepared.registryEpoch {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.continuationFreshness == nil ||
		!a.continuationFreshness.current(continuation, prepared.freshnessSequence) {
		return false
	}
	if a.autoContinuationHistory == nil {
		a.autoContinuationHistory = newAutoContinuationHistory()
	}
	if !a.autoContinuationHistory.canReserve(continuation) {
		return false
	}
	a.autoContinuationHistory.reserve(continuation)
	state.seen[continuation.Fingerprint] = struct{}{}
	state.steps++
	return true
}

func (a *Agent) resetAutoContinuationHistoryLocked() {
	a.autoContinuationHistory = newAutoContinuationHistory()
	a.continuationFreshness = newContinuationFreshnessState()
}

// autoContinuationHistorySnapshot is a deterministic test/diagnostic view. It
// exposes only host hashes and counters, never arguments or downstream data.
func (a *Agent) autoContinuationHistorySnapshot() (steps int, fingerprints []string) {
	if a == nil {
		return 0, nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.autoContinuationHistory == nil {
		return 0, nil
	}
	for _, record := range a.autoContinuationHistory.records {
		fingerprints = append(fingerprints, record.fingerprint)
	}
	sort.Strings(fingerprints)
	return len(fingerprints), fingerprints
}
