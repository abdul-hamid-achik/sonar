package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	ecosystemPkg "github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	executionPkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	mcpPkg "github.com/abdul-hamid-achik/sonar/internal/mcp"
)

// dispatchOutcome reports the per-iteration dispatch counters the settle
// phase needs.
type dispatchOutcome struct {
	preflightRejections   int
	capabilityRouteFailed bool
}

// dispatchStage executes the iteration's tool calls in provider order.
// Requested/approval/dispatch and terminal transitions are committed
// synchronously around the backend. A non-nil error stops the turn.
func (t *turnRuntime) dispatchStage(ctx context.Context, i int, toolCalls []llm.ToolCall, trackedExecutions []trackedToolExecution) (dispatchOutcome, error) {
	preflightRejections := 0
	capabilityRouteFailed := false
	for toolIndex, tc := range toolCalls {
		tracked := &trackedExecutions[toolIndex]
		if ctxErr := ctx.Err(); ctxErr != nil {
			if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex:], toolCalls[toolIndex:], t.out, ctxErr); ledgerErr != nil {
				return dispatchOutcome{}, ledgerErr
			}
			return dispatchOutcome{}, ctxErr
		}

		kind := tracked.identity.Kind
		requiresApproval := true

		// Classify policy/scope before hooks. Hooks may normalize arguments but
		// may not change identity; final arguments are hashed and approved below.
		switch kind {
		case executionPkg.KindMemory:
			if !t.turnToolPolicy.AllowsMemory(tc.Name) {
				if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventDenied, executionPkg.ApprovalPolicyDenied, "", "blocked by active mode")); err != nil {
					return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
				}
				t.a.blockedToolCall(tc, t.out)
				continue
			}
			requiresApproval = memoryToolRequiresApproval(tc.Name)
		case executionPkg.KindBuiltin:
			if !t.turnToolPolicy.AllowsBuiltin(tc.Name) {
				if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventDenied, executionPkg.ApprovalPolicyDenied, "", "blocked by active mode")); err != nil {
					return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
				}
				t.a.blockedToolCall(tc, t.out)
				continue
			}
			requiresApproval = builtinToolRequiresApproval(tc.Name)
		case executionPkg.KindMCP:
			if !t.turnToolPolicy.AllowMCP {
				if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventDenied, executionPkg.ApprovalPolicyDenied, "", "MCP blocked by active mode")); err != nil {
					return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
				}
				t.a.blockedToolCall(tc, t.out)
				continue
			}
			if !t.a.allowsMCPTool(tc.Name) {
				if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventDenied, executionPkg.ApprovalPolicyDenied, "", "blocked by active agent profile MCP scope")); err != nil {
					return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
				}
				t.a.deniedToolCall(tc, t.out, "tool call blocked by active agent profile MCP scope")
				continue
			}
			// MCP annotations are untrusted display metadata. All MCP calls
			// remain on the normal authorization path so explicit policy and
			// Skip-approval decisions retain their durable audit reason.
			requiresApproval = mcpToolRequiresApproval()
		}

		originalID, originalName := tc.ID, tc.Name
		hookCall := tc
		hookCall.Arguments = cloneApprovalArguments(tc.Arguments)
		block, reason := t.a.runPreHooks(ctx, &hookCall)
		if hookCall.ID != originalID || hookCall.Name != originalName {
			hookCall.ID, hookCall.Name = originalID, originalName
			block = true
			reason = "tool hook attempted to change approved tool identity"
		}
		// A hook may retain the pointer or argument map it received. Detach the
		// effective call immediately after the synchronous hook boundary so later
		// mutation cannot alter the hash, approval, UI, or backend dispatch.
		tc = hookCall
		tc.Arguments = cloneApprovalArguments(hookCall.Arguments)
		effectiveKind, effectiveEffect := t.a.executionKindForCall(tc)
		if effectiveKind != tracked.identity.Kind || effectiveEffect != tracked.identity.EffectClass {
			block = true
			reason = "tool hook attempted to change durable execution effect"
		}
		// A host-catalogued workspace-effectful route whose advertised schema
		// declares an optional `workspace` is pinned to the active workspace
		// here — after the hook boundary so a hook cannot observe or undo it,
		// and before the hash below so approval, the durable ledger, the UI,
		// and the backend all observe one set of bytes. Re-detach exactly as
		// the hook path does, so nothing outside this frame owns the arguments
		// that are about to be authorized.
		// A request that is already refused is never rewritten: its failure
		// receipt should record what the model asked for, not a host edit.
		if pinned, pinnedOK := t.a.pinCataloguedWorkspaceArgument(
			t.authorityMode, tc, tracked.identity.Kind, t.turnMCPSnapshot,
		); pinnedOK && !block {
			tc = pinned
			tc.Arguments = cloneApprovalArguments(pinned.Arguments)
		}
		effectiveHash, hashErr := executionPkg.HashCanonicalArguments(tc.Arguments)
		if hashErr != nil {
			reason := capToolResultForContext(fmt.Sprintf("invalid effective tool arguments: %v", hashErr), t.turnNumCtx)
			if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventFailed, executionPkg.ApprovalNotApplicable, reason, "effective argument hashing failed")); err != nil {
				return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
			}
			t.a.failedToolCall(tc, t.out, reason, t.turnNumCtx)
			continue
		}
		tracked.effectiveHash = effectiveHash
		autoContinuationStillEligible := func() bool {
			if !t.hostContinuationBatch || t.activeAutoContinuation == nil ||
				t.activeAutoContinuation.registryEpoch != t.turnMCPSnapshot.Epoch ||
				t.activeAutoContinuation.authorityVersion != t.a.approvalStateSnapshot().hostVersion ||
				!t.a.continuationFreshnessCurrent(
					&t.activeAutoContinuation.continuation, t.activeAutoContinuation.freshnessSequence,
				) ||
				!t.a.autoReadOnlyContinuationEligible(&t.activeAutoContinuation.continuation, t.turnMCPSnapshot, t.authorityMode) {
				return false
			}
			currentHash, err := executionPkg.HashCanonicalArguments(tc.Arguments)
			return err == nil && currentHash == tracked.originalHash
		}
		if t.hostContinuationBatch && !autoContinuationStillEligible() {
			block = true
			reason = "host-scheduled continuation changed or lost read-only authority before dispatch"
		}
		toolCalls[toolIndex] = tc
		if block {
			reason = capToolResultForContext(reason, t.turnNumCtx)
			if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventFailed, executionPkg.ApprovalHostRefused, reason, "host pre-tool hook refused request")); err != nil {
				return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
			}
			t.a.failedToolCall(tc, t.out, reason, t.turnNumCtx)
			if ctxErr := ctx.Err(); ctxErr != nil {
				if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex+1:], toolCalls[toolIndex+1:], t.out, ctxErr); ledgerErr != nil {
					return dispatchOutcome{}, ledgerErr
				}
				return dispatchOutcome{}, ctxErr
			}
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex:], toolCalls[toolIndex:], t.out, ctxErr); ledgerErr != nil {
				return dispatchOutcome{}, ledgerErr
			}
			return dispatchOutcome{}, ctxErr
		}

		var preflightErr error
		if tc.Name == "consult_experts" && t.expertConsultations >= 1 {
			preflightErr = errors.New("consult_experts may be dispatched at most once per parent turn")
		} else {
			preflightErr = t.a.preflightToolCall(kind, tc)
		}
		if preflightErr != nil {
			preflightRejections++
			result := capToolResultForContext(fmt.Sprintf("tool request failed preflight: %v", preflightErr), t.turnNumCtx)
			if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventFailed, executionPkg.ApprovalNotApplicable, result, "preflight rejected request before dispatch")); err != nil {
				return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
			}
			t.a.failedToolCall(tc, t.out, result, t.turnNumCtx)
			continue
		}
		if kind == executionPkg.KindBuiltin && effectiveEffect == executionPkg.EffectReadOnly {
			fingerprint := tc.Name + "\x00" + tracked.argumentsHash()
			if _, duplicate := t.completedBuiltinCalls[fingerprint]; duplicate {
				result := capToolResultForContext(repeatedBuiltinCorrection, t.turnNumCtx)
				if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(
					*tracked, executionPkg.EventFailed, executionPkg.ApprovalNotApplicable, result,
					"identical read-only built-in request suppressed before dispatch",
				)); err != nil {
					return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
				}
				t.a.failedToolCall(tc, t.out, result, t.turnNumCtx)
				continue
			}
		}

		autoApproved := requiresApproval &&
			t.a.authorityAutoApproves(t.authorityMode, tc, tracked.identity.Kind)
		if autoApproved {
			requiresApproval = false
		}
		if t.hostContinuationBatch && (!autoApproved || !autoContinuationStillEligible()) {
			result := "host-scheduled continuation no longer qualifies for automatic read-only authorization"
			if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventDenied, executionPkg.ApprovalPolicyDenied, result, "automatic continuation denied before dispatch")); err != nil {
				return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
			}
			t.a.failedToolCall(tc, t.out, result, t.turnNumCtx)
			continue
		}
		authorization := toolAuthorization{allowed: true, approval: executionPkg.ApprovalNotApplicable}
		if autoApproved {
			authorization.approval = executionPkg.ApprovalPolicy
		}
		if requiresApproval {
			var authorizationErr error
			authorization, authorizationErr = t.a.decideToolAuthorization(ctx, t.authorityMode, tc, func() error {
				// AUTO surfacing: name the scoped-shell rule this exact bash
				// request tripped, so the operator sees why it was not admitted
				// instead of a bare approval request.
				detail := "interactive approval requested"
				if tc.Name == "bash" {
					if command, ok := tc.Arguments["command"].(string); ok {
						if reason := t.a.autoCommandApprovalReason(t.authorityMode, command); reason != "" {
							detail += ": " + reason
						}
					}
				}
				return appendExecutionEvent(ctx, t.execRuntime, executionEvent(*tracked, executionPkg.EventApprovalRequested, executionPkg.ApprovalRequested, "", detail))
			})
			if authorizationErr != nil {
				return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, authorizationErr)
			}
		} else if tracked.identity.EffectClass != executionPkg.EffectReadOnly {
			// memory_recall persists LastUsed metadata but remains implicitly
			// allowed by the built-in tool policy.
			authorization.approval = executionPkg.ApprovalPolicy
		}
		if authorization.cancelled {
			cancelErr := ctx.Err()
			if cancelErr == nil {
				cancelErr = context.Canceled
			}
			result := fmt.Sprintf("CANCELLED — NOT DISPATCHED: approval for tool %q was cancelled: %s", tc.Name, authorization.reason)
			if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventCancelled, executionPkg.ApprovalCancelled, result, "interactive approval cancelled before dispatch")); err != nil {
				return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
			}
			t.a.failedToolCall(tc, t.out, result, t.turnNumCtx)
			if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex+1:], toolCalls[toolIndex+1:], t.out, cancelErr); ledgerErr != nil {
				return dispatchOutcome{}, ledgerErr
			}
			return dispatchOutcome{}, cancelErr
		}
		if !authorization.allowed {
			if authorization.hostRefused {
				code := authorization.refusalCode
				if code == "" {
					code = "host_refused"
				}
				result := capToolResultForContext(fmt.Sprintf("tool request refused by host [%s]: %s. Do not retry unchanged; change the request or approval renderer.", code, authorization.reason), t.turnNumCtx)
				if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventFailed, executionPkg.ApprovalHostRefused, result, "approval host refused request before dispatch")); err != nil {
					return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
				}
				t.a.failedToolCall(tc, t.out, result, t.turnNumCtx)
				key := tc.Name + "\x00" + tracked.argumentsHash() + "\x00" + code
				t.hostRefusalCounts[key]++
				if t.hostRefusalCounts[key] >= maxIdenticalHostRefusals {
					loopErr := &RepeatedHostRefusalError{
						ToolName:      tc.Name,
						ArgumentsHash: tracked.argumentsHash(),
						Code:          code,
						Attempts:      t.hostRefusalCounts[key],
					}
					if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex+1:], toolCalls[toolIndex+1:], t.out, loopErr); ledgerErr != nil {
						return dispatchOutcome{}, ledgerErr
					}
					t.out.Error(loopErr.Error())
					return dispatchOutcome{}, loopErr
				}
				continue
			}
			if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventDenied, authorization.approval, authorization.reason, "authorization denied before dispatch")); err != nil {
				return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
			}
			t.a.deniedToolCall(tc, t.out, "tool call denied: "+authorization.reason)
			continue
		}
		if err := appendExecutionEvent(ctx, t.execRuntime, executionEvent(*tracked, executionPkg.EventApproved, authorization.approval, "", "tool execution authorized")); err != nil {
			return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex:], toolCalls[toolIndex:], t.out, ctxErr); ledgerErr != nil {
				return dispatchOutcome{}, ledgerErr
			}
			return dispatchOutcome{}, ctxErr
		}

		if err := appendExecutionEvent(ctx, t.execRuntime, executionEvent(*tracked, executionPkg.EventStarted, executionPkg.ApprovalNotApplicable, "", "durable dispatch intent committed")); err != nil {
			return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
		}
		if tracked.identity.EffectClass != executionPkg.EffectReadOnly {
			clear(t.completedBuiltinCalls)
		}
		// Once a mutation has a durable dispatch intent, any prior Bob
		// convergence projection is stale even if the backend later fails or
		// cancellation makes the outcome unknown. Read-only and memory metadata
		// operations do not invalidate repository contract state.
		if kind != executionPkg.KindMemory && tracked.identity.EffectClass != executionPkg.EffectReadOnly {
			t.a.invalidateBobWorkspaceContext(t.out)
			// The system prompt was assembled before this durable mutation. Remove
			// both the cached Bob digest and any pre-mutation continuation hint
			// before a later provider iteration can observe stale convergence.
			t.capabilityBaseContext = t.capabilityBaseContextWithoutBob
			t.baseLoadedContext = composeCapabilityContext(t.capabilityHintText, t.capabilityBaseContext)
			t.loadedContext = t.baseLoadedContext
			t.rebuildSystem(ctx)
		}
		bobAdmission := t.a.captureBobContextAdmission(tc)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if err := t.a.cancelCommittedDispatchIntent(ctx, t.execRuntime, *tracked, tc, t.out, ctxErr, false, t.turnNumCtx); err != nil {
				t.a.cancelUndispatchedToolCalls(toolCalls[toolIndex+1:], t.out, err)
				return dispatchOutcome{}, err
			}
			if tracked.identity.EffectClass != executionPkg.EffectReadOnly && t.execRuntime.ledger != nil {
				unresolved := t.a.unresolvedFor(*tracked, executionPkg.EventOutcomeUnknown, fmt.Errorf("durable dispatch intent for tool %q has an unknown outcome", tc.Name))
				t.a.latchUnresolvedExecution(unresolved)
				if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex+1:], toolCalls[toolIndex+1:], t.out, unresolved); ledgerErr != nil {
					return dispatchOutcome{}, ledgerErr
				}
				return dispatchOutcome{}, unresolved
			}
			if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex+1:], toolCalls[toolIndex+1:], t.out, ctxErr); ledgerErr != nil {
				return dispatchOutcome{}, ledgerErr
			}
			return dispatchOutcome{}, ctxErr
		}
		t.out.ToolCallStart(tc.ID, tc.Name, cloneApprovalArguments(tc.Arguments))
		if ctxErr := ctx.Err(); ctxErr != nil {
			if err := t.a.cancelCommittedDispatchIntent(ctx, t.execRuntime, *tracked, tc, t.out, ctxErr, true, t.turnNumCtx); err != nil {
				t.a.cancelUndispatchedToolCalls(toolCalls[toolIndex+1:], t.out, err)
				return dispatchOutcome{}, err
			}
			if tracked.identity.EffectClass != executionPkg.EffectReadOnly && t.execRuntime.ledger != nil {
				unresolved := t.a.unresolvedFor(*tracked, executionPkg.EventOutcomeUnknown, fmt.Errorf("durable dispatch intent for tool %q has an unknown outcome", tc.Name))
				t.a.latchUnresolvedExecution(unresolved)
				if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex+1:], toolCalls[toolIndex+1:], t.out, unresolved); ledgerErr != nil {
					return dispatchOutcome{}, ledgerErr
				}
				return dispatchOutcome{}, unresolved
			}
			if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex+1:], toolCalls[toolIndex+1:], t.out, ctxErr); ledgerErr != nil {
				return dispatchOutcome{}, ledgerErr
			}
			return dispatchOutcome{}, ctxErr
		}
		if t.hostContinuationBatch && !autoContinuationStillEligible() {
			result := "host-scheduled continuation became stale before backend dispatch"
			if err := t.a.appendTerminalExecutionEvent(ctx, t.execRuntime, *tracked, executionEvent(*tracked, executionPkg.EventFailed, executionPkg.ApprovalPolicyDenied, result, "automatic continuation stopped after final policy check")); err != nil {
				return dispatchOutcome{}, t.a.stopBeforeDispatchAfterLedgerError(toolCalls[toolIndex:], t.out, err)
			}
			t.a.failedToolCall(tc, t.out, result, t.turnNumCtx)
			continue
		}
		startTime := time.Now()

		var result string
		var isErr bool
		var structured, errorMeta json.RawMessage
		transportErr := false
		var stopAfterTool error
		switch kind {
		case executionPkg.KindBuiltin:
			if tc.Name == "consult_experts" {
				t.expertConsultations++
				remaining := 0
				if t.limits.MaxEvalTokens > 0 {
					remaining = boundedEvalLimit(t.limits.MaxEvalTokens - t.totalEvalTokens)
				}
				var usage expertteam.Usage
				var usageErr error
				var progress expertteam.Observer
				if progressOut, ok := t.out.(ExpertProgressOutput); ok {
					progress = func(event expertteam.ProgressEvent) {
						progressOut.ExpertProgress(tc.ID, event)
					}
				}
				result, isErr, usage, usageErr = t.a.handleConsultExpertsWithBudgetAndProgress(ctx, tc.Arguments, remaining, progress)
				if usageErr != nil {
					if remaining > 0 {
						t.out.StreamDone(remaining, 0)
						t.totalEvalTokens += int64(remaining)
						stopAfterTool = fmt.Errorf("%w: expert consultation returned an invalid usage receipt; conservatively charged %d reserved token(s)", ErrTurnEvalBudgetExhausted, remaining)
					} else {
						stopAfterTool = errors.New("expert consultation usage could not be validated")
					}
					isErr = true
				} else if usage.EvalTokens > 0 || usage.PromptEvalTokens > 0 {
					if int64(usage.EvalTokens) > math.MaxInt64-t.totalEvalTokens {
						if remaining > 0 {
							t.out.StreamDone(remaining, 0)
							t.totalEvalTokens += int64(remaining)
							stopAfterTool = fmt.Errorf("%w: expert consultation usage overflowed the parent counter; conservatively charged %d reserved token(s)", ErrTurnEvalBudgetExhausted, remaining)
						} else {
							stopAfterTool = errors.New("expert consultation usage overflowed the parent turn counter")
						}
						isErr = true
					} else {
						t.out.StreamDone(usage.EvalTokens, usage.PromptEvalTokens)
						t.totalEvalTokens += int64(usage.EvalTokens)
						if t.limits.MaxEvalTokens > 0 && t.totalEvalTokens >= t.limits.MaxEvalTokens {
							stopAfterTool = fmt.Errorf("%w after expert consultation: used %d of %d", ErrTurnEvalBudgetExhausted, t.totalEvalTokens, t.limits.MaxEvalTokens)
							if t.totalEvalTokens > t.limits.MaxEvalTokens {
								isErr = true
								result = "error: expert provider exceeded the remaining evaluation-token budget\n" + result
							}
						}
					}
				}
			} else {
				result, isErr = t.a.handleBuiltinToolWithCancellation(ctx, tc, tracked.identity.EffectClass != executionPkg.EffectReadOnly)
			}
		case executionPkg.KindMemory:
			result, isErr = t.a.handleMemoryTool(tc)
		default:
			var toolResult *mcpPkg.ToolResult
			var callErr error
			// Tag the dispatch with this execution so the registry's trace line
			// joins to the lifecycle trace instead of having to be matched by
			// tool name and timing.
			mcpCtx := mcpPkg.WithCallCorrelation(ctx, tracked.identity.ExecutionID)
			if t.hostContinuationBatch {
				toolResult, callErr = t.a.registry.CallToolAtEpoch(
					mcpCtx, t.activeAutoContinuation.registryEpoch, tc.Name, cloneApprovalArguments(tc.Arguments),
				)
			} else {
				toolResult, callErr = t.a.registry.CallTool(mcpCtx, tc.Name, tc.Arguments)
			}
			if callErr != nil {
				result = mcpDispatchErrorReceipt(tc.Name, callErr)
				isErr = true
				transportErr = true
			} else if toolResult == nil {
				result, isErr = "ERROR: MCP tool returned no result", true
				transportErr = true
			} else {
				result = toolResult.Content
				isErr = toolResult.IsError
				structured = toolResult.Structured
				errorMeta = toolResult.ErrorMeta
			}
		}
		duration := time.Since(startTime)
		// Hooks are the result-redaction boundary. Apply them before the
		// durable receipt so secrets removed from UI/model text are not copied
		// into the execution ledger.
		completePostRedactionResult := t.a.runPostHooks(ctx, tc, &result, isErr)
		semanticText := result
		projection := t.a.projectSemanticToolReceipt(
			tc, semanticText, structured, errorMeta,
			transportErr, isErr, kind == executionPkg.KindBuiltin || kind == executionPkg.KindMemory,
		)
		// An explicitly requested, exact MCPHub describe result may seed the
		// bounded ephemeral schema cache used by LA-2. This never dispatches a
		// follow-up and never widens the host trust catalog.
		t.a.rememberContinuationContract(tc, projection, structured, t.turnMCPSnapshot)
		assembly := t.a.projectMCPHubResultAssembly(tc, projection, structured, isErr)
		projection = assembly.Projection
		bobAdmission = t.a.resolveBobContextAdmission(bobAdmission, projection, assembly)
		continuationReceipt := ecosystemPkg.RawReceipt{
			Text: semanticText, Structured: structured, ErrorMeta: errorMeta,
			TransportError: transportErr, ToolError: isErr,
		}
		continuationCandidates := assembly.Actions
		continuationSurface := assembly.ContinuationSurface
		sourceRouteVersion := t.a.mcpRouteVersionSnapshot()
		continuationSourceAuthorized := t.a.continuationSourceAuthorized(tc, assembly.Bound) &&
			t.a.allowsMCPTool(tc.Name) && !t.a.authorityPermissionDeniedForCall(tc)
		if !assembly.Bound {
			if continuationSourceAuthorized {
				continuationCandidates = ecosystemPkg.ProjectContinuationActions(projection, continuationReceipt)
			}
			continuationSurface = continuationSurfacePresent(projection, continuationReceipt)
		}
		var continuation *ValidatedContinuation
		if capabilityRouteOutcomeFailed(projection, isErr) &&
			t.a.markCapabilityRouteFailed(t.capabilityActivity, tc.Name, tc.Arguments, t.capabilityHint) {
			capabilityRouteFailed = true
		}
		// Typed MCP payloads stay inside the parser boundary. Only exact,
		// host-trusted, bounded projections may enter the active provider turn;
		// every durable boundary and the UI receive only the allowlisted receipt.
		modelResult, durableResult := t.a.semanticToolContents(tc, projection, result, structured, isErr)
		if assembly.Bound {
			// Stored-result pages are serialized CallToolResult fragments, not
			// downstream semantic documents. Keep every partial or rejected page
			// behind the parser boundary. A complete exact route may expose only
			// its validated, bounded model-only projection.
			durableResult = ecosystemPkg.SafeReceiptText(projection)
			modelResult = durableResult
			if assembly.Complete && assembly.Transient != "" {
				modelResult = assembly.Transient
			}
		}
		if continuationSurface {
			// Once an exact action has been normalized, do not also feed Cortex's
			// or Bob's raw command/reason payload to a small model. The bounded
			// semantic receipt plus tool+arguments contract is authoritative.
			durableResult = ecosystemPkg.SafeReceiptText(projection)
			modelResult = durableResult
		}
		answered := t.a.executionOutcomeAnswered(tc, kind, tracked.identity.EffectClass, result, transportErr, projection)
		terminalType := terminalExecutionEventType(tracked.identity.EffectClass, isErr, answered, ctx.Err())
		// Only a genuinely unverifiable outcome earns the OUTCOME UNKNOWN
		// framing. A backend that answered with a domain error keeps its
		// receipt intact so the model (and the ledger) see what it said.
		if terminalType == executionPkg.EventOutcomeUnknown && !strings.HasPrefix(durableResult, outcomeUnknownReceiptPrefix) {
			durableResult = dispatchedEffectErrorReceipt(tc.Name, durableResult, ctx.Err())
			modelResult = durableResult
		}
		// Only an ordinary unstructured result whose durable projection is
		// still the post-hook result may cross this ephemeral boundary.
		// Parser-private MCP text, action surfaces, unknown-outcome receipts,
		// and semantic replacements all fail closed.
		outputDetail := completeToolOutputDetail(
			tc.Name,
			completePostRedactionResult,
			result,
			durableResult,
			structured,
		)
		// Cap before the durable append so the terminal event hashes exactly
		// the receipt the session transcript will persist; the projection
		// boundary compares those hashes when advancing the snapshot cursor.
		durableResult = capToolResultForContext(durableResult, t.turnNumCtx)
		modelResult = capToolResultForContext(modelResult, t.turnNumCtx)
		terminalDetail := fmt.Sprintf("backend returned after %dms", duration.Milliseconds())
		if err := appendExecutionEvent(ctx, t.execRuntime, executionEvent(*tracked, terminalType, executionPkg.ApprovalNotApplicable, durableResult, terminalDetail)); err != nil {
			unresolved := t.a.unresolvedFor(*tracked, executionPkg.EventStarted, err)
			t.a.latchUnresolvedExecution(unresolved)
			unknownResult := capToolResultForContext(terminalLedgerFailureReceipt(tc.Name, err), t.turnNumCtx)
			t.out.ToolCallResult(tc.ID, tc.Name, unknownResult, true, duration)
			t.a.AppendMessage(llm.Message{Role: "tool", Content: unknownResult, ToolName: tc.Name, ToolCallID: tc.ID})
			t.a.cancelUndispatchedToolCalls(toolCalls[toolIndex+1:], t.out, unresolved)
			return dispatchOutcome{}, unresolved
		}
		t.autoProgress.settle(
			tc.Name, tracked.argumentsHash(), tracked.identity.EffectClass,
			terminalType, projection,
		)
		if kind == executionPkg.KindBuiltin && tracked.identity.EffectClass == executionPkg.EffectReadOnly {
			t.completedBuiltinCalls[tc.Name+"\x00"+tracked.argumentsHash()] = struct{}{}
		}
		t.a.settleBobContextAdmission(t.out, bobAdmission, projection, continuationReceipt, assembly, terminalType)

		// A continuation becomes schedulable only after the source outcome is
		// durably terminal. Automatic chains are single-action, successful,
		// exact-route reads and consume both a synthetic iteration and their
		// separate hard two-step budget. Preserve one additional provider
		// iteration after the read so its result can always be interpreted.
		// Every failed auto admission falls back to LA-2 suggestion mode without
		// granting authority.
		if terminalType == executionPkg.EventCompleted && ctx.Err() == nil &&
			len(toolCalls) == 1 && toolIndex == 0 && i < t.maxIters-2 && t.autoContinuationState != nil {
			t.queuedAutoContinuation = t.a.selectAutoReadOnlyContinuation(
				tc, projection, continuationCandidates, t.turnMCPSnapshot,
				continuationSourceAuthorized, sourceRouteVersion, t.authorityMode, t.autoContinuationState,
			)
		}
		if t.queuedAutoContinuation == nil && t.continuationsConfig.Mode != config.ContinuationOff {
			continuation = t.a.selectContinuation(
				tc, projection, continuationCandidates, t.turnMCPSnapshot,
				continuationSourceAuthorized, t.turnToolPolicy.AllowMCP, t.continuationState,
			)
		}
		if continuationContext := continuation.modelContext(); continuationContext != "" {
			modelResult += "\n\n" + continuationContext
			modelResult = capToolResultForContext(modelResult, t.turnNumCtx)
		}

		if t.lg != nil {
			// Correlate with the lifecycle trace in trace.go. This line used to
			// carry name/kind/ms/error and nothing else, so a failure could be
			// seen but never attributed: no execution to join on, and no reason.
			t.lg.Debug("tool dispatched",
				"execution", tracked.identity.ExecutionID,
				"name", tc.Name,
				"kind", kind,
				"effect", tracked.identity.EffectClass,
				"iter", i,
				"ms", duration.Milliseconds(),
				"error", isErr,
			)
		}
		emitSemanticToolResult(
			t.out, tc.ID, tc.Name, durableResult, structured, isErr, transportErr, duration, projection,
			outputDetail,
		)
		continuationSequence := uint64(i+1)<<32 | uint64(toolIndex+1)
		if continuation != nil || isContinuationSourceProjection(projection) {
			emitContinuationSuggestion(t.out, t.turnID, continuationSequence, continuation)
		}
		toolMessage := llm.Message{
			Role:       "tool",
			Content:    modelResult,
			ToolName:   tc.Name,
			ToolCallID: tc.ID,
		}
		if modelResult != durableResult {
			toolMessage.DurableContent = durableResult
		}
		t.a.AppendMessage(toolMessage)
		if terminalType == executionPkg.EventOutcomeUnknown && t.execRuntime.ledger != nil {
			unresolved := t.a.unresolvedFor(*tracked, executionPkg.EventOutcomeUnknown, fmt.Errorf("durable outcome for tool %q is unknown and requires explicit reconciliation", tc.Name))
			t.a.latchUnresolvedExecution(unresolved)
			if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex+1:], toolCalls[toolIndex+1:], t.out, unresolved); ledgerErr != nil {
				return dispatchOutcome{}, ledgerErr
			}
			return dispatchOutcome{}, unresolved
		}
		if stopAfterTool != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				stopAfterTool = errors.Join(ctxErr, stopAfterTool)
			}
			if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex+1:], toolCalls[toolIndex+1:], t.out, stopAfterTool); ledgerErr != nil {
				return dispatchOutcome{}, ledgerErr
			}
			t.out.Error(stopAfterTool.Error())
			return dispatchOutcome{}, stopAfterTool
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if ledgerErr := t.a.cancelTrackedToolCalls(ctx, t.execRuntime, trackedExecutions[toolIndex+1:], toolCalls[toolIndex+1:], t.out, ctxErr); ledgerErr != nil {
				return dispatchOutcome{}, ledgerErr
			}
			return dispatchOutcome{}, ctxErr
		}
	}
	return dispatchOutcome{preflightRejections: preflightRejections, capabilityRouteFailed: capabilityRouteFailed}, nil
}
