package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

const maxContinuationFingerprintHistory = 32

type continuationArgumentState uint8

const (
	continuationArgumentsInvalid continuationArgumentState = iota
	continuationArgumentsReady
	continuationArgumentsNeedInput
)

// ValidatedContinuation is an ephemeral host-owned continuation contract. Call
// has passed exact registry, schema, workspace, scope, trust, and deny-policy
// validation. LA-2 exposes it only as a suggestion; LA-3 applies additional
// read-only eligibility and atomic reservation before it may be scheduled.
// Command prose is absent by construction.
type ValidatedContinuation struct {
	Source          string
	SourceOperation string
	Tool            string
	Call            llm.ToolCall
	Inputs          []string
	BlockedBy       []string
	ReasonCode      string
	Effect          executionpkg.EffectClass
	SourceDomain    ecosystem.DomainState
	Fingerprint     string
	SourceTask      string
	SourceRevision  uint64
	ContextDigest   string
	SchemaDigest    string
	BehaviorDigest  string
	WorkspaceRef    string
	// hostBootstrap is set only by the Agent's Bob workspace bootstrap. It is
	// deliberately private: downstream action payloads and embedding callers
	// cannot manufacture the extra authority needed for this host-owned read.
	hostBootstrap *bobBootstrapClaim
}

// ContinuationSuggestion is the optional output surface consumed by the TUI.
// It deliberately excludes arguments, workspaces, arbitrary reasons, and raw
// downstream content.
type ContinuationSuggestion struct {
	Tool       string
	Inputs     []string
	BlockedBy  []string
	ReasonCode string
}

// ContinuationContext is an opaque, ephemeral capability produced only by this
// Agent after exact receipt, registry, schema, workspace and policy validation.
// Its fields are private so UI/advisor code cannot forge arguments or persist a
// downstream map; RunTurn revalidates it immediately before model use.
type ContinuationContext struct {
	mu                 sync.Mutex
	owner              *Agent
	registryEpoch      uint64
	sourceRouteVersion uint64
	issueSequence      uint64
	sourceCall         llm.ToolCall
	continuation       ValidatedContinuation
	consumed           bool
}

// ContinuationOutput is optional so headless and test outputs retain the small
// Output interface. A nil suggestion is an authoritative clear for Sequence.
type ContinuationOutput interface {
	ContinuationSuggestion(turnID string, sequence uint64, suggestion *ContinuationSuggestion)
}

func emitContinuationSuggestion(out Output, turnID string, sequence uint64, continuation *ValidatedContinuation) {
	consumer, ok := out.(ContinuationOutput)
	if !ok {
		return
	}
	if continuation == nil {
		consumer.ContinuationSuggestion(turnID, sequence, nil)
		return
	}
	blocked := []string(nil)
	if len(continuation.BlockedBy) > 0 {
		// Downstream blocker text is useful to the active model turn but remains
		// transient product prose. The TUI receives only a host-authored code.
		blocked = []string{"source_blocked"}
	}
	consumer.ContinuationSuggestion(turnID, sequence, &ContinuationSuggestion{
		Tool: continuation.Tool, Inputs: append([]string(nil), continuation.Inputs...),
		BlockedBy: blocked, ReasonCode: continuation.ReasonCode,
	})
}

// InterpretContinuationResult applies the same exact LA-2 boundary to a
// host-owned read that occurred outside the normal ReAct loop (notably Goal
// Runtime's Cortex status check). Raw StructuredContent never leaves this call.
func (a *Agent) InterpretContinuationResult(call llm.ToolCall, result *mcp.ToolResult) *ContinuationContext {
	if a == nil || result == nil || a.registry == nil {
		return nil
	}
	sourceRouteVersion := a.mcpRouteVersionSnapshot()
	projection := a.projectSemanticToolReceipt(
		call, result.Content, result.Structured, result.ErrorMeta, false, result.IsError, false,
	)
	receipt := ecosystem.RawReceipt{
		Text: result.Content, Structured: result.Structured, ErrorMeta: result.ErrorMeta, ToolError: result.IsError,
	}
	sourceAuthorized := a.continuationSourceAuthorized(call, false) &&
		a.allowsMCPTool(call.Name) && !a.authorityPermissionDeniedForCall(call)
	var candidates []ecosystem.ContinuationAction
	if sourceAuthorized {
		candidates = ecosystem.ProjectContinuationActions(projection, receipt)
	}
	snapshot := a.mcpToolSnapshot()
	state := newContinuationTurnState(snapshot.Epoch)
	continuation := a.selectContinuationCandidate(call, projection, candidates, snapshot, sourceAuthorized, true, state, false)
	if continuation == nil || a.mcpRouteVersionSnapshot() != sourceRouteVersion {
		return nil
	}
	issueSequence, ok := a.observeContinuationFreshness(continuation)
	if !ok || a.mcpRouteVersionSnapshot() != sourceRouteVersion {
		return nil
	}
	return &ContinuationContext{
		owner: a, registryEpoch: snapshot.Epoch, sourceRouteVersion: sourceRouteVersion,
		issueSequence: issueSequence, sourceCall: cloneContinuationToolCall(call), continuation: *continuation,
	}
}

