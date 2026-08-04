package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

func remapTestPalette(t *testing.T) semanticPalette {
	t.Helper()
	previous := noColor
	noColor = false
	t.Cleanup(func() { noColor = previous })
	return newSemanticPalette(true, defaultThemeID)
}

func remapStyle(palette semanticPalette, foreground interface{ RGBA() (r, g, b, a uint32) }, bold bool) lipgloss.Style {
	style := lipgloss.NewStyle().Foreground(foreground)
	if bold {
		style = style.Bold(true)
	}
	return style
}

func TestRemapANSI16BasicColorsMapToPalette(t *testing.T) {
	palette := remapTestPalette(t)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "black maps to dim",
			input: "\x1b[30mtext",
			want:  remapStyle(palette, palette.Dim, false).Render("text"),
		},
		{
			name:  "red maps to error",
			input: "\x1b[31mFAIL",
			want:  remapStyle(palette, palette.Error, false).Render("FAIL"),
		},
		{
			name:  "green maps to success",
			input: "\x1b[32mok",
			want:  remapStyle(palette, palette.Success, false).Render("ok"),
		},
		{
			name:  "yellow maps to warning",
			input: "\x1b[33mwarn",
			want:  remapStyle(palette, palette.Warning, false).Render("warn"),
		},
		{
			name:  "blue maps to accent2",
			input: "\x1b[34minfo",
			want:  remapStyle(palette, palette.Accent2, false).Render("info"),
		},
		{
			name:  "magenta maps to special",
			input: "\x1b[35mspecial",
			want:  remapStyle(palette, palette.Special, false).Render("special"),
		},
		{
			name:  "cyan maps to accent",
			input: "\x1b[36mnote",
			want:  remapStyle(palette, palette.Accent, false).Render("note"),
		},
		{
			name:  "white maps to text",
			input: "\x1b[37mplain",
			want:  remapStyle(palette, palette.Text, false).Render("plain"),
		},
		{
			name:  "bright red maps to the same error slot",
			input: "\x1b[91mFAIL",
			want:  remapStyle(palette, palette.Error, false).Render("FAIL"),
		},
		{
			name:  "bright green maps to the same success slot",
			input: "\x1b[92mok",
			want:  remapStyle(palette, palette.Success, false).Render("ok"),
		},
		{
			name:  "unstyled text uses the muted result style",
			input: "no color here",
			want:  remapStyle(palette, palette.Muted, false).Render("no color here"),
		},
		{
			name:  "reset returns to the default style",
			input: "\x1b[31mred\x1b[0m rest",
			want: remapStyle(palette, palette.Error, false).Render("red") +
				remapStyle(palette, palette.Muted, false).Render(" rest"),
		},
		{
			name:  "default foreground keeps bold",
			input: "\x1b[1m\x1b[31mred\x1b[39mbold default",
			want: remapStyle(palette, palette.Error, true).Render("red") +
				remapStyle(palette, palette.Muted, true).Render("bold default"),
		},
		{
			name:  "bold toggles on and off",
			input: "\x1b[1;32mbold\x1b[22m normal",
			want: remapStyle(palette, palette.Success, true).Render("bold") +
				remapStyle(palette, palette.Success, false).Render(" normal"),
		},
		{
			name:  "empty sgr body resets",
			input: "\x1b[31mred\x1b[mplain",
			want: remapStyle(palette, palette.Error, false).Render("red") +
				remapStyle(palette, palette.Muted, false).Render("plain"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := remapANSI16Line(tt.input, palette, 200); got != tt.want {
				t.Errorf("remapANSI16Line(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRemapANSI16StripsUnsupportedSequences(t *testing.T) {
	palette := remapTestPalette(t)
	muted := remapStyle(palette, palette.Muted, false)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "cursor movement dropped",
			input: "a\x1b[2Ab",
			want:  muted.Render("ab"),
		},
		{
			name:  "erase line dropped",
			input: "a\x1b[2Kb",
			want:  muted.Render("ab"),
		},
		{
			name:  "osc title dropped with bel terminator",
			input: "a\x1b]0;evil title\x07b",
			want:  muted.Render("ab"),
		},
		{
			name:  "osc hyperlink dropped with st terminator",
			input: "a\x1b]8;;https://example.com\x1b\\b",
			want:  muted.Render("ab"),
		},
		{
			name:  "dcs dropped",
			input: "a\x1bPq#0;2;0;0;0\x1b\\b",
			want:  muted.Render("ab"),
		},
		{
			name:  "256-color sgr dropped not applied",
			input: "\x1b[38;5;196mtext",
			want:  muted.Render("text"),
		},
		{
			name:  "truecolor sgr dropped not applied",
			input: "\x1b[38;2;255;0;0mtext",
			want:  muted.Render("text"),
		},
		{
			name:  "background sgr dropped",
			input: "\x1b[41mtext",
			want:  muted.Render("text"),
		},
		{
			name:  "mixed supported and unsupported params dropped whole",
			input: "\x1b[31;44mtext",
			want:  muted.Render("text"),
		},
		{
			name:  "unsupported sgr does not disturb tracked state",
			input: "\x1b[31mred\x1b[38;5;20mstill red",
			want:  remapStyle(palette, palette.Error, false).Render("redstill red"),
		},
		{
			name:  "unterminated csi at line end dropped",
			input: "text\x1b[31",
			want:  muted.Render("text"),
		},
		{
			name:  "unterminated osc at line end dropped",
			input: "text\x1b]0;evil",
			want:  muted.Render("text"),
		},
		{
			name:  "lone escape at line end dropped",
			input: "text\x1b",
			want:  muted.Render("text"),
		},
		{
			name:  "two-byte escape dropped",
			input: "a\x1bcb",
			want:  muted.Render("ab"),
		},
		{
			name:  "colon subparameters rejected",
			input: "\x1b[38:5:196mtext",
			want:  muted.Render("text"),
		},
		{
			name:  "control and bidi characters stripped from text",
			input: "a\u202eb\x07c",
			want:  muted.Render("abc"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := remapANSI16Line(tt.input, palette, 200)
			if got != tt.want {
				t.Errorf("remapANSI16Line(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRemapANSI16TruncatesToPlainWidth(t *testing.T) {
	palette := remapTestPalette(t)

	input := "\x1b[32m" + strings.Repeat("a", 50)
	got := remapANSI16Line(input, palette, 10)
	want := remapStyle(palette, palette.Success, false).Render(strings.Repeat("a", 9)) +
		remapStyle(palette, palette.Muted, false).Render("…")
	if got != want {
		t.Errorf("remapANSI16Line width 10 = %q, want %q", got, want)
	}
	if plainWidth := lipgloss.Width(got); plainWidth != 10 {
		t.Errorf("visible width = %d, want 10", plainWidth)
	}
}

func TestRenderSemanticResultLinesUsesDisplayVariantWithoutCacheLeak(t *testing.T) {
	palette := remapTestPalette(t)

	card := NewToolCard("bash", ToolCardBash, true, defaultThemeID)
	card.Result = "build ok"
	plain := card.renderSemanticResultLines(card.Result, 40)
	if len(plain) != 1 {
		t.Fatalf("plain render lines = %d, want 1", len(plain))
	}

	card.ResultDisplay = "\x1b[32mbuild ok\x1b[0m"
	display := card.renderSemanticResultLines(card.Result, 40)
	if len(display) != 1 {
		t.Fatalf("display render lines = %d, want 1", len(display))
	}
	want := remapStyle(palette, palette.Success, false).Render("build ok")
	if display[0] != want {
		t.Errorf("display line = %q, want %q", display[0], want)
	}
	if display[0] == plain[0] {
		t.Error("display variant must not reuse the cached plain rendering")
	}
	if strings.Contains(display[0], "\x1b[32m") {
		t.Error("raw input escape bytes must never be copied to output")
	}

	// The plain variant must still be served for the same result afterwards.
	card.ResultDisplay = ""
	again := card.renderSemanticResultLines(card.Result, 40)
	if again[0] != plain[0] {
		t.Errorf("plain rendering changed after display render: %q, want %q", again[0], plain[0])
	}
}

func TestRenderSemanticResultLinesDisplayVariantSkipsSearchAndCode(t *testing.T) {
	remapTestPalette(t)

	search := NewToolCard("grep", ToolCardSearch, true, defaultThemeID)
	search.Result = "main.go:1:package main"
	search.ResultDisplay = "\x1b[35mmain.go\x1b[0m:1:package main"
	baseline := NewToolCard("grep", ToolCardSearch, true, defaultThemeID)
	baseline.Result = search.Result
	if got, want := search.renderSemanticResultLines(search.Result, 60), baseline.renderSemanticResultLines(baseline.Result, 60); got[0] != want[0] {
		t.Errorf("search kind must ignore the display variant: %q, want %q", got[0], want[0])
	}

	code := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	code.ResultLanguage = "go"
	code.Result = "package main"
	code.ResultDisplay = "\x1b[31mpackage main\x1b[0m"
	codeBaseline := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	codeBaseline.ResultLanguage = "go"
	codeBaseline.Result = code.Result
	if got, want := code.renderSemanticResultLines(code.Result, 60), codeBaseline.renderSemanticResultLines(codeBaseline.Result, 60); got[0] != want[0] {
		t.Errorf("code kind must ignore the display variant: %q, want %q", got[0], want[0])
	}
}

func TestRemappedDisplayResultLinesRespectsNoColor(t *testing.T) {
	previous := noColor
	noColor = true
	t.Cleanup(func() { noColor = previous })

	card := NewToolCard("bash", ToolCardBash, true, defaultThemeID)
	card.Result = "ok"
	card.ResultDisplay = "\x1b[32mok\x1b[0m"
	if lines := card.remappedDisplayResultLines(40); lines != nil {
		t.Errorf("NO_COLOR must disable the display variant, got %q", lines)
	}
}

func TestPersistedSessionNeverContainsEscapeBytes(t *testing.T) {
	entries := []ToolEntry{{
		ID:            "tool-1",
		Name:          "bash",
		Summary:       "go test",
		Args:          "go test ./...",
		Result:        "ok  \tgithub.com/example/pkg\t0.2s",
		ResultDisplay: "\x1b[32mok\x1b[0m  \tgithub.com/example/pkg\t0.2s\x1b]0;title\x07",
		Status:        ToolStatusDone,
		StartTime:     time.Now(),
		Duration:      time.Second,
	}}

	persisted := persistToolEntries(entries)
	encoded, err := json.Marshal(persisted)
	if err != nil {
		t.Fatalf("marshal persisted entries: %v", err)
	}
	if bytes.ContainsRune(encoded, 0x1b) {
		t.Fatalf("persisted session output contains ESC byte: %q", encoded)
	}

	restored := restoreToolEntries(persisted)
	if restored[0].ResultDisplay != "" {
		t.Errorf("restored ResultDisplay = %q, want empty (restored sessions have no color)", restored[0].ResultDisplay)
	}
	if strings.ContainsRune(restored[0].Result, 0x1b) {
		t.Error("restored Result must never contain escape bytes")
	}
}

func TestToolCardAttentionExpandedUsesDisplayVariant(t *testing.T) {
	palette := remapTestPalette(t)

	card := NewToolCard("bash", ToolCardBash, true, defaultThemeID)
	card.State = ToolCardAttention
	card.Expanded = true
	card.Result = "warning: something"
	card.ResultDisplay = "\x1b[33mwarning: something\x1b[0m"

	view := card.View(60)
	want := remapStyle(palette, palette.Warning, false).Render("warning: something")
	if !strings.Contains(view, want) {
		t.Errorf("attention expanded view missing remapped line %q in %q", want, view)
	}
}
