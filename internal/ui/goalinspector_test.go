package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

func goalInspectorFixture(now time.Time) goal.Snapshot {
	return goal.Snapshot{
		Version:   goal.SnapshotVersion,
		ID:        "goal_inspector_test",
		SessionID: 42,
		Objective: "Ship a compact, durable Goal Inspector with honest proof status",
		AcceptanceCriteria: []goal.AcceptanceCriterion{
			{ID: "criterion_1", Description: "The inspector fits a 30 column terminal"},
			{ID: "criterion_2", Description: "Completion evidence remains explicit"},
		},
		State:       goal.StateBlocked,
		StateReason: "external effect needs reconciliation",
		Budget: goal.BudgetLimits{
			MaxContinuationTurns: 8,
			MaxEvalTokens:        12_000,
			MaxWallTime:          30 * time.Minute,
		},
		Usage:  goal.BudgetUsage{ContinuationTurns: 2, EvalTokens: 2_400},
		Cortex: goal.CortexCorrelation{TaskID: "cortex_case_123", Revision: 7, Actor: "sonar"},
		LastTurn: &goal.TurnReceipt{
			TurnReport: goal.TurnReport{TurnID: "turn_2", EvalTokens: 1_200, Productive: true, Summary: "wrote the inspector shell"},
			RecordedAt: now.Add(-3 * time.Minute),
		},
		Blocker: &goal.Blocker{
			Kind: goal.BlockOutcomeUnknown, Reference: "execution_7",
			Reason: "the prior write has no settled receipt", BlockedAt: now.Add(-2 * time.Minute),
		},
		CreatedAt: now.Add(-10 * time.Minute),
		UpdatedAt: now.Add(-2 * time.Minute),
	}
}

func goalInspectorActions(ctx *command.Context) []command.ActionState {
	registry := command.NewRegistry()
	command.RegisterBuiltins(registry)
	all := registry.Actions("goal", ctx)
	actions := make([]command.ActionState, 0, 4)
	for _, action := range all {
		switch action.Spec.ID {
		case command.GoalActionPause, command.GoalActionResume, command.GoalActionBudget, command.GoalActionDrop:
			actions = append(actions, action)
		}
	}
	return actions
}

func TestGoalInspectorHonestDocumentAndResponsiveFrame(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	snapshot := goalInspectorFixture(now)
	actions := goalInspectorActions(&command.Context{
		GoalConfigured: true,
		GoalStatus:     string(snapshot.State),
		GoalBlocker:    string(snapshot.Blocker.Kind),
	})

	for _, size := range []struct {
		name          string
		width, height int
	}{
		{name: "minimum", width: 30, height: 12},
		{name: "normal", width: 100, height: 28},
	} {
		t.Run(size.name, func(t *testing.T) {
			inspector := NewGoalInspector(snapshot, actions, GoalInspectorOptions{
				Width: size.width, Height: size.height, IsDark: true, ReducedMotion: true, Now: now,
			})
			rendered := inspector.View()
			plain := ansi.Strip(rendered)
			requiredText := []string{"Goal", "0/2 verified", "Actions", "esc"}
			if size.name == "normal" {
				requiredText[0] = "Goal inspector"
			}
			for _, required := range requiredText {
				if !strings.Contains(plain, required) {
					t.Fatalf("inspector missing %q:\n%s", required, plain)
				}
			}
			assertRenderedLinesFit(t, rendered, size.width)
			assertRenderedHeightFits(t, rendered, size.height)

			document := ansi.Strip(inspector.buildDocument())
			for _, required := range []string{
				"Objective", "Acceptance criteria", "○ pending", "Productive turns are progress signals, not acceptance proof",
				"Blocker", "outcome_unknown", "Last turn", "productive signal", "Cortex", "revision 7", "Budget",
			} {
				if !strings.Contains(document, required) {
					t.Fatalf("goal document missing %q:\n%s", required, document)
				}
			}
		})
	}
}

