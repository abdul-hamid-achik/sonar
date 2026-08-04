package ui

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/charmbracelet/x/ansi"
)

type modeAuthorityCaptureClient struct {
	options chan llm.ChatOptions
}

type modeAuthorityRouter struct {
	stubRouter
	context    config.ModeContext
	capability config.ModelCapability
}

func (r *modeAuthorityRouter) SetModeContext(mode config.ModeContext) {
	r.context = mode
}

func (r *modeAuthorityRouter) GetModelForCapability(capability config.ModelCapability) string {
	r.capability = capability
	return r.stubRouter.GetModelForCapability(capability)
}

func (c *modeAuthorityCaptureClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	c.options <- options
	return emit(llm.StreamChunk{Text: "done", Done: true, EvalCount: 1, PromptEvalCount: 1})
}

func (*modeAuthorityCaptureClient) Ping() error   { return nil }
func (*modeAuthorityCaptureClient) Model() string { return "mode-authority-test" }
func (*modeAuthorityCaptureClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func TestCycleMode(t *testing.T) {
	t.Run("cycles_normal_to_plan", func(t *testing.T) {
		m := newTestModel(t)
		m.input.SetValue("keep this draft")
		if m.mode != ModeNormal {
			t.Fatalf("expected initial mode ModeNormal, got %d", m.mode)
		}

		updated, _ := m.Update(shiftTabKey())
		m = updated.(*Model)

		if m.mode != ModePlan {
			t.Errorf("expected ModePlan after cycling from NORMAL, got %d", m.mode)
		}
		if got := m.input.Value(); got != "keep this draft" {
			t.Fatalf("mode cycle changed composer draft to %q", got)
		}
		if m.overlay != OverlayNone || m.planFormState != nil || m.goalFormState != nil {
			t.Fatalf("mode cycle opened UI: overlay=%d plan=%v goal=%v", m.overlay, m.planFormState != nil, m.goalFormState != nil)
		}
	})

	t.Run("cycles_normal_to_plan_explicit", func(t *testing.T) {
		m := newTestModel(t)
		m.mode = ModeNormal

		updated, _ := m.Update(shiftTabKey())
		m = updated.(*Model)

		if m.mode != ModePlan {
			t.Errorf("expected ModePlan after cycling from NORMAL, got %d", m.mode)
		}
	})

	t.Run("cycles_plan_to_auto", func(t *testing.T) {
		m := newTestModel(t)
		m.mode = ModePlan

		updated, _ := m.Update(shiftTabKey())
		m = updated.(*Model)

		if m.mode != ModeAuto {
			t.Errorf("expected ModeAuto after cycling from PLAN, got %d", m.mode)
		}
		if m.overlay != OverlayNone || m.goalFormState != nil {
			t.Errorf("AUTO mode switch created goal UI: overlay=%d form=%v", m.overlay, m.goalFormState != nil)
		}
	})

	t.Run("cycles_auto_to_normal", func(t *testing.T) {
		m := newTestModel(t)
		m.mode = ModeAuto

		updated, _ := m.Update(shiftTabKey())
		m = updated.(*Model)

		if m.mode != ModeNormal || m.overlay != OverlayNone {
			t.Fatalf("AUTO cycle = mode %d overlay %d, want NORMAL/chat", m.mode, m.overlay)
		}
	})

	t.Run("cycles_with_attached_goal_without_opening_ui", func(t *testing.T) {
		m := newTestModel(t)
		m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
		before := snapshotUIGoal(t, m.goalRuntime)
		m.input.SetValue("keep attached-goal draft")

		updated, _ := m.Update(shiftTabKey())
		m = updated.(*Model)

		if m.mode != ModePlan || m.overlay != OverlayNone || m.goalInspectorState != nil || m.goalFormState != nil || m.planFormState != nil {
			t.Fatalf("attached-goal Shift+Tab = mode %d overlay %d inspector=%v goal_form=%v plan_form=%v", m.mode, m.overlay, m.goalInspectorState != nil, m.goalFormState != nil, m.planFormState != nil)
		}
		if got := m.input.Value(); got != "keep attached-goal draft" {
			t.Fatalf("attached-goal Shift+Tab changed draft: %q", got)
		}
		after := snapshotUIGoal(t, m.goalRuntime)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("attached-goal Shift+Tab changed goal: before=%#v after=%#v", before, after)
		}
	})

	t.Run("ordinary_mode_switch_stays_quiet", func(t *testing.T) {
		// Mode already lives on the shortcuts row (model · PLAN). A durable
		// "notice · Mode · …" only cluttered scrollback after every Shift+Tab.
		m := newTestModel(t)
		m.entries = append(m.entries, ChatEntry{Kind: "user", Content: "hello"})
		before := len(m.entries)

		updated, _ := m.Update(shiftTabKey())
		m = updated.(*Model)

		if m.mode != ModePlan {
			t.Fatalf("mode = %v, want PLAN", m.mode)
		}
		if len(m.entries) != before {
			t.Fatalf("ordinary mode switch appended transcript noise: %#v", m.entries[before:])
		}
		for _, entry := range m.entries {
			if entry.Kind == "system" && strings.Contains(entry.Content, "Mode ·") {
				t.Fatalf("unexpected Mode · receipt: %#v", entry)
			}
		}
	})

	t.Run("cycles_ambient_mode_while_streaming", func(t *testing.T) {
		m := newTestModel(t)
		m.state = StateStreaming
		before := m.mode

		updated, _ := m.Update(shiftTabKey())
		m = updated.(*Model)

		// Ambient mode must cycle during a live turn so Shift+Tab never feels dead;
		// tool authority for an attached goal remains AUTO until the next send.
		if m.mode == before {
			t.Error("expected ambient mode to cycle while streaming")
		}
	})
}

