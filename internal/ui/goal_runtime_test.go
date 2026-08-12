package ui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/goaladvisor"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/charmbracelet/x/ansi"
)

type goalCountingClient struct {
	calls atomic.Int64
}

type goalStaticAdvisor struct{}

func (*goalStaticAdvisor) Open(context.Context, goaladvisor.OpenRequest) (goaladvisor.Advice, error) {
	return goaladvisor.Advice{}, nil
}

func (*goalStaticAdvisor) Status(context.Context, string) (goaladvisor.Advice, error) {
	return goaladvisor.Advice{}, nil
}

type goalDeadlineAdvisor struct {
	openDeadline   chan time.Time
	statusDeadline chan time.Time
}

func newGoalDeadlineAdvisor() *goalDeadlineAdvisor {
	return &goalDeadlineAdvisor{
		openDeadline:   make(chan time.Time, 1),
		statusDeadline: make(chan time.Time, 1),
	}
}

func (a *goalDeadlineAdvisor) Open(ctx context.Context, _ goaladvisor.OpenRequest) (goaladvisor.Advice, error) {
	deadline, _ := ctx.Deadline()
	a.openDeadline <- deadline
	return goaladvisor.Advice{}, nil
}

func (a *goalDeadlineAdvisor) Status(ctx context.Context, taskID string) (goaladvisor.Advice, error) {
	deadline, _ := ctx.Deadline()
	a.statusDeadline <- deadline
	return goaladvisor.Advice{TaskID: taskID, Phase: "investigating"}, nil
}

func (c *goalCountingClient) ChatStream(_ context.Context, _ llm.ChatOptions, _ func(llm.StreamChunk) error) error {
	c.calls.Add(1)
	return nil
}