// continuationSourceAuthorized proves that the receipt which supplied an
// action came from an exact host-trusted route. Target validation alone is not
// sufficient: an untrusted wrapper must not be able to emit a Cortex-shaped
// action that points at a separately trusted tool.
//
// A bound MCPHub page has two authority links. The assembler has already tied
// its call ID to the exact trusted downstream dispatch; this check proves that
// the current page itself came from the exact trusted get_result operation.
func (a *Agent) continuationSourceAuthorized(call llm.ToolCall, boundAssembly bool) bool {
	if a == nil {
		return false
	}
	if boundAssembly {
		parts := strings.Split(call.Name, "__")
		return len(parts) == 2 && parts[1] == "mcphub_get_result" &&
			a.trustedDirectMCPHubOperation(call, "mcphub_get_result")
	}
	_, trusted := a.trustedMCPContract(call)
	return trusted
}

// continuationSurfacePresent checks StructuredContent and TextContent as
// independent security surfaces. receiptDocument intentionally prefers
// StructuredContent for semantic parsing, but a server may return a different
// action-bearing TextContent document; that prose must still be suppressed.
func continuationSurfacePresent(projection ecosystem.ToolProjection, receipt ecosystem.RawReceipt) bool {
	if ecosystem.ReceiptHasContinuationActions(projection, receipt) {
		return true
	}
	if len(strings.TrimSpace(receipt.Text)) == 0 || len(receipt.Structured) == 0 {
		return false
	}
	textOnly := receipt
	textOnly.Structured = nil
	return ecosystem.ReceiptHasContinuationActions(projection, textOnly)
}

func (a *Agent) continuationContextText(context *ContinuationContext) string {
	if a == nil || context == nil || context.owner != a {
		return ""
	}
	context.mu.Lock()
	defer context.mu.Unlock()
	if context.consumed {
		return ""
	}
	modelContext := a.validatedContinuationContextText(context, &context.continuation)
	if modelContext == "" {
		return ""
	}

	// Commit source freshness and the LA-2 replay reservation under one Agent
	// lock. A newer Cortex revision/Bob digest or route-policy transition cannot
	// slip between validation and consumption.
	a.mu.Lock()
	defer a.mu.Unlock()
	if context.sourceRouteVersion != a.mcpRouteVersion || a.continuationFreshness == nil ||
		!a.continuationFreshness.current(&context.continuation, context.issueSequence) {
		return ""
	}
	if a.continuationHistory == nil {
		a.continuationHistory = newContinuationTurnState(0)
	}
	if !a.continuationHistory.accept(&context.continuation) {
		return ""
	}
	context.consumed = true
	return modelContext
}

// previewContinuationContext validates the same bounded model projection as
// continuationContextText without consuming the opaque capability or its
// replay fingerprint. RunTurn uses the preview only for context admission and
// commits it after the resulting prompt is known to fit.
func (a *Agent) previewContinuationContext(context *ContinuationContext) string {
	if a == nil || context == nil || context.owner != a {
		return ""
	}
	context.mu.Lock()
	defer context.mu.Unlock()
	if context.consumed {
		return ""
	}
	modelContext := a.validatedContinuationContextText(context, &context.continuation)
	if modelContext == "" || !a.wouldAcceptContinuationHistory(&context.continuation) {
		return ""
	}
	return modelContext
}

