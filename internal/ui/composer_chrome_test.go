package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The draft is unframed at every size. The transcript rule above it and the
// shortcuts row below already bound it; a box added a second rectangle and
// cost two rows on every frame.
func TestComposerIsUnframedAtEverySize(t *testing.T) {
	for _, size := range []struct {
		name          string
		width, height int
	}{
		{"minimum", minTerminalWidth, minTerminalHeight},
		{"standard", 80, 24},
		{"wide", 120, 36},
	} {
		m := newTestModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		m = updated.(*Model)

		view, _ := m.renderComposerChrome()
		plain := ansi.Strip(view)
		for _, border := range []string{"╭", "╮", "╰", "╯"} {
			if strings.Contains(plain, border) {
				t.Fatalf("%s: composer is still framed:\n%s", size.name, plain)
			}
		}
	}
}

// The textarea's own prompt must land on the shared content grid: accent in
// column 1, text in column 4, the same as tool receipts and notices.
func TestComposerPromptSitsOnTheContentGrid(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*Model)

	view, _ := m.renderComposerChrome()
	first := ansi.Strip(strings.SplitN(view, "\n", 2)[0])
	at := strings.Index(first, "❯")
	if at < 0 {
		t.Fatalf("composer has no prompt:\n%q", first)
	}
	// Display columns, not byte offsets: the rail glyph ahead of the prompt is
	// multi-byte. Grid origin is accent(1) + pad(2), and the composer spends
	// that pad on "❯ ", so text starts in column 4 like every other surface.
	if col := lipgloss.Width(first[:at]) + 1; col != 2 {
		t.Fatalf("composer prompt in column %d, want 2:\n%q", col, first)
	}
	if col := lipgloss.Width(first[:at]) + lipgloss.Width("❯ ") + 1; col != contentLeftColumns+1 {
		t.Fatalf("composer text starts in column %d, want %d:\n%q", col, contentLeftColumns+1, first)
	}
}

func TestFooterIdentityLivesOnShortcutsRow(t *testing.T) {
	// Authority sits on the bottom shortcuts row, beside the key that changes
	// it, and never under the composer box as a second meta line. Identity
	// (model · context) lives in the top bar — see planStatus.
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = updated.(*Model)
	m.model = "ornith:latest"
	m.setMode(ModePlan)
	bar := ansi.Strip(m.renderShortcutsBar(m.chatPaneWidth()))
	if !strings.Contains(bar, "PLAN") {
		t.Fatalf("shortcuts bar missing mode:\n%s", bar)
	}
	if strings.Contains(bar, "ornith") {
		t.Fatalf("shortcuts bar re-printed model owned by the top bar:\n%s", bar)
	}
	if top := ansi.Strip(m.renderSessionTopBar(m.chatPaneWidth())); !strings.Contains(top, "ornith") {
		t.Fatalf("top bar missing the model it owns:\n%s", top)
	}
	// The composer carries the draft and nothing else — no identity meta row.
	chrome, _ := m.renderComposerChrome()
	if plain := ansi.Strip(chrome); strings.Contains(plain, "ornith") {
		t.Fatalf("model leaked into the composer surface:\n%s", plain)
	}
}

func TestActivityOmitsStickyEcho(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = updated.(*Model)
	m.entries = []ChatEntry{{Kind: "user", Content: "hey chat unique prompt"}}
	m.activeSessionTitle = "hey chat unique prompt"
	m.sessionPublicID = "abcdef1"
	m.state = StateStreaming
	m.reducedMotion = true
	m.turnStartedAt = m.nowTime()
	if !m.stickyUserActive() {
		t.Fatal("expected sticky user for activity de-dupe")
	}
	line := ansi.Strip(m.renderWorkingLine())
	if strings.Contains(line, "hey chat unique prompt") {
		t.Fatalf("activity re-echoed sticky/title prompt:\n%s", line)
	}
}