func (*goalCountingClient) Ping() error   { return nil }
func (*goalCountingClient) Model() string { return "goal-test-model" }
func (*goalCountingClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func newGoalRuntimeTestModel(t *testing.T, client llm.Client) *Model {
	t.Helper()
	m := newTestModel(t)
	m.agent = agent.New(client, nil, 16_384)
	m.agent.SetWorkDir(t.TempDir())
	m.reducedMotion = true
	return m
}

func newUIGoalRuntime(t *testing.T, sessionID int64, budget goal.BudgetLimits) *goal.Runtime {
	t.Helper()
	runtime, err := goal.New(goal.Spec{
		ID:        "goal_ui_test",
		SessionID: sessionID,
		Objective: "Ship a durable goal safely",
		AcceptanceCriteria: []goal.AcceptanceCriterion{
			{ID: "criterion_1", Description: "The durable receipt is verified"},
		},
		Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func snapshotUIGoal(t *testing.T, runtime *goal.Runtime) goal.Snapshot {
	t.Helper()
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func recordUIGoalTurn(t *testing.T, runtime *goal.Runtime, kind goal.TurnAdmissionKind, report goal.TurnReport) {
	t.Helper()
	if _, err := runtime.BeginTurn(context.Background(), report.TurnID, kind); err != nil {
		t.Fatalf("admit %s goal turn %s: %v", kind, report.TurnID, err)
	}
	if err := runtime.RecordTurn(context.Background(), report); err != nil {
		t.Fatalf("record %s goal turn %s: %v", kind, report.TurnID, err)
	}
}

func beginGoalOperationForTest(t *testing.T, m *Model, label string) uint64 {
	t.Helper()
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	token, _, err := m.beginGoalOperation(label, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestGoalNoToolTurnPausesWithoutSchedulingContinuation(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	if _, err := m.goalRuntime.BeginTurn(context.Background(), "turn_no_tool", goal.AdmissionInitial); err != nil {
		t.Fatal(err)
	}
	m.goalTurnID = "turn_no_tool"
	m.turnEvalTotal = 17
	m.state = StateStreaming
	m.streamBuf.WriteString("I cannot make concrete progress without more information.")

	updated, _ := m.Update(AgentDoneMsg{TurnID: "turn_no_tool"})
	m = updated.(*Model)

	snapshot := snapshotUIGoal(t, m.goalRuntime)
	if snapshot.State != goal.StatePaused {
		t.Fatalf("no-tool goal state = %s, want paused", snapshot.State)
	}
	if snapshot.LastTurn == nil || snapshot.LastTurn.Productive || snapshot.LastTurn.EvalTokens != 17 {
		t.Fatalf("no-tool receipt = %#v", snapshot.LastTurn)
	}
	if snapshot.PendingContinuation != nil || snapshot.Usage.ContinuationTurns != 0 {
		t.Fatalf("no-tool yield admitted a continuation: %#v", snapshot)
	}
	if m.goalNeedsEvaluation || m.goalTurnID != "" {
		t.Fatalf("no-tool yield left continuation state armed: evaluate=%v turn=%q", m.goalNeedsEvaluation, m.goalTurnID)
	}
	if got := client.calls.Load(); got != 0 {
		t.Fatalf("no-tool settlement dispatched %d provider calls", got)
	}
	if !strings.Contains(snapshot.StateReason, "unproductive turn") || !strings.Contains(snapshot.LastTurn.Summary, "without a concrete tool receipt") {
		t.Fatalf("no-tool pause is not actionable: reason=%q summary=%q", snapshot.StateReason, snapshot.LastTurn.Summary)
	}
}

func TestGoalUnresolvedOutcomeBlocksEvenAfterSuccessfulToolReceipt(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	runtime := newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: "turn_initial", Productive: true, Summary: "verified initial progress",
	})
	if _, err := runtime.BeginContinuation(context.Background(), "turn_unknown"); err != nil {
		t.Fatal(err)
	}
	m.goalRuntime = runtime
	m.goalTurnID = "turn_unknown"
	m.goalTurnToolCalls = 1
	m.goalTurnSuccesses = 1
	m.turnEvalTotal = 23
	m.state = StateStreaming

	unknown := &agent.UnresolvedExecutionError{
		TurnID: "turn_unknown", ExecutionID: "exec_goal_unknown", ToolName: "write",
		Cause: errors.New("terminal execution receipt was not committed"),
	}
	updated, _ := m.Update(AgentDoneMsg{TurnID: "turn_unknown", Err: unknown})
	m = updated.(*Model)

	snapshot := snapshotUIGoal(t, m.goalRuntime)
	if snapshot.State != goal.StateBlocked || snapshot.Blocker == nil {
		t.Fatalf("unknown outcome did not block goal: %#v", snapshot)
	}
	if snapshot.PendingContinuation != nil || snapshot.Usage.ContinuationTurns != 1 {
		t.Fatalf("unknown outcome lost or refunded its permit: %#v", snapshot)
	}
	if snapshot.LastTurn == nil || !snapshot.LastTurn.OutcomeUnknown || snapshot.LastTurn.Productive {
		t.Fatalf("unknown outcome receipt = %#v", snapshot.LastTurn)
	}
	if snapshot.LastTurn.OutcomeRef != "exec_goal_unknown" || snapshot.Blocker.Reference != "exec_goal_unknown" {
		t.Fatalf("unknown outcome identity = receipt %q blocker %q", snapshot.LastTurn.OutcomeRef, snapshot.Blocker.Reference)
	}
	if m.goalNeedsEvaluation {
		t.Fatal("unknown outcome armed automatic evaluation")
	}
	if got := client.calls.Load(); got != 0 {
		t.Fatalf("unknown-outcome settlement dispatched %d provider calls", got)
	}
}

func TestGoalMismatchedAgentReceiptRecoversPendingPermitAsOutcomeUnknown(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	runtime := newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: "turn_initial", Productive: true, Summary: "verified initial progress",
	})
	if _, err := runtime.BeginContinuation(context.Background(), "turn_expected"); err != nil {
		t.Fatal(err)
	}
	m.goalRuntime = runtime
	m.goalTurnID = "turn_expected"
	m.state = StateStreaming

	updated, _ := m.Update(AgentDoneMsg{TurnID: "turn_unexpected"})
	m = updated.(*Model)

	snapshot := snapshotUIGoal(t, runtime)
	if snapshot.State != goal.StateBlocked || snapshot.Blocker == nil || snapshot.Blocker.Kind != goal.BlockOutcomeUnknown {
		t.Fatalf("mismatched receipt did not block as outcome-unknown: %#v", snapshot)
	}
	if snapshot.PendingContinuation != nil || snapshot.Usage.ContinuationTurns != 1 {
		t.Fatalf("mismatched receipt left or refunded permit: %#v", snapshot)
	}
	if snapshot.LastPendingRecovery == nil || snapshot.LastPendingRecovery.Recovery.Kind != goal.PendingOutcomeUnknown {
		t.Fatalf("mismatched receipt lacked durable pending recovery: %#v", snapshot.LastPendingRecovery)
	}
	if snapshot.LastPendingRecovery.Recovery.TurnID != "turn_expected" || snapshot.LastPendingRecovery.Recovery.OutcomeRef != "turn_expected" {
		t.Fatalf("mismatched receipt recovery identity = %#v", snapshot.LastPendingRecovery.Recovery)
	}
	if m.goalTurnID != "" || m.goalNeedsEvaluation || client.calls.Load() != 0 {
		t.Fatalf("mismatched receipt left work armed: turn=%q evaluate=%v calls=%d", m.goalTurnID, m.goalNeedsEvaluation, client.calls.Load())
	}
}

func TestShowTerminalGoalOmitsLiveWallTime(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	runtime := newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxWallTime: 30 * time.Minute})
	if err := runtime.Drop(context.Background(), "finished elsewhere"); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotUIGoal(t, runtime)
	m.goalRuntime = runtime
	m.now = func() time.Time { return snapshot.CreatedAt.Add(3 * time.Hour) }

	m.showGoal()

	if m.overlay != OverlayGoalInspector || m.goalInspectorState == nil {
		t.Fatal("show goal did not open the inspector")
	}
	inspector := m.goalInspectorState.View()
	if strings.Contains(inspector, "3h/30m") || strings.Contains(inspector, "30m") {
		t.Fatalf("terminal inspector kept a live wall clock: %q", inspector)
	}
}

func TestGoalTurnAdmissionSaveFailurePreventsEveryDispatchKind(t *testing.T) {
	for _, test := range []struct {
		name              string
		manual            bool
		priorTurn         bool
		wantKind          goal.TurnAdmissionKind
		wantContinuations int64
	}{
		{name: "initial", wantKind: goal.AdmissionInitial},
		{name: "manual", manual: true, priorTurn: true, wantKind: goal.AdmissionManual},
		{name: "automatic", priorTurn: true, wantKind: goal.AdmissionAutomatic, wantContinuations: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &goalCountingClient{}
			m := newGoalRuntimeTestModel(t, client)

			store, err := db.OpenPath(filepath.Join(t.TempDir(), "closed-goal.db"))
			if err != nil {
				t.Fatal(err)
			}
			session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
				Title: "closed goal store", Mode: "BUILD", WorkspaceID: m.agent.WorkDir(),
			})
			if err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			runtime := newUIGoalRuntime(t, session.ID, goal.BudgetLimits{MaxContinuationTurns: 3})
			if test.priorTurn {
				recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
					TurnID: "turn_initial", Productive: true, Summary: "verified initial progress",
				})
			}
			m.goalRuntime = runtime
			m.sessionStore = store
			m.sessionID = session.ID
			if err := m.initializeSessionStateRevision(0); err != nil {
				t.Fatal(err)
			}

			if cmd := m.startGoalTurn(nil, test.manual); cmd != nil {
				t.Fatalf("provider command returned after %s admission failed to persist", test.wantKind)
			}

			snapshot := snapshotUIGoal(t, runtime)
			if snapshot.PendingContinuation != nil {
				t.Fatalf("failed admission save left an in-flight turn: %#v", snapshot.PendingContinuation)
			}
			if snapshot.State != goal.StatePaused || snapshot.Usage.ContinuationTurns != test.wantContinuations {
				t.Fatalf("failed %s save state = %s usage=%#v", test.wantKind, snapshot.State, snapshot.Usage)
			}
			if snapshot.LastPendingRecovery == nil || snapshot.LastPendingRecovery.Recovery.Kind != goal.PendingCancelledBeforeDispatch {
				t.Fatalf("failed admission save lacked pre-dispatch recovery evidence: %#v", snapshot.LastPendingRecovery)
			}
			if snapshot.LastPendingRecovery.Permit.Kind != test.wantKind || snapshot.LastPendingRecovery.Recovery.Evidence == "" {
				t.Fatalf("failed admission recovery = %#v", snapshot.LastPendingRecovery)
			}
			if m.state != StateIdle || m.goalTurnID != "" || client.calls.Load() != 0 {
				t.Fatalf("failed %s save entered provider state=%v turn=%q calls=%d", test.wantKind, m.state, m.goalTurnID, client.calls.Load())
			}
		})
	}
}

