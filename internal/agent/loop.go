package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	executionPkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/ice"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	mcpPkg "github.com/abdul-hamid-achik/sonar/internal/mcp"
)

const (
	maxToolCallsPerResponse   = 64
	maxEmptyTerminalRepairs   = 1
	emptyTerminalRepairPrompt = "\n\nThe preceding provider response ended without visible text after a tool result. Produce a concise, visible final answer grounded in the latest tool result. Do not emit reasoning without visible text."
	repeatedBuiltinCorrection = "SUPPRESSED — NOT DISPATCHED: this identical read-only built-in tool request already returned a terminal result in the current turn, and no state-changing tool has been dispatched since. Reuse the prior result or change the tool arguments."
)

func prependContinuationContext(base, continuation string) string {
	if continuation == "" {
		return base
	}
	if base == "" {
		return continuation
	}
	return continuation + "\n\n" + base
}

func reportOptionalICEError(ctx context.Context, out Output, operation string, err error) {
	if err == nil || ctx.Err() != nil || errors.Is(err, ice.ErrICESessionChanged) {
		return
	}
	out.Error(fmt.Sprintf("ICE %s failed: %v", operation, err))
}

// Run executes the ReAct loop: query -> LLM -> tool calls -> observe -> repeat.
// It streams output via the Output interface and returns a terminal error for
// headless callers and automation.
func (a *Agent) Run(ctx context.Context, out Output) error {
	turnID, err := executionPkg.NewTurnID()
	if err != nil {
		out.Error(fmt.Sprintf("execution turn identity: %v", err))
		return fmt.Errorf("execution turn identity: %w", err)
	}
	return a.RunTurn(ctx, out, turnID)
}

// RunTurn executes one ReAct turn under a caller-supplied durable identity.
// Goal Runtime consumes a continuation permit before dispatch, so the host,
// execution ledger, and settled goal receipt must share this exact ID. Callers
// that do not need correlation should use Run.
func (a *Agent) RunTurn(ctx context.Context, out Output, turnID string) error {
	return a.RunTurnWithOptions(ctx, out, turnID, TurnOptions{})
}

// RunTurnWithLimits executes one turn with host-owned hard generation and wall
// limits. It preserves RunTurn's durable identity and execution-ledger rules.
func (a *Agent) RunTurnWithLimits(ctx context.Context, out Output, turnID string, limits TurnLimits) error {
	return a.RunTurnWithOptions(ctx, out, turnID, TurnOptions{Limits: limits})
}

