package config

import "testing"

// Enumerating remote provider types instead of excluding the one local runtime
// is what demoted a correctly configured hosted provider to the local path and
// produced "no compatible local chat model is installed" for opencode-zen.
// Every catalog provider must read as remote and as selectable.
func TestEveryCatalogProviderIsRemoteAndSelectable(t *testing.T) {
	for _, id := range []string{
		"deepseek", "opencode-zen", "groq", "xai",
		"cerebras", "moonshot", "anthropic", "gemini",
	} {
		if !(ProviderProfile{Type: id}).IsRemote() {
			t.Errorf("%q is not remote", id)
		}
		if !IsKnownProviderType(id) {
			t.Errorf("%q is not selectable", id)
		}
	}

	if (ProviderProfile{Type: ProviderTypeOllama}).IsRemote() {
		t.Error("the local runtime reported as remote")
	}
	if IsKnownProviderType("not-a-provider") {
		t.Error("an unknown provider reported as selectable")
	}
	// A private endpoint stays selectable without any catalog entry.
	if !IsKnownProviderType(ProviderTypeOpenAICompatible) {
		t.Error("openai_compatible is not selectable")
	}
}

// A catalog provider must arrive fully configured from its name alone —
// that is the whole point of the seam.
func TestCatalogProvidersResolveCompleteProfiles(t *testing.T) {
	for _, id := range []string{"deepseek", "opencode-zen", "groq", "cerebras", "moonshot"} {
		resolved := ProviderProfile{Type: id}.Resolve()
		if resolved.BaseURL == "" {
			t.Errorf("%q resolved no base_url", id)
		}
		if resolved.Model == "" {
			t.Errorf("%q resolved no model", id)
		}
		if resolved.APIKeyEnv == "" {
			t.Errorf("%q resolved no api_key_env", id)
		}
		if resolved.ContextSize <= 0 {
			t.Errorf("%q resolved a non-positive context size", id)
		}
	}
}
