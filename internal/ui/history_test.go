package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestPushHistory_Basic(t *testing.T) {
	m := newTestModel(t)
	m.pushHistory("hello")
	m.pushHistory("world")

	if len(m.promptHistory) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(m.promptHistory))
	}
	if m.promptHistory[0] != "hello" || m.promptHistory[1] != "world" {
		t.Errorf("unexpected history: %v", m.promptHistory)
	}
}

func TestPushHistory_Empty(t *testing.T) {
	m := newTestModel(t)
	m.pushHistory("")
	if len(m.promptHistory) != 0 {
		t.Error("empty string should not be added to history")
	}
}

func TestPushHistory_DedupConsecutive(t *testing.T) {
	m := newTestModel(t)
	m.pushHistory("hello")
	m.pushHistory("hello")
	m.pushHistory("hello")

	if len(m.promptHistory) != 1 {
		t.Errorf("expected 1 entry after dedup, got %d", len(m.promptHistory))
	}
}

func TestPushHistory_DedupNonConsecutive(t *testing.T) {
	m := newTestModel(t)
	m.pushHistory("hello")
	m.pushHistory("world")
	m.pushHistory("hello")

	if len(m.promptHistory) != 3 {
		t.Errorf("non-consecutive duplicates should be kept, got %d", len(m.promptHistory))
	}
}

func TestPushHistory_CapAt100(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < 110; i++ {
		m.pushHistory(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}

	if len(m.promptHistory) > 100 {
		t.Errorf("history should be capped at 100, got %d", len(m.promptHistory))
	}
}

func TestNavigateHistory_EmptyHistory(t *testing.T) {
	m := newTestModel(t)
	if m.navigateHistory(-1) {
		t.Error("up on empty history should return false")
	}
	if m.navigateHistory(1) {
		t.Error("down on empty history should return false")
	}
}

func TestNavigateHistory_UpDown(t *testing.T) {
	m := newTestModel(t)
	m.pushHistory("first")
	m.pushHistory("second")
	m.pushHistory("third")

	// Set current input
	m.input.SetValue("current")

	// Press up: should go to "third" (most recent)
	if !m.navigateHistory(-1) {
		t.Fatal("up should succeed")
	}
	if m.input.Value() != "third" {
		t.Errorf("expected 'third', got %q", m.input.Value())
	}
	if m.historySaved != "current" {
		t.Errorf("current input should be saved, got %q", m.historySaved)
	}

	// Press up again: "second"
	if !m.navigateHistory(-1) {
		t.Fatal("up should succeed")
	}
	if m.input.Value() != "second" {
		t.Errorf("expected 'second', got %q", m.input.Value())
	}

	// Press up again: "first"
	if !m.navigateHistory(-1) {
		t.Fatal("up should succeed")
	}
	if m.input.Value() != "first" {
		t.Errorf("expected 'first', got %q", m.input.Value())
	}

	// Press up again: at oldest, should fail
	if m.navigateHistory(-1) {
		t.Error("up at oldest should return false")
	}

	// Press down: "second"
	if !m.navigateHistory(1) {
		t.Fatal("down should succeed")
	}
	if m.input.Value() != "second" {
		t.Errorf("expected 'second', got %q", m.input.Value())
	}

	// Press down: "third"
	if !m.navigateHistory(1) {
		t.Fatal("down should succeed")
	}
	if m.input.Value() != "third" {
		t.Errorf("expected 'third', got %q", m.input.Value())
	}

	// Press down past newest: restore saved input
	if !m.navigateHistory(1) {
		t.Fatal("down past newest should succeed")
	}
	if m.input.Value() != "current" {
		t.Errorf("expected restored 'current', got %q", m.input.Value())
	}
	if m.historyIndex != -1 {
		t.Errorf("historyIndex should be -1 after exiting history, got %d", m.historyIndex)
	}
}

func TestNavigateHistory_DownNotBrowsing(t *testing.T) {
	m := newTestModel(t)
	m.pushHistory("hello")

	// Down without first pressing up should return false
	if m.navigateHistory(1) {
		t.Error("down when not browsing should return false")
	}
}

func TestNavigateHistoryReflowsHeightAndKeepsTailVisible(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	long := strings.Repeat("history words ", 70) + "HISTORY-TAIL"
	m.pushHistory("short")
	m.pushHistory(long)

	if !m.navigateHistory(-1) {
		t.Fatal("could not navigate to long history entry")
	}
	if m.inputLines != composerVisibleRowLimit(m.height) || m.input.ScrollYOffset() <= 0 {
		t.Fatalf("long history layout = rows %d offset %d", m.inputLines, m.input.ScrollYOffset())
	}
	if !strings.Contains(ansi.Strip(m.input.View()), "HISTORY-TAIL") {
		t.Fatalf("long history hid the cursor tail:\n%s", m.input.View())
	}

	if !m.navigateHistory(-1) {
		t.Fatal("could not navigate to short history entry")
	}
	if m.input.Value() != "short" || m.inputLines != 1 || m.input.Height() != 1 {
		t.Fatalf("short history layout = %q, parent %d child %d", m.input.Value(), m.inputLines, m.input.Height())
	}
}

func TestHistoryKey_OnlyWhenIdleAndEmpty(t *testing.T) {
	t.Run("idle_empty_with_history", func(t *testing.T) {
		m := newTestModel(t)
		m.pushHistory("hello")
		m.state = StateIdle
		m.overlay = OverlayNone

		updated, _ := m.Update(upKey())
		m = updated.(*Model)

		if m.input.Value() != "hello" {
			t.Errorf("up key should navigate history, got %q", m.input.Value())
		}
	})

	t.Run("idle_nonempty_no_history", func(t *testing.T) {
		m := newTestModel(t)
		m.pushHistory("hello")
		m.state = StateIdle
		m.overlay = OverlayNone
		m.input.SetValue("typing something")

		updated, _ := m.Update(upKey())
		m = updated.(*Model)

		// Should NOT navigate history when input has content and not already browsing
		if m.historyIndex != -1 {
			t.Error("up key should not navigate history when input is non-empty")
		}
	})

	t.Run("waiting_no_history", func(t *testing.T) {
		m := newTestModel(t)
		m.pushHistory("hello")
		m.state = StateWaiting

		updated, _ := m.Update(upKey())
		m = updated.(*Model)

		if m.historyIndex != -1 {
			t.Error("up key should not navigate history when not idle")
		}
	})

	t.Run("overlay_no_history", func(t *testing.T) {
		m := newTestModel(t)
		m.pushHistory("hello")
		m.state = StateIdle
		m.overlay = OverlayHelp

		updated, _ := m.Update(upKey())
		m = updated.(*Model)

		if m.historyIndex != -1 {
			t.Error("up key should not navigate history when overlay is open")
		}
	})
}

func TestHistoryKey_AlreadyBrowsing(t *testing.T) {
	m := newTestModel(t)
	m.pushHistory("first")
	m.pushHistory("second")
	m.state = StateIdle
	m.overlay = OverlayNone
	// Input must be empty to start browsing
	m.input.SetValue("")

	// Press up — enters history (input is empty, so allowed)
	updated, _ := m.Update(upKey())
	m = updated.(*Model)
	if m.input.Value() != "second" {
		t.Fatalf("expected 'second', got %q", m.input.Value())
	}

	// Now input is non-empty (from history), up should still work because historyIndex != -1
	updated, _ = m.Update(upKey())
	m = updated.(*Model)
	if m.input.Value() != "first" {
		t.Errorf("expected 'first', got %q", m.input.Value())
	}
}
