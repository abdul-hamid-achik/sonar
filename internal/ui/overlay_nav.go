package ui

// openSettingsChild records modal hierarchy before opening a child surface.
// If the child cannot open (for example, no models are available), Settings
// remains active and no stale parent is retained.
func (m *Model) openSettingsChild(open func()) {
	m.overlayParent = OverlaySettings
	open()
	if m.overlay == OverlaySettings {
		m.overlayParent = OverlayNone
	}
}

// closeOverlayToParent returns a Settings child to the refreshed root palette.
// Directly opened overlays dismiss to chat.
func (m *Model) closeOverlayToParent() {
	if m.overlayParent == OverlaySettings && m.settingsPickerState != nil {
		m.overlayParent = OverlayNone
		m.overlay = OverlaySettings
		m.refreshSettingsPicker()
		m.input.Blur()
		return
	}
	m.dismissOverlay()
}

func (m *Model) dismissOverlay() {
	if m.overlay == OverlayTranscriptSearch && m.transcriptSearch != nil {
		_ = m.closeTranscriptSearch(true)
		return
	}
	m.overlayParent = OverlayNone
	m.overlay = OverlayNone
	m.input.Focus()
}

func (m *Model) closeHelpOverlay() {
	m.closeOverlayToParent()
}

func (m *Model) overlayCloseLabel() string {
	if m.overlayParent == OverlaySettings {
		return "back"
	}
	return "close"
}
