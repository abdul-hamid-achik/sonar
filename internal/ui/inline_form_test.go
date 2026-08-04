package ui

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

func resizeInlineFormTestModel(t *testing.T, width, height int) *Model {
	t.Helper()
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return updated.(*Model)
}

func assertInlineFormFrameOwnsPane(t *testing.T, m *Model, content string) {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, "╭") {
			continue
		}
		if plain := ansi.Strip(line); !strings.HasPrefix(plain, "╭") {
			t.Fatalf("inline form was horizontally centered: %q", plain)
		}
		if got := lipgloss.Width(line); got != m.chatPaneWidth() {
			t.Fatalf("inline frame width = %d, want chat pane width %d", got, m.chatPaneWidth())
		}
		return
	}
	t.Fatalf("inline form frame not found:\n%s", ansi.Strip(content))
}

func assertInlineViewCursorInBounds(t *testing.T, view tea.View) {
	t.Helper()
	if view.Cursor == nil {
		t.Fatalf("expected focused inline input cursor:\n%s", ansi.Strip(view.Content))
	}
	lines := strings.Split(ansi.Strip(view.Content), "\n")
	if view.Cursor.Y < 0 || view.Cursor.Y >= len(lines) {
		t.Fatalf("cursor row %d outside %d rendered rows", view.Cursor.Y, len(lines))
	}
	if view.Cursor.X < 0 || view.Cursor.X > lipgloss.Width(lines[view.Cursor.Y]) {
		t.Fatalf("cursor column %d outside row width %d: %q", view.Cursor.X, lipgloss.Width(lines[view.Cursor.Y]), lines[view.Cursor.Y])
	}
}

func seedLongInlineFormTranscript(m *Model) {
	m.entries = []ChatEntry{{Kind: "system", Content: strings.Repeat("transcript row\n", 120)}}
	m.invalidateEntryCache()
	m.refreshTranscript()
}

func TestPlanFormRendersInlineAfterTranscriptAtSupportedSizes(t *testing.T) {
	for _, size := range []struct {
		name   string
		width  int
		height int
	}{
		{name: "minimum", width: 30, height: 12},
		{name: "wide", width: 120, height: 36},
	} {
		t.Run(size.name, func(t *testing.T) {
			m := resizeInlineFormTestModel(t, size.width, size.height)
			m.setTestTranscriptContent("TRANSCRIPT SENTINEL\ntranscript tail")
			m.input.SetValue("unchanged composer draft")
			m.openPlanForm("plan this exact task")

			view := m.View()
			plain := ansi.Strip(view.Content)
			transcriptAt := strings.Index(plain, "TRANSCRIPT SENTINEL")
			formAt := strings.Index(plain, "Plan")
			if transcriptAt < 0 || formAt <= transcriptAt {
				t.Fatalf("transcript/form order is wrong: transcript=%d form=%d\n%s", transcriptAt, formAt, plain)
			}
			if strings.Contains(plain, "unchanged composer draft") || m.input.Value() != "unchanged composer draft" {
				t.Fatalf("inline Plan form did not replace the composer without changing its draft: value=%q\n%s", m.input.Value(), plain)
			}
			assertInlineFormFrameOwnsPane(t, m, view.Content)
			assertInlineViewCursorInBounds(t, view)
			assertRenderedLinesFit(t, view.Content, size.width)
			assertRenderedHeightFits(t, view.Content, size.height)
			if m.viewport.Height() < 1 || m.viewport.Height() != m.viewportHeight() {
				t.Fatalf("transcript rows = %d, calculated = %d", m.viewport.Height(), m.viewportHeight())
			}
		})
	}
}

