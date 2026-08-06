package ui

import (
	"regexp"
	"strings"
	"unicode"
)

// Spoken projection: what a coding-agent transcript sounds like when it is
// worth hearing.
//
// This is the feature, not the synthesizer. The same assistant sentence
// measures 12.9 seconds spoken raw and 6.1 seconds spoken projected, and the
// difference is entirely a URL and a file path being spelled out one character
// at a time. Screen readers have not solved reading technical text aloud in
// decades of trying; the answer is not better phrasing, it is saying less.
//
// Three rules, in the order they matter:
//
//  1. Never speak what a listener cannot act on. A full path, a URL, a diff
//     hunk, a hex digest. Collapse it to the one part that identifies it, or
//     drop it.
//  2. Never split inside a fence. A naive sentence regex cuts a code block in
//     half and reads the fragments as prose, which is the failure mode every
//     existing implementation of this reports.
//  3. Never guess at a pause. A sentence is spoken when it is complete, which
//     during streaming means the text after the last boundary is held back
//     until more arrives.

var (
	// A fenced block, kept whole so it can be removed whole.
	spokenFencePattern = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~")
	// Inline code, links, and bare URLs, each collapsed rather than spelled.
	spokenInlineCode   = regexp.MustCompile("`([^`]*)`")
	spokenMarkdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	spokenBareURL      = regexp.MustCompile(`https?://\S+`)
	// Leading markup a reader sees and a listener does not need.
	spokenLeadingMarkup = regexp.MustCompile(`(?m)^\s*(#{1,6}\s+|[-*+]\s+|\d+\.\s+|>\s*)`)
	spokenEmphasis      = regexp.MustCompile(`\*\*|__|\*|_{2,}`)
	// Any token carrying a slash. Whether it is a PATH is decided by
	// slashIsAPath, not here: a regex that tried would also have to know that
	// "5/8/2026" is a date.
	spokenPathToken = regexp.MustCompile(`\S*/\S+`)
	// Long hex runs — digests, object ids, session handles.
	spokenHexRun     = regexp.MustCompile(`\b[0-9a-f]{7,}\b`)
	spokenWhitespace = regexp.MustCompile(`\s+`)
)

