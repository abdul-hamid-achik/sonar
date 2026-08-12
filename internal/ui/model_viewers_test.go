package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

func installOutputViewerTool(t *testing.T, m *Model, source string) OutputDetailReceipt {
	t.Helper()
	receipt, err := m.outputDetails.Admit(source)
	if err != nil {
		t.Fatal(err)
	}
	m.toolEntries = []ToolEntry{{
		ID: "call-output-1", Name: "read_file", Summary: "result.txt",
		Result: boundedToolCardResult(source), Status: ToolStatusDone,
		Collapsed: true, OutputDetail: receipt,
	}}
	m.entries = []ChatEntry{{Kind: "user", Content: "inspect output"}, testToolChatEntry(0)}
	m.lastTurnToolIndex = 0
	m.invalidateEntryCache()
	m.refreshTranscript()
	return receipt
}

func installDiffViewerTool(m *Model) {
	m.toolEntries = []ToolEntry{{
		ID: "call-diff-1", Name: "write_file", Summary: "internal/ui/example.go",
		Status: ToolStatusDone, Collapsed: true,
		DiffLines: []DiffLine{
			{Kind: DiffHunkHeader, Hunk: &DiffHunk{OldStart: 10, OldCount: 1, NewStart: 10, NewCount: 2}},
			{Kind: DiffRemoved, Content: "before", OldLine: 10},
			{Kind: DiffAdded, Content: "after", NewLine: 10},
			{Kind: DiffAdded, Content: "extra", NewLine: 11},
		},
	}}
	m.entries = []ChatEntry{{Kind: "user", Content: "inspect diff"}, testToolChatEntry(0)}
	m.lastTurnToolIndex = 0
	m.invalidateEntryCache()
	m.refreshTranscript()
}

func openOutputViewerForTest(t *testing.T, m *Model) (*Model, outputViewerPageResultMsg) {
	t.Helper()
	updated, _ := m.Update(ctrlKey('t'))
	m = updated.(*Model)
	if !m.receiptInspectActive || m.toolEntries[0].Collapsed {
		t.Fatal("Ctrl+T did not activate the typed receipt action")
	}
	updated, command := m.Update(altKey('o'))
	m = updated.(*Model)
	if !m.viewerModalActive() || m.modalStack.Len() != 1 || m.input.Focused() {
		t.Fatalf("output modal did not own focus: depth=%d focused=%t", m.modalStack.Len(), m.input.Focused())
	}
	result := awaitCommandMessage[outputViewerPageResultMsg](
		t, commandMessages(command), time.Second,
	)
	return m, result
}

func TestOutputViewerModelFlowIsPagedModalAndCopyIsTransient(t *testing.T) {
	m := newTestModel(t)
	source := strings.Repeat("line\n", 300) + "tail"
	installOutputViewerTool(t, m, source)

	m, pageResult := openOutputViewerForTest(t, m)
	updated, _ := m.Update(pageResult)
	m = updated.(*Model)
	top, _ := m.modalStack.Top()
	viewer := m.outputViewers[top.ID]
	if viewer == nil || viewer.Status() != OutputViewerReady ||
		viewer.CachedRowCount() == 0 || viewer.CachedRowCount() > maxOutputDetailPageRows {
		t.Fatalf("page was not admitted to the active viewer: %#v", viewer)
	}
	beforeEntries := len(m.entries)
	var copied string
	m.clipboardWrite = func(value string) error {
		copied = value
		return nil
	}
	updated, command := m.Update(charKey('c'))
	m = updated.(*Model)
	copyResult := awaitCommandMessage[viewerClipboardResultMsg](
		t, commandMessages(command), time.Second,
	)
	updated, _ = m.Update(copyResult)
	m = updated.(*Model)
	if copied == "" || len(m.entries) != beforeEntries {
		t.Fatalf("viewer copy was empty or polluted transcript: copied=%d entries=%d/%d", len(copied), len(m.entries), beforeEntries)
	}
	if viewer.notice != "Copied visible content." {
		t.Fatalf("copy receipt = %q", viewer.notice)
	}

	updated, _ = m.Update(escKey())
	m = updated.(*Model)
	if m.viewerModalActive() || !m.input.Focused() {
		t.Fatalf("Escape did not restore composer focus: modal=%t focused=%t", m.viewerModalActive(), m.input.Focused())
	}
}

func TestOutputViewerRejectsStalePageAfterClose(t *testing.T) {
	m := newTestModel(t)
	installOutputViewerTool(t, m, "alpha\nbeta")
	m, stale := openOutputViewerForTest(t, m)

	updated, _ := m.Update(escKey())
	m = updated.(*Model)
	updated, _ = m.Update(stale)
	m = updated.(*Model)
	if m.viewerModalActive() || len(m.outputViewers) != 0 {
		t.Fatalf("stale page revived a closed viewer: stack=%d viewers=%d", m.modalStack.Len(), len(m.outputViewers))
	}
}

