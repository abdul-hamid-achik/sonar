package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestMarkdownStyleUsesAdaptiveNonErrorInlineCode(t *testing.T) {
	tests := []struct {
		name       string
		isDark     bool
		foreground string
		background string
	}{
		{name: "light", foreground: "#3B4252", background: "#ECEFF4"},
		{name: "dark", isDark: true, foreground: "#E5E9F0", background: "#3B4252"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			style := markdownStyleConfig(tt.isDark, defaultThemeID)
			if style.Code.Color == nil || *style.Code.Color != tt.foreground {
				t.Fatalf("inline code foreground = %v, want %s", style.Code.Color, tt.foreground)
			}
			if style.Code.BackgroundColor == nil || *style.Code.BackgroundColor != tt.background {
				t.Fatalf("inline code background = %v, want %s", style.Code.BackgroundColor, tt.background)
			}
			if *style.Code.Color == "203" {
				t.Fatal("inline code retained Glamour's error-like red foreground")
			}
		})
	}
}

func TestMarkdownInlineCodeMeetsNormalTextContrast(t *testing.T) {
	for _, test := range []struct {
		name   string
		isDark bool
	}{
		{name: "light"},
		{name: "dark", isDark: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			style := markdownStyleConfig(test.isDark, defaultThemeID)
			if style.Code.Color == nil || style.Code.BackgroundColor == nil {
				t.Fatalf("inline code style is incomplete: foreground=%v background=%v", style.Code.Color, style.Code.BackgroundColor)
			}
			foreground := lipgloss.Color(*style.Code.Color)
			background := lipgloss.Color(*style.Code.BackgroundColor)
			const minimumContrast = 4.5
			if ratio := contrastRatio(foreground, background); ratio < minimumContrast {
				t.Fatalf("inline code contrast = %.2f:1, want >= %.1f:1 (foreground=%s background=%s)",
					ratio, minimumContrast, *style.Code.Color, *style.Code.BackgroundColor)
			}
		})
	}
}

func TestMarkdownReadableMeasurePreservesWideWorkBlocks(t *testing.T) {
	renderer := NewMarkdownRenderer(160, true, defaultThemeID)
	if renderer.proseWidth != ProseTargetCandidate {
		t.Fatalf("prose width = %d, want %d", renderer.proseWidth, ProseTargetCandidate)
	}

	prose := strings.Repeat("A readable paragraph should not span the entire terminal. ", 20)
	renderedProse := ansi.Strip(renderer.RenderFull(prose))
	if width := widestDisplayLine(renderedProse); width > ProseTargetCandidate {
		t.Fatalf("prose line width = %d, want <= %d:\n%s", width, ProseTargetCandidate, renderedProse)
	}

	code := "```text\n" + strings.Repeat("x", 120) + "\n```"
	renderedCode := ansi.Strip(renderer.RenderFull(code))
	if width := widestDisplayLine(renderedCode); width <= ProseTargetCandidate {
		t.Fatalf("code line width = %d, want work surface wider than prose measure:\n%s", width, renderedCode)
	}
}

func TestMarkdownWorkWidthClassification(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "plain prose", content: "A paragraph with `inline code`.", want: false},
		{name: "pipe in prose", content: "Choose red | blue in prose.", want: false},
		{name: "fenced code", content: "```go\nfmt.Println()\n```", want: true},
		{name: "tilde fence", content: "  ~~~sh\nmake test\n  ~~~", want: true},
		{name: "indented code", content: "Paragraph\n\n    make test", want: true},
		{name: "table", content: "Name | State\n--- | :---:\nA | ready", want: true},
		{name: "not a table delimiter", content: "Name | State\none | two", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := markdownUsesWorkWidth(test.content); got != test.want {
				t.Fatalf("markdownUsesWorkWidth(%q) = %t, want %t", test.content, got, test.want)
			}
		})
	}
}

func widestDisplayLine(content string) int {
	widest := 0
	for _, line := range strings.Split(content, "\n") {
		widest = max(widest, lipgloss.Width(line))
	}
	return widest
}

