package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/goaladvisor"
	"github.com/abdul-hamid-achik/sonar/internal/supervisor"
)

const (
	defaultGoalContinuationBudget int64 = 8
	defaultGoalTokenBudget        int64 = 12_000
	defaultGoalTimeBudget               = 30 * time.Minute
	goalAdvisorTimeout                  = 30 * time.Second
	goalActor                           = "sonar"
)

// GoalAdvisor is the semantic Cortex seam. Implementations may discover it
// directly or through MCPHub. Typed continuation suggestions are owned by the
// agent's exact registry/schema/policy interpreter, never this UI adapter.
type GoalAdvisor interface {
	Open(context.Context, goaladvisor.OpenRequest) (goaladvisor.Advice, error)
	Status(context.Context, string) (goaladvisor.Advice, error)
}

type goalOpenResultMsg struct {
	Token  uint64
	Manual bool
	Advice goaladvisor.Advice
	Err    error
}

type goalStatusResultMsg struct {
	Token                  uint64
	Manual                 bool
	DecisionOnly           bool
	DecisionRefresh        bool
	DecisionGeneration     uint64
	ExpectedDecisionTaskID string
	ExpectedDecisionID     string
	ExpectedRequestSHA256  string
	Advice                 goaladvisor.Advice
	Err                    error
}

// SetGoalAdvisor wires Cortex after the parent has built the MCP registry. It
// is safe to call before MCP startup; tool discovery happens per operation.
func (m *Model) SetGoalAdvisor(advisor GoalAdvisor) {
	m.goalAdvisor = advisor
}

func defaultGoalFormValues(objective string) GoalFormValues {
	return GoalFormValues{
		Objective:   strings.TrimSpace(objective),
		TurnBudget:  defaultGoalContinuationBudget,
		TokenBudget: defaultGoalTokenBudget,
		TimeBudget:  defaultGoalTimeBudget,
	}
}

func goalFormValuesFromSnapshot(snapshot goal.Snapshot) GoalFormValues {
	criteria := make([]string, 0, len(snapshot.AcceptanceCriteria))
	for _, criterion := range snapshot.AcceptanceCriteria {
		criteria = append(criteria, criterion.Description)
	}
	return GoalFormValues{
		Objective:          snapshot.Objective,
		AcceptanceCriteria: strings.Join(criteria, "\n"),
		TurnBudget:         snapshot.Budget.MaxContinuationTurns,
		TokenBudget:        snapshot.Budget.MaxEvalTokens,
		TimeBudget:         snapshot.Budget.MaxWallTime,
	}
}

func (m *Model) openGoalForm(objective string, budgetOnly bool) error {
	return m.openGoalFormInternal(objective, budgetOnly)
}

func (m *Model) openGoalRequestForm(request command.GoalRequest) (tea.Cmd, error) {
	anchor := m.captureInlineFormTranscriptAnchor()
	budget := goal.BudgetLimits{
		MaxContinuationTurns: defaultGoalContinuationBudget,
		MaxEvalTokens:        defaultGoalTokenBudget,
		MaxWallTime:          defaultGoalTimeBudget,
	}
	if request.TimeExplicit {
		budget = goal.BudgetLimits{MaxWallTime: request.TimeBudget}
	}
	draft, err := goal.InferDraft(request.Prompt, budget)
	if err != nil {
		return nil, fmt.Errorf("infer goal draft: %w", err)
	}
	values := GoalFormValues{
		Objective:          draft.Objective,
		AcceptanceCriteria: strings.Join(draft.AcceptanceCriteria, "\n"),
		TurnBudget:         draft.Budget.MaxContinuationTurns,
		TokenBudget:        draft.Budget.MaxEvalTokens,
		TimeBudget:         draft.Budget.MaxWallTime,
	}
	followUp := draft.FollowUpPrompt
	followUpField := GoalFieldObjective
	if !draft.NeedsFollowUp && !request.TimeExplicit {
		followUp = fmt.Sprintf("Confirm the %s AUTO time limit, or enter another duration.", formatGoalTimeInput(defaultGoalTimeBudget))
		followUpField = GoalFieldTime
	}
	if !m.prepareInlineFormOpen() {
		return nil, fmt.Errorf("another composer-owned surface is active")
	}
	m.goalFormState = NewGoalForm(values, GoalFormOptions{
		Width: m.width, Height: m.height, IsDark: m.isDark,
		ReducedMotion: m.reducedMotion, DraftFromPrompt: true, FollowUpPrompt: followUp,
		GlyphProfile: m.glyphProfile,
	})
	if followUp != "" {
		m.goalFormState.focusField(followUpField)
	} else {
		m.goalFormState.focusField(GoalFieldActions)
	}
	m.overlayParent = OverlayNone
	m.overlay = OverlayGoalForm
	m.input.Blur()
	m.refreshInlineFormLayout(anchor)

	// `/goal <duration> <prompt>` is itself the explicit creation intent. When
	// the deterministic draft found a concrete target, start it directly; the
	// transient form remains available if persistence or recovery preflight
	// needs human correction. Ambiguous prompts ask exactly one contextual
	// follow-up instead.
	if request.TimeExplicit && !draft.NeedsFollowUp {
		return m.applyGoalFormWithAuthority(GoalFormEvent{Action: GoalActionSave, Values: values}, true), nil
	}
	return nil, nil
}

func (m *Model) openGoalFormInternal(objective string, budgetOnly bool) error {
	anchor := m.captureInlineFormTranscriptAnchor()
	values := defaultGoalFormValues(objective)
	if budgetOnly {
		if m.goalRuntime == nil {
			return fmt.Errorf("no goal is configured; create one with /goal new")
		}
		snapshot, err := m.goalRuntime.Snapshot(context.Background())
		if err != nil {
			return fmt.Errorf("read goal: %w", err)
		}
		if snapshot.State.Terminal() {
			return fmt.Errorf("goal is %s and its budget can no longer be changed", snapshot.State)
		}
		values = goalFormValuesFromSnapshot(snapshot)
	}
	if !m.prepareInlineFormOpen() {
		return fmt.Errorf("another composer-owned surface is active")
	}
	m.goalFormState = NewGoalForm(values, GoalFormOptions{
		Width: m.width, Height: m.height, IsDark: m.isDark,
		ReducedMotion: m.reducedMotion, BudgetOnly: budgetOnly,
		GlyphProfile: m.glyphProfile,
	})
	m.overlayParent = OverlayNone
	m.overlay = OverlayGoalForm
	m.input.Blur()
	m.refreshInlineFormLayout(anchor)
	return nil
}

