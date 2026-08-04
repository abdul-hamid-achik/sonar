package ui

import (
	"strings"
	"testing"
)

func TestPlanForm_NewPrefilled(t *testing.T) {
	pf := NewPlanFormState("refactor auth module", defaultThemeID)

	if len(pf.Fields) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(pf.Fields))
	}

	// Task field should be pre-filled.
	if pf.Fields[0].Input.Value() != "refactor auth module" {
		t.Errorf("task field should be pre-filled, got %q", pf.Fields[0].Input.Value())
	}

	// Scope field should be select with 3 options.
	if pf.Fields[1].Kind != "select" {
		t.Errorf("scope field should be select, got %q", pf.Fields[1].Kind)
	}
	if len(pf.Fields[1].Options) != 3 {
		t.Errorf("scope should have 3 options, got %d", len(pf.Fields[1].Options))
	}

	// Focus field should be text.
	if pf.Fields[2].Kind != "text" {
		t.Errorf("focus field should be text, got %q", pf.Fields[2].Kind)
	}
}

func TestPlanForm_PrefillPreservesComposerSizedTask(t *testing.T) {
	task := strings.Repeat("界", 2048)
	pf := NewPlanFormState(task, defaultThemeID)

	if got := pf.Fields[0].Input.Value(); got != task {
		t.Fatalf("prefilled task was truncated: got %d runes, want %d", len([]rune(got)), len([]rune(task)))
	}
}

func TestPlanForm_AssemblePrompt(t *testing.T) {
	pf := NewPlanFormState("build a REST API", defaultThemeID)
	pf.Fields[1].OptionIndex = 1 // "module"
	pf.Fields[2].Input.SetValue("keep backward compat")

	prompt := pf.AssemblePrompt()

	if !strings.Contains(prompt, "build a REST API") {
		t.Error("prompt should contain task")
	}
	if !strings.Contains(prompt, "module") {
		t.Error("prompt should contain scope")
	}
	if !strings.Contains(prompt, "keep backward compat") {
		t.Error("prompt should contain focus")
	}
	for _, section := range []string{"Assumptions and open questions", "Ordered steps and dependencies", "Acceptance criteria", "Verification commands or checks"} {
		if !strings.Contains(prompt, section) {
			t.Errorf("prompt should require %q", section)
		}
	}
}

func TestPlanForm_RequiresTaskBeforeAdvancingOrSubmitting(t *testing.T) {
	m := newTestModel(t)
	m.openPlanForm("")

	updated, cmd := m.Update(enterKey())
	m = updated.(*Model)
	if m.planFormState == nil {
		t.Fatal("empty task closed the plan form")
	}
	if cmd != nil || m.planFormState.ActiveField != 0 {
		t.Fatalf("empty task advanced or dispatched: cmd=%v field=%d", cmd != nil, m.planFormState.ActiveField)
	}
	if !strings.Contains(m.View().Content, "Task is required") {
		t.Fatalf("empty task has no inline validation:\n%s", m.View().Content)
	}

	updated, _ = m.Update(charKey('x'))
	m = updated.(*Model)
	if m.planFormState.errorText != "" {
		t.Fatalf("valid task retained error %q", m.planFormState.errorText)
	}

	// Clearing the task after tabbing forward must still prevent final submit.
	m.planFormState.Fields[0].Input.SetValue("")
	m.planFormState.ActiveField = len(m.planFormState.Fields) - 1
	updated, cmd = m.Update(enterKey())
	m = updated.(*Model)
	if m.planFormState == nil {
		t.Fatal("empty final submit closed the plan form")
	}
	if cmd != nil || m.planFormState.ActiveField != 0 || m.planFormState.errorText == "" {
		t.Fatalf("empty final submit was not returned to Task: cmd=%v field=%d error=%q", cmd != nil, m.planFormState.ActiveField, m.planFormState.errorText)
	}
}

func TestPlanForm_AssemblePrompt_NoFocus(t *testing.T) {
	pf := NewPlanFormState("fix the bug", defaultThemeID)

	prompt := pf.AssemblePrompt()

	if !strings.Contains(prompt, "fix the bug") {
		t.Error("prompt should contain task")
	}
	if strings.Contains(prompt, "Focus:") {
		t.Error("prompt should not contain Focus when empty")
	}
}

func TestPlanForm_OpenClose(t *testing.T) {
	t.Run("open_sets_overlay", func(t *testing.T) {
		m := newTestModel(t)
		m.openPlanForm("test task")

		if m.overlay != OverlayPlanForm {
			t.Errorf("expected OverlayPlanForm, got %d", m.overlay)
		}
		if m.planFormState == nil {
			t.Fatal("planFormState should not be nil")
		}
		if m.planFormState.Fields[0].Input.Value() != "test task" {
			t.Error("task should be pre-filled")
		}
	})

	t.Run("close_resets_state", func(t *testing.T) {
		m := newTestModel(t)
		m.planFormState = NewPlanFormState("test", defaultThemeID)
		m.overlay = OverlayPlanForm

		m.closePlanForm()

		if m.planFormState != nil {
			t.Error("planFormState should be nil after close")
		}
		if m.overlay != OverlayNone {
			t.Errorf("overlay should be OverlayNone, got %d", m.overlay)
		}
	})
}

