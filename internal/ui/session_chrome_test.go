package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestSessionTopBarAppearsOnRoomyFrame(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*Model)
	frame := m.projectFrame()
	if !frame.Header.Visible || frame.Header.Content == "" {
		t.Fatalf("expected session top bar on roomy frame: %#v", frame.Header)
	}
	if frame.Header.Rect.Height() < 1 {
		t.Fatalf("header height = %d", frame.Header.Rect.Height())
	}
}

func TestSessionTopBarOwnsIdentityAndLeavesModeToTheShortcutsRow(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = updated.(*Model)
	m.model = "ornith:latest"
	m.setMode(ModeAuto)
	m.numCtx = 98_304
	m.promptTokens = 0
	bar := ansi.Strip(m.renderSessionTopBar(m.chatPaneWidth()))
	if !strings.Contains(bar, "ornith") {
		t.Fatalf("top bar missing the model it owns:\n%s", bar)
	}
	if !strings.Contains(bar, "0%") && !strings.Contains(bar, "0/") {
		t.Fatalf("top bar missing ambient context meter:\n%s", bar)
	}
	// Authority belongs beside shift+tab, not next to the workspace path.
	if strings.Contains(bar, "AUTO") {
		t.Fatalf("top bar re-printed mode owned by the shortcuts row:\n%s", bar)
	}
}

func TestSessionStickyUserRequiresConversation(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*Model)
	// Empty conversation: only top bar (1 row), no sticky user.
	if h := m.projectFrame().Header.Rect.Height(); h > 1 {
		t.Fatalf("empty conversation header height = %d, want <= 1", h)
	}
	m.entries = []ChatEntry{{Kind: "user", Content: "sticky prompt please keep me visible"}}
	m.settleChromeSpringForTest()
	m.recalcViewportHeight()
	frame := m.projectFrame()
	if frame.Header.Rect.Height() < 2 {
		t.Fatalf("expected sticky user row: header=%#v", frame.Header)
	}
	if !strings.Contains(ansi.Strip(frame.Header.Content), "sticky prompt") {
		t.Fatalf("sticky user missing prompt: %q", frame.Header.Content)
	}
}

func TestStickyUserStripIsFullWidthBand(t *testing.T) {
	// Sticky must be a full-pane band (Grok), not a partial "chip" highlight.
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*Model)
	m.entries = []ChatEntry{{Kind: "user", Content: "hello chat!"}}
	m.settleChromeSpringForTest()
	paneW := m.chatPaneWidth()
	bar := m.renderStickyUserStrip(paneW)
	if bar == "" {
		t.Fatal("expected sticky strip")
	}
	// Roomy frames: 3-row elevated band (vertical padding + prompt).
	if strings.Count(bar, "\n") != 2 {
		t.Fatalf("roomy sticky should be 3 rows (pad/prompt/pad), got:\n%s", ansi.Strip(bar))
	}
	for i, line := range strings.Split(bar, "\n") {
		if got := lipgloss.Width(line); got != paneW {
			t.Fatalf("sticky row %d width = %d, want full pane %d\n%s", i, got, paneW, ansi.Strip(line))
		}
	}
	if !strings.Contains(ansi.Strip(bar), "hello chat!") {
		t.Fatalf("sticky missing prompt: %q", ansi.Strip(bar))
	}
}

func TestStickyUserStripHasVerticalBreathingInHeader(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(*Model)
	m.entries = []ChatEntry{{Kind: "user", Content: "hello chat!"}}
	m.settleChromeSpringForTest()
	m.recalcViewportHeight()
	header := m.projectSessionHeader()
	// Roomy (H>=28): identity + 3-row elevated sticky (no outer blank-on-blank).
	if header.reservedHeight != 4 {
		t.Fatalf("header height = %d, want 4 (top + elevated sticky)\n%s",
			header.reservedHeight, ansi.Strip(header.content))
	}
	lines := strings.Split(ansi.Strip(header.content), "\n")
	if len(lines) != 4 {
		t.Fatalf("header lines = %d, want 4:\n%s", len(lines), ansi.Strip(header.content))
	}
	// No adjacent blank rows — elevated pad is the only breath.
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i-1]) == "" && strings.TrimSpace(lines[i]) == "" {
			t.Fatalf("blank-on-blank void at rows %d-%d:\n%s", i-1, i, ansi.Strip(header.content))
		}
	}
	plain := ansi.Strip(header.content)
	if !strings.Contains(plain, "hello chat!") {
		t.Fatalf("header missing sticky prompt:\n%s", plain)
	}

	// Ordinary elevated frames keep outer blanks so short readers still overflow.
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 22})
	m = updated.(*Model)
	m.entries = []ChatEntry{{Kind: "user", Content: "hello chat!"}}
	m.settleChromeSpringForTest()
	m.recalcViewportHeight()
	ordinary := m.projectSessionHeader()
	if ordinary.reservedHeight != 6 {
		t.Fatalf("ordinary elevated header height = %d, want 6 (top + blanks + band)\n%s",
			ordinary.reservedHeight, ansi.Strip(ordinary.content))
	}

	// Mid frames keep outer blanks around the single-row sticky.
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 18})
	m = updated.(*Model)
	m.entries = []ChatEntry{{Kind: "user", Content: "hello chat!"}}
	m.settleChromeSpringForTest()
	m.recalcViewportHeight()
	mid := m.projectSessionHeader()
	if mid.reservedHeight != 4 {
		t.Fatalf("mid header height = %d, want 4 (top + blank + sticky + blank)\n%s",
			mid.reservedHeight, ansi.Strip(mid.content))
	}
}

