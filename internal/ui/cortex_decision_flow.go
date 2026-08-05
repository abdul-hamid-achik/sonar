package ui

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/goaladvisor"
)

const cortexDecisionResponder = "sonar-user"

// GoalDecisionAdvisor is optional so existing GoalAdvisor implementations and
// test fakes retain source compatibility. Answering is discovered only at the
// explicit confirmation boundary.
type GoalDecisionAdvisor interface {
	AnswerDecision(context.Context, goaladvisor.AnswerDecisionRequest) (goaladvisor.Advice, error)
}

type cortexDecisionOperationKind uint8

const (
	cortexDecisionOperationAnswer cortexDecisionOperationKind = iota + 1
	cortexDecisionOperationRefresh
)

type cortexDecisionOperation struct {
	Kind          cortexDecisionOperationKind
	Token         uint64
	Generation    uint64
	TaskID        string
	DecisionID    string
	OptionID      string
	RequestSHA256 string
}

// cortexDecisionAttempt is the safe durable no-retry fence written before an
// authority-bearing answer call. It contains only stable IDs and hashes—never
// question, label, requester, or consequence prose.
type cortexDecisionAttempt struct {
	TaskID        string `json:"task_id"`
	DecisionID    string `json:"decision_id"`
	OptionID      string `json:"selected_option_id"`
	RequestSHA256 string `json:"request_sha256"`
	Revision      int64  `json:"revision"`
}

func cloneCortexDecisionAttempt(attempt *cortexDecisionAttempt) *cortexDecisionAttempt {
	if attempt == nil {
		return nil
	}
	cloned := *attempt
	return &cloned
}

func validateRestoredCortexDecisionAttempt(attempt *cortexDecisionAttempt, snapshot *goal.Snapshot) error {
	if attempt == nil {
		return nil
	}
	if snapshot == nil || snapshot.State != goal.StateBlocked || snapshot.Blocker == nil ||
		snapshot.Blocker.Kind != goal.BlockDecision || snapshot.Blocker.Reference != attempt.TaskID ||
		snapshot.Cortex.TaskID != attempt.TaskID {
		return fmt.Errorf("cortex decision answer fence does not match the blocked goal")
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{name: "task id", value: attempt.TaskID, limit: goal.MaxCorrelationIDBytes},
		{name: "decision id", value: attempt.DecisionID, limit: maxCortexDecisionControlStableIDBytes},
		{name: "selected option id", value: attempt.OptionID, limit: maxCortexDecisionControlStableIDBytes},
	} {
		if err := validateCortexDecisionBindingText(field.name, field.value, field.limit); err != nil {
			return err
		}
	}
	if attempt.Revision < 0 || len(attempt.RequestSHA256) != 64 {
		return fmt.Errorf("cortex decision answer fence is invalid")
	}
	if _, err := hex.DecodeString(attempt.RequestSHA256); err != nil {
		return fmt.Errorf("cortex decision answer fence is invalid")
	}
	return nil
}

type cortexDecisionAnswerResultMsg struct {
	Token         uint64
	Generation    uint64
	TaskID        string
	DecisionID    string
	OptionID      string
	RequestSHA256 string
	Advice        goaladvisor.Advice
	Err           error
}

func (m *Model) cortexDecisionActive() bool {
	return m != nil && m.overlay == OverlayCortexDecision && m.cortexDecision != nil
}

func (m *Model) cortexDecisionBusyMarker() string {
	if m == nil || m.reducedMotion || m.cortexDecision == nil ||
		(!m.cortexDecision.Answering && !m.cortexDecision.Refreshing) {
		return ""
	}
	return m.spin.View()
}

func (m *Model) presentCortexDecision(advice goaladvisor.Advice) error {
	if advice.Decision == nil || !advice.PendingDecision ||
		!strings.EqualFold(strings.TrimSpace(advice.Phase), "needs_human_decision") {
		return fmt.Errorf("cortex status has no consistent typed pending decision")
	}
	presentation, err := newCortexDecisionPresentation(
		advice.TaskID, *advice.Decision, m.width, m.height, m.isDark, m.themeID, m.reducedMotion,
		m.glyphProfile,
	)
	if err != nil {
		return fmt.Errorf("prepare Cortex decision: %w", err)
	}
	m.cortexDecisionGen++
	m.cortexDecision = presentation
	m.activateCortexDecision()
	return nil
}