func TestAttachedGoalPresentsAutoAuthorityWhileAmbientModeCycles(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	m := newTestModel(t)
	router := &modeAuthorityRouter{stubRouter: stubRouter{selected: m.model}}
	m.router = router
	m.modelPinned = true
	m.entries = []ChatEntry{{Kind: "user", Content: "goal work"}}
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
	m.syncComposerAuthority()

	updated, _ := m.Update(shiftTabKey())
	m = updated.(*Model)
	if m.mode != ModePlan {
		t.Fatalf("ambient mode = %v, want PLAN", m.mode)
	}
	if got := m.presentedMode(); got != ModeAuto {
		t.Fatalf("presented goal authority = %v, want AUTO", got)
	}
	if router.context != config.ModeBuildContext {
		t.Fatalf("active goal router authority = %v, want AUTO/build while ambient PLAN", router.context)
	}
	assertSameColor(t, "attached-goal composer rail", m.input.Styles().Focused.Prompt.GetForeground(), newSemanticPalette(m.isDark, m.themeID).Success)

	status := ansi.Strip(m.renderStatusLine())
	if !strings.Contains(status, "AUTO") || strings.Contains(status, "PLAN") {
		t.Fatalf("goal footer did not present its AUTO authority: %q", status)
	}
	last := m.entries[len(m.entries)-1]
	if last.Kind != "system" || !strings.Contains(last.Content, "After goal · PLAN") || !strings.Contains(last.Content, "active goal · AUTO") {
		t.Fatalf("ambient mode receipt did not distinguish future and active authority: %#v", last)
	}

	m.resetConversationSession()
	if got := m.presentedMode(); got != ModePlan {
		t.Fatalf("post-goal presented mode = %v, want saved ambient PLAN", got)
	}
	assertSameColor(t, "post-goal composer rail", m.input.Styles().Focused.Prompt.GetForeground(), newSemanticPalette(m.isDark, m.themeID).Special)
}

func TestAttachedGoalAmbientModeCyclesDuringControlPlaneOperation(t *testing.T) {
	m := newTestModel(t)
	m.goalRuntime = newUIGoalRuntime(t, 43, goal.BudgetLimits{MaxContinuationTurns: 3})
	m.goalOperation = "Checking goal"
	m.goalOperationRunning = true

	updated, _ := m.Update(shiftTabKey())
	m = updated.(*Model)
	if m.mode != ModePlan {
		t.Fatalf("ambient mode = %v, want PLAN", m.mode)
	}
	if m.goalOperation != "Checking goal" || !m.goalOperationRunning {
		t.Fatalf("mode cycle disturbed goal operation: label=%q running=%v", m.goalOperation, m.goalOperationRunning)
	}
	if got := m.presentedMode(); got != ModeAuto {
		t.Fatalf("active goal authority = %v, want AUTO", got)
	}
	last := m.entries[len(m.entries)-1]
	if last.Kind != "system" || !strings.Contains(last.Content, "After goal · PLAN · active goal · AUTO") {
		t.Fatalf("ambient mode receipt = %#v", last)
	}
}

