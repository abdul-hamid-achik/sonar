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
	// An HTML comment. Invisible on screen and, until this existed, perfectly
	// audible: the spoken digest travels in one, and a model that wrote it
	// mid-answer rather than at the end had it read out as
	// "less-than exclamation dash dash spoken colon". The digest is extracted
	// from the RAW message before any of this runs, so removing the comment
	// here costs the feature nothing and stops the marker being narrated.
	spokenHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	// Inline code, links, and bare URLs, each collapsed rather than spelled.
	spokenInlineCode   = regexp.MustCompile("`([^`]*)`")
	spokenMarkdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// A URL, without the sentence's own punctuation. Measured: "See
	// https://example.com. Next." collapsed to "See a link Next." — \S+ ate the
	// full stop, so two sentences became one and the boundary between them was
	// gone. Same failure the path reducer had, in a different regex.
	spokenBareURL = regexp.MustCompile(`https?://[^\s]*[^\s.,;:!?)\]]`)
	// Leading markup a reader sees and a listener does not need.
	spokenLeadingMarkup = regexp.MustCompile(`(?m)^\s*(#{1,6}\s+|[-*+]\s+|\d+\.\s+|>\s*)`)
	spokenEmphasis      = regexp.MustCompile(`\*\*|__|_{2,}`)
	// A single asterisk, and only where it is markup rather than arithmetic.
	// Measured: "Compute 2 * 3 = 6." was read as "2 3", because every literal
	// star was stripped. Markdown requires emphasis to hug the word it marks,
	// so touching a non-space is the test — and Go's regexp has no lookbehind,
	// hence the captured neighbour rather than an assertion.
	spokenEmphasisStar = regexp.MustCompile(`(\S)\*|\*(\S)`)
	// Any token carrying a slash. Whether it is a PATH is decided by
	// slashIsAPath, not here: a regex that tried would also have to know that
	// "5/8/2026" is a date.
	spokenPathToken = regexp.MustCompile(`\S*/\S+`)
	// Long hex runs — digests, object ids, session handles.
	spokenHexRun     = regexp.MustCompile(`\b[0-9a-f]{7,}\b`)
	spokenWhitespace = regexp.MustCompile(`\s+`)
	// Punctuation left stranded by a substitution. Replacing an inline span with
	// a padded reduction puts a space before whatever followed it, and "voice
	// dot go ." is both a pause the writer did not ask for and a terminator the
	// sentence splitter has to reach past.
	spokenStrandedPunctuation = regexp.MustCompile(`\s+([.,;:!?])`)
	// Initialisms the synthesizer reads as words. See spokenInitialisms.
	spokenInitialism = regexp.MustCompile(`\b(API|CLI|TUI|IDE|URL)(s?)\b`)
)

// spokenInitialisms are the acronyms that have to be spelled out, and the list
// is short because it was measured rather than guessed.
//
// `say` already spells most of them: rendering "MCP" and "M C P" produces
// audio within 10% of the same length, and CPU, TLS, XML, CSV, JWT, LLM, SQL
// and IO come out byte-identical — it is spelling them already. The ones below
// render 30–50% SHORTER than their spelled form, which is the measurable
// signature of being read as a word: "CLI" at half the length of "C L I" is
// "clee".
//
// The same measurement is what keeps the list from growing: RAM, JSON, YAML,
// REST, GET, POST, AUTO and OK are all read as words too, and every one of them
// is a word. Spelling those out would be the bug in the other direction.
//
// To extend it, measure first — `say -o` writes a file and its size is
// proportional to duration, so the comparison takes one shell loop.
var spokenInitialisms = map[string]string{
	"API": "A P I",
	"CLI": "C L I",
	"TUI": "T U I",
	"IDE": "I D E",
	"URL": "U R L",
}

// spokenAbbreviations do not end a sentence, however much they look like it.
//
// "e.g." and "i.e." are two terminators followed by a space, which is exactly
// the shape of a sentence boundary — so the answer channel spoke "it is forty
// percent faster, for example" as a complete thought and started a new one at
// "1.5ms vs 2.5ms". The clause was cut in half at the point that carried the
// comparison.
var spokenAbbreviations = map[string]bool{
	"e.g": true, "i.e": true, "vs": true, "etc": true, "cf": true,
	"aprox": true, "ej": true, "p.ej": true, "ss": true, "aka": true,
	// Titles, which end in a period and end nothing. "Talk to Mr. Smith."
	// was two sentences, and the second one was a surname.
	"mr": true, "mrs": true, "ms": true, "dr": true, "dra": true,
	"sr": true, "sra": true, "srta": true, "prof": true, "ing": true,
	"lic": true, "av": true, "no": true, "núm": true, "fig": true,
}