func (a *Agent) validatedContinuationContextText(context *ContinuationContext, continuation *ValidatedContinuation) string {
	if a == nil || context == nil || continuation == nil || a.registry == nil || context.registryEpoch == 0 {
		return ""
	}
	if !a.continuationContextSourceStillAuthorized(context) ||
		!a.continuationFreshnessCurrent(continuation, context.issueSequence) {
		return ""
	}
	snapshot := a.mcpToolSnapshot()
	if snapshot.Epoch != context.registryEpoch || !a.continuationWorkspaceMatches(continuation.WorkspaceRef) ||
		!a.allowsMCPTool(continuation.Call.Name) || a.authorityPermissionDeniedForCall(continuation.Call) {
		return ""
	}
	contract, trusted := a.trustedMCPContract(continuation.Call)
	_, effect := a.executionKindForCall(continuation.Call)
	if !trusted || effect == executionpkg.EffectUnknown || effect != contract.effect || effect != continuation.Effect {
		return ""
	}
	if !continuationSchemaStillCurrent(a, continuation, snapshot) {
		return ""
	}
	return continuation.modelContext()
}

// consumeContinuationContext turns an already validated opaque context into a
// one-shot model projection. Validation runs before history admission, so a
// stale registry, changed policy, or failed preflight cannot burn the action's
// fingerprint. The context lock and the Agent's history lock together ensure
// that concurrent replays admit at most one projection.
func consumeContinuationContext(
	a *Agent,
	context *ContinuationContext,
	validate func(*ValidatedContinuation) string,
) string {
	if a == nil || context == nil || context.owner != a || validate == nil {
		return ""
	}
	context.mu.Lock()
	defer context.mu.Unlock()
	if context.consumed {
		return ""
	}
	modelContext := validate(&context.continuation)
	if modelContext == "" {
		return ""
	}
	if !a.acceptContinuationHistory(&context.continuation) {
		return ""
	}
	context.consumed = true
	return modelContext
}

func continuationSchemaStillCurrent(a *Agent, continuation *ValidatedContinuation, snapshot mcp.ToolSnapshot) bool {
	if continuation == nil || continuation.SchemaDigest == "" {
		return false
	}
	// Lazy MCPHub calls are validated against the resolved downstream tool
	// contract, not the generic call_tool wrapper advertised by the registry.
	// Check the exact cached downstream schema first so the wrapper definition
	// cannot shadow it and make every valid lazy continuation look stale.
	if a.isTrustedLazyMCPHubCall(continuation.Call.Name) {
		gateway, server, tool, ok := a.trustedMCPHubDownstreamTarget(continuation.Call)
		if !ok {
			return false
		}
		_, digest, ok := a.continuationContract(
			continuationContractKey{Gateway: gateway, Server: server, Tool: tool}, snapshot.Epoch,
		)
		return ok && hex.EncodeToString(digest[:]) == continuation.SchemaDigest
	}
	for _, definition := range snapshot.Tools {
		if definition.Name != continuation.Call.Name {
			continue
		}
		digest, ok := continuationSchemaDigest(definition)
		return ok && digest == continuation.SchemaDigest
	}
	return false
}

func isContinuationSourceProjection(projection ecosystem.ToolProjection) bool {
	if projection.Transport != ecosystem.TransportSucceeded || !projection.DomainTyped {
		return false
	}
	switch projection.Specialist {
	case "cortex":
		return projection.Role == ecosystem.RoleCoordination
	case "bob":
		return projection.Role == ecosystem.RoleBuild &&
			(projection.Operation == "bob_context" || projection.Operation == "bob_path" || projection.Operation == "bob_playbook")
	default:
		return false
	}
}

type continuationTurnState struct {
	registryEpoch uint64
	seen          []string
	seenSet       map[string]struct{}
	latest        map[string]uint64
	immutable     map[string]continuationImmutable
	context       map[string]string
	retired       map[string][]string
	sourceOrder   []string
}

type continuationImmutable struct {
	revision    uint64
	fingerprint string
}

// continuationFreshnessState records the newest exact source state observed
// for each Cortex task or Bob operation without spending the separate LA-2 or
// LA-3 replay reservations. Opaque contexts carry the issue sequence so a
// newer observed revision/digest invalidates an older unconsumed capability.
type continuationFreshnessState struct {
	next    uint64
	records map[string]continuationFreshnessRecord
	order   []string
}

type continuationFreshnessRecord struct {
	sequence      uint64
	revision      uint64
	contextDigest string
	fingerprint   string
	retired       []string
}

func newContinuationFreshnessState() *continuationFreshnessState {
	return &continuationFreshnessState{records: make(map[string]continuationFreshnessRecord)}
}

