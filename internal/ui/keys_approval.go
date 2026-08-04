package ui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

// handlePendingApprovalKey resolves keyboard input while a tool approval is
// pending. Pending tool approval owns the keyboard before every other overlay.
// Decisions remain typed so a host failure cannot be reported as a human
// denial. Wider scopes (a/p/w) are only honored when offered for the tool.
func (m *Model) handlePendingApprovalKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	resumeActivity := false
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.resolvePendingApproval(permission.Cancelled("application is shutting down"))
		return m.beginShutdown(), true
	case key.Matches(msg, m.keys.Cancel):
		m.resolvePendingApproval(permission.Cancelled("approval cancelled by user"))
		if m.cancel != nil {
			m.cancel()
		}
	case strings.EqualFold(msg.String(), "y"):
		m.resolvePendingApproval(permission.AllowOnce())
		resumeActivity = true
	case strings.EqualFold(msg.String(), "n"):
		m.resolvePendingApproval(permission.Deny())
		resumeActivity = true
	case strings.EqualFold(msg.String(), "s"):
		// Exact-request session grant: same tool + same canonical arguments only.
		m.resolvePendingApproval(permission.AllowSession())
		resumeActivity = true
	case strings.EqualFold(msg.String(), "a"), strings.EqualFold(msg.String(), "p"), strings.EqualFold(msg.String(), "w"):
		keyName := strings.ToLower(msg.String())
		if choice, ok := m.approvalChoiceByKey(keyName); ok {
			m.resolvePendingApprovalWithScope(approvalResponseForScope(choice.ScopeKind), choice.ScopeKind)
			resumeActivity = true
			break
		}
		m.navigateApprovalViewport(msg.String())
	case key.Matches(msg, m.keys.CompleteSelect):
		resp, scope := m.selectedApprovalResponseAndScope()
		m.resolvePendingApprovalWithScope(resp, scope)
		resumeActivity = true
	case key.Matches(msg, m.keys.CompleteUp), strings.EqualFold(msg.String(), "k"):
		m.moveApprovalChoice(-1)
	case key.Matches(msg, m.keys.CompleteDown), strings.EqualFold(msg.String(), "j"):
		m.moveApprovalChoice(1)
	case strings.EqualFold(msg.String(), "d"):
		m.toggleApprovalDetails()
	default:
		m.navigateApprovalViewport(msg.String())
	}
	if resumeActivity {
		return m.startActivityCmd(), true
	}
	return nil, true
}

func (m *Model) approvalChoiceByKey(keyName string) (approvalChoice, bool) {
	for _, choice := range m.currentApprovalChoices() {
		if strings.EqualFold(choice.Key, keyName) {
			return choice, true
		}
	}
	return approvalChoice{}, false
}

// handleReadScopePromptKey resolves keyboard input while an external
// read-root authorization prompt is active. It is a host-owned decision that
// precedes every overlay and never falls through to composer or agent
// shortcuts.
func (m *Model) handleReadScopePromptKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		return m.beginShutdown(), true
	case strings.EqualFold(msg.String(), "y"):
		return m.confirmReadScopePrompt(), true
	case strings.EqualFold(msg.String(), "n"):
		m.resolveReadScopePrompt("denied")
	case key.Matches(msg, m.keys.Cancel):
		m.resolveReadScopePrompt("cancelled")
	}
	return nil, true
}

// handleSessionSwitchDecisionKey resolves keyboard input while an undecided
// session switch holds unsent text and images. Switching sessions must settle
// that draft as one atomic decision; it precedes loading and never falls
// through to ordinary composer or overlay shortcuts.
func (m *Model) handleSessionSwitchDecisionKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.clearPendingSessionSwitchSnapshot()
		return m.beginShutdown(), true
	case key.Matches(msg, m.keys.Cancel):
		m.clearPendingSessionSwitchSnapshot()
		m.input.Focus()
		m.recalcViewportHeight()
	case strings.EqualFold(msg.String(), "k"):
		return m.startPendingSessionSwitch(sessionSwitchKeep), true
	case strings.EqualFold(msg.String(), "d"):
		return m.startPendingSessionSwitch(sessionSwitchDiscard), true
	}
	return nil, true
}

// handlePendingPasteKey resolves the pending paste intercept: y/n/esc before
// anything else.
func (m *Model) handlePendingPasteKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	pending := m.pendingPaste
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.pendingPaste = nil
		m.recalcViewportHeight()
		return m.beginShutdown(), true
	case strings.EqualFold(msg.String(), "y"):
		if pending.PlainFits {
			m.clearCompletionSuppression()
			insertion := pending.Content
			if pending.FencedFits {
				insertion = pending.Fenced
			}
			m.input.InsertString(insertion)
			m.pendingPaste = nil
			m.recalcViewportHeight()
			m.syncInputHeight()
			m.activateCortexDecision()
			return m.reflowInputViewport(), true
		}
	case strings.EqualFold(msg.String(), "n"):
		if pending.PlainFits && pending.FencedFits {
			m.clearCompletionSuppression()
			m.input.InsertString(pending.Content)
			m.pendingPaste = nil
			m.recalcViewportHeight()
			m.syncInputHeight()
			m.activateCortexDecision()
			return m.reflowInputViewport(), true
		}
	case key.Matches(msg, m.keys.Cancel):
		m.pendingPaste = nil
		m.recalcViewportHeight()
		m.activateCortexDecision()
	}
	return nil, true
}