// RunTurnWithOptions executes one turn with a single immutable host-owned
// options snapshot.
func (a *Agent) RunTurnWithOptions(ctx context.Context, out Output, turnID string, options TurnOptions) error {
	limits := options.Limits
	if strings.TrimSpace(turnID) == "" || len(turnID) > executionPkg.MaxTurnIDBytes || !utf8.ValidString(turnID) {
		err := fmt.Errorf("execution turn identity is invalid")
		out.Error(err.Error())
		return err
	}
	if err := limits.validate(); err != nil {
		out.Error(err.Error())
		return err
	}
	a.turnMu.Lock()
	if a.closed {
		a.turnMu.Unlock()
		err := fmt.Errorf("agent is closed")
		out.Error(err.Error())
		return err
	}
	if !a.turnRunning.CompareAndSwap(false, true) {
		a.turnMu.Unlock()
		err := fmt.Errorf("another agent turn is already running")
		out.Error(err.Error())
		return err
	}
	turnFilesystem := a.pinTurnFilesystem()
	turnCtx, turnCancel := context.WithCancel(ctx)
	limitCancel := func() {}
	if deadline, ok := limits.effectiveDeadline(time.Now()); ok {
		turnCtx, limitCancel = context.WithDeadline(turnCtx, deadline)
	}
	turnDone := make(chan struct{})
	a.turnCancel = turnCancel
	a.turnDone = turnDone
	a.turnMu.Unlock()
	ctx = turnCtx
	defer func() {
		limitCancel()
		turnCancel()
		close(turnDone)
		a.turnMu.Lock()
		if a.turnDone == turnDone {
			a.turnCancel = nil
			a.turnDone = nil
		}
		a.unpinTurnFilesystem()
		a.turnRunning.Store(false)
		a.turnMu.Unlock()
	}()
	// Transient structured results exist only for provider iterations inside
	// this turn. Settle them before RunTurn returns so a future user turn,
	// checkpoint, or cursor projection can observe only the durable receipts.
	defer a.settleTransientMessages()
	defer a.mcphubResults.Reset()
	defer a.clearBobStoredAdmissions()
	defer a.clearContinuationContracts()
	// A provider such as ModelManager may expose a different context window for
	// each selected model. Resolve it exactly once so every budget decision in
	// this turn stays coherent even if selection changes concurrently.
	turnNumCtx := a.NumCtx()
	turnModel := a.llmClient.Model()
	authorityMode := a.AuthorityMode()
	modePrefix, turnToolPolicy := a.modeContext()
	execRuntime, err := a.executionRuntime(ctx)
	if err != nil {
		out.Error(err.Error())
		return err
	}
	turnICEEngine := execRuntime.iceEngine
	if turnICEEngine != nil {
		if limits.bounded() {
			// A bounded turn must not overlap an optional provider generation that
			// was launched by the previous turn. Cancel and join it before the main
			// request; the absolute deadline already covers this admission work.
			turnICEEngine.StopAutoMemory()
		} else {
			turnICEEngine.CancelAutoMemory()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if execRuntime.iceSessionID != "" {
			if err := turnICEEngine.SetSessionID(ctx, execRuntime.iceSessionID); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				reportOptionalICEError(ctx, out, "session binding", err)
				turnICEEngine = nil
			}
		}
	}

	var tools []llm.ToolDef
	var turnMCPSnapshot mcpPkg.ToolSnapshot
	if turnToolPolicy.AllowMCP && a.registry != nil {
		turnMCPSnapshot = a.mcpToolSnapshot()
		if authorityMode == AuthorityPlan {
			turnMCPSnapshot.Tools = a.filterTrustedReadOnlyMCP(turnMCPSnapshot.Tools)
		}
		tools = append(tools, turnMCPSnapshot.Tools...)
	}
	// Merge memory built-in tools if available.
	if a.memoryStore != nil {
		tools = append(tools, filterToolDefsByName(a.memoryBuiltinToolDefs(), turnToolPolicy.memoryTools)...)
	}
	// Merge built-in file tools according to the active mode policy.
	tools = append(tools, filterToolDefsByName(a.toolsBuiltinToolDefs(), turnToolPolicy.localTools)...)
	skillCatalog := a.skillCatalogPrompt()
	continuationsConfig := a.continuationsConfigSnapshot()
	bobCandidate, bobContextCached := a.reconcileBobWorkspaceContext(out)
	var bobBootstrap bobBootstrapPlan
	var bobBootstrapAvailable bool
	if turnToolPolicy.AllowMCP && !bobContextCached && continuationsConfig.Mode != config.ContinuationOff {
		bobBootstrap, bobBootstrapAvailable = a.planBobWorkspaceBootstrap(bobCandidate, turnMCPSnapshot)
	}

	// ICE context is assembled only after schemas are admitted. The current
	// user message is indexed only after a provider/host iteration succeeds.
	var iceContext string
	a.mu.RLock()
	hasMessages := len(a.messages) > 0
	var lastMsg llm.Message
	if hasMessages {
		lastMsg = a.messages[len(a.messages)-1]
	}
	a.mu.RUnlock()

	capabilityActivity := options.Capability
	if !capabilityActivity.NonTrivial && hasMessages && lastMsg.Role == "user" {
		capabilityActivity = CapabilityActivityFromPrompt(
			capabilityScopeID(execRuntime.sessionID, turnID), lastMsg.Content,
			capabilityPhaseForAuthority(authorityMode), strings.TrimSpace(turnFilesystem.workDir) != "",
		)
	}
	capabilityHintText, capabilityHint := a.resolveTurnCapabilityWithPolicy(ctx, out, capabilityActivity, turnToolPolicy.AllowMCP)
	if err := ctx.Err(); err != nil {
		return err
	}

	capabilityBaseContextWithoutBob := a.loadedCtx
	if turnToolPolicy.AllowMCP {
		if guidance := a.mcpServerGuidance(); guidance != "" {
			if capabilityBaseContextWithoutBob != "" {
				guidance += "\n\n"
			}
			capabilityBaseContextWithoutBob = guidance + capabilityBaseContextWithoutBob
		}
	}
	capabilityBaseContext := capabilityBaseContextWithoutBob
	if bobContext := a.bobWorkspaceContextPrompt(); bobContext != "" {
		capabilityBaseContext = prependContinuationContext(capabilityBaseContext, bobContext)
	}
	baseLoadedContext := composeCapabilityContext(capabilityHintText, capabilityBaseContext)
	bobBootstrapPreview := ""
	if bobBootstrapAvailable {
		bobBootstrapPreview = bobWorkspaceBootstrapHint(bobBootstrap)
		baseLoadedContext = prependContinuationContext(baseLoadedContext, bobBootstrapPreview)
	}
	loadedContext := baseLoadedContext
	var continuationPreview string
	if turnToolPolicy.AllowMCP && continuationsConfig.Mode != config.ContinuationOff &&
		(continuationsConfig.Mode != config.ContinuationAutoReadOnly || authorityMode != AuthorityAutoScoped) {
		continuationPreview = a.previewContinuationContext(options.Continuation)
		loadedContext = prependContinuationContext(baseLoadedContext, continuationPreview)
	}
	readGrants := a.ReadGrants()
	writeGrants := a.WriteGrants()
	trustedMCPHubNamespaces := a.trustedMCPHubNamespaces()
	rt := &turnRuntime{
		a:                               a,
		out:                             out,
		turnID:                          turnID,
		limits:                          limits,
		turnNumCtx:                      turnNumCtx,
		turnModel:                       turnModel,
		authorityMode:                   authorityMode,
		modePrefix:                      modePrefix,
		turnToolPolicy:                  turnToolPolicy,
		execRuntime:                     execRuntime,
		turnFilesystem:                  turnFilesystem,
		availableTools:                  append([]llm.ToolDef(nil), tools...),
		tools:                           append([]llm.ToolDef(nil), tools...),
		turnMCPSnapshot:                 turnMCPSnapshot,
		skillCatalog:                    skillCatalog,
		continuationsConfig:             continuationsConfig,
		iceEngine:                       turnICEEngine,
		iceContext:                      iceContext,
		readGrants:                      readGrants,
		writeGrants:                     writeGrants,
		trustedMCPHubNamespaces:         trustedMCPHubNamespaces,
		capabilityActivity:              capabilityActivity,
		capabilityHintText:              capabilityHintText,
		capabilityHint:                  capabilityHint,
		capabilityBaseContextWithoutBob: capabilityBaseContextWithoutBob,
		capabilityBaseContext:           capabilityBaseContext,
		baseLoadedContext:               baseLoadedContext,
		loadedContext:                   loadedContext,
	}
	if hasMessages && lastMsg.Role == "user" {
		rt.iceUserContent = lastMsg.Content
	}
	rt.rebuildSystem(ctx)

	rt.hostRefusalCounts = make(map[string]int)
	// Bind exact, completed read-only built-ins to the state observed in this
	// turn. A later non-read-only dispatch clears the set before entering its
	// backend because that call may change what a subsequent read observes.
	rt.completedBuiltinCalls = make(map[string]struct{})
	rt.continuationState = newContinuationTurnState(turnMCPSnapshot.Epoch)
	if turnToolPolicy.AllowMCP && authorityMode == AuthorityAutoScoped &&
		continuationsConfig.Mode == config.ContinuationAutoReadOnly {
		rt.autoContinuationState = newAutoContinuationState(turnMCPSnapshot.Epoch, continuationsConfig.MaxAutoSteps)
	}

	rt.maxIters = a.MaxIterationsForAuthority(authorityMode)
	if authorityMode == AuthorityAutoScoped {
		rt.autoProgress = newAutoTurnProgress()
	}

	// A process-independent turn ID threads through logs and every durable tool
	// execution identity for this turn.
	rt.lg = a.logTurn(turnID)
	rt.turnStart = time.Now()
	if rt.lg != nil {
		rt.lg.Info("turn start", "model", turnModel, "num_ctx", turnNumCtx, "tools", len(tools), "max_iters", rt.maxIters)
	}
	if rt.iceEngine != nil && hasMessages && lastMsg.Role == "user" {
		// Admit the native schema projection first, then give ICE an
		// authoritative count of system + schemas + messages. Optional retrieved
		// context can no longer consume space already reserved by the host.
		rt.admitToolSchemasForContext(ctx)
		basePromptTokens := rt.estimatedPromptTokens()
		assembled, assembleErr := rt.iceEngine.AssembleContextWithPromptTokens(ctx, lastMsg.Content, basePromptTokens)
		switch {
		case assembleErr == nil && assembled != "":
			rt.iceContext = assembled
			rt.rebuildSystem(ctx)
			if convs, facts := countICEChunks(assembled); convs > 0 || facts > 0 {
				rt.out.SystemMessage(iceRecallSummary(convs, facts))
			}
		case errors.Is(assembleErr, ice.ErrICESessionChanged):
			rt.iceEngine = nil
		case assembleErr != nil && ctx.Err() == nil && !errors.Is(assembleErr, ice.ErrICESessionChanged):
			if rt.lg != nil {
				rt.lg.Warn("ICE context assembly failed", "error", assembleErr)
			}
		}
	}

	// Compact before the next provider request as well as after responses. This
	// protects direct-answer conversations, which may never enter the tool loop,
	// from submitting an already oversized history to Ollama. Bounded turns may
	// not spend an untracked provider generation on compaction, so fail before the
	// first provider request with an explicit recovery path instead.
	//
	// Compaction admission is an eval-token accounting decision: a turn with a
	// hard generation budget may not spend an untracked summarization
	// generation. A wall-clock deadline alone does not forbid compaction — the
	// deadline naturally bounds it — so wall-limited AUTO turns still compact.
	rt.compactionForbidden = limits.MaxEvalTokens > 0
	if err := rt.admitSystemPrompt(ctx); err != nil {
		return err
	}

	// The preview above is non-consuming. Commit it only after context admission
	// succeeds; if another caller consumed or invalidated it concurrently, remove
	// the preview before the provider can observe stale host context.
	if continuationPreview != "" {
		committed := a.continuationContextText(options.Continuation)
		if committed != continuationPreview {
			rt.loadedContext = prependContinuationContext(rt.baseLoadedContext, committed)
			rt.rebuildSystem(ctx)
		}
	}

	// Claim an opaque host continuation only after prompt admission succeeds, so
	// a context-budget failure cannot burn its one-shot capability. If the exact
	// action no longer qualifies for automatic read-only execution, preserve the
	// existing suggestion path and rebuild the admitted prompt around that
	// bounded model context.
	if rt.autoContinuationState != nil {
		// An initial host read still needs one later provider iteration to
		// interpret its result. With no room, retain the LA-2 suggestion path.
		if rt.maxIters >= 2 {
			rt.queuedAutoContinuation = a.claimAutoReadOnlyContinuationContextWithSnapshot(
				options.Continuation, turnMCPSnapshot, authorityMode, rt.autoContinuationState,
			)
		}
		if rt.queuedAutoContinuation == nil {
			continuationPreview = a.previewContinuationContext(options.Continuation)
			if continuationPreview != "" {
				rt.loadedContext = prependContinuationContext(rt.baseLoadedContext, continuationPreview)
				rt.rebuildSystem(ctx)
				if err := rt.admitSystemPrompt(ctx); err != nil {
					return err
				}
				committed := a.continuationContextText(options.Continuation)
				if committed != continuationPreview {
					rt.loadedContext = prependContinuationContext(rt.baseLoadedContext, committed)
					rt.rebuildSystem(ctx)
				}
			}
		}
		// The optional host bootstrap spends provider iterations, which only a
		// hard generation budget forbids; a wall deadline bounds it naturally.
		if rt.queuedAutoContinuation == nil && bobBootstrapAvailable && rt.maxIters >= 2 && limits.MaxEvalTokens == 0 {
			rt.queuedAutoContinuation = a.prepareBobWorkspaceBootstrap(
				bobBootstrap, turnMCPSnapshot, authorityMode, rt.autoContinuationState,
			)
			if rt.queuedAutoContinuation != nil && bobBootstrapPreview != "" {
				// The host-owned read is already queued. Remove the suggestion from
				// the provider prompt so the model cannot repeat it after receiving
				// the exact result.
				rt.baseLoadedContext = composeCapabilityContext(rt.capabilityHintText, rt.capabilityBaseContext)
				rt.loadedContext = rt.baseLoadedContext
				rt.rebuildSystem(ctx)
				if err := rt.admitSystemPrompt(ctx); err != nil {
					return err
				}
			}
		}
	}
	if bobBootstrapAvailable && rt.queuedAutoContinuation == nil {
		emitContinuationSuggestion(out, turnID, 1, &ValidatedContinuation{
			Tool: "bob_context", ReasonCode: "workspace_context",
		})
	}
	for i := 0; i < rt.maxIters; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		text, toolCalls, signal, err := rt.providerStage(ctx, i)
		if err != nil {
			return err
		}
		if signal == stageNextIteration {
			continue
		}

		trackedExecutions, err := rt.recordAssistantTurn(ctx, i, text, toolCalls)
		if err != nil {
			return err
		}

		// If no tool calls, we're done.
		if len(toolCalls) == 0 {
			return rt.finishDirectResponse(ctx, i, text)
		}

		// Execute tool calls in provider order. Requested/approval/dispatch and
		// terminal transitions are committed synchronously around the backend.
		outcome, err := rt.dispatchStage(ctx, i, toolCalls, trackedExecutions)
		if err != nil {
			return err
		}
		if err := rt.settleIteration(ctx, i, len(toolCalls), outcome); err != nil {
			return err
		}
	}

	if rt.lg != nil {
		rt.lg.Warn("turn end", "reason", "max_iterations", "iters", rt.maxIters, "ms", time.Since(rt.turnStart).Milliseconds())
	}
	if checkpoint := rt.autoProgress.checkpoint(turnID, rt.maxIters, rt.totalEvalTokens, time.Since(rt.turnStart)); checkpoint != nil {
		if rt.lg != nil {
			rt.lg.Info(
				"AUTO iteration checkpoint",
				"tool_calls", checkpoint.ToolCalls,
				"successful_tools", checkpoint.SuccessfulToolCalls,
				"distinct_successful_tools", checkpoint.DistinctSuccessfulCalls,
				"eval_tokens", checkpoint.EvalTokens,
			)
		}
		return checkpoint
	}
	limitErr := fmt.Errorf("%w (%d)", ErrMaxIterations, rt.maxIters)
	out.Error(limitErr.Error())
	return limitErr
}