// spokenDigestPattern finds the closing line a model writes to be heard.
//
// An HTML comment, and the container is the design. The transcript has to stay
// the complete record — that rule is what keeps the projection honest about
// being lossy — so the digest cannot be stripped out of the text. But it is
// written for a listener and reads as duplication to a reader. A comment is
// both: present in the raw message, and rendered as nothing. Verified against
// this project's own Glamour configuration rather than assumed.
var spokenDigestPattern = regexp.MustCompile(`(?s)<!--\s*spoken:(.*?)-->`)

// spokenDigest returns the model's own summary for the ear, if it wrote one.
//
// The last one wins. A model that emits two has changed its mind, and the later
// line describes the finished work; a model that emits none gets today's
// behaviour, which is the property that makes this safe to ask for at all.
func spokenDigest(markdown string) string {
	matches := spokenDigestPattern.FindAllStringSubmatch(markdown, -1)
	if len(matches) == 0 {
		return ""
	}
	return strings.TrimSpace(matches[len(matches)-1][1])
}

// spokenText projects one assistant message into something worth hearing.
//
// It is deliberately lossy and says so: the transcript on screen remains the
// complete record, and a listener who needs the exact path reads it. Trying to
// keep both would produce the 12.9-second version nobody listens to twice.
func spokenText(markdown string) string {
	return spokenProjection(sanitizeTerminalMultiline(markdown))
}

// spokenProjection is the projection itself, over text already made safe.
//
// Split out so the streaming entry point sanitizes once rather than twice. The
// order is not interchangeable: ansi.Strip removes escape sequences whose
// payload can contain any byte, so counting delimiters before stripping would
// count a backtick that is not in the prose and hold back a span that was never
// opened.
func spokenProjection(text string) string {
	// Fences first: everything inside one is code, and code is never spoken.
	// Replacing rather than deleting keeps a sentence from fusing with the one
	// on the far side of a block.
	text = spokenFencePattern.ReplaceAllString(text, ". ")
	text = spokenHTMLComment.ReplaceAllString(text, " ")
	text = spokenMarkdownLink.ReplaceAllString(text, "$1")
	text = spokenBareURL.ReplaceAllString(text, "a link")
	// Inline code is usually an identifier or a path. Its CONTENT is what a
	// listener wants, reduced the same way a bare path is — token by token,
	// because a span is not always one token.
	text = spokenInlineCode.ReplaceAllStringFunc(text, func(match string) string {
		return " " + spokenInlineContent(strings.Trim(match, "`")) + " "
	})
	text = spokenPathToken.ReplaceAllStringFunc(text, spokenPath)
	text = spokenHexRun.ReplaceAllString(text, "a digest")
	text = spokenLeadingMarkup.ReplaceAllString(text, "")
	text = spokenEmphasis.ReplaceAllString(text, "")
	text = spokenEmphasisStar.ReplaceAllString(text, "$1$2")
	text = spokenInitialism.ReplaceAllStringFunc(text, spokenAcronym)
	text = withoutUnspeakableSymbols(text)
	text = spokenWhitespace.ReplaceAllString(text, " ")
	text = spokenStrandedPunctuation.ReplaceAllString(text, "$1")
	return strings.TrimSpace(text)
}

// spokenInlineContent reduces the inside of a `…` span, one token at a time.
//
// The whole span used to be handed to spokenPath, which reduces a token to its
// last path segment — and a span is very often a command rather than a token.
// Measured on ordinary output, "`go test ./internal/ui/ -run TestFoo`" was
// spoken as "run TestFoo": the reducer split on "/", kept the tail, and threw
// away the verb. The instruction survived as its own last two words, which is
// worse than saying nothing, because it still sounds like an instruction.
func spokenInlineContent(content string) string {
	fields := strings.Fields(content)
	for index, field := range fields {
		fields[index] = spokenPath(field)
	}
	return strings.Join(fields, " ")
}

// spokenAcronym spells one initialism out, keeping any plural.
func spokenAcronym(match string) string {
	letters, plural := match, ""
	if strings.HasSuffix(match, "s") {
		letters, plural = match[:len(match)-1], " s"
	}
	if spelled := spokenInitialisms[letters]; spelled != "" {
		return spelled + plural
	}
	return match
}

