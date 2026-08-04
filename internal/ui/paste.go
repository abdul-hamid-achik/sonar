package ui

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	pasteReviewLineThreshold = 10
	pasteReviewByteThreshold = 4 * 1024
	// Bubbles v2 textarea currently caps its backing grid at 10,000 logical
	// rows. Admission must mirror that bound so InsertString never truncates a
	// reviewed payload after consent.
	pasteTextareaMaxLines = 10000
)

// pendingPaste is an immutable admission receipt. The parent computes it once
// before showing a decision, so the prompt and the eventual insertion agree on
// size, capacity, and the Markdown fence that will be used.
type pendingPaste struct {
	Content     string
	Fenced      string
	Lines       int
	Bytes       int
	NeedsReview bool
	PlainFits   bool
	FencedFits  bool
}

type pasteCursorContext struct {
	atLineStart bool
	atLineEnd   bool
}

func assessPaste(content string, cursor pasteCursorContext, currentLength, currentLines, charLimit int) *pendingPaste {
	content = canonicalPasteContent(content)
	fence := markdownFenceFor(content)
	fenced := fencedPasteInsertion(content, fence, cursor)
	return &pendingPaste{
		Content:     content,
		Fenced:      fenced,
		Lines:       strings.Count(content, "\n") + 1,
		Bytes:       len(content),
		NeedsReview: strings.Count(content, "\n")+1 > pasteReviewLineThreshold || len(content) >= pasteReviewByteThreshold,
		PlainFits:   pasteInsertionFits(currentLength, currentLines, content, charLimit),
		FencedFits:  pasteInsertionFits(currentLength, currentLines, fenced, charLimit),
	}
}

// canonicalPasteContent applies the same text policy as the Bubbles textarea
// before admission and review. Doing this once in the parent makes displayed
// line/byte counts match the text that will actually be inserted, collapses a
// platform CRLF sequence to one logical newline, and prevents consent to one
// payload from silently producing a different draft in the child component.
func canonicalPasteContent(content string) string {
	var b strings.Builder
	b.Grow(len(content))
	runes := []rune(content)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		switch {
		case r == utf8.RuneError:
			// Match Bubbles: invalid UTF-8 and literal replacement runes are
			// omitted rather than entering the textarea backing grid.
		case r == '\t':
			b.WriteString("    ")
		case r == '\r':
			if index+1 < len(runes) && runes[index+1] == '\n' {
				index++
			}
			b.WriteByte('\n')
		case r == '\n':
			b.WriteByte('\n')
		case unicode.IsControl(r):
			// Other terminal control characters never become prompt text.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// pasteCursorAt reports whether the insertion point already has line
// boundaries on either side. Textarea rows and columns are logical rune
// indexes, so wrapped display rows do not affect the Markdown wrapper.
func pasteCursorAt(draft string, row, column int) pasteCursorContext {
	lines := strings.Split(draft, "\n")
	if row < 0 || row >= len(lines) {
		return pasteCursorContext{atLineStart: true, atLineEnd: true}
	}
	lineLength := utf8.RuneCountInString(lines[row])
	column = max(0, min(column, lineLength))
	return pasteCursorContext{
		atLineStart: column == 0,
		atLineEnd:   column == lineLength,
	}
}

func pasteCursorAtEnd(draft string) pasteCursorContext {
	lines := strings.Split(draft, "\n")
	return pasteCursorAt(draft, len(lines)-1, utf8.RuneCountInString(lines[len(lines)-1]))
}

func pasteInsertionFits(currentLength, currentLines int, insertion string, charLimit int) bool {
	sanitizedRunes, lineBreaks := sanitizedPasteSize(insertion)
	if max(1, currentLines)+lineBreaks > pasteTextareaMaxLines {
		return false
	}
	if charLimit <= 0 {
		return true
	}
	// Bubbles subtracts its display-width-based Length from CharLimit, then
	// clips the sanitized insertion by rune count. Mirror its default sanitizer
	// here because one tab expands to four spaces before that clip is applied.
	return sanitizedRunes <= charLimit-currentLength
}

func sanitizedPasteSize(insertion string) (runes, lineBreaks int) {
	for _, r := range insertion {
		switch {
		case r == utf8.RuneError:
			// Bubbles drops invalid UTF-8 and literal replacement runes.
		case r == '\t':
			runes += 4
		case r == '\r' || r == '\n':
			runes++
			lineBreaks++
		case unicode.IsControl(r):
			// Other control characters are removed.
		default:
			runes++
		}
	}
	return runes, lineBreaks
}

// fencedPasteInsertion preserves the payload byte-for-byte while ensuring both
// fences occupy complete logical lines, including when insertion happens in the
// middle of an existing line.
func fencedPasteInsertion(content, fence string, cursor pasteCursorContext) string {
	var b strings.Builder
	if !cursor.atLineStart {
		b.WriteByte('\n')
	}
	b.WriteString(fence)
	b.WriteByte('\n')
	b.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(fence)
	if !cursor.atLineEnd {
		b.WriteByte('\n')
	}
	return b.String()
}

// markdownFenceFor returns a backtick fence longer than every run present in
// the payload, so pasted Markdown containing ``` cannot terminate the wrapper.
func markdownFenceFor(content string) string {
	longest := 0
	current := 0
	for _, r := range content {
		if r == '`' {
			current++
			longest = max(longest, current)
			continue
		}
		current = 0
	}
	return strings.Repeat("`", max(3, longest+1))
}

func formatPasteSize(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f KiB", float64(bytes)/1024)
}

func (p *pendingPaste) descriptor() string {
	unit := "lines"
	if p.Lines == 1 {
		unit = "line"
	}
	return fmt.Sprintf("%d %s · %s", p.Lines, unit, formatPasteSize(p.Bytes))
}