func TestViewerGeometryUsesOneRectAcrossSupportedScreens(t *testing.T) {
	for _, size := range []struct{ width, height int }{{30, 12}, {80, 24}, {160, 48}} {
		t.Run(strings.Join([]string{formatTokens(size.width), "x", formatTokens(size.height)}, ""), func(t *testing.T) {
			m := newTestModel(t)
			installOutputViewerTool(t, m, "alpha\nbeta\ngamma")
			updated, _ := m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			m = updated.(*Model)
			m, page := openOutputViewerForTest(t, m)
			updated, _ = m.Update(page)
			m = updated.(*Model)

			top, _ := m.modalStack.Top()
			layout := m.outputViewers[top.ID].Layout()
			if layout.OuterRect.Empty() || !layout.ScreenRect.Contains(layout.OuterRect.MinX, layout.OuterRect.MinY) ||
				layout.OuterRect.MaxX > size.width || layout.OuterRect.MaxY > size.height {
				t.Fatalf("invalid modal geometry: %#v", layout)
			}
			rows := strings.Split(strings.TrimSuffix(m.View().Content, "\n"), "\n")
			if len(rows) != size.height {
				t.Fatalf("painted rows = %d, want %d", len(rows), size.height)
			}
			for row, value := range rows {
				if width := ansi.StringWidth(value); width != size.width {
					t.Fatalf("row %d width = %d, want %d: %q", row, width, size.width, ansi.Strip(value))
				}
			}
		})
	}
}

func TestDiffViewerModelFlowAndTypedCopy(t *testing.T) {
	m := newTestModel(t)
	installDiffViewerTool(m)
	updated, _ := m.Update(ctrlKey('t'))
	m = updated.(*Model)
	updated, _ = m.Update(altKey('d'))
	m = updated.(*Model)
	top, ok := m.modalStack.Top()
	if !ok || top.Kind != ModalKindDiffViewer || m.diffViewers[top.ID] == nil {
		t.Fatalf("diff viewer did not open: %#v", top)
	}
	if plain := ansi.Strip(m.View().Content); !strings.Contains(plain, "internal/ui/example.go") ||
		!strings.Contains(plain, "before") || !strings.Contains(plain, "after") {
		t.Fatalf("diff modal omitted its file/body:\n%s", plain)
	}

	var copied string
	m.clipboardWrite = func(value string) error {
		copied = value
		return nil
	}
	updated, command := m.Update(charKey('c'))
	m = updated.(*Model)
	copyResult := awaitCommandMessage[viewerClipboardResultMsg](
		t, commandMessages(command), time.Second,
	)
	updated, _ = m.Update(copyResult)
	m = updated.(*Model)
	if copied == "" || !strings.Contains(copied, "@@") {
		t.Fatalf("typed diff copy = %q", copied)
	}
	if notice := m.diffViewers[top.ID].notice; notice != "Copied visible content." {
		t.Fatalf("diff copy receipt = %q", notice)
	}
}

func TestViewerPointerRoutesToTopModalWithoutTranscriptLeak(t *testing.T) {
	m := newTestModel(t)
	installDiffViewerTool(m)
	updated, _ := m.Update(ctrlKey('t'))
	m = updated.(*Model)
	updated, _ = m.Update(altKey('d'))
	m = updated.(*Model)
	top, ok := m.modalStack.Top()
	if !ok {
		t.Fatal("diff modal did not open")
	}
	viewer := m.diffViewers[top.ID]
	if viewer == nil || len(viewer.rows) < 2 {
		t.Fatal("diff modal has no selectable second row")
	}
	beforeCollapsed := m.toolEntries[0].Collapsed
	layout := viewer.Layout()
	updated, _ = m.Update(tea.MouseClickMsg{
		X:      layout.BodyRect.MinX,
		Y:      layout.BodyRect.MinY + 1,
		Button: tea.MouseLeft,
	})
	m = updated.(*Model)
	if got := viewer.currentRowIndex(); got != 1 {
		t.Fatalf("modal body click selected physical row %d, want 1", got)
	}
	if m.toolEntries[0].Collapsed != beforeCollapsed || !m.viewerModalActive() {
		t.Fatal("modal click leaked to the transcript or closed its owner")
	}
}

