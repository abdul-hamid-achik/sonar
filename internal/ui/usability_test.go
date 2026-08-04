package ui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestNarrowTerminalUsesFullWidthConversation(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)

	view := m.View()
	// Session chrome owns empty state on usable frames; no mid-canvas wall.
	for _, noise := range []string{"LOCAL AGENT", "Local-first"} {
		if strings.Contains(view.Content, noise) {
			t.Fatalf("40×20 view still paints redundant welcome %q:\n%s", noise, view.Content)
		}
	}
	assertRenderedLinesFit(t, view.Content, 40)
	if hint := m.narrowTerminalHint(); hint != "" {
		t.Fatalf("40-column terminal should be directly usable, got hint %q", hint)
	}
}

func TestCompactWelcomeFitsActualChatPane(t *testing.T) {
	// Minimum frames still get a short orientation surface.
	m := newTestModel(t)
	m.width = minTerminalWidth
	m.height = minTerminalHeight

	var b strings.Builder
	m.renderWelcome(&b)
	got := b.String()
	if strings.Contains(got, "╦") {
		t.Fatal("compact welcome should omit the wide ASCII logo")
	}
	for _, want := range []string{"LOCAL AGENT", "Local-first", "Ask · /help"} {
		if !strings.Contains(got, want) {
			t.Errorf("minimum welcome missing %q:\n%s", want, got)
		}
	}
	assertRenderedLinesFit(t, got, m.chatPaneWidth())
}

func TestWelcomeUsesHonestLocalFirstCopy(t *testing.T) {
	// Local-first copy is only painted when session chrome is off.
	m := newTestModel(t)
	m.width = minTerminalWidth
	m.height = minTerminalHeight

	var b strings.Builder
	m.renderWelcome(&b)
	got := b.String()
	if !strings.Contains(got, "Local-first") {
		t.Fatalf("welcome is missing the local-first boundary:\n%s", got)
	}
	for _, overclaim := range []string{"100% local", "Your data never leaves"} {
		if strings.Contains(got, overclaim) {
			t.Fatalf("welcome contains privacy overclaim %q:\n%s", overclaim, got)
		}
	}
}

func TestRoomyEmptyStateOmitsWelcomeWall(t *testing.T) {
	m := newTestModel(t)
	m.width = 100
	m.height = 30
	var b strings.Builder
	m.renderWelcome(&b)
	if got := strings.TrimSpace(ansi.Strip(b.String())); got != "" {
		t.Fatalf("roomy empty welcome should be empty, got:\n%s", got)
	}
}

func TestWindowTitleIdentifiesOnlySanitizedWorkspaceBasename(t *testing.T) {
	m := newTestModel(t)
	parent := t.TempDir()
	unsafeName := "project\n\x1b]0;owned\a\u202e"
	m.agent.SetWorkDir(filepath.Join(parent, unsafeName))
	m.state = StateWaiting

	title := m.View().WindowTitle
	for _, want := range []string{"LOCAL AGENT", "project", "thinking"} {
		if !strings.Contains(title, want) {
			t.Fatalf("window title omitted %q: %q", want, title)
		}
	}
	for _, forbidden := range []string{parent, "\n", "\x1b", "\a", "\u202e"} {
		if strings.Contains(title, forbidden) {
			t.Fatalf("window title leaked unsafe/full-path fragment %q: %q", forbidden, title)
		}
	}
}

func TestMinimumTerminalCompactsOnlyKnownOllamaStartupRecovery(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(*Model)
	raw := "ollama: no model selected\ntry: ollama serve · ollama pull qwen3.5:2b"
	m.ollamaOffline = true
	updated, _ = m.Update(ErrorMsg{Msg: raw})
	m = updated.(*Model)
	transcript := m.renderEntries()
	if plain := ansi.Strip(transcript); !strings.Contains(plain, "Ollama model") {
		t.Fatalf("compact transcript projection missing:\n%s", plain)
	}

	visible := ansi.Strip(m.View().Content)
	for _, want := range []string{"LOCAL AGENT", "Ollama model", "ctrl+o", "ctrl+p settings"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("minimum startup recovery omitted %q:\n%s", want, visible)
		}
	}
	if strings.Contains(visible, "ollama pull") {
		t.Fatalf("minimum startup recovery retained the fragmented long command:\n%s", visible)
	}
	if got := m.entries[0].Content; got != raw {
		t.Fatalf("compact projection mutated raw diagnostic: %q", got)
	}
	assertRenderedLinesFit(t, m.View().Content, minTerminalWidth)
	assertRenderedHeightFits(t, m.View().Content, minTerminalHeight)
}

func TestCompactOllamaRecoveryDoesNotRewriteArbitraryErrors(t *testing.T) {
	if rendered, ok := compactOllamaStartupNotice("provider failed: no model selected", 23, true); ok || rendered != "" {
		t.Fatalf("arbitrary error compacted as Ollama startup recovery: %q", rendered)
	}
}