func TestComposerModeRailIsImmediateAdaptiveAndCompact(t *testing.T) {
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })

	for _, isDark := range []bool{false, true} {
		t.Run(map[bool]string{false: "light", true: "dark"}[isDark], func(t *testing.T) {
			m := newTestModel(t)
			m.isDark = isDark
			m.styles = NewStyles(isDark, defaultThemeID)
			configureComposerMode(&m.input, isDark, defaultThemeID, ModeNormal)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
			m = updated.(*Model)
			palette := newSemanticPalette(isDark, defaultThemeID)

			assertSameColor(t, "normal composer rail", m.input.Styles().Focused.Prompt.GetForeground(), palette.Dim)
			normal := m.View()
			if !strings.Contains(ansi.Strip(normal.Content), "▏❯ Ask") {
				t.Fatalf("empty-state NORMAL composer omitted neutral rail:\n%s", ansi.Strip(normal.Content))
			}
			if status := m.renderStatusLine(); status != "" {
				t.Fatalf("test requires empty-state status suppression, got %q", ansi.Strip(status))
			}

			updated, _ = m.Update(shiftTabKey())
			m = updated.(*Model)
			if m.mode != ModePlan || m.overlay != OverlayNone || m.planFormState != nil || m.goalFormState != nil {
				t.Fatalf("Shift+Tab did not directly select PLAN: mode=%v overlay=%v", m.mode, m.overlay)
			}
			assertSameColor(t, "plan composer rail", m.input.Styles().Focused.Prompt.GetForeground(), palette.Special)
			plan := m.View()
			if !strings.Contains(ansi.Strip(plan.Content), "▌❯ Ask") || !strings.Contains(ansi.Strip(plan.Content), "PLAN") {
				t.Fatalf("empty-state PLAN mode is not visible on composer/welcome:\n%s", ansi.Strip(plan.Content))
			}

			updated, _ = m.Update(shiftTabKey())
			m = updated.(*Model)
			if m.mode != ModeAuto || m.overlay != OverlayNone || m.planFormState != nil || m.goalFormState != nil {
				t.Fatalf("Shift+Tab did not directly select AUTO: mode=%v overlay=%v", m.mode, m.overlay)
			}
			assertSameColor(t, "auto composer rail", m.input.Styles().Focused.Prompt.GetForeground(), palette.Success)
			auto := m.View()
			if !strings.Contains(ansi.Strip(auto.Content), "▌❯ Ask") || !strings.Contains(ansi.Strip(auto.Content), "AUTO") {
				t.Fatalf("empty-state AUTO mode is not visible on composer/welcome:\n%s", ansi.Strip(auto.Content))
			}

			updated, _ = m.Update(shiftTabKey())
			m = updated.(*Model)
			if m.mode != ModeNormal || m.overlay != OverlayNone || m.planFormState != nil || m.goalFormState != nil {
				t.Fatalf("Shift+Tab did not wrap directly to NORMAL: mode=%v overlay=%v", m.mode, m.overlay)
			}
			assertSameColor(t, "wrapped normal composer rail", m.input.Styles().Focused.Prompt.GetForeground(), palette.Dim)
			normalAgain := m.View()
			plainNormalAgain := ansi.Strip(normalAgain.Content)
			if !strings.Contains(plainNormalAgain, "▏❯ Ask") || strings.Contains(plainNormalAgain, "PLAN ·") || strings.Contains(plainNormalAgain, "AUTO ·") {
				t.Fatalf("wrapped NORMAL mode is not quiet and visible on the composer:\n%s", plainNormalAgain)
			}

			for name, view := range map[string]tea.View{"normal": normal, "plan": plan, "auto": auto, "normal_again": normalAgain} {
				assertRenderedLinesFit(t, view.Content, minTerminalWidth)
				assertRenderedHeightFits(t, view.Content, minTerminalHeight)
				if view.Cursor == nil || view.Cursor.X != 3 {
					t.Fatalf("%s composer cursor drifted from three-cell rail/prompt: %#v", name, view.Cursor)
				}
			}
		})
	}
}

