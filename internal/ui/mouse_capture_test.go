package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/command"
)

// Mouse reporting consumes press and release, which is exactly what stops a
// terminal from doing native drag-select. That is the ordinary copy gesture,
// so capture starts off. Wheel scrolling and click-to-expand are the opt-in.
func TestMouseCaptureStartsOffForNativeSelect(t *testing.T) {
	m := newTestModel(t)
	if !m.mouseCaptureOff {
		t.Fatal("mouse capture starts on; native select is the default affordance")
	}
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Fatalf("default MouseMode = %v, want none", got)
	}

	updated, _ := m.Update(altKey('m'))
	m = updated.(*Model)
	if m.mouseCaptureOff {
		t.Fatal("alt+m did not enable mouse capture")
	}
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("enabled MouseMode = %v, want cell motion", got)
	}

	updated, _ = m.Update(altKey('m'))
	m = updated.(*Model)
	if !m.mouseCaptureOff {
		t.Fatal("alt+m did not restore native select")
	}
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Fatalf("restored MouseMode = %v, want none", got)
	}
}

// Turning capture on kills native drag-select. Saying so is the difference
// between a deliberate trade and what reads as a broken copy.
func TestMouseToggleNoticeNamesWhatWasTradedAway(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(altKey('m'))
	m = updated.(*Model)

	notice := strings.ToLower(m.footerNotice.text)
	for _, want := range []string{"mouse capture on", "wheel", "alt+m", "/mouse", "select"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q does not mention %q", notice, want)
		}
	}
}

// Help must still name every copy path. Native drag is the default; shift+drag
// is the terminal override once capture is on; alt+m / /mouse is the toggle;
// ctrl+y does not need the mouse at all.
func TestHelpNamesTheRightSelectionOverride(t *testing.T) {
	m := newTestModel(t)
	content := strings.ToLower(m.buildHelpContent(m.helpContentWidth()))
	selectIdx := strings.Index(content, "select & copy")
	inputIdx := strings.Index(content, "input shortcuts")
	if selectIdx < 0 || inputIdx < 0 || selectIdx > inputIdx {
		t.Fatalf("Select & Copy must appear above Input Shortcuts:\n%s", content)
	}
	for _, want := range []string{"shift+drag", "iterm2", "alt+m", "/mouse", "ctrl+y", "default"} {
		if !strings.Contains(content, want) {
			t.Errorf("help omits %q", want)
		}
	}
}

func TestMouseSlashCommandTogglesCapture(t *testing.T) {
	m := newTestModel(t)
	cmd := m.handleCommandAction(command.Result{Action: command.ActionToggleMouseCapture})
	if cmd != nil {
		_ = cmd
	}
	if m.mouseCaptureOff {
		t.Fatal("/mouse action did not enable mouse capture")
	}
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("enabled MouseMode = %v, want cell motion", got)
	}
}

func TestOptionComposedMuNamesMouseSlash(t *testing.T) {
	m := newTestModel(t)
	notice := m.noticeForOptionComposedKey("µ")
	for _, want := range []string{"Option+M", "/mouse", "µ"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("µ notice %q omitted %q", notice, want)
		}
	}
}

func TestWheelModeStickyChrome(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(*Model)
	m.entries = []ChatEntry{
		{Kind: "user", Content: "hi"},
		{Kind: "assistant", Content: "hello there"},
	}
	m.state = StateIdle
	m.mouseCaptureOff = false
	status := strings.ToLower(ansi.Strip(m.renderStatusLine()))
	if !strings.Contains(status, "wheel") || !strings.Contains(status, "alt+m") {
		t.Fatalf("status missing sticky wheel chip: %q", status)
	}
	bar := strings.ToLower(ansi.Strip(m.renderShortcutsBar(m.chatPaneWidth())))
	if !strings.Contains(bar, "alt+m") || !strings.Contains(bar, "select") {
		t.Fatalf("shortcuts missing restore-select hint: %q", bar)
	}
	m.mouseCaptureOff = true
	bar = strings.ToLower(ansi.Strip(m.renderShortcutsBar(m.chatPaneWidth())))
	if !strings.Contains(bar, "ctrl+y") || !strings.Contains(bar, "copy") {
		t.Fatalf("idle with last answer should advertise ctrl+y copy: %q", bar)
	}
}