func (state *continuationFreshnessState) observe(action *ValidatedContinuation) (uint64, bool) {
	if state == nil || action == nil || action.Fingerprint == "" {
		return 0, false
	}
	key := continuationSourceLifecycleKey(action)
	prior, exists := state.records[key]
	if action.SourceTask != "" {
		if exists && (action.SourceRevision < prior.revision ||
			(action.SourceRevision == prior.revision && action.Fingerprint != prior.fingerprint)) {
			return 0, false
		}
	} else {
		if action.ContextDigest == "" {
			return 0, false
		}
		if exists {
			for _, retired := range prior.retired {
				if action.ContextDigest == retired {
					return 0, false
				}
			}
			if action.ContextDigest == prior.contextDigest && action.Fingerprint != prior.fingerprint {
				return 0, false
			}
		}
	}

	state.next++
	if state.next == 0 {
		// Overflow is practically unreachable, but sequence zero is reserved as
		// invalid so a wrapped capability must fail closed.
		return 0, false
	}
	record := continuationFreshnessRecord{
		sequence: state.next, revision: action.SourceRevision,
		contextDigest: action.ContextDigest, fingerprint: action.Fingerprint,
	}
	if exists && action.SourceTask == "" {
		record.retired = append([]string(nil), prior.retired...)
		if prior.contextDigest != "" && prior.contextDigest != action.ContextDigest {
			record.retired = append(record.retired, prior.contextDigest)
			if len(record.retired) > maxContinuationFingerprintHistory {
				record.retired = record.retired[len(record.retired)-maxContinuationFingerprintHistory:]
			}
		}
	}
	if !exists {
		state.order = append(state.order, key)
	}
	state.records[key] = record
	for len(state.order) > maxContinuationFingerprintHistory {
		oldest := state.order[0]
		state.order = state.order[1:]
		delete(state.records, oldest)
	}
	return record.sequence, true
}

func (state *continuationFreshnessState) current(action *ValidatedContinuation, sequence uint64) bool {
	if state == nil || action == nil || sequence == 0 {
		return false
	}
	record, ok := state.records[continuationSourceLifecycleKey(action)]
	return ok && record.sequence == sequence && record.revision == action.SourceRevision &&
		record.contextDigest == action.ContextDigest && record.fingerprint == action.Fingerprint
}

func newContinuationTurnState(registryEpoch uint64) *continuationTurnState {
	return &continuationTurnState{
		registryEpoch: registryEpoch, seenSet: make(map[string]struct{}),
		latest: make(map[string]uint64), immutable: make(map[string]continuationImmutable),
		context: make(map[string]string), retired: make(map[string][]string),
	}
}

func (state *continuationTurnState) accept(action *ValidatedContinuation) bool {
	if state == nil || action == nil || action.Fingerprint == "" {
		return false
	}
	sourceKey := continuationSourceLifecycleKey(action)
	state.rememberSource(sourceKey)
	if action.ContextDigest != "" {
		for _, retired := range state.retired[sourceKey] {
			if action.ContextDigest == retired {
				return false
			}
		}
		if current := state.context[sourceKey]; current != "" && current != action.ContextDigest {
			retired := append(state.retired[sourceKey], current)
			if len(retired) > maxContinuationFingerprintHistory {
				retired = retired[len(retired)-maxContinuationFingerprintHistory:]
			}
			state.retired[sourceKey] = retired
		}
		state.context[sourceKey] = action.ContextDigest
	}
	if latest, exists := state.latest[sourceKey]; exists && action.SourceRevision < latest {
		return false
	}
	if action.SourceRevision > state.latest[sourceKey] {
		state.latest[sourceKey] = action.SourceRevision
	}
	// Cortex revisions are immutable. Bob context digests are allowed to advance
	// and are retired explicitly above because they have no monotonic revision.
	if action.SourceTask != "" {
		if prior, exists := state.immutable[sourceKey]; exists &&
			prior.revision == action.SourceRevision && prior.fingerprint != action.Fingerprint {
			return false
		}
		state.immutable[sourceKey] = continuationImmutable{revision: action.SourceRevision, fingerprint: action.Fingerprint}
	}
	if _, duplicate := state.seenSet[action.Fingerprint]; duplicate {
		return false
	}
	state.seenSet[action.Fingerprint] = struct{}{}
	state.seen = append(state.seen, action.Fingerprint)
	if len(state.seen) > maxContinuationFingerprintHistory {
		oldest := state.seen[0]
		state.seen = state.seen[1:]
		delete(state.seenSet, oldest)
	}
	return true
}