func (m *Model) closeGoalForm() {
	anchor := m.captureInlineFormTranscriptAnchor()
	m.goalFormState = nil
	if m.overlay == OverlayGoalForm {
		m.overlay = OverlayNone
	}
	if m.composerEditable() {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	m.refreshInlineFormLayout(anchor)
}

func (m *Model) applyGoalForm(event GoalFormEvent) tea.Cmd {
	return m.applyGoalFormWithAuthority(event, false)
}

func (m *Model) applyGoalFormWithAuthority(event GoalFormEvent, explicitGoalCommand bool) tea.Cmd {
	if event.Action != GoalActionSave || m.goalFormState == nil {
		return nil
	}
	anchor := m.captureInlineFormTranscriptAnchor()
	defer m.refreshInlineFormLayout(anchor)
	budgetOnly := m.goalFormState.BudgetOnly()
	values := event.Values
	if !budgetOnly && m.mode == ModePlan && !explicitGoalCommand {
		m.goalFormState.SetError("PLAN is read-only. Use AUTO to save this goal.")
		return nil
	}
	if budgetOnly {
		if m.goalRuntime == nil {
			m.appendGoalError("No goal is configured.")
			return nil
		}
		before, err := m.goalRuntime.Snapshot(context.Background())
		if err != nil {
			m.appendGoalError("Read goal budgets: " + err.Error())
			return nil
		}
		if err := m.goalRuntime.AmendBudget(context.Background(), goal.BudgetLimits{
			MaxContinuationTurns: values.TurnBudget,
			MaxEvalTokens:        values.TokenBudget,
			MaxWallTime:          values.TimeBudget,
		}, "user updated goal budgets"); err != nil {
			m.appendGoalError("Update goal budgets: " + err.Error())
			return nil
		}
		if err := m.persistGoalSession(); err != nil {
			rollbackErr := m.restoreGoalSnapshot(before)
			m.appendGoalError("Save goal budgets: " + errors.Join(err, rollbackErr).Error())
			return nil
		}
		m.closeGoalForm()
		m.appendGoalSystem("Goal budgets updated. Run /goal resume when you are ready to continue.")
		return nil
	}
	if err := m.blockGoalCreationDuringStandaloneRecovery(); err != nil {
		m.goalFormState.SetError(err.Error())
		return nil
	}

	if m.goalRuntime != nil {
		current, err := m.goalRuntime.Snapshot(context.Background())
		if err != nil {
			m.appendGoalError("Read goal: " + err.Error())
			return nil
		}
		if !current.State.Terminal() {
			m.appendGoalError("A goal is already in progress. Pause or drop it before creating another.")
			return nil
		}
	}

	createdSession, err := m.ensureExecutionSession(values.Objective, m.modeConfigs[ModeAuto].Label)
	if err != nil {
		m.appendGoalError(err.Error())
		return nil
	}
	if m.sessionStore == nil || m.sessionID <= 0 {
		m.appendGoalError("Goal Runtime requires durable session storage.")
		return nil
	}

	criteria := make([]goal.AcceptanceCriterion, 0, len(values.CriterionDescriptions()))
	for index, description := range values.CriterionDescriptions() {
		criteria = append(criteria, goal.AcceptanceCriterion{
			ID: fmt.Sprintf("criterion_%d", index+1), Description: description,
		})
	}
	previous := m.goalRuntime
	runtime, err := goal.New(goal.Spec{
		SessionID: m.sessionID, Objective: values.Objective,
		AcceptanceCriteria: criteria,
		Budget: goal.BudgetLimits{
			MaxContinuationTurns: values.TurnBudget,
			MaxEvalTokens:        values.TokenBudget,
			MaxWallTime:          values.TimeBudget,
		},
	})
	if err != nil {
		m.goalRuntime = previous
		if createdSession {
			_ = m.discardExecutionSession()
		}
		m.appendGoalError("Create goal: " + err.Error())
		return nil
	}
	m.goalRuntime = runtime
	oldMode := m.mode
	m.mode = ModeAuto
	m.syncComposerAuthority()
	m.setRouterMode(m.modeConfigs[ModeAuto].RouterMode)
	if err := m.persistGoalSession(); err != nil {
		m.goalRuntime = previous
		m.mode = oldMode
		m.syncComposerAuthority()
		m.setRouterMode(m.modeConfigs[oldMode].RouterMode)
		if createdSession {
			_ = m.discardExecutionSession()
		}
		m.appendGoalError("Save goal: " + err.Error())
		return nil
	}
	// Materialize the runtime-derived checklist before Cortex linking or the
	// first provider command can begin. Model-authored plan prose is never
	// interpreted as host progress.
	m.syncGoalPlan()
	m.closeGoalForm()
	if m.goalAdvisor == nil {
		m.appendGoalSystem("Goal saved · Cortex is not configured; starting one bounded local turn…")
	} else {
		m.appendGoalSystem("Goal saved · linking Cortex before the first turn…")
	}
	return m.beginGoalOpen(false)
}

func (m *Model) blockGoalCreationDuringStandaloneRecovery() error {
	const guidance = "goal creation is blocked by ordinary execution recovery; use /recover to reconcile it, or /new to start a clean session"
	if m == nil {
		return errors.New(guidance)
	}
	if m.standaloneRecovery != nil {
		return errors.New(guidance)
	}
	// An existing Goal Runtime owns its durable recovery path. This check is
	// only for converting an ordinary session into a newly goal-owned one.
	if m.goalRuntime != nil || m.sessionStore == nil || m.sessionID <= 0 || m.agent == nil {
		return nil
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		return fmt.Errorf("%s; recovery check failed: %w", guidance, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	projection, err := m.sessionStore.ProjectExecutionRecovery(ctx, m.sessionID, workspaceID, m.executionCursor, 100)
	if err != nil {
		return fmt.Errorf("%s; recovery check failed: %w", guidance, err)
	}
	if len(projection.Hazards) == 0 {
		return nil
	}
	m.rememberStandaloneRecovery(standaloneRecoveryTarget(projection.Hazards, m.executionCursor, m.sessionPublicID))
	return errors.New(guidance)
}

func (m *Model) beginGoalOpen(manual bool) tea.Cmd {
	if m.goalRuntime == nil {
		return nil
	}
	if m.goalPersistenceDirty {
		m.appendGoalError("Cortex link blocked until the current goal snapshot is saved. Run /goal resume to retry persistence.")
		return nil
	}
	// Cortex is an optional semantic advisor. An installation with no advisor
	// configured still gets one host-bounded initial turn; later progress
	// remains explicit because an unlinked goal cannot verify itself. Once an
	// advisor is configured, its failures continue through the durable
	// pause-and-retry path below rather than silently degrading.
	if m.goalAdvisor == nil {
		return m.startGoalTurn(nil, manual)
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		m.appendGoalError("Read goal: " + err.Error())
		return nil
	}
	token, ctx, operationErr := m.beginGoalOperation("Linking Cortex", snapshot)
	if operationErr != nil {
		m.handleGoalOperationStartFailure("Cortex link", operationErr)
		return nil
	}
	request := goaladvisor.OpenRequest{
		GoalID: snapshot.ID, Objective: snapshot.Objective,
		AcceptanceCriteria: append([]goal.AcceptanceCriterion(nil), snapshot.AcceptanceCriteria...),
	}
	advisor := m.goalAdvisor
	return tea.Batch(m.startActivityCmd(), func() tea.Msg {
		advice, err := advisor.Open(ctx, request)
		return goalOpenResultMsg{Token: token, Manual: manual, Advice: advice, Err: err}
	})
}

func (m *Model) beginGoalOperation(label string, snapshot goal.Snapshot) (uint64, context.Context, error) {
	if m.goalOperationRunning {
		return 0, nil, fmt.Errorf("another goal operation is already running")
	}
	deadline, err := goalAdvisorOperationDeadline(snapshot, m.nowTime())
	if err != nil {
		return 0, nil, err
	}
	m.goalOperationToken++
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	m.goalOperation = label
	m.goalOperationCancel = cancel
	m.goalOperationRunning = true
	m.input.Blur()
	m.recalcViewportHeight()
	return m.goalOperationToken, ctx, nil
}

// beginGoalControlOperation owns the same cancellable/tokened lifecycle as a
// goal operation without consulting continuation budgets. Reading or settling
// an already-pending human decision is control-plane work: it grants no model
// turn and must remain possible after a continuation or wall budget expires.
func (m *Model) beginGoalControlOperation(label string) (uint64, context.Context, error) {
	if m.goalOperationRunning {
		return 0, nil, fmt.Errorf("another goal operation is already running")
	}
	m.goalOperationToken++
	ctx, cancel := context.WithTimeout(context.Background(), goalAdvisorTimeout)
	m.goalOperation = label
	m.goalOperationCancel = cancel
	m.goalOperationRunning = true
	m.input.Blur()
	m.recalcViewportHeight()
	return m.goalOperationToken, ctx, nil
}

func goalAdvisorOperationDeadline(snapshot goal.Snapshot, now time.Time) (time.Time, error) {
	deadline := now.Add(goalAdvisorTimeout)
	if snapshot.Budget.MaxWallTime <= 0 {
		return deadline, nil
	}
	goalDeadline := snapshot.CreatedAt.Add(snapshot.Budget.MaxWallTime)
	if !now.Before(goalDeadline) {
		return time.Time{}, fmt.Errorf("%w: wall time", goal.ErrBudgetExhausted)
	}
	if goalDeadline.Before(deadline) {
		deadline = goalDeadline
	}
	return deadline, nil
}

func (m *Model) handleGoalOperationStartFailure(operation string, operationErr error) {
	if !errors.Is(operationErr, goal.ErrBudgetExhausted) {
		m.appendGoalError(operation + ": " + operationErr.Error())
		return
	}
	var transitionErr error
	if m.goalRuntime != nil {
		if snapshot, err := m.goalRuntime.Snapshot(context.Background()); err != nil {
			transitionErr = err
		} else if snapshot.State == goal.StateActive && snapshot.PendingContinuation == nil {
			transitionErr = m.goalRuntime.Pause(context.Background(), "goal deadline elapsed before "+strings.ToLower(operation))
		}
	}
	persistErr := m.persistGoalSession()
	if err := errors.Join(transitionErr, persistErr); err != nil {
		m.appendGoalError("Save elapsed goal deadline: " + err.Error())
	}
	m.appendGoalSystem("Goal budget exhausted before " + operation + ". Completion was not inferred; adjust /goal budget before resuming.")
}

func (m *Model) finishGoalOperation(token uint64) bool {
	if token != m.goalOperationToken {
		return false
	}
	if m.goalOperationCancel != nil {
		m.goalOperationCancel()
	}
	m.goalOperationCancel = nil
	m.goalOperation = ""
	m.goalOperationRunning = false
	m.input.Focus()
	m.recalcViewportHeight()
	return true
}

func (m *Model) cancelGoalOperation(reason string) {
	if !m.goalOperationRunning {
		return
	}
	if m.goalOperationCancel != nil {
		m.goalOperationCancel()
	}
	m.goalOperation = "Stopping goal operation"
	if m.goalRuntime != nil {
		if snapshot, err := m.goalRuntime.Snapshot(context.Background()); err == nil && snapshot.State == goal.StateActive && snapshot.PendingContinuation == nil {
			_ = m.goalRuntime.Pause(context.Background(), reason)
			_ = m.persistGoalSession()
		}
	}
	m.appendGoalSystem(reason)
	m.recalcViewportHeight()
}

func (m *Model) persistGoalSession() (err error) {
	defer func() { m.goalPersistenceDirty = err != nil }()
	if m.sessionStore == nil || m.sessionID <= 0 {
		return fmt.Errorf("durable session is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return m.persistSessionState(ctx)
}

func (m *Model) restoreGoalSnapshot(snapshot goal.Snapshot) error {
	restored, err := goal.Restore(snapshot)
	if err != nil {
		return fmt.Errorf("restore prior goal state: %w", err)
	}
	m.goalRuntime = restored
	return nil
}

func (m *Model) stopGoalAfterPersistenceFailure(before goal.Snapshot, reason string) error {
	if err := m.restoreGoalSnapshot(before); err != nil {
		return err
	}
	current, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		return err
	}
	if current.State == goal.StateActive && current.PendingContinuation == nil {
		if err := m.goalRuntime.Pause(context.Background(), reason); err != nil {
			return err
		}
	}
	return nil
}

func (m *Model) persistGoalAdvisorTransition(before goal.Snapshot, operation string) bool {
	if err := m.persistGoalSession(); err != nil {
		stopErr := m.stopGoalAfterPersistenceFailure(before, "Cortex transition could not be persisted")
		m.appendGoalError(operation + ": " + errors.Join(err, stopErr).Error())
		return false
	}
	return true
}

func (m *Model) discardExecutionSession() error {
	if m.sessionID <= 0 || m.sessionStore == nil {
		m.resetSessionStateRevision()
		return nil
	}
	id := m.sessionID
	leaseErr := m.releaseExecutionSessionLease()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	deleteErr := m.sessionStore.DeleteSession(ctx, id)
	cancel()
	m.sessionID = 0
	m.sessionPublicID = ""
	m.activeSessionTitle = ""
	m.executionCursor = 0
	m.resetSessionStateRevision()
	m.agent.SetCheckpointSessionID(0)
	m.agent.SetExecutionSessionID(0, "")
	m.agent.SetExecutionSnapshotCursor(0)
	return errors.Join(leaseErr, deleteErr)
}

func (m *Model) appendGoalSystem(message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	m.entries = append(m.entries, ChatEntry{Kind: "system", Content: message})
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.resumeFollow()
}

func (m *Model) appendGoalError(message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	m.entries = append(m.entries, ChatEntry{Kind: "error", Content: message})
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.resumeFollow()
}

func (m *Model) handleGoalOpenResult(message goalOpenResultMsg) tea.Cmd {
	if !m.finishGoalOperation(message.Token) || m.goalRuntime == nil {
		return nil
	}
	if m.shuttingDown {
		if m.shutdownReady() {
			return tea.Quit
		}
		return nil
	}
	before, snapshotErr := m.goalRuntime.Snapshot(context.Background())
	if snapshotErr != nil {
		m.appendGoalError("Read goal before Cortex link: " + snapshotErr.Error())
		return nil
	}
	if errors.Is(message.Err, context.Canceled) || errors.Is(message.Err, context.DeadlineExceeded) {
		if before.State == goal.StateActive && before.PendingContinuation == nil {
			_ = m.goalRuntime.Pause(context.Background(), "Cortex link was cancelled before its receipt was accepted")
		}
		_ = m.persistGoalSession()
		m.appendGoalSystem("Cortex link stopped; the local goal remains paused and can be relinked idempotently with /goal resume.")
		return nil
	}
	linked := false
	linkMessage := ""
	if message.Err != nil {
		// AUTO promises semantic supervision. Keep the durable local goal, but do
		// not silently dispatch an unsupervised provider turn when Cortex cannot
		// accept it. The exact bounded error is retained as an actionable receipt.
		detail := truncateGoalAdvisorError(message.Err)
		if before.State == goal.StateActive && before.PendingContinuation == nil {
			if err := m.goalRuntime.Pause(context.Background(), "Cortex link failed: "+detail); err != nil {
				m.appendGoalError("Pause goal after Cortex link failure: " + err.Error())
				return nil
			}
		}
		if err := m.persistGoalSession(); err != nil {
			stopErr := m.stopGoalAfterPersistenceFailure(before, "Cortex link failure could not be persisted")
			m.appendGoalError("Save Cortex link failure: " + errors.Join(err, stopErr).Error())
			return nil
		}
		m.appendGoalError("Cortex link failed · " + detail + ". Goal paused; update or rebuild Cortex so cortex_open_task is exposed, then run /goal resume.")
		return nil
	} else {
		if strings.TrimSpace(message.Advice.TaskID) == "" {
			linkMessage = "Cortex returned no task identity; continuing as a bounded local goal."
		} else if err := m.goalRuntime.AttachCortex(context.Background(), goal.CortexCorrelation{
			TaskID: message.Advice.TaskID, Revision: message.Advice.Revision, Actor: goalActor,
		}); err != nil {
			m.appendGoalError("Link Cortex goal: " + err.Error())
			_ = m.goalRuntime.Pause(context.Background(), "Cortex correlation could not be persisted")
			_ = m.persistGoalSession()
			return nil
		} else {
			linked = true
			linkMessage = fmt.Sprintf("Cortex linked · %s · %s", message.Advice.TaskID, fallbackGoalText(message.Advice.Phase, "ready"))
		}
	}
	if err := m.persistGoalSession(); err != nil {
		stopErr := m.stopGoalAfterPersistenceFailure(before, "goal link could not be persisted")
		m.appendGoalError("Save Cortex link: " + errors.Join(err, stopErr).Error())
		return nil
	}
	m.appendGoalSystem(linkMessage)
	if linked {
		if message.Manual || before.LastTurn != nil {
			return m.beginGoalEvaluation(message.Manual)
		}
		return m.startGoalTurn(&message.Advice, false)
	}
	return m.startGoalTurn(nil, message.Manual)
}

func truncateGoalAdvisorError(err error) string {
	detail := strings.Join(strings.Fields(err.Error()), " ")
	const maxBytes = 320
	if len(detail) > maxBytes {
		detail = detail[:maxBytes-1] + "…"
	}
	return detail
}

func fallbackGoalText(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func (m *Model) startGoalTurn(advice *goaladvisor.Advice, manual bool) tea.Cmd {
	if m.goalRuntime == nil || m.state != StateIdle || m.goalOperation != "" {
		return nil
	}
	if m.goalPersistenceDirty {
		m.appendGoalError("Goal dispatch blocked because its latest state is not durably saved. Run /goal resume to retry persistence.")
		return nil
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		m.appendGoalError("Read goal: " + err.Error())
		return nil
	}
	if snapshot.PendingContinuation != nil {
		m.appendGoalError("Goal turn blocked: an admitted turn still requires settlement or recovery.")
		return nil
	}
	if snapshot.State != goal.StateActive {
		m.appendGoalSystem(goalStatePreventsTurnMessage(snapshot))
		return nil
	}
	turnID, err := execution.NewTurnID()
	if err != nil {
		m.appendGoalError("Create goal turn: " + err.Error())
		return nil
	}
	admissionKind := goal.AdmissionInitial
	if snapshot.LastTurn != nil {
		if manual {
			admissionKind = goal.AdmissionManual
		} else {
			admissionKind = goal.AdmissionAutomatic
		}
	}
	if _, err := m.goalRuntime.BeginTurn(context.Background(), turnID, admissionKind); err != nil {
		if admissionKind == goal.AdmissionAutomatic {
			m.appendGoalSystem(goalContinuationDeniedMessage(err, snapshot.StateReason))
		} else {
			m.appendGoalError("Admit goal turn: " + err.Error())
		}
		return nil
	}
	// Every provider turn has one durable dispatch boundary. Never create or
	// return the provider command unless this exact pending TurnID and kind are
	// safely on disk.
	if err := m.persistGoalSession(); err != nil {
		recoveryErr := m.goalRuntime.RecoverPendingContinuation(context.Background(), goal.PendingRecovery{
			TurnID: turnID, Kind: goal.PendingCancelledBeforeDispatch,
			Reason:   "goal turn admission could not be persisted before dispatch",
			Evidence: "session snapshot save failed before the Bubble Tea provider command was returned",
		})
		m.appendGoalError("Save goal turn admission: " + errors.Join(err, recoveryErr).Error())
		return nil
	}
	snapshot, err = m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		recoveryErr := m.goalRuntime.RecoverPendingContinuation(context.Background(), goal.PendingRecovery{
			TurnID: turnID, Kind: goal.PendingCancelledBeforeDispatch,
			Reason:   "goal turn admission was persisted but could not be read before dispatch",
			Evidence: "the provider command had not been created when the durable snapshot read failed",
		})
		_ = m.persistGoalSession()
		m.appendGoalError("Read admitted goal turn: " + errors.Join(err, recoveryErr).Error())
		return nil
	}

	limits, limitErr := goalAgentTurnLimits(snapshot, m.nowTime())
	if limitErr != nil {
		current, _ := m.goalRuntime.Snapshot(context.Background())
		if current.PendingContinuation != nil {
			_ = m.goalRuntime.RecoverPendingContinuation(context.Background(), goal.PendingRecovery{
				TurnID: current.PendingContinuation.TurnID, Kind: goal.PendingCancelledBeforeDispatch,
				Reason: "goal budget expired before provider dispatch", Evidence: "host rechecked the remaining hard limits before creating the provider command",
			})
		}
		_ = m.persistGoalSession()
		m.appendGoalSystem("Goal budget exhausted before dispatch. Completion was not inferred; adjust /goal budget before resuming.")
		return nil
	}

	prompt := buildGoalPrompt(snapshot, advice, manual)
	capability := agent.DurableGoalCapabilityActivity(
		snapshot.ID, snapshot.Objective, goalCapabilityPhase(advice), "",
	)
	m.goalTurnID = turnID
	m.goalTurnToolCalls = 0
	m.goalTurnSuccesses = 0
	command := m.sendGoalToAgentTurnWithCapability(prompt, turnID, limits, capability, adviceContinuation(advice))
	if m.state != StateWaiting {
		if pending, snapErr := m.goalRuntime.Snapshot(context.Background()); snapErr == nil && pending.PendingContinuation != nil {
			_ = m.goalRuntime.RecoverPendingContinuation(context.Background(), goal.PendingRecovery{
				TurnID: turnID, Kind: goal.PendingCancelledBeforeDispatch,
				Reason:   "goal turn failed before provider dispatch",
				Evidence: "sendToAgentTurn returned without entering the waiting state",
			})
		}
		m.goalTurnID = ""
		_ = m.persistGoalSession()
		return nil
	}
	return command
}

func adviceContinuation(advice *goaladvisor.Advice) *agent.ContinuationContext {
	if advice == nil {
		return nil
	}
	return advice.Continuation
}

// goalAgentTurnLimits delegates to the shared supervisor policy so the TUI
// and headless controllers bound goal turns identically.
func goalAgentTurnLimits(snapshot goal.Snapshot, now time.Time) (agent.TurnLimits, error) {
	return supervisor.AgentTurnLimits(snapshot, now)
}

func goalStatePreventsTurnMessage(snapshot goal.Snapshot) string {
	switch snapshot.State {
	case goal.StateExhausted:
		return "Goal budget exhausted. Completion was not inferred; use /goal budget to add capacity."
	case goal.StateBlocked:
		return "Goal is blocked · " + fallbackGoalText(snapshot.StateReason, "resolve the blocker before resuming")
	case goal.StatePaused:
		return "Goal is paused. Resume it explicitly with /goal resume."
	case goal.StateCompleted, goal.StateDropped:
		return "Goal is already " + string(snapshot.State) + "."
	default:
		return "Goal cannot start a turn from " + string(snapshot.State) + "."
	}
}

func goalContinuationDeniedMessage(err error, reason string) string {
	switch {
	case errors.Is(err, goal.ErrBudgetExhausted):
		return "Goal budget exhausted. Completion was not inferred; use /goal budget to add capacity."
	case errors.Is(err, goal.ErrAutoContinuationDenied):
		if strings.TrimSpace(reason) != "" {
			return "Goal paused · " + reason
		}
		return "Goal paused because the previous turn did not establish progress."
	default:
		return "Goal continuation stopped · " + err.Error()
	}
}

func buildGoalPrompt(snapshot goal.Snapshot, advice *goaladvisor.Advice, manual bool) string {
	var builder strings.Builder
	if snapshot.LastTurn == nil {
		builder.WriteString("Start working on this durable goal.\n")
	} else if manual {
		builder.WriteString("The user explicitly resumed this durable goal. Continue with one concrete, verifiable slice.\n")
	} else {
		builder.WriteString("Continue this durable goal with the next concrete, verifiable slice.\n")
	}
	fmt.Fprintf(&builder, "\nGoal: %s\n", snapshot.Objective)
	builder.WriteString("Acceptance criteria:\n")
	for _, criterion := range snapshot.AcceptanceCriteria {
		fmt.Fprintf(&builder, "- [%s] %s\n", criterion.ID, criterion.Description)
	}
	if snapshot.Cortex.TaskID != "" {
		fmt.Fprintf(&builder, "\nCortex case: %s (revision %d)\n", snapshot.Cortex.TaskID, snapshot.Cortex.Revision)
		builder.WriteString("When submitting Cortex named verification claims, preserve each bracketed acceptance ID exactly as claimId. Local completion requires a current bound proof receipt for every ID.\n")
	}
	if advice != nil {
		if advice.Phase != "" {
			fmt.Fprintf(&builder, "Cortex phase: %s\n", advice.Phase)
		}
		if advice.Summary != "" {
			fmt.Fprintf(&builder, "Cortex status: %s\n", advice.Summary)
		}
		if len(advice.MissingVerification) > 0 {
			fmt.Fprintf(&builder, "Missing verification: %s\n", strings.Join(advice.MissingVerification, ", "))
		}
		if len(advice.StaleVerification) > 0 {
			fmt.Fprintf(&builder, "Stale verification: %s\n", strings.Join(advice.StaleVerification, ", "))
		}
		if advice.Degraded {
			builder.WriteString("Cortex warning: this status is degraded; corroborate it before relying on it.\n")
		}
	}
	remainingTurns := "unlimited"
	if snapshot.Budget.MaxContinuationTurns > 0 {
		remainingTurns = fmt.Sprintf("%d", max(int64(0), snapshot.Budget.MaxContinuationTurns-snapshot.Usage.ContinuationTurns))
	}
	remainingTokens := "unlimited"
	if snapshot.Budget.MaxEvalTokens > 0 {
		remainingTokens = formatGoalTokens(max(int64(0), snapshot.Budget.MaxEvalTokens-snapshot.Usage.EvalTokens))
	}
	fmt.Fprintf(&builder, "\nHost budget remaining: %s continuation turns · %s eval tokens.\n", remainingTurns, remainingTokens)
	builder.WriteString("Use tools to make or verify concrete progress. Do not merely restate a plan. If you cannot make progress, explain the blocker and yield; a no-progress turn pauses the goal. Never claim completion from budget exhaustion or prose alone. Cortex-verified acceptance is authoritative when linked.")
	return builder.String()
}

func (m *Model) settleGoalTurn(message AgentDoneMsg) {
	m.goalNeedsEvaluation = false
	if m.goalRuntime == nil || m.goalTurnID == "" {
		return
	}
	turnID := m.goalTurnID
	m.goalTurnID = ""
	if message.TurnID != turnID {
		snapshot, snapshotErr := m.goalRuntime.Snapshot(context.Background())
		var recoveryErr error
		switch {
		case snapshotErr != nil:
			recoveryErr = snapshotErr
		case snapshot.PendingContinuation != nil:
			recoveryErr = m.goalRuntime.RecoverPendingContinuation(context.Background(), goal.PendingRecovery{
				TurnID: turnID, Kind: goal.PendingOutcomeUnknown,
				Reason:     "provider receipt did not match the admitted goal turn identity",
				Evidence:   "the agent completion receipt did not carry the exact TurnID persisted before provider dispatch",
				OutcomeRef: turnID,
			})
		default:
			recoveryErr = m.goalRuntime.Block(context.Background(), goal.Blocker{
				Kind: goal.BlockOutcomeUnknown, Reference: turnID,
				Reason: "provider receipt did not match the admitted goal turn identity",
			})
		}
		if recoveryErr != nil {
			m.appendGoalError("Goal recovery failed after an unexpected provider turn identity: " + recoveryErr.Error())
		} else {
			m.appendGoalError("Goal recovery blocked: provider receipt used an unexpected turn identity.")
		}
		return
	}

	// An AUTO iteration checkpoint is a scheduler signal, not a failure: the
	// segment hit its iteration watchdog after verified distinct progress. The
	// goal records it as a productive turn and its own budgets and evaluation
	// decide whether another admitted turn continues the work.
	checkpointSignal := errors.Is(message.Err, agent.ErrAutoIterationCheckpoint)
	productive := (message.Err == nil || checkpointSignal) && m.goalTurnSuccesses > 0
	summary := goalTurnSummary(message.Err, m.goalTurnToolCalls, m.goalTurnSuccesses, m.lastAssistantContent())
	report := goal.TurnReport{
		TurnID: turnID, EvalTokens: int64(max(0, m.turnEvalTotal)),
		Productive: productive, Summary: summary,
	}
	var unresolved *agent.UnresolvedExecutionError
	if errors.As(message.Err, &unresolved) {
		report.Productive = false
		report.OutcomeUnknown = true
		report.OutcomeRef = fallbackGoalText(unresolved.ExecutionID, turnID)
	}
	if err := m.goalRuntime.RecordTurn(context.Background(), report); err != nil {
		m.appendGoalError("Settle goal turn: " + err.Error())
		return
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		m.appendGoalError("Read settled goal: " + err.Error())
		return
	}
	switch snapshot.State {
	case goal.StateActive:
		m.goalNeedsEvaluation = true
	case goal.StateExhausted:
		// A final admitted turn can still have completed the Cortex case. Check
		// once before presenting exhaustion as the terminal run state.
		m.goalNeedsEvaluation = snapshot.Cortex.TaskID != ""
	case goal.StateBlocked:
		m.appendGoalError("Goal blocked · " + snapshot.StateReason)
	case goal.StatePaused:
		m.appendGoalSystem("Goal paused · " + snapshot.StateReason)
	}
	m.goalTurnToolCalls = 0
	m.goalTurnSuccesses = 0
}

func goalTurnSummary(turnErr error, toolCalls, successfulTools int, assistantText string) string {
	switch {
	case turnErr != nil:
		return boundGoalSummary(turnErr.Error())
	case successfulTools > 0:
		return fmt.Sprintf("settled %d tool call(s), %d successful", toolCalls, successfulTools)
	case toolCalls > 0:
		return fmt.Sprintf("settled %d tool call(s) without a successful receipt", toolCalls)
	case strings.TrimSpace(assistantText) != "":
		return "assistant yielded without a concrete tool receipt"
	default:
		return "turn yielded without concrete progress"
	}
}

func boundGoalSummary(value string) string {
	return boundGoalText(value, goal.MaxReasonBytes)
}

func (m *Model) beginGoalEvaluation(manual bool) tea.Cmd {
	if m.goalRuntime == nil || m.state != StateIdle || m.goalOperation != "" {
		return nil
	}
	if m.goalPersistenceDirty {
		m.appendGoalError("Cortex status blocked until the current goal snapshot is durably saved.")
		return nil
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		m.appendGoalError("Read goal: " + err.Error())
		return nil
	}
	if snapshot.Cortex.TaskID == "" || m.goalAdvisor == nil {
		if manual {
			return m.startGoalTurn(nil, true)
		}
		if snapshot.State == goal.StateActive {
			if err := m.goalRuntime.Pause(context.Background(), "Cortex is not linked; automatic progress cannot be verified"); err != nil {
				m.appendGoalError("Pause local goal: " + err.Error())
				return nil
			}
			if err := m.persistGoalSession(); err != nil {
				m.appendGoalError("Save local goal pause: " + err.Error())
				return nil
			}
			m.appendGoalSystem("Goal paused · Cortex is not linked, so review the result and resume explicitly.")
		}
		return nil
	}

	decisionOnly := snapshot.State == goal.StateBlocked && snapshot.Blocker != nil && snapshot.Blocker.Kind == goal.BlockDecision
	var token uint64
	var ctx context.Context
	var operationErr error
	if decisionOnly {
		token, ctx, operationErr = m.beginGoalControlOperation("Refreshing Cortex decision")
	} else {
		token, ctx, operationErr = m.beginGoalOperation("Checking goal", snapshot)
	}
	if operationErr != nil {
		m.handleGoalOperationStartFailure("Cortex status", operationErr)
		return nil
	}
	advisor := m.goalAdvisor
	taskID := snapshot.Cortex.TaskID
	expectedDecisionID := ""
	expectedRequestSHA256 := ""
	if decisionOnly && m.cortexDecisionAttempt != nil && m.cortexDecisionAttempt.TaskID == taskID {
		expectedDecisionID = m.cortexDecisionAttempt.DecisionID
		expectedRequestSHA256 = m.cortexDecisionAttempt.RequestSHA256
	}
	return tea.Batch(m.startActivityCmd(), func() tea.Msg {
		advice, err := advisor.Status(ctx, taskID)
		return goalStatusResultMsg{
			Token: token, Manual: manual, DecisionOnly: decisionOnly,
			ExpectedDecisionTaskID: taskID, ExpectedDecisionID: expectedDecisionID,
			ExpectedRequestSHA256: expectedRequestSHA256, Advice: advice, Err: err,
		}
	})
}

func (m *Model) handleGoalStatusResult(message goalStatusResultMsg) tea.Cmd {
	if message.DecisionRefresh {
		operation := m.cortexDecisionOp
		if operation == nil || operation.Kind != cortexDecisionOperationRefresh ||
			operation.Token != message.Token || operation.Generation != message.DecisionGeneration ||
			operation.TaskID != message.ExpectedDecisionTaskID || operation.DecisionID != message.ExpectedDecisionID ||
			operation.RequestSHA256 != message.ExpectedRequestSHA256 {
			return nil
		}
	}
	if !m.finishGoalOperation(message.Token) || m.goalRuntime == nil {
		return nil
	}
	if message.DecisionRefresh {
		m.cortexDecisionOp = nil
	}
	if m.shuttingDown {
		if m.shutdownReady() {
			return tea.Quit
		}
		return nil
	}
	if message.Err != nil {
		if message.DecisionOnly {
			m.markCortexDecisionOutcomeUnknown()
			return nil
		}
		snapshot, _ := m.goalRuntime.Snapshot(context.Background())
		if snapshot.State == goal.StateActive {
			_ = m.goalRuntime.Pause(context.Background(), "Cortex status is unavailable; automatic continuation stopped")
		}
		_ = m.persistGoalSession()
		m.appendGoalError("Cortex status unavailable; goal paused safely: " + message.Err.Error())
		return nil
	}

	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		m.appendGoalError("Read goal: " + err.Error())
		return nil
	}
	beforeStatus := snapshot
	reconciliationOnlyPending := false
	supersededDecisionAttempt := false
	decisionAttemptBeforeStatus := cloneCortexDecisionAttempt(m.cortexDecisionAttempt)
	if message.DecisionOnly {
		if !validCortexDecisionStatusAdvice(message.Advice, message.ExpectedDecisionTaskID) {
			m.markCortexDecisionOutcomeUnknown()
			return nil
		}
		if !cortexDecisionMatchesGoal(snapshot, message.ExpectedDecisionTaskID) {
			m.markCortexDecisionOutcomeUnknown()
			return nil
		}
		if message.ExpectedRequestSHA256 != "" && message.Advice.Decision != nil {
			requestSHA256, requestErr := message.Advice.Decision.RequestBindingSHA256(message.Advice.TaskID)
			if requestErr != nil {
				m.markCortexDecisionOutcomeUnknown()
				return nil
			}
			if message.Advice.Decision.ID == message.ExpectedDecisionID {
				if requestSHA256 != message.ExpectedRequestSHA256 {
					// Reusing a stable decision ID for changed request content is an
					// immutable binding conflict, not a new decision.
					m.markCortexDecisionOutcomeUnknown()
					return nil
				}
				reconciliationOnlyPending = true
			} else {
				attempt := m.cortexDecisionAttempt
				if attempt == nil || attempt.TaskID != message.ExpectedDecisionTaskID ||
					attempt.DecisionID != message.ExpectedDecisionID ||
					attempt.RequestSHA256 != message.ExpectedRequestSHA256 {
					m.markCortexDecisionOutcomeUnknown()
					return nil
				}
				if err := m.supersedeCortexDecisionControlItem(
					snapshot, attempt, message.Advice.Decision.ID, requestSHA256,
				); err != nil {
					m.markCortexDecisionOutcomeUnknown()
					return nil
				}
				supersededDecisionAttempt = true
			}
		}
	}

	phase := strings.ToLower(strings.TrimSpace(message.Advice.Phase))
	pendingDecision := message.Advice.PendingDecision || phase == "needs_human_decision"
	// The durable request is recorded before any local state transition. A
	// typed pending status can become visible only after both the control item
	// and matching BlockDecision snapshot have been persisted.
	if pendingDecision {
		if controlErr := m.recordCortexDecisionControlItem(beforeStatus, message.Advice); controlErr != nil {
			m.protectControlPlaneFailure("Record Cortex decision", controlErr)
			return nil
		}
	} else if controlErr := m.resolveCortexDecisionControlItems(beforeStatus, message.Advice); controlErr != nil {
		m.protectControlPlaneFailure("Resolve Cortex decision", controlErr)
		return nil
	}

	correlation := snapshot.Cortex
	if message.Advice.TaskID != "" {
		correlation.TaskID = message.Advice.TaskID
	}
	correlation.Revision = message.Advice.Revision
	if correlation.Actor == "" {
		correlation.Actor = goalActor
	}
	if err := m.goalRuntime.AttachCortex(context.Background(), correlation); err != nil {
		var pauseErr error
		if current, snapshotErr := m.goalRuntime.Snapshot(context.Background()); snapshotErr == nil && current.State == goal.StateActive {
			pauseErr = m.goalRuntime.Pause(context.Background(), "Cortex correlation conflicted with durable goal state")
		}
		persistErr := m.persistGoalSession()
		m.appendGoalError("Update Cortex correlation: " + errors.Join(err, pauseErr, persistErr).Error())
		return nil
	}

	// A decision refresh is reconciliation only. Fresh status may have advanced
	// to any semantic phase, but this first authority boundary resolves the
	// decision blocker and lands paused before another explicit resume can act.
	if message.DecisionOnly && !pendingDecision {
		current, snapshotErr := m.goalRuntime.Snapshot(context.Background())
		if snapshotErr != nil || !cortexDecisionMatchesGoal(current, message.ExpectedDecisionTaskID) {
			m.markCortexDecisionOutcomeUnknown()
			return nil
		}
		if err := m.goalRuntime.ResolveBlock(context.Background(), goal.BlockResolution{
			Reference: current.Blocker.Reference,
			Reason:    "fresh Cortex status no longer reports the decision",
			Evidence:  "fresh Cortex status",
		}); err != nil {
			m.markCortexDecisionOutcomeUnknown()
			return nil
		}
		m.cortexDecisionAttempt = nil
		if !m.persistGoalAdvisorTransition(beforeStatus, "Save cleared Cortex decision blocker") {
			m.cortexDecisionAttempt = decisionAttemptBeforeStatus
			return nil
		}
		m.clearCortexDecisionPresentation()
		if message.Manual {
			m.appendGoalSystem("Decision cleared; run /goal resume again to process fresh Cortex status.")
		} else {
			m.appendGoalSystem("Decision cleared; run /goal resume to process fresh Cortex status.")
		}
		return nil
	}

	switch {
	case pendingDecision:
		if err := ensureGoalBlock(m.goalRuntime, goal.BlockDecision, correlation.TaskID, "Cortex is waiting for a human decision"); err != nil {
			m.appendGoalError("Pause for Cortex decision: " + err.Error())
			return nil
		}
		if supersededDecisionAttempt {
			// The old exact item is already durably answered/dismissed and the new
			// request has been recorded. Clear the old no-retry fence in the same
			// session-state write that retains the new blocker.
			m.cortexDecisionAttempt = nil
		}
		if !m.persistGoalAdvisorTransition(beforeStatus, "Save Cortex decision blocker") {
			if supersededDecisionAttempt {
				m.cortexDecisionAttempt = decisionAttemptBeforeStatus
			}
			return nil
		}
		if err := m.presentCortexDecision(message.Advice); err != nil {
			m.appendGoalError("Show Cortex decision: " + err.Error())
			return nil
		}
		if reconciliationOnlyPending {
			m.cortexDecision.setOutcomeUnknown()
			m.input.Blur()
			m.recalcViewportHeight()
		}
		m.appendGoalSystem("Goal paused · Cortex requires a human decision.")
		return nil

	case phase == "blocked":
		if err := ensureGoalBlock(m.goalRuntime, goal.BlockDependency, correlation.TaskID, fallbackGoalText(message.Advice.Summary, "Cortex case is blocked")); err != nil {
			m.appendGoalError("Block goal: " + err.Error())
			return nil
		}
		if !m.persistGoalAdvisorTransition(beforeStatus, "Save Cortex dependency blocker") {
			return nil
		}
		return nil

	case phase == "abandoned":
		if snapshot.State != goal.StateDropped && !snapshot.State.Terminal() {
			_ = m.goalRuntime.Drop(context.Background(), "Cortex case was abandoned")
		}
		if !m.persistGoalAdvisorTransition(beforeStatus, "Save abandoned Cortex goal") {
			return nil
		}
		m.appendGoalSystem("Goal dropped · Cortex case was abandoned.")
		return nil

	case phase == "complete":
		if strings.EqualFold(message.Advice.VerificationOutcome, "verified") &&
			len(message.Advice.MissingVerification) == 0 &&
			len(message.Advice.StaleVerification) == 0 &&
			!message.Advice.Degraded {
			if err := m.verifyGoalCompletionWorkspace(beforeStatus, message.Advice); err != nil {
				blockErr := ensureGoalBlock(m.goalRuntime, goal.BlockDependency, correlation.TaskID, "Cortex completion proof no longer matches the current workspace")
				if !m.persistGoalAdvisorTransition(beforeStatus, "Save stale Cortex proof blocker") {
					return nil
				}
				m.appendGoalError("Goal not completed: " + errors.Join(err, blockErr).Error())
				return nil
			}
			if err := completeGoalFromCortex(m.goalRuntime, message.Advice); err != nil {
				blockErr := ensureGoalBlock(m.goalRuntime, goal.BlockDependency, correlation.TaskID, "Cortex completion lacks criterion-bound verification evidence")
				if !m.persistGoalAdvisorTransition(beforeStatus, "Save incomplete Cortex proof blocker") {
					return nil
				}
				m.appendGoalError("Goal not completed: " + errors.Join(err, blockErr).Error())
				return nil
			}
			if !m.persistGoalAdvisorTransition(beforeStatus, "Save verified goal completion") {
				return nil
			}
			m.appendGoalSystem("Goal completed · Cortex verified every acceptance criterion.")
			return nil
		}
		if err := ensureGoalBlock(m.goalRuntime, goal.BlockDependency, correlation.TaskID, "Cortex completed without a verified assessment"); err != nil {
			m.appendGoalError("Protect unverified completion: " + err.Error())
		}
		if !m.persistGoalAdvisorTransition(beforeStatus, "Save unverified Cortex blocker") {
			return nil
		}
		m.appendGoalError("Goal not completed: Cortex has no current verified assessment.")
		return nil
	}

	if message.Manual {
		if err := m.prepareManualGoalResume(snapshot, message.Advice); err != nil {
			m.appendGoalError("Resume goal: " + err.Error())
			_ = m.persistGoalSession()
			return nil
		}
		if err := m.persistGoalSession(); err != nil {
			stopErr := m.stopGoalAfterPersistenceFailure(beforeStatus, "resumed goal could not be persisted")
			m.appendGoalError("Save resumed goal: " + errors.Join(err, stopErr).Error())
			return nil
		}
		return m.startGoalTurn(&message.Advice, true)
	}
	if message.Advice.Revision <= beforeStatus.Cortex.Revision {
		updated, snapshotErr := m.goalRuntime.Snapshot(context.Background())
		if snapshotErr != nil {
			m.appendGoalError("Read Cortex progress: " + snapshotErr.Error())
			return nil
		}
		pausedForProgress := false
		if updated.State == goal.StateActive {
			if err := m.goalRuntime.Pause(context.Background(), "Cortex case did not advance during the previous turn"); err != nil {
				m.appendGoalError("Pause unverified continuation: " + err.Error())
				return nil
			}
			pausedForProgress = true
		}
		if !m.persistGoalAdvisorTransition(beforeStatus, "Save Cortex no-progress pause") {
			return nil
		}
		if pausedForProgress {
			m.appendGoalSystem("Goal paused · Cortex recorded no new semantic progress. Review the turn, then resume explicitly if needed.")
		} else if updated.State == goal.StateExhausted {
			m.appendGoalSystem("Goal budget exhausted. Completion was not inferred; use /goal budget to add capacity.")
		}
		return nil
	}

	updated, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		m.appendGoalError("Read goal: " + err.Error())
		return nil
	}
	if updated.State != goal.StateActive {
		if updated.State == goal.StateExhausted {
			m.appendGoalSystem("Goal budget exhausted. Completion was not inferred; use /goal budget to add capacity.")
		}
		_ = m.persistGoalSession()
		return nil
	}
	return m.startGoalTurn(&message.Advice, false)
}

func (m *Model) verifyGoalCompletionWorkspace(snapshot goal.Snapshot, advice goaladvisor.Advice) error {
	if !advice.ProofRevision.Valid() {
		return fmt.Errorf("%w: Cortex completion has no workspace proof", goal.ErrAcceptanceIncomplete)
	}
	deadline, err := goalAdvisorOperationDeadline(snapshot, m.nowTime())
	if err != nil {
		return fmt.Errorf("%w: refresh workspace proof: %v", goal.ErrAcceptanceIncomplete, err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	current, err := goaladvisor.CurrentWorkspaceRevision(ctx, m.agent.WorkDir())
	if err != nil {
		return fmt.Errorf("%w: refresh workspace proof: %v", goal.ErrAcceptanceIncomplete, err)
	}
	if current.Commit != advice.ProofRevision.Commit || current.DirtyDigest != advice.ProofRevision.DirtyDigest {
		return fmt.Errorf("%w: workspace changed after Cortex verification", goal.ErrAcceptanceIncomplete)
	}
	return nil
}

func ensureGoalBlock(runtime *goal.Runtime, kind goal.BlockKind, reference, reason string) error {
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		return err
	}
	if snapshot.State == goal.StateBlocked {
		if snapshot.Blocker != nil && snapshot.Blocker.Kind == kind && snapshot.Blocker.Reference == reference {
			return nil
		}
		return fmt.Errorf("goal is already blocked by %s", snapshot.StateReason)
	}
	return runtime.Block(context.Background(), goal.Blocker{
		Kind: kind, Reference: fallbackGoalText(reference, snapshot.ID), Reason: reason,
	})
}

func completeGoalFromCortex(runtime *goal.Runtime, advice goaladvisor.Advice) error {
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		return err
	}
	if snapshot.Cortex.TaskID == "" || advice.TaskID != snapshot.Cortex.TaskID || advice.Revision != snapshot.Cortex.Revision || !advice.ProofRevision.Valid() {
		return fmt.Errorf("%w: Cortex completion revision does not match the durable correlation", goal.ErrAcceptanceIncomplete)
	}
	results := make([]goal.AcceptanceResult, 0, len(snapshot.AcceptanceCriteria))
	for _, criterion := range snapshot.AcceptanceCriteria {
		proof, exists := advice.CriterionEvidence[criterion.ID]
		if !exists || len(proof.Evidence) == 0 || strings.TrimSpace(proof.Claim) != strings.TrimSpace(criterion.Description) {
			return fmt.Errorf("%w: Cortex returned no bound evidence for %s", goal.ErrAcceptanceIncomplete, criterion.ID)
		}
		if proof.Revision != advice.ProofRevision.Commit || proof.DirtyDigest != advice.ProofRevision.DirtyDigest {
			return fmt.Errorf("%w: Cortex evidence for %s does not match the current workspace", goal.ErrAcceptanceIncomplete, criterion.ID)
		}
		evidence := fmt.Sprintf(
			"Cortex case %s revision %d verified %s at workspace HEAD %s with dirty digest %s; refs: %s",
			snapshot.Cortex.TaskID, advice.Revision, criterion.ID,
			advice.ProofRevision.Commit, advice.ProofRevision.DirtyDigest,
			strings.Join(proof.Evidence, ", "),
		)
		results = append(results, goal.AcceptanceResult{
			CriterionID: criterion.ID, Satisfied: true, Evidence: boundGoalEvidence(evidence),
		})
	}
	if snapshot.State == goal.StateBlocked {
		if snapshot.Blocker == nil || snapshot.Blocker.Kind == goal.BlockOutcomeUnknown {
			return goal.ErrOutcomeUnknown
		}
		if err := runtime.ResolveBlock(context.Background(), goal.BlockResolution{
			Reference: snapshot.Blocker.Reference,
			Reason:    "fresh criterion-bound Cortex verification superseded the blocker",
			Evidence:  fmt.Sprintf("Cortex case %s revision %d is complete and verified", snapshot.Cortex.TaskID, advice.Revision),
		}); err != nil {
			return err
		}
	}
	return runtime.Complete(context.Background(), goal.CompletionRequest{
		ValidatedBy: fmt.Sprintf("cortex:%s@%d", snapshot.Cortex.TaskID, advice.Revision),
		Summary:     fallbackGoalText(advice.Summary, "Cortex verified the goal"),
		Results:     results,
	})
}

func boundGoalEvidence(value string) string {
	return boundGoalText(value, goal.MaxEvidenceBytes)
}

func boundGoalText(value string, limit int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "�"))
	if limit <= 0 || len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && cut < len(value) && value[cut]&0xc0 == 0x80 {
		cut--
	}
	return value[:cut]
}

