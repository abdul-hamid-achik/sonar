package ui

import (
	"errors"
	"strings"
	"testing"
)

type modelPreferenceStoreStub struct {
	model          string
	provider       string
	theme          string
	themeSets      int
	themeSetErr    error
	setCalls       int
	clearCalls     int
	providerSets   int
	providerClears int
	setErr         error
	clearErr       error
	providerSetErr error
}

func (s *modelPreferenceStoreStub) SetTheme(id string) error {
	s.themeSets++
	if s.themeSetErr != nil {
		return s.themeSetErr
	}
	s.theme = id
	return nil
}

func (s *modelPreferenceStoreStub) SetManualModel(model string) error {
	s.setCalls++
	if s.setErr != nil {
		return s.setErr
	}
	s.model = model
	return nil
}

func (s *modelPreferenceStoreStub) ClearManualModel() error {
	s.clearCalls++
	if s.clearErr != nil {
		return s.clearErr
	}
	s.model = ""
	return nil
}

func (s *modelPreferenceStoreStub) SetManualProvider(name string) error {
	s.providerSets++
	if s.providerSetErr != nil {
		return s.providerSetErr
	}
	s.provider = name
	return nil
}

func (s *modelPreferenceStoreStub) ClearManualProvider() error {
	s.providerClears++
	s.provider = ""
	return nil
}

func TestManualModelSelectionPersistsAcrossRestarts(t *testing.T) {
	m := newTestModel(t)
	store := &modelPreferenceStoreStub{}
	m.SetModelPreferenceStore(store)
	m.model = "qwen3.5:2b"

	if !m.switchSelectedModel("qwen3.5:4b") {
		t.Fatal("manual selection failed")
	}
	if store.model != "qwen3.5:4b" || store.setCalls != 1 || !m.modelPinned {
		t.Fatalf("preference=%q calls=%d pinned=%v", store.model, store.setCalls, m.modelPinned)
	}
	if !m.switchSelectedModel("qwen3.5:4b") {
		t.Fatal("idempotent manual selection failed")
	}
	if store.setCalls != 2 {
		t.Fatalf("idempotent selection did not repair preference: calls=%d", store.setCalls)
	}
}

func TestManualPreferenceFailureDoesNotPersistSensitiveError(t *testing.T) {
	m := newTestModel(t)
	m.SetModelPreferenceStore(&modelPreferenceStoreStub{setErr: errors.New("/Users/private/runtime-preferences.json")})
	m.model = "qwen3.5:2b"
	if !m.switchSelectedModel("qwen3.5:4b") {
		t.Fatal("selection failed")
	}
	for _, entry := range m.entries {
		if strings.Contains(entry.Content, "/Users/private") {
			t.Fatalf("preference error leaked into transcript: %#v", entry)
		}
	}
}

var _ ModelPreferenceStore = (*modelPreferenceStoreStub)(nil)
