package main

import (
	"io"
	"os"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
	"github.com/abdul-hamid-achik/sonar/internal/ui"
)

type sessionResumeInfoSource interface {
	SessionResumeInfo() (ui.SessionResumeInfo, bool)
}

// writeSessionResumeMessage runs only after Bubble Tea has returned and
// restored the terminal. Validate and reformat the handle at this final output
// boundary so user-derived text can never become part of the command.
func writeSessionResumeMessage(writer io.Writer, finalModel tea.Model, runErr error) {
	if writer == nil || runErr != nil || finalModel == nil {
		return
	}
	source, ok := finalModel.(sessionResumeInfoSource)
	if !ok {
		return
	}
	info, ok := source.SessionResumeInfo()
	if !ok {
		return
	}
	// Parse normalizes case; Format is the identity for a valid public id.
	publicID, err := sessionref.Parse(strings.TrimSpace(info.Handle))
	if err != nil {
		return
	}
	handle := sessionref.Format(publicID)
	if handle == "" {
		return
	}
	label := "Session " + handle
	if title := sanitizeExitSessionTitle(info.Title); title != "" {
		label += " · " + title
	}
	lines := []string{"", label, "Resume this session with:", "  sonar --resume " + handle}
	_, _ = io.WriteString(writer, exitReceiptText(lines, terminalWriter(writer)))
}

// exitReceiptText joins the exit receipt, erasing to end of line on a terminal.
//
// These lines land on rows the restored terminal may still be showing the last
// TUI frame on. Without the erase, any leftover tail of the row underneath is
// appended to ours, and the resume line in particular becomes a command that
// looks copyable and is wrong ("--resume c0ffee1" printed over a row ending in
// "session" reads as "--resume c0ffee1ession"). The escape is emitted only for
// a terminal so redirected output stays plain text.
func exitReceiptText(lines []string, isTerminal bool) string {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		if isTerminal {
			b.WriteString("\x1b[K")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// terminalWriter reports whether writer is an interactive terminal.
func terminalWriter(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	return ok && term.IsTerminal(file.Fd())
}

func sanitizeExitSessionTitle(title string) string {
	title = ansi.Strip(strings.ToValidUTF8(title, "�"))
	title = strings.Map(func(value rune) rune {
		if unicode.IsControl(value) || isExitBidiControl(value) {
			return ' '
		}
		return value
	}, title)
	title = strings.Join(strings.Fields(title), " ")
	if runes := []rune(title); len(runes) > 72 {
		title = string(runes[:69]) + "..."
	}
	return title
}

func isExitBidiControl(value rune) bool {
	switch value {
	case '\u061c', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069':
		return true
	default:
		return false
	}
}