func (m *Model) prepareManualGoalResume(before goal.Snapshot, advice goaladvisor.Advice) error {
	current, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		return err
	}
	if current.State == goal.StateBlocked {
		if current.Blocker == nil || current.Blocker.Kind == goal.BlockOutcomeUnknown {
			return goal.ErrOutcomeUnknown
		}
		if advice.PendingDecision || strings.EqualFold(advice.Phase, "needs_human_decision") || strings.EqualFold(advice.Phase, "blocked") {
			return fmt.Errorf("cortex still reports the blocker")
		}
		if err := m.goalRuntime.ResolveBlock(context.Background(), goal.BlockResolution{
			Reference: current.Blocker.Reference,
			Reason:    "Cortex status no longer reports the blocker",
			Evidence:  fallbackGoalText(advice.Summary, "fresh Cortex status"),
		}); err != nil {
			return err
		}
		current, err = m.goalRuntime.Snapshot(context.Background())
		if err != nil {
			return err
		}
	}
	if current.State == goal.StateExhausted {
		return goal.ErrBudgetExhausted
	}
	if current.State == goal.StatePaused {
		return m.goalRuntime.Resume(context.Background(), "user explicitly resumed after reviewing Cortex status")
	}
	if current.State == goal.StateActive {
		return nil
	}
	return fmt.Errorf("cannot resume a %s goal", before.State)
}

