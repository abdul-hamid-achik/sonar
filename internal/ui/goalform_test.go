package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestGoalFormInitialValuesAndTypedBudgets(t *testing.T) {
	initial := GoalFormValues{
		Objective:          "ship durable goals",
		AcceptanceCriteria: "resumes after restart\nshows progress",
		TurnBudget:         12,
		TokenBudget:        64_000,
		TimeBudget:         90 * time.Minute,
	}
	form := NewGoalForm(initial, GoalFormOptions{})

	values, err := form.Values()
	if err != nil {
		t.Fatalf("Values() returned error: %v", err)
	}
	if values != initial {
		t.Fatalf("Values() = %#v, want %#v", values, initial)
	}
	criteria := values.CriterionDescriptions()
	if len(criteria) != 2 || criteria[0] != "resumes after restart" || criteria[1] != "shows progress" {
		t.Fatalf("criterion descriptions = %#v", criteria)
	}
	if form.ActiveField() != GoalFieldObjective {
		t.Fatalf("initial field = %d, want objective", form.ActiveField())
	}
	if len(form.choices) != 2 || form.choices[0].Action != GoalActionSave || form.choices[1].Action != GoalActionCancel {
		t.Fatalf("default actions = %#v", form.choices)
	}
}

func TestGoalFormKeyboardNavigationPreservesTextareaEnter(t *testing.T) {
	form := NewGoalForm(GoalFormValues{Objective: "goal"}, GoalFormOptions{})

	event, _ := form.Update(enterKey())
	if event.Action != GoalActionNone || form.ActiveField() != GoalFieldAcceptance {
		t.Fatalf("Enter from objective: event=%#v field=%d", event, form.ActiveField())
	}

	event, _ = form.Update(enterKey())
	if event.Action != GoalActionNone || form.ActiveField() != GoalFieldAcceptance {
		t.Fatalf("Enter in acceptance should insert a newline, event=%#v field=%d", event, form.ActiveField())
	}

	_, _ = form.Update(tabKey())
	if form.ActiveField() != GoalFieldTurns {
		t.Fatalf("Tab from acceptance field = %d, want turns", form.ActiveField())
	}
	_, _ = form.Update(shiftTabKey())
	if form.ActiveField() != GoalFieldAcceptance {
		t.Fatalf("Shift+Tab field = %d, want acceptance", form.ActiveField())
	}

	form.SetActiveField(GoalFieldActions)
	_, _ = form.Update(tabKey())
	if form.ActiveField() != GoalFieldActions {
		t.Fatalf("Tab should clamp at actions, got %d", form.ActiveField())
	}
	form.SetActiveField(GoalFieldObjective)
	_, _ = form.Update(shiftTabKey())
	if form.ActiveField() != GoalFieldObjective {
		t.Fatalf("Shift+Tab should clamp at objective, got %d", form.ActiveField())
	}
}

func TestGoalFormSubmitAndCancelEvents(t *testing.T) {
	form := NewGoalForm(GoalFormValues{
		Objective:          "finish the release",
		AcceptanceCriteria: "CI is green",
		TurnBudget:         8,
		TokenBudget:        24_000,
		TimeBudget:         time.Hour,
	}, GoalFormOptions{})
	form.SetActiveField(GoalFieldActions)

	event, _ := form.Update(enterKey())
	if event.Action != GoalActionSave {
		t.Fatalf("action = %q, want save", event.Action)
	}
	if event.Values.Objective != "finish the release" || event.Values.TokenBudget != 24_000 {
		t.Fatalf("submitted values = %#v", event.Values)
	}

	event, _ = form.Update(escKey())
	if event.Action != GoalActionCancel {
		t.Fatalf("Esc action = %q, want cancel", event.Action)
	}

	_, _ = form.Update(rightKey())
	event, _ = form.Update(enterKey())
	if event.Action != GoalActionCancel {
		t.Fatalf("selected cancel action = %q", event.Action)
	}
}

