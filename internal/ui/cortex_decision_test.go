package ui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/goaladvisor"
	"github.com/abdul-hamid-achik/sonar/internal/supervisor"
)

type cortexDecisionTestAdvisor struct {
	mu       sync.Mutex
	answers  []goaladvisor.AnswerDecisionRequest
	statuses []string
	answer   func(context.Context, goaladvisor.AnswerDecisionRequest) (goaladvisor.Advice, error)
	status   func(context.Context, string) (goaladvisor.Advice, error)
}

func (*cortexDecisionTestAdvisor) Open(context.Context, goaladvisor.OpenRequest) (goaladvisor.Advice, error) {
	return goaladvisor.Advice{}, nil
}

func (a *cortexDecisionTestAdvisor) Status(ctx context.Context, taskID string) (goaladvisor.Advice, error) {
	a.mu.Lock()
	a.statuses = append(a.statuses, taskID)
	fn := a.status
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, taskID)
	}
	return goaladvisor.Advice{OK: true, TaskID: taskID, Phase: "investigating"}, nil
}

func (a *cortexDecisionTestAdvisor) AnswerDecision(ctx context.Context, request goaladvisor.AnswerDecisionRequest) (goaladvisor.Advice, error) {
	a.mu.Lock()
	a.answers = append(a.answers, request)
	fn := a.answer
	a.mu.Unlock()
	if fn != nil {
		return fn(ctx, request)
	}
	return goaladvisor.Advice{OK: true, TaskID: request.TaskID, Phase: "investigating"}, nil
}

func (a *cortexDecisionTestAdvisor) calls() (answers []goaladvisor.AnswerDecisionRequest, statuses []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]goaladvisor.AnswerDecisionRequest(nil), a.answers...), append([]string(nil), a.statuses...)
}

func cortexDecisionFixture(secret string) *goaladvisor.PendingDecision {
	return &goaladvisor.PendingDecision{
		ID:          "decision_exact",
		Question:    "Choose the safe rollout " + secret,
		Requester:   "cortex-planner",
		RequestedAt: time.Date(2026, time.July, 13, 12, 30, 0, 0, time.UTC),
		Status:      goaladvisor.DecisionStatusPending,
		Sensitive:   true,
		Options: []goaladvisor.DecisionOption{
			{ID: "two_step", Label: "Two-step " + secret, Consequence: "Canary first " + secret},
			{ID: "one_step", Label: "One-step " + secret, Consequence: "All at once " + secret},
		},
	}
}

func installCortexDecisionForTest(
	t *testing.T,
	m *Model,
	decision *goaladvisor.PendingDecision,
) (*db.Store, goaladvisor.Advice) {
	t.Helper()
	store, sessionID := attachGoalTestSession(t, m)
	m.goalRuntime = newUIGoalRuntime(t, sessionID, goal.BudgetLimits{MaxContinuationTurns: 3})
	if err := m.goalRuntime.AttachCortex(context.Background(), goal.CortexCorrelation{
		TaskID: "task_decision", Revision: 4, Actor: goalActor,
	}); err != nil {
		t.Fatal(err)
	}
	advice := goaladvisor.Advice{
		OK: true, TaskID: "task_decision", Revision: 5,
		Phase: "needs_human_decision", PendingDecision: true, Decision: decision,
	}
	token := beginGoalOperationForTest(t, m, "Checking goal")
	if cmd := m.handleGoalStatusResult(goalStatusResultMsg{Token: token, Advice: advice}); cmd != nil {
		t.Fatal("pending decision scheduled provider work")
	}
	if !m.cortexDecisionActive() {
		t.Fatalf("pending decision did not open inline: overlay=%v decision=%v", m.overlay, m.cortexDecision != nil)
	}
	return store, advice
}