func TestGoalInspectorVerifiedCriteriaRequireCompletionEvidence(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	snapshot := goalInspectorFixture(now)
	snapshot.State = goal.StateCompleted
	snapshot.StateReason = "verified"
	snapshot.Blocker = nil
	snapshot.Completion = &goal.CompletionRecord{
		CompletionRequest: goal.CompletionRequest{
			ValidatedBy: "cortex:cortex_case_123@7",
			Summary:     "one criterion verified",
			Results: []goal.AcceptanceResult{
				{CriterionID: "criterion_1", Satisfied: true, Evidence: "verification receipt 1"},
				{CriterionID: "criterion_2", Satisfied: false, Evidence: ""},
			},
		},
		CompletedAt: now,
	}
	inspector := NewGoalInspector(snapshot, nil, GoalInspectorOptions{Width: 80, Height: 24, Now: now})

	verified, total := goalAcceptanceProgress(snapshot)
	if verified != 1 || total != 2 {
		t.Fatalf("acceptance progress = %d/%d, want 1/2", verified, total)
	}
	document := ansi.Strip(inspector.buildDocument())
	if strings.Count(document, "✓ verified") != 1 || strings.Count(document, "○ pending") != 1 {
		t.Fatalf("criterion proof states are not honest:\n%s", document)
	}
}

func TestGoalInspectorActionsExposeReasonsAndConfirmDrop(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	snapshot := goalInspectorFixture(now)
	actions := goalInspectorActions(&command.Context{
		GoalConfigured: true,
		GoalStatus:     string(snapshot.State),
		GoalBlocker:    string(snapshot.Blocker.Kind),
	})
	inspector := NewGoalInspector(snapshot, actions, GoalInspectorOptions{Width: 40, Height: 16, Now: now})

	// The first enabled action is Budget. Moving left selects disabled Resume.
	_, _ = inspector.Update(leftKey())
	if plain := strings.ToLower(ansi.Strip(inspector.View())); !strings.Contains(plain, "unavailable") || !strings.Contains(plain, "reconcile") {
		t.Fatalf("disabled resume reason is not visible:\n%s", plain)
	}
	if event, _ := inspector.Update(enterKey()); event.Action != command.ActionNone {
		t.Fatalf("disabled action emitted %d", event.Action)
	}

	// Move through Budget to Drop. The first Enter arms; the second confirms.
	_, _ = inspector.Update(rightKey())
	_, _ = inspector.Update(rightKey())
	if event, _ := inspector.Update(enterKey()); event.Action != command.ActionNone {
		t.Fatalf("first destructive enter emitted %d", event.Action)
	}
	if !strings.Contains(strings.ToLower(ansi.Strip(inspector.View())), "confirm drop") {
		t.Fatalf("drop confirmation not visible:\n%s", inspector.View())
	}
	if !inspector.CancelConfirmation() {
		t.Fatal("escape did not consume armed drop confirmation")
	}
	_, _ = inspector.Update(enterKey())
	event, _ := inspector.Update(enterKey())
	if event.Action != command.ActionDropGoal {
		t.Fatalf("confirmed action = %d, want drop", event.Action)
	}
}

func TestGoalInspectorEmitsSelectedActionIdentityWithoutRoutingChildren(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	snapshot := goalInspectorFixture(now)
	inspector := NewGoalInspector(snapshot, []command.ActionState{{
		Spec:    command.ActionSpec{ID: goalInspectorRecoveryActionID, Title: "Recovery"},
		Enabled: true,
	}}, GoalInspectorOptions{Width: 80, Height: 24, Now: now})

	event, _ := inspector.Update(enterKey())
	if event.ActionID != goalInspectorRecoveryActionID || event.Action != command.ActionNone {
		t.Fatalf("selected action event = %#v", event)
	}
}

func TestGoalInspectorCachesUntilPresentationChanges(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	snapshot := goalInspectorFixture(now)
	inspector := NewGoalInspector(snapshot, goalInspectorActions(&command.Context{
		GoalConfigured: true, GoalStatus: string(snapshot.State), GoalBlocker: string(snapshot.Blocker.Kind),
	}), GoalInspectorOptions{Width: 80, Height: 24, Now: now})

	first := inspector.View()
	second := inspector.View()
	if first != second || inspector.cache.renders != 1 {
		t.Fatalf("unchanged inspector was not cached: renders=%d", inspector.cache.renders)
	}
	_, _ = inspector.Update(rightKey())
	_ = inspector.View()
	if inspector.cache.renders != 2 {
		t.Fatalf("action navigation did not invalidate cache: renders=%d", inspector.cache.renders)
	}
	inspector.SetTheme(!inspector.isDark, defaultThemeID)
	_ = inspector.View()
	if inspector.cache.renders != 3 {
		t.Fatalf("theme change did not invalidate cache: renders=%d", inspector.cache.renders)
	}
}

