package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"
)

// Mouse reporting consumes press and release, which is exactly what stops a
// terminal from doing native drag-select. The harness needs the mouse only for
// wheel scrolling and a few click affordances that all have keyboard
// equivalents, so the trade must be available to the user rather than assumed.
//
// The documented alternative — a terminal-side modifier that withholds mouse
// events — is not uniform: Shift in Ghostty/kitty/WezTerm/Alacritty/xterm,
// Option in iTerm2, and nothing at all in Terminal.app. The help overlay named
// Shift for every terminal, so for some users the only documented escape hatch
// simply did not work. This toggle is the one that always does.
func TestMouseCaptureTogglesOffAndBack(t *testing.T) {
	m := newTestModel(t)
	if m.mouseCaptureOff {
		t.Fatal("mouse capture starts off; wheel scrolling is the default affordance")
	}

	updated, _ := m.Update(altKey('m'))
	m = updated.(*Model)
	if !m.mouseCaptureOff {
		t.Fatal("alt+m did not release the mouse")
	}

	updated, _ = m.Update(altKey('m'))
	m = updated.(*Model)
	if m.mouseCaptureOff {
		t.Fatal("alt+m did not restore capture")
	}
}

// The view is what actually declares the mode to the terminal. Bubble Tea
// diffs MouseMode between frames and emits the reset sequence itself, so the
// state has to reach the view or the toggle is cosmetic.
func TestViewDeclaresTheCurrentMouseMode(t *testing.T) {
	m := newTestModel(t)
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("default MouseMode = %v, want cell motion", got)
	}
	m.mouseCaptureOff = true
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Fatalf("released MouseMode = %v, want none", got)
	}
}

// Turning capture off kills the wheel. Saying so is the difference between a
// deliberate trade and what reads as a broken scroll wheel.
func TestMouseToggleNoticeNamesWhatWasTradedAway(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(altKey('m'))
	m = updated.(*Model)

	notice := strings.ToLower(m.footerNotice.text)
	for _, want := range []string{"mouse capture off", "select", "pgup", "alt+m"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q does not mention %q", notice, want)
		}
	}
}

// Help must not promise Shift to terminals that do not honour it. Naming the
// terminals, and naming the toggle for the ones with no override at all, is
// the whole point of the correction.
func TestHelpNamesTheRightSelectionOverride(t *testing.T) {
	m := newTestModel(t)
	content := strings.ToLower(m.buildHelpContent(m.helpContentWidth()))
	// shift+drag is the terminal's own override and iTerm2's differs; alt+m is
	// the harness fallback for terminals that offer no override at all.
	for _, want := range []string{"shift+drag", "iterm2", "alt+m"} {
		if !strings.Contains(content, want) {
			t.Errorf("help omits %q", want)
		}
	}
}


func TestSelectModeStickyChrome(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(*Model)
	m.entries = []ChatEntry{
		{Kind: "user", Content: "hi"},
		{Kind: "assistant", Content: "hello there"},
	}
	m.state = StateIdle
	m.mouseCaptureOff = true
	status := strings.ToLower(ansi.Strip(m.renderStatusLine()))
	if !strings.Contains(status, "select") || !strings.Contains(status, "alt+m") {
		t.Fatalf("status missing sticky select chip: %q", status)
	}
	bar := strings.ToLower(ansi.Strip(m.renderShortcutsBar(m.chatPaneWidth())))
	if !strings.Contains(bar, "alt+m") || !strings.Contains(bar, "select") {
		t.Fatalf("shortcuts missing exit-select hint: %q", bar)
	}
	m.mouseCaptureOff = false
	bar = strings.ToLower(ansi.Strip(m.renderShortcutsBar(m.chatPaneWidth())))
	if !strings.Contains(bar, "ctrl+y") || !strings.Contains(bar, "copy") {
		t.Fatalf("idle with last answer should advertise ctrl+y copy: %q", bar)
	}
}