// spokenText projects one assistant message into something worth hearing.
//
// It is deliberately lossy and says so: the transcript on screen remains the
// complete record, and a listener who needs the exact path reads it. Trying to
// keep both would produce the 12.9-second version nobody listens to twice.
func spokenText(markdown string) string {
	text := withoutUnfinishedMarkup(sanitizeTerminalMultiline(markdown))
	// Fences first: everything inside one is code, and code is never spoken.
	// Replacing rather than deleting keeps a sentence from fusing with the one
	// on the far side of a block.
	text = spokenFencePattern.ReplaceAllString(text, ". ")
	text = spokenMarkdownLink.ReplaceAllString(text, "$1")
	text = spokenBareURL.ReplaceAllString(text, "a link")
	// Inline code is usually an identifier or a path. Its CONTENT is what a
	// listener wants, reduced the same way a bare path is.
	text = spokenInlineCode.ReplaceAllStringFunc(text, func(match string) string {
		return " " + spokenPath(strings.Trim(match, "`")) + " "
	})
	text = spokenPathToken.ReplaceAllStringFunc(text, spokenPath)
	text = spokenHexRun.ReplaceAllString(text, "a digest")
	text = spokenLeadingMarkup.ReplaceAllString(text, "")
	text = spokenEmphasis.ReplaceAllString(text, "")
	text = spokenWhitespace.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

// slashIsAPath reports whether a slash-bearing token is a path at all.
//
// Reducing a token to its last segment is right for
// internal/agent/auto_command.go and wrong for most other things people write
// with a slash. Measured against ordinary prose, the reducer turned "5/8/2026"
// into "2026", "1/2" into "2", "3/4" into "4" and "y/o" into "o" — a date, two
// fractions and a conjunction, each reduced to the half that carries the least
// meaning, and the last of them reversing what the sentence said.
//
// Two shapes are excluded, both by what they are rather than by a list:
// segments that are all digits are an amount, a ratio or a date; and a pair of
// single characters is a conjunction (y/o, s/n, and/or is caught by neither and
// is accepted as the cost of a rule this small).
func slashIsAPath(token string) bool {
	segments := strings.Split(token, "/")
	numeric, named := true, false
	for _, segment := range segments {
		if segment == "" {
			// "/" and "dir/" have empty segments and are still paths. Only what
			// a segment CONTAINS can disqualify a token.
			continue
		}
		named = true
		if strings.ContainsFunc(segment, func(r rune) bool { return !unicode.IsDigit(r) }) {
			numeric = false
			break
		}
	}
	if named && numeric {
		return false
	}
	if len(segments) == 2 &&
		len([]rune(segments[0])) == 1 && len([]rune(segments[1])) == 1 {
		return false
	}
	return true
}

// withoutUnfinishedMarkup drops a span whose closing delimiter has not arrived.
//
// This is rule 2 applied to streaming, and it is not a refinement — without it
// the answer channel reads source code aloud. A fence is only recognizable once
// both halves exist, so while one streams, every line of the block is ordinary
// prose to the projection: a comment ending in a period, or `foo.` at the end
// of a line, is spoken as a sentence. Then the closing fence lands, the block
// collapses, and the projection is suddenly SHORTER than what was already read
// out. Holding the tail back until it closes costs one chunk of latency on a
// block nobody wanted to hear anyway.
//
// The markers are handled longest-first so that the three backticks of a fence
// are consumed as a fence rather than counted as inline spans.
func withoutUnfinishedMarkup(text string) string {
	for _, marker := range []string{"```", "~~~", "`"} {
		if strings.Count(text, marker)%2 == 0 {
			continue
		}
		if index := strings.LastIndex(text, marker); index >= 0 {
			text = text[:index]
		}
	}
	return text
}

// spokenPath reduces a path to what identifies it out loud.
//
// "internal/agent/auto_command.go:852" becomes "auto command dot go" — the
// name a listener recognizes, without the directories they cannot hear or the
// line number they cannot use. Underscores become spaces because every
// synthesizer reads them as "underscore", and a Go file name is otherwise a
// string of run-together words.
func spokenPath(token string) string {
	trimmed := strings.Trim(token, "`'\"(),;")
	if trimmed == "" {
		return token
	}
	// A line/column suffix carries nothing audible.
	if index := strings.IndexByte(trimmed, ':'); index > 0 {
		trimmed = trimmed[:index]
	}
	if strings.ContainsRune(trimmed, '/') && !slashIsAPath(trimmed) {
		return token
	}
	base := trimmed
	if index := strings.LastIndexByte(base, '/'); index >= 0 {
		base = base[index+1:]
	}
	if base == "" {
		return "a path"
	}
	if !strings.ContainsFunc(base, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}) {
		// Punctuation with no name in it — "./..." is the whole Go tree, and a
		// listener hearing "go test" already knows which tree. Saying "dot dot
		// dot" adds a second of noise and no information.
		return ""
	}
	base = strings.ReplaceAll(base, "_", " ")
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, ".", " dot ")
	return strings.TrimSpace(spokenWhitespace.ReplaceAllString(base, " "))
}

// spokenSentences splits projected text into complete sentences and returns the
// remainder that is not yet complete.
//
// The remainder is the point. During streaming the text after the last boundary
// may still be growing, and speaking it would mean saying half a clause and
// then the other half as if it were new. The caller holds it and offers it
// again with whatever arrives next.
func spokenSentences(text string) (sentences []string, remainder string) {
	runes := []rune(text)
	start := 0
	for index := 0; index < len(runes); index++ {
		if !isSentenceTerminator(runes[index]) {
			continue
		}
		// A terminator ends a sentence only when whitespace follows. This is
		// what keeps "1.5" and "v0.4.2" whole, and it is why the projection
		// above turns file extensions into " dot " before this runs.
		next := index + 1
		for next < len(runes) && isSentenceTerminator(runes[next]) {
			next++
		}
		if next < len(runes) && runes[next] != ' ' && runes[next] != '\n' && runes[next] != '\t' {
			continue
		}
		sentence := strings.TrimSpace(string(runes[start:next]))
		if sentence != "" {
			sentences = append(sentences, sentence)
		}
		start = next
	}
	return sentences, strings.TrimSpace(string(runes[start:]))
}

func isSentenceTerminator(r rune) bool {
	return r == '.' || r == '!' || r == '?'
}

// spokenActivity is the one-line form of what the harness is doing.
//
// It reuses the labels the transcript already paints — tool_presentation.go
// maps every tool to prose, and a collapsed read run already summarizes itself
// as "Read 4 files" — so the spoken and the visible surfaces cannot describe
// the same work differently. What it drops is the target: "reading" is worth
// hearing, the path it is reading is not.
func spokenActivity(label, summary string) string {
	label = strings.TrimSpace(sanitizeTerminalSingleLine(label))
	if label == "" {
		return ""
	}
	if summary = strings.TrimSpace(sanitizeTerminalSingleLine(summary)); summary != "" {
		if projected := spokenPath(summary); projected != "" && projected != summary {
			return label + " " + projected
		}
	}
	return label
}
