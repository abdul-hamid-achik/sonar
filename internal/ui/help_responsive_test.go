package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestHelpUsesSharedFrameAtSupportedSizes(t *testing.T) {
	for _, size := range []struct {
		name          string
		width, height int
	}{
		{name: "minimum", width: 30, height: 12},
		{name: "narrow", width: 40, height: 20},
		{name: "normal", width: 80, height: 24},
	} {
		t.Run(size.name, func(t *testing.T) {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			m = updated.(*Model)
			m.overlay = OverlayHelp
			m.initHelpViewport()

			rendered := m.renderHelpOverlay(m.width)
			assertRenderedLinesFit(t, rendered, size.width)
			assertRenderedHeightFits(t, rendered, size.height)
		})
	}
}

func TestHelpFooterAdvertisesPageAndEndpointNavigation(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = 80, 24
	m.overlay = OverlayHelp
	m.initHelpViewport()
	footer := ansi.Strip(m.renderHelpOverlay(m.width))
	for _, want := range []string{"pgdn more", "g/shift+g ends"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("help footer omitted %q:\n%s", want, footer)
		}
	}
}

func TestHelpMinimumFooterKeepsClosePageAndEndpointNavigation(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = 30, 12
	m.overlay = OverlayHelp
	m.initHelpViewport()
	footer := ansi.Strip(m.renderHelpOverlay(m.width))
	for _, want := range []string{"esc/q close", "pgdn more", "g/⇧g ends"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("minimum help footer omitted %q:\n%s", want, footer)
		}
	}
	assertRenderedLinesFit(t, footer, 30)
	assertRenderedHeightFits(t, footer, 12)
}

func TestHelpKeepsKeysAndContextTruthfulAtNarrowWidth(t *testing.T) {
	for _, width := range []int{30, 34, 36, 39, 40} {
		m := newTestModel(t)
		updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 20})
		m = updated.(*Model)
		plain := ansi.Strip(m.buildHelpContent(m.helpContentWidth()))
		compact := strings.Join(strings.Fields(plain), " ")
		if !strings.Contains(compact, "shift+enter New line · ctrl+j/alt+enter also") || strings.Contains(plain, "shift+ent…") {
			t.Fatalf("width %d help omitted or truncated newline fallbacks:\n%s", width, plain)
		}
		if !strings.Contains(compact, "shift+drag") || !strings.Contains(compact, "terminal selection override") {
			t.Fatalf("width %d help omitted mouse selection guidance:\n%s", width, plain)
		}
	}

	m := newTestModel(t)
	foundContext := false
	for _, row := range m.keyHelpRows() {
		if row.key == "f1" && strings.Contains(strings.ToLower(row.desc), "empty input") {
			foundContext = true
			break
		}
	}
	if !foundContext {
		t.Fatal("help does not disclose the empty-input requirement")
	}
}

func TestHelpExplainsOneSlotFollowUpLifecycle(t *testing.T) {
	m := newTestModel(t)
	content := strings.Join(strings.Fields(strings.ToLower(ansi.Strip(m.buildHelpContent(m.helpContentWidth())))), " ")
	for _, want := range []string{
		"send / queue one follow-up",
		"enter (running)",
		"after the current turn settles successfully",
		"esc (running)",
		"clear a queued follow-up first",
		"press again to cancel",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("help omitted %q:\n%s", want, content)
		}
	}
}

func TestHelpDescribesMentionsAsComposerInsertions(t *testing.T) {
	m := newTestModel(t)
	content := strings.Join(strings.Fields(strings.ToLower(ansi.Strip(m.buildHelpContent(m.helpContentWidth())))), " ")
	for _, want := range []string{
		"@file / @agent insert file or agent mention text",
		"#skill insert skill mention text",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("help omitted truthful insertion contract %q:\n%s", want, content)
		}
	}
	for _, stale := range []string{"attach file or agent", "activate skill"} {
		if strings.Contains(content, stale) {
			t.Fatalf("help still claims unsupported action %q:\n%s", stale, content)
		}
	}
}

func TestHelpDescribesClipboardImageConversionAndFileFormats(t *testing.T) {
	m := newTestModel(t)
	content := strings.Join(strings.Fields(strings.ToLower(ansi.Strip(m.buildHelpContent(m.helpContentWidth())))), " ")
	for _, want := range []string{
		"on macos convert a clipboard image to png",
		"attach up to four png, jpeg, or gif files",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("help omitted truthful image contract %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "clipboard png, jpeg, or gif") {
		t.Fatalf("help still claims the clipboard preserves its source format:\n%s", content)
	}
}

func TestHelpDescribesExplicitExternalPathReviewBoundary(t *testing.T) {
	m := newTestModel(t)
	content := strings.Join(strings.Fields(strings.ToLower(ansi.Strip(m.buildHelpContent(m.helpContentWidth())))), " ")
	for _, want := range []string{
		"~/… or /…",
		"temporary read-only access",
		"mcp tools require separate approval",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("help omitted external-path boundary %q:\n%s", want, content)
		}
	}
}

func TestHelpPreservesScrollAcrossResize(t *testing.T) {
	m := newTestModel(t)
	m.overlay = OverlayHelp
	m.initHelpViewport()
	m.helpViewport.GotoBottom()
	if m.helpViewport.YOffset() == 0 {
		t.Fatal("help fixture is not scrollable")
	}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 18})
	m = updated.(*Model)
	if m.helpViewport.YOffset() == 0 {
		t.Fatal("help resize discarded the scroll position")
	}
	assertRenderedLinesFit(t, m.renderHelpOverlay(m.width), 60)
	assertRenderedHeightFits(t, m.renderHelpOverlay(m.width), 18)
}

func TestHelpGoalActionsShareStateAwareRegistryMetadata(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = 80, 24
	content := ansi.Strip(m.buildHelpContent(m.helpContentWidth()))
	for _, action := range []string{"/goal new", "/goal show", "/goal pause", "/goal resume", "/goal budget", "/goal drop"} {
		if !strings.Contains(content, action) {
			t.Fatalf("help omitted %q:\n%s", action, content)
		}
	}
	if !strings.Contains(content, "Unavailable") || !strings.Contains(content, "No goal is configured") {
		t.Fatalf("help omitted goal action availability:\n%s", content)
	}
}
