package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	// renderPickerFrame uses a one-cell border and one-cell horizontal
	// padding. Keep cursor translation next to that frame contract so modal
	// inputs cannot drift independently of their rendered controls.
	pickerFrameCursorX = 2
	pickerFrameCursorY = 1
)

// offsetCursor returns a translated copy without mutating the child-owned
// cursor. A nil child cursor means that surface does not currently own focus.
func offsetCursor(cursor *tea.Cursor, x, y int) *tea.Cursor {
	if cursor == nil {
		return nil
	}

	translated := *cursor
	translated.X += x
	translated.Y += y
	return &translated
}

func pickerFrameCursor(cursor *tea.Cursor) *tea.Cursor {
	return offsetCursor(cursor, pickerFrameCursorX, pickerFrameCursorY)
}

// centeredOverlayStartY mirrors overlayOnContent's vertical placement.
//
// Modals are anchored, not centered on their own height: a four-row prompt and
// a twenty-row picker open with their top edge on the same row, so navigating
// between overlays no longer walks the panel up and down the screen. The
// anchor sits above the middle because modal content grows downward.
func centeredOverlayStartY(base, overlay string) int {
	baseHeight := len(strings.Split(base, "\n"))
	overlayHeight := len(strings.Split(overlay, "\n"))
	room := baseHeight - overlayHeight
	if room <= 0 {
		return 0
	}
	anchor := baseHeight / 4
	if anchor > room {
		anchor = room
	}
	return max(0, anchor)
}

// centeredOverlayLineX mirrors overlayOnContent's horizontal placement.
//
// The whole modal block shares one column. Placing each line independently
// made ragged rows (a short footer, a padded title) drift against each other
// inside the same border. Lip Gloss width keeps ANSI styling and wide runes
// coordinate-safe.
func centeredOverlayLineX(width int, block string) int {
	return max(0, (width-overlayBlockWidth(block))/2)
}

// overlayBlockWidth is the widest rendered line in a modal block.
func overlayBlockWidth(block string) int {
	widest := 0
	for _, line := range strings.Split(block, "\n") {
		if w := lipgloss.Width(line); w > widest {
			widest = w
		}
	}
	return widest
}

// overlayCursor translates a cursor local to a rendered overlay into the
// parent terminal coordinate space. Horizontal centering is calculated from
// the cursor's actual row because styled modal lines can have different widths.
func overlayCursor(base, overlay string, width int, cursor *tea.Cursor) *tea.Cursor {
	if cursor == nil {
		return nil
	}

	overlayLines := strings.Split(overlay, "\n")
	if cursor.Y < 0 || cursor.Y >= len(overlayLines) {
		return nil
	}

	return offsetCursor(
		cursor,
		centeredOverlayLineX(width, overlay),
		centeredOverlayStartY(base, overlay),
	)
}
