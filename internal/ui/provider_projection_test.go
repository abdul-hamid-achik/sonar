package ui

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestProviderProjectionDropsTransportConfiguration(t *testing.T) {
	descriptorType := reflect.TypeFor[llm.ProviderDescriptor]()
	if _, ok := descriptorType.FieldByName("BaseURL"); ok {
		t.Fatal("provider catalog exposes BaseURL to the UI")
	}

	itemType := reflect.TypeFor[providerItem]()
	if itemType.NumField() != 1 || itemType.Field(0).Type != reflect.TypeFor[ProviderOptionPresentation]() {
		t.Fatalf("provider item retains a non-presentation field: %#v", itemType)
	}
	presentationType := reflect.TypeFor[ProviderOptionPresentation]()
	for _, forbidden := range []string{"BaseURL", "APIKeyEnv", "Headers", "Client", "CredentialValue"} {
		if _, ok := presentationType.FieldByName(forbidden); ok {
			t.Fatalf("provider presentation exposes forbidden field %q", forbidden)
		}
	}
}

func TestProviderProjectionAdmitsOnlyValidatedCredentialHint(t *testing.T) {
	options := providerOptionPresentations([]llm.ProviderDescriptor{
		{
			Name:       "xai-ready",
			Type:       "xai",
			Model:      "grok",
			APIKeyEnv:  "XAI_API_KEY",
			Remote:     true,
			KeyPresent: true,
		},
		{
			Name:      "xai-missing",
			Type:      "xai",
			Model:     "grok",
			APIKeyEnv: "XAI_API_KEY",
			Remote:    true,
		},
		{
			Name:      "unsafe-env",
			Type:      "openai-compatible",
			APIKeyEnv: "TOKEN\x1b]8;;https://secret.invalid\x07",
			Remote:    true,
		},
	})
	if len(options) != 3 {
		t.Fatalf("provider options = %d, want 3", len(options))
	}
	if !options[0].Selectable || options[0].Credential != ProviderCredentialReady {
		t.Fatalf("ready provider projection = %#v", options[0])
	}
	if options[1].Selectable || options[1].CredentialHint != "$XAI_API_KEY" ||
		options[1].DisabledReason != "credential unavailable" {
		t.Fatalf("missing provider projection = %#v", options[1])
	}
	if options[2].Selectable || options[2].CredentialHint != "" {
		t.Fatalf("unsafe credential hint crossed the UI boundary: %#v", options[2])
	}
	for _, option := range options {
		item := providerItem{presentation: option}
		rendered := item.Title() + "\n" + item.Description() + "\n" + item.FilterValue()
		if strings.Contains(rendered, "secret.invalid") || strings.ContainsRune(rendered, '\x1b') {
			t.Fatalf("provider presentation retained terminal payload: %q", rendered)
		}
	}
}

func TestUnavailableProviderFailsClosedForKeyboardAndPointer(t *testing.T) {
	m := newTestModel(t)
	descriptor := llm.ProviderDescriptor{
		Name:      "xai",
		Type:      "xai",
		Model:     "grok",
		APIKeyEnv: "XAI_API_KEY",
		Remote:    true,
	}
	m.providerPickerState = newProviderPickerState(
		[]llm.ProviderDescriptor{descriptor},
		"",
		m.width,
		m.height,
		m.isDark,
		m.themeID,
	)
	m.overlay = OverlayProviderPicker

	updated, cmd := m.Update(enterKey())
	m = updated.(*Model)
	if cmd != nil || m.providerSwitchRunning || m.overlay != OverlayProviderPicker ||
		m.providerPickerState == nil {
		t.Fatalf(
			"keyboard selected unavailable provider: cmd=%v running=%v overlay=%v state=%v",
			cmd != nil,
			m.providerSwitchRunning,
			m.overlay,
			m.providerPickerState != nil,
		)
	}
	if !strings.Contains(m.providerPickerState.List.Title, "credential unavailable") {
		t.Fatalf("keyboard disabled reason = %q", m.providerPickerState.List.Title)
	}

	x, y := providerPickerItemPoint(t, m, 0)
	updated, cmd = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m = updated.(*Model)
	if cmd != nil || m.providerSwitchRunning || m.overlay != OverlayProviderPicker ||
		m.providerPickerState == nil {
		t.Fatalf(
			"pointer selected unavailable provider: cmd=%v running=%v overlay=%v state=%v",
			cmd != nil,
			m.providerSwitchRunning,
			m.overlay,
			m.providerPickerState != nil,
		)
	}
}
