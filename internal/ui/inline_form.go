package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// inlineFormTranscriptAnchor preserves the transcript row under the user's
// eye while a composer-owned form changes the footer's exact height.
type inlineFormTranscriptAnchor struct {
	valid   bool
	paused  bool
	yOffset int
}

func (m *Model) captureInlineFormTranscriptAnchor() inlineFormTranscriptAnchor {
	if m == nil || !m.ready {
		return inlineFormTranscriptAnchor{}
	}
	return inlineFormTranscriptAnchor{
		valid:   true,
		paused:  m.followPaused(),
		yOffset: m.transcriptYOffset(),
	}
}

func (m *Model) restoreInlineFormTranscriptAnchor(anchor inlineFormTranscriptAnchor) {
	if m == nil || !anchor.valid {
		return
	}
	m.restoreFollowPosition(anchor.paused, anchor.yOffset)
}

func (m *Model) refreshInlineFormLayout(anchor inlineFormTranscriptAnchor) {
	m.recalcViewportHeight()
	m.restoreInlineFormTranscriptAnchor(anchor)
}

// prepareInlineFormOpen enforces a single visible composer owner. Completion
// yields to an explicitly opened form; approval, paste review, queued input,
// busy operations, and other overlays retain their existing authority.
func (m *Model) prepareInlineFormOpen() bool {
	if m == nil || m.pendingApproval != nil || m.pendingPaste != nil ||
		m.queuedFollowUp != nil || m.state != StateIdle || m.composerIsBusy() {
		return false
	}
	switch m.overlay {
	case OverlayNone:
		return true
	case OverlayCompletion:
		m.dismissCompletion()
		return m.overlay == OverlayNone
	default:
		return false
	}
}

func (m *Model) inlineFormActive() bool {
	if m == nil {
		return false
	}
	return (m.overlay == OverlayPlanForm && m.planFormState != nil) ||
		(m.overlay == OverlayGoalForm && m.goalFormState != nil)
}

// inlineFormContentWidth is the usable width inside the one-cell border and
// one-cell horizontal padding. The complete frame matches chatPaneWidth.
func inlineFormContentWidth(terminalWidth int) int {
	return max(1, terminalWidth-5)
}

func renderInlineFormFrame(styles Styles, content, footer string, terminalWidth int, profiles ...GlyphProfile) string {
	content = strings.TrimRight(content, "\n")
	if footer != "" {
		content += "\n" + styles.OverlayDim.Render(footer)
	}
	return lipgloss.NewStyle().
		Border(borderForGlyphProfile(resolveGlyphProfile(profiles...))).
		BorderForeground(styles.OverlayBorder).
		Padding(0, 1).
		Width(max(1, terminalWidth-1)).
		Render(content)
}