func TestShortcutsBarInFooter(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*Model)
	footer := ansi.Strip(m.projectFrame().Footer.Content)
	for _, want := range []string{"enter", "shift+tab"} {
		if !strings.Contains(strings.ToLower(footer), want) {
			t.Fatalf("footer shortcuts missing %q:\n%s", want, footer)
		}
	}
	// Idle with nothing to cancel must not advertise a dead "esc cancel".
	if strings.Contains(strings.ToLower(footer), "esc") {
		t.Fatalf("idle footer advertised esc with nothing to cancel:\n%s", footer)
	}
}

func TestShortcutsBarShowsEscWhenCancelDoesSomething(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*Model)
	m.queuedFollowUp = &queuedFollowUp{Prompt: "later"}
	footer := ansi.Strip(m.projectFrame().Footer.Content)
	if !strings.Contains(strings.ToLower(footer), "esc") {
		t.Fatalf("queued follow-up should advertise esc cancel:\n%s", footer)
	}
}

func TestMinimumTerminalSkipsSessionHeader(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(*Model)
	frame := m.projectFrame()
	if frame.Header.Visible && frame.Header.Rect.Height() > 0 {
		t.Fatalf("min terminal should omit session header: %#v", frame.Header)
	}
}

func TestSessionChromeAlignsToContentOrigin(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*Model)
	header := ansi.Strip(m.projectFrame().Header.Content)
	if header == "" {
		t.Fatal("expected header content")
	}
	// Top bar shares OriginX with transcript content (leading pad cells).
	firstLine := strings.Split(header, "\n")[0]
	if !strings.HasPrefix(firstLine, strings.Repeat(" ", contentLeftColumns)) {
		t.Fatalf("header should start at content OriginX=%d: %q", contentLeftColumns, firstLine)
	}
	// Empty welcome must not float mid-canvas: at most one blank row of pad.
	if got := emptyWelcomeTopPad(20, 4); got != 1 {
		t.Fatalf("emptyWelcomeTopPad(20,4)=%d, want 1", got)
	}
	if got := emptyWelcomeTopPad(4, 4); got != 0 {
		t.Fatalf("emptyWelcomeTopPad(4,4)=%d, want 0", got)
	}
	// Roomy empty frames: top bar + composer own orientation; no mid-canvas wall.
	m.model = "ornith:latest"
	var welcome strings.Builder
	m.renderWelcome(&welcome)
	plain := ansi.Strip(welcome.String())
	if strings.Contains(plain, "ornith:latest") {
		t.Fatalf("welcome re-printed model already on top bar:\n%s", plain)
	}
	if strings.Contains(plain, "SONAR") || strings.Contains(plain, "API-first") ||
		strings.Contains(plain, "Ask, @mention") || strings.Contains(plain, "Ask · /help") {
		t.Fatalf("roomy welcome should stay empty when chrome owns the frame:\n%s", plain)
	}
}

func TestEmptyStateFooterDoesNotDuplicateTopBar(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(*Model)
	m.model = "ornith:latest"
	m.promptTokens = 0
	m.numCtx = 98_304
	// Status line must stay quiet; composer meta may show the model under the
	// framed draft (Grok-style), which is not the ambient status strip.
	status := ansi.Strip(m.renderStatusLine())
	if status != "" {
		t.Fatalf("empty-state status should stay quiet, got %q", status)
	}
	footer := ansi.Strip(m.projectFrame().Footer.Content)
	if strings.Contains(footer, "0%") {
		t.Fatalf("empty footer re-printed ambient context meter:\n%s", footer)
	}
	if !strings.Contains(strings.ToLower(footer), "enter") {
		t.Fatalf("expected shortcuts bar in empty footer:\n%s", footer)
	}
}

