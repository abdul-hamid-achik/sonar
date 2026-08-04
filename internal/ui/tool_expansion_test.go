package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestToolCallStartStoresSemanticSummaryForPersistence(t *testing.T) {
	m := newTestModel(t)
	args := map[string]any{"path": "internal/ui/session.go"}
	updated, _ := m.Update(ToolCallStartMsg{
		ID: "read-1", Name: "read_file", Args: args, StartTime: time.Now(),
	})
	m = updated.(*Model)

	if len(m.toolEntries) != 1 {
		t.Fatalf("tool lifecycle was not created: entries=%d", len(m.toolEntries))
	}
	if got, want := m.toolEntries[0].Summary, "internal/ui/session.go"; got != want {
		t.Fatalf("stored tool summary = %q, want %q", got, want)
	}
	if got, want := testProjectedToolCard(t, m, 0).Summary, m.toolEntries[0].Summary; got != want {
		t.Fatalf("live card summary = %q, want stored summary %q", got, want)
	}

	args["path"] = "internal/ui/model.go"
	if got := m.toolEntries[0].Summary; got != "internal/ui/session.go" {
		t.Fatalf("stored summary changed with ephemeral arguments: %q", got)
	}
}

func TestPerEntryCollapse_Default(t *testing.T) {
	m := newTestModel(t)
	m.toolsCollapsed = true

	// Simulate tool call start — new entry should inherit collapse state.
	updated, _ := m.Update(ToolCallStartMsg{
		Name: "read_file",
		Args: map[string]any{"path": "test.go"},
	})
	m = updated.(*Model)

	if len(m.toolEntries) != 1 {
		t.Fatalf("expected 1 tool entry, got %d", len(m.toolEntries))
	}
	if !m.toolEntries[0].Collapsed {
		t.Error("new tool entry should inherit toolsCollapsed=true")
	}
}

func TestPerEntryCollapse_InheritsFalse(t *testing.T) {
	m := newTestModel(t)
	m.toolsCollapsed = false

	updated, _ := m.Update(ToolCallStartMsg{
		Name: "bash",
		Args: map[string]any{"command": "ls"},
	})
	m = updated.(*Model)

	if m.toolEntries[0].Collapsed {
		t.Error("new tool entry should inherit toolsCollapsed=false")
	}
}

func TestBatchToggleAll(t *testing.T) {
	m := newTestModel(t)
	m.toolsCollapsed = true

	// Add multiple tool entries.
	m.toolEntries = []ToolEntry{
		{Name: "a", Status: ToolStatusDone, Collapsed: true},
		{Name: "b", Status: ToolStatusDone, Collapsed: true},
		{Name: "c", Status: ToolStatusDone, Collapsed: false},
	}

	// A batch toggle should flip toolsCollapsed and apply to all.
	m.toolsCollapsed = !m.toolsCollapsed // false now
	for i := range m.toolEntries {
		m.toolEntries[i].Collapsed = m.toolsCollapsed
	}

	for i, te := range m.toolEntries {
		if te.Collapsed {
			t.Errorf("entry[%d] should be expanded after batch toggle", i)
		}
	}
}

func TestToggleLastTool(t *testing.T) {
	m := newTestModel(t)

	m.toolEntries = []ToolEntry{
		{Name: "a", Status: ToolStatusDone, Collapsed: true},
		{Name: "b", Status: ToolStatusDone, Collapsed: true},
	}

	// Toggle last only.
	last := len(m.toolEntries) - 1
	m.toolEntries[last].Collapsed = !m.toolEntries[last].Collapsed

	if m.toolEntries[0].Collapsed != true {
		t.Error("first entry should remain collapsed")
	}
	if m.toolEntries[1].Collapsed != false {
		t.Error("last entry should be expanded")
	}
}

func TestCtrlRTogglesLastToolThroughBubbleTeaKeyName(t *testing.T) {
	m := newTestModel(t)
	m.toolEntries = []ToolEntry{{Name: "write", Status: ToolStatusError, Collapsed: true}}

	updated, _ := m.Update(ctrlKey('r'))
	m = updated.(*Model)
	if m.toolEntries[0].Collapsed {
		t.Fatal("Ctrl+R did not expand the advertised last ToolCard")
	}
	if m.input.Value() != "" {
		t.Fatalf("ToolCard chord leaked into the composer: %q", m.input.Value())
	}
}

func TestCtrlRInspectionRevealsOffscreenReceiptAndRestoresFollow(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(ToolCallStartMsg{
		ID: "inspect-1", Name: "bash", Args: map[string]any{"command": "go test ./..."}, StartTime: time.Now(),
	})
	m = updated.(*Model)
	updated, _ = m.Update(ToolCallResultMsg{
		ID: "inspect-1", Name: "bash", Result: "ok\nsecond detail", Duration: time.Millisecond,
	})
	m = updated.(*Model)
	m.entries = append(m.entries, ChatEntry{Kind: "assistant", Content: strings.Repeat("A later answer keeps the receipt above the fold.\n", 40)})
	m.lastTurnToolIndex = 0
	m.footerNotice = &footerNotice{text: "✓ Done", severity: noticeSuccess}
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.markFollowingLatest()
	m.transcriptGotoBottom()
	if len(m.toolHitRegions) != 1 {
		t.Fatalf("tool hit regions = %#v", m.toolHitRegions)
	}
	toolRow := m.toolHitRegions[0].Row
	if toolRow >= m.transcriptYOffset() {
		t.Fatalf("test receipt was not offscreen: row=%d offset=%d", toolRow, m.transcriptYOffset())
	}

	updated, _ = m.Update(ctrlKey('r'))
	m = updated.(*Model)
	if m.toolEntries[0].Collapsed || !m.followPaused() {
		t.Fatalf("inspection did not expand and pause at receipt: collapsed=%v paused=%v", m.toolEntries[0].Collapsed, m.followPaused())
	}
	if toolRow < m.transcriptYOffset() || toolRow >= m.transcriptYOffset()+m.viewport.Height() {
		t.Fatalf("expanded receipt header remains offscreen: row=%d offset=%d height=%d", toolRow, m.transcriptYOffset(), m.viewport.Height())
	}

	updated, _ = m.Update(ctrlKey('r'))
	m = updated.(*Model)
	if !m.toolEntries[0].Collapsed || m.followPaused() || !m.transcriptAtBottom() {
		t.Fatalf("hiding receipt did not restore latest-follow: collapsed=%v paused=%v bottom=%v", m.toolEntries[0].Collapsed, m.followPaused(), m.transcriptAtBottom())
	}
}

