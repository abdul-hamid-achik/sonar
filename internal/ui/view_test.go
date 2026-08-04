package ui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{999, "999"},
		{1000, "1.0k"},
		{1234, "1.2k"},
		{8192, "8.2k"},
		{0, "0"},
		{500, "500"},
		{10000, "10.0k"},
		{999_999, "1000.0k"},
		{1_000_000, "1.0M"},
		{1_048_576, "1.0M"},
		{2_500_000, "2.5M"},
	}

	for _, tt := range tests {
		got := formatTokens(tt.input)
		if got != tt.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  string
	}{
		{42 * time.Millisecond, "42ms"},
		{1300 * time.Millisecond, "1.3s"},
		{0, "0ms"},
		{999 * time.Millisecond, "999ms"},
		{time.Second, "1.0s"},
		{2500 * time.Millisecond, "2.5s"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.input)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTruncateDisplay(t *testing.T) {
	tests := []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{"within_limit", "hello", 10, "hello"},
		{"exact_limit", "hello", 5, "hello"},
		{"ascii_over_limit", "hello world", 8, "hello w…"},
		{"cjk_uses_cell_width", "模型abcdef", 5, "模型…"},
		{"zwj_emoji_stays_whole", "A👨‍👩‍👧‍👦BC", 4, "A👨‍👩‍👧‍👦…"},
		{"combining_mark_stays_attached", "e\u0301clair", 4, "e\u0301cl…"},
		{"cluster_wider_than_budget_is_not_split", "界a", 2, "…"},
		{"one_cell_budget", "模型", 1, "…"},
		{"zero_width_budget", "hello", 0, ""},
		{"negative_width_budget", "hello", -1, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateDisplay(tt.input, tt.max)
			if got != tt.want {
				t.Errorf("truncateDisplay(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncateDisplay returned invalid UTF-8: %q", got)
			}
			if tt.max > 0 && lipgloss.Width(got) > tt.max {
				t.Fatalf("truncateDisplay width = %d, want <= %d: %q", lipgloss.Width(got), tt.max, got)
			}
		})
	}
}

func TestWrapText(t *testing.T) {
	t.Run("no_wrap_needed", func(t *testing.T) {
		got := wrapText("short", 20)
		if got != "short" {
			t.Errorf("expected 'short', got %q", got)
		}
	})

	t.Run("word_wrap", func(t *testing.T) {
		got := wrapText("hello world foo bar", 11)
		lines := strings.Split(got, "\n")
		for _, line := range lines {
			if len(line) > 11 {
				t.Errorf("line %q exceeds width 11", line)
			}
		}
	})

	t.Run("preserves_newlines", func(t *testing.T) {
		got := wrapText("line1\nline2", 20)
		if !strings.Contains(got, "\n") {
			t.Error("should preserve existing newlines")
		}
		lines := strings.Split(got, "\n")
		if len(lines) < 2 {
			t.Errorf("expected at least 2 lines, got %d", len(lines))
		}
	})

	t.Run("width_zero_guard", func(t *testing.T) {
		got := wrapText("hello", 0)
		if got != "hello" {
			t.Errorf("width<=0 should return original, got %q", got)
		}
	})

	t.Run("width_negative", func(t *testing.T) {
		got := wrapText("hello", -1)
		if got != "hello" {
			t.Errorf("negative width should return original, got %q", got)
		}
	})
}

func TestContextUsageInComposerStatus(t *testing.T) {
	// Context meter lives on the session top bar when chrome is active. Footer
	// status only repeats it without a header, or when pressure is high.
	withConversation := func(m *Model) {
		m.entries = []ChatEntry{{Kind: "user", Content: "hello"}}
	}

	t.Run("no_pct_when_zero_tokens", func(t *testing.T) {
		m := newTestModel(t)
		withConversation(m)
		m.model = "test-model"
		m.promptTokens = 0
		m.numCtx = 8192
		// Header owns ambient 0%; idle footer stays quiet on that signal.
		status := m.renderStatusLine()
		if strings.Contains(status, "0%") || strings.Contains(status, "ctx") {
			t.Errorf("header-active idle footer should not re-print ambient context, got %q", status)
		}
	})

	t.Run("no_pct_when_zero_numCtx", func(t *testing.T) {
		m := newTestModel(t)
		withConversation(m)
		m.model = "test-model"
		m.promptTokens = 1000
		m.numCtx = 0
		status := m.renderStatusLine()
		if strings.Contains(status, "ctx") || strings.Contains(status, "%") {
			t.Error("should not show context usage when numCtx is 0")
		}
	})

	t.Run("correct_percentage_without_header", func(t *testing.T) {
		m := newTestModel(t)
		withConversation(m)
		// Short frame skips the session header; roomy width keeps ambient meter.
		m.width, m.height = 80, 13
		m.model = "test-model"
		m.promptTokens = 4096
		m.numCtx = 8192
		status := m.renderStatusLine()
		// Zero-style absolute used/limit · percent.
		if !strings.Contains(status, "4.1k/8.2k · 50%") && !strings.Contains(status, "50%") {
			t.Errorf("expected status to contain context usage, got %q", status)
		}
	})

	t.Run("high_percentage", func(t *testing.T) {
		m := newTestModel(t)
		withConversation(m)
		m.model = "test-model"
		m.promptTokens = 7500
		m.numCtx = 8192
		status := m.renderStatusLine()
		if !strings.Contains(status, "91%") {
			t.Errorf("expected status to contain high context usage, got %q", status)
		}
		// High context outranks model; model may be omitted when header owns it.
		if ctxAt := strings.Index(status, "91%"); ctxAt < 0 {
			t.Errorf("high context warning missing, got %q", status)
		}
	})

	t.Run("cloud_uses_effective_window", func(t *testing.T) {
		m := newTestModel(t)
		withConversation(m)
		// No session header: footer carries ambient absolute meter.
		m.width, m.height = 80, 13
		m.model = "kimi-k2.7-code:cloud"
		m.promptTokens = 120_351
		m.numCtx = 1_048_576
		status := m.renderStatusLine()
		if !strings.Contains(status, "11%") || strings.Contains(status, "100%") {
			t.Fatalf("cloud context status = %q, want 11%%", status)
		}
	})

	t.Run("ambient_context_at_zero_used", func(t *testing.T) {
		m := newTestModel(t)
		m.model = "test-model"
		m.promptTokens = 0
		m.numCtx = 8192
		// Absolute meter is ambient on the top bar / context helper.
		got := m.renderContextStatus()
		if !strings.Contains(ansi.Strip(got), "0/8.2k · 0%") && !strings.Contains(ansi.Strip(got), "0%") {
			t.Fatalf("ambient context = %q", got)
		}
	})

	t.Run("empty_state_footer_stays_quiet_with_header", func(t *testing.T) {
		m := newTestModel(t)
		m.model = "test-model"
		m.promptTokens = 0
		m.numCtx = 8192
		if status := m.renderStatusLine(); status != "" {
			t.Fatalf("empty-state status should be quiet when welcome+header own identity, got %q", ansi.Strip(status))
		}
	})
}