func TestCortexDecisionRendersInlineAtSupportedSizesWithoutPreselection(t *testing.T) {
	for _, size := range []struct {
		name          string
		width, height int
	}{
		{name: "minimum", width: 30, height: 12},
		{name: "wide", width: 120, height: 36},
	} {
		t.Run(size.name, func(t *testing.T) {
			m := resizeInlineFormTestModel(t, size.width, size.height)
			m.setTestTranscriptContent("VISIBLE TRANSCRIPT")
			m.input.SetValue("preserved draft")
			presentation, err := newCortexDecisionPresentation(
				"task_decision", *cortexDecisionFixture("display"),
				size.width, size.height, m.isDark, m.themeID, true,
			)
			if err != nil {
				t.Fatal(err)
			}
			m.cortexDecision = presentation
			m.overlay = OverlayCortexDecision
			m.input.Blur()
			m.recalcViewportHeight()

			view := m.View()
			plain := ansi.Strip(view.Content)
			if presentation.Selected != -1 || (size.name == "minimum" && !strings.Contains(plain, "No option selected")) {
				t.Fatalf("decision was preselected: selected=%d\n%s", presentation.Selected, plain)
			}
			for _, text := range []string{"VISIBLE TRANSCRIPT", "Cortex decision", "Q ·", "enter confirm", "sensitive"} {
				if !strings.Contains(plain, text) {
					t.Fatalf("minimum decision omitted %q:\n%s", text, plain)
				}
			}
			if view.Cursor != nil || m.input.Value() != "preserved draft" || m.viewport.Height() < 1 {
				t.Fatalf("inline ownership cursor=%v draft=%q transcriptRows=%d", view.Cursor, m.input.Value(), m.viewport.Height())
			}
			assertInlineFormFrameOwnsPane(t, m, view.Content)
			assertRenderedLinesFit(t, view.Content, size.width)
			assertRenderedHeightFits(t, view.Content, size.height)

			updated, cmd := m.Update(enterKey())
			m = updated.(*Model)
			if cmd != nil || !strings.Contains(m.cortexDecision.Notice, "Choose an option") {
				t.Fatalf("Enter without selection confirmed: cmd=%v notice=%q", cmd != nil, m.cortexDecision.Notice)
			}
			updated, _ = m.Update(downKey())
			m = updated.(*Model)
			if m.cortexDecision.Selected != 0 || !strings.Contains(ansi.Strip(m.cortexDecision.View("")), "Canary first") {
				t.Fatalf("navigation did not expose consequence: selected=%d\n%s", m.cortexDecision.Selected, ansi.Strip(m.cortexDecision.View("")))
			}
		})
	}
}

func TestCortexDecisionAnswerResolvesExactDurableItemWithoutProseAndStaysBlocked(t *testing.T) {
	const secret = "PRIVATE-ANSWER-PROSE-71d2"
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	advisor := &cortexDecisionTestAdvisor{}
	m.goalAdvisor = advisor
	store, _ := installCortexDecisionForTest(t, m, cortexDecisionFixture(secret))
	defer func() { _ = store.Close() }()

	updated, _ := m.Update(downKey())
	m = updated.(*Model)
	updated, cmd := m.Update(enterKey())
	m = updated.(*Model)
	if cmd == nil || !m.cortexDecision.Answering || m.input.Focused() {
		t.Fatalf("answer did not take async ownership: cmd=%v answering=%v focused=%v", cmd != nil, m.cortexDecision.Answering, m.input.Focused())
	}
	// Escape is presentation-only even while the shared operation is running.
	updated, escapeCmd := m.Update(escKey())
	m = updated.(*Model)
	if escapeCmd != nil || m.cortexDecision != nil || m.overlay != OverlayNone || !m.goalOperationRunning {
		t.Fatalf("escape cancelled answer: cmd=%v decision=%v overlay=%v running=%v", escapeCmd != nil, m.cortexDecision != nil, m.overlay, m.goalOperationRunning)
	}

	result := awaitCommandMessage[cortexDecisionAnswerResultMsg](t, commandMessages(cmd), time.Second)
	stale := result
	stale.Generation++
	if staleCmd := m.handleCortexDecisionAnswerResult(stale); staleCmd != nil || !m.goalOperationRunning {
		t.Fatal("stale answer receipt changed operation ownership")
	}
	if finalCmd := m.handleCortexDecisionAnswerResult(result); finalCmd != nil {
		t.Fatal("successful answer scheduled provider work")
	}
	requests, _ := advisor.calls()
	if len(requests) != 1 || requests[0] != (goaladvisor.AnswerDecisionRequest{
		TaskID: "task_decision", DecisionID: "decision_exact",
		OptionID: "two_step", Responder: cortexDecisionResponder,
	}) {
		t.Fatalf("answer request = %#v", requests)
	}
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	if snapshot.State != goal.StateBlocked || snapshot.Blocker == nil || snapshot.Blocker.Kind != goal.BlockDecision {
		t.Fatalf("answer resumed local goal: %#v", snapshot)
	}
	if client.calls.Load() != 0 || m.goalOperationRunning {
		t.Fatalf("answer dispatched provider=%d or left operation running=%v", client.calls.Load(), m.goalOperationRunning)
	}
	if m.cortexDecisionAttempt == nil {
		t.Fatal("successful answer cleared the no-retry fence before fresh Cortex status")
	}

	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: snapshot.SessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindCortexDecision, GoalID: snapshot.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Resolution == nil || states[0].Pending() {
		t.Fatalf("durable answer state = %#v", states)
	}
	var evidence map[string]any
	if err := json.Unmarshal([]byte(states[0].Resolution.EvidenceJSON), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence["selected_option_id"] != "two_step" || evidence["resolution_source"] != "local_answer_decision" {
		t.Fatalf("durable answer evidence = %#v", evidence)
	}
	encoded := states[0].Item.PayloadJSON + states[0].Resolution.EvidenceJSON
	for _, forbidden := range []string{secret, "Two-step", "Canary first", "Choose the safe rollout"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("durable answer persisted prose %q: %s", forbidden, encoded)
		}
	}
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Content, "two_step") ||
		strings.Contains(m.entries[len(m.entries)-1].Content, secret) {
		t.Fatalf("safe answer receipt = %#v", m.entries)
	}
}