// withoutUnspeakableSymbols drops what a synthesizer reads by NAME.
//
// An emoji is not silent: measured, "Listo" renders 29,376 bytes and "Listo ✅"
// 56,512 — the tick nearly doubles the utterance, because `say` reads out
// "check mark button". Two of them at the end of an answer is three seconds of
// a robot naming punctuation. Arrows and box-drawing characters do the same
// thing and carry even less, since they are usually the remains of a diagram
// the projection has no business reading at all.
//
// The ranges are deliberately narrow. Everything below U+2190 stays, which
// keeps the em dash, the ellipsis, the accented letters, and every arithmetic
// and comparison symbol that appears in ordinary prose — dropping "+" would
// silently rewrite "ctrl+f".
func withoutUnspeakableSymbols(text string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0x2190 && r <= 0x2BFF, r >= 0x1F000:
			return -1
		default:
			return r
		}
	}, text)
}

// spokenStreamingText projects an answer that is still arriving.
//
// It differs from spokenText in one way, and only while text is still on its
// way: a span whose closing delimiter has not shown up yet is held back rather
// than projected. That distinction is the whole point. Applying the holdback to
// SETTLED text loses information permanently — a finished answer containing one
// unmatched backtick ("El símbolo ` sirve para código. Eso era todo.") kept
// only "El símbolo", because a delimiter that will never be closed is not an
// unfinished span, it is a character. Once nothing more is coming, there is
// nothing to wait for.
func spokenStreamingText(markdown string) string {
	return spokenProjection(withoutUnfinishedMarkup(sanitizeTerminalMultiline(markdown)))
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
	// The sentence's own punctuation is not part of the path, and letting it
	// through costs the sentence. "~/.config/sonar/env." reduced to "env dot" —
	// the terminator became an extension, the boundary disappeared, and every
	// following word was held back as an unfinished tail. A trailing dot is
	// never an extension: an extension has something after it.
	trailing, body := "", trimmed
	for len(body) > 0 && isSentenceTerminator(rune(body[len(body)-1])) {
		trailing = body[len(body)-1:] + trailing
		body = body[:len(body)-1]
	}
	if hasLetterOrDigit(body) {
		trimmed = body
	} else {
		// Nothing with a name in front of it, so those dots are the token rather
		// than the sentence's: "./..." is the whole Go tree, and peeling its
		// wildcard would leave a reduction that then says it back.
		trailing = ""
	}
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
		// A directory written with a trailing slash still has a name; it is one
		// segment further back. "./internal/ui/" is the ui package, not "a path".
		segments := strings.Split(strings.TrimSuffix(trimmed, "/"), "/")
		base = segments[len(segments)-1]
	}
	if base == "" {
		return "a path" + trailing
	}
	if !hasLetterOrDigit(base) {
		// Punctuation with no name in it — "./..." is the whole Go tree, and a
		// listener hearing "go test" already knows which tree. Saying "dot dot
		// dot" adds a second of noise and no information. The sentence's own
		// terminator still has to survive it.
		return trailing
	}
	base = strings.ReplaceAll(base, "_", " ")
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, ".", " dot ")
	return strings.TrimSpace(spokenWhitespace.ReplaceAllString(base, " ")) + trailing
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
		// A closing quote or bracket sits between the terminator and the space
		// that follows it, and it does not stop the sentence ending. Measured:
		// `He said "Done." Next.` came back as one sentence, so the whole quote
		// and everything after it was spoken as a single breath.
		for next < len(runes) && isClosingPunctuation(runes[next]) {
			next++
		}
		if next < len(runes) && runes[next] != ' ' && runes[next] != '\n' && runes[next] != '\t' {
			continue
		}
		if endsWithAbbreviation(string(runes[start:next])) {
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

// hasLetterOrDigit reports whether a token has a name in it at all, as opposed
// to being punctuation that happens to sit where a word would.
func hasLetterOrDigit(token string) bool {
	return strings.ContainsFunc(token, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	})
}

// isClosingPunctuation is what may follow a terminator and still end a
// sentence: the marks that close something the sentence opened.
func isClosingPunctuation(r rune) bool {
	switch r {
	case '"', '\'', '»', '”', '’', ')', ']', '}':
		return true
	default:
		return false
	}
}

func isSentenceTerminator(r rune) bool {
	return r == '.' || r == '!' || r == '?'
}

// endsWithAbbreviation reports whether a candidate sentence ends on one of the
// short forms that carry a period without ending anything.
//
// Checked on the last word only, which is all the shape needs: the terminator
// under consideration is that word's own. A sentence that really ends on "etc."
// is then held open until the next boundary, which is the cheap direction to be
// wrong in — one over-long sentence, against a clause cut at its comparison.
func endsWithAbbreviation(candidate string) bool {
	fields := strings.Fields(candidate)
	if len(fields) == 0 {
		return false
	}
	last := strings.ToLower(strings.TrimRight(fields[len(fields)-1], ".!?"))
	return spokenAbbreviations[last]
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