func TestReceiptInspectionSettlesBeforeResizeOrBatchToggle(t *testing.T) {
	for _, test := range []struct {
		name string
		act  func(*Model) *Model
	}{
		{
			name: "resize",
			act: func(m *Model) *Model {
				updated, _ := m.Update(tea.WindowSizeMsg{Width: 64, Height: 20})
				return updated.(*Model)
			},
		},
		{
			name: "batch toggle",
			act: func(m *Model) *Model {
				updated, _ := m.Update(ctrlKey('b'))
				return updated.(*Model)
			},
		},
		{
			name: "jump latest",
			act: func(m *Model) *Model {
				updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
				return updated.(*Model)
			},
		},
		{
			name: "thinking toggle",
			act: func(m *Model) *Model {
				updated, _ := m.Update(ctrlKey('t'))
				return updated.(*Model)
			},
		},
		{
			name: "clear view",
			act: func(m *Model) *Model {
				updated, _ := m.Update(ctrlKey('l'))
				return updated.(*Model)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.entries = append(m.entries,
				ChatEntry{Kind: "tool_group", ToolIndex: 0},
				ChatEntry{Kind: "assistant", Content: strings.Repeat("later transcript row\n", 40)},
			)
			m.toolEntries = []ToolEntry{{Name: "bash", Status: ToolStatusDone, Collapsed: true}}
			m.lastTurnToolIndex = 0
			m.invalidateEntryCache()
			m.refreshTranscript()
			m.markFollowingLatest()
			m.transcriptGotoBottom()
			updated, _ := m.Update(ctrlKey('r'))
			m = updated.(*Model)
			if !m.receiptInspectActive || !m.followPaused() {
				t.Fatal("precondition: keyboard receipt inspection did not activate")
			}

			m = test.act(m)
			if m.receiptInspectActive || m.receiptInspectToolIndex != -1 {
				t.Fatalf("%s retained stale receipt anchor: active=%v index=%d", test.name, m.receiptInspectActive, m.receiptInspectToolIndex)
			}
			if m.followPaused() || !m.transcriptAtBottom() {
				t.Fatalf("%s did not restore latest-follow: paused=%v bottom=%v", test.name, m.followPaused(), m.transcriptAtBottom())
			}
		})
	}
}

func TestReceiptInspectionSurvivesHeightOnlyResize(t *testing.T) {
	m := newTestModel(t)
	m.entries = append(m.entries, ChatEntry{Kind: "tool_group", ToolIndex: 0})
	m.toolEntries = []ToolEntry{{Name: "bash", Status: ToolStatusDone, Collapsed: true}}
	m.lastTurnToolIndex = 0
	m.invalidateEntryCache()
	m.refreshTranscript()
	updated, _ := m.Update(ctrlKey('r'))
	m = updated.(*Model)
	if !m.receiptInspectActive {
		t.Fatal("precondition: receipt inspection did not activate")
	}

	updated, _ = m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height + 3})
	m = updated.(*Model)
	if !m.receiptInspectActive || m.toolEntries[0].Collapsed {
		t.Fatalf("height-only resize settled receipt inspection: active=%v collapsed=%v", m.receiptInspectActive, m.toolEntries[0].Collapsed)
	}
}

func TestFileWriteSnapshotBefore(t *testing.T) {
	m := newTestModel(t)

	// Tool name containing "write" triggers snapshot.
	updated, _ := m.Update(ToolCallStartMsg{
		Name: "file_write",
		Args: map[string]any{"path": "/nonexistent/path"},
	})
	m = updated.(*Model)

	if len(m.toolEntries) != 1 {
		t.Fatalf("expected 1 tool entry, got %d", len(m.toolEntries))
	}
	// BeforeContent should be empty since file doesn't exist, but it should not panic.
	if m.toolEntries[0].BeforeContent != "" {
		t.Error("nonexistent file should give empty before content")
	}
}

func TestCollapsedDefaultNeverHidesToolError(t *testing.T) {
	m := newTestModel(t)
	m.toolsCollapsed = true
	m.entries = append(m.entries, ChatEntry{Kind: "user", Content: "run the check"})

	updated, _ := m.Update(ToolCallStartMsg{
		ID: "failed-call", Name: "bash", Args: map[string]any{"command": "go test ./..."},
	})
	m = updated.(*Model)
	updated, _ = m.Update(ToolCallResultMsg{
		ID: "failed-call", Name: "bash", Result: "permission denied", IsError: true,
	})
	m = updated.(*Model)

	rendered := m.renderEntries()
	for _, want := range []string{"go test ./...", "permission denied"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("collapsed error receipt missing %q:\n%s", want, rendered)
		}
	}
}