func (m *Model) showGoal() tea.Cmd {
	if m.goalRuntime == nil {
		m.appendGoalSystem("No goal is configured. Start one with /goal new.")
		return nil
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		m.appendGoalError("Read goal: " + err.Error())
		return nil
	}
	recoveryCommand := m.ensureGoalRecoveryProjection(snapshot, false)
	m.renderGoalInspector(snapshot)
	return recoveryCommand
}

func (m *Model) rejectPromptWhileGoalAttached(prompt string, generated bool) tea.Cmd {
	if m == nil || m.goalRuntime == nil {
		return nil
	}
	message := "Goal attached · draft not sent. Use the Goal Inspector actions, or /new for unrelated work."
	if generated {
		if m.preserveGeneratedPrompt(prompt) {
			message = "Goal attached · generated prompt not sent; it remains in the composer. Use the Goal Inspector actions, or /new for unrelated work."
		} else {
			message = "Goal attached · generated prompt not sent. Use the Goal Inspector actions, or /new for unrelated work."
		}
	}
	m.appendGoalError(message)
	return m.showGoal()
}

func (m *Model) preserveGeneratedPrompt(prompt string) bool {
	trimmed := strings.TrimSpace(prompt)
	if m == nil || trimmed == "" || strings.HasPrefix(trimmed, "/") || strings.TrimSpace(m.input.Value()) != "" {
		return false
	}
	if !pasteInsertionFits(0, 1, prompt, m.input.CharLimit) {
		return false
	}
	m.input.SetValue(prompt)
	if strings.TrimSpace(m.input.Value()) == "" {
		return false
	}
	m.input.CursorEnd()
	_ = m.reflowInputViewport()
	return true
}