func TestCortexDecisionSuccessfulAnswerSamePendingStatusRemainsReconciliationOnly(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	advisor := &cortexDecisionTestAdvisor{}
	m.goalAdvisor = advisor
	store, pendingAdvice := installCortexDecisionForTest(t, m, cortexDecisionFixture("stale-status"))
	defer func() { _ = store.Close() }()

	updated, _ := m.Update(downKey())
	m = updated.(*Model)
	updated, answerCmd := m.Update(enterKey())
	m = updated.(*Model)
	answerResult := awaitCommandMessage[cortexDecisionAnswerResultMsg](t, commandMessages(answerCmd), time.Second)
	m.handleCortexDecisionAnswerResult(answerResult)
	if m.cortexDecisionAttempt == nil || m.cortexDecision != nil {
		t.Fatalf("successful answer fence/presentation = attempt=%#v decision=%v", m.cortexDecisionAttempt, m.cortexDecision != nil)
	}

	advisor.status = func(context.Context, string) (goaladvisor.Advice, error) { return pendingAdvice, nil }
	refreshCmd := m.resumeGoal()
	if refreshCmd == nil {
		t.Fatal("blocked decision did not request fresh status")
	}
	statusResult := awaitCommandMessage[goalStatusResultMsg](t, commandMessages(refreshCmd), time.Second)
	if statusResult.ExpectedRequestSHA256 == "" || statusResult.ExpectedDecisionID != "decision_exact" {
		t.Fatalf("fresh status lost the successful answer fence: %#v", statusResult)
	}
	m.handleGoalStatusResult(statusResult)
	if m.cortexDecision == nil || !m.cortexDecision.OutcomeUnknown || m.cortexDecisionAttempt == nil {
		t.Fatalf("same pending status reopened a successful answer: decision=%#v attempt=%#v", m.cortexDecision, m.cortexDecisionAttempt)
	}
	updated, _ = m.Update(downKey())
	m = updated.(*Model)
	updated, retryCmd := m.Update(enterKey())
	m = updated.(*Model)
	answers, statuses := advisor.calls()
	if retryCmd != nil || len(answers) != 1 || len(statuses) != 1 || m.cortexDecision.Selected != -1 {
		t.Fatalf("same pending status enabled duplicate answer: cmd=%v answers=%#v statuses=%#v selected=%d", retryCmd != nil, answers, statuses, m.cortexDecision.Selected)
	}

	// A later fresh status may legitimately advance straight to another decision.
	// The already-answered old item permits that new request without weakening the
	// same-request fence above.
	nextDecision := cortexDecisionFixture("after-success")
	nextDecision.ID = "decision_after_success"
	advisor.status = func(context.Context, string) (goaladvisor.Advice, error) {
		return goaladvisor.Advice{
			OK: true, TaskID: "task_decision", Revision: 6,
			Phase: "needs_human_decision", PendingDecision: true, Decision: nextDecision,
		}, nil
	}
	updated, nextCmd := m.Update(charKey('r'))
	m = updated.(*Model)
	m.handleGoalStatusResult(awaitCommandMessage[goalStatusResultMsg](t, commandMessages(nextCmd), time.Second))
	if m.cortexDecision == nil || m.cortexDecision.Decision.ID != "decision_after_success" ||
		m.cortexDecision.OutcomeUnknown || m.cortexDecisionAttempt != nil {
		t.Fatalf("new request after successful answer was not admitted safely: decision=%#v attempt=%#v", m.cortexDecision, m.cortexDecisionAttempt)
	}
}