func TestEveryGoalTurnKindIsDurableBeforeProviderCommandRuns(t *testing.T) {
	for _, test := range []struct {
		name              string
		manual            bool
		priorTurn         bool
		wantKind          goal.TurnAdmissionKind
		wantContinuations int64
	}{
		{name: "initial automatic path", wantKind: goal.AdmissionInitial},
		{name: "initial manual path", manual: true, wantKind: goal.AdmissionInitial},
		{name: "manual", manual: true, priorTurn: true, wantKind: goal.AdmissionManual},
		{name: "automatic", priorTurn: true, wantKind: goal.AdmissionAutomatic, wantContinuations: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &goalCountingClient{}
			m := newGoalRuntimeTestModel(t, client)
			store, err := db.OpenPath(filepath.Join(t.TempDir(), "goal-admission.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if m.cancel != nil {
					m.cancel()
				}
				_ = m.releaseExecutionSessionLease()
				_ = store.Close()
			}()
			workspace, err := canonicalWorkspaceID(m.agent.WorkDir())
			if err != nil {
				t.Fatal(err)
			}
			session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
				Title: "durable goal admission", Mode: "BUILD", WorkspaceID: workspace,
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime := newUIGoalRuntime(t, session.ID, goal.BudgetLimits{MaxContinuationTurns: 3})
			if test.priorTurn {
				recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
					TurnID: "turn_initial", Productive: true, Summary: "verified initial progress",
				})
			}
			m.goalRuntime = runtime
			m.SetSessionStore(store)
			m.sessionID = session.ID
			if err := m.initializeSessionStateRevision(0); err != nil {
				t.Fatal(err)
			}

			cmd := m.startGoalTurn(nil, test.manual)
			if cmd == nil {
				t.Fatalf("durably admitted %s turn returned no provider command: state=%v turn=%q entries=%#v", test.wantKind, m.state, m.goalTurnID, m.entries)
			}
			if m.state != StateWaiting || m.goalTurnID == "" {
				t.Fatalf("admitted %s state=%v turn=%q", test.wantKind, m.state, m.goalTurnID)
			}
			if got := client.calls.Load(); got != 0 {
				t.Fatalf("provider ran before Bubble Tea executed its command: %d calls", got)
			}

			raw, err := store.GetSessionState(context.Background(), session.ID)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := decodeSessionState(raw)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Goal == nil || persisted.Goal.PendingContinuation == nil {
				t.Fatalf("provider command exists without a durable admission: %#v", persisted.Goal)
			}
			pending := persisted.Goal.PendingContinuation
			if pending.TurnID != m.goalTurnID || pending.Kind != test.wantKind {
				t.Fatalf("durable admission = %#v, provider turn = %q", pending, m.goalTurnID)
			}
			if persisted.Goal.Usage.ContinuationTurns != test.wantContinuations {
				t.Fatalf("%s continuation usage = %#v", test.wantKind, persisted.Goal.Usage)
			}
		})
	}
}

func TestGoalManualTurnCannotBypassPendingContinuation(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	runtime := newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: "turn_initial", Productive: true, Summary: "verified initial progress",
	})
	if _, err := runtime.BeginContinuation(context.Background(), "turn_pending"); err != nil {
		t.Fatal(err)
	}
	m.goalRuntime = runtime

	if cmd := m.startGoalTurn(nil, true); cmd != nil {
		t.Fatal("manual path returned a provider command while a continuation was pending")
	}
	snapshot := snapshotUIGoal(t, runtime)
	if snapshot.PendingContinuation == nil || snapshot.PendingContinuation.TurnID != "turn_pending" {
		t.Fatalf("manual admission mutated pending permit: %#v", snapshot.PendingContinuation)
	}
	if m.state != StateIdle || m.goalTurnID != "" || client.calls.Load() != 0 {
		t.Fatalf("manual admission bypassed gate: state=%v turn=%q calls=%d", m.state, m.goalTurnID, client.calls.Load())
	}
}