func TestOllamaStartupRecoveryRemainsDetailedAtOrdinaryWidth(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 32})
	m = updated.(*Model)
	m.ollamaOffline = true
	raw := "ollama: no model selected\ntry: ollama serve · ollama pull qwen3.5:2b"
	updated, _ = m.Update(ErrorMsg{Msg: raw})
	m = updated.(*Model)

	plain := ansi.Strip(m.renderEntries())
	for _, want := range []string{"ollama: no model selected", "ollama serve", "ollama pull"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("ordinary startup recovery omitted %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "✗ error") {
		t.Fatalf("known startup recovery was promoted to a generic operation error:\n%s", plain)
	}
}

func TestNarrowStatusPrioritizesConnectionFailure(t *testing.T) {
	m := newTestModel(t)
	m.width = 40
	m.height = 20
	m.failedServers = []FailedServer{{Name: "local-tools", Reason: "connection refused"}}

	status := m.renderStatusLine()
	if !strings.Contains(status, "1 MCP server unavailable") {
		t.Fatalf("narrow status hid operational failure: %q", status)
	}
	assertRenderedLinesFit(t, status, m.chatPaneWidth())
}

func TestHelpOverlayFitsNarrowTerminal(t *testing.T) {
	m := newTestModel(t)
	m.width = 40
	m.height = 20
	m.initHelpViewport()

	overlay := m.renderHelpOverlay(m.width)
	assertRenderedLinesFit(t, overlay, m.width)
	if !strings.Contains(overlay, "Keyboard Shortcuts") {
		t.Fatalf("help content missing from overlay:\n%s", overlay)
	}
	if !strings.Contains(ansi.Strip(overlay), "esc/q close") {
		t.Fatalf("help footer hid the close affordance:\n%s", overlay)
	}
}

func TestToolCardFitsNarrowWidthAndUnicode(t *testing.T) {
	card := NewToolCard("write_文件_with_a_very_long_name", ToolCardFile, true, defaultThemeID)
	card.State = ToolCardSuccess
	card.Duration = 1250 * time.Millisecond
	card.Expanded = true
	card.Args = `{"path":"a/very/long/path"}`
	card.Result = "first very long result line\nsecond very long result line"

	for _, width := range []int{4, 12, 20} {
		t.Run(string(rune('a'+width)), func(t *testing.T) {
			assertRenderedLinesFit(t, card.View(width), width)
		})
	}
}

func TestRenderDiffAtWidthFitsPane(t *testing.T) {
	lines := []DiffLine{
		{Kind: DiffAdded, Content: "a very long added line that cannot fit"},
		{Kind: DiffRemoved, Content: "a very long removed line that cannot fit"},
		{Kind: DiffContext, Content: "context that cannot fit either"},
	}
	got := renderDiffAtWidth(lines, NewStyles(true, defaultThemeID), 10, 20)
	assertRenderedLinesFit(t, got, 20)
}

func TestRenderDiffStripsTerminalControlsAndPreservesIndentation(t *testing.T) {
	lines := []DiffLine{{Kind: DiffAdded, Content: "  value\x1b]0;owned\x07\u202espoof"}}
	rendered := renderDiffAtWidth(lines, NewStyles(true, defaultThemeID), 10, 40)
	for _, forbidden := range []string{"\x1b]", "\x07", "\u202e", "owned"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("diff retained terminal control payload %q: %q", forbidden, rendered)
		}
	}
	if !strings.Contains(ansi.Strip(rendered), "  value") || !strings.Contains(rendered, "spoof") {
		t.Fatalf("diff lost visible or indented content: %q", rendered)
	}
}

func TestAdaptiveStatusTextUsesReadableMutedColor(t *testing.T) {
	tests := []struct {
		name   string
		isDark bool
		want   string
	}{
		{name: "light", isDark: false, want: "#5B6779"},
		{name: "dark", isDark: true, want: "#96A2B8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adaptiveStyles(tt.isDark, defaultThemeID).StatusText.GetForeground()
			want := lipgloss.Color(tt.want)
			gr, gg, gb, ga := got.RGBA()
			wr, wg, wb, wa := want.RGBA()
			if gr != wr || gg != wg || gb != wb || ga != wa {
				t.Fatalf("status text color = %#v, want %s", got, tt.want)
			}
		})
	}
}

func TestTruncateDisplayPreservesUnicodeAndCellWidth(t *testing.T) {
	got := truncateDisplay("模型🙂abcdef", 7)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated value should use an ellipsis, got %q", got)
	}
	if lipgloss.Width(got) > 7 {
		t.Fatalf("display width = %d, want <= 7 (%q)", lipgloss.Width(got), got)
	}
}

func TestWrapTextHasNoBlankContinuationAndPreservesUnicode(t *testing.T) {
	got := wrapText("Toggle this help when input is empty", 18)
	if strings.Contains(got, "\n\n") {
		t.Fatalf("wrapped prose contains an empty continuation line: %q", got)
	}
	assertRenderedLinesFit(t, got, 18)

	unicode := wrapText("模型模型模型🙂abcdef", 6)
	if !utf8.ValidString(unicode) {
		t.Fatalf("wrapped Unicode is invalid UTF-8: %q", unicode)
	}
	assertRenderedLinesFit(t, unicode, 6)
}

func assertRenderedLinesFit(t *testing.T, rendered string, width int) {
	t.Helper()
	for i, line := range strings.Split(rendered, "\n") {
		if got := lipgloss.Width(line); got > width {
			t.Errorf("line %d width = %d, want <= %d: %q", i+1, got, width, line)
		}
	}
}
