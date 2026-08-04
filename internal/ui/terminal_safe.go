package ui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// sanitizeTerminalMultiline removes terminal-state and visual-order controls
// from untrusted display text while retaining ordinary newlines and tabs for
// callers that own their final layout.
func sanitizeTerminalMultiline(value string) string {
	value = strings.ReplaceAll(strings.ReplaceAll(strings.ToValidUTF8(value, "�"), "\r\n", "\n"), "\r", "\n")
	value = ansi.Strip(value)
	return strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' {
			return character
		}
		if unicode.IsControl(character) || isBidiControl(character) {
			return -1
		}
		return character
	}, value)
}

// sanitizeTerminalSingleLine additionally prevents untrusted text from
// creating rows or moving columns inside fixed TUI chrome.
func sanitizeTerminalSingleLine(value string) string {
	value = sanitizeTerminalMultiline(value)
	value = strings.NewReplacer("\n", " ", "\t", " ").Replace(value)
	return strings.Join(strings.Fields(value), " ")
}

// sanitizeTerminalLine keeps code indentation and ordinary spacing while
// preventing an untrusted value from escaping the row it was assigned. Use it
// for tool output and diff lines where collapsing whitespace would make the
// content materially harder to inspect.
func sanitizeTerminalLine(value string) string {
	value = sanitizeTerminalMultiline(value)
	return strings.ReplaceAll(value, "\n", " ")
}

func isBidiControl(value rune) bool {
	switch value {
	case '\u061c', '\u200e', '\u200f',
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e',
		'\u2066', '\u2067', '\u2068', '\u2069':
		return true
	default:
		return false
	}
}