func TestComposerModeRailRespectsNoColor(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })

	for _, mode := range []Mode{ModeNormal, ModePlan, ModeAuto} {
		m := newTestModel(t)
		m.mode = mode
		m.styles = NewStyles(true, defaultThemeID)
		configureComposerMode(&m.input, true, defaultThemeID, mode)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
		m = updated.(*Model)

		styles := agentTextareaStylesForMode(true, defaultThemeID, mode)
		if rendered := styles.Focused.Prompt.Render("┃"); lipgloss.Width(rendered) != 1 || hasANSIColor(rendered) {
			t.Fatalf("mode %v NO_COLOR rail = %q", mode, rendered)
		}
		view := m.View()
		plain := ansi.Strip(view.Content)
		wantRail := "▌❯ Ask"
		if mode == ModeNormal {
			wantRail = "▏❯ Ask"
		}
		if !strings.Contains(plain, wantRail) || hasANSIColor(view.Content) {
			t.Fatalf("mode %v NO_COLOR composer omitted its glyph fallback or emitted color:\n%s", mode, plain)
		}
		assertRenderedLinesFit(t, view.Content, minTerminalWidth)
		assertRenderedHeightFits(t, view.Content, minTerminalHeight)
	}
}

func TestExplicitGoalDurationOpensReviewWithoutHiddenCaps(t *testing.T) {
	m := newTestModel(t)
	m.handleCommandAction(command.Result{Action: command.ActionOpenGoal, Goal: &command.GoalRequest{
		Prompt: "polish the model picker", TimeBudget: 45 * time.Minute, TimeExplicit: true,
	}})
	if m.overlay != OverlayGoalForm || m.goalFormState == nil {
		t.Fatalf("goal review overlay=%v form=%v", m.overlay, m.goalFormState != nil)
	}
	values, err := m.goalFormState.Values()
	if err != nil {
		t.Fatalf("goal review values: %v", err)
	}
	if values.TimeBudget != 45*time.Minute || values.TurnBudget != 0 || values.TokenBudget != 0 {
		t.Fatalf("goal budgets = %#v", values)
	}
	if m.goalFormState.active != GoalFieldActions {
		t.Fatalf("complete goal request focused field %v, want actions", m.goalFormState.active)
	}
}

func TestPlanModeCannotStartReviewedGoal(t *testing.T) {
	m := newTestModel(t)
	m.mode = ModePlan
	m.overlay = OverlayGoalForm
	m.goalFormState = NewGoalForm(GoalFormValues{
		Objective: "ship safely", AcceptanceCriteria: "tests pass", TimeBudget: time.Minute,
	}, GoalFormOptions{})
	m.goalFormState.SetActiveField(GoalFieldActions)
	entriesBefore := len(m.entries)
	cmd := m.applyGoalForm(GoalFormEvent{Action: GoalActionSave, Values: GoalFormValues{
		Objective: "ship safely", AcceptanceCriteria: "tests pass", TimeBudget: time.Minute,
	}})
	if cmd != nil || m.goalRuntime != nil || m.mode != ModePlan {
		t.Fatalf("plan goal started: cmd=%v runtime=%v mode=%v", cmd != nil, m.goalRuntime != nil, m.mode)
	}
	if m.overlay != OverlayGoalForm || m.goalFormState == nil || m.goalFormState.ActiveField() != GoalFieldActions {
		t.Fatalf("plan rejection dismissed or moved form: overlay=%v form=%v field=%v", m.overlay, m.goalFormState != nil, m.goalFormState.ActiveField())
	}
	if len(m.entries) != entriesBefore {
		t.Fatalf("plan rejection leaked behind inline form: entries=%d, want %d", len(m.entries), entriesBefore)
	}
	for _, want := range []string{"PLAN", "AUTO"} {
		if !strings.Contains(m.goalFormState.Error(), want) {
			t.Fatalf("inline error %q omits %q", m.goalFormState.Error(), want)
		}
		if !strings.Contains(ansi.Strip(m.goalFormState.View()), want) {
			t.Fatalf("rendered form omits %q:\n%s", want, ansi.Strip(m.goalFormState.View()))
		}
	}
	values, err := m.goalFormState.Values()
	if err != nil || values.Objective != "ship safely" || values.AcceptanceCriteria != "tests pass" || values.TimeBudget != time.Minute {
		t.Fatalf("plan rejection changed form values: values=%#v err=%v", values, err)
	}
}