func TestGoalTurnDeadlineDoesNotRebaseAfterPredispatchDelay(t *testing.T) {
	createdAt := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	snapshot := goal.Snapshot{
		CreatedAt: createdAt,
		Budget:    goal.BudgetLimits{MaxWallTime: 10 * time.Minute},
	}
	limits, err := goalAgentTurnLimits(snapshot, createdAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	wantDeadline := createdAt.Add(10 * time.Minute)
	if !limits.Deadline.Equal(wantDeadline) {
		t.Fatalf("turn deadline = %s, want immutable goal deadline %s", limits.Deadline, wantDeadline)
	}
	if limits.MaxWallTime != 0 {
		t.Fatalf("turn retained a relative wall timeout that can rebase: %s", limits.MaxWallTime)
	}

	// Routing, lease acquisition, and session persistence may finish later. The
	// limit handed to Agent must remain the original whole-goal deadline.
	predispatchFinishedAt := wantDeadline.Add(time.Nanosecond)
	if predispatchFinishedAt.Before(limits.Deadline) {
		t.Fatalf("elapsed pre-dispatch work extended deadline to %s", limits.Deadline)
	}
}

func TestGoalAdvisorOperationsUseWholeGoalDeadline(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*Model) tea.Cmd
		read func(*goalDeadlineAdvisor) <-chan time.Time
	}{
		{
			name: "open",
			run:  func(m *Model) tea.Cmd { return m.beginGoalOpen(false) },
			read: func(a *goalDeadlineAdvisor) <-chan time.Time { return a.openDeadline },
		},
		{
			name: "status",
			run:  func(m *Model) tea.Cmd { return m.beginGoalEvaluation(false) },
			read: func(a *goalDeadlineAdvisor) <-chan time.Time { return a.statusDeadline },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newGoalRuntimeTestModel(t, &goalCountingClient{})
			runtime := newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxWallTime: 10 * time.Second})
			if test.name == "status" {
				if err := runtime.AttachCortex(context.Background(), goal.CortexCorrelation{TaskID: "task_1", Revision: 1, Actor: goalActor}); err != nil {
					t.Fatal(err)
				}
			}
			m.goalRuntime = runtime
			advisor := newGoalDeadlineAdvisor()
			m.goalAdvisor = advisor
			snapshot := snapshotUIGoal(t, runtime)
			m.now = func() time.Time { return snapshot.CreatedAt.Add(time.Second) }

			cmd := test.run(m)
			if cmd == nil {
				t.Fatalf("%s returned no advisor command", test.name)
			}
			messages := commandMessages(cmd)
			if test.name == "open" {
				result := awaitCommandMessage[goalOpenResultMsg](t, messages, time.Second)
				m.finishGoalOperation(result.Token)
			} else {
				result := awaitCommandMessage[goalStatusResultMsg](t, messages, time.Second)
				m.finishGoalOperation(result.Token)
			}
			select {
			case got := <-test.read(advisor):
				want := snapshot.CreatedAt.Add(snapshot.Budget.MaxWallTime)
				if !got.Equal(want) {
					t.Fatalf("%s context deadline = %s, want whole-goal deadline %s", test.name, got, want)
				}
			default:
				t.Fatalf("%s did not receive a deadline-bearing context", test.name)
			}
		})
	}
}

func TestGoalAdvisorOperationUsesAdvisorTimeoutWhenGoalDeadlineIsLater(t *testing.T) {
	now := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	snapshot := goal.Snapshot{
		CreatedAt: now,
		Budget:    goal.BudgetLimits{MaxWallTime: 2 * time.Hour},
	}
	deadline, err := goalAdvisorOperationDeadline(snapshot, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(goalAdvisorTimeout); !deadline.Equal(want) {
		t.Fatalf("advisor deadline = %s, want advisor cap %s", deadline, want)
	}
}

func TestGoalAdvisorOperationsFailClosedAfterWholeGoalDeadline(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*Model) tea.Cmd
	}{
		{name: "open", run: func(m *Model) tea.Cmd { return m.beginGoalOpen(false) }},
		{name: "status", run: func(m *Model) tea.Cmd { return m.beginGoalEvaluation(false) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newGoalRuntimeTestModel(t, &goalCountingClient{})
			runtime := newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxWallTime: 10 * time.Second})
			if test.name == "status" {
				if err := runtime.AttachCortex(context.Background(), goal.CortexCorrelation{TaskID: "task_1", Revision: 1, Actor: goalActor}); err != nil {
					t.Fatal(err)
				}
			}
			m.goalRuntime = runtime
			m.goalAdvisor = newGoalDeadlineAdvisor()
			snapshot := snapshotUIGoal(t, runtime)
			m.now = func() time.Time { return snapshot.CreatedAt.Add(snapshot.Budget.MaxWallTime) }

			if cmd := test.run(m); cmd != nil {
				t.Fatalf("%s returned a command after the whole-goal deadline", test.name)
			}
			if m.goalOperationRunning || m.goalOperation != "" {
				t.Fatalf("%s armed an advisor operation after deadline: %q", test.name, m.goalOperation)
			}
			if state := snapshotUIGoal(t, runtime).State; state == goal.StateActive {
				t.Fatalf("%s left elapsed goal active", test.name)
			}
		})
	}
}

func TestCompleteGoalFromCortexRequiresExactCriterionEvidence(t *testing.T) {
	runtime := newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	if err := runtime.AttachCortex(context.Background(), goal.CortexCorrelation{
		TaskID: "task_1", Revision: 7, Actor: goalActor,
	}); err != nil {
		t.Fatal(err)
	}
	advice := goaladvisor.Advice{
		TaskID: "task_1", Revision: 7, Summary: "verified",
		ProofRevision: goaladvisor.WorkspaceRevision{Commit: "commit_1", DirtyDigest: "sha256:dirty_1"},
	}
	if err := completeGoalFromCortex(runtime, advice); !errors.Is(err, goal.ErrAcceptanceIncomplete) {
		t.Fatalf("completion without criterion evidence error = %v", err)
	}
	if snapshot := snapshotUIGoal(t, runtime); snapshot.State != goal.StateActive || snapshot.Completion != nil {
		t.Fatalf("missing evidence mutated completion: %#v", snapshot)
	}

	advice.CriterionEvidence = map[string]goaladvisor.CriterionProof{
		"criterion_1": {
			Claim: "an unrelated easier claim", Evidence: []string{"case://task_1/verification/vr_claim"},
			Revision: "commit_1", DirtyDigest: "sha256:dirty_1",
		},
	}
	if err := completeGoalFromCortex(runtime, advice); !errors.Is(err, goal.ErrAcceptanceIncomplete) {
		t.Fatalf("completion with mismatched claim statement error = %v", err)
	}
	advice.CriterionEvidence["criterion_1"] = goaladvisor.CriterionProof{
		Claim: "The durable receipt is verified", Evidence: []string{"case://task_1/verification/vr_claim", "ev_1"},
		Revision: "commit_1", DirtyDigest: "sha256:dirty_1",
	}
	if err := completeGoalFromCortex(runtime, advice); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotUIGoal(t, runtime)
	if snapshot.State != goal.StateCompleted || snapshot.Completion == nil || len(snapshot.Completion.Results) != 1 {
		t.Fatalf("criterion-bound completion = %#v", snapshot)
	}
	if evidence := snapshot.Completion.Results[0].Evidence; !strings.Contains(evidence, "vr_claim") || !strings.Contains(evidence, "ev_1") {
		t.Fatalf("completion evidence = %q", evidence)
	}
}

