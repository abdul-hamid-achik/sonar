package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

func goalPlanFixture(now time.Time) goal.Snapshot {
	return goal.Snapshot{
		Version:   goal.SnapshotVersion,
		ID:        "goal_plan_test",
		SessionID: 42,
		Objective: "Ship an honest pre-execution checklist",
		AcceptanceCriteria: []goal.AcceptanceCriterion{
			{ID: "criterion_1", Description: "The plan is visible before dispatch"},
			{ID: "criterion_2", Description: "Only durable evidence verifies a criterion"},
		},
		State:     goal.StateActive,
		CreatedAt: now.Add(-time.Minute),
		UpdatedAt: now,
	}
}

func TestGoalPlanDerivesLifecycleOnlyFromDurableSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	base := goalPlanFixture(now)
	tests := []struct {
		name     string
		mutate   func(*goal.Snapshot)
		phase    goalPlanPhase
		progress string
	}{
		{name: "preparing", phase: goalPlanPreparing, progress: "0/2 verified"},
		{name: "coordinating", phase: goalPlanCoordinating, progress: "0/2 verified", mutate: func(snapshot *goal.Snapshot) {
			snapshot.Cortex = goal.CortexCorrelation{TaskID: "task_1", Revision: 1, Actor: "sonar"}
		}},
		{name: "running", phase: goalPlanRunning, progress: "0/2 verified", mutate: func(snapshot *goal.Snapshot) {
			snapshot.PendingContinuation = &goal.ContinuationPermit{TurnID: "turn_1", Kind: goal.AdmissionInitial, GrantedAt: now}
		}},
		{name: "checking", phase: goalPlanChecking, progress: "0/2 verified", mutate: func(snapshot *goal.Snapshot) {
			snapshot.LastTurn = &goal.TurnReceipt{TurnReport: goal.TurnReport{TurnID: "turn_1", Productive: true}, RecordedAt: now}
		}},
		{name: "paused", phase: goalPlanPaused, progress: "0/2 verified", mutate: func(snapshot *goal.Snapshot) {
			snapshot.State = goal.StatePaused
		}},
		{name: "blocked", phase: goalPlanBlocked, progress: "0/2 verified", mutate: func(snapshot *goal.Snapshot) {
			snapshot.State = goal.StateBlocked
			snapshot.Blocker = &goal.Blocker{Kind: goal.BlockDependency, Reference: "dependency_1", Reason: "waiting", BlockedAt: now}
		}},
		{name: "exhausted", phase: goalPlanExhausted, progress: "0/2 verified", mutate: func(snapshot *goal.Snapshot) {
			snapshot.State = goal.StateExhausted
			snapshot.ExhaustedBy = []goal.BudgetDimension{goal.BudgetContinuationTurns}
		}},
		{name: "completed", phase: goalPlanCompleted, progress: "1/2 verified", mutate: func(snapshot *goal.Snapshot) {
			snapshot.State = goal.StateCompleted
			snapshot.Completion = &goal.CompletionRecord{
				CompletionRequest: goal.CompletionRequest{ValidatedBy: "cortex:task_1@2", Results: []goal.AcceptanceResult{
					{CriterionID: "criterion_1", Satisfied: true, Evidence: "verification receipt 1"},
					{CriterionID: "criterion_2", Satisfied: true},
				}},
				CompletedAt: now,
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := cloneGoalPlanSnapshot(base)
			if test.mutate != nil {
				test.mutate(&snapshot)
			}
			if phase := goalPlanPhaseForSnapshot(snapshot); phase != test.phase {
				t.Fatalf("phase = %q, want %q", phase, test.phase)
			}
			card, ok := newGoalPlanCard(snapshot, true, defaultThemeID)
			if !ok {
				t.Fatal("valid durable snapshot was rejected")
			}
			card.SetSize(90, 28)
			plain := ansi.Strip(card.View())
			for _, want := range []string{"plan", string(test.phase), test.progress} {
				if !strings.Contains(plain, want) {
					t.Fatalf("plan missing %q:\n%s", want, plain)
				}
			}
		})
	}
}

