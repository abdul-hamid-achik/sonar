package ui

// ModelPreferenceStore is the narrow user-runtime persistence boundary for a
// manually selected model and optional provider profile. It is intentionally
// independent from session state.
type ModelPreferenceStore interface {
	SetManualModel(string) error
	ClearManualModel() error
	SetManualProvider(string) error
	ClearManualProvider() error
	SetTheme(string) error
}

// SetModelPreferenceStore enables restart persistence for explicit /model,
// /provider, and picker selections. Startup flags and agent profiles remain
// separate runtime authorities and are never written through this store.
func (m *Model) SetModelPreferenceStore(store ModelPreferenceStore) {
	m.modelPreferenceStore = store
}

func (m *Model) saveManualModelPreference(model string) {
	if m.modelPreferenceStore == nil {
		return
	}
	if err := m.modelPreferenceStore.SetManualModel(model); err != nil {
		if m.logger != nil {
			m.logger.Error("save model preference", "error", err)
		}
		m.entries = append(m.entries, ChatEntry{
			Kind:    "error",
			Content: "Model selected, but its restart preference could not be saved.",
		})
	}
}

func (m *Model) saveManualProviderPreference(name string) {
	if m.modelPreferenceStore == nil {
		return
	}
	if err := m.modelPreferenceStore.SetManualProvider(name); err != nil {
		if m.logger != nil {
			m.logger.Error("save provider preference", "error", err)
		}
		m.entries = append(m.entries, ChatEntry{
			Kind:    "error",
			Content: "Provider selected, but its restart preference could not be saved.",
		})
	}
}
