package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/capabilityadvisor"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ice"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	mcpPkg "github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/charmbracelet/log"
)

// turnStageSignal tells the RunTurnWithOptions iteration loop how to proceed
// after a stage that used to "continue" the loop inline.
type turnStageSignal int

const (
	// stageProceed continues with the next statement in the iteration body.
	stageProceed turnStageSignal = iota
	// stageNextIteration advances to the next loop iteration.
	stageNextIteration
)

// turnRuntime carries the loop-scoped state a single RunTurnWithOptions turn
// shares between its provider and dispatch stages.
type turnRuntime struct {
	a *Agent

	out    Output
	lg     *log.Logger
	turnID string
	limits TurnLimits

	turnNumCtx     int
	turnModel      string
	authorityMode  AuthorityMode
	modePrefix     string
	turnToolPolicy ToolPolicy
	execRuntime    executionRuntime
	turnFilesystem filesystemContext

	// availableTools is the immutable provider catalog captured at turn start.
	// tools is the context-admitted projection for the next provider request.
	// Keeping both lets a successful compaction restore schemas that earlier
	// pressure temporarily omitted.
	availableTools  []llm.ToolDef
	tools           []llm.ToolDef
	turnMCPSnapshot mcpPkg.ToolSnapshot
	skillCatalog    string

	continuationsConfig config.ContinuationsConfig

	iceEngine  *ice.Engine
	iceContext string
	// Persist the user message only after a provider or host-continuation
	// iteration succeeds, so every preflight failure remains reversible.
	iceUserContent string
	iceUserIndexed bool
	readGrants     []ReadGrant
	writeGrants    []WriteGrant
	// trustedMCPHubNamespaces is a host-owned turn snapshot. A remote server
	// cannot become a lazy-gateway bootstrap candidate merely by advertising
	// MCPHub-shaped operation names.
	trustedMCPHubNamespaces map[string]struct{}

	capabilityActivity              CapabilityActivity
	capabilityHintText              string
	capabilityHint                  *capabilityadvisor.Hint
	capabilityBaseContextWithoutBob string
	capabilityBaseContext           string
	baseLoadedContext               string
	loadedContext                   string

	system                     string
	emptyTerminalRepairPending bool
	compactionForbidden        bool

	maxIters     int
	autoProgress *autoTurnProgress
	turnStart    time.Time

	lastPromptTokens int
	lastEvalTokens   int
	// lastReasoning holds the chain-of-thought streamed for the current
	// iteration. DeepSeek rejects a later request whose assistant tool-call
	// message dropped its own reasoning_content, so the loop must carry it onto
	// that message rather than only streaming it to the UI. It is reset per
	// iteration and never persisted.
	lastReasoning                        string
	totalEvalTokens                      int64
	retryCount                           int
	emptyTerminalRepairs                 int
	previousIterationEndedWithToolResult bool
	malformedToolIterations              int
	capabilityReroutes                   int
	hostRefusalCounts                    map[string]int
	completedBuiltinCalls                map[string]struct{}

	continuationState      *continuationTurnState
	autoContinuationState  *autoContinuationState
	queuedAutoContinuation *preparedAutoContinuation
	hostContinuationBatch  bool
	activeAutoContinuation *preparedAutoContinuation
}

// rebuildSystem reassembles the system prompt from the turn's current bounded
// context, appending the one-shot empty-terminal repair correction when armed.
func (t *turnRuntime) rebuildSystem(ctx context.Context) {
	t.system = buildSystemPrompt(ctx, systemPromptOptions{
		ModePrefix:    t.modePrefix,
		Tools:         t.tools,
		SkillContent:  t.a.skillContent,
		VoiceHint:     t.a.voiceHint,
		SkillCatalog:  t.skillCatalog,
		LoadedContext: t.loadedContext,
		MemStore:      t.a.memoryStore,
		ICEContext:    t.iceContext,
		WorkDir:       t.turnFilesystem.workDir,
		IgnoreContent: t.turnFilesystem.ignoreContent,
		ModelName:     t.turnModel,
		NumCtx:        t.turnNumCtx,
		ReadGrants:    t.readGrants,
		WriteGrants:   t.writeGrants,
	})
	if t.emptyTerminalRepairPending {
		t.system += emptyTerminalRepairPrompt
	}
}