func TestGoalPlanRejectsStaleAndEquivocatingSnapshots(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	base := goalPlanFixture(now)
	card, ok := newGoalPlanCard(base, true, defaultThemeID)
	if !ok {
		t.Fatal("fixture rejected")
	}
	card.SetSize(80, 24)

	newer := cloneGoalPlanSnapshot(base)
	newer.UpdatedAt = now.Add(2 * time.Second)
	newer.PendingContinuation = &goal.ContinuationPermit{TurnID: "turn_new", Kind: goal.AdmissionInitial, GrantedAt: now}
	if !card.SetSnapshot(newer) {
		t.Fatal("newer snapshot was rejected")
	}

	stale := cloneGoalPlanSnapshot(base)
	stale.UpdatedAt = now.Add(time.Second)
	stale.State = goal.StatePaused
	if card.SetSnapshot(stale) {
		t.Fatal("stale snapshot was accepted")
	}
	equivocating := cloneGoalPlanSnapshot(newer)
	equivocating.State = goal.StateBlocked
	if card.SetSnapshot(equivocating) {
		t.Fatal("same-revision equivocation was accepted")
	}
	plain := ansi.Strip(card.View())
	if !strings.Contains(plain, "running") || strings.Contains(plain, "paused") || strings.Contains(plain, "blocked") {
		t.Fatalf("stale update changed the card:\n%s", plain)
	}
}

func TestGoalPlanResponsiveCachedAndWidthSafe(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	snapshot := goalPlanFixture(now)
	snapshot.Cortex = goal.CortexCorrelation{TaskID: "task_1", Revision: 1, Actor: "sonar"}
	card, ok := newGoalPlanCard(snapshot, true, defaultThemeID)
	if !ok {
		t.Fatal("fixture rejected")
	}

	card.SetSize(29, 12)
	compact := card.View()
	plain := ansi.Strip(compact)
	if lipgloss.Height(compact) != 1 || lipgloss.Width(compact) > 29 || !strings.Contains(plain, "plan") ||
		!strings.Contains(plain, "coordinating") || !strings.Contains(plain, "0/2") {
		t.Fatalf("compact plan lost identity or overflowed: width=%d height=%d %q", lipgloss.Width(compact), lipgloss.Height(compact), plain)
	}
	_ = card.View()
	if card.renders != 1 {
		t.Fatalf("unchanged compact card rendered %d times", card.renders)
	}

	card.SetSize(90, 28)
	expanded := ansi.Strip(card.View())
	if card.renders != 2 || !strings.Contains(expanded, "□ pending") || !strings.Contains(expanded, "The plan is visible") {
		t.Fatalf("expanded card did not invalidate or show criteria: renders=%d\n%s", card.renders, expanded)
	}
	card.SetTheme(false, defaultThemeID)
	_ = card.View()
	if card.renders != 3 {
		t.Fatalf("theme change did not invalidate cache: renders=%d", card.renders)
	}
}

func TestGoalPlanDoesNotPromoteProseOrRawLookingContent(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	snapshot := goalPlanFixture(now)
	snapshot.Objective = "completed verified succeeded"
	snapshot.AcceptanceCriteria = []goal.AcceptanceCriterion{{
		ID: "criterion_1", Description: "StructuredContent domain=succeeded evidence=verified\x1b]0;spoof\a",
	}}
	snapshot.LastTurn = &goal.TurnReceipt{
		TurnReport: goal.TurnReport{TurnID: "turn_1", Productive: true, Summary: "all criteria verified"},
		RecordedAt: now,
	}
	// Even an impossible injected completion value cannot verify an active
	// snapshot. Valid runtimes can only retain Completion in StateCompleted.
	snapshot.Completion = &goal.CompletionRecord{CompletionRequest: goal.CompletionRequest{Results: []goal.AcceptanceResult{{
		CriterionID: "criterion_1", Satisfied: true, Evidence: "untrusted transcript text",
	}}}}
	card, ok := newGoalPlanCard(snapshot, true, defaultThemeID)
	if !ok {
		t.Fatal("bounded snapshot rejected")
	}
	card.SetSize(100, 28)
	plain := ansi.Strip(card.View())
	if !strings.Contains(plain, "checking") || !strings.Contains(plain, "0/1 verified") || !strings.Contains(plain, "□ pending") {
		t.Fatalf("prose upgraded plan authority:\n%s", plain)
	}
	if strings.Contains(card.View(), "\x1b]0;spoof") || strings.Contains(card.View(), "\a") {
		t.Fatalf("criterion controls reached terminal output: %q", card.View())
	}

	card.SetContinuation(&ContinuationActionPresentation{
		Tool: "cortex_plan", Inputs: []string{"hypotheses"}, BlockedBy: []string{"insufficient_evidence"},
	})
	plain = ansi.Strip(card.View())
	for _, want := range []string{"next: cortex_plan", "needs: hypotheses", "blocked: insufficient_evidence"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("typed continuation omitted %q:\n%s", want, plain)
		}
	}
}