func TestExpandedReceiptDoesNotStealComposerFirstCharacter(t *testing.T) {
	for _, first := range []rune{'d', 'o'} {
		t.Run(string(first), func(t *testing.T) {
			m := newTestModel(t)
			installDiffViewerTool(m)

			// Batch expansion deliberately leaves the receipt visible without
			// giving it exclusive keyboard ownership. Plain letters must still
			// start a composer draft.
			updated, _ := m.Update(altKey('t'))
			m = updated.(*Model)
			if m.toolEntries[0].Collapsed || m.receiptInspectActive {
				t.Fatalf("precondition: expanded=%v inspected=%v",
					!m.toolEntries[0].Collapsed, m.receiptInspectActive)
			}

			draft := string(first) + "raft remains composer-owned"
			for _, character := range draft {
				updated, _ = m.Update(charKey(character))
				m = updated.(*Model)
			}
			if got := m.input.Value(); got != draft {
				t.Fatalf("draft = %q, want %q", got, draft)
			}
			if m.viewerModalActive() {
				t.Fatal("plain composer input opened a receipt viewer")
			}
		})
	}
}

func TestToolToggleMouseAndKeyboardUseSameActionSemantics(t *testing.T) {
	keyboard := newTestModel(t)
	installOutputViewerTool(t, keyboard, "output")
	pointer := newTestModel(t)
	installOutputViewerTool(t, pointer, "output")

	target, ok := keyboard.toolActionTarget(0)
	if !ok {
		t.Fatal("keyboard ToolCard has no typed action target")
	}
	registry, _, _, ok := keyboard.toolActionRegistry(target)
	if !ok {
		t.Fatal("keyboard ToolCard has no action registry")
	}
	toggle, ok := registry.Action(toolToggleActionID)
	if !ok || toggle.Shortcut.Help().Key != "ctrl+t" {
		t.Fatalf("toggle shortcut = %#v, want ctrl+t", toggle.Shortcut.Help())
	}

	updated, _ := keyboard.Update(ctrlKey('t'))
	keyboard = updated.(*Model)
	region := pointer.toolHitRegions[0]
	updated, _ = pointer.Update(tea.MouseClickMsg{
		X: region.StartCol, Y: region.Row - pointer.viewport.YOffset(), Button: tea.MouseLeft,
	})
	pointer = updated.(*Model)

	if keyboard.toolEntries[0].Collapsed != pointer.toolEntries[0].Collapsed ||
		keyboard.receiptInspectActive != pointer.receiptInspectActive ||
		keyboard.receiptInspectToolIndex != pointer.receiptInspectToolIndex {
		t.Fatalf("input paths diverged: keyboard=%#v pointer=%#v",
			keyboard.toolEntries[0], pointer.toolEntries[0])
	}
}

func TestToolViewerActionHintResolvesFromRenderedChatInConstantWork(t *testing.T) {
	m := newTestModel(t)
	installDiffViewerTool(m)
	chat := m.entries[len(m.entries)-1]

	// Rendering already owns this exact ChatEntry. Removing the global
	// transcript proves hint projection does not scan or resolve through
	// m.entries; mutating dispatch retains that stricter lookup.
	m.entries = nil
	hint := m.toolViewerActionHint(chat)
	if !strings.Contains(hint, "alt+d Open diff") {
		t.Fatalf("render-owned ToolCard hint = %q, want diff action", hint)
	}

	invalid := chat
	invalid.BlockID = "invalid block id"
	if hint := m.toolViewerActionHint(invalid); hint != "" {
		t.Fatalf("invalid render-owned identity produced hint %q", hint)
	}
}

func BenchmarkToolViewerActionHintConstantHistoryCost(b *testing.B) {
	for _, historyEntries := range []int{1, 10_000} {
		b.Run(fmt.Sprintf("history_%d", historyEntries), func(b *testing.B) {
			m := newTestModel(b)
			installDiffViewerTool(m)
			chat := m.entries[len(m.entries)-1]
			m.entries = make([]ChatEntry, historyEntries, historyEntries+1)
			for index := range m.entries {
				m.entries[index] = ChatEntry{
					BlockID:   BlockID(fmt.Sprintf("history_%05d", index)),
					TurnID:    TurnID(fmt.Sprintf("turn_history_%05d", index)),
					Revision:  1,
					Lifecycle: BlockSettled,
					Kind:      "user",
					Content:   "history",
				}
			}
			m.entries = append(m.entries, chat)

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if hint := m.toolViewerActionHint(chat); hint == "" {
					b.Fatal("ToolCard action hint unexpectedly empty")
				}
			}
		})
	}
}

func TestApprovalPreemptsAndDestroysViewerState(t *testing.T) {
	m := newTestModel(t)
	installOutputViewerTool(t, m, "output")
	m, _ = openOutputViewerForTest(t, m)
	responses := make(chan permission.ApprovalResponse, 1)

	updated, _ := m.Update(ToolApprovalMsg{ToolName: "bash", Response: responses})
	m = updated.(*Model)
	if m.viewerModalActive() || len(m.outputViewers) != 0 || m.pendingApproval == nil {
		t.Fatalf("approval did not preempt viewer: stack=%d viewers=%d approval=%t",
			m.modalStack.Len(), len(m.outputViewers), m.pendingApproval != nil)
	}
	updated, _ = m.Update(escKey())
	_ = updated.(*Model)
}