func TestInlineFormsFitEveryActiveFieldAtMinimumSize(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		for active := 0; active < 3; active++ {
			m := resizeInlineFormTestModel(t, minTerminalWidth, minTerminalHeight)
			m.setTestTranscriptContent("visible transcript")
			m.openPlanForm("minimum plan")
			for m.planFormState.ActiveField < active {
				updated, _ := m.Update(tabKey())
				m = updated.(*Model)
			}

			view := m.View()
			assertRenderedLinesFit(t, view.Content, minTerminalWidth)
			assertRenderedHeightFits(t, view.Content, minTerminalHeight)
			if m.viewport.Height() < 1 || !strings.Contains(ansi.Strip(view.Content), "visible transcript") {
				t.Fatalf("active Plan field %d hid the transcript:\n%s", active, ansi.Strip(view.Content))
			}
			if active == 1 {
				if view.Cursor != nil {
					t.Fatalf("select field exposed a fake cursor at (%d,%d)", view.Cursor.X, view.Cursor.Y)
				}
			} else {
				assertInlineViewCursorInBounds(t, view)
			}
		}
	})

	t.Run("goal", func(t *testing.T) {
		for field := GoalFieldObjective; field < goalFormFieldCount; field++ {
			m := resizeInlineFormTestModel(t, minTerminalWidth, minTerminalHeight)
			m.setTestTranscriptContent("visible transcript")
			if err := m.openGoalForm("minimum goal", false); err != nil {
				t.Fatalf("open goal: %v", err)
			}
			m.goalFormState.acceptance.SetValue("first\nsecond\nfinal")
			m.goalFormState.acceptance.MoveToEnd()
			m.goalFormState.invalidate()
			for m.goalFormState.ActiveField() < field {
				updated, _ := m.Update(tabKey())
				m = updated.(*Model)
			}

			view := m.View()
			assertRenderedLinesFit(t, view.Content, minTerminalWidth)
			assertRenderedHeightFits(t, view.Content, minTerminalHeight)
			if m.viewport.Height() < 1 || !strings.Contains(ansi.Strip(view.Content), "visible transcript") {
				t.Fatalf("active Goal field %d hid the transcript:\n%s", field, ansi.Strip(view.Content))
			}
			if field == GoalFieldActions {
				if view.Cursor != nil {
					t.Fatalf("Goal actions exposed a fake cursor at (%d,%d)", view.Cursor.X, view.Cursor.Y)
				}
			} else {
				assertInlineViewCursorInBounds(t, view)
			}
			if field == GoalFieldAcceptance {
				line := strings.Split(ansi.Strip(view.Content), "\n")[view.Cursor.Y]
				if !strings.Contains(line, "final") {
					t.Fatalf("multiline acceptance cursor row does not show its final value: %q\n%s", line, ansi.Strip(view.Content))
				}
			}
		}
	})
}

func TestInlineFormLifecyclePreservesPausedTranscriptAndDraft(t *testing.T) {
	assertAnchor := func(t *testing.T, m *Model, want int, stage string) {
		t.Helper()
		if got := m.transcriptYOffset(); got != want || !m.followPaused() {
			t.Fatalf("%s moved paused anchor to %d (want %d), paused=%v", stage, got, want, m.followPaused())
		}
	}

	t.Run("plan_open_field_resize_theme_cancel", func(t *testing.T) {
		m := newTestModel(t)
		seedLongInlineFormTranscript(m)
		m.setTranscriptYOffset(7)
		m.pauseFollow()
		m.input.SetValue("composer draft survives")

		m.openPlanForm("anchored plan")
		assertAnchor(t, m, 7, "open")
		updated, _ := m.Update(tabKey())
		m = updated.(*Model)
		assertAnchor(t, m, 7, "field change")
		updated, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		m = updated.(*Model)
		assertAnchor(t, m, 7, "resize")
		updated, _ = m.Update(tea.BackgroundColorMsg{Color: color.White})
		m = updated.(*Model)
		assertAnchor(t, m, 7, "theme")
		updated, _ = m.Update(escKey())
		m = updated.(*Model)
		assertAnchor(t, m, 7, "cancel")
		if m.input.Value() != "composer draft survives" || m.planFormState != nil || m.overlay != OverlayNone {
			t.Fatalf("cancel changed composer/form state: draft=%q form=%v overlay=%v", m.input.Value(), m.planFormState != nil, m.overlay)
		}
	})

	t.Run("plan_submit_exact", func(t *testing.T) {
		m := newTestModel(t)
		seedLongInlineFormTranscript(m)
		m.setTranscriptYOffset(9)
		m.pauseFollow()
		m.input.SetValue("next composer draft")
		m.openPlanForm("submit this plan")
		m.planFormState.Fields[1].OptionIndex = 1
		m.planFormState.Fields[2].Input.SetValue("keep exact focus")
		wantPrompt := m.planFormState.AssemblePrompt()
		for m.planFormState.ActiveField < 2 {
			updated, _ := m.Update(tabKey())
			m = updated.(*Model)
		}
		updated, _ := m.Update(enterKey())
		m = updated.(*Model)
		assertAnchor(t, m, 9, "submit")
		if m.planFormState != nil || m.overlay != OverlayNone || m.input.Value() != "next composer draft" {
			t.Fatalf("submit left hidden form state or changed draft: form=%v overlay=%v draft=%q", m.planFormState != nil, m.overlay, m.input.Value())
		}
		if len(m.entries) == 0 || m.entries[len(m.entries)-1].Kind != "user" || m.entries[len(m.entries)-1].Content != wantPrompt {
			t.Fatalf("submitted prompt differs from assembled values: want %q entries=%#v", wantPrompt, m.entries)
		}
	})

	t.Run("goal_field_validation_resize_cancel", func(t *testing.T) {
		m := newTestModel(t)
		seedLongInlineFormTranscript(m)
		m.setTranscriptYOffset(11)
		m.pauseFollow()
		m.input.SetValue("goal composer draft")
		if err := m.openGoalForm("anchored goal", false); err != nil {
			t.Fatalf("open goal: %v", err)
		}
		assertAnchor(t, m, 11, "open")
		updated, _ := m.Update(tabKey())
		m = updated.(*Model)
		assertAnchor(t, m, 11, "field change")
		updated, _ = m.Update(tea.WindowSizeMsg{Width: 96, Height: 31})
		m = updated.(*Model)
		assertAnchor(t, m, 11, "resize")
		m.goalFormState.acceptance.SetValue("done is verifiable")
		m.goalFormState.turns.SetValue("")
		m.goalFormState.tokens.SetValue("")
		m.goalFormState.time.SetValue("")
		m.goalFormState.invalidate()
		for m.goalFormState.ActiveField() < GoalFieldActions {
			updated, _ = m.Update(tabKey())
			m = updated.(*Model)
		}
		updated, _ = m.Update(enterKey())
		m = updated.(*Model)
		assertAnchor(t, m, 11, "validation submit")
		if m.goalFormState == nil || m.goalFormState.Error() == "" {
			t.Fatal("invalid Goal submit did not remain editable with feedback")
		}
		updated, _ = m.Update(escKey())
		m = updated.(*Model)
		assertAnchor(t, m, 11, "cancel")
		if m.input.Value() != "goal composer draft" || m.goalFormState != nil {
			t.Fatalf("Goal cancel changed draft/form state: draft=%q form=%v", m.input.Value(), m.goalFormState != nil)
		}
	})
}

