package ui

import (
	"context"
	"fmt"
	"reflect"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

// sendToAgent sends a message to the agent, setting mode context first.
func (m *Model) sendToAgent(text string) tea.Cmd {
	if m.goalRuntime != nil {
		return m.rejectPromptWhileGoalAttached(text, true)
	}
	turnID, err := execution.NewTurnID()
	if err != nil {
		return m.failTurnBeforeRun(text, fmt.Sprintf("Create turn identity: %v", err))
	}
	return m.sendToAgentTurn(text, turnID)
}

// sendToAgentTurn dispatches a message under an already-reserved identity.
// Goal continuation permits are consumed before this call, so replacing the
// ID here would sever crash recovery from the execution ledger.
func (m *Model) sendToAgentTurn(text, turnID string) tea.Cmd {
	attachments := clonePendingImages(m.pendingImages)
	return m.sendToAgentTurnPresentedWithAttachments(text, turnID, true, agent.TurnLimits{}, m.mode, agent.CapabilityActivity{}, nil, attachments)
}

func (m *Model) sendGoalToAgentTurn(text, turnID string, limits agent.TurnLimits) tea.Cmd {
	// A durable Goal Runtime owns its execution authority independently from the
	// conversational mode selector. Shift+Tab may prepare the user's eventual
	// post-goal mode, but it must never downgrade or otherwise mutate an already
	// admitted goal turn's tool contract.
	return m.sendToAgentTurnPresentedWithMode(text, turnID, false, limits, ModeAuto)
}

func (m *Model) sendGoalToAgentTurnWithCapability(
	text, turnID string,
	limits agent.TurnLimits,
	capability agent.CapabilityActivity,
	continuation *agent.ContinuationContext,
) tea.Cmd {
	return m.sendToAgentTurnPresentedWithAttachments(text, turnID, false, limits, ModeAuto, capability, continuation, nil)
}

func (m *Model) sendToAgentTurnPresentedWithMode(text, turnID string, visible bool, limits agent.TurnLimits, authority Mode) tea.Cmd {
	return m.sendToAgentTurnPresentedWithCapability(text, turnID, visible, limits, authority, agent.CapabilityActivity{})
}

func (m *Model) sendToAgentTurnPresentedWithCapability(text, turnID string, visible bool, limits agent.TurnLimits, authority Mode, capability agent.CapabilityActivity) tea.Cmd {
	return m.sendToAgentTurnPresentedWithAttachments(text, turnID, visible, limits, authority, capability, nil, nil)
}

func (m *Model) sendToAgentTurnPresentedWithAttachments(
	text, turnID string,
	visible bool,
	limits agent.TurnLimits,
	authority Mode,
	capability agent.CapabilityActivity,
	continuation *agent.ContinuationContext,
	attachments []pendingImageAttachment,
) tea.Cmd {
	messagesBeforeTurn := m.agent.Messages()
	if err := validateImageConversationBudget(messagesBeforeTurn, attachmentRefs(attachments)); err != nil {
		return m.failPresentedTurnBeforeRun(text, "Attach images: "+err.Error(), visible)
	}
	visionRequired := len(attachments) > 0 || messagesRequireVision(messagesBeforeTurn)
	if visionRequired {
		if err := m.ensureVisionModel(); err != nil {
			return m.failPresentedTurnBeforeRun(text, "Attach images: "+err.Error(), visible)
		}
	}
	if len(attachments) > 0 {
		m.pendingImages = nil
		m.turnImages = clonePendingImages(attachments)
		m.recalcViewportHeight()
	}
	m.cancelSessionLoad()
	m.cancelSessionList()
	// Background session-title generation shares ordinary inference admission;
	// yield it before a foreground turn acquires the lease.
	m.cancelSessionTitleGen()
	m.turnMessagesBefore = append([]llm.Message(nil), messagesBeforeTurn...)
	m.turnPromptFloor = m.agent.ContextPromptFloor()
	m.turnPrompt = text
	m.turnPromptVisible = visible
	m.turnEntryIndex = -1
	m.turnCheckpointSet = true
	createdSession := false
	if authority < ModeNormal || authority > ModeAuto {
		authority = ModeNormal
	}
	cfg := m.modeConfigs[authority]
	if m.logger != nil {
		m.logger.Info("user message", "mode", cfg.Label, "length", len(text))
	}

	m.resumeFollow()
	m.state = StateWaiting
	// Ordinary active turns keep the Bubbles textarea focused so real terminal
	// key events can draft and queue a follow-up. Rendering an editable-looking
	// composer while the child is blurred makes the queue affordance inert.
	// Goal-owned turns still reject child updates through composerEditable.
	m.input.Focus()
	m.turnStartedAt = m.nowTime()
	m.resetTurnDiagnostics()
	m.beginContinuationTurn(turnID)
	m.turnToolStartIndex = len(m.toolEntries)
	m.recalcViewportHeight()
	m.resetTranscriptStreamText()

	if visible {
		m.entries = append(m.entries, ChatEntry{
			Kind:        "user",
			Content:     text,
			Attachments: attachmentRefs(attachments),
		})
		m.turnEntryIndex = len(m.entries) - 1
	}
	m.refreshTranscript()
	m.gotoBottomIfFollowing()

	var sessionErr error
	sessionTitleSource := text
	if text == "Analyze the attached image." && len(attachments) > 0 {
		sessionTitleSource = imageOnlySessionTitle(attachments)
	}
	createdSession, sessionErr = m.ensureExecutionSession(sessionTitleSource, cfg.Label)
	if sessionErr != nil {
		return m.failPresentedTurnBeforeRun(text, sessionErr.Error(), visible)
	}

	// Set mode context on the agent.
	m.setRouterMode(cfg.RouterMode)
	if !visionRequired && !m.modelPinned && m.router != nil && m.modelManager != nil {
		if newModel := m.router.SelectModelForMode(text, cfg.RouterMode); newModel != "" && newModel != m.model {
			m.prepareModelSwitch()
			if err := m.modelManager.SetCurrentModel(newModel); err == nil {
				m.setCurrentModelProjection(newModel)
			} else {
				m.entries = append(m.entries, ChatEntry{
					Kind:    "error",
					Content: fmt.Sprintf("Failed to switch routed model: %v", err),
				})
				m.refreshTranscript()
				m.gotoBottomIfFollowing()
			}
		}
	}
	if err := m.agent.AddUserMessageWithImages(text, attachmentData(attachments)); err != nil {
		rollbackErr := m.agent.RestoreMessagesWithinSession(messagesBeforeTurn, m.turnPromptFloor)
		var cleanupErr error
		if createdSession {
			cleanupErr = m.discardCreatedExecutionSession()
		}
		message := fmt.Sprintf("Attach images: %v", err)
		if rollbackErr != nil {
			message = fmt.Sprintf("%s (rollback: %v)", message, rollbackErr)
		}
		if cleanupErr != nil {
			message = fmt.Sprintf("%s (cleanup: %v)", message, cleanupErr)
		}
		return m.failPresentedTurnBeforeRun(text, message, visible)
	}
	m.agent.SetModeContext(cfg.SystemPromptPrefix, cfg.ToolPolicy)
	m.agent.SetAuthorityMode(agentAuthorityMode(authority))
	if m.sessionID > 0 && m.sessionStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := m.persistSessionState(ctx)
		cancel()
		if err != nil {
			rollbackErr := m.agent.RestoreMessagesWithinSession(messagesBeforeTurn, m.turnPromptFloor)
			if createdSession {
				if cleanupFailure := m.discardCreatedExecutionSession(); cleanupFailure != nil {
					return m.failPresentedTurnBeforeRun(text, fmt.Sprintf("Save session: %v (cleanup: %v)", err, cleanupFailure), visible)
				}
			}
			if rollbackErr != nil {
				return m.failPresentedTurnBeforeRun(text, fmt.Sprintf("Save session: %v (rollback: %v)", err, rollbackErr), visible)
			}
			return m.failPresentedTurnBeforeRun(text, fmt.Sprintf("Save session: %v", err), visible)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	options := agent.TurnOptions{
		Limits: limits, Capability: capability, Continuation: continuation,
	}
	autoSegments, autoWallTime := m.autoTurnCeilings()
	options.Limits = defaultPlainAutoTurnLimits(options.Limits, authority, autoWallTime)
	options.Limits = normalizeLogicalTurnLimits(options.Limits, m.nowTime())
	m.turnRunContext = ctx
	m.turnRunOptions = options
	m.turnLogicalID = turnID
	m.turnSegmentID = turnID
	m.turnAuthority = authority
	m.autoCheckpoints.reset(turnID, m.turnStartedAt, autoSegments, autoWallTime)

	p := m.program

	// Set up the approval callback so tool permission prompts go through the TUI.
	m.agent.SetApprovalCallback(func(req permission.ApprovalRequest) {
		p.Send(ToolApprovalMsg{
			RequestID:       req.RequestID,
			ToolName:        req.ToolName,
			Args:            req.Args,
			ArgumentsSHA256: req.ArgumentsSHA256,
			Preview:         req.Preview,
			Scope:           req.Scope,
			Response:        req.Response,
		})
	})

	runAgent := newAgentSegmentCmd(
		m.agent, p, m.outputDetails, ctx, turnID, turnID, options,
	)

	m.scramble.Reset()
	return tea.Batch(m.startActivityCmd(), runAgent)
}

func (m *Model) failPresentedTurnBeforeRun(text, message string, visible bool) tea.Cmd {
	presented := m.turnCheckpointSet
	m.restoreTurnImages()
	if visible && presented {
		m.removePresentedTurnEntry()
	}
	m.clearTurnMessageCheckpoint()
	if visible {
		return m.failTurnBeforeRun(text, message)
	}
	m.entries = append(m.entries, ChatEntry{Kind: "error", Content: message})
	m.state = StateIdle
	m.input.Focus()
	m.syncInputHeight()
	m.recalcViewportHeight()
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.resumeFollow()
	return nil
}

func (m *Model) rollbackPreflightRejectedPrompt() bool {
	if m == nil || m.agent == nil || !m.turnCheckpointSet {
		return false
	}
	current := m.agent.Messages()
	before := m.turnMessagesBefore
	if len(current) != len(before)+1 || (len(before) > 0 && !reflect.DeepEqual(current[:len(before)], before)) {
		return false
	}
	last := current[len(current)-1]
	if last.Role != "user" || last.Content != m.turnPrompt || len(last.ToolCalls) != 0 || last.ToolName != "" || last.ToolCallID != "" {
		return false
	}
	if err := m.agent.RestoreMessagesWithinSession(append([]llm.Message(nil), before...), m.turnPromptFloor); err != nil {
		if m.logger != nil {
			m.logger.Error("restore preflight-rejected prompt", "err", err)
		}
		return false
	}
	m.restoreTurnImages()
	if m.turnPromptVisible {
		m.removePresentedTurnEntry()
		m.input.SetValue(m.turnPrompt)
		m.input.CursorEnd()
		_ = m.reflowInputViewport()
		m.invalidateEntryCache()
	}
	return true
}

// removePresentedTurnEntry removes the exact user row admitted for the active
// turn. Provider pre-dispatch errors are delivered before AgentDone, so the
// row is not necessarily the last transcript entry by the time rollback runs.
func (m *Model) removePresentedTurnEntry() bool {
	if m == nil || !m.turnPromptVisible {
		return false
	}
	index := m.turnEntryIndex
	if index < 0 || index >= len(m.entries) {
		return false
	}
	entry := m.entries[index]
	if entry.Kind != "user" || entry.Content != m.turnPrompt {
		return false
	}
	m.entries = append(m.entries[:index], m.entries[index+1:]...)
	m.invalidateEntryCache()
	return true
}

func (m *Model) clearTurnMessageCheckpoint() {
	if m == nil {
		return
	}
	m.turnMessagesBefore = nil
	m.turnPromptFloor = agent.ContextPromptFloor{}
	m.turnPrompt = ""
	m.turnPromptVisible = false
	m.turnEntryIndex = -1
	m.turnCheckpointSet = false
	m.turnImages = nil
	m.turnRunContext = nil
	m.turnRunOptions = agent.TurnOptions{}
	m.turnLogicalID = ""
	m.turnSegmentID = ""
	m.turnAuthority = ModeNormal
	m.autoCheckpoints.clear()
}

func (m *Model) failTurnBeforeRun(text, message string) tea.Cmd {
	_ = m.revokeTemporaryWriteScopes()
	m.entries = append(m.entries, ChatEntry{Kind: "error", Content: message})
	m.state = StateIdle
	m.input.SetValue(text)
	m.input.CursorEnd()
	m.input.Focus()
	_ = m.reflowInputViewport()
	m.recalcViewportHeight()
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.resumeFollow()
	return nil
}
