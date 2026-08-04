package ui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// handleBusyOperationKey serializes keyboard input while an owned host
// operation is in flight. Each guard owns the full keyboard for its
// operation; the second return value reports whether any guard matched.
func (m *Model) handleBusyOperationKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	// Session restoration replaces the complete agent/UI runtime state. Keep
	// input disabled while the DB read is in flight, and let Escape invalidate
	// the generation so a late result cannot overwrite a newer conversation.
	if m.sessionLoading {
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m.beginShutdown(), true
		case key.Matches(msg, m.keys.Cancel):
			m.cancelSessionLoad()
		}
		return nil, true
	}
	if m.sessionListing {
		switch {
		case key.Matches(msg, m.keys.Quit):
			m.cancelSessionList()
			return m.beginShutdown(), true
		case key.Matches(msg, m.keys.Cancel):
			m.cancelSessionList()
			if m.overlay == OverlaySessionsPicker {
				m.closeSessionsPicker()
			}
		}
		return nil, true
	}
	// Provider switching can wait on runtime admission and a bounded local
	// inventory refresh. It runs outside Update, owns the composer until its
	// tokened receipt arrives, and exposes cancellation through Escape.
	if m.providerSwitchRunning {
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m.beginShutdown(), true
		case key.Matches(msg, m.keys.Cancel):
			if m.providerSwitchCancel != nil {
				m.providerSwitchCancel()
			}
		}
		return nil, true
	}
	// Context loads and transcript imports replace prompt authority. Keep
	// input disabled until their tokened result arrives; Escape invalidates
	// a late result without blocking the UI on filesystem cancellation.
	if m.fileLoading {
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m.beginShutdown(), true
		case key.Matches(msg, m.keys.Cancel):
			m.fileOpToken++
			m.fileLoading = false
			m.input.Focus()
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "File operation cancelled; any late read result will be ignored."})
			m.invalidateEntryCache()
			m.refreshTranscript()
			m.gotoBottomIfFollowing()
		}
		return nil, true
	}
	if m.imageAttachRunning {
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m.beginShutdown(), true
		case key.Matches(msg, m.keys.Cancel):
			fallback := m.imageAttachFallback
			if m.imageAttachCancel != nil {
				m.imageAttachCancel()
			}
			m.imageAttachToken++
			m.imageAttachRunning = false
			m.imageAttachCancel = nil
			m.imageAttachFallback = ""
			m.clearImageAttachmentQueue()
			m.input.Focus()
			m.entries = append(m.entries, ChatEntry{Kind: "system", Content: "Image attachment cancelled."})
			m.invalidateEntryCache()
			m.refreshTranscript()
			m.gotoBottomIfFollowing()
			m.recalcViewportHeight()
			if fallback != "" && m.composerEditable() {
				return m.insertPasteWithReview(fallback), true
			}
		}
		return nil, true
	}
	// Read-scope preview and commit are serialized host filesystem work. They
	// are intentionally not cancellable halfway through validation/commit;
	// quit waits for the tokened receipt and every other key is ignored.
	if m.readScopeOpRunning {
		if key.Matches(msg, m.keys.Quit) {
			return m.beginShutdown(), true
		}
		return nil, true
	}
	// Export is an owned filesystem effect. Serialize input until its atomic
	// publication receipt returns so a later turn/commit cannot overlap it.
	if m.exportRunning {
		if key.Matches(msg, m.keys.Quit) {
			return m.beginShutdown(), true
		}
		return nil, true
	}
	// /commit owns a cancellable local-model + git transaction. Do not let a
	// model switch or foreground turn race it; Escape cancels and waits for
	// its tokened receipt, while quit follows the same cancel/join path.
	if m.commitRunning {
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m.beginShutdown(), true
		case key.Matches(msg, m.keys.Cancel):
			if m.commitCancel != nil {
				m.commitCancel()
			}
		}
		return nil, true
	}
	// Installing a verified Ollama snapshot can change execution location and
	// the current model's authority. Do not let a new turn or model switch race
	// that reconciliation. If a refresh is waiting behind an active turn,
	// Escape still cancels the foreground turn so the commit can finish.
	if m.ollamaInventoryCommitting {
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m.beginShutdown(), true
		case key.Matches(msg, m.keys.Cancel):
			if m.cancel != nil {
				m.cancel()
			}
		}
		return nil, true
	}
	if m.goalOperation != "" {
		switch {
		case key.Matches(msg, m.keys.Quit):
			m.cancelGoalOperation("Goal operation cancelled during shutdown.")
			return m.beginShutdown(), true
		case key.Matches(msg, m.keys.Cancel):
			m.cancelGoalOperation("Goal operation cancelled; the goal is paused.")
		case key.Matches(msg, m.keys.CycleMode):
			// Ambient mode only; goal tool authority stays AUTO until next send.
			m.cycleMode()
		}
		return nil, true
	}
	return nil, false
}