func TestInlineFormAuthorityIsDeterministic(t *testing.T) {
	t.Run("completion_yields_to_explicit_form", func(t *testing.T) {
		m := newTestModel(t)
		m.input.SetValue("/he")
		m.input.CursorEnd()
		_ = m.triggerCompletion(m.input.Value())
		if !m.isCompletionActive() {
			t.Fatal("completion fixture did not open")
		}
		m.openPlanForm("own the composer")
		if m.completionState != nil || m.overlay != OverlayPlanForm || m.planFormState == nil {
			t.Fatalf("completion/form authority = completion:%v overlay:%v form:%v", m.completionState != nil, m.overlay, m.planFormState != nil)
		}
	})

	t.Run("approval_cancels_transient_form", func(t *testing.T) {
		m := newTestModel(t)
		m.openPlanForm("transient plan")
		responses := make(chan permission.ApprovalResponse, 1)
		if err := m.openApproval(ToolApprovalMsg{ToolName: "bash", Args: map[string]any{"command": "go test ./..."}, Response: responses}); err != nil {
			t.Fatalf("open approval: %v", err)
		}
		if m.pendingApproval == nil || m.planFormState != nil || m.goalFormState != nil || m.overlay != OverlayNone {
			t.Fatalf("approval left hidden form authority: approval=%v plan=%v goal=%v overlay=%v", m.pendingApproval != nil, m.planFormState != nil, m.goalFormState != nil, m.overlay)
		}
	})

	t.Run("paste_queue_and_active_form_block_new_form", func(t *testing.T) {
		m := newTestModel(t)
		m.pendingPaste = assessPaste("one\ntwo\nthree", pasteCursorAtEnd(""), 0, 1, m.input.CharLimit)
		m.openPlanForm("must not hide paste")
		if m.planFormState != nil || m.overlay != OverlayNone {
			t.Fatal("Plan opened behind paste review")
		}

		m.pendingPaste = nil
		m.queuedFollowUp = &queuedFollowUp{Prompt: "already queued"}
		if err := m.openGoalForm("must not hide queue", false); err == nil || m.goalFormState != nil {
			t.Fatalf("Goal opened behind queue: err=%v form=%v", err, m.goalFormState != nil)
		}

		m.queuedFollowUp = nil
		m.openPlanForm("first owner")
		if err := m.openGoalForm("second owner", false); err == nil || m.goalFormState != nil || m.overlay != OverlayPlanForm {
			t.Fatalf("Goal coexisted with Plan: err=%v goal=%v overlay=%v", err, m.goalFormState != nil, m.overlay)
		}
	})
}