func TestPlanForm_EscCancels(t *testing.T) {
	m := newTestModel(t)
	m.openPlanForm("some task")

	updated, _ := m.Update(escKey())
	m = updated.(*Model)

	if m.planFormState != nil {
		t.Error("ESC should close plan form")
	}
	if m.overlay != OverlayNone {
		t.Errorf("overlay should be OverlayNone, got %d", m.overlay)
	}
}

func TestPlanForm_FieldNavigation(t *testing.T) {
	m := newTestModel(t)
	m.openPlanForm("task")

	// Initially on field 0.
	if m.planFormState.ActiveField != 0 {
		t.Fatalf("expected active field 0, got %d", m.planFormState.ActiveField)
	}

	// Tab advances to field 1.
	updated, _ := m.Update(tabKey())
	m = updated.(*Model)

	if m.planFormState.ActiveField != 1 {
		t.Errorf("expected active field 1, got %d", m.planFormState.ActiveField)
	}

	// Tab again advances to field 2.
	updated, _ = m.Update(tabKey())
	m = updated.(*Model)

	if m.planFormState.ActiveField != 2 {
		t.Errorf("expected active field 2, got %d", m.planFormState.ActiveField)
	}

	// Tab at last field stays on last field.
	updated, _ = m.Update(tabKey())
	m = updated.(*Model)

	if m.planFormState.ActiveField != 2 {
		t.Errorf("expected active field to stay at 2, got %d", m.planFormState.ActiveField)
	}
}

func TestPlanForm_SelectFieldLeftRight(t *testing.T) {
	m := newTestModel(t)
	m.openPlanForm("task")

	// Tab to scope field.
	updated, _ := m.Update(tabKey())
	m = updated.(*Model)

	if m.planFormState.ActiveField != 1 {
		t.Fatalf("expected field 1, got %d", m.planFormState.ActiveField)
	}
	if m.planFormState.Fields[1].OptionIndex != 0 {
		t.Fatalf("expected option 0, got %d", m.planFormState.Fields[1].OptionIndex)
	}

	// Right should advance to option 1.
	updated, _ = m.Update(rightKey())
	m = updated.(*Model)

	if m.planFormState.Fields[1].OptionIndex != 1 {
		t.Errorf("expected option 1 after right, got %d", m.planFormState.Fields[1].OptionIndex)
	}

	// Left should go back to option 0.
	updated, _ = m.Update(leftKey())
	m = updated.(*Model)

	if m.planFormState.Fields[1].OptionIndex != 0 {
		t.Errorf("expected option 0 after left, got %d", m.planFormState.Fields[1].OptionIndex)
	}
}

func TestPlanForm_SelectFieldBounds(t *testing.T) {
	m := newTestModel(t)
	m.openPlanForm("task")

	// Tab to scope field.
	updated, _ := m.Update(tabKey())
	m = updated.(*Model)

	// Up at 0 stays at 0.
	updated, _ = m.Update(upKey())
	m = updated.(*Model)

	if m.planFormState.Fields[1].OptionIndex != 0 {
		t.Errorf("expected option 0 after up at boundary, got %d", m.planFormState.Fields[1].OptionIndex)
	}

	// Left at 0 stays at 0.
	updated, _ = m.Update(leftKey())
	m = updated.(*Model)

	if m.planFormState.Fields[1].OptionIndex != 0 {
		t.Errorf("expected option 0 after left at boundary, got %d", m.planFormState.Fields[1].OptionIndex)
	}

	// Navigate to last option.
	updated, _ = m.Update(downKey())
	m = updated.(*Model)
	updated, _ = m.Update(downKey())
	m = updated.(*Model)

	if m.planFormState.Fields[1].OptionIndex != 2 {
		t.Fatalf("expected option 2, got %d", m.planFormState.Fields[1].OptionIndex)
	}

	// Down at last stays at last.
	updated, _ = m.Update(downKey())
	m = updated.(*Model)

	if m.planFormState.Fields[1].OptionIndex != 2 {
		t.Errorf("expected option 2 after down at boundary, got %d", m.planFormState.Fields[1].OptionIndex)
	}

	// Right at last stays at last.
	updated, _ = m.Update(rightKey())
	m = updated.(*Model)

	if m.planFormState.Fields[1].OptionIndex != 2 {
		t.Errorf("expected option 2 after right at boundary, got %d", m.planFormState.Fields[1].OptionIndex)
	}
}

