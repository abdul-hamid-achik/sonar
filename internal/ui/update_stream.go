package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// handleStreamText appends a streamed text chunk to the live buffers and
// coalesces repaints; it returns the accumulated commands slice.
func (m *Model) handleStreamText(msg StreamTextMsg, cmds []tea.Cmd) []tea.Cmd {
	if m.state == StateWaiting {
		m.state = StateStreaming
		cmds = append(cmds, m.startActivityCmd())
	}
	// Route through thinking tag parser.
	mainText, thinkText, outInThinking, outSearchBuf := processStreamChunk(
		msg.Text, m.inThinking, m.thinkSearchBuf,
	)
	m.inThinking = outInThinking
	m.thinkSearchBuf = outSearchBuf
	if mainText != "" {
		m.appendTranscriptStreamText(mainText)
		// The answer channel is offered the whole answer so far, not the
		// delta: the projection has to see complete markdown, and a fence
		// that is still open cannot be recognized from its opening line.
		m.speakAnswerDelta(m.streamBuf.String())
	}
	if thinkText != "" {
		m.appendTranscriptThinkingText(thinkText)
	}
	// Coalesce repaints to ~30fps. Fast local models emit tokens faster
	// than the terminal can usefully redraw; repainting every token wastes
	// CPU and causes flicker. StreamDoneMsg always repaints, so the final
	// partial is never dropped.
	if now := time.Now(); now.Sub(m.lastStreamPaint) >= 33*time.Millisecond {
		m.lastStreamPaint = now
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
	}
	return cmds
}

// handleStreamThinking appends a native reasoning chunk and coalesces
// repaints; it returns the accumulated commands slice.
func (m *Model) handleStreamThinking(msg StreamThinkingMsg, cmds []tea.Cmd) []tea.Cmd {
	if m.state == StateWaiting {
		m.state = StateStreaming
		cmds = append(cmds, m.startActivityCmd())
	}
	m.appendTranscriptThinkingText(msg.Text)
	if now := time.Now(); now.Sub(m.lastStreamPaint) >= 33*time.Millisecond {
		m.lastStreamPaint = now
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
	}
	return cmds
}

// handleStreamDone records the settled token counts for one model response.
func (m *Model) handleStreamDone(msg StreamDoneMsg) {
	// A model response rarely ends on a period, and a final clause held back
	// forever is the one sentence a listener most wanted. This is a SEGMENT
	// boundary, not the turn's: the loop ends one here at every tool round.
	m.speakSegmentEnd(m.streamBuf.String())
	if thinking := strings.TrimSpace(m.thinkBuf.String()); thinking != "" {
		m.speakReasoning(thinking)
	}
	m.evalCount = msg.EvalCount
	m.promptTokens = msg.PromptTokens
	m.turnEvalTotal += msg.EvalCount
	m.turnPromptTotal += msg.PromptTokens
	m.sessionEvalTotal += msg.EvalCount
	m.sessionPromptTotal += msg.PromptTokens
	// After the counts settle: the pressure verdict this speaks is computed
	// from the promptTokens line above.
	m.speakContextPressure()
}

// handleContextCompacted reconciles visible image references after the agent
// compacted its context window.
func (m *Model) handleContextCompacted(msg ContextCompactedMsg) {
	m.promptTokens = 0
	if err := m.reconcileVisibleImageProjection(m.agent.Messages()); err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Context compaction could not reconcile image references: " + err.Error()})
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
	}
}

// handleContextCompactionStarted shows the compaction status line; it
// returns the accumulated commands slice.
func (m *Model) handleContextCompactionStarted(msg ContextCompactionStartedMsg, cmds []tea.Cmd) []tea.Cmd {
	m.compactingContext = true
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
	cmds = append(cmds, m.startActivityCmd())
	return cmds
}

// handleContextCompactionFinished clears the compaction status line.
func (m *Model) handleContextCompactionFinished(msg ContextCompactionFinishedMsg) {
	m.compactingContext = false
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
}

