package ui

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// handleThemeChange applies a terminal background color change to every
// themed surface without disturbing transcript anchors.
func (m *Model) handleThemeChange(msg tea.BackgroundColorMsg) {
	m.isDark = msg.IsDark()
	m.rebuildThemedSurfaces()
}

// rebuildThemedSurfaces repaints every themed surface from the current
// appearance — light/dark and selected color scheme — while preserving
// transcript, approval, and form anchors.
//
// Light/dark detection and /theme are the same event as far as the frame is
// concerned: both change what newSemanticPalette returns, and both must reach
// the cached styles held by child components, the Bubbles delegates, the
// approval viewport, and the Glamour renderer. Keeping one routine means a
// surface added later cannot be repainted by one path and missed by the other.
func (m *Model) rebuildThemedSurfaces() {
	transcriptAnchor := m.captureTranscriptReflowAnchor()
	approvalAnchor := m.captureApprovalTranscriptAnchor()
	inlineFormAnchor := m.captureInlineFormTranscriptAnchor()
	m.styles = NewStyles(m.isDark, m.themeID)
	// Update spinner style for theme. Unset StatusDot pad — content-grid Prefix
	// owns the activity-rail lead (see renderWorkingLine).
	m.spin.Style = m.styles.StatusDot.UnsetPaddingLeft()
	m.syncComposerAuthority()
	m.scramble.SetDark(m.isDark, m.themeID)
	m.restylePickerOverlays()
	m.restyleViewerModals()
	m.restyleTranscriptSearch()
	if m.goalFormState != nil {
		m.goalFormState.SetTheme(m.isDark, m.themeID)
		m.goalFormState.SetReducedMotion(m.reducedMotion)
	}
	if m.cortexDecision != nil {
		m.cortexDecision.SetTheme(m.isDark, m.themeID)
		m.cortexDecision.reducedMotion = m.reducedMotion
	}
	if m.continuation.card != nil {
		m.continuation.card.SetTheme(m.isDark, m.themeID)
	}
	if m.bobWorkspaceContext.card != nil {
		m.bobWorkspaceContext.card.SetTheme(m.isDark, m.themeID)
	}
	if m.goalInspectorState != nil {
		m.goalInspectorState.SetTheme(m.isDark, m.themeID)
		m.goalInspectorState.SetReducedMotion(m.reducedMotion)
	}
	if m.goalPlan != nil {
		m.goalPlan.SetTheme(m.isDark, m.themeID)
	}
	if m.goalRecoveryState != nil {
		m.goalRecoveryState.SetTheme(m.isDark, m.themeID)
		m.goalRecoveryState.SetReducedMotion(m.reducedMotion)
	}
	if m.pendingApproval != nil && m.approvalState != nil {
		// Approval previews live in a cached Bubbles viewport. Rebuild its
		// styled content immediately so a live theme switch cannot leave the
		// body on the old palette while the title and choices change.
		m.resizeApproval(true)
	}
	// Recreate markdown renderer for new theme.
	if m.width > 0 {
		m.markdownWidth = m.chatContentWidth()
		m.md = NewMarkdownRenderer(m.markdownWidth, m.isDark, m.themeID)
		m.invalidateRenderedCache()
	}
	if m.ready {
		m.refreshTranscript()
	}
	m.refreshInlineFormLayout(inlineFormAnchor)
	m.restoreApprovalTranscriptAnchor(approvalAnchor)
	m.restoreTranscriptReflowAnchor(transcriptAnchor)
}