func TestGoalPlanIsVisibleSynchronouslyBeforeFirstGoalCommand(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	store, sessionID := attachGoalTestSession(t, m)
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveSessionState(context.Background(), sessionID, `{"version":2,"goal":null,"execution_cursor":0}`); err != nil {
		t.Fatal(err)
	}
	if err := m.initializeSessionStateRevision(1); err != nil {
		t.Fatal(err)
	}
	m.mode = ModeAsk
	m.goalAdvisor = &goalStaticAdvisor{}
	values := GoalFormValues{
		Objective: "Ship the checklist before dispatch", AcceptanceCriteria: "Checklist is visible\nNo prose grants authority",
		TurnBudget: 3, TokenBudget: 4_000, TimeBudget: 10 * time.Minute,
	}
	m.goalFormState = NewGoalForm(values, GoalFormOptions{Width: m.width, Height: m.height})

	cmd := m.applyGoalForm(GoalFormEvent{Action: GoalActionSave, Values: values})
	if cmd == nil {
		t.Fatalf("goal did not schedule Cortex linking: runtime=%v form=%v form_error=%q operation=%q dirty=%v session=%d entries=%#v",
			m.goalRuntime != nil, m.goalFormState != nil, m.goalFormState.Error(), m.goalOperation, m.goalPersistenceDirty, m.sessionID, m.entries)
	}
	if m.goalPlan == nil || !m.goalOperationRunning {
		t.Fatalf("plan=%v operation_running=%v", m.goalPlan != nil, m.goalOperationRunning)
	}
	plain := ansi.Strip(m.renderGoalPlan())
	if !strings.Contains(plain, "preparing") || !strings.Contains(plain, "0/2 verified") || !strings.Contains(plain, "Checklist is visible") {
		t.Fatalf("plan was not ready before async command execution:\n%s", plain)
	}
	if m.goalOperationCancel != nil {
		m.goalOperationCancel()
	}
}

func TestGoalPlanKeepsBobSeparateAndOwnsValidatedNextAction(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	updated, _ := m.Update(BobWorkspaceContextMsg{
		Generation: 1, Digest: testBobWorkspaceDigest("go-agent-tool", "clean", 0),
	})
	m = updated.(*Model)
	m.beginContinuationTurn("turn-plan-next")
	action := ContinuationActionPresentation{Tool: "bob_playbook"}
	updated, _ = m.Update(ContinuationActionMsg{TurnID: "turn-plan-next", Sequence: 1, Action: &action})
	m = updated.(*Model)

	plan := ansi.Strip(m.renderGoalPlan())
	bob := ansi.Strip(m.renderBobWorkspaceContext())
	if !strings.Contains(plan, "0/1 verified") || !strings.Contains(plan, "next: bob_playbook") || strings.Contains(plan, "bob: ◇") {
		t.Fatalf("plan mixed Bob repository state with completion authority:\n%s", plan)
	}
	if !strings.Contains(bob, "repo clean") || strings.Contains(strings.ToLower(bob), "verified") {
		t.Fatalf("Bob row implied verification: %q", bob)
	}
	view := ansi.Strip(m.View().Content)
	planAt, bobAt := strings.Index(view, "plan ·"), strings.Index(view, "bob: ◇")
	if planAt < 0 || bobAt < 0 || planAt >= bobAt || strings.Count(view, "next: bob_playbook") != 1 {
		t.Fatalf("plan/Bob/next hierarchy is not stable:\n%s", view)
	}

	encoded, err := encodeSessionState(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "bob_playbook") || strings.Contains(encoded, "go-agent-tool") {
		t.Fatalf("ephemeral plan companions entered session state: %s", encoded)
	}
	if snapshot, err := m.goalRuntime.Snapshot(context.Background()); err != nil || snapshot.Completion != nil {
		t.Fatalf("presentation changed durable completion: completion=%#v err=%v", snapshot.Completion, err)
	}
}
