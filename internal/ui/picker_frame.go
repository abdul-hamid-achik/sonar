package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

const defaultPickerItemHeight = 2

var asciiPickerTextReplacer = strings.NewReplacer(
	"↑/↓", "j/k",
	"↑", "^",
	"↓", "v",
	"←", "<",
	"→", ">",
	" · ", " | ",
	"…", "~",
)

func (m *Model) renderPickerFrame(content string, footer string) string {
	width := pickerContentWidth(m.width)
	content = strings.TrimRight(content, "\n")
	content = pickerTextForGlyphProfile(content, m.glyphProfile)
	if footer != "" {
		footer = pickerTextForGlyphProfile(footer, m.glyphProfile)
		content += "\n" + m.styles.OverlayDim.Render(footer)
	}
	return lipgloss.NewStyle().
		Border(borderForGlyphProfile(m.glyphProfile)).
		BorderForeground(m.styles.FocusIndicator.GetForeground()).
		Padding(0, 1).
		Width(width + 2).
		Render(content)
}

func (m *Model) pickerNavigationFooter(filterable bool) string {
	width := pickerListWidth(m.width)
	hints := []keyHint{
		{Key: m.keys.Cancel.Help().Key, Action: m.overlayCloseLabel()},
		{Key: m.keys.CompleteSelect.Help().Key, Action: "select"},
		{Key: pickerMoveKey(m.glyphProfile), Action: "move"},
	}
	if filterable {
		hints = append(hints, keyHint{Key: "/", Action: "filter"})
	}
	return pickerTextForGlyphProfile(
		m.renderKeyHints(width, hints...),
		m.glyphProfile,
	)
}

func pickerMoveKey(profile GlyphProfile) string {
	if resolveGlyphProfile(profile) == GlyphASCII {
		return "j/k"
	}
	return "↑/↓"
}

// pickerTextForGlyphProfile closes the final ASCII gap after Bubbles has
// rendered styled list text. Every replacement is cell-width preserving, so
// the already-measured picker frame remains exact.
func pickerTextForGlyphProfile(value string, profile GlyphProfile) string {
	if resolveGlyphProfile(profile) != GlyphASCII {
		return value
	}
	return asciiPickerTextReplacer.Replace(value)
}
