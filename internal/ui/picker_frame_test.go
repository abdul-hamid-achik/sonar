package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestPickerFrameASCIIProfileNormalizesNavigationChrome(t *testing.T) {
	m := newTestModel(t)
	m.width = 80
	m.glyphProfile = GlyphASCII

	footer := m.pickerNavigationFooter(true)
	plainFooter := ansi.Strip(footer)
	if !strings.Contains(plainFooter, "j/k move") ||
		!strings.Contains(plainFooter, " | ") {
		t.Fatalf("ASCII picker footer = %q", plainFooter)
	}
	if strings.ContainsAny(plainFooter, "…·↑↓←→") {
		t.Fatalf("ASCII picker footer leaked Unicode chrome: %q", plainFooter)
	}

	rawStyledFooter := m.renderKeyHints(
		80,
		keyHint{Key: "↑/↓", Action: "move"},
		keyHint{Key: "←", Action: "old"},
		keyHint{Key: "→", Action: "new"},
	)
	frame := ansi.Strip(m.renderPickerFrame("picker", rawStyledFooter))
	if !strings.Contains(frame, "j/k move") ||
		!strings.Contains(frame, "< old") ||
		!strings.Contains(frame, "> new") ||
		!strings.Contains(frame, " | ") {
		t.Fatalf("ASCII frame did not normalize styled footer:\n%s", frame)
	}
	if strings.ContainsAny(frame, "…·↑↓←→╭╮╰╯│") {
		t.Fatalf("ASCII picker frame leaked Unicode chrome:\n%s", frame)
	}
	wantWidth := pickerContentWidth(m.width) + 2
	for row, line := range strings.Split(frame, "\n") {
		if got := lipgloss.Width(line); got != wantWidth {
			t.Fatalf("frame row %d width = %d, want %d: %q", row, got, wantWidth, line)
		}
	}
}

func TestPickerMoveKeyKeepsUnicodeAndASCIIProfilesIndependent(t *testing.T) {
	if got := pickerMoveKey(GlyphUnicode); got != "↑/↓" {
		t.Fatalf("Unicode move key = %q", got)
	}
	if got := pickerMoveKey(GlyphASCII); got != "j/k" {
		t.Fatalf("ASCII move key = %q", got)
	}
}
