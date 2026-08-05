package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// Switching between two providers must work in both directions. This used to
// switch to xai and back to a local Ollama runtime; `ollama` now names Ollama
// Cloud, so both directions are hosted. A switch that dropped to a local path
// would be the bug here, not the expectation.
func TestSwitchProviderRemoteAndBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.5"},{"id":"deepseek-v4-flash"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	t.Setenv("XAI_API_KEY", "test-key")
	t.Setenv("OLLAMA_API_KEY", "test-key")

	manager := NewModelManager("http://127.0.0.1:9", 4096)
	defer manager.Close()
	manager.ConfigureProviderCatalog(config.ProviderConfig{
		Active: "ollama",
		Profiles: map[string]config.ProviderProfile{
			// Pointed at the loopback fake rather than left to resolve: the
			// ollama default is now ollama.com, and a unit test must not reach
			// a metered endpoint.
			"ollama": {
				Type:    config.ProviderTypeOllama,
				BaseURL: server.URL + "/v1",
				Model:   "deepseek-v4-flash",
			},
			"xai": {
				Type:    config.ProviderTypeXAI,
				BaseURL: server.URL + "/v1",
				Model:   "grok-4.5",
			},
		},
	}, false, "qwen3.5:2b")

	if err := manager.SwitchProvider("xai"); err != nil {
		t.Fatal(err)
	}
	if !manager.RemoteProvider() || manager.Model() != "grok-4.5" {
		t.Fatalf("remote state: remote=%v model=%q", manager.RemoteProvider(), manager.Model())
	}
	if manager.ActiveProviderName() != "xai" {
		t.Fatalf("active = %q", manager.ActiveProviderName())
	}

	if err := manager.SwitchProvider("ollama"); err != nil {
		t.Fatal(err)
	}
	if !manager.RemoteProvider() {
		t.Fatal("switching to ollama dropped to a local path; it names ollama.com now")
	}
	if manager.Model() != "deepseek-v4-flash" {
		t.Fatalf("switched model = %q, want the ollama profile's model", manager.Model())
	}
	if manager.ActiveProviderName() != "ollama" {
		t.Fatalf("active = %q", manager.ActiveProviderName())
	}
}

func TestSwitchProviderMissingKey(t *testing.T) {
	t.Setenv("XAI_API_KEY", "")
	manager := NewModelManager("http://127.0.0.1:9", 4096)
	defer manager.Close()
	manager.ConfigureProviderCatalog(config.ProviderConfig{
		Profiles: map[string]config.ProviderProfile{
			"xai": {Type: config.ProviderTypeXAI},
		},
	}, false, "qwen3.5:2b")
	err := manager.SwitchProvider("xai")
	if err == nil || !strings.Contains(err.Error(), "XAI_API_KEY") {
		t.Fatalf("expected missing key error, got %v", err)
	}
}

// privacy.local_only bounds tool endpoints, not inference. A remote switch must
// therefore succeed under it — sonar has no local inference path to fall back
// to, so rejecting here would strand the harness with no usable provider.
func TestSwitchProviderIgnoresLocalOnly(t *testing.T) {
	t.Setenv("TEST_PROVIDER_API_KEY", "test-key")
	manager := NewModelManager("http://127.0.0.1:9", 4096)
	defer manager.Close()
	manager.ConfigureProviderCatalog(config.ProviderConfig{
		Active: "ollama",
		Profiles: map[string]config.ProviderProfile{
			"ollama": {Type: config.ProviderTypeOllama},
			"remote": {
				Type:      config.ProviderTypeOpenAICompatible,
				BaseURL:   "https://example.com/v1",
				Model:     "test-model",
				APIKeyEnv: "TEST_PROVIDER_API_KEY",
			},
		},
	}, true, "qwen3.5:2b")

	if err := manager.SwitchProvider("remote"); err != nil {
		t.Fatalf("remote switch rejected under local_only: %v", err)
	}
	if !manager.RemoteProvider() || manager.ActiveProviderName() != "remote" {
		t.Fatalf("switch did not take effect: remote=%v active=%q", manager.RemoteProvider(), manager.ActiveProviderName())
	}
}

func TestSwitchProviderContextCanceledBeforeMutation(t *testing.T) {
	t.Setenv("XAI_API_KEY", "test-key")
	manager := NewModelManager("http://127.0.0.1:9", 4096)
	defer manager.Close()
	manager.ConfigureProviderCatalog(config.ProviderConfig{
		Active: "ollama",
		Profiles: map[string]config.ProviderProfile{
			"ollama": {Type: config.ProviderTypeOllama},
			"xai": {
				Type:    config.ProviderTypeXAI,
				BaseURL: "https://api.x.ai/v1",
				Model:   "grok-4.5",
			},
		},
	}, false, "qwen3.5:2b")
	originalProvider := manager.ActiveProviderName()
	originalModel := manager.Model()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := manager.SwitchProviderContext(ctx, "xai")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SwitchProviderContext error = %v, want context.Canceled", err)
	}
	if manager.RemoteProvider() {
		t.Fatal("canceled switch mutated the active provider")
	}
	if got := manager.ActiveProviderName(); got != originalProvider {
		t.Fatalf("active provider = %q, want unchanged %q", got, originalProvider)
	}
	if got := manager.Model(); got != originalModel {
		t.Fatalf("model = %q, want unchanged %q", got, originalModel)
	}
}
