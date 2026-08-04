package ui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestParseSlashCommandInputSupportsSafeQuotedPaths(t *testing.T) {
	tests := []struct {
		input    string
		wantName string
		wantArgs []string
		wantErr  bool
	}{
		{input: `/load "notes/My Project.md"`, wantName: "load", wantArgs: []string{"notes/My Project.md"}},
		{input: `/export --force notes/My Project.md`, wantName: "export", wantArgs: []string{"--force", "notes/My", "Project.md"}},
		{input: `/import notes/My\ Project.md`, wantName: "import", wantArgs: []string{"notes/My Project.md"}},
		{input: `/load 'literal $HOME.md'`, wantName: "load", wantArgs: []string{"literal $HOME.md"}},
		{input: `/load "unterminated`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			name, args, err := parseSlashCommandInput(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected parse error, got name=%q args=%#v", name, args)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if name != test.wantName || !reflect.DeepEqual(args, test.wantArgs) {
				t.Fatalf("parse = %q %#v, want %q %#v", name, args, test.wantName, test.wantArgs)
			}
		})
	}
}

func TestConversationExportIsAtomicPrivateAndRequiresForce(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "conversation notes.md")
	resolved, err := writeConversationExport(workDir, "conversation notes.md", []byte("first"), false)
	if err != nil {
		t.Fatal(err)
	}
	wantResolved, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	wantResolved = filepath.Join(wantResolved, filepath.Base(path))
	if resolved != wantResolved {
		t.Fatalf("resolved path = %q, want %q", resolved, wantResolved)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first" {
		t.Fatalf("export = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("new export permissions = %o, want 600", info.Mode().Perm())
	}

	if _, err := writeConversationExport(workDir, path, []byte("second"), false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("existing destination was replaced without confirmation: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "first" {
		t.Fatalf("refused export changed destination to %q", data)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := writeConversationExport(workDir, path, []byte("second"), true); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "second" {
		t.Fatalf("forced export = %q", data)
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("forced export permissions = %o, want preserved 640", info.Mode().Perm())
	}
}

func TestConversationExportRefusesSymlinkVictim(t *testing.T) {
	workDir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workDir, "transcript.md")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if _, err := writeConversationExport(workDir, link, []byte("secret"), true); err == nil || !strings.Contains(err.Error(), "symbolic-link") {
		t.Fatalf("symlink export error = %v", err)
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatalf("symlink victim changed to %q", data)
	}
}

func TestRelativeExportCannotEscapeThroughParentSymlink(t *testing.T) {
	workDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workDir, "out")); err != nil {
		t.Fatal(err)
	}
	if _, err := writeConversationExport(workDir, filepath.Join("out", "conversation.md"), []byte("no"), false); err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("parent symlink escape error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "conversation.md")); !os.IsNotExist(err) {
		t.Fatalf("outside export exists: %v", err)
	}
}

