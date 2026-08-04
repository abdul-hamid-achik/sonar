package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/ui"
)

type exitResumeTestModel struct {
	info ui.SessionResumeInfo
	ok   bool
}

func (m *exitResumeTestModel) Init() tea.Cmd { return nil }

func (m *exitResumeTestModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }

func (m *exitResumeTestModel) View() tea.View { return tea.NewView("") }

func (m *exitResumeTestModel) SessionResumeInfo() (ui.SessionResumeInfo, bool) {
	return m.info, m.ok
}

func TestWriteSessionResumeMessageUsesCanonicalShortHandle(t *testing.T) {
	var output bytes.Buffer
	writeSessionResumeMessage(&output, &exitResumeTestModel{
		info: ui.SessionResumeInfo{Handle: "a1b2c3d", Title: "Polish transcript UX"},
		ok:   true,
	}, nil)

	if got, want := output.String(), "\nSession a1b2c3d · Polish transcript UX\nResume this session with:\n  sonar --resume a1b2c3d\n"; got != want {
		t.Fatalf("resume message = %q, want %q", got, want)
	}
}

func TestWriteSessionResumeMessageSanitizesTitleOutsideCommand(t *testing.T) {
	var output bytes.Buffer
	writeSessionResumeMessage(&output, &exitResumeTestModel{
		info: ui.SessionResumeInfo{
			Handle: "deadbee",
			Title:  "Review\x1b]0;owned\x07\nthen deploy\u202e",
		},
		ok: true,
	}, nil)

	got := output.String()
	if !strings.Contains(got, "Session deadbee · Review then deploy") {
		t.Fatalf("sanitized session label = %q", got)
	}
	if strings.Contains(got, "owned") || strings.Contains(got, "\x1b]") || strings.Contains(got, "\u202e") {
		t.Fatalf("unsafe title content survived: %q", got)
	}
	if strings.Count(got, "sonar --resume deadbee") != 1 {
		t.Fatalf("canonical command changed: %q", got)
	}
}

func TestWriteSessionResumeMessageSuppressesUnavailableOrFailedExit(t *testing.T) {
	tests := []struct {
		name  string
		model tea.Model
		err   error
	}{
		{name: "no final model"},
		{name: "no durable session", model: &exitResumeTestModel{}},
		{name: "invalid handle", model: &exitResumeTestModel{info: ui.SessionResumeInfo{Handle: "a1b2c3d\nrm -rf /"}, ok: true}},
		{name: "tui error", model: &exitResumeTestModel{info: ui.SessionResumeInfo{Handle: "a1b2c3d"}, ok: true}, err: errors.New("terminal restore failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			writeSessionResumeMessage(&output, test.model, test.err)
			if output.Len() != 0 {
				t.Fatalf("unexpected resume message %q", output.String())
			}
		})
	}
}

// The exit receipt lands on rows the restored terminal may still be painting
// the last TUI frame on. Without an erase, the tail of the row underneath is
// appended to ours and the resume line becomes a wrong, copyable command.
func TestExitReceiptErasesLineTailsOnATerminal(t *testing.T) {
	lines := []string{"", "Session c0ffee1", "Resume this session with:", "  sonar --resume c0ffee1"}

	terminal := exitReceiptText(lines, true)
	for _, line := range lines {
		if !strings.Contains(terminal, line+"\x1b[K\n") {
			t.Fatalf("line %q was written without an erase:\n%q", line, terminal)
		}
	}

	// Redirected output must stay plain text.
	plain := exitReceiptText(lines, false)
	if strings.Contains(plain, "\x1b") {
		t.Fatalf("non-terminal receipt leaked an escape sequence:\n%q", plain)
	}
	if plain != "\nSession c0ffee1\nResume this session with:\n  sonar --resume c0ffee1\n" {
		t.Fatalf("non-terminal receipt changed shape:\n%q", plain)
	}
}
