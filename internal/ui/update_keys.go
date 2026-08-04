package ui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// handleKeyPress is the single keyboard router. It dispatches in strict
// authority order: startup, pending approval decisions, host-owned prompts,
// owned busy operations, global quit, overlays, and finally the idle
// composer. The second return value reports whether the key was consumed; an
// unhandled key falls through to the composer and transcript sub-components
// in Update.
func (m *Model) handleKeyPress(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	// Pending tool approval owns the keyboard before every other overlay.
	if m.pendingApproval != nil {
		return m.handlePendingApprovalKey(msg)
	}
	// External read-root authorization is a host-owned decision. It precedes
	// every overlay and never falls through to composer or agent shortcuts.
	if m.readScopePrompt != nil {
		return m.handleReadScopePromptKey(msg)
	}
	// Switching sessions must settle unsent text and images as one atomic
	// draft. This host-owned decision precedes loading and never falls through
	// to ordinary composer or overlay shortcuts.
	if m.pendingSessionSwitch != nil && m.pendingSessionSwitch.Choice == sessionSwitchUndecided {
		return m.handleSessionSwitchDecisionKey(msg)
	}
	// Pending paste intercept: y/n/esc before anything else.
	if m.pendingPaste != nil {
		return m.handlePendingPasteKey(msg)
	}
	// Cortex decisions own keys ahead of busy goal-operation guards. Escape
	// hides only the presentation while an exact answer/refresh continues;
	// it must never be reinterpreted as cancellation of that operation.
	if m.cortexDecisionActive() {
		return m.updateCortexDecisionKey(msg), true
	}
	// Stacked viewers own every key except the already-handled host authority
	// surfaces and graceful global quit. The transcript and composer must not
	// react behind the modal.
	if m.viewerModalActive() {
		if key.Matches(msg, m.keys.Quit) {
			return m.beginShutdown(), true
		}
		return m.handleViewerKey(msg), true
	}
	// End is the transcript's explicit recovery action whenever the composer
	// is empty or temporarily unavailable. Handle it before owned busy-state
	// guards so the advertised action cannot be swallowed by an in-flight
	// session, file, export, or commit operation.
	if key.Matches(msg, m.keys.JumpLatest) && m.canJumpToLatest() {
		m.cancelReceiptInspection(true)
		m.resumeFollow()
		return nil, true
	}
	// Transcript search is a read-only, non-printable global gesture. It stays
	// available while a turn streams or a background operation owns the
	// composer, but never preempts host decisions, viewers, or another overlay.
	if m.overlay == OverlayNone && key.Matches(msg, m.keys.TranscriptSearch) {
		return m.openTranscriptSearch(), true
	}
	if cmd, handled := m.handleBusyOperationKey(msg); handled {
		return cmd, true
	}
	// Quit remains global even while a list/modal owns keyboard focus.
	// Bubbles list quit bindings are deliberately disabled so Ctrl+C must
	// follow the application's graceful cancel/join path here.
	if key.Matches(msg, m.keys.Quit) {
		return m.beginShutdown(), true
	}
	// Handle overlay keys first.
	if m.overlay != OverlayNone {
		return m.handleOverlayKey(msg)
	}
	return m.handleIdleKey(msg)
}
