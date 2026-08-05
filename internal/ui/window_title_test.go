package ui

import "testing"

// Session titles are model-generated prose and arrive with markdown in them.
// A real session produced the title "```", which reached a terminal tab
// verbatim — the fallback to the product name never fired because the string
// was non-empty.
func TestWindowTitleStripsMarkdownFromSessionTitles(t *testing.T) {
	for _, test := range []struct {
		name string
		in   string
		want string
	}{
		{"bare code fence", "```", ""},
		{"tilde fence", "~~~", ""},
		{"fence with language", "```go", "go"},
		{"backticked identifier", "Wire up `providers refresh`", "Wire up providers refresh"},
		{"heading punctuation", "## Fix the parser", "Fix the parser"},
		{"emphasis", "**urgent** cleanup", "urgent cleanup"},
		{"blockquote", "> quoted title", "quoted title"},
		{"punctuation only", " ** ## `` ", ""},
		{"collapses whitespace", "too   many\tspaces", "too many spaces"},
		{"empty", "", ""},
		{"plain title untouched", "Investigating the theme system", "Investigating the theme system"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := windowTitleFromSessionTitle(test.in); got != test.want {
				t.Errorf("windowTitleFromSessionTitle(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

// A title made only of markup must fall through to the product name rather
// than render an empty or punctuation-only tab.
func TestWindowTitleFallsBackWhenNothingSurvives(t *testing.T) {
	m := &Model{activeSessionTitle: "```"}
	base := m.windowTitleBase()
	if base == "" {
		t.Fatal("window title is empty")
	}
	if base == "```" {
		t.Fatal("raw markdown reached the window title")
	}
}

// Long titles must not push the workspace name out of a narrow tab.
func TestWindowTitleIsBounded(t *testing.T) {
	long := ""
	for range 40 {
		long += "verylongword "
	}
	got := windowTitleFromSessionTitle(long)
	if len([]rune(got)) > 48 {
		t.Errorf("title is %d runes, want at most 48", len([]rune(got)))
	}
}
