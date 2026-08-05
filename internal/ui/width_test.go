package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestSinglePaneWidthTracksTerminal(t *testing.T) {
	for _, width := range []int{30, 40, 80, 120, 200} {
		t.Run(strings.Repeat("w", width/10), func(t *testing.T) {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			m = updated.(*Model)
			if got, want := m.chatPaneWidth(), width-1; got != want {
				t.Fatalf("chat width = %d, want %d", got, want)
			}
			if got, want := m.viewport.Width(), width-1; got != want {
				t.Fatalf("viewport width = %d, want %d", got, want)
			}
			m.entries = []ChatEntry{{Kind: "assistant", Content: strings.Repeat("longword ", 80)}}
			m.invalidateEntryCache()
			m.refreshTranscript()
			assertRenderedLinesFit(t, m.View().Content, width)
		})
	}
}

func TestWideTranscriptSeparatesReadableProseFromWorkWidth(t *testing.T) {
	const terminalWidth = 200
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: terminalWidth, Height: 30})
	m = updated.(*Model)

	if got, want := m.chatContentWidth(), terminalWidth-7; got != want {
		t.Fatalf("wide chat content width = %d, want %d", got, want)
	}
	if m.markdownWidth != m.chatContentWidth() || m.md == nil || m.md.width != m.chatContentWidth() {
		t.Fatalf("wide markdown renderer is stale: model=%d renderer=%v content=%d", m.markdownWidth, m.md, m.chatContentWidth())
	}
	m.entries = []ChatEntry{{
		Kind:    "assistant",
		Content: strings.Repeat("use the available transcript width ", 24),
	}}
	m.invalidateRenderedCache()
	rendered := ansi.Strip(m.renderEntries())

	usedWidth := 0
	for _, line := range strings.Split(rendered, "\n") {
		// Recap digests restate prose at the content-grid origin and are not
		// the readable-measure contract under test.
		if strings.Contains(line, "recap:") {
			continue
		}
		if strings.Contains(line, "available transcript width") {
			usedWidth = max(usedWidth, lipgloss.Width(line))
		}
	}
	// Prose follows the pane unless ui.prose_width says otherwise, so the
	// contract here is that it fills the pane without overflowing it — not
	// that it stops at a fixed measure. The overflow check below is the half
	// that always mattered.
	if usedWidth < 60 {
		t.Fatalf("wide assistant prose used only %d columns:\n%s", usedWidth, rendered)
	}
	if usedWidth > m.chatPaneWidth() {
		t.Fatalf("wide assistant prose overflowed pane: used=%d pane=%d", usedWidth, m.chatPaneWidth())
	}

	m.entries = []ChatEntry{{
		Kind:    "assistant",
		Content: "```text\n" + strings.Repeat("x", 120) + "\n```",
	}}
	m.invalidateRenderedCache()
	rendered = ansi.Strip(m.renderEntries())
	if used := widestDisplayLine(rendered); used <= ProseTargetCandidate || used > m.chatPaneWidth() {
		t.Fatalf("wide work block used %d columns; want (%d,%d]:\n%s",
			used, ProseTargetCandidate, m.chatPaneWidth(), rendered)
	}
}

func TestTrueMinimumWidthShowsResizeState(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth - 1, Height: 20})
	m = updated.(*Model)
	view := m.View().Content
	if !strings.Contains(view, "TERMINAL TOO NARROW") || !strings.Contains(view, "30 columns") || !strings.Contains(view, "Input paused") {
		t.Fatalf("minimum-width guidance missing:\n%s", view)
	}
	assertRenderedLinesFit(t, view, minTerminalWidth-1)
}

func TestUndersizedTerminalViewFitsEveryDeficientDimension(t *testing.T) {
	tests := []struct {
		name         string
		width        int
		height       int
		wantTitle    string
		wantGuidance string
	}{
		{name: "narrow", width: 29, height: 20, wantTitle: "TERMINAL TOO NARROW", wantGuidance: "30 columns"},
		{name: "short", width: 80, height: 11, wantTitle: "TERMINAL TOO SHORT", wantGuidance: "12 rows"},
		{name: "both", width: 20, height: 5, wantTitle: "TERMINAL TOO SMALL", wantGuidance: "30 columns"},
		{name: "single cell", width: 1, height: 1, wantTitle: "…"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: test.width, Height: test.height})
			m = updated.(*Model)
			view := m.View().Content
			if !strings.Contains(view, test.wantTitle) {
				t.Fatalf("undersized title missing %q:\n%s", test.wantTitle, view)
			}
			if test.wantGuidance != "" && !strings.Contains(view, test.wantGuidance) {
				t.Fatalf("undersized guidance missing %q:\n%s", test.wantGuidance, view)
			}
			if test.height > 1 && !strings.Contains(view, "ctrl+c") {
				t.Fatalf("undersized view hid graceful quit:\n%s", view)
			}
			assertRenderedLinesFit(t, view, test.width)
			assertRenderedHeightFits(t, view, test.height)
		})
	}
}