func continuationSourceLifecycleKey(action *ValidatedContinuation) string {
	if action == nil {
		return ""
	}
	// Cortex revisions belong to the task rather than to an individual status,
	// plan, or handoff operation. Keying them by operation would allow a delayed
	// response from one operation to replay stale state after a newer response
	// from another. Bob has no task revision, so its digests remain scoped to the
	// exact read-only operation that defines their meaning.
	if action.SourceTask != "" {
		return strings.Join([]string{action.Source, action.SourceTask, action.WorkspaceRef}, "\x00")
	}
	return strings.Join([]string{action.Source, action.SourceOperation, action.WorkspaceRef}, "\x00")
}

func (state *continuationTurnState) rememberSource(sourceKey string) {
	for _, existing := range state.sourceOrder {
		if existing == sourceKey {
			return
		}
	}
	state.sourceOrder = append(state.sourceOrder, sourceKey)
	if len(state.sourceOrder) <= maxContinuationFingerprintHistory {
		return
	}
	oldest := state.sourceOrder[0]
	state.sourceOrder = state.sourceOrder[1:]
	delete(state.latest, oldest)
	delete(state.immutable, oldest)
	delete(state.context, oldest)
	delete(state.retired, oldest)
}

func (a *Agent) acceptContinuationHistory(action *ValidatedContinuation) bool {
	if a == nil || action == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.continuationHistory == nil {
		a.continuationHistory = newContinuationTurnState(0)
	}
	return a.continuationHistory.accept(action)
}