func TestModePickerKeepsAllAuthoritiesActionableAtMinimum(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = minTerminalWidth, minTerminalHeight
	m.openModePicker()
	rendered := m.renderModePicker()
	plain := ansi.Strip(rendered)
	for _, want := range []string{"NORMAL", "PLAN", "AUTO", "esc close", "enter", "↑/↓"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("minimum mode picker omitted %q:\n%s", want, plain)
		}
	}
	assertRenderedLinesFit(t, rendered, minTerminalWidth)
	assertRenderedHeightFits(t, rendered, minTerminalHeight)
}

func TestModeStatusLine(t *testing.T) {
	// Roomy frames: mode lives on the shortcuts row (model · PLAN), not the
	// status strip — otherwise PLAN appears twice.
	m := newTestModel(t)
	m.state = StateIdle
	m.entries = []ChatEntry{{Kind: "user", Content: "conversation started"}}

	t.Run("roomy_chrome_keeps_mode_off_status", func(t *testing.T) {
		if !m.sessionHeaderActive() {
			t.Fatal("expected session chrome on default test size")
		}
		for _, mode := range []Mode{ModeAuto, ModePlan} {
			m.mode = mode
			status := ansi.Strip(m.renderStatusLine())
			label := m.modeConfigs[mode].Label
			if strings.Contains(status, label) {
				t.Errorf("status re-printed %s already on shortcuts: %q", label, status)
			}
		}
		// Shortcuts row still owns identity.
		m.mode = ModePlan
		bar := ansi.Strip(m.renderShortcutsBar(m.chatPaneWidth()))
		if !strings.Contains(bar, "PLAN") {
			t.Fatalf("shortcuts lost PLAN mode: %q", bar)
		}
	})

	t.Run("normal_mode_is_unbadged", func(t *testing.T) {
		m.mode = ModeNormal
		status := m.renderStatusLine()
		if strings.Contains(status, "NORMAL") {
			t.Errorf("normal mode should be visually quiet, got %q", status)
		}
	})

	t.Run("minimum_frame_keeps_mode_badge", func(t *testing.T) {
		// Without session chrome the status strip is the only ambient mode surface.
		updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
		m = updated.(*Model)
		m.state = StateIdle
		m.entries = []ChatEntry{{Kind: "user", Content: "conversation started"}}
		if m.sessionHeaderActive() {
			t.Fatal("minimum terminal should not claim session header")
		}
		m.mode = ModePlan
		status := ansi.Strip(m.renderStatusLine())
		if !strings.Contains(status, "PLAN") {
			t.Errorf("minimum status line should contain PLAN badge, got %q", status)
		}
		m.mode = ModeAuto
		status = ansi.Strip(m.renderStatusLine())
		if !strings.Contains(status, "AUTO") {
			t.Errorf("minimum status line should contain AUTO badge, got %q", status)
		}
	})
}

// Reachability travels with the model label so the pair reads as one fact.
// Which surface carries it depends on the frame, so assert the composed view.
func TestUnavailableOllamaModelIsMarkedOfflineBesideItsName(t *testing.T) {
	m := newTestModel(t)
	m.model = "qwen3.5:2b"
	m.ollamaOffline = true
	m.recalcViewportHeight()
	m.refreshTranscript()

	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "qwen3.5:2b · offline") {
		t.Fatalf("offline marker is not attached to the model name:\n%s", view)
	}
	if got := strings.Count(view, "offline"); got != 1 {
		t.Fatalf("offline appeared %d times, want exactly 1:\n%s", got, view)
	}
}

func TestDefaultModeConfigs(t *testing.T) {
	configs := DefaultModeConfigs()

	if configs[ModeNormal].Label != "NORMAL" {
		t.Errorf("ModeNormal label should be NORMAL, got %q", configs[ModeNormal].Label)
	}
	if !configs[ModeNormal].ToolPolicy.AllowMCP {
		t.Error("ModeNormal should allow approval-gated MCP tools")
	}

	if configs[ModePlan].Label != "PLAN" {
		t.Errorf("ModePlan label should be PLAN, got %q", configs[ModePlan].Label)
	}
	if configs[ModePlan].ToolPolicy.AllowMCP {
		t.Error("ModePlan should not allow MCP tools")
	}

	if configs[ModeAuto].Label != "AUTO" {
		t.Errorf("ModeAuto label should be AUTO, got %q", configs[ModeAuto].Label)
	}
	if !configs[ModeAuto].ToolPolicy.AllowMCP {
		t.Error("ModeAuto should allow tools under the configured permission policy")
	}
	for _, want := range []string{"implemented and verified", "Do not ask for progress confirmation", "routine development commands", "Pause only for Git"} {
		if !strings.Contains(configs[ModeAuto].SystemPromptPrefix, want) {
			t.Errorf("ModeAuto system prompt omitted %q: %q", want, configs[ModeAuto].SystemPromptPrefix)
		}
	}
	for _, stale := range []string{"Pause for shell commands", "same approval policy"} {
		if strings.Contains(configs[ModeAuto].SystemPromptPrefix, stale) {
			t.Errorf("ModeAuto system prompt retained stale authority claim %q", stale)
		}
	}
}