func TestGoalFormCustomActionsAndVimNavigation(t *testing.T) {
	form := NewGoalForm(GoalFormValues{}, GoalFormOptions{
		Choices: []GoalFormChoice{
			{Action: GoalActionPause, Label: "Pause", Description: "Keep progress"},
			{Action: GoalActionResume, Label: "Resume", Description: "Continue safely"},
			{Action: GoalActionClear, Label: "Clear", Destructive: true},
		},
	})
	form.SetActiveField(GoalFieldActions)

	_, _ = form.Update(charKey('l'))
	_, _ = form.Update(charKey('j'))
	_, _ = form.Update(rightKey()) // clamp at the final action
	if form.choiceIndex != 2 {
		t.Fatalf("choice index = %d, want 2", form.choiceIndex)
	}
	_, _ = form.Update(charKey('h'))
	if form.choiceIndex != 1 {
		t.Fatalf("choice index after h = %d, want 1", form.choiceIndex)
	}

	event, _ := form.Update(enterKey())
	if event.Action != GoalActionResume {
		t.Fatalf("custom action = %q, want resume", event.Action)
	}
}

func TestGoalFormRejectsUnvalidatedCompletionAction(t *testing.T) {
	form := NewGoalForm(GoalFormValues{}, GoalFormOptions{})
	form.SetChoices(
		GoalFormChoice{Action: GoalAction("complete"), Label: "Complete"},
		GoalFormChoice{Action: GoalActionCancel, Label: "Cancel"},
	)

	if len(form.choices) != 1 || form.choices[0].Action != GoalActionCancel {
		t.Fatalf("unsafe completion choice reached UI: %#v", form.choices)
	}
}

func TestGoalFormValidationMovesFocusAndExplainsError(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*GoalForm)
		field     GoalFormField
		message   string
	}{
		{
			name:    "objective",
			field:   GoalFieldObjective,
			message: "objective is required",
		},
		{
			name: "acceptance",
			configure: func(form *GoalForm) {
				form.objective.SetValue("goal")
			},
			field:   GoalFieldAcceptance,
			message: "at least one acceptance criterion",
		},
		{
			name: "turns",
			configure: func(form *GoalForm) {
				form.objective.SetValue("goal")
				form.acceptance.SetValue("done is verifiable")
				form.turns.SetValue("zero")
			},
			field:   GoalFieldTurns,
			message: "turn budget",
		},
		{
			name: "tokens",
			configure: func(form *GoalForm) {
				form.objective.SetValue("goal")
				form.acceptance.SetValue("done is verifiable")
				form.tokens.SetValue("-1")
			},
			field:   GoalFieldTokens,
			message: "token budget",
		},
		{
			name: "time",
			configure: func(form *GoalForm) {
				form.objective.SetValue("goal")
				form.acceptance.SetValue("done is verifiable")
				form.time.SetValue("later")
			},
			field:   GoalFieldTime,
			message: "time budget",
		},
		{
			name: "all budgets blank",
			configure: func(form *GoalForm) {
				form.objective.SetValue("goal")
				form.acceptance.SetValue("done is verifiable")
			},
			field:   GoalFieldTurns,
			message: "at least one",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			form := NewGoalForm(GoalFormValues{}, GoalFormOptions{})
			if test.configure != nil {
				test.configure(form)
			}
			form.SetActiveField(GoalFieldActions)

			event, _ := form.Update(enterKey())
			if event.Action != GoalActionNone {
				t.Fatalf("invalid form emitted action %q", event.Action)
			}
			if form.ActiveField() != test.field {
				t.Fatalf("active field = %d, want %d", form.ActiveField(), test.field)
			}
			if !strings.Contains(form.Error(), test.message) {
				t.Fatalf("error %q missing %q", form.Error(), test.message)
			}
		})
	}
}

func TestGoalFormLimitsAcceptanceCriteriaToRuntimeContract(t *testing.T) {
	criteria := make([]string, goalFormMaximumCriteria+1)
	for index := range criteria {
		criteria[index] = "criterion " + formatGoalBudget(index+1)
	}
	form := NewGoalForm(GoalFormValues{
		Objective:          "bounded goal",
		AcceptanceCriteria: strings.Join(criteria, "\n"),
	}, GoalFormOptions{})
	form.SetActiveField(GoalFieldActions)

	event, _ := form.Update(enterKey())
	if event.Action != GoalActionNone {
		t.Fatalf("too many criteria emitted action %q", event.Action)
	}
	if form.ActiveField() != GoalFieldAcceptance {
		t.Fatalf("active field = %d, want acceptance", form.ActiveField())
	}
	if !strings.Contains(form.Error(), "limited to 64 lines") {
		t.Fatalf("error did not explain criterion limit: %q", form.Error())
	}
}