// activateCortexDecision applies the documented ownership order: approval,
// paste review, Cortex decision, structured forms, completion, queue/draft.
// Higher-priority inline owners keep the presentation hidden until they settle.
func (m *Model) activateCortexDecision() {
	if m == nil || m.cortexDecision == nil || m.shuttingDown ||
		m.pendingApproval != nil || m.pendingPaste != nil {
		return
	}
	m.preemptTranscriptSearch()
	anchor := m.captureInlineFormTranscriptAnchor()
	m.clearViewerModals(false)
	if m.isCompletionActive() {
		m.dismissCompletion()
	}
	// Cortex owns the inline interaction outright.
	m.planFormState = nil
	m.goalFormState = nil
	m.overlayParent = OverlayNone
	m.overlay = OverlayCortexDecision
	m.input.Blur()
	m.refreshInlineFormLayout(anchor)
}

// hideCortexDecision is presentation-only. It neither answers Cortex nor
// resolves the durable control item or local Goal blocker. An in-flight exact
// operation retains only its bounded identifiers and may still settle.
func (m *Model) hideCortexDecision() {
	if m == nil {
		return
	}
	anchor := m.captureInlineFormTranscriptAnchor()
	m.cortexDecision = nil
	if m.overlay == OverlayCortexDecision {
		m.overlay = OverlayNone
		m.overlayParent = OverlayNone
	}
	if m.composerEditable() {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	m.refreshInlineFormLayout(anchor)
}

func (m *Model) clearCortexDecisionPresentation() {
	if m == nil {
		return
	}
	anchor := m.captureInlineFormTranscriptAnchor()
	m.cortexDecision = nil
	if m.overlay == OverlayCortexDecision {
		m.overlay = OverlayNone
		m.overlayParent = OverlayNone
	}
	if m.composerEditable() {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	m.refreshInlineFormLayout(anchor)
}

func (m *Model) beginCortexDecisionAnswer(option goaladvisor.DecisionOption) tea.Cmd {
	presentation := m.cortexDecision
	if presentation == nil || m.goalRuntime == nil || m.goalOperationRunning {
		return nil
	}
	decisionAdvisor, ok := m.goalAdvisor.(GoalDecisionAdvisor)
	if !ok || decisionAdvisor == nil {
		m.markCortexDecisionOutcomeUnknown()
		return nil
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil || !cortexDecisionMatchesGoal(snapshot, presentation.TaskID) {
		m.markCortexDecisionOutcomeUnknown()
		return nil
	}
	if !cortexDecisionContainsOption(presentation.Decision, option.ID) {
		m.markCortexDecisionOutcomeUnknown()
		return nil
	}
	token, ctx, operationErr := m.beginGoalControlOperation("Recording Cortex decision")
	if operationErr != nil {
		m.markCortexDecisionOutcomeUnknown()
		return nil
	}
	m.cortexDecisionGen++
	m.cortexDecisionAttempt = &cortexDecisionAttempt{
		TaskID: presentation.TaskID, DecisionID: presentation.Decision.ID,
		OptionID: option.ID, RequestSHA256: presentation.RequestSHA256,
		Revision: snapshot.Cortex.Revision,
	}
	// Persist intent before dispatch. A crash or response loss must recover via
	// Status instead of blindly repeating an authority-bearing answer.
	if err := m.persistGoalSession(); err != nil {
		m.finishGoalOperation(token)
		m.markCortexDecisionOutcomeUnknown()
		return nil
	}
	operation := &cortexDecisionOperation{
		Kind: cortexDecisionOperationAnswer, Token: token, Generation: m.cortexDecisionGen,
		TaskID: presentation.TaskID, DecisionID: presentation.Decision.ID,
		OptionID: option.ID, RequestSHA256: presentation.RequestSHA256,
	}
	m.cortexDecisionOp = operation
	presentation.setAnswering()
	m.recalcViewportHeight()
	request := goaladvisor.AnswerDecisionRequest{
		TaskID: operation.TaskID, DecisionID: operation.DecisionID,
		OptionID: operation.OptionID, Responder: cortexDecisionResponder,
	}
	return tea.Batch(m.startActivityCmd(), func() tea.Msg {
		advice, answerErr := decisionAdvisor.AnswerDecision(ctx, request)
		return cortexDecisionAnswerResultMsg{
			Token: operation.Token, Generation: operation.Generation,
			TaskID: operation.TaskID, DecisionID: operation.DecisionID,
			OptionID: operation.OptionID, RequestSHA256: operation.RequestSHA256,
			Advice: advice, Err: answerErr,
		}
	})
}

func (m *Model) beginCortexDecisionRefresh(taskID, decisionID string) tea.Cmd {
	if m == nil || m.goalRuntime == nil || m.goalAdvisor == nil || m.goalOperationRunning {
		return nil
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil || !cortexDecisionMatchesGoal(snapshot, taskID) {
		m.markCortexDecisionOutcomeUnknown()
		return nil
	}
	token, ctx, operationErr := m.beginGoalControlOperation("Refreshing Cortex decision")
	if operationErr != nil {
		m.markCortexDecisionOutcomeUnknown()
		return nil
	}
	m.cortexDecisionGen++
	operation := &cortexDecisionOperation{
		Kind: cortexDecisionOperationRefresh, Token: token, Generation: m.cortexDecisionGen,
		TaskID: taskID, DecisionID: decisionID,
	}
	if m.cortexDecisionAttempt != nil &&
		m.cortexDecisionAttempt.TaskID == taskID && m.cortexDecisionAttempt.DecisionID == decisionID {
		operation.RequestSHA256 = m.cortexDecisionAttempt.RequestSHA256
	}
	m.cortexDecisionOp = operation
	if m.cortexDecision != nil {
		m.cortexDecision.setRefreshing()
		m.recalcViewportHeight()
	}
	advisor := m.goalAdvisor
	return tea.Batch(m.startActivityCmd(), func() tea.Msg {
		advice, refreshErr := advisor.Status(ctx, taskID)
		return goalStatusResultMsg{
			Token: token, DecisionOnly: true, DecisionRefresh: true,
			DecisionGeneration:     operation.Generation,
			ExpectedDecisionTaskID: taskID, ExpectedDecisionID: decisionID,
			ExpectedRequestSHA256: operation.RequestSHA256,
			Advice:                advice, Err: refreshErr,
		}
	})
}

func (m *Model) beginRestoredCortexDecisionRefresh() tea.Cmd {
	if m == nil || m.goalRuntime == nil || m.goalAdvisor == nil {
		return nil
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil || snapshot.State != goal.StateBlocked || snapshot.Blocker == nil ||
		snapshot.Blocker.Kind != goal.BlockDecision || snapshot.Cortex.TaskID == "" {
		return nil
	}
	decisionID := ""
	if m.cortexDecisionAttempt != nil && m.cortexDecisionAttempt.TaskID == snapshot.Cortex.TaskID {
		decisionID = m.cortexDecisionAttempt.DecisionID
	}
	return m.beginCortexDecisionRefresh(snapshot.Cortex.TaskID, decisionID)
}

func (m *Model) handleCortexDecisionAnswerResult(message cortexDecisionAnswerResultMsg) tea.Cmd {
	operation := m.cortexDecisionOp
	if operation == nil || operation.Kind != cortexDecisionOperationAnswer ||
		operation.Token != message.Token || operation.Generation != message.Generation ||
		operation.TaskID != message.TaskID || operation.DecisionID != message.DecisionID ||
		operation.OptionID != message.OptionID || operation.RequestSHA256 != message.RequestSHA256 {
		return nil
	}
	if !m.finishGoalOperation(message.Token) {
		return nil
	}
	m.cortexDecisionOp = nil
	if m.shuttingDown {
		if m.shutdownReady() {
			return tea.Quit
		}
		return nil
	}
	if message.Err != nil || !validSettledCortexDecisionAdvice(message.Advice, message.TaskID) || m.goalRuntime == nil {
		m.markCortexDecisionOutcomeUnknown()
		return nil
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil || !cortexDecisionMatchesGoal(snapshot, message.TaskID) {
		m.markCortexDecisionOutcomeUnknown()
		return nil
	}
	if err := m.resolveAnsweredCortexDecisionControlItem(
		snapshot, message.TaskID, message.DecisionID, message.OptionID, message.RequestSHA256,
	); err != nil {
		m.markCortexDecisionOutcomeUnknown()
		return nil
	}
	m.clearCortexDecisionPresentation()
	m.appendGoalSystem(fmt.Sprintf(
		"Cortex recorded decision option %s. Goal remains blocked; run /goal resume to refresh Cortex status.",
		message.OptionID,
	))
	if err := m.persistGoalSession(); err != nil {
		m.appendGoalError("Save Cortex decision receipt: " + err.Error())
	}
	return nil
}

func (m *Model) markCortexDecisionOutcomeUnknown() {
	if m.cortexDecision != nil {
		m.cortexDecision.setOutcomeUnknown()
	}
	if m.cortexDecisionActive() {
		m.input.Blur()
	}
	m.appendGoalError("Outcome unknown — refresh Cortex status")
	m.recalcViewportHeight()
	// Best-effort persistence retains the safe no-retry fence and generic
	// receipt across restart. A failure only tightens authority through the
	// existing goalPersistenceDirty gate; it never triggers another answer.
	_ = m.persistGoalSession()
}

func cortexDecisionMatchesGoal(snapshot goal.Snapshot, taskID string) bool {
	return snapshot.State == goal.StateBlocked && snapshot.Blocker != nil &&
		snapshot.Blocker.Kind == goal.BlockDecision && snapshot.Blocker.Reference == taskID &&
		snapshot.Cortex.TaskID == taskID
}

func cortexDecisionContainsOption(decision goaladvisor.PendingDecision, optionID string) bool {
	for _, option := range decision.Options {
		if option.ID == optionID {
			return true
		}
	}
	return false
}

func validSettledCortexDecisionAdvice(advice goaladvisor.Advice, taskID string) bool {
	if !advice.OK || advice.TaskID != taskID || advice.Degraded || advice.PendingDecision || advice.Decision != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(advice.Phase)) {
	case "new", "orienting", "investigating", "planned", "changing", "verifying", "persisting",
		"complete", "blocked", "abandoned":
		return true
	default:
		return false
	}
}

func validCortexDecisionStatusAdvice(advice goaladvisor.Advice, taskID string) bool {
	if !advice.OK || advice.TaskID != taskID || advice.Degraded || taskID == "" ||
		advice.PendingDecision != (advice.Decision != nil) {
		return false
	}
	wantsDecision := strings.EqualFold(strings.TrimSpace(advice.Phase), "needs_human_decision")
	if wantsDecision != (advice.Decision != nil) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(advice.Phase)) {
	case "new", "orienting", "investigating", "planned", "changing", "verifying", "persisting",
		"complete", "blocked", "abandoned", "needs_human_decision":
		return true
	default:
		return false
	}
}

func (m *Model) updateCortexDecisionKey(message tea.KeyPressMsg) tea.Cmd {
	if !m.cortexDecisionActive() {
		return nil
	}
	switch strings.ToLower(message.String()) {
	case "ctrl+c":
		return m.beginShutdown()
	case "esc":
		m.hideCortexDecision()
		return nil
	}
	presentation := m.cortexDecision
	if presentation == nil || presentation.Answering || presentation.Refreshing {
		return nil
	}
	if presentation.OutcomeUnknown {
		if strings.EqualFold(message.String(), "r") {
			return m.beginCortexDecisionRefresh(presentation.TaskID, presentation.Decision.ID)
		}
		return nil
	}
	switch strings.ToLower(message.String()) {
	case "up", "k":
		presentation.move(-1)
	case "down", "j":
		presentation.move(1)
	case "enter":
		if option, ok := presentation.confirm(); ok {
			return m.beginCortexDecisionAnswer(option)
		}
	default:
		presentation.navigateDetail(strings.ToLower(message.String()))
	}
	m.recalcViewportHeight()
	return nil
}