func TestPlanForm_SelectField(t *testing.T) {
	m := newTestModel(t)
	m.openPlanForm("task")

	// Tab to scope field.
	updated, _ := m.Update(tabKey())
	m = updated.(*Model)

	if m.planFormState.ActiveField != 1 {
		t.Fatalf("expected field 1, got %d", m.planFormState.ActiveField)
	}

	// Down should cycle scope option.
	if m.planFormState.Fields[1].OptionIndex != 0 {
		t.Fatalf("expected option 0, got %d", m.planFormState.Fields[1].OptionIndex)
	}

	updated, _ = m.Update(downKey())
	m = updated.(*Model)

	if m.planFormState.Fields[1].OptionIndex != 1 {
		t.Errorf("expected option 1 after down, got %d", m.planFormState.Fields[1].OptionIndex)
	}
}

func TestPlanFormFitsSupportedTerminalSizes(t *testing.T) {
	sizes := []struct {
		name    string
		width   int
		height  int
		compact bool
	}{
		{name: "minimum", width: 30, height: 12, compact: true},
		{name: "narrow", width: 40, height: 20, compact: true},
		{name: "normal", width: 80, height: 24, compact: false},
	}

	for _, size := range sizes {
		for active := 0; active < 3; active++ {
			t.Run(size.name+"_"+strings.Repeat("step", active+1), func(t *testing.T) {
				m := newTestModel(t)
				m.width = size.width
				m.height = size.height
				m.openPlanForm("refactor the Unicode 模型 routing path without breaking compatibility")
				for m.planFormState.ActiveField < active {
					m.advancePlanFormField(1)
				}

				rendered := m.renderPlanForm()
				assertRenderedLinesFit(t, rendered, size.width)
				assertRenderedHeightFits(t, rendered, size.height)
				if strings.Contains(rendered, "> >") {
					t.Fatalf("plan form rendered duplicate input prompts:\n%s", rendered)
				}
				if !strings.Contains(rendered, "╰") {
					t.Fatalf("plan form lost its closing border:\n%s", rendered)
				}
				if !strings.Contains(rendered, "esc cancel") {
					t.Fatalf("plan form lost its cancellation affordance:\n%s", rendered)
				}

				if size.compact {
					progress := []string{"Plan · 1/3", "Plan · 2/3", "Plan · 3/3"}[active]
					if !strings.Contains(rendered, progress) {
						t.Fatalf("compact plan form missing progress %q:\n%s", progress, rendered)
					}
					for i, label := range []string{"Task", "Scope", "Focus (optional)"} {
						if (i == active) != strings.Contains(rendered, label) {
							t.Fatalf("compact plan form field visibility for %q is wrong:\n%s", label, rendered)
						}
					}
					return
				}

				for _, label := range []string{"Plan task", "Task", "Scope", "Focus (optional)"} {
					if !strings.Contains(rendered, label) {
						t.Fatalf("normal plan form missing %q:\n%s", label, rendered)
					}
				}
			})
		}
	}
}

func TestPlanFormFooterMatchesEnterBehavior(t *testing.T) {
	sizes := []struct {
		name   string
		width  int
		height int
	}{
		{name: "minimum", width: 30, height: 12},
		{name: "narrow", width: 40, height: 20},
		{name: "normal", width: 80, height: 24},
	}
	steps := []struct {
		active        int
		footer        string
		wantSubmitted bool
		wantNext      int
	}{
		{active: 0, footer: "next", wantNext: 1},
		{active: 1, footer: "next", wantNext: 2},
		{active: 2, footer: "enter submit", wantSubmitted: true, wantNext: 2},
	}

	for _, size := range sizes {
		for _, step := range steps {
			t.Run(size.name+"_"+strings.Repeat("enter", step.active+1), func(t *testing.T) {
				m := newTestModel(t)
				m.width = size.width
				m.height = size.height
				m.openPlanForm("plan this change")
				for m.planFormState.ActiveField < step.active {
					m.advancePlanFormField(1)
				}

				rendered := m.renderPlanForm()
				if !strings.Contains(rendered, step.footer) {
					t.Fatalf("step %d footer missing %q:\n%s", step.active+1, step.footer, rendered)
				}
				if !strings.Contains(rendered, "enter") {
					t.Fatalf("step %d footer does not name Enter behavior:\n%s", step.active+1, rendered)
				}
				if step.active < 2 && strings.Contains(rendered, "enter submit") {
					t.Fatalf("step %d falsely advertised submission:\n%s", step.active+1, rendered)
				}

				submitted, cancelled := m.updatePlanForm(enterKey())
				if cancelled {
					t.Fatalf("Enter cancelled step %d", step.active+1)
				}
				if submitted != step.wantSubmitted {
					t.Fatalf("step %d submitted = %v, want %v", step.active+1, submitted, step.wantSubmitted)
				}
				if got := m.planFormState.ActiveField; got != step.wantNext {
					t.Fatalf("step %d active field after Enter = %d, want %d", step.active+1, got, step.wantNext)
				}
			})
		}
	}
}