func TestGoalFormBudgetOnlyLocksDefinitionAndSkipsItsFocus(t *testing.T) {
	initial := GoalFormValues{
		Objective:          "ship durable goals",
		AcceptanceCriteria: "resume after restart\nshow bounded progress",
		TurnBudget:         10,
		TokenBudget:        32_000,
		TimeBudget:         time.Hour,
	}
	form := NewGoalForm(initial, GoalFormOptions{Width: 80, Height: 32, BudgetOnly: true})

	if !form.BudgetOnly() {
		t.Fatal("budget-only option was not applied")
	}
	if form.ActiveField() != GoalFieldTurns {
		t.Fatalf("initial budget-only field = %d, want turns", form.ActiveField())
	}
	form.SetActiveField(GoalFieldObjective)
	if form.ActiveField() != GoalFieldTurns {
		t.Fatalf("locked objective received focus: %d", form.ActiveField())
	}
	_, _ = form.Update(shiftTabKey())
	if form.ActiveField() != GoalFieldTurns {
		t.Fatalf("Shift+Tab entered locked definition: %d", form.ActiveField())
	}

	objectiveBefore := form.objective.Value()
	acceptanceBefore := form.acceptance.Value()
	_, _ = form.Update(charKey('9'))
	if form.objective.Value() != objectiveBefore || form.acceptance.Value() != acceptanceBefore {
		t.Fatal("budget-only input mutated the immutable definition")
	}

	rendered := form.View()
	for _, want := range []string{
		"Goal budgets",
		"Definition locked",
		"Objective · locked",
		"ship durable goals",
		"Acceptance criteria · locked",
		"resume after restart",
		"Save budgets",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("budget-only form missing %q:\n%s", want, rendered)
		}
	}
	assertRenderedLinesFit(t, rendered, 80)
	assertRenderedHeightFits(t, rendered, 32)
}

func TestGoalFormBudgetOnlyCompactKeepsLockedContext(t *testing.T) {
	form := NewGoalForm(GoalFormValues{
		Objective:          "ship durable goals",
		AcceptanceCriteria: "resume\nshow progress",
		TurnBudget:         4,
	}, GoalFormOptions{Width: 30, Height: 12, BudgetOnly: true})

	for field := GoalFieldTurns; field <= GoalFieldActions; field++ {
		form.SetActiveField(field)
		rendered := form.View()
		progress := "Goal budgets · " + string(rune('1'+field-GoalFieldTurns)) + "/4"
		for _, want := range []string{progress, "Locked · ship", "2 acceptance criteria", goalFormFieldLabel(field)} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("compact budget form missing %q:\n%s", want, rendered)
			}
		}
		assertRenderedLinesFit(t, rendered, 30)
		assertRenderedHeightFits(t, rendered, 12)
	}
}

func TestGoalFormBudgetOnlyCompactValidationStillFits(t *testing.T) {
	form := NewGoalForm(GoalFormValues{
		Objective:          "ship durable goals",
		AcceptanceCriteria: "done",
		TurnBudget:         4,
	}, GoalFormOptions{Width: 30, Height: 12, BudgetOnly: true})
	form.tokens.SetValue("invalid")
	form.SetActiveField(GoalFieldActions)
	_, _ = form.Update(enterKey())

	rendered := form.View()
	if form.ActiveField() != GoalFieldTokens || !strings.Contains(rendered, "token budget must") {
		t.Fatalf("compact validation did not focus/explain token budget:\n%s", rendered)
	}
	assertRenderedLinesFit(t, rendered, 30)
	assertRenderedHeightFits(t, rendered, 12)
}

func TestGoalFormSetBudgetOnlyUpdatesTruthfulDefaults(t *testing.T) {
	form := NewGoalForm(GoalFormValues{
		Objective:          "goal",
		AcceptanceCriteria: "done",
	}, GoalFormOptions{})
	form.SetActiveField(GoalFieldAcceptance)
	form.SetBudgetOnly(true)

	if form.ActiveField() != GoalFieldTurns {
		t.Fatalf("enabling budget-only left focus on %d", form.ActiveField())
	}
	if form.choices[0].Label != "Save budgets" {
		t.Fatalf("primary action = %q, want Save budgets", form.choices[0].Label)
	}
}

