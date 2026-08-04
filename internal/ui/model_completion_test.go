package ui

import "testing"

func TestTriggerCompletion(t *testing.T) {
	t.Run("slash_triggers_command", func(t *testing.T) {
		m := newTestModel(t)
		m.triggerCompletion("/")

		if !m.isCompletionActive() {
			t.Error("/ should activate completion")
		}
		if m.completionState.Kind != "command" {
			t.Errorf("expected kind 'command', got %q", m.completionState.Kind)
		}
		if m.overlay != OverlayCompletion {
			t.Errorf("expected OverlayCompletion, got %d", m.overlay)
		}
		if len(m.completionState.AllItems) == 0 {
			t.Error("should have completion items for /")
		}
	})

	t.Run("at_triggers_attachments_with_multiselect", func(t *testing.T) {
		m := newTestModel(t)
		m.triggerCompletion("@")

		// @ triggers agent/file completion.
		// It may or may not find matches depending on agents + cwd.
		// If agents exist, it should activate.
		if m.isCompletionActive() {
			if m.completionState.Kind != "attachments" {
				t.Errorf("expected kind 'attachments', got %q", m.completionState.Kind)
			}
			if m.completionState.Selected == nil {
				t.Error("attachments should initialize Selected map")
			}
		}
	})

	t.Run("hash_triggers_skills", func(t *testing.T) {
		m := newTestModel(t)
		m.triggerCompletion("#")

		if !m.isCompletionActive() {
			t.Error("# should activate completion for skills")
		}
		if m.completionState.Kind != "skills" {
			t.Errorf("expected kind 'skills', got %q", m.completionState.Kind)
		}
		if m.completionState.Selected == nil {
			t.Error("skills should initialize Selected map")
		}
	})

	t.Run("no_matches_stays_inactive", func(t *testing.T) {
		m := newTestModel(t)
		m.triggerCompletion("/zzzznonexistent")

		if m.isCompletionActive() {
			t.Error("should not activate with no matches")
		}
	})

	t.Run("plain_text_no_trigger", func(t *testing.T) {
		m := newTestModel(t)
		m.triggerCompletion("hello")

		if m.isCompletionActive() {
			t.Error("plain text should not trigger completion")
		}
	})
}

func TestCommandCompletionReturnsArgumentsToComposer(t *testing.T) {
	m := newTestModel(t)
	m.triggerCompletion("/goal")
	if !m.isCompletionActive() {
		t.Fatal("goal completion did not open")
	}

	updated, _ := m.Update(charKey(' '))
	m = updated.(*Model)
	if m.isCompletionActive() || m.overlay != OverlayNone || m.input.Value() != "/goal " {
		t.Fatalf("argument handoff = overlay %d active=%v input=%q", m.overlay, m.isCompletionActive(), m.input.Value())
	}
	updated, _ = m.Update(charKey('3'))
	m = updated.(*Model)
	if m.isCompletionActive() || m.input.Value() != "/goal 3" {
		t.Fatalf("completion reopened while typing arguments: active=%v input=%q", m.isCompletionActive(), m.input.Value())
	}
}

func TestGoalActionCompletionIsReachableFromComposer(t *testing.T) {
	tests := []struct {
		name string
		open func(*testing.T, *Model) *Model
	}{
		{
			name: "typing action prefix",
			open: func(t *testing.T, m *Model) *Model {
				t.Helper()
				m.input.SetValue("/goal ")
				m.input.CursorEnd()
				m.completionSuppressedDraft = "/goal "
				updated, _ := m.Update(charKey('r'))
				m = updated.(*Model)
				updated, _ = m.Update(charKey('e'))
				return updated.(*Model)
			},
		},
		{
			name: "explicit tab",
			open: func(t *testing.T, m *Model) *Model {
				t.Helper()
				m.input.SetValue("/goal re")
				m.input.CursorEnd()
				updated, _ := m.Update(tabKey())
				return updated.(*Model)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := test.open(t, newTestModel(t))
			if !m.isCompletionActive() || len(m.completionState.FilteredItems) != 1 ||
				m.completionState.FilteredItems[0].Label != "/goal resume" {
				t.Fatalf("goal action completion = %#v", m.completionState)
			}
			updated, _ := m.Update(enterKey())
			m = updated.(*Model)
			if m.isCompletionActive() || m.input.Value() != "/goal resume " {
				t.Fatalf("accepted goal action: active=%v draft=%q", m.isCompletionActive(), m.input.Value())
			}
		})
	}
}