// rejectContextPrompt surfaces a context-budget admission failure.
func (t *turnRuntime) rejectContextPrompt(estimated int, bounded bool, attributes ...any) error {
	err := &TurnContextBudgetError{
		EstimatedPromptTokens: estimated,
		ContextWindowTokens:   t.turnNumCtx,
	}
	if t.lg != nil {
		fields := []any{"prompt_tokens", estimated, "num_ctx", t.turnNumCtx, "bounded", bounded}
		t.lg.Warn("context admission denied", append(fields, attributes...)...)
	}
	t.out.Error(err.Error())
	return err
}

// admitSystemPrompt gates the admitted prompt against the context window,
// compacting once when allowed. Compaction admission is an eval-token
// accounting decision: a turn with a hard generation budget may not spend an
// untracked summarization generation.
func (t *turnRuntime) admitSystemPrompt(ctx context.Context) error {
	t.admitToolSchemasForContext(ctx)
	estimated := t.estimatedPromptTokens()
	if !shouldCompactForContext(estimated, t.turnNumCtx) {
		return nil
	}
	if t.compactionForbidden {
		return t.rejectContextPrompt(estimated, true)
	}
	if t.lg != nil {
		t.lg.Info("compaction", "phase", "before_request", "prompt_tokens", estimated, "num_ctx", t.turnNumCtx)
	}
	if t.a.compactForContextAndModelWithICE(ctx, t.out, t.turnNumCtx, t.turnModel, t.iceEngine) {
		t.rebuildSystem(ctx)
		t.admitToolSchemasForContext(ctx)
	}
	estimated = t.estimatedPromptTokens()
	if shouldCompactForContext(estimated, t.turnNumCtx) {
		// Compaction keeps the newest messages intact. Recent oversized tool
		// receipts can still trip the threshold; shrink them before hard-fail.
		if t.a.shrinkToolResultsForContext(t.turnNumCtx) {
			estimated = t.estimatedPromptTokens()
			if !shouldCompactForContext(estimated, t.turnNumCtx) {
				return nil
			}
		}
		return t.rejectContextPrompt(estimated, false)
	}
	return nil
}

// recordAssistantTurn commits the durable requested-execution records for the
// iteration's tool calls and appends the assistant message to history.
func (t *turnRuntime) recordAssistantTurn(ctx context.Context, i int, text string, toolCalls []llm.ToolCall) ([]trackedToolExecution, error) {
	var providerCallIDs []string
	if !t.hostContinuationBatch {
		providerCallIDs = make([]string, len(toolCalls))
		for toolIndex := range toolCalls {
			providerCallIDs[toolIndex] = toolCalls[toolIndex].ID
		}
	}
	t.a.ensureToolCallIDs(toolCalls, t.turnID, i+1)
	var trackedExecutions []trackedToolExecution
	var err error
	if t.hostContinuationBatch {
		trackedExecutions, err = t.a.newTrackedContinuationExecutions(ctx, t.execRuntime, t.turnID, i+1, toolCalls)
	} else {
		trackedExecutions, err = t.a.newTrackedExecutions(ctx, t.execRuntime, t.turnID, i+1, toolCalls, providerCallIDs)
	}
	if err != nil {
		t.out.Error(fmt.Sprintf("record requested tool execution: %v", err))
		return nil, fmt.Errorf("record requested tool execution: %w", err)
	}
	t.indexICEUserMessage(ctx)

	// Record assistant message in conversation history.
	assistantToolCalls := toolCalls
	if t.hostContinuationBatch {
		assistantToolCalls = []llm.ToolCall{t.activeAutoContinuation.detachedCall()}
		assistantToolCalls[0].ID = toolCalls[0].ID
	}
	assistantMsg := llm.Message{
		Role:    "assistant",
		Content: text,
		// A tool-call turn must carry its own reasoning back on every later
		// request, or the provider rejects the next iteration.
		ReasoningContent: t.lastReasoning,
		ToolCalls:        assistantToolCalls,
		HostOwned:        t.hostContinuationBatch,
	}
	t.a.AppendMessage(assistantMsg)

	// ICE: index assistant message.
	if t.iceEngine != nil && assistantMsg.Content != "" {
		if err := t.iceEngine.IndexMessage(ctx, "assistant", assistantMsg.Content); err != nil {
			reportOptionalICEError(ctx, t.out, "assistant indexing", err)
			if errors.Is(err, ice.ErrICESessionChanged) {
				t.iceEngine = nil
			}
		}
	}
	return trackedExecutions, nil
}