func (m *Model) renderGoalInspector(snapshot goal.Snapshot) {
	m.preemptTranscriptSearch()
	// A newly attached/restored goal adds one stable status row. Reconcile the
	// transcript height before centering the inspector so a 30x12 view retains
	// the terminal safety row.
	m.recalcViewportHeight()
	actions := make([]command.ActionState, 0, 4)
	if m.cmdRegistry != nil {
		for _, action := range m.cmdRegistry.Actions("goal", m.buildCommandContext()) {
			switch action.Spec.ID {
			case command.GoalActionPause, command.GoalActionResume, command.GoalActionBudget, command.GoalActionDrop:
				actions = append(actions, action)
			}
		}
	}
	actions, recoveryStatus := m.decorateGoalInspectorRecovery(snapshot, actions)
	m.goalInspectorState = NewGoalInspector(snapshot, actions, GoalInspectorOptions{
		Width: m.width, Height: m.height, IsDark: m.isDark,
		ReducedMotion: m.reducedMotion, Now: m.nowTime(), PersistenceDirty: m.goalPersistenceDirty,
		RecoveryStatus: recoveryStatus, GlyphProfile: m.glyphProfile,
	})
	m.overlayParent = OverlayNone
	m.overlay = OverlayGoalInspector
	m.input.Blur()
}