func TestGoalFormFitsMinimumAndNormalCanvases(t *testing.T) {
	sizes := []struct {
		name    string
		width   int
		height  int
		compact bool
	}{
		{name: "minimum", width: 30, height: 12, compact: true},
		{name: "narrow", width: 40, height: 20, compact: true},
		{name: "normal", width: 80, height: 32, compact: false},
	}

	for _, size := range sizes {
		for field := GoalFieldObjective; field < goalFormFieldCount; field++ {
			t.Run(size.name+"_"+goalFormFieldLabel(field), func(t *testing.T) {
				form := NewGoalForm(GoalFormValues{
					Objective:          "Polish Unicode 模型 goals without losing session context",
					AcceptanceCriteria: "Resume safely\nShow useful progress",
					TurnBudget:         12,
					TokenBudget:        64_000,
					TimeBudget:         90 * time.Minute,
				}, GoalFormOptions{Width: size.width, Height: size.height})
				form.SetActiveField(field)

				rendered := form.View()
				assertRenderedLinesFit(t, rendered, size.width)
				assertRenderedHeightFits(t, rendered, size.height)
				if !strings.Contains(rendered, "╰") {
					t.Fatalf("goal form lost its closing border:\n%s", rendered)
				}
				if !strings.Contains(rendered, "esc") || !strings.Contains(rendered, "cancel") {
					t.Fatalf("goal form lost cancellation affordance:\n%s", rendered)
				}

				if size.compact {
					progress := "Goal · " + string(rune('1'+field)) + "/6"
					if !strings.Contains(rendered, progress) {
						t.Fatalf("compact goal form missing %q:\n%s", progress, rendered)
					}
					if !strings.Contains(rendered, goalFormFieldLabel(field)) {
						t.Fatalf("compact goal form missing active label:\n%s", rendered)
					}
					return
				}

				for _, label := range []string{"Goal", "Objective", "Acceptance criteria", "Limits", "Auto turns", "Tokens", "Time", "Actions"} {
					if !strings.Contains(rendered, label) {
						t.Fatalf("normal goal form missing %q:\n%s", label, rendered)
					}
				}
			})
		}
	}
}

func TestGoalFormFooterMatchesEnterBehavior(t *testing.T) {
	form := NewGoalForm(GoalFormValues{Objective: "goal"}, GoalFormOptions{Width: 30, Height: 12})

	form.SetActiveField(GoalFieldObjective)
	if view := form.View(); !strings.Contains(view, "enter/tab") || !strings.Contains(view, "next") {
		t.Fatalf("objective footer does not advertise Enter behavior:\n%s", view)
	}
	form.SetActiveField(GoalFieldAcceptance)
	if view := form.View(); !strings.Contains(view, "enter") || !strings.Contains(view, "newline") {
		t.Fatalf("acceptance footer does not advertise newline behavior:\n%s", view)
	}
	form.SetActiveField(GoalFieldActions)
	if view := form.View(); !strings.Contains(view, "enter") || !strings.Contains(view, "select") || !strings.Contains(view, "choose") {
		t.Fatalf("actions footer does not advertise selection behavior:\n%s", view)
	}
}

func TestGoalFormActionsSupportSpaceAndTruthfulBoundaryArrows(t *testing.T) {
	form := NewGoalForm(GoalFormValues{
		Objective:          "goal",
		AcceptanceCriteria: "done",
		TurnBudget:         4,
	}, GoalFormOptions{Width: 30, Height: 12})
	form.SetActiveField(GoalFieldActions)

	first := form.View()
	if !strings.Contains(first, "▸ Save goal →") || strings.Contains(first, "← ▸ Save goal") {
		t.Fatalf("first action advertised an unavailable direction:\n%s", first)
	}
	_, _ = form.Update(rightKey())
	last := form.View()
	if !strings.Contains(last, "← ▸ Cancel") || strings.Contains(last, "Cancel →") {
		t.Fatalf("last action advertised an unavailable direction:\n%s", last)
	}
	if !strings.Contains(last, "enter/space") || !strings.Contains(last, "arrows/hjkl") {
		t.Fatalf("action footer omitted supported keys:\n%s", last)
	}

	event, _ := form.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	if event.Action != GoalActionCancel {
		t.Fatalf("Space action = %q, want cancel", event.Action)
	}
}

func TestGoalFormFormatsTimeBudgetWithoutZeroNoise(t *testing.T) {
	tests := map[time.Duration]string{
		time.Hour:                    "1h",
		90 * time.Minute:             "1h30m",
		time.Minute + 30*time.Second: "1m30s",
	}
	for duration, want := range tests {
		form := NewGoalForm(GoalFormValues{
			Objective:          "goal",
			AcceptanceCriteria: "done",
			TurnBudget:         1,
			TimeBudget:         duration,
		}, GoalFormOptions{})
		if got := form.time.Value(); got != want {
			t.Errorf("time input for %s = %q, want %q", duration, got, want)
		}
	}
}