// handleToolCallStart records a new running tool receipt and its live card;
// it returns the accumulated commands slice.
func (m *Model) handleToolCallStart(msg ToolCallStartMsg, cmds []tea.Cmd) []tea.Cmd {
	if m.goalTurnID != "" {
		m.goalTurnToolCalls++
	}
	startToolSpinner := m.state != StateStreaming && m.toolsPending == 0
	if m.state == StateWaiting {
		m.state = StateStreaming
	}
	projection := ecosystem.ProjectToolCall(msg.Name, msg.Args)
	args := agent.FormatToolArgsForTool(msg.Name, msg.Args)
	rawArgs := agent.SafeToolArgsForPersistence(msg.Name, msg.Args)
	resultLanguage := trustedResultLanguageForTool(msg.Name, msg.Args)
	collapsed := m.toolsCollapsed
	te := ToolEntry{
		ID:             msg.ID,
		Name:           msg.Name,
		Args:           args,
		RawArgs:        rawArgs,
		Status:         ToolStatusRunning,
		StartTime:      msg.StartTime,
		Collapsed:      collapsed,
		Projection:     projection,
		ResultLanguage: resultLanguage,
	}
	te.Summary = boundedToolCardSummary(toolSummary(classifyTool(msg.Name), te))
	// The activity channel reuses the label and the summary the transcript
	// itself paints, so what is heard and what is shown cannot describe the
	// same work differently. presentTool owns that vocabulary already.
	m.speakActivity(
		presentTool(msg.Name, toolCardKindForProjectedTool(msg.Name, projection), ToolCardRunning).label,
		te.Summary,
	)
	if classifyTool(msg.Name) == ToolTypeFileWrite {
		// The Adapter captured this before returning control to the tool
		// execution path. Update only installs the immutable result.
		te.BeforeContent = msg.BeforeContent
		te.BeforeSnapshotAvailable = msg.BeforeSnapshotAvailable
	}
	m.toolEntries = append(m.toolEntries, te)
	m.toolsPending++
	if startToolSpinner {
		cmds = append(cmds, m.startActivityCmd())
	}

	// Settle the assistant segment before its tool receipt so transcript order
	// remains reasoning/prose → tool. Thinking-only segments render as one
	// compact disclosure without an empty assistant block.
	m.flushStream()
	m.entries = append(m.entries, ChatEntry{
		Kind:      "tool_group",
		ToolIndex: len(m.toolEntries) - 1,
	})
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
	return cmds
}

