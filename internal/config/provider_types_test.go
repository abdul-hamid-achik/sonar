package config

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/catalog"
)

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

// endpointOverrideEnv maps each catalog provider whose api_endpoint is an
// environment-variable template to the variable it names. Catwalk writes these
// four as "$VAR" instead of a URL: three are operator overrides, and Azure
// OpenAI's host is genuinely per-resource.
var endpointOverrideEnv = map[string]string{
	"anthropic": "ANTHROPIC_API_ENDPOINT",
	"openai":    "OPENAI_API_ENDPOINT",
	"gemini":    "GEMINI_API_ENDPOINT",
	"azure":     "AZURE_OPENAI_API_ENDPOINT",
}

// clearEndpointOverrides makes endpoint resolution independent of whatever the
// developer happens to export. Resolve reads the process environment, so
// without this a machine with ANTHROPIC_API_ENDPOINT set would quietly test a
// different code path than CI.
func clearEndpointOverrides(t *testing.T) {
	t.Helper()
	for _, env := range endpointOverrideEnv {
		t.Setenv(env, "")
	}
}

// A catalog provider must arrive fully configured from its name alone — that is
// the whole point of the seam — and what it arrives with must be a URL.
//
// This covers every provider in the catalog rather than a five-name spot check,
// and asserts the shape of the resolved base_url rather than only that it is
// non-empty. The literal string "$OPENAI_API_ENDPOINT" is non-empty, parses as
// a valid URL host ("$" is a legal sub-delims character), and therefore cleared
// both validateProviderProfile and parseProviderBaseURL before failing as a DNS
// lookup on the first real request. A non-empty assertion over five hand-picked
// providers is exactly why that shipped.
func TestCatalogProvidersResolveCompleteProfiles(t *testing.T) {
	clearEndpointOverrides(t)

	ids := catalog.ProviderIDs()
	if len(ids) < 10 {
		t.Fatalf("catalog exposed %d providers; the snapshot looks broken", len(ids))
	}

	for _, providerID := range ids {
		id := string(providerID)
		t.Run(id, func(t *testing.T) {
			resolved := ProviderProfile{Type: id}.Resolve()

			if strings.Contains(resolved.BaseURL, "$") {
				t.Errorf("resolved base_url = %q, an unresolved environment template", resolved.BaseURL)
			}
			if strings.HasPrefix(resolved.APIKeyEnv, "$") {
				t.Errorf("resolved api_key_env = %q, want a bare variable name", resolved.APIKeyEnv)
			}
			if resolved.Model == "" {
				t.Errorf("%q resolved no model", id)
			}
			if resolved.ContextSize <= 0 {
				t.Errorf("%q resolved a non-positive context size", id)
			}

			provider, ok := catalog.LookupProvider(providerID)
			if !ok {
				t.Fatalf("%q is listed by the catalog but cannot be looked up", id)
			}
			endpoint := strings.TrimSpace(provider.APIEndpoint)
			switch {
			case endpoint == "":
				// bedrock and vertexai address and authenticate through a cloud
				// credential chain; they name no single endpoint to resolve.
			case strings.HasPrefix(endpoint, "$"):
				// No override is exported here, so the only acceptable outcomes
				// are the documented public endpoint or nothing at all.
				if want := providerEndpointFallbacks[id]; resolved.BaseURL != want {
					t.Errorf("resolved base_url = %q for template %q, want %q", resolved.BaseURL, endpoint, want)
				}
			default:
				if resolved.BaseURL != endpoint {
					t.Errorf("resolved base_url = %q, want the catalog literal %q", resolved.BaseURL, endpoint)
				}
			}

			if resolved.BaseURL == "" {
				return
			}
			// Whatever resolution produced must be a base_url the validator
			// accepts on its own terms. Probing through a synthetic profile
			// isolates the URL rules from the providers that legitimately carry
			// no api_key_env (bedrock, vertexai, copilot).
			probe := ProviderProfile{
				Type:      ProviderTypeOpenAICompatible,
				BaseURL:   resolved.BaseURL,
				Model:     "probe-model",
				APIKeyEnv: "PROBE_API_KEY",
			}
			if err := ValidateProviderProfile("probe", probe); err != nil {
				t.Errorf("resolved base_url %q fails base_url validation: %v", resolved.BaseURL, err)
			}
		})
	}
}

