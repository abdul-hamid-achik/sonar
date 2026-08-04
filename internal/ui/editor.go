package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// openExternalEditor opens $VISUAL/$EDITOR with the current input text, then replaces
// the textarea content with whatever the user wrote. tea.ExecProcess owns this
// interactive child synchronously: Bubble Tea cannot process a normal quit or
// restore the terminal until the editor callback returns. Keep interactive
// processes on this path rather than dispatching them as unjoined tea.Cmd work.
func (m *Model) openExternalEditor() tea.Cmd {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		editor = "vi"
	}
	editorArgs, err := splitEditorCommand(editor)
	if err != nil {
		return func() tea.Msg {
			return ErrorMsg{Msg: fmt.Sprintf("editor: %v", err)}
		}
	}

	// Write current input to a temp file.
	tmpFile, err := os.CreateTemp("", "sonar-*.md")
	if err != nil {
		return func() tea.Msg {
			return ErrorMsg{Msg: fmt.Sprintf("editor: %v", err)}
		}
	}
	tmpPath := tmpFile.Name()
	if current := m.input.Value(); current != "" {
		if _, err := tmpFile.WriteString(current); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
			return func() tea.Msg {
				return ErrorMsg{Msg: fmt.Sprintf("editor: %v", err)}
			}
		}
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return func() tea.Msg {
			return ErrorMsg{Msg: fmt.Sprintf("editor: %v", err)}
		}
	}

	c := exec.Command(editorArgs[0], append(editorArgs[1:], tmpPath)...)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		defer func() { _ = os.Remove(tmpPath) }()
		return editorResultMessage(tmpPath, err, m.input.CharLimit)
	})
}

// splitEditorCommand accepts the useful command+arguments subset of shell
// words without invoking a shell. Quoted paths and flags work; expansions,
// pipelines, and command substitution never gain execution authority here.
func splitEditorCommand(value string) ([]string, error) {
	var args []string
	var current strings.Builder
	var quote rune
	escaped := false
	started := false
	flush := func() {
		if !started {
			return
		}
		args = append(args, current.String())
		current.Reset()
		started = false
	}
	for _, char := range value {
		if escaped {
			current.WriteRune(char)
			escaped = false
			started = true
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				current.WriteRune(char)
			}
			started = true
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
			started = true
		case ' ', '\t', '\r', '\n':
			flush()
		default:
			current.WriteRune(char)
			started = true
		}
	}
	if escaped {
		return nil, errors.New("editor command ends with an incomplete escape")
	}
	if quote != 0 {
		return nil, errors.New("editor command has an unterminated quote")
	}
	flush()
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return nil, errors.New("editor command is empty")
	}
	return args, nil
}

func editorResultMessage(path string, runErr error, charLimit int) tea.Msg {
	if runErr != nil {
		return ErrorMsg{Msg: fmt.Sprintf("editor: %v", runErr)}
	}
	file, err := os.Open(path)
	if err != nil {
		return ErrorMsg{Msg: fmt.Sprintf("editor: %v", err)}
	}
	defer func() { _ = file.Close() }()

	// A valid UTF-8 rune occupies at most utf8.UTFMax bytes. Allow one extra
	// byte for the conventional trailing newline, but never read an unbounded
	// editor file into the TUI. The textarea must not silently truncate content
	// the user just edited outside sonar.
	maxBytes := int64(1 << 20)
	if charLimit > 0 {
		maxBytes = int64(charLimit)*utf8.UTFMax + 1
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return ErrorMsg{Msg: fmt.Sprintf("editor: %v", err)}
	}
	if int64(len(data)) > maxBytes {
		return ErrorMsg{Msg: fmt.Sprintf("editor: edited draft exceeds the %d-character limit", charLimit)}
	}
	if !utf8.Valid(data) {
		return ErrorMsg{Msg: "editor: edited draft is not valid UTF-8"}
	}

	content := strings.TrimRight(string(data), "\n")
	if charLimit > 0 && utf8.RuneCountInString(content) > charLimit {
		return ErrorMsg{Msg: fmt.Sprintf("editor: edited draft exceeds the %d-character limit", charLimit)}
	}
	// Empty output is intentional: clearing the file clears the draft.
	return editorReturnMsg{Content: content}
}

// editorReturnMsg is sent when the external editor closes.
type editorReturnMsg struct {
	Content string
}

// handleEditorReturn restores the composer draft produced by the external
// editor.
func (m *Model) handleEditorReturn(msg editorReturnMsg) {
	m.clearCompletionSuppression()
	m.input.SetValue(msg.Content)
	m.input.CursorEnd()
	_ = m.reflowInputViewport()
	m.input.Focus()
}