// handleToolCallResult settles the matching running tool receipt and card;
// it returns the accumulated commands slice.
func (m *Model) handleToolCallResult(msg ToolCallResultMsg, cmds []tea.Cmd) []tea.Cmd {
	m.invalidateEntryCache()
	if m.logger != nil {
		m.logger.Info("tool call", "name", msg.Name, "duration", msg.Duration, "error", msg.IsError)
	}
	matched := false
	outputDetail := msg.OutputDetail
	if !outputDetail.Ref.Valid() || !outputDetail.Digest.Valid() {
		if outputDetail.Ref.Valid() && m.outputDetails != nil {
			m.outputDetails.Drop(outputDetail.Ref)
		}
		outputDetail = OutputDetailReceipt{}
	}
	result := boundedToolCardResult(msg.Result)
	resultDisplay := ""
	if strings.ContainsRune(msg.Result, '\x1b') {
		// Raw bytes are retained only for the render-time ANSI-16 remap; the
		// sanitized result above stays the only persisted representation.
		resultDisplay = boundedToolCardResultDisplay(msg.Result)
	}
	// Bob envelopes carry stable conflict/error codes and copy-pasteable
	// corrective commands; keep that digest visible ahead of the raw JSON.
	if digest := bobReceiptDigest(msg.Name, msg.Result); digest != "" {
		result = boundedToolCardResult(digest + "\n" + msg.Result)
	}
	var diffCmd tea.Cmd
	for i := len(m.toolEntries) - 1; i >= 0; i-- {
		if toolCallMatches(msg.ID, msg.Name, m.toolEntries[i].ID, m.toolEntries[i].Name) && m.toolEntries[i].Status == ToolStatusRunning {
			matched = true
			projection := msg.Projection.Normalize()
			if projection.Transport == "" {
				projection = ecosystem.ProjectToolResult(m.toolEntries[i].Projection, msg.Result, msg.IsError)
			}
			m.toolEntries[i].Projection = projection
			m.toolEntries[i].Result = result
			m.toolEntries[i].ResultDisplay = resultDisplay
			m.toolEntries[i].OutputDetail = outputDetail
			m.toolEntries[i].IsError = projection.Transport == ecosystem.TransportFailed || projection.Domain == ecosystem.DomainFailed
			m.toolEntries[i].Duration = msg.Duration
			if m.toolEntries[i].IsError {
				m.toolEntries[i].Status = ToolStatusError
			} else {
				m.toolEntries[i].Status = ToolStatusDone
			}
			// Successful file writes schedule the bounded post-write read and LCS
			// outside Update. The command owns only the path and pre-write bytes;
			// raw arguments and entry snapshots are cleared before Update returns.
			if classifyTool(m.toolEntries[i].Name) == ToolTypeFileWrite && projection.Successful() {
				path := toolSummary(ToolTypeFileWrite, m.toolEntries[i])
				if path != "" {
					if m.fileChanges == nil {
						m.fileChanges = make(map[string]int)
					}
					m.fileChanges[path]++
				}
				beforeAvailable := m.toolEntries[i].BeforeSnapshotAvailable || m.toolEntries[i].BeforeContent != ""
				if diffPath := diffPathFromArgs(m.toolEntries[i].RawArgs); diffPath != "" && beforeAvailable {
					m.diffGeneration++
					m.toolEntries[i].DiffPending = true
					m.toolEntries[i].DiffGeneration = m.diffGeneration
					diffCmd = buildFileDiffCmd(diffBuildRequest{
						Generation:      m.diffGeneration,
						ToolID:          m.toolEntries[i].ID,
						ToolName:        m.toolEntries[i].Name,
						Path:            diffPath,
						WorkDir:         m.agent.WorkDir(),
						Before:          m.toolEntries[i].BeforeContent,
						BeforeAvailable: beforeAvailable,
					})
				}
			}
			// Raw arguments and pre-write snapshots are needed only while the
			// call is active. Do not retain them in memory or session state.
			m.toolEntries[i].RawArgs = nil
			m.toolEntries[i].BeforeContent = ""
			m.toolEntries[i].BeforeSnapshotAvailable = false
			break
		}
	}
	if !matched {
		if outputDetail.Ref.Valid() && m.outputDetails != nil {
			m.outputDetails.Drop(outputDetail.Ref)
		}
		return cmds
	}
	var completedProjection ecosystem.ToolProjection
	for i := len(m.toolEntries) - 1; i >= 0; i-- {
		if toolCallMatches(msg.ID, msg.Name, m.toolEntries[i].ID, m.toolEntries[i].Name) {
			completedProjection = m.toolEntries[i].Projection
			break
		}
	}
	if m.goalTurnID != "" && completedProjection.Successful() {
		m.goalTurnSuccesses++
	}
	if m.toolsPending > 0 {
		m.toolsPending--
	}
	if diffCmd != nil {
		cmds = append(cmds, diffCmd)
	}
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
	return cmds
}

// handleSystemMessage appends a system notice to the transcript.
func (m *Model) handleSystemMessage(msg SystemMessageMsg) {
	m.entries = append(m.entries, ChatEntry{
		Kind:    "system",
		Content: msg.Msg,
	})
	// The first startup/recovery notice can add a fixed Settings row at
	// compact heights. Recompute the transcript allocation before painting.
	m.recalcViewportHeight()
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
}

// handleErrorMsg appends an error notice to the transcript.
func (m *Model) handleErrorMsg(msg ErrorMsg) {
	if m.logger != nil {
		m.logger.Error("error", "msg", msg.Msg)
	}
	m.entries = append(m.entries, ChatEntry{
		Kind:    "error",
		Content: msg.Msg,
	})
	m.recalcViewportHeight()
	m.refreshTranscript()
	m.gotoBottomIfFollowing()
}