// An exported override is used verbatim, for every provider whose catalog entry
// names one. Resolution lives in the config layer precisely so this is not a
// property of the one dialect that remembered to write a guard.
func TestCatalogEndpointTemplateResolvesFromEnvironment(t *testing.T) {
	const override = "https://gateway.internal.example/v1"
	for id, env := range endpointOverrideEnv {
		t.Run(id, func(t *testing.T) {
			clearEndpointOverrides(t)
			t.Setenv(env, override)

			resolved := ProviderProfile{Type: id}.Resolve()
			if resolved.BaseURL != override {
				t.Fatalf("resolved base_url = %q, want the exported %s value %q", resolved.BaseURL, env, override)
			}
			if err := ValidateProviderProfile(id, resolved); err != nil {
				t.Fatalf("an overridden profile failed validation: %v", err)
			}
		})
	}
}

// An explicit base_url always wins: resolution fills a gap, it does not
// override a decision the operator already made.
func TestExplicitBaseURLBeatsEndpointOverride(t *testing.T) {
	clearEndpointOverrides(t)
	t.Setenv("ANTHROPIC_API_ENDPOINT", "https://override.example")

	resolved := ProviderProfile{Type: "anthropic", BaseURL: "https://explicit.example"}.Resolve()
	if resolved.BaseURL != "https://explicit.example" {
		t.Fatalf("resolved base_url = %q, want the explicit value preserved", resolved.BaseURL)
	}
}

// With no override exported, a template resolves to the provider's documented
// public endpoint — or to nothing, which surfaces as a load-time config error
// naming the variable. Either way the template itself never survives.
func TestCatalogEndpointTemplateWithoutOverride(t *testing.T) {
	tests := []struct {
		id      string
		want    string
		wantErr bool
	}{
		{id: "anthropic", want: AnthropicDefaultAPIEndpoint},
		{id: "openai", want: OpenAIDefaultAPIEndpoint},
		// Azure OpenAI's host is per-resource and Gemini has no Google dialect
		// in sonar yet, so neither gets an invented default. Absent is honest;
		// a guessed endpoint would only move the failure to the first request.
		{id: "gemini", wantErr: true},
		{id: "azure", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			clearEndpointOverrides(t)

			resolved := ProviderProfile{Type: test.id}.Resolve()
			if resolved.BaseURL != test.want {
				t.Fatalf("resolved base_url = %q, want %q", resolved.BaseURL, test.want)
			}
			err := ValidateProviderProfile(test.id, resolved)
			if test.wantErr {
				if err == nil {
					t.Fatal("an unresolvable endpoint was accepted at config load")
				}
				if !strings.Contains(err.Error(), endpointOverrideEnv[test.id]) {
					t.Errorf("error does not name the variable to export: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a resolved profile failed validation: %v", err)
			}
		})
	}
}

// An environment variable is not a privileged input. Everything resolution
// produces goes through the same base_url rules as a value typed into YAML, so
// an override cannot become a way to inject an unvalidated endpoint — and the
// rejection must not echo the value back.
func TestCatalogEndpointOverrideIsStillValidated(t *testing.T) {
	tests := []struct {
		name     string
		override string
	}{
		{name: "plaintext http to a remote host", override: "http://evil.example/v1"},
		{name: "embedded credentials", override: "https://user:super-secret@evil.example/v1"},
		{name: "query string", override: "https://evil.example/v1?token=super-secret"},
		{name: "fragment", override: "https://evil.example/v1#super-secret"},
		{name: "another template", override: "$SOME_OTHER_ENDPOINT"},
		{name: "not a url at all", override: "https://not a url"},
		{name: "non-http scheme", override: "file:///etc/passwd"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearEndpointOverrides(t)
			t.Setenv("OPENAI_API_ENDPOINT", test.override)

			resolved := ProviderProfile{Type: "openai"}.Resolve()
			err := ValidateProviderProfile("openai", resolved)
			if err == nil {
				t.Fatalf("environment override %q was accepted unvalidated", test.override)
			}
			if strings.Contains(err.Error(), "super-secret") {
				t.Fatalf("validation error echoed the override: %v", err)
			}
		})
	}
}

// The same rule reaches a base_url written by hand. No real host contains "$",
// so a template in YAML is a mistake worth catching at load rather than a
// hostname worth resolving.
func TestProviderProfileRejectsHandWrittenEndpointTemplate(t *testing.T) {
	profile := ProviderProfile{
		Type:      ProviderTypeOpenAICompatible,
		BaseURL:   "$MY_PRIVATE_GATEWAY",
		Model:     "some-model",
		APIKeyEnv: "MY_PRIVATE_GATEWAY_API_KEY",
	}
	err := ValidateProviderProfile("private", profile)
	if err == nil {
		t.Fatal("an unresolved template base_url was accepted")
	}
	if !strings.Contains(err.Error(), "template") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}
