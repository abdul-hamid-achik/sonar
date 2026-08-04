package main

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

func localOnlyCatalogConfig() *config.Config {
	base := config.Defaults()
	cfg := &base
	cfg.Privacy.LocalOnly = true
	cfg.Ollama.Model = "qwen3.5:2b"
	cfg.Provider = config.ProviderConfig{
		Active: "ollama",
		Profiles: map[string]config.ProviderProfile{
			"ollama": {Type: string(config.ProviderTypeOllama), BaseURL: "http://localhost:11434", Model: "qwen3.5:2b"},
			"remote": {Type: string(config.ProviderTypeOpenAICompatible), BaseURL: "https://api.example.com/v1", Model: "gpt-x", APIKeyEnv: "EXAMPLE_KEY"},
		},
	}
	return cfg
}

// A saved /provider selection is applied after Config.Validate. It must still
// re-validate, but privacy.local_only is no longer part of that decision: it
// bounds tool endpoints, and every sonar provider is remote by construction.
func TestSavedProviderPreferenceRestoresARemoteProfile(t *testing.T) {
	cfg := localOnlyCatalogConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("baseline config must be valid: %v", err)
	}

	if warning := restoreManualProviderPreference(cfg, "remote"); warning != "" {
		t.Fatalf("remote preference rejected under local_only: %q", warning)
	}
	if cfg.Provider.Active != "remote" {
		t.Fatalf("preference did not take effect, active = %q", cfg.Provider.Active)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config was left invalid after restoring a preference: %v", err)
	}
}

// An unknown name must still be refused, or the re-validation would be useless.
func TestSavedProviderPreferenceRejectsAnUnknownProfile(t *testing.T) {
	cfg := localOnlyCatalogConfig()

	warning := restoreManualProviderPreference(cfg, "not-a-profile")

	if warning == "" {
		t.Fatal("an unknown provider name was restored with no warning")
	}
	if !strings.Contains(warning, "not-a-profile") {
		t.Fatalf("warning does not name the rejected provider: %q", warning)
	}
	if cfg.Provider.Active != "ollama" {
		t.Fatalf("rejected preference changed the active provider to %q", cfg.Provider.Active)
	}
}

// A local profile must still restore normally, or the guard would be useless.
func TestSavedProviderPreferenceRestoresALocalProfile(t *testing.T) {
	cfg := localOnlyCatalogConfig()
	cfg.Provider.Active = "remote-placeholder"
	cfg.Provider.Profiles["remote-placeholder"] = cfg.Provider.Profiles["ollama"]

	if warning := restoreManualProviderPreference(cfg, "ollama"); warning != "" {
		t.Fatalf("a local profile was rejected: %s", warning)
	}
	if cfg.Provider.Active != "ollama" {
		t.Fatalf("active provider = %q, want ollama", cfg.Provider.Active)
	}
}

func TestSavedProviderPreferenceIgnoresUnknownNames(t *testing.T) {
	cfg := localOnlyCatalogConfig()
	if warning := restoreManualProviderPreference(cfg, "not-a-profile"); warning == "" {
		t.Fatal("an unknown provider name was accepted silently")
	}
	if cfg.Provider.Active != "ollama" {
		t.Fatalf("unknown preference changed the active provider to %q", cfg.Provider.Active)
	}
}
