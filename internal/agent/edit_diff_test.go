package agent

import (
	"strings"
	"testing"
)

// Under AUTO nothing prompts, so the tool receipt is the only place a change is
// visible while it happens. "Written to x (500 bytes)" is compatible with
// having deleted three hundred lines.
func TestEditChangeSummaryReportsWhatChanged(t *testing.T) {
	tests := []struct {
		name          string
		before, after string
		wantAdded     int
		wantRemoved   int
		wantLines     []string
	}{
		{
			name:      "one line replaced",
			before:    "alpha\nbravo\ncharlie\n",
			after:     "alpha\nBRAVO\ncharlie\n",
			wantAdded: 1, wantRemoved: 1,
			wantLines: []string{"  - bravo", "  + BRAVO"},
		},
		{
			name:      "pure append",
			before:    "alpha\n",
			after:     "alpha\nbravo\n",
			wantAdded: 1, wantRemoved: 0,
			wantLines: []string{"  + bravo"},
		},
		{
			name:      "pure deletion",
			before:    "alpha\nbravo\n",
			after:     "alpha\n",
			wantAdded: 0, wantRemoved: 1,
			wantLines: []string{"  - bravo"},
		},
		{
			name:   "identical content is not a change",
			before: "alpha\nbravo\n",
			after:  "alpha\nbravo\n",
		},
		{
			// The case the byte count hides.
			name:      "an overwrite that deletes most of the file",
			before:    strings.Repeat("keep\n", 300),
			after:     "keep\n",
			wantAdded: 0, wantRemoved: 299,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := summarizeEditChange(test.before, test.after)
			if got.Added != test.wantAdded || got.Removed != test.wantRemoved {
				t.Fatalf("+%d/-%d, want +%d/-%d", got.Added, got.Removed, test.wantAdded, test.wantRemoved)
			}
			if test.wantLines == nil {
				if len(got.Lines) != 0 {
					t.Errorf("unexpected inline lines: %v", got.Lines)
				}
				return
			}
			if strings.Join(got.Lines, "\n") != strings.Join(test.wantLines, "\n") {
				t.Errorf("lines =\n%s\nwant\n%s", strings.Join(got.Lines, "\n"), strings.Join(test.wantLines, "\n"))
			}
		})
	}
}

// A receipt that reproduced a large diff would push the conversation off the
// screen on every edit. Past the threshold the counts stand alone.
func TestLargeChangesReportCountsWithoutTheBody(t *testing.T) {
	before := ""
	after := strings.Repeat("new line\n", maxInlineDiffLines+1)
	summary := summarizeEditChange(before, after)

	if summary.Added != maxInlineDiffLines+1 {
		t.Fatalf("added = %d, want %d", summary.Added, maxInlineDiffLines+1)
	}
	if len(summary.Lines) != 0 {
		t.Errorf("a large change inlined %d lines", len(summary.Lines))
	}
	if rendered := summary.String(); !strings.HasPrefix(rendered, "+13/-0") || strings.Contains(rendered, "new line") {
		t.Errorf("rendered = %q", rendered)
	}
}

// The changed text is model-supplied and lands in a terminal. An escape
// sequence in an edited line would repaint the frame around the receipt.
func TestInlineDiffLinesAreTerminalSafeAndBounded(t *testing.T) {
	hostile := "before\x1b[31mred\x07\ttabbed"
	summary := summarizeEditChange("", hostile+"\n")
	if len(summary.Lines) != 1 {
		t.Fatalf("lines = %v", summary.Lines)
	}
	line := summary.Lines[0]
	if strings.ContainsAny(line, "\x1b\x07\n\r\t") {
		t.Errorf("control characters survived: %q", line)
	}
	if !strings.Contains(line, "red") || !strings.Contains(line, "tabbed") {
		t.Errorf("stripping removed real content: %q", line)
	}

	long := summarizeEditChange("", strings.Repeat("x", maxInlineDiffLineBytes*3)+"\n")
	if got := len(long.Lines[0]); got > maxInlineDiffLineBytes+8 {
		t.Errorf("line is %d bytes, want it bounded near %d", got, maxInlineDiffLineBytes)
	}
}

// A file that ends in a newline must not read as having a trailing empty line
// added or removed, or every append reports one change too many.
func TestTrailingNewlineIsNotAPhantomChange(t *testing.T) {
	if got := summarizeEditChange("alpha\n", "alpha\nbravo\n"); got.Added != 1 || got.Removed != 0 {
		t.Errorf("append with trailing newline = +%d/-%d, want +1/-0", got.Added, got.Removed)
	}
	if got := summarizeEditChange("alpha", "alpha\n"); got.Added != 0 || got.Removed != 0 {
		t.Errorf("adding a final newline reported +%d/-%d", got.Added, got.Removed)
	}
	// CRLF input must not make every line read as changed.
	if got := summarizeEditChange("alpha\r\nbravo\r\n", "alpha\nbravo\n"); got.Added != 0 || got.Removed != 0 {
		t.Errorf("line-ending normalisation reported +%d/-%d", got.Added, got.Removed)
	}
}

// An unchanged write still reports its size; the summary only adds the change.
func TestNoChangeRendersEmpty(t *testing.T) {
	if got := (editChangeSummary{}).String(); got != "" {
		t.Errorf("empty summary rendered %q", got)
	}
}