func (t *turnRuntime) indexICEUserMessage(ctx context.Context) {
	if t.iceEngine == nil || t.iceUserIndexed || t.iceUserContent == "" {
		return
	}
	if err := t.iceEngine.IndexMessage(ctx, "user", t.iceUserContent); err != nil {
		reportOptionalICEError(ctx, t.out, "user indexing", err)
		if errors.Is(err, ice.ErrICESessionChanged) {
			t.iceEngine = nil
		} else {
			t.iceUserIndexed = true
		}
		return
	}
	t.iceUserIndexed = true
}

// finishDirectResponse settles a turn whose provider response carried no tool
// calls: optional auto-memory detection, final compaction, and turn-end
// logging.
func (t *turnRuntime) finishDirectResponse(ctx context.Context, i int, assistantContent string) error {
	// ICE: detect auto-memories from the exchange.
	t.a.mu.RLock()
	hasEnoughMessages := len(t.a.messages) >= 2
	var userContent string
	if hasEnoughMessages {
		for idx := len(t.a.messages) - 2; idx >= 0; idx-- {
			if t.a.messages[idx].Role == "user" {
				userContent = t.a.messages[idx].Content
				break
			}
		}
	}
	t.a.mu.RUnlock()

	// Background auto-memory is an untracked optional generation, which
	// only an eval-token budget forbids; wall-limited turns keep it.
	if t.limits.MaxEvalTokens == 0 && t.iceEngine != nil && hasEnoughMessages && userContent != "" {
		t.iceEngine.DetectAutoMemory(ctx, userContent, assistantContent)
	}
	estimatedPromptTokens := t.estimatedPromptTokens()
	if !t.compactionForbidden && shouldCompactForContext(estimatedPromptTokens, t.turnNumCtx) {
		if t.lg != nil {
			t.lg.Info("compaction", "phase", "direct_response", "prompt_tokens", estimatedPromptTokens, "num_ctx", t.turnNumCtx)
		}
		t.a.compactForContextAndModelWithICE(ctx, t.out, t.turnNumCtx, t.turnModel, t.iceEngine)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.lg != nil {
		t.lg.Info("turn end", "reason", "complete", "iters", i+1, "ms", time.Since(t.turnStart).Milliseconds())
	}
	return nil
}

// settleIteration runs the post-dispatch bookkeeping for iteration i:
// malformed-tool accounting, capability rerouting, compaction, and the
// iteration-limit warning.
func (t *turnRuntime) settleIteration(ctx context.Context, i int, toolCallCount int, outcome dispatchOutcome) error {
	if !t.hostContinuationBatch {
		if outcome.preflightRejections == toolCallCount {
			t.malformedToolIterations++
			if t.malformedToolIterations >= 2 {
				err := fmt.Errorf("%w twice consecutively; switch to a larger model or retry with explicit tool arguments", ErrMalformedToolLoop)
				// A malformed-tool spiral ends this segment, but verified distinct
				// progress earlier in it belongs to the logical turn. Let the host
				// supervisor re-prompt within its segment and digest budgets.
				if checkpoint := t.autoProgress.segmentCheckpoint(t.turnID, i+1, t.totalEvalTokens, time.Since(t.turnStart)); checkpoint != nil {
					if t.lg != nil {
						t.lg.Info("AUTO segment checkpoint after malformed tool loop", "iter", i)
					}
					return checkpoint
				}
				t.out.Error(err.Error())
				return err
			}
		} else {
			t.malformedToolIterations = 0
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if outcome.capabilityRouteFailed && t.capabilityReroutes < maxCapabilityReroutesPerTurn && i < t.maxIters-1 {
		t.capabilityReroutes++
		// Force MCPHub to reconsider: the exact advisory route already failed
		// under host policy, so a cached recommendation is not reusable.
		retryActivity := t.capabilityActivity
		retryActivity.Reconsider = true
		t.capabilityHintText, t.capabilityHint = t.a.resolveTurnCapabilityWithPolicy(ctx, t.out, retryActivity, t.turnToolPolicy.AllowMCP)
		if err := ctx.Err(); err != nil {
			return err
		}
		t.loadedContext = composeCapabilityContext(t.capabilityHintText, t.capabilityBaseContext)
		t.rebuildSystem(ctx)
		// Re-rank the immutable turn catalog around the new capability hint.
		// A route selected after a failed call may have been omitted from the
		// previous provider projection under context pressure.
		t.admitToolSchemasForContext(ctx)
	}

	// A queued host read executes before another provider request. Deferring
	// compaction avoids inserting an untracked summarization generation between
	// the source receipt and its exact continuation.
	if t.queuedAutoContinuation == nil {
		// Check if we should compact the conversation.
		estimatedPromptTokens := t.estimatedPromptTokens()
		if shouldCompactForContext(estimatedPromptTokens, t.turnNumCtx) {
			if t.compactionForbidden {
				return t.rejectContextPrompt(estimatedPromptTokens, true, "phase", "after_tools", "iter", i)
			}
			if t.lg != nil {
				t.lg.Info("compaction", "iter", i, "prompt_tokens", estimatedPromptTokens, "num_ctx", t.turnNumCtx)
			}
			compacted := t.a.compactForContextAndModelWithICE(ctx, t.out, t.turnNumCtx, t.turnModel, t.iceEngine)
			if compacted {
				// Rebuild system prompt after compaction (memory may have changed).
				t.rebuildSystem(ctx)
				// Compaction may have freed enough room for the complete catalog.
				// Re-admit from availableTools instead of carrying a stale,
				// pressure-narrowed projection into the next provider request.
				t.admitToolSchemasForContext(ctx)
			}
			estimatedPromptTokens = t.a.estimatePromptTokens(t.system, t.tools)
			if !compacted && estimatedPromptTokens < t.lastPromptTokens {
				// The provider receipt is authoritative for the unchanged prompt.
				// A failed or inapplicable compaction may not erase that floor.
				estimatedPromptTokens = t.lastPromptTokens
			}
			if receiptFloor := t.a.contextPromptFloorEstimate(t.turnModel, estimateHostPromptTokens(t.system, t.tools)); estimatedPromptTokens < receiptFloor {
				estimatedPromptTokens = receiptFloor
			}
			if shouldCompactForContext(estimatedPromptTokens, t.turnNumCtx) {
				// keepMessages leaves the newest tool receipts intact. When those
				// receipts are what blew the budget (large reads on 16k windows),
				// shrink them in place before hard-rejecting the turn. Re-estimate
				// from the shrunk history only — provider/receipt floors describe
				// the pre-shrink prompt and must not undo the recovery.
				if t.a.shrinkToolResultsForContext(t.turnNumCtx) {
					if t.lg != nil {
						t.lg.Info("tool result shrink", "phase", "after_tools", "iter", i, "num_ctx", t.turnNumCtx)
					}
					estimatedPromptTokens = t.a.estimatePromptTokens(t.system, t.tools)
					if !shouldCompactForContext(estimatedPromptTokens, t.turnNumCtx) {
						return nil
					}
				}
				return t.rejectContextPrompt(estimatedPromptTokens, false, "phase", "after_tools", "iter", i)
			}
		}
	}
	t.previousIterationEndedWithToolResult = false
	t.a.mu.RLock()
	if messageCount := len(t.a.messages); messageCount > 0 {
		t.previousIterationEndedWithToolResult = t.a.messages[messageCount-1].Role == "tool"
	}
	t.a.mu.RUnlock()

	// Interactive modes surface their deliberately tight turn ceiling. AUTO
	// has a larger host-owned safety budget and stays quiet while using it.
	if t.authorityMode != AuthorityAutoScoped && i == t.maxIters-2 {
		t.out.Error(fmt.Sprintf("approaching iteration limit (%d/%d)", i+2, t.maxIters))
	}
	return nil
}