func (m *Model) closeGoalInspector() {
	m.goalInspectorState = nil
	m.closeOverlayToParent()
}

func (m *Model) pauseGoal() {
	if m.goalRuntime == nil {
		m.appendGoalError("No goal is configured.")
		return
	}
	if err := m.goalRuntime.Pause(context.Background(), "paused by user"); err != nil {
		m.appendGoalError("Pause goal: " + err.Error())
		return
	}
	if err := m.persistGoalSession(); err != nil {
		m.appendGoalError("Save paused goal: " + err.Error())
		return
	}
	m.appendGoalSystem("Goal paused. Resume with /goal resume.")
}

func (m *Model) resumeGoal() tea.Cmd {
	if m.goalRuntime == nil {
		m.appendGoalError("No goal is configured.")
		return nil
	}
	if m.goalPersistenceDirty {
		if err := m.persistGoalSession(); err != nil {
			m.appendGoalError("Retry goal persistence: " + err.Error())
			return nil
		}
		m.appendGoalSystem("Goal persistence recovered; evaluating the requested resume.")
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		m.appendGoalError("Read goal: " + err.Error())
		return nil
	}
	if snapshot.PendingContinuation != nil {
		m.appendGoalError("Goal has an admitted turn without a settled receipt. Restore or reconcile it before retrying.")
		return nil
	}
	switch snapshot.State {
	case goal.StateCompleted, goal.StateDropped:
		m.appendGoalError(fmt.Sprintf("Goal is already %s.", snapshot.State))
		return nil
	case goal.StateExhausted:
		m.appendGoalError("Goal budget is exhausted. Increase it with /goal budget, then resume.")
		return nil
	case goal.StateBlocked:
		if snapshot.Blocker == nil || snapshot.Blocker.Kind == goal.BlockOutcomeUnknown {
			m.appendGoalError("Goal has an outcome-unknown effect and cannot be retried automatically. Reconcile the workspace first.")
			return nil
		}
		if snapshot.Cortex.TaskID == "" || m.goalAdvisor == nil {
			m.appendGoalError("The blocker requires a fresh Cortex status before resume.")
			return nil
		}
		return m.beginGoalEvaluation(true)
	case goal.StatePaused:
		if err := m.goalRuntime.Resume(context.Background(), "user explicitly resumed the goal"); err != nil {
			m.appendGoalError("Resume goal: " + err.Error())
			return nil
		}
		if err := m.persistGoalSession(); err != nil {
			rollbackErr := m.restoreGoalSnapshot(snapshot)
			m.appendGoalError("Save resumed goal: " + errors.Join(err, rollbackErr).Error())
			return nil
		}
		if snapshot.Cortex.TaskID == "" && m.goalAdvisor != nil {
			return m.beginGoalOpen(true)
		}
		return m.beginGoalEvaluation(true)
	case goal.StateActive:
		if snapshot.Cortex.TaskID == "" && m.goalAdvisor != nil {
			return m.beginGoalOpen(true)
		}
		return m.beginGoalEvaluation(true)
	default:
		m.appendGoalError("Goal cannot be resumed from " + string(snapshot.State) + ".")
		return nil
	}
}