// ErrMaxIterations marks a turn that ended at its iteration ceiling with work
// possibly unfinished. AUTO chains a new segment when its checkpoint allows;
// interactive hosts use this to offer the human the same continuation.
var ErrMaxIterations = errors.New("reached max iterations")

type builtinToolResult struct {
	content string
	isErr   bool
}

func (a *Agent) handleBuiltinToolWithCancellation(ctx context.Context, tc llm.ToolCall, effectful bool) (string, bool) {
	if effectful {
		// Mutations must join so their final/unknown receipt reflects the real
		// backend. Cooperative cancellation is threaded to bash.
		return a.handleToolsTool(ctx, tc)
	}
	// Some filesystem syscalls cannot be interrupted by context (notably a
	// network ReadDir). Read-only calls may be abandoned safely so shutdown
	// remains bounded; the worker has no mutation authority. The slot bounds a
	// permanently blocked syscall to one worker per Agent.
	select {
	case a.readOnlySlots <- struct{}{}:
	case <-ctx.Done():
		return fmt.Sprintf("error: read-only tool %q cancelled before dispatch: %v", tc.Name, ctx.Err()), true
	}
	done := make(chan builtinToolResult, 1)
	go func() {
		defer func() { <-a.readOnlySlots }()
		content, isErr := a.handleToolsTool(ctx, tc)
		done <- builtinToolResult{content: content, isErr: isErr}
	}()
	select {
	case result := <-done:
		return result.content, result.isErr
	case <-ctx.Done():
		return fmt.Sprintf("error: read-only tool %q cancelled before completion: %v", tc.Name, ctx.Err()), true
	}
}

