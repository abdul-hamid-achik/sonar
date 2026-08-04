package ui

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// completionAnchor identifies the exact trigger token owned by an open
// completion surface. All offsets are rune indexes into Draft so multibyte
// text before or after the token cannot move the replacement boundary.
type completionAnchor struct {
	Draft      string
	StartRune  int
	EndRune    int
	CursorRune int
	Trigger    rune
	Valid      bool
}

type completionToken struct {
	Kind          string
	Query         string
	Source        string
	CommandPrefix string
	Anchor        completionAnchor
}

// completionTokenAtCursor returns the non-whitespace token intersecting the
// textarea cursor. @ and # are discoverable at a token boundary; / is limited
// to command position (only whitespace may precede it in the whole draft).
func completionTokenAtCursor(draft string, cursorRune int) (completionToken, bool) {
	runes := []rune(draft)
	cursorRune = max(0, min(cursorRune, len(runes)))
	start := cursorRune
	for start > 0 && !unicode.IsSpace(runes[start-1]) {
		start--
	}
	if start >= len(runes) {
		return commandActionCompletionToken(runes, draft, cursorRune)
	}

	trigger := runes[start]
	if trigger != '/' && trigger != '@' && trigger != '#' {
		return commandActionCompletionToken(runes, draft, cursorRune)
	}
	if start > 0 && !unicode.IsSpace(runes[start-1]) {
		return completionToken{}, false
	}
	if trigger == '/' {
		for _, value := range runes[:start] {
			if !unicode.IsSpace(value) {
				return completionToken{}, false
			}
		}
	}

	end := cursorRune
	for end < len(runes) && !unicode.IsSpace(runes[end]) {
		end++
	}
	if cursorRune < start+1 {
		return completionToken{}, false
	}

	kind := "command"
	switch trigger {
	case '@':
		kind = "attachments"
	case '#':
		kind = "skills"
	}
	return completionToken{
		Kind:   kind,
		Query:  string(runes[start+1 : cursorRune]),
		Source: string(trigger),
		Anchor: completionAnchor{
			Draft:      draft,
			StartRune:  start,
			EndRune:    end,
			CursorRune: cursorRune,
			Trigger:    trigger,
			Valid:      true,
		},
	}, true
}

// commandActionCompletionToken recognizes only the first argument of a
// leading slash command. The whole `/command prefix` span is replaced on
// acceptance, while the filter owns only the action prefix. Later positional
// arguments remain ordinary composer input and never reopen completion.
func commandActionCompletionToken(runes []rune, draft string, cursorRune int) (completionToken, bool) {
	commandStart := 0
	for commandStart < len(runes) && unicode.IsSpace(runes[commandStart]) {
		commandStart++
	}
	if commandStart >= len(runes) || runes[commandStart] != '/' {
		return completionToken{}, false
	}

	commandEnd := commandStart + 1
	for commandEnd < len(runes) && !unicode.IsSpace(runes[commandEnd]) {
		commandEnd++
	}
	if commandEnd == commandStart+1 || commandEnd >= cursorRune {
		return completionToken{}, false
	}

	actionStart := commandEnd
	for actionStart < len(runes) && unicode.IsSpace(runes[actionStart]) {
		actionStart++
	}
	if cursorRune < actionStart {
		return completionToken{}, false
	}
	for _, value := range runes[actionStart:cursorRune] {
		if unicode.IsSpace(value) {
			return completionToken{}, false
		}
	}

	actionEnd := cursorRune
	for actionEnd < len(runes) && !unicode.IsSpace(runes[actionEnd]) {
		actionEnd++
	}
	prefix := string(runes[commandStart:actionStart])
	query := string(runes[actionStart:cursorRune])
	return completionToken{
		Kind:          "command",
		Query:         query,
		Source:        prefix + query,
		CommandPrefix: prefix,
		Anchor: completionAnchor{
			Draft:      draft,
			StartRune:  commandStart,
			EndRune:    actionEnd,
			CursorRune: cursorRune,
			Trigger:    '/',
			Valid:      true,
		},
	}, true
}

// textareaCursorRuneOffset converts Bubbles' logical line/rune-column cursor
// into a single rune index for completion anchoring.
func textareaCursorRuneOffset(draft string, line, column int) int {
	lines := strings.Split(draft, "\n")
	if len(lines) == 0 {
		return 0
	}
	line = max(0, min(line, len(lines)-1))
	offset := 0
	for index := 0; index < line; index++ {
		offset += utf8.RuneCountInString(lines[index]) + 1
	}
	return offset + max(0, min(column, utf8.RuneCountInString(lines[line])))
}

func normalizedCompletionAnchor(cs *CompletionState, fallback string) completionAnchor {
	if cs != nil && cs.Anchor.Valid {
		return cs.Anchor
	}
	runeCount := utf8.RuneCountInString(fallback)
	return completionAnchor{
		Draft:      fallback,
		StartRune:  0,
		EndRune:    runeCount,
		CursorRune: runeCount,
		Valid:      true,
	}
}

func replaceCompletionAnchor(anchor completionAnchor, replacement string) (string, int) {
	return replaceCompletionAnchorAt(anchor, replacement, utf8.RuneCountInString(replacement))
}

func replaceCompletionAnchorAt(anchor completionAnchor, replacement string, replacementCursorRune int) (string, int) {
	runes := []rune(anchor.Draft)
	start := max(0, min(anchor.StartRune, len(runes)))
	end := max(start, min(anchor.EndRune, len(runes)))
	replacementRunes := []rune(replacement)
	replacementCursorRune = max(0, min(replacementCursorRune, len(replacementRunes)))
	result := make([]rune, 0, len(runes)-(end-start)+len(replacementRunes))
	result = append(result, runes[:start]...)
	result = append(result, replacementRunes...)
	result = append(result, runes[end:]...)
	return string(result), start + replacementCursorRune
}

// setComposerDraftAtRune updates the textarea while leaving its logical
// cursor between prefix and suffix. Building from the suffix lets Bubbles keep
// its own backing grid and cursor invariants without byte-based cursor math.
func (m *Model) setComposerDraftAtRune(draft string, cursorRune int) {
	runes := []rune(draft)
	cursorRune = max(0, min(cursorRune, len(runes)))
	m.input.SetValue(string(runes[cursorRune:]))
	m.input.MoveToBegin()
	m.input.InsertString(string(runes[:cursorRune]))
	_ = m.reflowInputViewport()
}

func completionInsertion(cs *CompletionState, suffixStartsWithSpace bool) string {
	if cs == nil {
		return ""
	}

	indices := make([]int, 0, len(cs.Selected))
	for index, selected := range cs.Selected {
		if selected && index >= 0 && index < len(cs.AllItems) {
			indices = append(indices, index)
		}
	}
	sort.Ints(indices)
	parts := make([]string, 0, max(1, len(indices)))
	for _, index := range indices {
		if insert := strings.TrimSpace(cs.AllItems[index].Insert); insert != "" {
			parts = append(parts, insert)
		}
	}
	if len(parts) == 0 && cs.Index >= 0 && cs.Index < len(cs.FilteredItems) {
		if insert := strings.TrimSpace(cs.FilteredItems[cs.Index].Insert); insert != "" {
			parts = append(parts, insert)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	insertion := strings.Join(parts, " ")
	if !suffixStartsWithSpace {
		insertion += " "
	}
	return insertion
}

func completionAnchorSuffixStartsWithSpace(anchor completionAnchor) bool {
	runes := []rune(anchor.Draft)
	return anchor.EndRune >= 0 && anchor.EndRune < len(runes) && unicode.IsSpace(runes[anchor.EndRune])
}