func (m *Model) dropGoal() {
	if m.goalRuntime == nil {
		m.appendGoalError("No goal is configured.")
		return
	}
	if m.goalOperationRunning || m.cortexDecisionOp != nil {
		m.appendGoalError("Wait for the active goal operation before dropping the goal.")
		return
	}
	before, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		m.appendGoalError("Read goal before drop: " + err.Error())
		return
	}
	workspaceID, dismissals, err := m.prepareCortexDecisionDropResolutions(before)
	if err != nil {
		m.appendGoalError("Prepare goal drop: " + err.Error())
		return
	}
	attemptBefore := cloneCortexDecisionAttempt(m.cortexDecisionAttempt)
	if err := m.goalRuntime.Drop(context.Background(), "dropped by user"); err != nil {
		m.appendGoalError("Drop goal: " + err.Error())
		return
	}
	// A dropped goal cannot retain an answer fence: restore rejects that
	// combination, and the user-drop receipts settle any still-pending durable
	// controls in the same transaction as this fence-free snapshot.
	m.cortexDecisionAttempt = nil
	if err := m.persistDroppedGoalWithCortexDismissals(workspaceID, dismissals); err != nil {
		rollbackErr := m.restoreGoalSnapshot(before)
		m.cortexDecisionAttempt = attemptBefore
		m.appendGoalError("Save dropped goal: " + errors.Join(err, rollbackErr).Error())
		return
	}
	m.cortexDecisionGen++
	m.cortexDecisionAttempt = nil
	m.clearCortexDecisionPresentation()
	m.appendGoalSystem("Goal dropped without claiming completion.")
}

