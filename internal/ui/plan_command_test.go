package ui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/charmbracelet/x/ansi"
)

func TestPlanCommandOpensGuidedFormInPlanMode(t *testing.T) {
	m := newTestModel(t)
	m.mode = ModeAuto
	m.input.SetValue(`/plan "review the auth flow"`)

	if cmd := m.submitInput(); cmd != nil {
		t.Fatal("opening the plan form dispatched work")
	}
	if m.mode != ModePlan || m.overlay != OverlayPlanForm || m.planFormState == nil {
		t.Fatalf("/plan entry = mode %v overlay %v form=%v", m.mode, m.overlay, m.planFormState != nil)
	}
	if got := m.planFormState.Fields[0].Input.Value(); got != "review the auth flow" {
		t.Fatalf("prefilled task = %q", got)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("slash command remained in composer: %q", got)
	}
}

func TestPlanCommandSubmissionUsesStructuredReadOnlyTurn(t *testing.T) {
	client := &modeAuthorityCaptureClient{options: make(chan llm.ChatOptions, 1)}
	m := newGoalRuntimeTestModel(t, client)
	m.mode = ModeAuto
	m.input.SetValue("/plan refactor the router")
	if cmd := m.submitInput(); cmd != nil {
		t.Fatal("opening the plan form dispatched work")
	}

	m.planFormState.Fields[1].OptionIndex = 1
	m.planFormState.Fields[2].Input.SetValue("preserve compatibility")
	wantPrompt := m.planFormState.AssemblePrompt()
	for m.planFormState.ActiveField < len(m.planFormState.Fields)-1 {
		m.advancePlanFormField(1)
	}

	updated, cmd := m.Update(enterKey())
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("submitting the plan form did not dispatch a turn")
		return
	}
	if m.mode != ModePlan || m.agent.AuthorityMode() != agent.AuthorityPlan {
		t.Fatalf("submitted authority = mode %v agent %v", m.mode, m.agent.AuthorityMode())
	}
	if m.overlay != OverlayNone || m.planFormState != nil {
		t.Fatalf("submitted form remained visible: overlay=%v form=%v", m.overlay, m.planFormState != nil)
	}
	if len(m.entries) == 0 || m.entries[len(m.entries)-1].Kind != "user" || m.entries[len(m.entries)-1].Content != wantPrompt {
		t.Fatalf("submitted prompt = %#v, want %q", m.entries, wantPrompt)
	}

	done := awaitCommandMessage[AgentDoneMsg](t, commandMessages(cmd), 2*time.Second)
	if done.Err != nil {
		t.Fatalf("plan provider result = %#v", done)
	}
	options := <-client.options
	if !strings.Contains(options.System, m.modeConfigs[ModePlan].SystemPromptPrefix) ||
		strings.Contains(options.System, m.modeConfigs[ModeAuto].SystemPromptPrefix) {
		t.Fatalf("plan system authority was not preserved:\n%s", options.System)
	}
	toolNames := make(map[string]bool, len(options.Tools))
	for _, tool := range options.Tools {
		toolNames[tool.Name] = true
	}
	if !toolNames["read"] {
		t.Fatalf("PLAN turn omitted read authority: %#v", toolNames)
	}
	for _, forbidden := range []string{"write", "edit", "bash", "mkdir", "remove", "memory_save"} {
		if toolNames[forbidden] {
			t.Fatalf("PLAN turn exposed mutating tool %q: %#v", forbidden, toolNames)
		}
	}
}

func TestPlanCommandCancellationDoesNotDispatch(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	m.input.SetValue("/plan investigate the cache")
	if cmd := m.submitInput(); cmd != nil {
		t.Fatal("opening the plan form dispatched work")
	}

	updated, _ := m.Update(escKey())
	m = updated.(*Model)
	if m.mode != ModePlan || m.overlay != OverlayNone || m.planFormState != nil {
		t.Fatalf("cancelled state = mode %v overlay %v form=%v", m.mode, m.overlay, m.planFormState != nil)
	}
	if m.state != StateIdle || len(m.agent.Messages()) != 0 || client.calls.Load() != 0 {
		t.Fatalf("cancelled plan reached turn state: state=%v calls=%d messages=%#v", m.state, client.calls.Load(), m.agent.Messages())
	}
}

func TestPlanCommandDoesNotDisplaceAttachedGoal(t *testing.T) {
	m := newTestModel(t)
	m.mode = ModeAuto
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	before := snapshotUIGoal(t, m.goalRuntime)
	m.input.SetValue("/plan investigate another task")

	if cmd := m.submitInput(); cmd != nil {
		t.Fatal("rejected /plan scheduled work")
	}
	after := snapshotUIGoal(t, m.goalRuntime)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected /plan mutated the goal:\nbefore=%#v\nafter=%#v", before, after)
	}
	if m.mode != ModeAuto || m.planFormState != nil || m.overlay != OverlayGoalInspector {
		t.Fatalf("attached goal lost ownership: mode=%v form=%v overlay=%v", m.mode, m.planFormState != nil, m.overlay)
	}
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Content, "Goal attached") {
		t.Fatalf("rejected /plan has no explanation: %#v", m.entries)
	}
}

func TestPlanCommandFitsNarrowTerminal(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	m = updated.(*Model)
	m.input.SetValue("/plan map API")
	if cmd := m.submitInput(); cmd != nil {
		t.Fatal("opening the plan form dispatched work")
	}

	view := m.View()
	plain := ansi.Strip(view.Content)
	for _, want := range []string{"Plan · 1/3", "Task", "map API", "esc cancel", "enter next"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("narrow /plan view omitted %q:\n%s", want, plain)
		}
	}
	assertRenderedLinesFit(t, view.Content, 30)
	assertRenderedHeightFits(t, view.Content, 12)
}
