package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestSessionTitleTruncationPreservesUnicode(t *testing.T) {
	title := strings.Repeat("会話🙂", 20)
	got := (sessionItem{title: title}).Title()
	if !utf8.ValidString(got) {
		t.Fatalf("session title is invalid UTF-8: %q", got)
	}
	if width := lipgloss.Width(got); width > 40 {
		t.Fatalf("session title width = %d, want <= 40", width)
	}
}

func TestSessionItemSanitizesPersistedTitle(t *testing.T) {
	item := sessionItem{id: 7, publicID: "a1b2c3d", title: "safe\x1b]8;;https://example.invalid\x07link\x1b]8;;\x07\n\u202eevil"}

	if got, want := item.Title(), "a1b2c3d · safelink evil"; got != want {
		t.Fatalf("session title = %q, want %q", got, want)
	}
	if got, want := item.FilterValue(), "a1b2c3d 7 safelink evil"; got != want {
		t.Fatalf("session filter value = %q, want %q", got, want)
	}
}

func TestFormatSessionTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "utc with fractional seconds",
			value: "2026-07-11T12:00:00.000Z",
			want:  "Jul 11, 2026 · 12:00 UTC",
		},
		{
			name:  "recorded negative offset",
			value: "2026-07-11T12:00:00-06:00",
			want:  "Jul 11, 2026 · 12:00 UTC-06:00",
		},
		{
			name:  "recorded positive offset",
			value: "2026-07-11T12:00:00+05:30",
			want:  "Jul 11, 2026 · 12:00 UTC+05:30",
		},
		{name: "friendly label", value: "yesterday", want: "yesterday"},
		{name: "unparseable label", value: "  just now  ", want: "  just now  "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatSessionTimestamp(tt.value); got != tt.want {
				t.Fatalf("formatSessionTimestamp(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestSessionsPickerFormatsPersistedTimestampOnce(t *testing.T) {
	state := newSessionsPickerState([]SessionListItem{{
		ID:        1,
		Title:     "Release polish",
		CreatedAt: "2026-07-11T12:00:00.000Z",
	}}, 80, 24, true, defaultThemeID)

	item, ok := state.List.Items()[0].(sessionItem)
	if !ok {
		t.Fatalf("sessions item has type %T, want sessionItem", state.List.Items()[0])
	}
	if got, want := item.Description(), "Jul 11, 2026 · 12:00 UTC"; got != want {
		t.Fatalf("session description = %q, want %q", got, want)
	}
}

func TestSessionsPickerFitsMinimumAndKeepsFooter(t *testing.T) {
	m := newTestModel(t)
	m.width = minTerminalWidth
	m.height = minTerminalHeight
	m.sessionsPickerState = newSessionsPickerState([]SessionListItem{
		{ID: 1, Title: strings.Repeat("会話🙂", 12), CreatedAt: "just now"},
		{ID: 2, Title: "Second session", CreatedAt: "yesterday"},
	}, m.width, m.height, m.isDark, m.themeID)
	m.overlay = OverlaySessionsPicker

	rendered := m.renderSessionsPicker()
	assertRenderedLinesFit(t, rendered, minTerminalWidth)
	assertRenderedHeightFits(t, rendered, minTerminalHeight)
	plain := ansi.Strip(rendered)
	if !strings.Contains(plain, "esc close") {
		t.Fatalf("sessions footer missing close affordance:\n%s", rendered)
	}
}

func TestSessionsPickerFooterNamesSlashFilterBinding(t *testing.T) {
	m := newTestModel(t)
	m.sessionsPickerState = newSessionsPickerState([]SessionListItem{
		{ID: 1, Title: "First session", CreatedAt: "just now"},
		{ID: 2, Title: "Second session", CreatedAt: "yesterday"},
	}, m.width, m.height, m.isDark, m.themeID)
	m.overlay = OverlaySessionsPicker

	rendered := ansi.Strip(m.renderSessionsPicker())
	if !strings.Contains(rendered, "/ filter") {
		t.Fatalf("sessions footer should expose the Bubbles filter binding:\n%s", rendered)
	}
	if strings.Contains(rendered, "Type to filter") {
		t.Fatalf("sessions footer should not imply filtering starts without /:\n%s", rendered)
	}
}
