package ui

import (
	"fmt"
)

const (
	// The compact transcript remains usable at 30 columns. Configuration and
	// runtime detail live in overlays, so the conversation always owns the
	// complete terminal width.
	minTerminalWidth  = 30
	minTerminalHeight = 12
)

// transcriptLayoutCapabilities evaluates the allocated transcript viewport,
// not the outer terminal. The viewport dimensions are authoritative once the
// Bubble Tea parent has measured it. Before that, only the parent's planned
// pane width is known; height remains zero so presentation fails safe.
func (m *Model) transcriptLayoutCapabilities() LayoutCapabilities {
	paneWidth := m.viewport.Width()
	workHeight := m.viewport.Height()
	if paneWidth <= 0 {
		paneWidth = m.chatPaneWidth()
	}

	workRect := transcriptWorkRect(NewCellRect(0, 0, paneWidth, workHeight))
	return DeriveLayoutCapabilities(workRect, LayoutCapabilityOptions{
		ForceCompact: m.forceCompact,
	})
}

// transcriptWorkRect applies the content-grid horizontal chrome to an already
// allocated viewport rectangle: left accent+pad (contentLeftColumns) and right
// slack (contentRightChromeColumns). WorkWidth stays pane − 6; Origin X is 3.
// Callers must split the parent frame first; this helper never infers capacity
// from the outer terminal.
func transcriptWorkRect(viewportRect CellRect) CellRect {
	return Inset(viewportRect, Insets{
		Left:  contentLeftColumns,
		Right: contentRightChromeColumns,
	})
}

// chatPaneWidth is the width owned by the viewport component. Keep this in one
// place so welcome text, messages, tool cards, and diffs all obey the same
// boundary as the smart parent model.
func (m *Model) chatPaneWidth() int {
	w := m.width - 1
	if w < 1 {
		return 1
	}
	return w
}

// chatContentWidth is the transcript content-grid flex width (WorkWidth) used
// by wide structures and by the current Markdown migration. Numerically it is
// pane − contentLeftColumns − contentRightChromeColumns (still pane − 6).
// Readable prose must use transcriptLayoutCapabilities().ProseWidth once the
// renderer can distinguish prose blocks from fences and tables; applying that
// cap here would incorrectly narrow every transcript surface. Keeping WorkWidth
// stable across height-only resizes preserves completed-message caches.
func (m *Model) chatContentWidth() int {
	return m.contentGrid().ContentWidth()
}

// chatProseWidth is the readable measure for conversational text. It is
// intentionally narrower than WorkWidth on large terminals; structural
// surfaces continue to use chatContentWidth.
func (m *Model) chatProseWidth() int {
	return max(1, min(m.chatContentWidth(), m.transcriptLayoutCapabilities().ProseWidth))
}

// narrowTerminalHint returns a recovery-oriented empty state instead of
// letting fixed-width components overflow or disappear.
func (m *Model) narrowTerminalHint() string {
	if m.height < minTerminalHeight && m.width < minTerminalWidth {
		return fmt.Sprintf("Resize the terminal to at least %d columns × %d rows.", minTerminalWidth, minTerminalHeight)
	}
	if m.height < minTerminalHeight {
		return fmt.Sprintf("Resize the terminal to at least %d rows.", minTerminalHeight)
	}
	if m.width < minTerminalWidth {
		return fmt.Sprintf("Resize the terminal to at least %d columns.", minTerminalWidth)
	}
	return ""
}

// terminalInteractionPaused keeps both terminal safety fallbacks honest: while
// the decision/composer surfaces cannot be rendered, they cannot consume input.
// Resize and asynchronous receipts are still processed by the parent model;
// Ctrl+C deliberately falls through to the ordinary graceful-shutdown path.
func (m *Model) terminalInteractionPaused() bool {
	return m.ready && (m.narrowTerminalHint() != "" || m.terminalInputResumeActive())
}