func TestCortexDecisionAnswerErrorIsOutcomeUnknownAndRefreshOnly(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	decision := cortexDecisionFixture("error-secret")
	advisor := &cortexDecisionTestAdvisor{
		answer: func(context.Context, goaladvisor.AnswerDecisionRequest) (goaladvisor.Advice, error) {
			return goaladvisor.Advice{}, errors.New("private transport detail")
		},
	}
	m.goalAdvisor = advisor
	store, pendingAdvice := installCortexDecisionForTest(t, m, decision)
	defer func() { _ = store.Close() }()

	updated, _ := m.Update(downKey())
	m = updated.(*Model)
	updated, cmd := m.Update(enterKey())
	m = updated.(*Model)
	result := awaitCommandMessage[cortexDecisionAnswerResultMsg](t, commandMessages(cmd), time.Second)
	m.handleCortexDecisionAnswerResult(result)
	if m.cortexDecision == nil || !m.cortexDecision.OutcomeUnknown || m.input.Focused() {
		t.Fatalf("error focus/state = decision=%v unknown=%v focused=%v", m.cortexDecision != nil, m.cortexDecision != nil && m.cortexDecision.OutcomeUnknown, m.input.Focused())
	}
	unknownCount := 0
	for _, entry := range m.entries {
		if entry.Content == "Outcome unknown — refresh Cortex status" {
			unknownCount++
		}
		if strings.Contains(entry.Content, "private transport detail") {
			t.Fatalf("raw answer error leaked: %#v", entry)
		}
	}
	if unknownCount != 1 {
		t.Fatalf("outcome-unknown receipts = %d, entries=%#v", unknownCount, m.entries)
	}

	advisor.status = func(context.Context, string) (goaladvisor.Advice, error) { return pendingAdvice, nil }
	updated, refreshCmd := m.Update(charKey('r'))
	m = updated.(*Model)
	if refreshCmd == nil || !m.cortexDecision.Refreshing || m.input.Focused() {
		t.Fatalf("refresh ownership cmd=%v refreshing=%v focused=%v", refreshCmd != nil, m.cortexDecision.Refreshing, m.input.Focused())
	}
	statusResult := awaitCommandMessage[goalStatusResultMsg](t, commandMessages(refreshCmd), time.Second)
	m.handleGoalStatusResult(statusResult)
	answers, statuses := advisor.calls()
	if len(answers) != 1 || len(statuses) != 1 || statuses[0] != "task_decision" {
		t.Fatalf("refresh retried answer or wrong status: answers=%#v statuses=%#v", answers, statuses)
	}
	if m.cortexDecision == nil || !m.cortexDecision.OutcomeUnknown || m.cortexDecision.Selected != -1 {
		t.Fatalf("same pending request escaped no-retry reconciliation: %#v", m.cortexDecision)
	}
	updated, _ = m.Update(downKey())
	m = updated.(*Model)
	updated, retryCmd := m.Update(enterKey())
	m = updated.(*Model)
	if retryCmd != nil || m.cortexDecision.Selected != -1 {
		t.Fatalf("same-request refresh enabled a blind retry: cmd=%v selected=%d", retryCmd != nil, m.cortexDecision.Selected)
	}

	// The safe pre-dispatch fence—not the question/options—survives restart, so
	// fresh Status of the same request remains reconciliation-only.
	raw, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "error-secret") || !strings.Contains(raw, "cortex_decision_attempt") {
		t.Fatalf("restart fence persistence leaked prose or was absent: %s", raw)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	m.cortexDecision = nil
	m.cortexDecisionAttempt = nil
	if err := m.restoreSessionState(state); err != nil {
		t.Fatal(err)
	}
	if m.cortexDecision != nil || m.cortexDecisionAttempt == nil {
		t.Fatalf("restart restored prose or lost no-retry fence: decision=%v attempt=%#v", m.cortexDecision != nil, m.cortexDecisionAttempt)
	}
	restartCmd := m.beginRestoredCortexDecisionRefresh()
	if restartCmd == nil {
		t.Fatal("restart did not schedule fresh Cortex status")
	}
	restartStatus := awaitCommandMessage[goalStatusResultMsg](t, commandMessages(restartCmd), time.Second)
	m.handleGoalStatusResult(restartStatus)
	if m.cortexDecision == nil || !m.cortexDecision.OutcomeUnknown {
		t.Fatalf("restart status enabled same-request retry: %#v", m.cortexDecision)
	}
}