func TestGoalInspectorHonorsNoColor(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	snapshot := goalInspectorFixture(now)
	inspector := NewGoalInspector(snapshot, goalInspectorActions(&command.Context{
		GoalConfigured: true, GoalStatus: string(snapshot.State), GoalBlocker: string(snapshot.Blocker.Kind),
	}), GoalInspectorOptions{Width: 80, Height: 24, IsDark: true, Now: now})
	if rendered := inspector.View(); strings.Contains(rendered, "\x1b[38") || strings.Contains(rendered, "\x1b[48") {
		t.Fatalf("NO_COLOR inspector emitted ANSI foreground/background colors: %q", rendered)
	}
}

func TestGoalInspectorIntegratedViewFitsMinimumTerminal(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	m = updated.(*Model)
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 4})
	m.showGoal()

	view := m.View()
	assertRenderedLinesFit(t, view.Content, 30)
	assertRenderedHeightFits(t, view.Content, 12)
	plain := ansi.Strip(view.Content)
	if !strings.Contains(plain, "Goal") || !strings.Contains(plain, "esc") {
		t.Fatalf("minimum integrated inspector lost identity or dismissal:\n%s", plain)
	}
}

func TestShowGoalUsesInspectorAndRestoresStableFooter(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.width, m.height = 100, 28
	m.mode = ModeAuto
	m.model = "qwen3.5:9b"
	m.promptTokens = 4_096
	m.numCtx = 8_192
	m.entries = []ChatEntry{{Kind: "user", Content: "working"}}
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 4})

	// The goal row owns authority and goal progress; identity and the meter
	// belong to the top bar, so the stable-footer contract is checked on the
	// composed frame and each fact must appear exactly once.
	m.recalcViewportHeight()
	m.refreshTranscript()
	frame := ansi.Strip(m.View().Content)
	for _, required := range []string{"AUTO", "qwen3.5:9b", "50%", "active"} {
		if got := strings.Count(frame, required); got != 1 {
			t.Fatalf("frame shows %q %d times, want exactly 1:\n%s", required, got, frame)
		}
	}
	before := ansi.Strip(m.renderStatusLine())
	m.showGoal()
	if m.overlay != OverlayGoalInspector || m.goalInspectorState == nil || m.input.Focused() {
		t.Fatalf("show goal did not transfer focus to inspector: overlay=%d state=%v focused=%v", m.overlay, m.goalInspectorState != nil, m.input.Focused())
	}

	updated, _ := m.Update(escKey())
	m = updated.(*Model)
	if m.overlay != OverlayNone || m.goalInspectorState != nil || !m.input.Focused() {
		t.Fatalf("closing inspector did not restore composer: overlay=%d state=%v focused=%v", m.overlay, m.goalInspectorState != nil, m.input.Focused())
	}
	after := ansi.Strip(m.renderStatusLine())
	if after != before {
		t.Fatalf("inspector changed stable footer:\nbefore %q\nafter  %q", before, after)
	}

	// The inspector is read-only: opening and closing leaves the runtime intact.
	snapshot, err := m.goalRuntime.Snapshot(context.Background())
	if err != nil || snapshot.State != goal.StateActive {
		t.Fatalf("inspector changed runtime: state=%s err=%v", snapshot.State, err)
	}
}

func TestShowGoalCommandRequestsFullRepaint(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 4})

	cmd := m.handleCommandAction(command.Result{Action: command.ActionShowGoal})
	if cmd == nil {
		t.Fatal("show goal command did not request a presentation repaint")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("show goal presentation command returned no message")
	}
	if m.overlay != OverlayGoalInspector || m.goalInspectorState == nil {
		t.Fatalf("show goal command overlay=%d inspector=%v", m.overlay, m.goalInspectorState != nil)
	}
}

func TestGoalInspectorRoutesBudgetIntentThroughSmartParent(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.width, m.height = 80, 24
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 4})
	m.showGoal()

	// Active goals focus Pause first; two right moves select Budget.
	updated, _ := m.Update(rightKey())
	m = updated.(*Model)
	updated, _ = m.Update(rightKey())
	m = updated.(*Model)
	updated, _ = m.Update(enterKey())
	m = updated.(*Model)

	if m.overlay != OverlayGoalForm || m.goalFormState == nil || !m.goalFormState.BudgetOnly() {
		t.Fatalf("budget intent did not route through parent-owned form: overlay=%d form=%v budgetOnly=%v",
			m.overlay, m.goalFormState != nil, m.goalFormState != nil && m.goalFormState.BudgetOnly())
	}
	if m.goalInspectorState != nil {
		t.Fatal("parent retained stale inspector after routing its action")
	}
}
