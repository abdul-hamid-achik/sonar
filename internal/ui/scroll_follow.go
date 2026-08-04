package ui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// anchorActive is the canonical follow intent. userScrolledUp remains a mirror
// for compatibility with existing state inspection, and these helpers keep the
// two values from disagreeing.
func (m *Model) followPaused() bool {
	return !m.anchorActive
}

func (m *Model) pauseFollow() {
	m.anchorActive = false
	m.userScrolledUp = true
}

func (m *Model) markFollowingLatest() {
	m.anchorActive = true
	m.userScrolledUp = false
}

func (m *Model) resumeFollow() {
	m.markFollowingLatest()
	m.transcriptGotoBottom()
}

func (m *Model) gotoBottomIfFollowing() {
	if m.anchorActive {
		m.transcriptGotoBottom()
	}
}

func (m *Model) restoreFollowPosition(paused bool, yOffset int) {
	if !paused {
		m.resumeFollow()
		return
	}
	m.setTranscriptYOffset(yOffset)
	m.pauseFollow()
}

func (m *Model) transcriptScrollKey(msg tea.KeyPressMsg) bool {
	return key.Matches(msg, m.keys.PageUp) ||
		key.Matches(msg, m.keys.PageDown) ||
		key.Matches(msg, m.keys.HalfPageUp) ||
		key.Matches(msg, m.keys.HalfPageDn)
}

// transcriptOwnsScrollKey keeps transcript navigation and composer editing
// mutually exclusive. PgUp/PgDn are always transcript navigation. Ctrl+U and
// Ctrl+D remain the textarea's conventional delete bindings while a draft is
// editable and nonempty; otherwise they retain the established half-page
// transcript shortcuts.
func (m *Model) transcriptOwnsScrollKey(msg tea.KeyPressMsg) bool {
	if key.Matches(msg, m.keys.PageUp) || key.Matches(msg, m.keys.PageDown) {
		return true
	}
	if !key.Matches(msg, m.keys.HalfPageUp) && !key.Matches(msg, m.keys.HalfPageDn) {
		return false
	}
	return !m.composerEditable() || m.input.Value() == ""
}

func (m *Model) updateTranscriptScroll(msg tea.KeyPressMsg) tea.Cmd {
	m.cancelReceiptInspection(false)
	beforeOffset := m.transcriptYOffset()
	cmd := m.updateTranscriptViewport(msg)
	if m.transcriptAtBottom() {
		m.markFollowingLatest()
	} else if m.transcriptYOffset() != beforeOffset {
		m.pauseFollow()
	}
	return cmd
}

func (m *Model) canJumpToLatest() bool {
	if m.overlay != OverlayNone || m.pendingApproval != nil || m.pendingPaste != nil {
		return false
	}
	// A visible draft owns ordinary textarea navigation even while an agent turn
	// is waiting or streaming. End is only the transcript's "latest" shortcut
	// when there is no editable text whose cursor can move.
	if m.composerEditable() && m.input.Value() != "" {
		return false
	}
	if m.state != StateIdle || m.composerIsBusy() {
		return true
	}
	// Preserve normal textarea End behavior for every nonempty draft, including
	// whitespace-only input.
	return m.input.Value() == ""
}

func (m *Model) renderFollowPausedStatus(width int) string {
	titleLimit := 0
	if width >= 72 {
		titleLimit = 24
	}
	session := sessionDisplayLabel(m.sessionPublicID, m.activeSessionTitle, titleLimit)
	base := []string{
		"Follow paused · end latest",
		"Paused · end latest",
		"Paused · end",
	}
	candidates := make([]string, 0, len(base)*2)
	if session != "" {
		for _, candidate := range base {
			candidates = append(candidates, session+" · "+candidate)
		}
	}
	candidates = append(candidates, base...)
	// Share the transcript content grid instead of a hand-written indent, so
	// this row lines up with the top bar, notices, and shortcuts around it.
	lead := m.contentGrid().Prefix(" ")
	available := max(1, width-lipgloss.Width(lead))
	chosen := candidates[len(candidates)-1]
	for _, candidate := range candidates {
		if lipgloss.Width(candidate) <= available {
			chosen = candidate
			break
		}
	}
	parts := strings.SplitN(chosen, "end", 2)
	line := lead + m.styles.StatusText.Render(parts[0]) + m.styles.FocusIndicator.Render("end")
	if len(parts) == 2 {
		line += m.styles.StatusText.Render(parts[1])
	}
	return truncateDisplay(line, width)
}