func TestAgentAuthorityModeMapping(t *testing.T) {
	tests := []struct {
		mode Mode
		want agent.AuthorityMode
	}{
		{mode: ModeNormal, want: agent.AuthorityNormal},
		{mode: ModePlan, want: agent.AuthorityPlan},
		{mode: ModeAuto, want: agent.AuthorityAutoScoped},
		{mode: Mode(99), want: agent.AuthorityNormal},
	}
	for _, test := range tests {
		if got := agentAuthorityMode(test.mode); got != test.want {
			t.Fatalf("agent authority for %v = %v, want %v", test.mode, got, test.want)
		}
	}
}

func TestConversationalPresetSubmitDispatchesImmediately(t *testing.T) {
	for _, test := range []struct {
		name string
		mode Mode
	}{
		{name: "normal", mode: ModeNormal},
		{name: "plan", mode: ModePlan},
		{name: "auto", mode: ModeAuto},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &goalCountingClient{}
			m := newGoalRuntimeTestModel(t, client)
			m.mode = test.mode
			m.input.SetValue("ship a verified compact interface")
			cmd := m.submitInput()
			if cmd == nil {
				t.Fatal("ordinary prompt did not dispatch a provider command")
			}
			if m.overlay != OverlayNone || m.planFormState != nil || m.goalFormState != nil {
				t.Fatalf("submit opened UI: overlay=%d plan=%v goal=%v", m.overlay, m.planFormState != nil, m.goalFormState != nil)
			}
			if m.goalRuntime != nil {
				t.Fatal("ordinary prompt implicitly created a durable goal")
			}
			if got := m.input.Value(); got != "" {
				t.Fatalf("submitted composer draft was not cleared: %q", got)
			}
			if m.state != StateWaiting || len(m.entries) == 0 || m.entries[len(m.entries)-1].Kind != "user" {
				t.Fatalf("submit presentation: state=%v entries=%#v", m.state, m.entries)
			}
			done := awaitCommandMessage[AgentDoneMsg](t, commandMessages(cmd), 2*time.Second)
			if done.TurnID == "" {
				t.Fatalf("provider result = %#v", done)
			}
			if got := client.calls.Load(); got != 1 {
				t.Fatalf("provider calls = %d, want 1", got)
			}
		})
	}

	t.Run("attached goal rejects ordinary prompts in every mode", func(t *testing.T) {
		for _, mode := range []Mode{ModeNormal, ModePlan, ModeAuto} {
			t.Run(DefaultModeConfigs()[mode].Label, func(t *testing.T) {
				client := &goalCountingClient{}
				m := newGoalRuntimeTestModel(t, client)
				m.mode = mode
				m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
				before := snapshotUIGoal(t, m.goalRuntime)
				entriesBefore := len(m.entries)
				historyBefore := len(m.promptHistory)
				const draft = "one-off instruction must remain editable"
				m.input.SetValue(draft)

				if cmd := m.submitInput(); cmd != nil {
					t.Fatal("attached-goal prompt returned a side-turn command")
				}
				if got := client.calls.Load(); got != 0 {
					t.Fatalf("provider calls = %d, want 0", got)
				}
				if got := m.input.Value(); got != draft {
					t.Fatalf("attached-goal prompt consumed draft: %q", got)
				}
				if m.overlay != OverlayGoalInspector || m.goalInspectorState == nil || m.goalFormState != nil || m.planFormState != nil {
					t.Fatalf("attached-goal UI: overlay=%d inspector=%v goal_form=%v plan_form=%v", m.overlay, m.goalInspectorState != nil, m.goalFormState != nil, m.planFormState != nil)
				}
				if len(m.promptHistory) != historyBefore {
					t.Fatalf("rejected prompt entered history: %#v", m.promptHistory)
				}
				if len(m.entries) != entriesBefore+1 || m.entries[len(m.entries)-1].Kind != "error" ||
					!strings.Contains(m.entries[len(m.entries)-1].Content, "Goal Inspector") ||
					!strings.Contains(m.entries[len(m.entries)-1].Content, "/new") {
					t.Fatalf("attached-goal status = %#v", m.entries[entriesBefore:])
				}
				after := snapshotUIGoal(t, m.goalRuntime)
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("rejected prompt changed durable goal: before=%#v after=%#v", before, after)
				}
				if m.state != StateIdle || len(m.agent.Messages()) != 0 {
					t.Fatalf("rejected prompt reached turn state: state=%v messages=%#v", m.state, m.agent.Messages())
				}
			})
		}
	})

	t.Run("attached goal rejects and preserves custom prompt", func(t *testing.T) {
		client := &goalCountingClient{}
		m := newGoalRuntimeTestModel(t, client)
		m.mode = ModeAuto
		m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})
		before := snapshotUIGoal(t, m.goalRuntime)
		const generated = "expanded custom instruction"
		cmd := m.handleCommandAction(command.Result{
			Action: command.ActionSendPrompt,
			Data:   generated,
		})
		if cmd != nil {
			t.Fatal("attached-goal custom prompt returned a side-turn command")
		}
		if m.overlay != OverlayGoalInspector || m.goalInspectorState == nil || m.goalFormState != nil || m.planFormState != nil {
			t.Fatalf("attached-goal custom UI: overlay=%d inspector=%v goal_form=%v plan_form=%v", m.overlay, m.goalInspectorState != nil, m.goalFormState != nil, m.planFormState != nil)
		}
		if got := m.input.Value(); got != generated {
			t.Fatalf("generated custom prompt was not preserved: %q", got)
		}
		if got := client.calls.Load(); got != 0 {
			t.Fatalf("provider calls = %d, want 0", got)
		}
		if len(m.entries) != 1 || m.entries[0].Kind != "error" || !strings.Contains(m.entries[0].Content, "remains in the composer") {
			t.Fatalf("custom prompt status = %#v", m.entries)
		}
		after := snapshotUIGoal(t, m.goalRuntime)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("custom prompt changed durable goal: before=%#v after=%#v", before, after)
		}
	})
}