func TestAcceptCompletion(t *testing.T) {
	t.Run("single_select", func(t *testing.T) {
		m := newTestModel(t)
		items := []Completion{
			{Label: "/help", Insert: "/help "},
			{Label: "/clear", Insert: "/clear "},
		}
		m.completionState = newCompletionState("command", items, false, defaultThemeID)
		m.overlay = OverlayCompletion
		m.completionState.Index = 0

		m.acceptCompletion()

		if m.input.Value() != "/help " {
			t.Errorf("expected '/help ', got %q", m.input.Value())
		}
		if m.isCompletionActive() {
			t.Error("should be inactive after accept")
		}
		if m.overlay != OverlayNone {
			t.Error("overlay should be OverlayNone")
		}
	})

	t.Run("multi_select_with_selections", func(t *testing.T) {
		m := newTestModel(t)
		items := []Completion{
			{Label: "@a", Insert: "@a "},
			{Label: "@b", Insert: "@b "},
			{Label: "@c", Insert: "@c "},
		}
		m.completionState = newCompletionState("attachments", items, true, defaultThemeID)
		m.overlay = OverlayCompletion
		m.completionState.Index = 0
		m.completionState.Selected[1] = true

		m.acceptCompletion()

		if m.input.Value() != "@b " {
			t.Errorf("expected '@b ', got %q", m.input.Value())
		}
		if m.isCompletionActive() {
			t.Error("should be inactive after accept")
		}
	})

	t.Run("multi_select_empty_fallback", func(t *testing.T) {
		m := newTestModel(t)
		items := []Completion{
			{Label: "@x", Insert: "@x "},
			{Label: "@y", Insert: "@y "},
		}
		m.completionState = newCompletionState("attachments", items, true, defaultThemeID)
		m.overlay = OverlayCompletion
		m.completionState.Index = 1

		m.acceptCompletion()

		if m.input.Value() != "@y " {
			t.Errorf("expected '@y ' as fallback, got %q", m.input.Value())
		}
	})

	t.Run("inactive_noop", func(t *testing.T) {
		m := newTestModel(t)
		m.completionState = nil
		m.input.SetValue("original")

		m.acceptCompletion()

		if m.input.Value() != "original" {
			t.Errorf("inactive accept should be noop, got %q", m.input.Value())
		}
	})
}

func TestCloseCompletion(t *testing.T) {
	m := newTestModel(t)
	items := []Completion{{Label: "test"}}
	m.completionState = newCompletionState("command", items, true, defaultThemeID)
	m.completionState.Index = 5
	m.completionState.Selected[0] = true
	m.overlay = OverlayCompletion

	m.closeCompletion()

	if m.isCompletionActive() {
		t.Error("completionState should be nil")
	}
	if m.overlay != OverlayNone {
		t.Errorf("overlay should be OverlayNone, got %d", m.overlay)
	}
}

func TestFilterCompletions(t *testing.T) {
	items := []Completion{
		{Label: "/help"},
		{Label: "/clear"},
		{Label: "/model"},
	}

	t.Run("empty_query_returns_all", func(t *testing.T) {
		filtered := FilterCompletions(items, "")
		if len(filtered) != 3 {
			t.Errorf("expected 3, got %d", len(filtered))
		}
	})

	t.Run("filters_by_substring", func(t *testing.T) {
		filtered := FilterCompletions(items, "el")
		if len(filtered) != 2 {
			t.Errorf("expected 2 (help, model), got %d", len(filtered))
		}
	})

	t.Run("case_insensitive", func(t *testing.T) {
		filtered := FilterCompletions(items, "HELP")
		if len(filtered) != 1 {
			t.Errorf("expected 1, got %d", len(filtered))
		}
	})

	t.Run("hidden_alias_metadata", func(t *testing.T) {
		withAlias := []Completion{{Label: "/help", Description: "Show help", SearchTerms: "help h ?"}}
		filtered := FilterCompletions(withAlias, "?")
		if len(filtered) != 1 || filtered[0].Label != "/help" {
			t.Fatalf("alias metadata did not find the canonical row: %#v", filtered)
		}
	})

	t.Run("no_match", func(t *testing.T) {
		filtered := FilterCompletions(items, "zzz")
		if len(filtered) != 0 {
			t.Errorf("expected 0, got %d", len(filtered))
		}
	})
}
