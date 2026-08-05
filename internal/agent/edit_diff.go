package agent

import (
	"fmt"
	"strings"
)

// A write or an edit reported only "Written to path (N bytes)". Under NORMAL
// authority the approval modal showed the diff before the change landed, so
// the number was enough — you had already seen what it was going to do. Under
// AUTO nothing prompts, so a whole session of edits scrolled past as byte
// counts and the only way to see what actually changed was `git diff`
// afterwards, by which point several turns had landed on top of each other.
//
// The receipt now carries the shape of the change: how many lines were added
// and removed, and for a small edit the changed lines themselves. That is the
// question a reader is actually asking when an agent says it edited a file.
//
// This is a summary, deliberately, not a second diff renderer. The full
// unified diff already has one — approvalDiff for the modal and DiffViewer for
// the expandable surface — and a receipt line that reproduced it would push
// the conversation off the screen on every edit.

const (
	// maxInlineDiffLines bounds what a receipt shows before it stops being a
	// receipt. A change larger than this is summarised by its counts; the full
	// text stays available through the expandable detail.
	maxInlineDiffLines = 12
	// maxInlineDiffLineBytes keeps one changed line on one row. A minified
	// bundle is a legitimate edit target and a single line of it would
	// otherwise fill the transcript.
	maxInlineDiffLineBytes = 120
)

// editChangeSummary describes one applied change in the terms a reader cares
// about. Counts are always present; Lines is populated only for a change small
// enough to read at a glance.
type editChangeSummary struct {
	Added   int
	Removed int
	Lines   []string
}

// String renders the receipt suffix, or "" when nothing changed.
func (s editChangeSummary) String() string {
	if s.Added == 0 && s.Removed == 0 {
		return ""
	}
	head := fmt.Sprintf("+%d/-%d", s.Added, s.Removed)
	if len(s.Lines) == 0 {
		return head
	}
	return head + "\n" + strings.Join(s.Lines, "\n")
}

// summarizeEditChange compares the file before and after.
//
// The line walk is a longest-common-subsequence-free scan: it finds the
// matching prefix and suffix and treats everything between as the change. That
// is exact for the counts at the edges and slightly over-counts a change with
// unmodified lines in its middle, which is the right trade for a receipt —
// it never under-reports how much was touched, and the authoritative diff is
// one keystroke away.
func summarizeEditChange(before, after string) editChangeSummary {
	if before == after {
		return editChangeSummary{}
	}
	oldLines := splitEditLines(before)
	newLines := splitEditLines(after)

	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	removed := oldLines[prefix : len(oldLines)-suffix]
	added := newLines[prefix : len(newLines)-suffix]
	summary := editChangeSummary{Added: len(added), Removed: len(removed)}

	if len(removed)+len(added) > maxInlineDiffLines {
		return summary
	}
	for _, line := range removed {
		summary.Lines = append(summary.Lines, "  - "+boundEditLine(line))
	}
	for _, line := range added {
		summary.Lines = append(summary.Lines, "  + "+boundEditLine(line))
	}
	return summary
}

// splitEditLines splits without inventing a trailing empty line, so appending
// to a file that ends in a newline does not read as two changes.
func splitEditLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// boundEditLine keeps one changed line on one row. Control characters are
// stripped: this text is model-supplied and lands in a terminal, where an
// escape sequence would repaint the frame around it.
func boundEditLine(line string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, line)
	if len(cleaned) <= maxInlineDiffLineBytes {
		return cleaned
	}
	return truncateUTF8Bytes(cleaned, maxInlineDiffLineBytes) + "…"
}