func (a *Agent) observeContinuationFreshness(action *ValidatedContinuation) (uint64, bool) {
	if a == nil || action == nil {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.continuationFreshness == nil {
		a.continuationFreshness = newContinuationFreshnessState()
	}
	return a.continuationFreshness.observe(action)
}

func (a *Agent) continuationFreshnessCurrent(action *ValidatedContinuation, sequence uint64) bool {
	if a == nil || action == nil || sequence == 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.continuationFreshness != nil && a.continuationFreshness.current(action, sequence)
}

func (a *Agent) continuationContextSourceStillAuthorized(context *ContinuationContext) bool {
	if a == nil || context == nil || context.sourceRouteVersion != a.mcpRouteVersionSnapshot() {
		return false
	}
	call := context.sourceCall
	return a.continuationSourceAuthorized(call, false) && a.allowsMCPTool(call.Name) &&
		!a.authorityPermissionDeniedForCall(call)
}

func cloneContinuationToolCall(call llm.ToolCall) llm.ToolCall {
	clone := call
	clone.Arguments = cloneApprovalArguments(call.Arguments)
	return clone
}

// wouldAcceptContinuationHistory is the non-mutating counterpart used during
// prompt admission. The state is tiny and bounded, so copying it avoids
// reserving a fingerprint for a turn that may still fail its context budget.
func (a *Agent) wouldAcceptContinuationHistory(action *ValidatedContinuation) bool {
	if a == nil || action == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	state := cloneContinuationTurnState(a.continuationHistory)
	if state == nil {
		state = newContinuationTurnState(0)
	}
	return state.accept(action)
}

func cloneContinuationTurnState(source *continuationTurnState) *continuationTurnState {
	if source == nil {
		return nil
	}
	clone := &continuationTurnState{
		registryEpoch: source.registryEpoch,
		seen:          append([]string(nil), source.seen...),
		seenSet:       make(map[string]struct{}, len(source.seenSet)),
		latest:        make(map[string]uint64, len(source.latest)),
		immutable:     make(map[string]continuationImmutable, len(source.immutable)),
		context:       make(map[string]string, len(source.context)),
		retired:       make(map[string][]string, len(source.retired)),
		sourceOrder:   append([]string(nil), source.sourceOrder...),
	}
	for key := range source.seenSet {
		clone.seenSet[key] = struct{}{}
	}
	for key, value := range source.latest {
		clone.latest[key] = value
	}
	for key, value := range source.immutable {
		clone.immutable[key] = value
	}
	for key, value := range source.context {
		clone.context[key] = value
	}
	for key, value := range source.retired {
		clone.retired[key] = append([]string(nil), value...)
	}
	return clone
}

func (a *Agent) selectContinuation(
	sourceCall llm.ToolCall,
	projection ecosystem.ToolProjection,
	candidates []ecosystem.ContinuationAction,
	snapshot mcp.ToolSnapshot,
	sourceAuthorized bool,
	allowMCP bool,
	state *continuationTurnState,
) *ValidatedContinuation {
	return a.selectContinuationCandidate(sourceCall, projection, candidates, snapshot, sourceAuthorized, allowMCP, state, true)
}

func (a *Agent) selectContinuationCandidate(
	sourceCall llm.ToolCall,
	projection ecosystem.ToolProjection,
	candidates []ecosystem.ContinuationAction,
	snapshot mcp.ToolSnapshot,
	sourceAuthorized bool,
	allowMCP bool,
	state *continuationTurnState,
	recordHistory bool,
) *ValidatedContinuation {
	if a == nil || !sourceAuthorized || len(candidates) == 0 || !allowMCP || state == nil ||
		state.registryEpoch == 0 || snapshot.Epoch != state.registryEpoch || a.registry == nil ||
		a.registry.SnapshotTools().Epoch != state.registryEpoch {
		return nil
	}
	for _, candidate := range candidates {
		validated, ok := a.validateContinuation(sourceCall, projection, candidate, snapshot)
		fresh := true
		if ok && recordHistory {
			_, fresh = a.observeContinuationFreshness(validated)
		}
		if ok && fresh && state.accept(validated) && (!recordHistory || a.acceptContinuationHistory(validated)) {
			return validated
		}
	}
	return nil
}

func (a *Agent) validateContinuation(
	sourceCall llm.ToolCall,
	projection ecosystem.ToolProjection,
	candidate ecosystem.ContinuationAction,
	snapshot mcp.ToolSnapshot,
) (*ValidatedContinuation, bool) {
	if candidate.Source != projection.Specialist || candidate.SourceOperation != projection.Operation ||
		candidate.Tool == "" || candidate.WorkspaceRef == "" || snapshot.Epoch == 0 {
		return nil, false
	}
	server, ok := continuationToolServer(candidate.Tool)
	if !ok || !a.continuationWorkspaceMatches(candidate.WorkspaceRef) {
		return nil, false
	}
	arguments := candidate.ArgumentValues()
	call, definition, schemaDigest, ok := a.resolveContinuationTarget(
		sourceCall, projection, server, candidate.Tool, arguments, snapshot,
	)
	if !ok || !a.allowsMCPTool(call.Name) || a.authorityPermissionDeniedForCall(call) {
		return nil, false
	}
	contract, trusted := a.trustedMCPContract(call)
	if !trusted {
		return nil, false
	}
	_, effect := a.executionKindForCall(call)
	if effect == executionpkg.EffectUnknown || effect != contract.effect {
		return nil, false
	}
	argumentState, err := validateContinuationArguments(definition, arguments, candidate.Inputs)
	if err != nil || argumentState == continuationArgumentsInvalid {
		return nil, false
	}
	if len(candidate.Inputs) == 0 && argumentState != continuationArgumentsReady ||
		len(candidate.Inputs) > 0 && argumentState != continuationArgumentsNeedInput {
		return nil, false
	}

	reasonCode := candidate.ReasonCode
	if reasonCode == "" {
		switch {
		case len(candidate.BlockedBy) > 0:
			reasonCode = "source_blocked"
		case len(candidate.Inputs) > 0:
			reasonCode = "needs_input"
		default:
			reasonCode = "continuation"
		}
	}
	argumentHash, err := executionpkg.HashCanonicalArguments(arguments)
	if err != nil {
		return nil, false
	}
	inputs := append([]string(nil), candidate.Inputs...)
	sort.Strings(inputs)
	blockers := append([]string(nil), candidate.BlockedBy...)
	sort.Strings(blockers)
	behaviorDigest := autoContinuationBehaviorDigest(definition)
	fingerprint := executionpkg.HashText(strings.Join([]string{
		candidate.Source, candidate.SourceOperation, candidate.TaskID,
		fmt.Sprint(candidate.SourceRevision), candidate.ContextDigest,
		string(projection.Domain), call.Name, argumentHash, strings.Join(inputs, ","), strings.Join(blockers, ","),
		schemaDigest, behaviorDigest,
	}, "\x00"))
	return &ValidatedContinuation{
		Source: candidate.Source, SourceOperation: candidate.SourceOperation, Tool: candidate.Tool,
		Call: call, Inputs: append([]string(nil), candidate.Inputs...), BlockedBy: append([]string(nil), candidate.BlockedBy...),
		ReasonCode: reasonCode, Effect: effect, SourceDomain: projection.Domain, Fingerprint: fingerprint,
		SourceTask: candidate.TaskID, SourceRevision: candidate.SourceRevision,
		ContextDigest: candidate.ContextDigest, SchemaDigest: schemaDigest, BehaviorDigest: behaviorDigest,
		WorkspaceRef: candidate.WorkspaceRef,
	}, true
}

func continuationToolServer(tool string) (string, bool) {
	switch {
	case strings.HasPrefix(tool, "cortex_"):
		return "cortex", true
	case strings.HasPrefix(tool, "bob_"):
		return "bob", true
	default:
		return "", false
	}
}

func (a *Agent) resolveContinuationTarget(
	sourceCall llm.ToolCall,
	projection ecosystem.ToolProjection,
	server, tool string,
	arguments map[string]any,
	snapshot mcp.ToolSnapshot,
) (llm.ToolCall, llm.ToolDef, string, bool) {
	preferred := continuationPreferredTarget(sourceCall, projection, server, tool)
	matches := make([]llm.ToolDef, 0, 2)
	for _, definition := range snapshot.Tools {
		namespace, _, namespaced := strings.Cut(definition.Name, "__")
		if !namespaced || !snapshot.ServerAvailable(namespace) {
			continue
		}
		if continuationDefinitionMatchesRoute(a, definition, projection.Route.Gateway, server, tool) {
			matches = append(matches, definition)
		}
	}
	var definition llm.ToolDef
	if preferred != "" {
		for _, match := range matches {
			if match.Name == preferred {
				definition = match
				break
			}
		}
	}
	if definition.Name == "" && len(matches) == 1 {
		definition = matches[0]
	}
	if definition.Name != "" {
		digest, ok := continuationSchemaDigest(definition)
		if !ok {
			return llm.ToolCall{}, llm.ToolDef{}, "", false
		}
		return llm.ToolCall{Name: definition.Name, Arguments: arguments}, definition, digest, true
	}

	gateway := projection.Route.Gateway
	if gateway == "" || !snapshot.ServerAvailable(gateway) {
		return llm.ToolCall{}, llm.ToolDef{}, "", false
	}
	genericName := gateway + "__mcphub_call_tool"
	var generic llm.ToolDef
	for _, candidate := range snapshot.Tools {
		if candidate.Name == genericName {
			generic = candidate
			break
		}
	}
	if generic.Name == "" {
		return llm.ToolCall{}, llm.ToolDef{}, "", false
	}
	key := continuationContractKey{Gateway: gateway, Server: server, Tool: tool}
	cached, digest, ok := a.continuationContract(key, snapshot.Epoch)
	if !ok {
		return llm.ToolCall{}, llm.ToolDef{}, "", false
	}
	call := llm.ToolCall{Name: generic.Name, Arguments: map[string]any{
		"server": server, "tool": tool, "arguments": arguments,
	}}
	if preflightMCPToolArguments(generic, call.Arguments) != nil {
		return llm.ToolCall{}, llm.ToolDef{}, "", false
	}
	return call, cached, hex.EncodeToString(digest[:]), true
}

func continuationPreferredTarget(sourceCall llm.ToolCall, projection ecosystem.ToolProjection, server, tool string) string {
	if projection.Route.Gateway != "" {
		return projection.Route.Gateway + "__" + server + "__" + tool
	}
	parts := strings.Split(sourceCall.Name, "__")
	if len(parts) == 2 && projection.Specialist == server {
		return parts[0] + "__" + tool
	}
	return ""
}

func continuationDefinitionMatches(a *Agent, definition llm.ToolDef, server, tool string) bool {
	parts := strings.Split(definition.Name, "__")
	switch len(parts) {
	case 2:
		trusted, ok := a.trustedMCPServer(parts[0])
		return ok && trusted.gateway == "" && trusted.localOwner == server && parts[1] == tool
	case 3:
		trusted, ok := a.trustedMCPServer(parts[0])
		return ok && trusted.gateway != "" && parts[1] == server && parts[2] == tool
	default:
		return false
	}
}

// continuationDefinitionMatchesRoute keeps a continuation on the same
// transport authority as its exact source receipt. A gateway-routed action may
// use only that gateway's pin (or its exact cached lazy contract below), while
// a direct receipt may use only a trusted direct companion connection. This
// prevents an otherwise unique lookalike route from silently changing the
// identity that granted parser authority.
func continuationDefinitionMatchesRoute(a *Agent, definition llm.ToolDef, gateway, server, tool string) bool {
	if !continuationDefinitionMatches(a, definition, server, tool) {
		return false
	}
	parts := strings.Split(definition.Name, "__")
	if gateway != "" {
		return len(parts) == 3 && parts[0] == gateway
	}
	if len(parts) != 2 {
		return false
	}
	trusted, ok := a.trustedMCPServer(parts[0])
	return ok && trusted.gateway == ""
}

func continuationSchemaDigest(definition llm.ToolDef) (string, bool) {
	encoded, err := json.Marshal(definition.Parameters)
	if err != nil || len(encoded) == 0 || len(encoded) > maxTransientSchemaBytes {
		return "", false
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), true
}

func validateContinuationArguments(definition llm.ToolDef, arguments map[string]any, inputs []string) (continuationArgumentState, error) {
	if len(inputs) == 0 {
		if err := preflightMCPToolArguments(definition, arguments); err != nil {
			return continuationArgumentsInvalid, errors.New(mcpArgumentSchemaMismatchMessage)
		}
		return continuationArgumentsReady, nil
	}
	clone := cloneContinuationDefinition(definition)
	if clone.Parameters == nil {
		return continuationArgumentsInvalid, errors.New(mcpArgumentSchemaMismatchMessage)
	}
	properties, ok := clone.Parameters["properties"].(map[string]any)
	if !ok || len(properties) == 0 {
		return continuationArgumentsInvalid, errors.New(mcpArgumentSchemaMismatchMessage)
	}
	required, ok := continuationRequiredNames(clone.Parameters["required"])
	if !ok {
		return continuationArgumentsInvalid, errors.New(mcpArgumentSchemaMismatchMessage)
	}
	inputSet := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if !safeMCPRequiredPropertyName(input) {
			return continuationArgumentsInvalid, errors.New(mcpArgumentSchemaMismatchMessage)
		}
		if _, duplicate := inputSet[input]; duplicate {
			return continuationArgumentsInvalid, errors.New(mcpArgumentSchemaMismatchMessage)
		}
		if _, declared := properties[input]; !declared {
			return continuationArgumentsInvalid, errors.New(mcpArgumentSchemaMismatchMessage)
		}
		if _, supplied := arguments[input]; supplied {
			return continuationArgumentsInvalid, errors.New(mcpArgumentSchemaMismatchMessage)
		}
		inputSet[input] = struct{}{}
	}
	remainingRequired := make([]any, 0, len(required))
	for _, name := range required {
		if _, supplied := arguments[name]; supplied {
			remainingRequired = append(remainingRequired, name)
			continue
		}
		if _, named := inputSet[name]; !named {
			return continuationArgumentsInvalid, errors.New(mcpArgumentSchemaMismatchMessage)
		}
	}
	clone.Parameters["required"] = remainingRequired
	if err := preflightMCPToolArguments(clone, arguments); err != nil {
		return continuationArgumentsInvalid, errors.New(mcpArgumentSchemaMismatchMessage)
	}
	return continuationArgumentsNeedInput, nil
}