func TestComposerStatusProjectsActiveSessionHandleAndTitle(t *testing.T) {
	m := newTestModel(t)
	m.sessionID = 7
	m.sessionPublicID = "aaaaaa7"
	m.activeSessionTitle = "Polish composer wrapping"
	m.entries = []ChatEntry{{Kind: "user", Content: "polish the composer"}}

	status := m.renderStatusLine()
	if !strings.Contains(status, "aaaaaa7 · Polish composer wrapping") {
		t.Fatalf("status omitted active session identity: %q", status)
	}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	status = m.renderStatusLine()
	if !strings.Contains(status, "aaaaaa7") || strings.Contains(status, "Polish composer wrapping") {
		t.Fatalf("compact status session identity = %q, want handle without title", status)
	}
}

// The plain and offset-carrying views of wrapping are the same algorithm. They
// used to be two implementations that had to stay identical for the
// incremental paint path to keep matching the canonical render, with nothing
// enforcing it.
func TestWrappingViewsAgreeOnRowsAndOffsets(t *testing.T) {
	lines := []string{
		"short",
		"",
		"   ",
		strings.Repeat("word ", 40),
		"a very-long-unbreakable-token-that-must-be-split-across-several-rows-exactly",
		"界 wide 🙂 runes 界界界 mixed with ordinary words to force packing",
		strings.Repeat("界", 50),
	}
	for _, width := range []int{1, 2, 7, 20, 80} {
		for _, line := range lines {
			chunks, _ := wrapLineChunks(line, width, false)

			rows := make([]string, len(chunks))
			for i := range chunks {
				rows[i] = chunks[i].text
			}
			if got, want := strings.Join(rows, "\n"), wrapLine(line, width); got != want {
				t.Fatalf("width %d: wrapLine disagrees with wrapLineChunks:\n got %q\nwant %q", width, got, want)
			}

			// Offsets must be non-decreasing and inside the source line, or the
			// live tail would resume from a position that is not a boundary.
			previous := -1
			for i, chunk := range chunks {
				if chunk.rawStart < previous {
					t.Fatalf("width %d line %q: chunk %d offset %d went backwards from %d",
						width, line, i, chunk.rawStart, previous)
				}
				if chunk.rawStart > len(line) {
					t.Fatalf("width %d line %q: chunk %d offset %d exceeds line length %d",
						width, line, i, chunk.rawStart, len(line))
				}
				previous = chunk.rawStart
			}

			// A row may exceed the requested width only when it is a single
			// grapheme cluster that is itself wider — a two-cell CJK rune
			// cannot be split to fit a one-column budget.
			if lipgloss.Width(line) > width && width > 0 {
				for i, row := range rows {
					if lipgloss.Width(row) <= width {
						continue
					}
					graphemes := uniseg.NewGraphemes(row)
					count := 0
					for graphemes.Next() {
						count++
					}
					if count > 1 {
						t.Fatalf("width %d: row %d is %d cells across %d clusters: %q",
							width, i, lipgloss.Width(row), count, row)
					}
				}
			}
		}
	}
}

// splitDisplayChunks is the text projection of the offset-aware splitter.
func TestSplitDisplayChunksProjectsTheOffsetAwareSplitter(t *testing.T) {
	for _, word := range []string{"", "abc", strings.Repeat("x", 33), "界界界🙂界"} {
		for _, width := range []int{1, 3, 8} {
			withOffsets := splitDisplayChunksAt(word, 5, width)
			plain := splitDisplayChunks(word, width)
			if len(withOffsets) != len(plain) {
				t.Fatalf("word %q width %d: %d chunks vs %d", word, width, len(withOffsets), len(plain))
			}
			for i := range plain {
				if plain[i] != withOffsets[i].text {
					t.Fatalf("word %q width %d chunk %d: %q vs %q", word, width, i, plain[i], withOffsets[i].text)
				}
				if withOffsets[i].rawStart < 5 {
					t.Fatalf("word %q width %d chunk %d: offset %d ignores the word start", word, width, i, withOffsets[i].rawStart)
				}
			}
		}
	}
}
