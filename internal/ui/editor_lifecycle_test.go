package ui

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestSplitEditorCommandSupportsQuotedPathsAndArgumentsWithoutShell(t *testing.T) {
	tests := []struct {
		value string
		want  []string
	}{
		{value: "nvim -f", want: []string{"nvim", "-f"}},
		{value: `"/Applications/Visual Editor/bin/editor" --wait`, want: []string{"/Applications/Visual Editor/bin/editor", "--wait"}},
		{value: `editor --cmd 'set spell' ""`, want: []string{"editor", "--cmd", "set spell", ""}},
		{value: `path\ with\ spaces --flag`, want: []string{"path with spaces", "--flag"}},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			got, err := splitEditorCommand(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("splitEditorCommand(%q) = %#v, want %#v", test.value, got, test.want)
			}
		})
	}
	for _, value := range []string{`editor "unterminated`, `editor trailing\`} {
		if _, err := splitEditorCommand(value); err == nil {
			t.Fatalf("splitEditorCommand(%q) accepted incomplete syntax", value)
		}
	}
}

func TestEditorResultMessageCanClearDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "draft.md")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	message, ok := editorResultMessage(path, nil, 32*1024).(editorReturnMsg)
	if !ok || message.Content != "" {
		t.Fatalf("empty editor result = %#v, want an explicit empty return", message)
	}

	m := newTestModel(t)
	m.input.SetValue("remove this draft")
	updated, _ := m.Update(message)
	m = updated.(*Model)
	if got := m.input.Value(); got != "" {
		t.Fatalf("empty editor result left draft %q", got)
	}

	runErr := errors.New("editor stopped")
	if message, ok := editorResultMessage(path, runErr, 32*1024).(ErrorMsg); !ok || !strings.Contains(message.Msg, runErr.Error()) {
		t.Fatalf("editor failure result = %#v", message)
	}
}

func TestEditorResultMessageRejectsOversizedDraftWithoutReplacingComposer(t *testing.T) {
	tests := []struct {
		name    string
		content string
		limit   int
	}{
		{name: "ascii", content: "123456", limit: 5},
		{name: "multibyte", content: strings.Repeat("界", 6), limit: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "draft.md")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			message, ok := editorResultMessage(path, nil, test.limit).(ErrorMsg)
			if !ok || !strings.Contains(message.Msg, "exceeds") {
				t.Fatalf("oversized editor result = %#v, want limit error", message)
			}

			m := newTestModel(t)
			m.input.SetValue("keep this draft")
			updated, _ := m.Update(message)
			m = updated.(*Model)
			if got := m.input.Value(); got != "keep this draft" {
				t.Fatalf("oversized editor result replaced draft with %q", got)
			}
		})
	}
}

func TestEditorResultMessageRejectsInvalidUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "draft.md")
	if err := os.WriteFile(path, []byte{0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}
	message, ok := editorResultMessage(path, nil, 32*1024).(ErrorMsg)
	if !ok || !strings.Contains(message.Msg, "UTF-8") {
		t.Fatalf("invalid UTF-8 editor result = %#v", message)
	}
}

type blockingExecCommand struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (c blockingExecCommand) Run() error {
	close(c.started)
	<-c.release
	return nil
}

func (blockingExecCommand) SetStdin(io.Reader)  {}
func (blockingExecCommand) SetStdout(io.Writer) {}
func (blockingExecCommand) SetStderr(io.Writer) {}

type execOwnershipModel struct {
	command tea.ExecCommand
}

func (m execOwnershipModel) Init() tea.Cmd {
	return tea.Exec(m.command, nil)
}

func (m execOwnershipModel) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return m, nil
}

func (m execOwnershipModel) View() tea.View { return tea.NewView("") }

// openExternalEditor uses tea.ExecProcess, which goes through the same
// synchronous tea.Exec path exercised here. Bubble Tea releases the terminal,
// blocks its event loop in ExecCommand.Run, then restores the terminal before
// it can process a normal quit. Therefore a graceful sonar exit cannot
// leave an editor child running; the second OS signal remains the emergency
// escape for a wedged interactive program.
func TestBubbleTeaOwnsInteractiveEditorUntilItReturns(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	p := tea.NewProgram(
		execOwnershipModel{command: blockingExecCommand{started: started, release: release}},
		tea.WithInput(strings.NewReader("")),
		tea.WithOutput(io.Discard),
		tea.WithoutRenderer(),
		tea.WithoutSignalHandler(),
	)
	runDone := make(chan error, 1)
	go func() {
		_, err := p.Run()
		runDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("interactive command did not start")
	}

	quitReturned := make(chan struct{})
	go func() {
		p.Quit()
		close(quitReturned)
	}()
	select {
	case err := <-runDone:
		t.Fatalf("program exited while interactive child was running: %v", err)
	case <-quitReturned:
		t.Fatal("graceful quit bypassed the synchronously owned interactive child")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("program exit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("program did not exit after interactive child returned")
	}
}