func continuationRequiredNames(value any) ([]string, bool) {
	if value == nil {
		return nil, true
	}
	items, ok := value.([]any)
	if !ok {
		if strings, ok := value.([]string); ok {
			return append([]string(nil), strings...), true
		}
		return nil, false
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok || !safeMCPRequiredPropertyName(name) {
			return nil, false
		}
		result = append(result, name)
	}
	return result, true
}

func (a *Agent) continuationWorkspaceMatches(workspace string) bool {
	root := strings.TrimSpace(a.activeWorkDir())
	if root == "" || strings.TrimSpace(workspace) != workspace || workspace == "" {
		return false
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return false
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	if resolved, resolveErr := filepath.EvalSymlinks(workspace); resolveErr == nil {
		workspace = resolved
	}
	return filepath.Clean(root) == filepath.Clean(workspace)
}

func (continuation *ValidatedContinuation) modelContext() string {
	if continuation == nil {
		return ""
	}
	payload := struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
		Inputs    []string       `json:"missing_inputs,omitempty"`
		Blocked   []string       `json:"blocked_by,omitempty"`
		Effect    string         `json:"effect"`
	}{
		Tool: continuation.Call.Name, Arguments: continuation.Call.Arguments,
		Inputs: continuation.Inputs, Blocked: continuation.BlockedBy, Effect: string(continuation.Effect),
	}
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > maxTransientSchemaBytes {
		return ""
	}
	return "Validated continuation suggestion (transient; not saved; not executed). " +
		"Use tool+arguments only; ask only for missing_inputs and respect blocked_by.\n" + string(encoded)
}
