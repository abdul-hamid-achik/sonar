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
		case key.Matches(msg, m.keys.Paste) && m.composerEditable():
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
	if key.Matches(msg, m.keys.Paste) && m.composerEditable() {
		return m.readClipboardPaste(), true
	}
	if _, ok := m.currentInspectedToolTarget(); ok {
		switch {
		case key.Matches(msg, m.keys.InspectOutput):
			return m.dispatchInspectedToolAction(toolOpenOutputActionID), true
		case key.Matches(msg, m.keys.InspectDiff):
			return m.dispatchInspectedToolAction(toolOpenDiffActionID), true
		}
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m.beginShutdown(), true

	case key.Matches(msg, m.keys.Cancel):
		// An open microphone owns Escape before anything else does. It is the
		// only stop that works in every terminal — the toggle chord may be
		// claimed by a multiplexer, and /voice needs the composer, which is
		// awkward while dictating into it — and discarding is the right
		// default for a key that means "undo this": someone hitting escape
		// mid-sentence wants the recording gone, not transcribed.
		if m.listeningForVoice() {
			m.voiceInput.listener.Cancel()
			m.voiceInput.token++
			m.voiceInput.transcribing = false
			return m.setFooterNotice(noticeInfo, "Voice input cancelled.", 2*time.Second), true
		}
		// A visible queued follow-up owns the first Escape. Clearing the queue
		// must not also cancel the active run; a later Escape still reaches the
		// ordinary cancellation path below.
		if m.clearQueuedFollowUp() {
			return nil, true
		}
		if (m.state == StateStreaming || m.state == StateWaiting) && m.cancel != nil {
			m.cancel()
			return nil, true
		}
		// The listening stage leaves LAST, after every other meaning of Escape.
		//
		// It claimed the key first at one point, which is the obvious reading —
		// it is the whole screen, so closing it is what "close this" means. It
		// is also wrong twice. A turn running while the panel is up is exactly
		// the run somebody is watching from across the room, and stopping it
		// matters more than putting the transcript back. And an open microphone
		// is worse: Escape's job there is to discard the recording, and a panel
		// that swallowed it would leave the room being captured while the screen
		// changed.
		//
		// So it is the fallback, which also makes the gesture consistent: one
		// Escape means "undo the most urgent thing", and leaving a panel is
		// never the most urgent thing.
		if m.voiceStageActive() {
			m.voiceStage = false
			m.refreshTranscript()
			return nil, true
		}

	case key.Matches(msg, m.keys.Help):
		// F1 opens help regardless of the draft — an overlay covers the
		// composer without touching it, so the text survives the detour.
		// Ctrl+H shares the binding but is backspace whenever a draft exists,
		// so only the empty-draft case may read it as help.
		if m.state == StateIdle && (strings.TrimSpace(m.input.Value()) == "" || msg.String() == "f1") {
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
		// A chord that silently does nothing reads as a dead key. Say what it
		// needs instead; the draft is untouched either way.
		if m.state == StateIdle {
			return m.setFooterNotice(noticeInfo, "alt+t needs an empty draft · draft kept", 3*time.Second), true
		}

	case key.Matches(msg, m.keys.VoiceInput):
		// Dictation is composer input, so it is available wherever the composer
		// is: mid-turn a spoken follow-up queues exactly as a typed one does.
		return m.toggleVoiceInput(), true

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
		if m.state == StateIdle {
			return m.setFooterNotice(noticeInfo, "ctrl+t needs an empty draft · draft kept", 3*time.Second), true
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
			// disclosure shortcut must never silently rewrite a draft — and a
			// consumed key that says nothing reads as a dead one.
			return m.setFooterNotice(noticeInfo, "alt+r needs an empty draft · draft kept", 3*time.Second), true
		}
		m.cancelReceiptInspection(true)
		m.toggleAllThinkingReceipts()
		return nil, true

	case key.Matches(msg, m.keys.ExternalEditor):
		if m.state == StateIdle {
			return m.openExternalEditor(), true
		}

	case key.Matches(msg, m.keys.ToggleMouse):
		return m.toggleMouseCapture(), true

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

// toggleMouseCapture flips mouse reporting for native terminal select/copy.
//
// Mouse reporting consumes press and release, which is what stops the terminal
// from doing native drag-select. Turning it off hands the mouse back; the
// notice says what was traded away, because a dead scroll wheel with no
// explanation reads as a bug. Shared by alt+m and /mouse so both paths paint
// the same sticky select chrome and the same restore hint.
func (m *Model) toggleMouseCapture() tea.Cmd {
	m.mouseCaptureOff = !m.mouseCaptureOff
	if m.mouseCaptureOff {
		return m.setFooterNotice(noticeInfo,
			"mouse capture off · select and copy with the mouse · pgup/pgdn still scroll · alt+m or /mouse restores", 6*time.Second)
	}
	return m.setFooterNotice(noticeInfo, "mouse capture on · wheel scrolling restored", 3*time.Second)
}
