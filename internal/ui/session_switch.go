package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

type sessionSwitchChoice uint8

const (
	sessionSwitchUndecided sessionSwitchChoice = iota
	sessionSwitchKeep
	sessionSwitchDiscard
)

// pendingSessionSwitch is a transient, host-owned boundary around unsent
// composer state. It never enters SessionLoadedMsg or durable session JSON.
type pendingSessionSwitch struct {
	TargetSessionID int64
	TargetPublicID  string
	TargetTitle     string
	Draft           string
	CursorRune      int
	Images          []pendingImageAttachment
	Choice          sessionSwitchChoice
	LoadToken       uint64
}

func (m *Model) beginSessionSwitch(targetID int64, targetPublicID, targetTitle string) tea.Cmd {
	m.overlayParent = OverlayNone
	m.closeSessionsPicker()
	if m.blockSessionReplacementForHeldFollowUp("opening a saved session") {
		return nil
	}
	if m.input.Value() == "" && len(m.pendingImages) == 0 {
		selector, _ := SessionIDResumeSelector(targetID)
		return m.requestSessionRestore(selector)
	}
	m.pendingSessionSwitch = &pendingSessionSwitch{
		TargetSessionID: targetID,
		TargetPublicID:  targetPublicID,
		TargetTitle:     sanitizeTerminalSingleLine(targetTitle),
		Draft:           m.input.Value(),
		CursorRune:      textareaCursorRuneOffset(m.input.Value(), m.input.Line(), m.input.Column()),
		Images:          clonePendingImages(m.pendingImages),
	}
	m.input.Blur()
	m.recalcViewportHeight()
	return nil
}

func (m *Model) startPendingSessionSwitch(choice sessionSwitchChoice) tea.Cmd {
	pending := m.pendingSessionSwitch
	if pending == nil || pending.Choice != sessionSwitchUndecided ||
		(choice != sessionSwitchKeep && choice != sessionSwitchDiscard) {
		return nil
	}
	pending.Choice = choice
	selector, err := SessionIDResumeSelector(pending.TargetSessionID)
	if err != nil {
		m.restoreAndClearPendingSessionSwitch()
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Load session: " + err.Error()})
		m.refreshTranscript()
		m.resumeFollow()
		return nil
	}
	cmd := m.requestSessionRestore(selector)
	pending.LoadToken = m.sessionLoadToken
	m.recalcViewportHeight()
	return cmd
}

func (m *Model) renderSessionSwitchPrompt(width int) string {
	pending := m.pendingSessionSwitch
	if pending == nil || pending.Choice != sessionSwitchUndecided {
		return ""
	}
	titleLimit := 0
	if width >= 72 {
		titleLimit = 24
	}
	handle := sessionDisplayLabel(pending.TargetPublicID, pending.TargetTitle, titleLimit)
	subject := "Open saved session?"
	details := []string{handle}
	compact := width < 40
	if titleLimit > 0 && pending.TargetTitle != "" {
		// Keep the wide target title in the non-negotiable subject. The shared
		// decision renderer progressively truncates detail to preserve complete
		// action labels, so leaving identity in detail can reduce a useful title
		// to only one or two cells even when the terminal has ample width.
		subject = "Open " + handle + "?"
		details = nil
	} else if compact {
		subject = "Open " + sessionDisplayLabel(pending.TargetPublicID, "", 0) + "?"
		details = nil
	}
	if pending.Draft != "" {
		lines := strings.Count(pending.Draft, "\n") + 1
		label := fmt.Sprintf("%d draft line%s", lines, pluralSuffix(lines))
		if compact {
			label = fmt.Sprintf("%d line%s", lines, pluralSuffix(lines))
		}
		details = append(details, label)
	}
	if count := len(pending.Images); count > 0 {
		details = append(details, fmt.Sprintf("%d image%s", count, pluralSuffix(count)))
	}
	detail := strings.Join(details, " · ")
	return m.renderDecisionPrompt(
		subject, truncateDisplay(detail, max(1, width-2)),
		keyHint{Key: "esc", Action: "cancel"},
		keyHint{Key: "k", Action: "keep both"},
		keyHint{Key: "d", Action: "discard both"},
	)
}

func (m *Model) validatePendingSessionSwitch(message SessionLoadedMsg) error {
	pending := m.pendingSessionSwitch
	if pending == nil {
		return nil
	}
	if pending.Choice != sessionSwitchKeep && pending.Choice != sessionSwitchDiscard {
		return fmt.Errorf("session switch decision is not settled")
	}
	if pending.LoadToken == 0 || pending.LoadToken != message.LoadToken {
		return fmt.Errorf("session switch receipt does not match the active load")
	}
	if pending.TargetSessionID <= 0 || pending.TargetSessionID != message.SessionID {
		return fmt.Errorf(
			"session switch target %s does not match loaded %s",
			sessionDisplayLabel(pending.TargetPublicID, "", 0),
			sessionDisplayLabel(message.SessionPublicID, "", 0),
		)
	}
	if pending.Choice != sessionSwitchKeep {
		return nil
	}
	if err := validateImageConversationBudget(message.State.Messages, attachmentRefs(pending.Images)); err != nil {
		return fmt.Errorf("keep draft images: %w", err)
	}
	return nil
}

func (m *Model) applyPendingSessionSwitchSuccess(message SessionLoadedMsg) {
	pending := m.pendingSessionSwitch
	if pending == nil || pending.LoadToken != message.LoadToken || pending.TargetSessionID != message.SessionID ||
		(pending.Choice != sessionSwitchKeep && pending.Choice != sessionSwitchDiscard) {
		// Startup and direct restores have no unsent-state boundary. Successful
		// replacement still starts with a clean composer.
		m.clearCompletionSuppression()
		m.input.Reset()
		m.historyIndex = -1
		m.historySaved = ""
		m.syncInputHeight()
		m.recalcViewportHeight()
		return
	}

	m.clearCompletionSuppression()
	m.historyIndex = -1
	m.historySaved = ""
	if pending.Choice == sessionSwitchKeep {
		m.setComposerDraftAtRune(pending.Draft, pending.CursorRune)
		m.pendingImages = pending.Images
		pending.Images = nil // ownership moved to the live composer
	} else {
		m.input.Reset()
		m.syncInputHeight()
	}
	m.clearPendingSessionSwitchSnapshot()
	m.input.Focus()
	m.recalcViewportHeight()
}

func (m *Model) restoreAndClearPendingSessionSwitch() {
	pending := m.pendingSessionSwitch
	if pending == nil {
		return
	}
	m.clearPendingImages()
	m.pendingImages = pending.Images
	pending.Images = nil
	m.setComposerDraftAtRune(pending.Draft, pending.CursorRune)
	m.clearPendingSessionSwitchSnapshot()
	m.input.Focus()
	m.recalcViewportHeight()
}

func (m *Model) clearPendingSessionSwitchSnapshot() {
	if m.pendingSessionSwitch == nil {
		return
	}
	clearTransientImages(m.pendingSessionSwitch.Images)
	m.pendingSessionSwitch.Images = nil
	m.pendingSessionSwitch.Draft = ""
	m.pendingSessionSwitch = nil
}