// handleWindowSize reflows every sized surface for a terminal resize and
// returns the accumulated commands slice.
func (m *Model) handleWindowSize(msg tea.WindowSizeMsg, cmds []tea.Cmd) []tea.Cmd {
	widthChanged := msg.Width != m.width
	heightChanged := msg.Height != m.height
	if widthChanged {
		// Width reflow invalidates both the inspected ToolCard row and the
		// numeric offset saved before inspection. Settle that temporary
		// disclosure first; height-only repaint events preserve it.
		m.cancelReceiptInspection(true)
	}
	// Capture after settling a width-invalidated receipt inspector: closing
	// that temporary disclosure restores its pre-inspection follow intent,
	// which is the semantic position the resize must preserve.
	transcriptAnchor := m.captureTranscriptReflowAnchor()
	approvalAnchor := m.captureApprovalTranscriptAnchor()
	completionAnchor := m.captureCompletionTranscriptAnchor()
	inlineFormAnchor := m.captureInlineFormTranscriptAnchor()
	wasUndersized := m.ready && m.narrowTerminalHint() != ""
	m.width = msg.Width
	m.height = msg.Height
	isUndersized := m.narrowTerminalHint() != ""
	switch {
	case isUndersized:
		m.resetHiddenApprovalChoice()
		m.cancelTerminalInputResume()
	case wasUndersized:
		cmds = append(cmds, m.armTerminalInputResume())
	case m.terminalInputResumeActive():
		cmds = append(cmds, m.armTerminalInputResume())
	}
	if m.goalFormState != nil {
		m.goalFormState.SetSize(m.width, m.height)
	}
	if m.goalPlan != nil {
		m.goalPlan.SetSize(m.chatPaneWidth(), m.height)
	}
	if m.cortexDecision != nil {
		m.cortexDecision.SetSize(m.width, m.height)
	}

	// The conversation always owns the full terminal width. Infrequent
	// controls are presented in overlays.
	viewportWidth := msg.Width - 1
	if viewportWidth < 20 {
		viewportWidth = 20
	}

	contentWidth := m.chatContentWidth()
	markdownChanged := m.md == nil || contentWidth != m.markdownWidth
	if markdownChanged {
		m.markdownWidth = contentWidth
		m.md = NewMarkdownRenderer(contentWidth, m.isDark, m.themeID)
	}

	// Recalculate content height
	contentH := m.viewportHeight()
	oldViewportHeight := m.viewport.Height()

	if !m.ready {
		m.viewport = viewport.New(
			viewport.WithWidth(viewportWidth),
			viewport.WithHeight(contentH),
		)
		// Override viewport KeyMap: keep only pgup/pgdown/ctrl+u/ctrl+d
		m.viewport.KeyMap.PageDown = key.NewBinding(key.WithKeys("pgdown"))
		m.viewport.KeyMap.PageUp = key.NewBinding(key.WithKeys("pgup"))
		m.viewport.KeyMap.HalfPageUp = key.NewBinding(key.WithKeys("ctrl+u"))
		m.viewport.KeyMap.HalfPageDown = key.NewBinding(key.WithKeys("ctrl+d"))
		m.viewport.KeyMap.Up = key.NewBinding(key.WithDisabled())
		m.viewport.KeyMap.Down = key.NewBinding(key.WithDisabled())
		m.viewport.KeyMap.Left = key.NewBinding(key.WithDisabled())
		m.viewport.KeyMap.Right = key.NewBinding(key.WithDisabled())
		m.refreshTranscript()
		m.ready = true
		// Initialize scroll follow intent at the newest transcript row.
		m.markFollowingLatest()
		// Hit regions are populated by the transcript renderer.
		m.toolHitRegions = nil
		m.thinkingHitRegions = nil
	} else {
		m.viewport.SetWidth(viewportWidth)
		m.viewport.SetHeight(contentH)
		if markdownChanged {
			// Re-wrap completed assistant messages only when the actual
			// markdown width changes. Height-only resizes preserve caches.
			m.invalidateRenderedCache()
		} else if widthChanged {
			m.invalidateEntryCache()
		} else if heightChanged &&
			m.transcriptGeometryDependsOnHeight(oldViewportHeight, contentH) {
			// Most transcript blocks depend only on width. Preserve their
			// measured prefix across an ordinary vertical resize; only the
			// centered welcome projection and expanded diff row budgets require
			// a semantic rebuild.
			m.invalidateEntryCache()
		}
		if markdownChanged || widthChanged || heightChanged {
			m.refreshTranscript()
		}
	}

	// Resize help viewport if it's open.
	if m.overlay == OverlayHelp {
		m.resizeHelpViewport(true)
	}
	m.resizePickerOverlays()
	m.resizeViewerModals()
	m.resizeTranscriptSearch()
	if m.pendingApproval != nil && m.approvalState != nil {
		m.resizeApproval(true)
		m.recalcViewportHeight()
	}
	if m.goalInspectorState != nil {
		m.goalInspectorState.SetSize(m.width, m.height)
	}
	if m.goalRecoveryState != nil {
		m.goalRecoveryState.SetSize(m.width, m.height)
	}

	// Input width matches viewport exactly - they're one unified area.
	// Keep placeholders short so mid-width frames (and CI PTYs) never truncate
	// the invite into host-dependent "Ask, @m…" chips.
	if msg.Width < 36 {
		m.input.Placeholder = "Ask · /help"
	} else {
		m.input.Placeholder = "Ask"
	}
	m.input.MaxHeight = composerVisibleRowLimit(msg.Height)
	m.input.SetWidth(viewportWidth)
	m.syncInputHeight()
	m.restoreApprovalTranscriptAnchor(approvalAnchor)
	m.restoreCompletionTranscriptAnchor(completionAnchor)
	m.restoreInlineFormTranscriptAnchor(inlineFormAnchor)
	m.restoreTranscriptReflowAnchor(transcriptAnchor)
	return cmds
}