func TestDropGoalDismissesPendingCortexDecisionClearsFenceAndRestartsWithoutOrphan(t *testing.T) {
	const secret = "DROP-PRIVATE-DECISION-6d91"
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.goalAdvisor = &cortexDecisionTestAdvisor{
		answer: func(context.Context, goaladvisor.AnswerDecisionRequest) (goaladvisor.Advice, error) {
			return goaladvisor.Advice{}, errors.New("ambiguous answer transport")
		},
	}
	store, _ := installCortexDecisionForTest(t, m, cortexDecisionFixture(secret))
	defer func() { _ = store.Close() }()

	updated, _ := m.Update(downKey())
	m = updated.(*Model)
	updated, answerCmd := m.Update(enterKey())
	m = updated.(*Model)
	m.handleCortexDecisionAnswerResult(awaitCommandMessage[cortexDecisionAnswerResultMsg](t, commandMessages(answerCmd), time.Second))
	if m.cortexDecisionAttempt == nil || m.cortexDecision == nil || !m.cortexDecision.OutcomeUnknown {
		t.Fatalf("drop fixture lacks ambiguous decision fence: decision=%#v attempt=%#v", m.cortexDecision, m.cortexDecisionAttempt)
	}

	m.dropGoal()
	dropped := snapshotUIGoal(t, m.goalRuntime)
	if dropped.State != goal.StateDropped || m.cortexDecisionAttempt != nil || m.cortexDecision != nil || m.overlay == OverlayCortexDecision {
		t.Fatalf("drop retained decision authority: goal=%#v decision=%#v attempt=%#v overlay=%v", dropped, m.cortexDecision, m.cortexDecisionAttempt, m.overlay)
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: dropped.SessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindCortexDecision, GoalID: dropped.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Resolution == nil ||
		states[0].Resolution.Outcome != controlplane.OutcomeDismissed ||
		states[0].Resolution.Detail != cortexDecisionDropResolutionDetail {
		t.Fatalf("dropped Cortex decision state = %#v", states)
	}
	var evidence map[string]any
	if err := json.Unmarshal([]byte(states[0].Resolution.EvidenceJSON), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence["resolution_source"] != cortexDecisionDropResolutionSource || evidence["goal_dropped"] != true {
		t.Fatalf("drop evidence = %#v", evidence)
	}
	issues, err := supervisor.IssuesFromControlStates(states)
	if err != nil || len(issues) != 0 {
		t.Fatalf("dropped decision remained a supervisor issue: issues=%#v err=%v", issues, err)
	}
	pending, err := store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: dropped.SessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindCortexDecision, GoalID: dropped.ID, PendingOnly: true, Limit: 10,
	})
	if err != nil || len(pending) != 0 {
		t.Fatalf("dropped decision remained pending: states=%#v err=%v", pending, err)
	}

	record, err := store.GetSessionStateRecord(context.Background(), dropped.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := decodeSessionState(record.StateJSON)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Goal == nil || durable.Goal.State != goal.StateDropped || durable.CortexDecisionAttempt != nil {
		t.Fatalf("durable dropped session retained authority: %#v", durable)
	}
	encoded := record.StateJSON + states[0].Item.PayloadJSON + states[0].Resolution.EvidenceJSON
	if strings.Contains(encoded, secret) || strings.Contains(encoded, "Choose the safe rollout") || strings.Contains(encoded, "Canary first") {
		t.Fatalf("drop persistence leaked decision prose: %s", encoded)
	}
	restarted := newGoalRuntimeTestModel(t, &goalCountingClient{})
	if err := restarted.restoreSessionState(durable); err != nil {
		t.Fatal(err)
	}
	restored := snapshotUIGoal(t, restarted.goalRuntime)
	if restored.State != goal.StateDropped || restarted.cortexDecisionAttempt != nil || restarted.cortexDecision != nil {
		t.Fatalf("restart restored dropped decision authority: goal=%#v decision=%#v attempt=%#v", restored, restarted.cortexDecision, restarted.cortexDecisionAttempt)
	}
}

func TestDropGoalSessionCASFailureRollsBackGoalControlAndFence(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.goalAdvisor = &cortexDecisionTestAdvisor{
		answer: func(context.Context, goaladvisor.AnswerDecisionRequest) (goaladvisor.Advice, error) {
			return goaladvisor.Advice{}, errors.New("ambiguous answer transport")
		},
	}
	store, _ := installCortexDecisionForTest(t, m, cortexDecisionFixture("drop-conflict"))
	defer func() { _ = store.Close() }()
	updated, _ := m.Update(downKey())
	m = updated.(*Model)
	updated, answerCmd := m.Update(enterKey())
	m = updated.(*Model)
	m.handleCortexDecisionAnswerResult(awaitCommandMessage[cortexDecisionAnswerResultMsg](t, commandMessages(answerCmd), time.Second))
	before := snapshotUIGoal(t, m.goalRuntime)
	attemptBefore := cloneCortexDecisionAttempt(m.cortexDecisionAttempt)
	presentationBefore := m.cortexDecision
	if attemptBefore == nil || presentationBefore == nil {
		t.Fatal("drop conflict fixture lacks decision authority")
	}

	record, err := store.GetSessionStateRecord(context.Background(), before.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSessionStateCAS(context.Background(), before.SessionID, record.Revision, record.StateJSON); err != nil {
		t.Fatal(err)
	}
	m.dropGoal()

	after := snapshotUIGoal(t, m.goalRuntime)
	if after.ID != before.ID || after.State != before.State || after.Blocker == nil || before.Blocker == nil ||
		after.Blocker.Kind != before.Blocker.Kind || after.Blocker.Reference != before.Blocker.Reference {
		t.Fatalf("failed drop did not restore goal: before=%#v after=%#v", before, after)
	}
	if m.cortexDecisionAttempt == nil || *m.cortexDecisionAttempt != *attemptBefore ||
		m.cortexDecision != presentationBefore || m.overlay != OverlayCortexDecision {
		t.Fatalf("failed drop did not restore decision authority: decision=%#v attempt=%#v overlay=%v", m.cortexDecision, m.cortexDecisionAttempt, m.overlay)
	}
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: before.SessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindCortexDecision, GoalID: before.ID, PendingOnly: true, Limit: 10,
	})
	if err != nil || len(states) != 1 || !states[0].Pending() {
		t.Fatalf("failed session CAS consumed the pending control: states=%#v err=%v", states, err)
	}
	m.sessionStateMu.RLock()
	known, dirty := m.sessionStateRevisionKnown, m.sessionStatePersistenceDirty
	m.sessionStateMu.RUnlock()
	if known || !dirty || !m.goalPersistenceDirty {
		t.Fatalf("failed drop persistence authority = revision-known=%v session-dirty=%v goal-dirty=%v", known, dirty, m.goalPersistenceDirty)
	}
	durableRecord, err := store.GetSessionStateRecord(context.Background(), before.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := decodeSessionState(durableRecord.StateJSON)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Goal == nil || durable.Goal.State != goal.StateBlocked || durable.CortexDecisionAttempt == nil {
		t.Fatalf("failed drop changed durable goal/fence: %#v", durable)
	}
}

func TestCortexDecisionFreshDifferentRequestSupersedesFenceAndShowsNewDecision(t *testing.T) {
	const oldSecret = "OLD-DECISION-PRIVATE-1bc2"
	const nextSecret = "NEXT-DECISION-PRIVATE-8a71"
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	advisor := &cortexDecisionTestAdvisor{
		answer: func(context.Context, goaladvisor.AnswerDecisionRequest) (goaladvisor.Advice, error) {
			return goaladvisor.Advice{}, errors.New("ambiguous transport failure")
		},
	}
	m.goalAdvisor = advisor
	store, _ := installCortexDecisionForTest(t, m, cortexDecisionFixture(oldSecret))
	defer func() { _ = store.Close() }()

	updated, _ := m.Update(downKey())
	m = updated.(*Model)
	updated, answerCmd := m.Update(enterKey())
	m = updated.(*Model)
	answerResult := awaitCommandMessage[cortexDecisionAnswerResultMsg](t, commandMessages(answerCmd), time.Second)
	m.handleCortexDecisionAnswerResult(answerResult)
	if m.cortexDecisionAttempt == nil || m.cortexDecision == nil || !m.cortexDecision.OutcomeUnknown {
		t.Fatalf("ambiguous answer did not retain its fence: decision=%#v attempt=%#v", m.cortexDecision, m.cortexDecisionAttempt)
	}

	nextDecision := cortexDecisionFixture(nextSecret)
	nextDecision.ID = "decision_next"
	nextAdvice := goaladvisor.Advice{
		OK: true, TaskID: "task_decision", Revision: 6,
		Phase: "needs_human_decision", PendingDecision: true, Decision: nextDecision,
	}
	advisor.status = func(context.Context, string) (goaladvisor.Advice, error) { return nextAdvice, nil }
	updated, refreshCmd := m.Update(charKey('r'))
	m = updated.(*Model)
	statusResult := awaitCommandMessage[goalStatusResultMsg](t, commandMessages(refreshCmd), time.Second)
	m.handleGoalStatusResult(statusResult)
	if m.cortexDecision == nil || m.cortexDecision.Decision.ID != "decision_next" ||
		m.cortexDecision.OutcomeUnknown || m.cortexDecision.Selected != -1 || m.cortexDecisionAttempt != nil {
		t.Fatalf("new decision did not replace the superseded fence safely: decision=%#v attempt=%#v", m.cortexDecision, m.cortexDecisionAttempt)
	}

	snapshot := snapshotUIGoal(t, m.goalRuntime)
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: snapshot.SessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindCortexDecision, GoalID: snapshot.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Fatalf("superseded/new decision states = %#v", states)
	}
	var oldState, nextState *controlplane.State
	for index := range states {
		switch states[index].Item.ExternalID {
		case "decision_exact":
			oldState = &states[index]
		case "decision_next":
			nextState = &states[index]
		}
	}
	if oldState == nil || oldState.Resolution == nil || oldState.Resolution.Outcome != controlplane.OutcomeDismissed ||
		nextState == nil || !nextState.Pending() {
		t.Fatalf("superseded/new durable states = old=%#v next=%#v", oldState, nextState)
	}
	if !strings.Contains(oldState.Resolution.EvidenceJSON, "cortex_status_superseded") {
		t.Fatalf("supersession evidence = %s", oldState.Resolution.EvidenceJSON)
	}
	encoded := oldState.Item.PayloadJSON + oldState.Resolution.EvidenceJSON + nextState.Item.PayloadJSON
	if strings.Contains(encoded, oldSecret) || strings.Contains(encoded, nextSecret) {
		t.Fatalf("supersession persisted decision prose: %s", encoded)
	}
}