// countICEChunks counts the conversation and memory entries in an assembled
// ICE context string by counting "- " list items under each section header.
func countICEChunks(assembled string) (conversations, memories int) {
	section := 0 // 0=none, 1=conversations, 2=memories
	for _, line := range strings.Split(assembled, "\n") {
		switch {
		case strings.HasPrefix(line, "## Relevant Past Conversations"):
			section = 1
		case strings.HasPrefix(line, "## Remembered Facts"):
			section = 2
		case strings.HasPrefix(line, "## "):
			section = 0
		case strings.HasPrefix(line, "- ") && section == 1:
			conversations++
		case strings.HasPrefix(line, "- ") && section == 2:
			memories++
		}
	}
	return conversations, memories
}

func iceRecallSummary(conversations, memories int) string {
	switch {
	case conversations > 0 && memories > 0:
		return fmt.Sprintf("ICE · recalled %d past conversation%s · %d remembered fact%s",
			conversations, pluralSuffix(conversations), memories, pluralSuffix(memories))
	case conversations > 0:
		return fmt.Sprintf("ICE · recalled %d past conversation%s",
			conversations, pluralSuffix(conversations))
	default:
		return fmt.Sprintf("ICE · recalled %d remembered fact%s",
			memories, pluralSuffix(memories))
	}
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