func TestStickyUserOmitsDuplicateFromTranscript(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(*Model)
	// Live bug: omit must not wait for sticky spring settle. Dual-print
	// (sticky + body) is wrong from the first frame after send.
	m.reducedMotion = false
	m.entries = []ChatEntry{
		{Kind: "user", Content: "unique sticky prompt xyz"},
		{Kind: "system", Content: "ICE · recalled 1 past conversation"},
		{Kind: "assistant", Content: "Waiting reply"},
	}
	m.pullChromeSpringTargets()
	m.recalcViewportHeight()
	if !m.stickyUserActive() {
		t.Fatal("expected sticky user on roomy conversation frame")
	}
	header := ansi.Strip(m.projectFrame().Header.Content)
	if !strings.Contains(header, "unique sticky prompt xyz") {
		t.Fatalf("sticky missing prompt:\n%s", header)
	}
	// Body must not re-print the sticky-owned prompt.
	body := ansi.Strip(m.renderEntries())
	if strings.Contains(body, "unique sticky prompt xyz") {
		t.Fatalf("transcript re-printed sticky user prompt:\n%s", body)
	}
	if !strings.Contains(body, "ICE") || !strings.Contains(body, "Waiting reply") {
		t.Fatalf("transcript lost non-user content:\n%s", body)
	}
	// Older user turns remain visible when a newer sticky prompt exists.
	m.entries = []ChatEntry{
		{Kind: "user", Content: "first older prompt"},
		{Kind: "assistant", Content: "first reply"},
		{Kind: "user", Content: "second sticky prompt"},
	}
	m.entryCacheValid = false
	m.pullChromeSpringTargets()
	m.recalcViewportHeight()
	body = ansi.Strip(m.renderEntries())
	if !strings.Contains(body, "first older prompt") {
		t.Fatalf("older user turn was omitted:\n%s", body)
	}
	if strings.Contains(body, "second sticky prompt") {
		t.Fatalf("latest user still in body with sticky:\n%s", body)
	}
}

func TestRoomyStatusDoesNotDoublePlan(t *testing.T) {
	// Captured live: status "[ PLAN · read-only ]" + shortcuts "· PLAN".
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(*Model)
	m.entries = []ChatEntry{{Kind: "user", Content: "hey chat"}}
	m.mode = ModePlan
	m.model = "ornith:latest"
	status := ansi.Strip(m.renderStatusLine())
	if strings.Contains(status, "PLAN") {
		t.Fatalf("status should not re-print PLAN when shortcuts own mode: %q", status)
	}
	bar := ansi.Strip(m.renderShortcutsBar(m.chatPaneWidth()))
	if !strings.Contains(bar, "PLAN") {
		t.Fatalf("shortcuts should own mode: %q", bar)
	}
	if strings.Contains(status, "ornith") || strings.Contains(bar, "ornith") {
		t.Fatalf("model belongs to the top bar, not status/shortcuts: status=%q bar=%q", status, bar)
	}
}

func TestBusyShortcutsDoNotDuplicateActivityControls(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(*Model)
	m.state = StateWaiting
	m.turnStartedAt = m.nowTime()
	footer := strings.ToLower(ansi.Strip(m.projectFrame().Footer.Content))
	// Activity rail owns esc stop / enter queue; shortcuts keep mode only.
	if strings.Count(footer, "esc") > 1 {
		t.Fatalf("esc stop duplicated across activity+shortcuts:\n%s", footer)
	}
	if !strings.Contains(footer, "shift+tab") {
		t.Fatalf("busy shortcuts should keep mode cycle:\n%s", footer)
	}
}

func TestTranscriptSeparatorsUseConsistentMessagePadding(t *testing.T) {
	// Grok rhythm: one blank row between messages.
	if got := transcriptEntrySeparator("user", "system"); got != "\n\n" {
		t.Fatalf("user→system separator = %q, want blank row", got)
	}
	if got := transcriptEntrySeparator("system", "assistant"); got != "\n\n" {
		t.Fatalf("system→assistant separator = %q, want blank row", got)
	}
	if got := transcriptEntrySeparator("assistant", "user"); got != "\n\n" {
		t.Fatalf("assistant→user separator = %q, want blank row", got)
	}
	// Tool cards stay dense.
	if got := transcriptEntrySeparator("tool_group", "tool_group"); got != "\n" {
		t.Fatalf("tool→tool separator = %q, want dense stack", got)
	}
	if got := transcriptEntrySeparator("assistant", "tool_group"); got != "\n" {
		t.Fatalf("assistant→tool separator = %q, want dense same-turn stack", got)
	}
}
