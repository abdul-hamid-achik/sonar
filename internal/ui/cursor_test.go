package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func assertViewCursorAfter(t *testing.T, view tea.View, marker string) {
	t.Helper()
	if view.Cursor == nil {
		t.Fatalf("expected cursor after %q, got nil\n%s", marker, ansi.Strip(view.Content))
	}

	lines := strings.Split(ansi.Strip(view.Content), "\n")
	if view.Cursor.Y < 0 || view.Cursor.Y >= len(lines) {
		t.Fatalf("cursor row %d outside %d rendered lines", view.Cursor.Y, len(lines))
	}

	line := lines[view.Cursor.Y]
	markerAt := strings.Index(line, marker)
	if markerAt < 0 {
		t.Fatalf("cursor row %d does not contain %q: %q", view.Cursor.Y, marker, line)
	}
	wantX := lipgloss.Width(line[:markerAt] + marker)
	if view.Cursor.X != wantX {
		t.Fatalf("cursor = (%d,%d), want x=%d immediately after %q on %q",
			view.Cursor.X, view.Cursor.Y, wantX, marker, line)
	}
}

func TestViewCursorOwnsFocusedComposer(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("ship it")
	wasFocused := m.input.Focused()

	view := m.View()
	assertViewCursorAfter(t, view, "❯ ship it")
	if !m.input.VirtualCursor() {
		t.Fatal("View mutated the live composer cursor mode")
	}
	if m.input.Focused() != wasFocused {
		t.Fatal("View mutated live composer focus")
	}
}

func TestViewCursorOwnsRunningFollowUpComposer(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.input.SetValue("check the focused tests")
	m.input.CursorEnd()

	view := m.View()
	assertViewCursorAfter(t, view, "❯ check the focused tests")
}

func TestViewCursorOwnsCompletionFilter(t *testing.T) {
	m := newTestModel(t)
	m.completionState = newCompletionState("command", []Completion{{Label: "/help", Insert: "/help"}}, false, m.themeID, m.isDark)
	m.completionState.Filter.SetValue("help")
	m.completionState.Filter.CursorEnd()
	m.overlay = OverlayCompletion
	m.input.Blur()
	m.resizePickerOverlays()
	filterWasFocused := m.completionState.Filter.Focused()
	composerWasFocused := m.input.Focused()

	view := m.View()
	assertViewCursorAfter(t, view, completionFilterPrompt+"help")
	if !m.completionState.Filter.VirtualCursor() {
		t.Fatal("View mutated the live completion filter cursor mode")
	}
	if m.completionState.Filter.Focused() != filterWasFocused || m.input.Focused() != composerWasFocused {
		t.Fatal("View mutated completion or composer focus")
	}
}

func TestReducedMotionDisablesTransientInputCursorBlink(t *testing.T) {
	t.Run("main composer", func(t *testing.T) {
		t.Setenv("SONAR_REDUCED_MOTION", "1")
		m := newTestModel(t)
		if m.input.Styles().Cursor.Blink {
			t.Fatal("reduced motion left the main textarea cursor blinking")
		}
		view := m.View()
		if view.Cursor == nil || view.Cursor.Blink {
			t.Fatalf("reduced-motion composer cursor = %#v, want visible and static", view.Cursor)
		}

		m.mode = ModeAuto
		m.syncComposerAuthority()
		if m.input.Styles().Cursor.Blink {
			t.Fatal("authority restyle re-enabled the reduced-motion composer cursor")
		}
	})

	t.Run("completion filter", func(t *testing.T) {
		m := newTestModel(t)
		m.reducedMotion = true
		m.completionState = newCompletionState(
			"command", []Completion{{Label: "/help", Insert: "/help"}}, false, m.themeID, m.isDark, m.reducedMotion,
		)
		m.overlay = OverlayCompletion
		m.input.Blur()
		m.resizePickerOverlays()
		view := m.View()
		if view.Cursor == nil || view.Cursor.Blink {
			t.Fatalf("reduced-motion completion cursor = %#v, want visible and static", view.Cursor)
		}
	})

	t.Run("plan field", func(t *testing.T) {
		m := newTestModel(t)
		m.reducedMotion = true
		m.openPlanForm("keep the cursor still")
		view := m.View()
		if view.Cursor == nil || view.Cursor.Blink {
			t.Fatalf("reduced-motion Plan cursor = %#v, want visible and static", view.Cursor)
		}
	})
}