func TestGoalCompletionRejectsWorkspaceMutationAfterCortexAdvice(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	repository := t.TempDir()
	runGoalGitTest(t, repository, "init", "-q")
	runGoalGitTest(t, repository, "config", "user.name", "Goal UI Test")
	runGoalGitTest(t, repository, "config", "user.email", "goal-ui@example.invalid")
	tracked := filepath.Join(repository, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("verified\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGoalGitTest(t, repository, "add", "tracked.txt")
	runGoalGitTest(t, repository, "commit", "-qm", "initial")

	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.agent.SetWorkDir(repository)
	store, sessionID := attachGoalTestSession(t, m)
	defer func() { _ = store.Close() }()
	runtime := newUIGoalRuntime(t, sessionID, goal.BudgetLimits{MaxContinuationTurns: 3})
	if err := runtime.AttachCortex(context.Background(), goal.CortexCorrelation{TaskID: "task_1", Revision: 7, Actor: goalActor}); err != nil {
		t.Fatal(err)
	}
	m.goalRuntime = runtime

	proofRevision, err := goaladvisor.CurrentWorkspaceRevision(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	advice := goaladvisor.Advice{
		OK: true, TaskID: "task_1", Revision: 7, Phase: "complete", Summary: "verified",
		VerificationOutcome: "verified",
		ProofRevision:       proofRevision,
		CriterionEvidence: map[string]goaladvisor.CriterionProof{
			"criterion_1": {
				Claim: "The durable receipt is verified", Evidence: []string{"case://task_1/verification/vr_1"},
				Revision: proofRevision.Commit, DirtyDigest: proofRevision.DirtyDigest,
			},
		},
	}
	token := beginGoalOperationForTest(t, m, "Checking goal")

	// The advice proves the prior bytes. Mutating after Advice but before the UI
	// completion transition must invalidate that proof.
	if err := os.WriteFile(tracked, []byte("mutated after verification\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cmd := m.handleGoalStatusResult(goalStatusResultMsg{Token: token, Advice: advice}); cmd != nil {
		t.Fatal("stale completion proof scheduled more work")
	}
	snapshot := snapshotUIGoal(t, runtime)
	if snapshot.State != goal.StateBlocked || snapshot.Completion != nil {
		t.Fatalf("workspace mutation completed goal: state=%s completion=%#v", snapshot.State, snapshot.Completion)
	}
	if snapshot.Blocker == nil || !strings.Contains(snapshot.Blocker.Reason, "current workspace") {
		t.Fatalf("workspace mutation blocker = %#v", snapshot.Blocker)
	}
	if m.goalPersistenceDirty {
		t.Fatal("workspace mutation blocker was not durably persisted")
	}
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Content, "workspace changed after Cortex verification") {
		t.Fatalf("workspace mutation receipt = %#v", m.entries)
	}
}

func TestGoalCompletionFromCortexPersistsAndRestoresVerifiedGitProof(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	repository := t.TempDir()
	runGoalGitTest(t, repository, "init", "-q")
	runGoalGitTest(t, repository, "config", "user.name", "Goal UI Test")
	runGoalGitTest(t, repository, "config", "user.email", "goal-ui@example.invalid")
	tracked := filepath.Join(repository, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("verified completion\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGoalGitTest(t, repository, "add", "tracked.txt")
	runGoalGitTest(t, repository, "commit", "-qm", "verified completion")

	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.agent.SetWorkDir(repository)
	store, sessionID := attachGoalTestSession(t, m)
	defer func() { _ = store.Close() }()
	runtime := newUIGoalRuntime(t, sessionID, goal.BudgetLimits{MaxContinuationTurns: 3})
	if err := runtime.AttachCortex(context.Background(), goal.CortexCorrelation{TaskID: "task_1", Revision: 6, Actor: goalActor}); err != nil {
		t.Fatal(err)
	}
	recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: "turn_productive", Productive: true, Summary: "write completed with a durable receipt",
	})
	m.goalRuntime = runtime
	if err := m.persistGoalSession(); err != nil {
		t.Fatal(err)
	}

	proofRevision, err := goaladvisor.CurrentWorkspaceRevision(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	advice := goaladvisor.Advice{
		OK: true, TaskID: "task_1", Revision: 7, Phase: "complete", Summary: "verified in Cortex",
		VerificationOutcome: "verified",
		ProofRevision:       proofRevision,
		CriterionEvidence: map[string]goaladvisor.CriterionProof{
			"criterion_1": {
				Claim: "The durable receipt is verified", Evidence: []string{"case://task_1/verification/vr_complete"},
				Revision: proofRevision.Commit, DirtyDigest: proofRevision.DirtyDigest,
			},
		},
	}
	token := beginGoalOperationForTest(t, m, "Checking goal")
	if cmd := m.handleGoalStatusResult(goalStatusResultMsg{Token: token, Advice: advice}); cmd != nil {
		t.Fatal("verified completion unexpectedly scheduled more work")
	}
	completed := snapshotUIGoal(t, runtime)
	if completed.State != goal.StateCompleted || completed.Completion == nil {
		t.Fatalf("verified Cortex advice did not complete goal: %#v", completed)
	}
	if completed.Cortex.Revision != 7 || completed.Completion.ValidatedBy != "cortex:task_1@7" {
		t.Fatalf("completion correlation = %#v", completed)
	}
	if len(completed.Completion.Results) != 1 || !strings.Contains(completed.Completion.Results[0].Evidence, proofRevision.Commit) {
		t.Fatalf("completion omitted Git proof: %#v", completed.Completion.Results)
	}

	workspaceID, err := canonicalWorkspaceID(repository)
	if err != nil {
		t.Fatal(err)
	}
	_, persisted, _, err := loadPersistedSession(context.Background(), store, sessionID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Goal == nil || persisted.Goal.State != goal.StateCompleted || persisted.Goal.Completion == nil {
		t.Fatalf("persisted verified completion = %#v", persisted.Goal)
	}

	restarted := newGoalRuntimeTestModel(t, &goalCountingClient{})
	restarted.agent.SetWorkDir(repository)
	if err := restarted.restoreSessionState(persisted); err != nil {
		t.Fatal(err)
	}
	restored := snapshotUIGoal(t, restarted.goalRuntime)
	if restored.State != goal.StateCompleted || restored.Completion == nil || restored.Completion.ValidatedBy != "cortex:task_1@7" {
		t.Fatalf("restored verified completion = %#v", restored)
	}
}

func TestBoundGoalSummaryPreservesUTF8AtByteLimit(t *testing.T) {
	got := boundGoalSummary(strings.Repeat("界", goal.MaxReasonBytes))
	if len(got) > goal.MaxReasonBytes || !utf8.ValidString(got) {
		t.Fatalf("bounded summary bytes=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

// Without Cortex a goal used to pause after every turn and wait for an
// explicit resume, so the mechanism built for durable work could not take a
// second step unattended. It now defers to the local runtime's own verdict.
//
// StateActive is not an absence of verification: RecordTurn pauses any turn
// whose report is not Productive, and Productive requires a successful tool
// receipt in the durable ledger. Continuing on it is weaker than Cortex
// acceptance but is still evidence, not prose.
func TestGoalLocalOnlyVerifiedProgressContinuesAutomatically(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	store, sessionID := attachGoalTestSession(t, m)
	defer func() { _ = store.Close() }()
	runtime := newUIGoalRuntime(t, sessionID, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: "turn_read", Productive: true, Summary: "one successful read",
	})
	m.goalRuntime = runtime

	if cmd := m.beginGoalEvaluation(false); cmd == nil {
		t.Fatal("verified local progress did not schedule the next goal turn")
	}
	snapshot := snapshotUIGoal(t, runtime)
	if snapshot.State != goal.StateActive {
		t.Fatalf("a productive local-only turn left the goal in %s (%q)", snapshot.State, snapshot.StateReason)
	}
}

// The other half of the contract, and the reason continuing is safe: a turn
// with no successful receipt is not progress, and the runtime pauses it. The
// unlinked path must not turn a stalled goal into an unattended budget burn.
func TestGoalLocalOnlyUnproductiveTurnStillPauses(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	store, sessionID := attachGoalTestSession(t, m)
	defer func() { _ = store.Close() }()
	runtime := newUIGoalRuntime(t, sessionID, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: "turn_stall", Productive: false, Summary: "no concrete progress",
	})
	m.goalRuntime = runtime

	if snapshot := snapshotUIGoal(t, runtime); snapshot.State != goal.StatePaused {
		t.Fatalf("an unproductive turn left the goal in %s", snapshot.State)
	}
	if cmd := m.beginGoalEvaluation(false); cmd != nil {
		t.Fatal("a paused goal scheduled an automatic provider turn")
	}
	if client.calls.Load() != 0 {
		t.Fatalf("a paused goal dispatched %d provider call(s)", client.calls.Load())
	}
}

func TestGoalCortexStatusWithoutRevisionAdvancePauses(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	store, sessionID := attachGoalTestSession(t, m)
	defer func() { _ = store.Close() }()
	runtime := newUIGoalRuntime(t, sessionID, goal.BudgetLimits{MaxContinuationTurns: 3})
	if err := runtime.AttachCortex(context.Background(), goal.CortexCorrelation{TaskID: "task_1", Revision: 4, Actor: goalActor}); err != nil {
		t.Fatal(err)
	}
	recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: "turn_read", Productive: true, Summary: "read succeeded",
	})
	m.goalRuntime = runtime
	token := beginGoalOperationForTest(t, m, "Checking goal")

	cmd := m.handleGoalStatusResult(goalStatusResultMsg{
		Token:  token,
		Advice: goaladvisor.Advice{OK: true, TaskID: "task_1", Revision: 4, Phase: "investigating"},
	})
	if cmd != nil {
		t.Fatal("unchanged Cortex revision scheduled a provider turn")
	}
	snapshot := snapshotUIGoal(t, runtime)
	if snapshot.State != goal.StatePaused || !strings.Contains(snapshot.StateReason, "did not advance") {
		t.Fatalf("no-progress status = state %s reason %q", snapshot.State, snapshot.StateReason)
	}
	if client.calls.Load() != 0 || m.goalPersistenceDirty {
		t.Fatalf("no-progress calls=%d persistenceDirty=%v", client.calls.Load(), m.goalPersistenceDirty)
	}
}

func TestGoalRelinkChecksStatusBeforeManualProviderTurn(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	store, sessionID := attachGoalTestSession(t, m)
	defer func() { _ = store.Close() }()
	m.goalRuntime = newUIGoalRuntime(t, sessionID, goal.BudgetLimits{MaxContinuationTurns: 3})
	m.goalAdvisor = &goalStaticAdvisor{}
	token := beginGoalOperationForTest(t, m, "Linking Cortex")

	cmd := m.handleGoalOpenResult(goalOpenResultMsg{
		Token: token, Manual: true,
		Advice: goaladvisor.Advice{OK: true, TaskID: "task_1", Revision: 6, Phase: "complete"},
	})
	if cmd == nil {
		t.Fatal("successful relink did not schedule a Cortex status check")
		return
	}
	if m.goalOperation != "Checking goal" || !m.goalOperationRunning || m.state != StateIdle || m.goalTurnID != "" {
		t.Fatalf("relink skipped status: operation=%q running=%v state=%v turn=%q", m.goalOperation, m.goalOperationRunning, m.state, m.goalTurnID)
	}
	if client.calls.Load() != 0 {
		t.Fatalf("relink dispatched %d provider calls before status", client.calls.Load())
	}
}

func TestGoalOpenWithoutConfiguredAdvisorStartsOneBoundedLocalTurn(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	store, sessionID := attachGoalTestSession(t, m)
	defer func() { _ = store.Close() }()
	m.goalRuntime = newUIGoalRuntime(t, sessionID, goal.BudgetLimits{MaxContinuationTurns: 3})

	cmd := m.beginGoalOpen(false)
	if cmd == nil {
		t.Fatal("unconfigured Cortex did not schedule the initial local goal turn")
		return
	}
	if m.state != StateWaiting || m.goalTurnID == "" || m.goalOperationRunning {
		t.Fatalf("local goal startup = state %v turn %q operation_running %v", m.state, m.goalTurnID, m.goalOperationRunning)
	}
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	if snapshot.State != goal.StateActive || snapshot.Cortex.TaskID != "" || snapshot.PendingContinuation == nil {
		t.Fatalf("local goal startup snapshot = %#v", snapshot)
	}
	if snapshot.PendingContinuation.Kind != goal.AdmissionInitial {
		t.Fatalf("local goal admission = %q, want %q", snapshot.PendingContinuation.Kind, goal.AdmissionInitial)
	}
	if client.calls.Load() != 0 {
		t.Fatalf("provider ran synchronously during admission: %d calls", client.calls.Load())
	}
}

func TestGoalOpenFailurePausesWithoutProviderDispatch(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	store, sessionID := attachGoalTestSession(t, m)
	defer func() { _ = store.Close() }()
	m.goalRuntime = newUIGoalRuntime(t, sessionID, goal.BudgetLimits{MaxContinuationTurns: 3})
	token := beginGoalOperationForTest(t, m, "Linking Cortex")

	cmd := m.handleGoalOpenResult(goalOpenResultMsg{
		Token: token,
		Err:   errors.New("cortex_open_task: unknown tool"),
	})
	if cmd != nil {
		t.Fatal("failed Cortex link scheduled work")
	}
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	if snapshot.State != goal.StatePaused || !strings.Contains(snapshot.StateReason, "unknown tool") {
		t.Fatalf("failed link state = %s reason %q", snapshot.State, snapshot.StateReason)
	}
	if client.calls.Load() != 0 {
		t.Fatalf("failed link dispatched %d provider calls", client.calls.Load())
	}
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Content, "cortex_open_task") {
		t.Fatalf("missing actionable Cortex receipt: %#v", m.entries)
	}
}

func TestExplicitGoalPromptOpensReviewedInferredDraft(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	if cmd := m.handleCommandAction(command.Result{
		Action: command.ActionOpenGoal,
		Goal:   &command.GoalRequest{Prompt: "Polish the goal workflow"},
	}); cmd != nil {
		t.Fatal("draft review dispatched work")
	}
	if m.goalFormState == nil || !m.goalFormState.draftFromPrompt {
		t.Fatal("explicit /goal prompt did not open an inferred draft review")
	}
	values, err := m.goalFormState.Values()
	if err != nil {
		t.Fatal(err)
	}
	if values.Objective != "Polish the goal workflow" || len(values.CriterionDescriptions()) < 2 {
		t.Fatalf("draft values = %#v", values)
	}
	if m.goalFormState.ActiveField() != GoalFieldTime || !strings.Contains(m.goalFormState.followUpPrompt, "30m") {
		t.Fatalf("missing-time follow-up field=%v prompt=%q", m.goalFormState.ActiveField(), m.goalFormState.followUpPrompt)
	}
}

func TestExplicitBoundedGoalStartsDirectlyWhenPromptIsConcrete(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	store, sessionID := attachGoalTestSession(t, m)
	defer func() { _ = store.Close() }()
	if err := store.SaveSessionState(context.Background(), sessionID, `{"version":2,"goal":null,"execution_cursor":0}`); err != nil {
		t.Fatal(err)
	}
	if err := m.initializeSessionStateRevision(1); err != nil {
		t.Fatal(err)
	}
	m.goalAdvisor = &goalStaticAdvisor{}
	m.mode = ModePlan

	cmd := m.handleCommandAction(command.Result{
		Action: command.ActionOpenGoal,
		Goal: &command.GoalRequest{
			Prompt:     "Make Shift+Tab cycle NORMAL, PLAN, and AUTO without opening a goal form",
			TimeBudget: 45 * time.Minute, TimeExplicit: true,
		},
	})
	if cmd == nil {
		formError := ""
		if m.goalFormState != nil {
			formError = m.goalFormState.Error()
		}
		t.Fatalf("complete bounded goal did not start Cortex linking: runtime=%v form_error=%q entries=%#v", m.goalRuntime != nil, formError, m.entries)
	}
	if m.goalRuntime == nil || m.goalFormState != nil || m.overlay != OverlayNone || m.mode != ModeAuto {
		t.Fatalf("direct goal runtime=%v form=%v overlay=%v mode=%v", m.goalRuntime != nil, m.goalFormState != nil, m.overlay, m.mode)
	}
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	if snapshot.Objective != "Make Shift+Tab cycle NORMAL, PLAN, and AUTO without opening a goal form" {
		t.Fatalf("objective = %q", snapshot.Objective)
	}
	if snapshot.Budget.MaxWallTime != 45*time.Minute || snapshot.Budget.MaxContinuationTurns != 0 || snapshot.Budget.MaxEvalTokens != 0 {
		t.Fatalf("explicit budget = %#v", snapshot.Budget)
	}
	if len(snapshot.AcceptanceCriteria) != 2 {
		t.Fatalf("criteria = %#v", snapshot.AcceptanceCriteria)
	}
	for _, criterion := range snapshot.AcceptanceCriteria {
		if !strings.Contains(criterion.Description, "Shift+Tab") || !strings.Contains(criterion.Description, "goal form") {
			t.Fatalf("criterion is not prompt-specific: %#v", criterion)
		}
	}
	if m.goalOperationCancel != nil {
		m.goalOperationCancel()
	}
}

func TestExplicitBoundedGoalAsksContextualFollowUpWhenPromptIsAmbiguous(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 32})
	m = updated.(*Model)
	cmd := m.handleCommandAction(command.Result{
		Action: command.ActionOpenGoal,
		Goal: &command.GoalRequest{
			Prompt: "fix it", TimeBudget: 20 * time.Minute, TimeExplicit: true,
		},
	})
	if cmd != nil || m.goalRuntime != nil || m.overlay != OverlayGoalForm || m.goalFormState == nil {
		t.Fatalf("ambiguous goal cmd=%v runtime=%v overlay=%v form=%v", cmd != nil, m.goalRuntime != nil, m.overlay, m.goalFormState != nil)
	}
	if m.goalFormState.ActiveField() != GoalFieldObjective {
		t.Fatalf("follow-up field = %v, want objective", m.goalFormState.ActiveField())
	}
	normalized := strings.Join(strings.Fields(ansi.Strip(m.goalFormState.View())), " ")
	for _, want := range []string{"Complete goal details", "concrete behavior or artifact", "observable", "result would prove it"} {
		if !strings.Contains(normalized, want) {
			t.Fatalf("contextual follow-up omitted %q:\n%s", want, ansi.Strip(m.goalFormState.View()))
		}
	}
	assertRenderedLinesFit(t, m.goalFormState.View(), m.width)
}

func TestGoalAdvisorOperationJoinsShutdown(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	token := beginGoalOperationForTest(t, m, "Linking Cortex")
	_ = m.beginShutdown()
	if m.shutdownReady() || !m.goalOperationRunning {
		t.Fatalf("shutdown did not wait for advisor: ready=%v running=%v", m.shutdownReady(), m.goalOperationRunning)
	}
	cmd := m.handleGoalOpenResult(goalOpenResultMsg{Token: token, Err: context.Canceled})
	if cmd == nil || !m.shutdownReady() || m.goalOperationRunning {
		t.Fatalf("advisor receipt did not release shutdown: cmd=%v ready=%v running=%v", cmd != nil, m.shutdownReady(), m.goalOperationRunning)
	}
}

func TestGoalPersistenceFailureLatchesDispatchClosed(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	m.pauseGoal()
	if !m.goalPersistenceDirty {
		t.Fatal("failed goal save did not latch persistence uncertainty")
	}
	if snapshot := snapshotUIGoal(t, m.goalRuntime); snapshot.State != goal.StatePaused {
		t.Fatalf("failed pause persistence left state %s", snapshot.State)
	}
	if cmd := m.resumeGoal(); cmd != nil {
		t.Fatal("persistence recovery failure returned a provider command")
	}
	if snapshot := snapshotUIGoal(t, m.goalRuntime); snapshot.State != goal.StatePaused {
		t.Fatalf("failed persistence retry resumed state %s", snapshot.State)
	}
	if client.calls.Load() != 0 {
		t.Fatalf("persistence uncertainty dispatched %d provider calls", client.calls.Load())
	}
}

func TestGoalCreationPersistenceFailureRestoresModeWithoutReceipt(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "goal-mode-rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "mode rollback", Mode: "ASK", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	m.SetSessionStore(store)
	m.sessionID = session.ID
	m.executionLease = lease
	if err := m.initializeSessionStateRevision(0); err != nil {
		t.Fatal(err)
	}
	// Advance the durable revision behind the model. Recovery preflight remains
	// healthy, then goal persistence fails at its optimistic concurrency guard.
	if err := store.SaveSessionState(context.Background(), session.ID, `{"version":2,"goal":null}`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m.mode = ModeAsk
	m.goalFormState = NewGoalForm(defaultGoalFormValues("Ship safely"), GoalFormOptions{})
	entriesBefore := len(m.entries)

	cmd := m.applyGoalForm(GoalFormEvent{Action: GoalActionSave, Values: GoalFormValues{
		Objective: "Ship safely", AcceptanceCriteria: "Tests pass", TurnBudget: 2,
	}})
	if cmd != nil {
		t.Fatal("failed goal creation returned an async command")
	}
	if m.mode != ModeAsk || m.goalRuntime != nil || len(m.entries) != entriesBefore+1 {
		t.Fatalf("rollback mode=%v runtime=%v entries=%#v", m.mode, m.goalRuntime, m.entries)
	}
	if last := m.entries[len(m.entries)-1]; last.Kind != "error" || strings.Contains(last.Content, "Mode ·") {
		t.Fatalf("rollback receipt = %#v", last)
	}
}

func attachGoalTestSession(t *testing.T, m *Model) (*db.Store, int64) {
	t.Helper()
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "goal-session.db"))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "goal integration", Mode: "BUILD", WorkspaceID: workspace,
	})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	m.SetSessionStore(store)
	m.sessionID = session.ID
	if err := m.initializeSessionStateRevision(0); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, workspace)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	m.executionLease = lease
	t.Cleanup(func() {
		if m.executionLease == lease {
			m.executionLease = nil
		}
		_ = lease.Close()
	})
	return store, session.ID
}

func runGoalGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