func TestGoalFormValidationIsInlineAndClearsWhenFocusMoves(t *testing.T) {
	form := NewGoalForm(GoalFormValues{
		Objective:          "bounded work",
		AcceptanceCriteria: "done is verifiable",
	}, GoalFormOptions{Width: 80, Height: 32})
	form.SetActiveField(GoalFieldActions)
	_, _ = form.Update(enterKey())

	if form.ActiveField() != GoalFieldTurns {
		t.Fatalf("validation focus = %d, want auto turns", form.ActiveField())
	}
	rendered := form.View()
	errorAt := strings.Index(rendered, "! set at least one auto-turn, token, or time budget")
	tokensAt := strings.Index(rendered, "Tokens")
	if errorAt < 0 || tokensAt < 0 || errorAt > tokensAt {
		t.Fatalf("validation error was not rendered beside Auto turns:\n%s", rendered)
	}
	assertRenderedHeightFits(t, rendered, 32)

	_, _ = form.Update(tabKey())
	if form.ActiveField() != GoalFieldTokens || form.Error() != "" {
		t.Fatalf("focus move retained stale validation: field=%d error=%q", form.ActiveField(), form.Error())
	}
}

func TestGoalFormBudgetCopyExplainsAutomaticContinuationLimit(t *testing.T) {
	compact := NewGoalForm(GoalFormValues{
		Objective:          "goal",
		AcceptanceCriteria: "done",
		TokenBudget:        1000,
	}, GoalFormOptions{Width: 30, Height: 12})
	compact.SetActiveField(GoalFieldTurns)
	compactView := compact.View()
	for _, want := range []string{"Auto-turn budget", "Blank skips", "set one"} {
		if !strings.Contains(compactView, want) {
			t.Fatalf("compact budget step missing %q:\n%s", want, compactView)
		}
	}
	assertRenderedHeightFits(t, compactView, 12)

	wide := NewGoalForm(GoalFormValues{
		Objective:          "goal",
		AcceptanceCriteria: "done",
		TurnBudget:         8,
	}, GoalFormOptions{Width: 80, Height: 32})
	wideView := wide.View()
	if !strings.Contains(wideView, "Limits") || !strings.Contains(wideView, "set at least one") || !strings.Contains(wideView, "Auto turns") {
		t.Fatalf("wide budget grammar is not truthful:\n%s", wideView)
	}
}

func TestGoalFormUsesCompactLayoutBelowFullHeight(t *testing.T) {
	form := NewGoalForm(GoalFormValues{
		Objective:          "goal",
		AcceptanceCriteria: "done",
		TurnBudget:         4,
	}, GoalFormOptions{Width: 80, Height: 23})
	view := form.View()
	if !strings.Contains(view, "Goal · 1/6") || strings.Contains(view, "Define done") {
		t.Fatalf("23-row terminal did not use the compact form:\n%s", view)
	}
	assertRenderedHeightFits(t, view, 23)
}

func TestGoalFormCompactActionsSummarizeInferredGoal(t *testing.T) {
	form := NewGoalForm(GoalFormValues{
		Objective: "ship a bounded migration", AcceptanceCriteria: "tests pass\nrollback works", TimeBudget: 30 * time.Minute,
	}, GoalFormOptions{Width: 60, Height: 20, DraftFromPrompt: true})
	form.SetActiveField(GoalFieldActions)
	view := form.View()
	for _, want := range []string{"ship a bounded migration", "2 criteria", "30m", "turns/tokens unlimited", "Proof · tests pass", "+1 more", "Save goal"} {
		if !strings.Contains(view, want) {
			t.Fatalf("compact review missing %q:\n%s", want, view)
		}
	}
	assertRenderedHeightFits(t, view, 20)
}

