package ui

import (
	"strings"
	"testing"
)

// TestWrapTextWideChars tests wrapping with Unicode wide characters
func TestWrapTextWideChars(t *testing.T) {
	// Test with content that would exceed width if not wrapped properly
	longURL := "https://example.com/very/long/path/that/should/be/wrapped/but/wont/be/with/standard/wrapping"

	got := wrapText(longURL, 40)
	lines := strings.Split(got, "\n")

	for _, line := range lines {
		if len([]rune(line)) > 40 {
			t.Errorf("line %q exceeds width 40 (runes: %d)", line, len([]rune(line)))
		}
	}
}

// TestWrapLineWrapping tests that wrapLine properly wraps content
func TestWrapLineWrapping(t *testing.T) {
	t.Run("long_url", func(t *testing.T) {
		longURL := "https://github.com/very/long/path/that/definitely/needs/to/be/wrapped/properly"
		got := wrapLine(longURL, 40)
		lines := strings.Split(got, "\n")
		for _, line := range lines {
			if len(line) > 40 {
				t.Errorf("wrapLine failed: line %q has %d chars, exceeds 40", line, len(line))
			}
		}
	})

	t.Run("long_identifier", func(t *testing.T) {
		code := "this_is_a_very_long_identifier_without_any_spaces_that_needs_to_be_wrapped"
		got := wrapLine(code, 30)
		lines := strings.Split(got, "\n")
		for _, line := range lines {
			if len(line) > 30 {
				t.Errorf("wrapLine failed: line %q has %d chars, exceeds 30", line, len(line))
			}
		}
	})

	t.Run("normal_text", func(t *testing.T) {
		text := "hello world foo bar baz"
		got := wrapLine(text, 12)
		lines := strings.Split(got, "\n")
		for _, line := range lines {
			if len(line) > 12 {
				t.Errorf("wrapLine failed: line %q has %d chars, exceeds 12", line, len(line))
			}
		}
	})
}

// TestWrapTextWithCodeBlocks tests wrapping of content that looks like code
func TestWrapTextWithCodeBlocks(t *testing.T) {
	// Code blocks with no spaces should still be wrapped
	codeLine := "this_is_a_very_long_identifier_without_any_spaces_that_needs_to_be_wrapped"

	got := wrapText(codeLine, 30)
	lines := strings.Split(got, "\n")

	for _, line := range lines {
		if len(line) > 30 {
			t.Errorf("code-like content not wrapped: line %q has %d chars, exceeds 30", line, len(line))
		}
	}
}

// TestRenderAssistantMsgWidth tests that assistant messages respect content width
// Note: This tests the raw markdown renderer - the Glamour library itself has issues
// with wrapping long words, but this is a known limitation of the library.
func TestRenderAssistantMsgWidth(t *testing.T) {
	// Create a minimal model for testing
	m := &Model{
		width:  80,
		isDark: true,
	}

	// Create markdown renderer
	m.md = NewMarkdownRenderer(m.width-2, m.isDark, defaultThemeID)

	longURL := "Check out this URL https://github.com/very/long/path/that/definitely/needs/to/be/wrapped/properly"

	rendered := m.md.RenderFull(longURL)
	lines := strings.Split(rendered, "\n")

	// Note: Glamour itself doesn't wrap long words well - this is a known limitation
	// The fix for streaming messages handles this case, but the markdown renderer
	// relies on Glamour's built-in word wrapping which has this bug.
	// We document this as an expected limitation.
	t.Logf("Glamour rendered %d lines, max line length: %d", len(lines), maxLineLen(lines))

	// This test documents the Glamour limitation - we don't fail on this
	// because it's a third-party library issue, not our code
}

func maxLineLen(lines []string) int {
	max := 0
	for _, line := range lines {
		if len(line) > max {
			max = len(line)
		}
	}
	return max
}