func (m *Model) goalStatusSummary() (GoalSummary, bool) {
	if m.goalRuntime == nil {
		return GoalSummary{}, false
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		return GoalSummary{}, false
	}
	phase := GoalPhase(snapshot.State)
	if m.goalPersistenceDirty && !snapshot.State.Terminal() {
		phase = GoalPhaseBlocked
	}
	return GoalSummary{
		Objective:   snapshot.Objective,
		Phase:       phase,
		TurnsUsed:   snapshot.Usage.ContinuationTurns,
		TurnBudget:  snapshot.Budget.MaxContinuationTurns,
		TokensUsed:  snapshot.Usage.EvalTokens,
		TokenBudget: snapshot.Budget.MaxEvalTokens,
		Elapsed:     max(time.Duration(0), m.nowTime().Sub(snapshot.CreatedAt)),
		TimeBudget:  snapshot.Budget.MaxWallTime,
	}, true
}

// recoverRestoredGoal deliberately never restarts provider work. A persisted
// turn admission without a settled receipt is outcome-unknown; an otherwise active
// goal becomes paused until the user explicitly resumes it.
func (m *Model) recoverRestoredGoal() error {
	if m.goalRuntime == nil {
		return nil
	}
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil {
		return err
	}
	changed := false
	if snapshot.PendingContinuation != nil {
		permit := snapshot.PendingContinuation
		err = m.goalRuntime.RecoverPendingContinuation(context.Background(), goal.PendingRecovery{
			TurnID: permit.TurnID, Kind: goal.PendingOutcomeUnknown,
			Reason:     "session restored with an unsettled goal turn admission",
			Evidence:   "the prior process ended without a durable goal turn receipt, so provider dispatch cannot be ruled out",
			OutcomeRef: permit.TurnID,
		})
		changed = err == nil
		if err == nil {
			m.appendGoalError("Goal recovery blocked: an admitted turn has no settled receipt. Inspect and reconcile before retrying.")
		}
	} else if snapshot.State == goal.StateActive {
		err = m.goalRuntime.Pause(context.Background(), "session restored; resume explicitly")
		changed = err == nil
		if err == nil {
			m.appendGoalSystem("Goal restored in a paused state. Resume explicitly with /goal resume.")
		}
	}
	if changed {
		err = errors.Join(err, m.persistGoalSession())
	}
	return err
}
