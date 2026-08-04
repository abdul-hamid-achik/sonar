package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestRenderKeyHintsDegradesByPriority(t *testing.T) {
	m := newTestModel(t)
	hints := []keyHint{
		{Key: "Esc", Action: "Close"},
		{Key: "Enter", Action: "Select"},
		{Key: "↑/↓", Action: "Move"},
		{Key: "/", Action: "Filter"},
	}

	wide := ansi.Strip(m.renderKeyHints(80, hints...))
	if wide != "esc close · enter select · ↑/↓ move · / filter" {
		t.Fatalf("wide hints = %q", wide)
	}

	progressive := ansi.Strip(m.renderKeyHints(30, hints[:3]...))
	if progressive != "esc close · enter select · ↑/↓" {
		t.Fatalf("progressive hints hid an action that fits = %q", progressive)
	}

	compact := ansi.Strip(m.renderKeyHints(24, hints...))
	if compact != "esc close · enter · ↑/↓" {
		t.Fatalf("compact hints = %q", compact)
	}

	tiny := ansi.Strip(m.renderKeyHints(11, hints...))
	if !strings.HasPrefix(tiny, "esc") || strings.Contains(tiny, "filter") {
		t.Fatalf("tiny hints lost priority: %q", tiny)
	}
	if got := lipgloss.Width(tiny); got > 11 {
		t.Fatalf("tiny hint width = %d, want <= 11", got)
	}
}

// Two keys bound to one verb read as a bug. The grammar already has a form for
// alternatives, so they merge instead of appearing twice.
func TestKeyHintsMergeAliasesForOneAction(t *testing.T) {
	merged := mergeKeyHintAliases([]keyHint{
		{Key: "esc", Action: "cancel"},
		{Key: "enter", Action: "cancel"},
		{Key: "↑/↓", Action: "move"},
	})
	if len(merged) != 2 {
		t.Fatalf("expected 2 hints after merge, got %d: %+v", len(merged), merged)
	}
	if merged[0].Key != "esc/enter" || merged[0].Action != "cancel" {
		t.Fatalf("aliases did not merge into one hint: %+v", merged[0])
	}
	if merged[1].Key != "↑/↓" {
		t.Fatalf("unrelated hint was disturbed: %+v", merged[1])
	}
}

func TestKeyHintsKeepDistinctActionsAndRepeatedKeys(t *testing.T) {
	merged := mergeKeyHintAliases([]keyHint{
		{Key: "esc", Action: "cancel"},
		{Key: "enter", Action: "select"},
		{Key: "esc", Action: "cancel"},
	})
	if len(merged) != 2 {
		t.Fatalf("expected 2 hints, got %d: %+v", len(merged), merged)
	}
	// The repeated esc/cancel pair must not become "esc/esc".
	if merged[0].Key != "esc" {
		t.Fatalf("identical hint was duplicated into the key list: %+v", merged[0])
	}
}