func TestFindSafeMarkdownBoundary(t *testing.T) {
	tests := []struct {
		name    string
		content string
		// wantPrefix is an explicit fixture, independent from the production
		// parser. An empty prefix means no safe boundary.
		wantPrefix string
	}{
		{
			name:       "no blank line yet",
			content:    "just a single line still streaming",
			wantPrefix: "",
		},
		{
			name:       "one complete paragraph then partial",
			content:    "First paragraph done.\n\nSecond para still goi",
			wantPrefix: "First paragraph done.",
		},
		{
			name:       "blank line inside open code fence is not a boundary",
			content:    "Intro.\n\n```go\nfunc main() {\n\n\tprintln(1)",
			wantPrefix: "Intro.",
		},
		{
			name:       "closed code fence then blank line is safe",
			content:    "```go\nx := 1\n```\n\nNext partial",
			wantPrefix: "```go\nx := 1\n```",
		},
		{
			name:       "blank line inside open tilde fence is not a boundary",
			content:    "Intro.\n\n~~~\nblock\n\nstill in block",
			wantPrefix: "Intro.",
		},
		{
			name:       "inline code backticks are not a fence",
			content:    "Use `go test` to run.\n\nNext partial paragraph",
			wantPrefix: "Use `go test` to run.",
		},
		{
			name:       "consecutive blank lines pick the latest safe boundary",
			content:    "One.\n\nTwo.\n\nThree partial",
			wantPrefix: "One.\n\nTwo.",
		},
		{
			name:       "second closed fence then blank is safe",
			content:    "```\na\n```\n\ntext\n\n```\nb\n```\n\ntail",
			wantPrefix: "```\na\n```\n\ntext\n\n```\nb\n```",
		},
		{
			name:       "up to three spaces of fence indentation are accepted",
			content:    "   ````go\ncode\n   `````\n\nTail partial",
			wantPrefix: "   ````go\ncode\n   `````",
		},
		{
			name:       "four spaces do not open a fence",
			content:    "    ```go\nordinary text\n\nTail partial",
			wantPrefix: "    ```go\nordinary text",
		},
		{
			name:       "closing run shorter than opener stays inside fence",
			content:    "Intro.\n\n````\ncode\n```\n\nstill code",
			wantPrefix: "Intro.",
		},
		{
			name:       "closing run may be longer than opener",
			content:    "````\ncode\n``````\n\nTail partial",
			wantPrefix: "````\ncode\n``````",
		},
		{
			name:       "closing fence rejects trailing content",
			content:    "Intro.\n\n```\ncode\n``` not a close\n\nstill code",
			wantPrefix: "Intro.",
		},
		{
			name:       "different marker character cannot close fence",
			content:    "Intro.\n\n```\ncode\n~~~\n\nstill code",
			wantPrefix: "Intro.",
		},
		{
			name:       "backtick in backtick info string prevents opening",
			content:    "```lang`variant\nordinary text\n\nTail partial",
			wantPrefix: "```lang`variant\nordinary text",
		},
		{
			name:       "backticks are allowed in tilde info string",
			content:    "~~~ markdown`variant\ncode\n~~~\n\nTail partial",
			wantPrefix: "~~~ markdown`variant\ncode\n~~~",
		},
		{
			name:       "closer indented by four spaces stays inside fence",
			content:    "Intro.\n\n```\ncode\n    ```\n\nstill code",
			wantPrefix: "Intro.",
		},
		{
			name:       "closing fence accepts trailing horizontal whitespace",
			content:    "```\ncode\n``` \t\r\n\nTail partial",
			wantPrefix: "```\ncode\n``` \t\r",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := findSafeMarkdownBoundary(tt.content)
			if tt.wantPrefix == "" {
				if b != 0 {
					t.Fatalf("expected no boundary (0), got %d (prefix=%q)", b, tt.content[:b])
				}
				return
			}
			if b <= 0 {
				t.Fatalf("expected a boundary, got %d", b)
			}
			got := tt.content[:b]
			if got != tt.wantPrefix {
				t.Fatalf("prefix mismatch:\n got: %q\nwant: %q", got, tt.wantPrefix)
			}
		})
	}
}

func TestMarkdownFenceStateRemembersOpeningMarker(t *testing.T) {
	var state markdownFenceState
	state.applyLine("```go")
	if !state.open || state.char != '`' || state.length != 3 {
		t.Fatalf("three-backtick opener state = %+v", state)
	}
	state.applyLine("~~~~")
	state.applyLine("``")
	state.applyLine("``` trailing")
	if !state.open || state.char != '`' || state.length != 3 {
		t.Fatalf("incompatible closers changed state = %+v", state)
	}
	state.applyLine("````")
	if state.open || state.char != 0 || state.length != 0 {
		t.Fatalf("compatible longer closer did not reset state = %+v", state)
	}
}