func TestGoalFormIntegratedInlineFitsAndOwnsCursor(t *testing.T) {
	for _, size := range []struct {
		name   string
		width  int
		height int
	}{
		{name: "minimum", width: 30, height: 12},
		{name: "wide", width: 120, height: 36},
	} {
		t.Run(size.name, func(t *testing.T) {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			m = updated.(*Model)
			m.setTestTranscriptContent("TRANSCRIPT SENTINEL\ntranscript tail")
			m.input.SetValue("saved composer draft")
			if err := m.openGoalForm("integrated goal", false); err != nil {
				t.Fatalf("open goal form: %v", err)
			}

			view := m.View()
			assertRenderedLinesFit(t, view.Content, size.width)
			assertRenderedHeightFits(t, view.Content, size.height)
			assertViewCursorAfter(t, view, "> integrated goal")
			if !m.goalFormState.objective.VirtualCursor() {
				t.Fatal("integrated View mutated the live goal input cursor mode")
			}
			if m.input.Focused() {
				t.Fatal("composer retained focus under the inline goal form")
			}
			plain := ansi.Strip(view.Content)
			if transcriptAt, formAt := strings.Index(plain, "TRANSCRIPT SENTINEL"), strings.Index(plain, "Goal"); transcriptAt < 0 || formAt <= transcriptAt {
				t.Fatalf("transcript/form order is wrong:\n%s", plain)
			}
			if strings.Contains(plain, "saved composer draft") || m.input.Value() != "saved composer draft" {
				t.Fatalf("form did not replace composer without mutating its draft: value=%q\n%s", m.input.Value(), plain)
			}
			for _, line := range strings.Split(view.Content, "\n") {
				if strings.Contains(line, "╭") {
					if !strings.HasPrefix(ansi.Strip(line), "╭") || lipgloss.Width(line) != m.chatPaneWidth() {
						t.Fatalf("inline form frame is centered or not pane-wide: width=%d want=%d line=%q", lipgloss.Width(line), m.chatPaneWidth(), ansi.Strip(line))
					}
					break
				}
			}
		})
	}
}

func TestGoalFormAcceptanceWrapRetainsPromptGutterAtMinimum(t *testing.T) {
	form := NewGoalForm(GoalFormValues{
		Objective:          "Ship a polished goal UI",
		AcceptanceCriteria: "The durable goal status is visible and accurate",
		TurnBudget:         4,
	}, GoalFormOptions{Width: 30, Height: 12})
	form.SetActiveField(GoalFieldAcceptance)

	rendered := form.View()
	plain := ansi.Strip(rendered)
	lines := strings.Split(plain, "\n")
	labelAt := -1
	footerAt := -1
	for index, line := range lines {
		if strings.Contains(line, "Acceptance criteria") {
			labelAt = index
		}
		if footerAt < 0 && strings.Contains(line, "esc cancel") {
			footerAt = index
		}
	}
	if labelAt < 0 || footerAt < 0 {
		t.Fatalf("acceptance control or footer missing:\n%s", plain)
	}
	wantHeight := form.acceptanceHeight()
	if got := footerAt - labelAt - 1; got != wantHeight {
		t.Fatalf("acceptance gained a frame-wrapped row: got %d control rows, want %d\n%s", got, wantHeight, plain)
	}
	for row := 0; row < wantHeight; row++ {
		line := lines[labelAt+1+row]
		prefix := "│   "
		if row == 0 {
			prefix = "│ > "
		}
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("acceptance row %d lost its prompt gutter: %q\n%s", row+1, line, plain)
		}
	}
	assertRenderedLinesFit(t, rendered, 30)
	assertRenderedHeightFits(t, rendered, 12)
}

func TestGoalFormViewCachesUntilStateChanges(t *testing.T) {
	form := NewGoalForm(GoalFormValues{Objective: "ship"}, GoalFormOptions{})
	first, firstCursor := form.ViewWithCursor()
	second, secondCursor := form.ViewWithCursor()
	if first != second {
		t.Fatal("cached view changed without state change")
	}
	if form.cache.renders != 1 {
		t.Fatalf("render count = %d, want 1", form.cache.renders)
	}
	if firstCursor == nil || secondCursor == nil {
		t.Fatal("focused input should expose a cursor")
	}
	firstCursor.X = 999
	_, cursor := form.ViewWithCursor()
	if cursor.X == 999 {
		t.Fatal("caller mutated cached cursor")
	}

	_, _ = form.Update(charKey('!'))
	third := form.View()
	if third == first {
		t.Fatal("view did not change after input")
	}
	if form.cache.renders != 2 {
		t.Fatalf("render count after update = %d, want 2", form.cache.renders)
	}
}