// handleMouseWheel routes wheel input to the surface that owns pointer
// scrolling; every path consumes the event.
func (m *Model) handleMouseWheel(msg tea.MouseWheelMsg) tea.Cmd {
	// Inline permission requests own wheel input just like document overlays,
	// but remain in normal layout flow so the transcript stays visible. Scroll
	// their bounded preview without moving or changing follow intent below it.
	if m.pendingApproval != nil {
		if m.approvalState != nil {
			m.approvalState.Viewport, _ = m.approvalState.Viewport.Update(msg)
		}
		return nil
	}
	// The external read-scope prompt is an authority-changing inline
	// decision. It is keyboard-first and owns pointer input just like an
	// approval, so the transcript cannot move behind it.
	if m.readScopePrompt != nil {
		return nil
	}
	if m.viewerModalActive() {
		return m.handleViewerWheel(msg)
	}
	// A visible overlay owns pointer input. Scroll document overlays through
	// their own Bubbles viewports, deliver picker input to the active Bubbles
	// child, and swallow wheel events for all other overlays so the hidden
	// transcript cannot move underneath a modal.
	if m.overlay != OverlayNone {
		switch m.overlay {
		case OverlayCortexDecision:
			if m.cortexDecision != nil {
				m.cortexDecision.detail, _ = m.cortexDecision.detail.Update(msg)
				m.cortexDecision.cacheValid = false
			}
		case OverlayHelp:
			m.helpViewport, _ = m.helpViewport.Update(msg)
		case OverlayRuntimeStatus:
			if m.runtimeStatusState != nil {
				m.runtimeStatusState.Viewport, _ = m.runtimeStatusState.Viewport.Update(msg)
			}
		case OverlayGoalInspector:
			if m.goalInspectorState != nil {
				m.goalInspectorState.updateViewport(msg)
			}
		case OverlayGoalRecovery:
			if m.goalRecoveryState != nil {
				_, _ = m.goalRecoveryState.Update(msg)
			}
		case OverlayProviderPicker:
			if m.providerPickerState != nil {
				m.updateProviderPickerWheel(msg)
			}
		}
		return nil
	}

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

// handleMouseClickMsg swallows clicks behind modal surfaces and forwards
// left clicks to transcript hit testing. handled reports whether Update must
// return immediately without running the shared component tail.
func (m *Model) handleMouseClickMsg(msg tea.MouseClickMsg) (cmd tea.Cmd, handled bool) {
	if m.pendingApproval != nil || m.readScopePrompt != nil {
		return nil, true
	}
	if m.viewerModalActive() {
		// The exact viewer frame owns pointer focus and diff-row selection.
		// Every click remains consumed even when it lands on modal chrome.
		return m.handleViewerClick(msg), true
	}
	// Provider selection is a Bubbles list and explicitly owns pointer
	// interaction while visible.
	if m.overlay == OverlayProviderPicker && m.providerPickerState != nil {
		return m.selectProviderPickerPointer(msg), true
	}
	// Other modal and inline decision surfaces are intentionally keyboard-first.
	// Until a child explicitly owns pointer interaction, clicks are swallowed
	// rather than reaching ToolCards behind an authority-changing prompt.
	if m.overlay != OverlayNone {
		return nil, true
	}
	if msg.Button == tea.MouseLeft {
		return m.handleMouseClick(msg.X, msg.Y), true
	}
	return nil, false
}

// handlePasteMsg owns composer paste insertion. handled reports whether
// Update must return immediately with cmd instead of running the shared
// component tail (which would insert the paste a second time).
func (m *Model) handlePasteMsg(msg tea.PasteMsg) (cmd tea.Cmd, handled bool) {
	if m.composerEditable() {
		if paths, ok := pastedImagePaths(msg.Content); ok {
			return m.beginPastedImageFileAttachments(paths), true
		}
		// The parent owns insertion and any safety prompt. Do not forward this
		// PasteMsg to the textarea or the child would insert it a second time.
		return m.insertPasteWithReview(msg.Content), true
	}
	return nil, false
}

// handleClipboardImagePaste validates a clipboard image capture and starts
// the attachment flow. handled reports whether Update must return
// immediately with cmd.
func (m *Model) handleClipboardImagePaste(msg ClipboardImagePasteMsg) (cmd tea.Cmd, handled bool) {
	if !m.composerEditable() {
		return nil, false
	}
	if msg.Err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: "Paste image: clipboard has no supported image data."})
		m.invalidateEntryCache()
		m.refreshTranscript()
		m.gotoBottomIfFollowing()
		return nil, false
	}
	return m.beginImageBytesAttachment(msg.Name, msg.Data), true
}