func TestCortexDecisionSameIDChangedRequestRemainsFailClosed(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	advisor := &cortexDecisionTestAdvisor{
		answer: func(context.Context, goaladvisor.AnswerDecisionRequest) (goaladvisor.Advice, error) {
			return goaladvisor.Advice{}, errors.New("ambiguous transport failure")
		},
	}
	m.goalAdvisor = advisor
	store, _ := installCortexDecisionForTest(t, m, cortexDecisionFixture("original"))
	defer func() { _ = store.Close() }()
	updated, _ := m.Update(downKey())
	m = updated.(*Model)
	updated, answerCmd := m.Update(enterKey())
	m = updated.(*Model)
	m.handleCortexDecisionAnswerResult(awaitCommandMessage[cortexDecisionAnswerResultMsg](t, commandMessages(answerCmd), time.Second))

	changed := cortexDecisionFixture("changed-request")
	changedAdvice := goaladvisor.Advice{
		OK: true, TaskID: "task_decision", Revision: 6,
		Phase: "needs_human_decision", PendingDecision: true, Decision: changed,
	}
	advisor.status = func(context.Context, string) (goaladvisor.Advice, error) { return changedAdvice, nil }
	updated, refreshCmd := m.Update(charKey('r'))
	m = updated.(*Model)
	m.handleGoalStatusResult(awaitCommandMessage[goalStatusResultMsg](t, commandMessages(refreshCmd), time.Second))
	if m.cortexDecision == nil || !m.cortexDecision.OutcomeUnknown || m.cortexDecisionAttempt == nil ||
		m.cortexDecision.Decision.Question == changed.Question {
		t.Fatalf("same-id changed request escaped immutable binding: decision=%#v attempt=%#v", m.cortexDecision, m.cortexDecisionAttempt)
	}
	answers, statuses := advisor.calls()
	if len(answers) != 1 || len(statuses) != 1 {
		t.Fatalf("changed-request reconciliation calls = answers=%#v statuses=%#v", answers, statuses)
	}
}