func TestGoalFormReducedMotionUsesStaticBubblesCursors(t *testing.T) {
	form := NewGoalForm(GoalFormValues{}, GoalFormOptions{ReducedMotion: true})
	if form.objective.Styles().Cursor.Blink || form.acceptance.Styles().Cursor.Blink {
		t.Fatal("reduced motion left a cursor blinking")
	}

	form.SetReducedMotion(false)
	if !form.objective.Styles().Cursor.Blink || !form.acceptance.Styles().Cursor.Blink {
		t.Fatal("normal motion did not restore Bubbles cursor blinking")
	}
}

func TestGoalFormPromptDraftMakesReviewBoundaryExplicit(t *testing.T) {
	initial := GoalFormValues{
		Objective:          "Polish the Cortex goal experience",
		AcceptanceCriteria: "Cortex status is compact\nTool failures remain readable",
		TurnBudget:         8,
	}

	wide := NewGoalForm(initial, GoalFormOptions{
		Width: 80, Height: 32, DraftFromPrompt: true,
	})
	wideView := wide.View()
	wideCopy := strings.Join(strings.Fields(ansi.Strip(wideView)), " ")
	for _, want := range []string{"Review goal draft", "Inferred from your prompt", "before AUTO", "starts."} {
		if !strings.Contains(wideCopy, want) {
			t.Fatalf("prompt review form missing %q:\n%s", want, wideView)
		}
	}
	values, err := wide.Values()
	if err != nil || values != initial {
		t.Fatalf("review presentation changed inferred values: values=%#v err=%v", values, err)
	}
	assertRenderedLinesFit(t, wideView, 80)
	assertRenderedHeightFits(t, wideView, 32)

	compact := NewGoalForm(initial, GoalFormOptions{
		Width: 30, Height: 12, DraftFromPrompt: true, ReducedMotion: true,
	})
	compactView := compact.View()
	if !strings.Contains(compactView, "Prompt draft") || !strings.Contains(compactView, "review") {
		t.Fatalf("compact prompt review boundary missing:\n%s", compactView)
	}
	if compact.objective.Styles().Cursor.Blink || compact.acceptance.Styles().Cursor.Blink {
		t.Fatal("prompt draft ignored reduced-motion cursor policy")
	}
	assertRenderedLinesFit(t, compactView, 30)
	assertRenderedHeightFits(t, compactView, 12)
}

func TestGoalFormContextualFollowUpIsGuidanceNotAnError(t *testing.T) {
	const followUp = "What concrete behavior should change, and what result would prove it?"
	for _, size := range []struct {
		width  int
		height int
	}{
		{width: 80, height: 32},
		{width: 30, height: 12},
	} {
		form := NewGoalForm(GoalFormValues{
			Objective: "fix it", AcceptanceCriteria: "Demonstrate the requested outcome: fix it", TimeBudget: time.Minute,
		}, GoalFormOptions{
			Width: size.width, Height: size.height, DraftFromPrompt: true, FollowUpPrompt: followUp,
		})
		rendered := ansi.Strip(form.View())
		if form.Error() != "" || strings.Contains(rendered, "! "+followUp) {
			t.Fatalf("follow-up rendered as an error: error=%q\n%s", form.Error(), rendered)
		}
		if size.width >= 80 {
			for _, want := range []string{"Complete goal details", "What concrete behavior should change"} {
				if !strings.Contains(rendered, want) {
					t.Fatalf("full follow-up omitted %q:\n%s", want, rendered)
				}
			}
		} else if !strings.Contains(rendered, "Needs detail") {
			t.Fatalf("compact follow-up omitted guidance:\n%s", rendered)
		}
		assertRenderedLinesFit(t, form.View(), size.width)
	}
}

func TestGoalFormCursorStaysInsideFrame(t *testing.T) {
	form := NewGoalForm(GoalFormValues{Objective: "模型 goal"}, GoalFormOptions{Width: 30, Height: 12})
	view, cursor := form.ViewWithCursor()
	if cursor == nil {
		t.Fatal("focused field returned no cursor")
	}
	lines := strings.Split(view, "\n")
	if cursor.Y < 0 || cursor.Y >= len(lines) {
		t.Fatalf("cursor Y=%d outside %d rows", cursor.Y, len(lines))
	}
	if cursor.X < 0 || cursor.X > lipgloss.Width(lines[cursor.Y]) {
		t.Fatalf("cursor X=%d outside row width %d", cursor.X, lipgloss.Width(lines[cursor.Y]))
	}
}
