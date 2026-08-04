package ui

import (
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
)

// commandMessages executes a Bubble Tea command tree the same way the runtime
// dispatches tea.Batch children. Tests can then assert on the owned effect
// receipt without depending on whether presentation clocks are batched beside
// it.
func commandMessages(cmd tea.Cmd) <-chan tea.Msg {
	messages := make(chan tea.Msg, 16)
	var run func(tea.Cmd)
	run = func(next tea.Cmd) {
		if next == nil {
			return
		}
		go func() {
			msg := next()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, child := range batch {
					run(child)
				}
				return
			}
			messages <- msg
		}()
	}
	run(cmd)
	return messages
}

func awaitCommandMessage[T tea.Msg](t *testing.T, messages <-chan tea.Msg, timeout time.Duration) T {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case msg := <-messages:
			if typed, ok := msg.(T); ok {
				return typed
			}
		case <-deadline.C:
			var zero T
			t.Fatalf("timed out waiting for command message %T", zero)
		}
	}
}

// Test helpers for time values
var (
	testTime     = time.Now()
	testDuration = 100 * time.Millisecond
)

// newTestModel creates a Model with a real command.Registry and Agent, sends WindowSizeMsg to set ready=true.
// Sets initializing=false so tests exercise normal (post-startup) behavior.
func newTestModel(t testing.TB) *Model {
	t.Helper()
	reg := command.NewRegistry()
	command.RegisterBuiltins(reg)
	completer := NewCompleter(reg, []string{"model-a", "model-b"}, []string{"skill-a"}, []string{"agent-x"}, nil)
	ag := agent.New(nil, nil, 0)
	m := New(ag, reg, nil, completer, nil, nil, nil)
	// Historical fixtures assert the canonical Unicode presentation regardless
	// of the shell running `go test` (CI commonly exports TERM=dumb). Dedicated
	// glyph-profile tests construct Model through New without this override.
	m.glyphProfile = GlyphUnicode
	m.spin.Spinner = spinner.MiniDot
	m.syncComposerAuthority()
	m.initializing = false // skip startup phase for tests
	m.turnReady = true
	// Send WindowSizeMsg to set ready=true
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return updated.(*Model)
}

// setTestTranscriptContent installs a synthetic complete Bubbles document for
// component-level tests that do not exercise semantic transcript rendering.
// Production transcript paths always use refreshTranscript.
func (m *Model) setTestTranscriptContent(content string) {
	m.transcriptPaint.active = false
	m.viewport.SetContent(content)
}

// Key helpers — construct tea.KeyPressMsg directly using the Key struct fields.

func escKey() tea.KeyPressMsg   { return tea.KeyPressMsg{Code: tea.KeyEscape} }
func f1Key() tea.KeyPressMsg    { return tea.KeyPressMsg{Code: tea.KeyF1} }
func enterKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }
func tabKey() tea.KeyPressMsg   { return tea.KeyPressMsg{Code: tea.KeyTab} }
func upKey() tea.KeyPressMsg    { return tea.KeyPressMsg{Code: tea.KeyUp} }
func downKey() tea.KeyPressMsg  { return tea.KeyPressMsg{Code: tea.KeyDown} }
func leftKey() tea.KeyPressMsg  { return tea.KeyPressMsg{Code: tea.KeyLeft} }
func rightKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyRight} }

func charKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func ctrlKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
}

func altKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModAlt}
}

func shiftTabKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
}