func TestCortexDecisionRefreshClearsToPausedWithoutProcessingNonProgressPhase(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	decision := cortexDecisionFixture("blocked-phase")
	advisor := &cortexDecisionTestAdvisor{
		status: func(context.Context, string) (goaladvisor.Advice, error) {
			return goaladvisor.Advice{
				OK: true, TaskID: "task_decision", Revision: 6,
				Phase: "blocked", Summary: "external dependency",
			}, nil
		},
	}
	m.goalAdvisor = advisor
	store, _ := installCortexDecisionForTest(t, m, decision)
	defer func() { _ = store.Close() }()

	cmd := m.resumeGoal()
	if cmd == nil {
		t.Fatal("first resume did not request fresh Cortex status")
	}
	message := awaitCommandMessage[goalStatusResultMsg](t, commandMessages(cmd), time.Second)
	if !message.DecisionOnly || !message.Manual {
		t.Fatalf("first resume lost decision-only boundary: %#v", message)
	}
	if next := m.handleGoalStatusResult(message); next != nil {
		t.Fatal("decision-clear status scheduled provider work")
	}
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	if snapshot.State != goal.StatePaused || snapshot.Blocker != nil {
		t.Fatalf("decision clear processed blocked phase or resumed: %#v", snapshot)
	}
	if client.calls.Load() != 0 || len(m.entries) == 0 ||
		m.entries[len(m.entries)-1].Content != "Decision cleared; run /goal resume again to process fresh Cortex status." {
		t.Fatalf("decision clear receipt/provider calls=%d entries=%#v", client.calls.Load(), m.entries)
	}
}