func TestViewCursorOwnsPlanTextFields(t *testing.T) {
	for _, test := range []struct {
		name   string
		width  int
		height int
		last   bool
		value  string
	}{
		{name: "normal_last_field_after_multiline_select", width: 80, height: 24, last: true, value: "tests first"},
		{name: "compact_first_field", width: minTerminalWidth, height: minTerminalHeight, value: "tiny plan"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: test.width, Height: test.height})
			m = updated.(*Model)
			m.openPlanForm("initial")

			fieldIndex := 0
			if test.last {
				m.advancePlanFormField(1)
				m.advancePlanFormField(1)
				fieldIndex = 2
			}
			m.planFormState.Fields[fieldIndex].Input.SetValue(test.value)
			m.planFormState.Fields[fieldIndex].Input.CursorEnd()
			fieldWasFocused := m.planFormState.Fields[fieldIndex].Input.Focused()
			composerWasFocused := m.input.Focused()

			view := m.View()
			assertViewCursorAfter(t, view, "> "+test.value)
			if !m.planFormState.Fields[fieldIndex].Input.VirtualCursor() {
				t.Fatal("View mutated the live plan input cursor mode")
			}
			if m.planFormState.Fields[fieldIndex].Input.Focused() != fieldWasFocused || m.input.Focused() != composerWasFocused {
				t.Fatal("View mutated plan or composer focus")
			}
		})
	}
}

func TestViewCursorHiddenWithoutTextOwnership(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Model)
	}{
		{
			name: "document_overlay_hides_composer",
			setup: func(m *Model) {
				m.overlay = OverlayHelp
			},
		},
		{
			name: "settings_overlay_hides_composer",
			setup: func(m *Model) {
				m.openSettingsPicker()
			},
		},
		{
			name: "plan_select_has_no_text_cursor",
			setup: func(m *Model) {
				m.openPlanForm("task")
				m.advancePlanFormField(1)
			},
		},
		{
			name: "paste_confirmation_owns_footer",
			setup: func(m *Model) {
				m.pendingPaste = assessPaste("one\ntwo", pasteCursorAtEnd(m.input.Value()), m.input.Length(), m.input.LineCount(), m.input.CharLimit)
			},
		},
		{
			name: "approval_confirmation_owns_footer",
			setup: func(m *Model) {
				m.pendingApproval = &ToolApprovalMsg{ToolName: "bash", Args: map[string]any{"command": "go test ./..."}}
			},
		},
		{
			name: "queued_follow_up_replaces_composer",
			setup: func(m *Model) {
				m.state = StateWaiting
				m.queuedFollowUp = &queuedFollowUp{Prompt: "already queued"}
			},
		},
		{
			name: "goal_turn_replaces_composer",
			setup: func(m *Model) {
				m.state = StateWaiting
				m.goalTurnID = "goal-turn"
			},
		},
		{
			name: "narrow_terminal_fallback",
			setup: func(m *Model) {
				m.width = minTerminalWidth - 1
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			test.setup(m)
			if cursor := m.View().Cursor; cursor != nil {
				t.Fatalf("unexpected cursor at (%d,%d)", cursor.X, cursor.Y)
			}
		})
	}
}

func TestOverlaySuppressesComposerPromptAndDraft(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("draft must stay private under settings")
	m.openSettingsPicker()

	view := m.View()
	plain := ansi.Strip(view.Content)
	if strings.Contains(plain, "draft must stay private") {
		t.Fatalf("overlay leaked the composer draft:\n%s", plain)
	}
	if strings.Contains(plain, "❯") {
		t.Fatalf("overlay retained an active-looking composer prompt:\n%s", plain)
	}
	if view.Cursor != nil {
		t.Fatalf("settings overlay retained cursor at (%d,%d)", view.Cursor.X, view.Cursor.Y)
	}
}

// The cursor follows the modal block, not the width of its own row. A short
// row inside a wide panel must not pull the caret away from the panel's column.
func TestOverlayCursorFollowsModalBlockColumn(t *testing.T) {
	base := strings.Join(make([]string, 9), "\n")
	overlay := "────────\n────\n────────"
	local := tea.NewCursor(2, 1)

	// Block width 8 in a 12-cell terminal anchors at X=2; the caret sits at +2.
	// Base is 9 rows, so the anchor row is 9/4 = 2; the caret is on row 1 of the
	// modal.
	got := overlayCursor(base, overlay, 12, local)
	if got == nil {
		t.Fatal("expected translated cursor")
		return
	}
	if got.X != 4 || got.Y != 3 {
		t.Fatalf("translated cursor = (%d,%d), want (4,3)", got.X, got.Y)
	}
	if local.X != 2 || local.Y != 1 {
		t.Fatalf("translation mutated child cursor: (%d,%d)", local.X, local.Y)
	}
}