func TestContextLoadRunsOffUpdateAndIgnoresStaleResult(t *testing.T) {
	m := newTestModel(t)
	path := filepath.Join(t.TempDir(), "context.md")
	if err := os.WriteFile(path, []byte("bounded context"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := m.handleCommandAction(command.Result{Action: command.ActionLoadContext, Data: path})
	if cmd == nil || !m.fileLoading {
		t.Fatal("context load did not start asynchronously")
	}
	result := awaitCommandMessage[ContextLoadResultMsg](t, commandMessages(cmd), 2*time.Second)
	m.fileOpToken++ // simulate Escape or a superseding authority operation
	updated, _ := m.Update(result)
	m = updated.(*Model)
	if m.loadedFile != "" || m.manualLoadedContext != "" {
		t.Fatalf("stale load changed context: file=%q content=%q", m.loadedFile, m.manualLoadedContext)
	}
}

func TestImportResultTokenPreventsLateTranscriptReplacement(t *testing.T) {
	m := newTestModel(t)
	m.fileLoading = true
	m.fileOpToken = 8
	m.agent.ReplaceMessages([]llm.Message{{Role: "user", Content: "current"}})
	late := ImportResultMsg{
		Token:    7,
		Entries:  []ChatEntry{{Kind: "user", Content: "stale"}},
		Messages: []llm.Message{{Role: "user", Content: "stale"}},
	}
	updated, _ := m.Update(late)
	m = updated.(*Model)
	messages := m.agent.Messages()
	if len(messages) != 1 || messages[0].Content != "current" {
		t.Fatalf("late import replaced transcript: %#v", messages)
	}
}

func TestGracefulShutdownWaitsForMatchingExportReceipt(t *testing.T) {
	m := newTestModel(t)
	m.exportRunning = true
	m.exportToken = 9
	if cmd := m.beginShutdown(); cmd == nil {
		t.Fatal("shutdown did not start its liveness clock while waiting for the export receipt")
	}

	updated, cmd := m.Update(ExportResultMsg{Token: 8, Path: "stale.md"})
	m = updated.(*Model)
	if !m.exportRunning || cmd != nil {
		t.Fatal("stale export receipt released graceful shutdown")
	}

	updated, cmd = m.Update(ExportResultMsg{Token: 9, Path: "done.md"})
	m = updated.(*Model)
	if m.exportRunning || cmd == nil {
		t.Fatal("matching export receipt did not release graceful shutdown")
	}
}

func TestShutdownQuitGateRequiresAllOwnedEffects(t *testing.T) {
	tests := []struct {
		name          string
		cancel        bool
		commitRunning bool
		exportRunning bool
		wantQuit      bool
	}{
		{name: "agent still running", cancel: true},
		{name: "commit still running", commitRunning: true},
		{name: "export still running", exportRunning: true},
		{name: "all receipts returned", wantQuit: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			m.shuttingDown = true
			m.commitRunning = test.commitRunning
			m.exportRunning = test.exportRunning
			if test.cancel {
				m.cancel = func() {}
			}
			var commands []tea.Cmd
			m.appendShutdownQuit(&commands)
			if got := len(commands) == 1; got != test.wantQuit {
				t.Fatalf("quit appended = %v, want %v", got, test.wantQuit)
			}
		})
	}
}

func TestCommitReceiptCannotQuitBeforeExportReceipt(t *testing.T) {
	m := newTestModel(t)
	m.shuttingDown = true
	m.commitRunning = true
	m.commitToken = 3
	m.exportRunning = true
	m.exportToken = 4

	updated, cmd := m.Update(CommitResultMsg{Token: 3, Message: "done"})
	m = updated.(*Model)
	if m.commitRunning || !m.exportRunning || cmd != nil {
		t.Fatalf("commit receipt released export join: commit=%v export=%v cmd=%v", m.commitRunning, m.exportRunning, cmd != nil)
	}
	updated, cmd = m.Update(ExportResultMsg{Token: 4, Path: "conversation.md"})
	m = updated.(*Model)
	if m.exportRunning || cmd == nil {
		t.Fatal("last owned receipt did not release shutdown")
	}
}

func TestSecondExportIsRejectedUntilReceipt(t *testing.T) {
	m := newTestModel(t)
	m.exportRunning = true
	m.exportToken = 4
	cmd := m.handleCommandAction(command.Result{Action: command.ActionExport, Data: filepath.Join(t.TempDir(), "second.md")})
	if cmd != nil {
		t.Fatal("second export started while first export was running")
	}
	if m.exportToken != 4 || !m.exportRunning {
		t.Fatalf("running export identity changed: token=%d running=%v", m.exportToken, m.exportRunning)
	}
	if last := m.entries[len(m.entries)-1]; last.Kind != "error" || !strings.Contains(last.Content, "already in progress") {
		t.Fatalf("missing duplicate-export receipt: %#v", last)
	}
}

func TestPublishedExportWarningIsNotReportedAsFailure(t *testing.T) {
	m := newTestModel(t)
	m.exportRunning = true
	m.exportToken = 2
	updated, _ := m.Update(ExportResultMsg{
		Token: 2,
		Path:  "conversation.md",
		Err:   &exportPublishedWarning{cause: os.ErrInvalid},
	})
	m = updated.(*Model)
	last := m.entries[len(m.entries)-1]
	if last.Kind != "system" || !strings.Contains(last.Content, "do not retry blindly") {
		t.Fatalf("published outcome was misreported: %#v", last)
	}
}