func TestCortexDecisionControlOperationsIgnoreExpiredWallBudget(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	advisor := &cortexDecisionTestAdvisor{
		status: func(context.Context, string) (goaladvisor.Advice, error) {
			return goaladvisor.Advice{OK: true, TaskID: "task_decision", Revision: 6, Phase: "investigating"}, nil
		},
	}
	m.goalAdvisor = advisor
	store, _ := installCortexDecisionForTest(t, m, cortexDecisionFixture("elapsed"))
	defer func() { _ = store.Close() }()
	if err := m.goalRuntime.AmendBudget(context.Background(), goal.BudgetLimits{
		MaxContinuationTurns: 3,
		MaxWallTime:          time.Hour,
	}, "exercise expired decision control operations"); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	m.now = func() time.Time { return snapshot.CreatedAt.Add(24 * time.Hour) }
	if _, err := goalAdvisorOperationDeadline(snapshot, m.nowTime()); !errors.Is(err, goal.ErrBudgetExhausted) {
		t.Fatalf("fixture did not expire the wall budget: %v", err)
	}

	updated, _ := m.Update(downKey())
	m = updated.(*Model)
	updated, cmd := m.Update(enterKey())
	m = updated.(*Model)
	if cmd == nil || !m.goalOperationRunning {
		t.Fatal("elapsed goal budget prevented the pending human answer")
	}
	result := awaitCommandMessage[cortexDecisionAnswerResultMsg](t, commandMessages(cmd), time.Second)
	m.handleCortexDecisionAnswerResult(result)
	if answers, _ := advisor.calls(); len(answers) != 1 {
		t.Fatalf("elapsed-budget answer calls = %#v", answers)
	}
	refreshCmd := m.resumeGoal()
	if refreshCmd == nil {
		t.Fatal("expired wall budget prevented decision-only status refresh")
	}
	statusResult := awaitCommandMessage[goalStatusResultMsg](t, commandMessages(refreshCmd), time.Second)
	if !statusResult.DecisionOnly {
		t.Fatalf("expired-budget refresh lost decision-only boundary: %#v", statusResult)
	}
	m.handleGoalStatusResult(statusResult)
	cleared := snapshotUIGoal(t, m.goalRuntime)
	if cleared.State != goal.StatePaused || cleared.Blocker != nil || m.cortexDecisionAttempt != nil {
		t.Fatalf("expired-budget refresh did not clear safely: %#v attempt=%#v", cleared, m.cortexDecisionAttempt)
	}
}

func TestCortexDecisionAdviceValidatorsRequireDomainOK(t *testing.T) {
	settled := goaladvisor.Advice{TaskID: "task_decision", Phase: "investigating"}
	if validSettledCortexDecisionAdvice(settled, "task_decision") {
		t.Fatal("OK=false answer advice was treated as domain success")
	}
	settled.OK = true
	if !validSettledCortexDecisionAdvice(settled, "task_decision") {
		t.Fatal("valid settled answer advice was rejected")
	}

	pending := cortexDecisionFixture("domain-ok")
	status := goaladvisor.Advice{
		TaskID: "task_decision", Phase: "needs_human_decision",
		PendingDecision: true, Decision: pending,
	}
	if validCortexDecisionStatusAdvice(status, "task_decision") {
		t.Fatal("OK=false status advice was treated as domain success")
	}
	status.OK = true
	if !validCortexDecisionStatusAdvice(status, "task_decision") {
		t.Fatal("valid pending decision status was rejected")
	}
}

func TestCortexDecisionShutdownCancelsInFlightAnswerAndLeavesItemPending(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	started := make(chan struct{})
	advisor := &cortexDecisionTestAdvisor{
		answer: func(ctx context.Context, _ goaladvisor.AnswerDecisionRequest) (goaladvisor.Advice, error) {
			close(started)
			<-ctx.Done()
			return goaladvisor.Advice{}, ctx.Err()
		},
	}
	m.goalAdvisor = advisor
	store, _ := installCortexDecisionForTest(t, m, cortexDecisionFixture("shutdown"))
	defer func() { _ = store.Close() }()
	updated, _ := m.Update(downKey())
	m = updated.(*Model)
	updated, answerCmd := m.Update(enterKey())
	m = updated.(*Model)
	messages := commandMessages(answerCmd)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("answer did not start")
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = updated.(*Model)
	result := awaitCommandMessage[cortexDecisionAnswerResultMsg](t, messages, time.Second)
	m.handleCortexDecisionAnswerResult(result)
	if !m.shuttingDown || m.goalOperationRunning {
		t.Fatalf("shutdown ownership = shutting=%v running=%v", m.shuttingDown, m.goalOperationRunning)
	}
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	workspaceID, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: snapshot.SessionID, WorkspaceID: workspaceID,
		Kind: controlplane.KindCortexDecision, GoalID: snapshot.ID, PendingOnly: true, Limit: 10,
	})
	if err != nil || len(states) != 1 {
		t.Fatalf("shutdown resolved pending decision: states=%#v err=%v", states, err)
	}
}

func TestCortexDecisionPresentationIsNotPersistedAcrossRestart(t *testing.T) {
	const secret = "NO-SESSION-PROSE-0ef4"
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	store, _ := installCortexDecisionForTest(t, m, cortexDecisionFixture(secret))
	defer func() { _ = store.Close() }()
	raw, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, secret) || strings.Contains(raw, "decision_exact") {
		t.Fatalf("session JSON retained decision presentation: %s", raw)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newGoalRuntimeTestModel(t, &goalCountingClient{})
	if err := restarted.restoreSessionState(state); err != nil {
		t.Fatal(err)
	}
	if restarted.cortexDecision != nil || restarted.overlay == OverlayCortexDecision {
		t.Fatalf("restart restored pending question/options: decision=%v overlay=%v", restarted.cortexDecision != nil, restarted.overlay)
	}
}
