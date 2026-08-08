package ui

import (
	"strings"
	"testing"
)

// The tab marker appears exactly for human-owed states: pending prompts
// always, and a turn that finished while the window was unfocused until focus
// returns — arriving is acknowledging.
func TestWindowTitleMarksAttention(t *testing.T) {
	m := newTestModel(t)
	if title := m.View().WindowTitle; strings.HasPrefix(title, "●") {
		t.Fatalf("idle tab carries the marker: %q", title)
	}
	if m.windowAttentionRequested() {
		t.Fatal("idle model requests attention")
	}

	m.pendingApproval = &ToolApprovalMsg{}
	if !m.windowAttentionRequested() {
		t.Fatal("pending approval does not request attention")
	}
	m.pendingApproval = nil

	m.noteTerminalFocus(false)
	m.turnUnseen = true
	title := m.View().WindowTitle
	if !strings.HasPrefix(title, "● ") || !strings.Contains(title, "needs you") {
		t.Fatalf("unseen turn did not mark the tab: %q", title)
	}

	m.noteTerminalFocus(true)
	if m.turnUnseen {
		t.Fatal("focus did not acknowledge the unseen turn")
	}
	if title := m.View().WindowTitle; strings.HasPrefix(title, "●") {
		t.Fatalf("acknowledged tab still carries the marker: %q", title)
	}
}

// The subagents panel opens from /agents, renders its empty state without a
// crash, survives selection keys with no children, and closes with esc.
func TestSubagentsPanelOpensAndCloses(t *testing.T) {
	m := newTestModel(t)
	cmd := m.openSubagentsPanel()
	if cmd == nil {
		t.Fatal("open returned no repaint tick")
	}
	if m.overlay != OverlaySubagents {
		t.Fatalf("overlay = %v, want subagents", m.overlay)
	}
	view := m.View().Content
	if !strings.Contains(view, "Subagents") || !strings.Contains(view, "No subagents") {
		t.Fatalf("panel did not render its empty state:\n%s", view)
	}
	m.moveSubagentSelection(1)
	m.moveSubagentSelection(-1)
	if tick := m.handleSubagentsPanelTick(); tick == nil {
		t.Fatal("tick chain stopped while the panel is open")
	}
	m.closeSubagentsPanel()
	if m.overlay != OverlayNone {
		t.Fatalf("overlay after close = %v", m.overlay)
	}
	if tick := m.handleSubagentsPanelTick(); tick != nil {
		t.Fatal("tick chain survived the panel closing")
	}
}