func TestGoalTurnAuthorityRemainsAutoAfterConversationalModeCycle(t *testing.T) {
	client := &modeAuthorityCaptureClient{options: make(chan llm.ChatOptions, 1)}
	m := newGoalRuntimeTestModel(t, client)
	// Reproduce a goal created in AUTO followed by a Shift+Tab cycle to PLAN.
	m.mode = ModePlan
	configureComposerMode(&m.input, m.isDark, m.themeID, m.mode)

	cmd := m.sendGoalToAgentTurn("continue the admitted goal", "turn_goal_authority", agent.TurnLimits{})
	if cmd == nil || m.state != StateWaiting {
		t.Fatalf("goal turn did not reach provider boundary: cmd=%v state=%v", cmd != nil, m.state)
	}
	done := awaitCommandMessage[AgentDoneMsg](t, commandMessages(cmd), 2*time.Second)
	if done.Err != nil {
		t.Fatalf("goal provider result = %#v", done)
	}
	options := <-client.options
	autoPrefix := m.modeConfigs[ModeAuto].SystemPromptPrefix
	planPrefix := m.modeConfigs[ModePlan].SystemPromptPrefix
	if !strings.Contains(options.System, autoPrefix) || strings.Contains(options.System, planPrefix) {
		t.Fatalf("goal authority drifted with UI mode:\n%s", options.System)
	}
	toolNames := make(map[string]bool, len(options.Tools))
	for _, tool := range options.Tools {
		toolNames[tool.Name] = true
	}
	for _, required := range []string{"write", "bash"} {
		if !toolNames[required] {
			t.Fatalf("AUTO goal authority omitted %q after PLAN cycle: %#v", required, toolNames)
		}
	}
	if m.mode != ModePlan {
		t.Fatalf("goal authority silently rewrote conversational mode to %v", m.mode)
	}
}
