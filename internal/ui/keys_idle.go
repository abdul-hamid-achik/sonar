package ui

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// handleIdleKey routes keyboard input once no approval, owned operation, or
// overlay owns the keyboard. The second return value reports whether the key
// was consumed here; unhandled keys fall through to the composer and
// transcript sub-components in Update.
func (m *Model) handleIdleKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	// The first frame owns draft input immediately, but execution remains
	// closed until the host commits the startup model and profile. Unhandled
	// editing keys fall through to Bubbles; application shortcuts stay inert.
	if !m.turnReady {
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m.beginShutdown(), true
		case msg.String() == "ctrl+v" && m.composerEditable():
			return m.readClipboardPaste(), true
		case key.Matches(msg, m.keys.NewLine) && m.composerEditable():
			m.clearCompletionSuppression()
			m.input.InsertString("\n")
			m.syncInputHeight()
			return nil, true
		case key.Matches(msg, m.keys.Send):
			return m.setFooterNotice(noticeInfo, "runtime still starting · draft kept", 2*time.Second), true
		default:
			return nil, false
		}
	}

	// A focused editable composer owns every printable key, including the
	// first character of an empty draft. Key.Text is Bubble Tea's explicit
	// printable-character signal and remains empty for application chords.
	// Keep this guard before every global shortcut so future printable
	// bindings cannot silently steal the start of a prompt.
	if m.composerEditable() && m.input.Focused() && msg.Text != "" {
		return nil, false
	}

	// Transcript paging is parent-owned and must never fall through to the
	// composer. PgUp/PgDn always page the conversation. Ctrl+U/Ctrl+D retain
	// their standard textarea editing behavior while a draft is present, and
	// act as half-page transcript shortcuts only when the composer is empty or
	// unavailable.
	if m.transcriptOwnsScrollKey(msg) {
		return m.updateTranscriptScroll(msg), true
	}
	if msg.String() == "ctrl+v" && m.composerEditable() {
		return m.readClipboardPaste(), true
	}
	if _, ok := m.currentInspectedToolTarget(); ok {
		switch msg.String() {
		case "alt+o":
			return m.dispatchInspectedToolAction(toolOpenOutputActionID), true
		case "alt+d":
			return m.dispatchInspectedToolAction(toolOpenDiffActionID), true
		}
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m.beginShutdown(), true

	case key.Matches(msg, m.keys.AgentHub):
		// Agent activity remains inspectable while a foreground turn is
		// running, but an unsent draft retains ownership of application
		// shortcuts so opening a modal never hides text unexpectedly.
		if m.input.Value() == "" {
			m.overlayParent = OverlayNone
			m.openAgentHub()
		}
		return nil, true

	case key.Matches(msg, m.keys.Cancel):
		// A visible queued follow-up owns the first Escape. Clearing the queue
		// must not also cancel the active run; a later Escape still reaches the
		// ordinary cancellation path below.
		if m.clearQueuedFollowUp() {
			return nil, true
		}
		if (m.state == StateStreaming || m.state == StateWaiting) && m.cancel != nil {
			m.cancel()
		}

	case key.Matches(msg, m.keys.Help):
		// Only toggle help when input is empty.
		if m.state == StateIdle && strings.TrimSpace(m.input.Value()) == "" {
			m.overlayParent = OverlayNone
			m.overlay = OverlayHelp
			m.initHelpViewport()
			m.input.Blur()
			return nil, true
		}

	case key.Matches(msg, m.keys.ToggleTools):
		// Batch-toggle all tools when input is empty and idle.
		if m.state == StateIdle && strings.TrimSpace(m.input.Value()) == "" {
			m.cancelReceiptInspection(true)
			anchor := m.captureTranscriptReflowAnchor()
			m.toolsCollapsed = !m.toolsCollapsed
			for i := range m.toolEntries {
				m.toolEntries[i].Collapsed = m.toolsCollapsed
			}
			m.invalidateEntryCache()
			m.refreshTranscript()
			m.restoreTranscriptReflowAnchor(anchor)
			return nil, true
		}

	case key.Matches(msg, m.keys.ToggleFocusedTool):
		// Toggle last tool entry only when input is empty.
		if m.state == StateIdle && strings.TrimSpace(m.input.Value()) == "" {
			if len(m.toolEntries) > 0 {
				target := len(m.toolEntries) - 1
				if _, ok := m.inspectableToolReceiptAction(); ok {
					target = m.lastTurnToolIndex
				}
				if entity, ok := m.toolActionTarget(target); ok {
					return m.dispatchUIAction(UIActionRequest{
						ActionID: toolToggleActionID,
						Target:   entity,
						Source:   UIActionSourceKeyboard,
					}), true
				}
				// Keep the shortcut usable during the narrow pre-reconciliation
				// interval; once a transcript BlockID exists, the registry path
				// above is the sole production dispatcher.
				m.toggleToolReceipt(target, true)
			}
			return nil, true
		}

	case key.Matches(msg, m.keys.CompactToggle):
		if m.state == StateIdle {
			m.cancelReceiptInspection(true)
			anchor := m.captureTranscriptReflowAnchor()
			m.forceCompact = !m.forceCompact
			m.invalidateEntryCache()
			m.refreshTranscript()
			m.restoreTranscriptReflowAnchor(anchor)
			return nil, true
		}

	case key.Matches(msg, m.keys.ToggleThinking):
		// Completed reasoning remains inspectable while the next turn runs. A
		// non-empty draft retains ownership of every control key, and a live
		// Thinking row is never part of this batch operation.
		if m.input.Value() != "" {
			// Bubbles treats Ctrl+T as transpose. This application-level
			// disclosure shortcut must never silently rewrite a draft.
			return nil, true
		}
		m.cancelReceiptInspection(true)
		m.toggleAllThinkingReceipts()
		return nil, true

	case key.Matches(msg, m.keys.ExternalEditor):
		if m.state == StateIdle {
			return m.openExternalEditor(), true
		}

	case key.Matches(msg, m.keys.ToggleMouse):
		// Mouse reporting consumes press and release, which is what stops the
		// terminal from doing native drag-select. Turning it off hands the
		// mouse back to the terminal; the notice says what was traded away,
		// because a dead scroll wheel with no explanation reads as a bug.
		m.mouseCaptureOff = !m.mouseCaptureOff
		if m.mouseCaptureOff {
			return m.setFooterNotice(noticeInfo,
				"mouse capture off · select and copy with the mouse · pgup/pgdn still scroll · alt+m restores", 6*time.Second), true
		}
		return m.setFooterNotice(noticeInfo, "mouse capture on · wheel scrolling restored", 3*time.Second), true

	case key.Matches(msg, m.keys.CopyLast):
		// Prefer last assistant answer; allow copy while a turn is live when the
		// draft is empty so users can grab text without waiting for idle.
		if strings.TrimSpace(m.input.Value()) == "" {
			if content := m.lastAssistantContent(); content != "" {
				return m.copyToClipboard(content), true
			}
		}

	case key.Matches(msg, m.keys.ClearView):
		if m.state == StateIdle {
			m.cancelReceiptInspection(true)
			m.refreshTranscript()
			m.resumeFollow()
			return nil, true
		}

	case key.Matches(msg, m.keys.NewConvo):
		if m.state == StateIdle {
			if m.blockSessionReplacementForHeldFollowUp("starting a new conversation") {
				return nil, true
			}
			m.agent.ClearHistory()
			m.entries = nil
			m.toolEntries = nil
			m.resetConversationSession()
			m.invalidateEntryCache()
			m.entries = append(m.entries, ChatEntry{
				Kind:    "system",
				Content: "New conversation started.",
			})
			m.refreshTranscript()
			m.resumeFollow()
			return nil, true
		}

	case key.Matches(msg, m.keys.CycleMode):
		// Always cycle ambient mode. During a live turn this only prepares the
		// next send (goal turns keep AUTO authority); never swallow Shift+Tab.
		m.cycleMode()
		if m.state != StateIdle {
			label := m.modeConfigs[m.mode].Label
			return m.setFooterNotice(noticeInfo,
				"mode → "+label+" · applies on next send",
				2*time.Second), true
		}
		return nil, true

	case key.Matches(msg, m.keys.ModelPicker):
		if m.state == StateIdle {
			m.overlayParent = OverlayNone
			m.openModelPicker()
			return nil, true
		}

	case key.Matches(msg, m.keys.SettingsPicker):
		if m.state == StateIdle {
			m.openSettingsPicker()
			return nil, true
		}

	case key.Matches(msg, m.keys.NewLine):
		// Insert newline in textarea (shift+enter).
		if m.composerEditable() {
			m.clearCompletionSuppression()
			m.input.InsertString("\n")
			m.syncInputHeight()
			return nil, true
		}

	case key.Matches(msg, m.keys.Send):
		if m.state == StateIdle {
			return m.submitInput(), true
		}
		if m.composerEditable() {
			return m.queueComposerFollowUp(), true
		}
		// The queue slot is occupied, but a safe slash command still runs
		// immediately: local commands do not need the next-iteration slot.
		if m.state == StateWaiting || m.state == StateStreaming {
			if strings.HasPrefix(strings.TrimSpace(m.input.Value()), "/") {
				return m.queueComposerFollowUp(), true
			}
		}

	case key.Matches(msg, m.keys.Complete):
		// Tab key for autocomplete
		if m.composerEditable() && m.completer != nil && !m.isCompletionActive() {
			// Explicit completion always overrides an earlier Escape dismissal.
			m.completionSuppressedDraft = ""
			return m.triggerCompletion(m.input.Value()), true
		}

	case key.Matches(msg, m.keys.HistoryUp):
		// During an active turn, Up edits the one visible queued follow-up
		// before it can be mistaken for ordinary prompt-history navigation.
		if m.editQueuedFollowUp() {
			return nil, true
		}
		if m.state == StateIdle && m.overlay == OverlayNone {
			if strings.TrimSpace(m.input.Value()) == "" || m.historyIndex != -1 {
				if m.navigateHistory(-1) {
					return nil, true
				}
			}
		}

	case key.Matches(msg, m.keys.HistoryDown):
		if m.state == StateIdle && m.overlay == OverlayNone {
			if m.historyIndex != -1 {
				if m.navigateHistory(1) {
					return nil, true
				}
			}
		}
	}

	return nil, false
}