// handleAgentDone settles a finished agent turn: rollback, persistence,
// goal evaluation, and queued follow-up dispatch. It returns the accumulated
// commands slice.
func (m *Model) handleAgentDone(msg AgentDoneMsg, cmds []tea.Cmd) []tea.Cmd {
	if command, handled, replacement := m.handleAutoIterationCheckpoint(msg); handled {
		if command != nil {
			cmds = append(cmds, command)
		}
		return cmds
	} else if replacement != nil {
		msg.Err = replacement
	}
	if err := m.revokeTemporaryWriteScopes(); err != nil {
		m.entries = append(m.entries, ChatEntry{
			Kind: "error", Content: "Temporary external write scope cleanup failed: " + sanitizeTerminalSingleLine(err.Error()),
		})
	}
	m.compactingContext = false
	m.capabilityRoute = nil
	if m.logger != nil {
		m.logger.Info("agent done", "eval_tokens", m.evalCount, "err", msg.Err)
	}
	var unresolved *agent.UnresolvedExecutionError
	hasUnresolved := errors.As(msg.Err, &unresolved)
	turnCancelled := errors.Is(msg.Err, context.Canceled) && !hasUnresolved
	preDispatchRejected := errors.Is(msg.Err, llm.ErrInferenceNotStarted) ||
		errors.Is(msg.Err, llm.ErrNoModelSelected) ||
		errors.Is(msg.Err, agent.ErrTurnContextBudgetExceeded)
	capturedFollowUp := false
	rolledBackPrompt := false
	if hasUnresolved || preDispatchRejected {
		capturedFollowUp = m.captureComposerFollowUpForRollback()
		rolledBackPrompt = m.rollbackPreflightRejectedPrompt()
		if rolledBackPrompt {
			m.holdQueuedFollowUpAfterRollback()
		} else if capturedFollowUp {
			// The exact pre-dispatch checkpoint could not be proven. Return the
			// temporarily separated live draft to its ordinary owner.
			m.restoreQueuedFollowUp()
		}
	}
	m.clearTurnMessageCheckpoint()
	m.flushStream()
	if turnCancelled {
		m.settleCancelledToolEntries()
	}
	m.settleGoalTurn(msg)
	if msg.Err != nil {
		m.clearContinuationAction()
	}
	if msg.Err == nil {
		m.sessionTurnCount++
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if msg.Err != nil && !turnCancelled && errors.Is(msg.Err, agent.ErrMaxIterations) {
		// The interactive counterpart of AUTO's segment chaining: the ceiling
		// is a checkpoint, not a wall. Enter on the empty composer resumes.
		m.iterationLimitContinue = true
		m.entries = append(m.entries, ChatEntry{
			Kind:    "system",
			Content: "Iteration limit reached mid-work. Press enter on the empty composer to continue where it left off, or type a new instruction.",
		})
	}
	m.lastTurnDuration = m.turnElapsed()
	// The turn is over, so this is the boundary the alert channel reports — and
	// the one place the whole turn's outcome is known. Bounded by how long it
	// ran: announcing a four-second answer says nothing the answer did not.
	m.speakTurnOutcome(m.lastTurnDuration, msg.Err != nil && !turnCancelled, turnCancelled)
	// The tab marker is the visual sibling of that spoken alert: a turn that
	// settled while the window was unfocused deserves a glance.
	if m.terminalFocusReported && !m.terminalFocused && !turnCancelled {
		m.turnUnseen = true
	}
	m.state = StateIdle
	// Upgrade the provisional first-line title with a short background model
	// naming job (workspace + user + assistant). Deliberately not gated on
	// msg.Err: sessions whose FIRST turn died — at the iteration ceiling, on a
	// cancel — are precisely the ones reopened later from the picker, where a
	// raw prompt line is least useful. Three of the four blankcode sessions
	// measured on 2026-08-08 kept their provisional titles for this reason.
	// The schedule itself still refuses when the provider is offline, and a
	// failed generation keeps the provisional title for the next retry.
	if cmd := m.scheduleSessionTitleGen(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if msg.Err != nil && !rolledBackPrompt {
		m.restoreQueuedFollowUp()
	}
	m.input.Focus()
	if m.queuedFollowUp == nil && strings.TrimSpace(m.input.Value()) == "" {
		m.input.SetHeight(1)
		m.inputLines = 1
	} else {
		m.syncInputHeight()
	}
	m.recalcViewportHeight()
	m.refreshTranscript()
	if msg.Err == nil {
		var toolReceiptsSuccessful bool
		m.lastTurnToolIndex, toolReceiptsSuccessful = m.currentTurnToolReceiptOutcome()
		if toolReceiptsSuccessful {
			// The success notice is a completion receipt, not a generic stopped
			// state; it also flashes the terminal title while active.
			// Turn completion as instrumentation (harness DX): duration + output
			// tokens when known — operators scan cost without opening /status.
			doneText := glyphSet(m.glyphProfile).Success + " Done"
			sep := glyphSeparator(m.glyphProfile)
			if m.lastTurnDuration > 0 {
				doneText += sep + formatWorkingElapsed(m.lastTurnDuration)
			}
			if m.evalCount > 0 {
				doneText += sep + "↑ " + formatTokens(m.evalCount)
			}
			// The ambient footer meter already shows used/limit persistently on
			// this same row. Repeat it in the notice only when occupancy is an
			// operational warning (ContextPctMid threshold), not as metadata.
			if m.promptTokens > 0 && m.numCtx > 0 {
				if pct := min(100, m.promptTokens*100/m.numCtx); pct >= 65 {
					doneText += sep + formatTokens(m.promptTokens) + "/" + formatTokens(m.numCtx) +
						sep + fmt.Sprintf("%d%%", pct)
				}
			}
			cmds = append(cmds, m.setFooterNotice(noticeSuccess, doneText, 3*time.Second))
		} else {
			// AgentDone without an error proves only that the provider loop
			// settled. A failed, cancelled, incomplete, or semantically
			// non-successful tool receipt must not become a global green outcome.
			// The durable ToolCard already presents the exact typed state.
			m.footerNotice = nil
		}
	} else {
		m.footerNotice = nil
		switch {
		case hasUnresolved:
			m.entries, _ = appendExecutionRecoveryNotice(m.entries, unresolved)
			m.rememberStandaloneRecovery(unresolved)
		case turnCancelled && !m.shuttingDown:
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Turn cancelled."})
		}
		m.refreshTranscript()
	}
	// Persist a lossless state snapshot after every settled attempt. Failed
	// turns may contain cancellation or unknown-outcome receipts that must
	// survive restart even though they do not count as completed turns.
	settledPersisted := m.sessionID <= 0 || m.sessionStore == nil
	// projectionAdvanced records that this same settlement pass proved every
	// post-cursor hazard is already answered in the transcript, saved that
	// transcript, and moved the snapshot cursor past it.
	projectionAdvanced := false
	if m.sessionID > 0 && m.sessionStore != nil {
		previousCursor := m.executionCursor
		var cursorErr error
		cursorStoppedAtRecovery := false
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if m.executionLease == nil {
			cursorErr = errors.New("execution session lease is unavailable; snapshot cursor was not advanced")
		} else {
			m.executionCursor, cursorErr = m.snapshotExecutionCursor(ctx)
			// An unresolved execution deliberately keeps the snapshot cursor on
			// the safe side of the effect. The transcript can still be saved at
			// that old cursor; presenting the expected boundary stop as a second
			// "Save session" failure makes one recovery condition look like data
			// loss and floods the chat with duplicate red errors.
			cursorStoppedAtRecovery = hasUnresolved && cursorErr != nil
		}
		saveErr := m.persistSessionState(ctx)
		if saveErr != nil {
			m.executionCursor = previousCursor
		} else if cursorErr == nil {
			m.agent.SetExecutionSnapshotCursor(m.executionCursor)
			projectionAdvanced = m.executionCursor > previousCursor
		}
		var usageErr error
		if saveErr == nil && msg.Err == nil {
			_, usageErr = m.sessionStore.RecordTokenUsage(ctx, db.RecordTokenUsageParams{
				SessionID: m.sessionID, Turn: int64(m.sessionTurnCount), EvalCount: int64(m.turnEvalTotal),
				PromptTokens: int64(m.turnPromptTotal), Model: m.model,
			})
		}
		cancel()
		persistErr := errors.Join(saveErr, usageErr)
		if !cursorStoppedAtRecovery {
			persistErr = errors.Join(cursorErr, persistErr)
		}
		if persistErr != nil {
			settledPersisted = false
			if m.goalRuntime != nil {
				m.goalPersistenceDirty = true
			}
			m.entries = append(m.entries, ChatEntry{Kind: "error", Content: fmt.Sprintf("Save session: %v", persistErr)})
			m.refreshTranscript()
		} else {
			settledPersisted = true
			if m.goalRuntime != nil {
				m.goalPersistenceDirty = false
			}
			if cmd := m.ensureCurrentGoalRecoveryProjection(false); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	// A projection-repair notice must not outlive its own repair. Advancing the
	// cursor here proved the flagged effect is already answered in the saved
	// transcript — exactly the state for which `sonar session repair` answers
	// "already current". Withdraw the instruction and release anything latched
	// on a repair that would be a no-op.
	if hasUnresolved && projectionAdvanced {
		m.entries, _ = downgradeExecutionRecoveryNotice(m.entries, unresolved)
		if state := m.standaloneRecovery; state != nil && !state.loading && !state.applying &&
			state.target.SessionID == unresolved.SessionID &&
			state.target.ExecutionID == unresolved.ExecutionID {
			m.standaloneRecovery = nil
		}
		m.invalidateEntryCache()
		m.refreshTranscript()
	}
	if m.goalNeedsEvaluation && !m.shuttingDown {
		if settledPersisted {
			m.footerNotice = nil
			if cmd := m.beginGoalEvaluation(false); cmd != nil {
				cmds = append(cmds, cmd)
			}
		} else if m.goalRuntime != nil {
			m.goalNeedsEvaluation = false
			if snapshot, err := m.goalRuntime.Snapshot(context.Background()); err == nil && snapshot.State == goal.StateActive {
				_ = m.goalRuntime.Pause(context.Background(), "settled goal turn could not be persisted")
			}
			m.appendGoalError("Goal continuation stopped because the settled turn was not durably saved.")
		}
	}
	if msg.Err == nil && !settledPersisted {
		// A queued follow-up may only cross a durable settlement boundary.
		// Return it to the composer when saving fails so it cannot dispatch
		// unexpectedly after some later, unrelated turn.
		m.restoreQueuedFollowUp()
		m.recalcViewportHeight()
	}
	if msg.Err == nil && settledPersisted && !m.goalNeedsEvaluation && !m.shuttingDown && m.queuedFollowUpAutoDispatchable() {
		m.footerNotice = nil
		if cmd := m.dispatchQueuedFollowUp(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	m.appendShutdownQuit(&cmds)
	return cmds
}

// currentTurnToolReceiptOutcome returns the last terminal receipt owned by the
// active logical turn and whether every receipt in that turn is semantically
// successful. An empty turn is successful. The validated start boundary keeps
// restored or historical receipts from affecting the current completion.
func (m *Model) currentTurnToolReceiptOutcome() (lastTerminal int, successful bool) {
	lastTerminal = -1
	successful = true
	start := m.turnToolStartIndex
	if start < 0 || start > len(m.toolEntries) {
		// This boundary is host-owned and set exactly when a turn is admitted.
		// Corrupt state must omit a green receipt instead of treating the
		// unknown slice as an empty successful turn.
		return lastTerminal, false
	}
	for index := start; index < len(m.toolEntries); index++ {
		entry := m.toolEntries[index]
		if entry.Status != ToolStatusRunning {
			lastTerminal = index
		}
		if entry.Status != ToolStatusDone || entry.IsError || !entry.Projection.Successful() {
			successful = false
		}
	}
	return lastTerminal, successful
}

// settleCancelledToolEntries terminates only the still-running invocations
// owned by the current turn. Cancellation is a durable lifecycle distinct from
// tool failure: transport stopped, the domain outcome is unknown, and no
// evidence may be inferred. Late results cannot overwrite this terminal state
// because result matching admits only ToolStatusRunning.
func (m *Model) settleCancelledToolEntries() int {
	start := max(0, min(m.turnToolStartIndex, len(m.toolEntries)))
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	settled := 0
	for index := start; index < len(m.toolEntries); index++ {
		entry := &m.toolEntries[index]
		if entry.Status != ToolStatusRunning {
			continue
		}
		entry.Status = ToolStatusCancelled
		entry.IsError = false
		entry.Result = cancelledToolResult
		entry.ResultDisplay = ""
		entry.ResultLanguage = ""
		if entry.OutputDetail.Ref.Valid() && m.outputDetails != nil {
			m.outputDetails.Drop(entry.OutputDetail.Ref)
		}
		entry.OutputDetail = OutputDetailReceipt{}
		entry.RawArgs = nil
		entry.BeforeContent = ""
		entry.BeforeSnapshotAvailable = false
		entry.DiffLines = nil
		entry.DiffPending = false
		entry.DiffGeneration = 0
		if !entry.StartTime.IsZero() {
			entry.Duration = min(now.Sub(entry.StartTime), maxToolViewDuration)
			if entry.Duration < 0 {
				entry.Duration = 0
			}
		}
		projection := entry.Projection.Normalize()
		if projection.Transport == "" && projection.Domain == "" {
			projection = ecosystem.ProjectToolCall(entry.Name, nil)
		}
		projection.Transport = ecosystem.TransportFailed
		projection.Domain = ecosystem.DomainUnknown
		projection.DomainTyped = false
		projection.Evidence = ecosystem.EvidenceNone
		projection.Digest = nil
		projection.Artifact = nil
		entry.Projection = projection.Normalize()
		settled++
	}
	if settled > 0 {
		m.toolsPending = max(0, m.toolsPending-settled)
		m.invalidateEntryCache()
	}
	return settled
}
