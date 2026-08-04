package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

type selectionRouterStub struct {
	models      []string
	resolved    string
	headless    string
	headlessHit bool
}

func (r *selectionRouterStub) SetAvailableModels(models []string) {
	r.models = append([]string(nil), models...)
}

func (r *selectionRouterStub) ResolveAvailableModel(string) string { return r.resolved }
func (r *selectionRouterStub) SelectModel(string) string           { return r.headless }
func (r *selectionRouterStub) SelectModelForMode(string, config.ModeContext) string {
	r.headlessHit = true
	return r.headless
}
func (r *selectionRouterStub) RecordOverride(string, string) {}
func (r *selectionRouterStub) GetModelForCapability(config.ModelCapability) string {
	return r.headless
}
func (r *selectionRouterStub) ListModels() []config.Model { return config.DefaultModels() }

func TestResolveStartupModelValidatesPinnedLocalInventory(t *testing.T) {
	cfg := config.DefaultModelConfig()
	discovered := []string{"nomic-embed-text", "qwen3.5:2b", "gemma4:e2b", "unknown:latest"}
	router := &selectionRouterStub{resolved: "qwen3.5:2b"}

	selected, models, err := resolveStartupModel("gemma4:e2b", true, true, &cfg, discovered, discovered, true, router)
	if err != nil {
		t.Fatalf("locally installed profile pin rejected: %v", err)
	}
	if selected != "gemma4:e2b" {
		t.Fatalf("selected = %q, want profile pin", selected)
	}
	wantModels := discovered
	if !reflect.DeepEqual(models, wantModels) {
		t.Fatalf("model list = %#v, want %#v", models, wantModels)
	}
	if !reflect.DeepEqual(router.models, wantModels) {
		t.Fatalf("router inventory = %#v, want %#v", router.models, wantModels)
	}

	_, _, err = resolveStartupModel("qwen3.5:cloud", true, true, &cfg, discovered, discovered, true, router)
	if err == nil {
		t.Fatal("cloud profile pin accepted")
	}
	_, _, err = resolveStartupModel("qwen3.5:4b", true, true, &cfg, discovered, discovered, true, router)
	if err == nil {
		t.Fatal("missing local --model pin accepted")
	}
}

func TestResolveStartupModelAcceptsPinnedImplicitLatestTag(t *testing.T) {
	cfg := config.ModelConfig{
		Models:       []config.Model{{Name: "llama3", Family: config.FamilyLlama}},
		DefaultModel: "llama3",
	}
	router := &selectionRouterStub{resolved: "llama3"}
	selected, models, err := resolveStartupModel("llama3", true, true, &cfg, []string{"llama3:latest"}, []string{"llama3:latest"}, true, router)
	if err != nil {
		t.Fatalf("implicit :latest pin rejected: %v", err)
	}
	if selected != "llama3" || !reflect.DeepEqual(models, []string{"llama3:latest"}) {
		t.Fatalf("selection = %q, models=%#v", selected, models)
	}
}

func TestResolveStartupModelAcceptsPinnedOrnith(t *testing.T) {
	cfg := config.DefaultModelConfig()
	router := &selectionRouterStub{resolved: "qwen3.5:2b"}
	selected, models, err := resolveStartupModel(
		"ornith:latest", true, true, &cfg, []string{"ornith:latest"}, []string{"ornith:latest"}, true, router,
	)
	if err != nil {
		t.Fatalf("installed Ornith pin rejected: %v", err)
	}
	if selected != "ornith:latest" || !reflect.DeepEqual(models, []string{"ornith:latest"}) {
		t.Fatalf("selection = %q, models=%#v", selected, models)
	}
}

func TestResolveStartupModelAcceptsPinnedOllamaCloudWhenLocalOnlyDisabled(t *testing.T) {
	cfg := config.DefaultModelConfig()
	router := &selectionRouterStub{resolved: "qwen3.5:cloud"}
	selected, models, err := resolveStartupModel(
		"qwen3.5:cloud", true, false, &cfg, []string{"qwen3.5:cloud"}, nil, true, router,
	)
	if err != nil {
		t.Fatalf("pinned Ollama Cloud model rejected with local-only disabled: %v", err)
	}
	if selected != "qwen3.5:cloud" || !reflect.DeepEqual(models, []string{"qwen3.5:cloud"}) {
		t.Fatalf("selection = %q models=%#v", selected, models)
	}
	if len(router.models) != 0 {
		t.Fatalf("manual cloud leaked into router inventory: %#v", router.models)
	}
}

func TestResolveStartupModelNeverAutomaticallySelectsCloud(t *testing.T) {
	cfg := config.DefaultModelConfig()
	router := &selectionRouterStub{resolved: "qwen3.5:cloud"}
	selected, models, err := resolveStartupModel(
		"qwen3.5:cloud", false, false, &cfg,
		[]string{"local-code", "qwen3.5:cloud"}, []string{"local-code"}, true, router,
	)
	if err != nil {
		t.Fatalf("local fallback rejected: %v", err)
	}
	if selected != "local-code" || !reflect.DeepEqual(models, []string{"local-code", "qwen3.5:cloud"}) {
		t.Fatalf("selection = %q models=%#v", selected, models)
	}
	if !reflect.DeepEqual(router.models, []string{"local-code"}) {
		t.Fatalf("router inventory = %#v, want local only", router.models)
	}
}

func TestResolveStartupModelUsesVerifiedLocationInsteadOfCloudLikeTag(t *testing.T) {
	cfg := config.DefaultModelConfig()
	router := &selectionRouterStub{}
	selected, _, err := resolveStartupModel(
		"provider-cloud:latest", true, true, &cfg, []string{"provider-cloud:latest"}, []string{"provider-cloud:latest"}, true, router,
	)
	if err != nil || selected != "provider-cloud:latest" {
		t.Fatalf("verified local cloud-like tag selected=%q err=%v", selected, err)
	}
}

func TestResolveStartupModelFallsBackToCustomOnlyInventory(t *testing.T) {
	cfg := config.DefaultModelConfig()
	router := &selectionRouterStub{resolved: ""}
	selected, models, err := resolveStartupModel(
		cfg.DefaultModel, false, true, &cfg, []string{"custom-code:latest"}, []string{"custom-code:latest"}, true, router,
	)
	if err != nil {
		t.Fatalf("custom-only Ollama inventory rejected: %v", err)
	}
	if selected != "custom-code:latest" || !reflect.DeepEqual(models, []string{"custom-code:latest"}) {
		t.Fatalf("selection = %q models=%#v", selected, models)
	}
}

func TestResolveStartupModelEmptyInventoryAndOfflineDiagnostics(t *testing.T) {
	cfg := config.DefaultModelConfig()
	router := &selectionRouterStub{resolved: ""}

	if _, _, err := resolveStartupModel(cfg.DefaultModel, false, true, &cfg, []string{}, []string{}, true, router); err == nil {
		t.Fatal("known empty inventory accepted an automatic model")
	}

	selected, models, err := resolveStartupModel(cfg.DefaultModel, false, true, &cfg, nil, nil, false, router)
	if err != nil {
		t.Fatalf("offline diagnostic startup rejected: %v", err)
	}
	if selected != cfg.DefaultModel || len(models) != 0 {
		t.Fatalf("offline fallback = %q, %#v", selected, models)
	}

	t.Setenv("SONAR_ALLOW_LARGE_MODELS", "1")
	if _, _, err := resolveStartupModel("qwen3.5:cloud", true, true, &cfg, nil, nil, false, router); err == nil {
		t.Fatal("offline inventory plus hardware override accepted cloud alias")
	}
}

func TestResolveStartupModelOfflineMixedPrivacyStillRejectsOversizedLocalTier(t *testing.T) {
	t.Setenv("SONAR_ALLOW_LARGE_MODELS", "")
	cfg := config.DefaultModelConfig()
	router := &selectionRouterStub{}

	if _, _, err := resolveStartupModel("qwen3.5:70b", true, false, &cfg, nil, nil, false, router); err == nil {
		t.Fatal("unverified oversized local model was admitted with local-only disabled")
	}
	selected, models, err := resolveStartupModel("provider-cloud:latest", true, false, &cfg, nil, nil, false, router)
	if err != nil {
		t.Fatalf("offline fallback inferred execution location from a custom tag: %v", err)
	}
	if selected != "provider-cloud:latest" || len(models) != 0 {
		t.Fatalf("offline mixed-privacy fallback = %q, %#v", selected, models)
	}
}

func TestSelectHeadlessModelHonorsAnyPin(t *testing.T) {
	router := &selectionRouterStub{headless: "qwen3.5:4b"}
	if got := selectHeadlessModel("gemma4:e2b", "build it", true, router, config.ModeBuildContext); got != "gemma4:e2b" {
		t.Fatalf("profile pin was routed to %q", got)
	}
	if router.headlessHit {
		t.Fatal("router called for pinned headless model")
	}

	if got := selectHeadlessModel("qwen3.5:2b", "build it", false, router, config.ModeBuildContext); got != "qwen3.5:4b" {
		t.Fatalf("automatic headless selection = %q", got)
	}
	if !router.headlessHit {
		t.Fatal("router not called for automatic headless model")
	}
}

func TestDisabledAutoSelectPinsConfiguredModelAcrossSurfaces(t *testing.T) {
	if !shouldPinStartupModel("", false) {
		t.Fatal("model.auto_select=false did not create a startup pin")
	}
	router := &selectionRouterStub{headless: "qwen3.5:4b"}
	if got := selectHeadlessModel("gemma4:e2b", "build it", shouldPinStartupModel("", false), router, config.ModeBuildContext); got != "gemma4:e2b" {
		t.Fatalf("disabled auto-selection routed headless model to %q", got)
	}
	if router.headlessHit {
		t.Fatal("headless router ran while model.auto_select=false")
	}

	if shouldPinStartupModel("", true) {
		t.Fatal("default automatic routing was unexpectedly pinned")
	}
	if !shouldPinStartupModel("qwen3.5:2b", true) {
		t.Fatal("explicit --model override was not pinned")
	}
}

func TestRestoreManualModelPreferenceRequiresVerifiedLocalInventory(t *testing.T) {
	tests := []struct {
		name       string
		preferred  string
		manual     []string
		local      []string
		known      bool
		want       string
		wantOK     bool
		wantDetail string
	}{
		{
			name: "verified local alias", preferred: "local-code", manual: []string{"local-code:latest"},
			local: []string{"local-code:latest"}, known: true, want: "local-code:latest", wantOK: true,
		},
		{
			name: "cloud needs fresh consent", preferred: "cloud-code", manual: []string{"local-code", "cloud-code"},
			local: []string{"local-code"}, known: true, wantDetail: "fresh explicit selection",
		},
		{
			name: "removed model falls back", preferred: "removed-code", manual: []string{"local-code"},
			local: []string{"local-code"}, known: true, wantDetail: "no longer available",
		},
		{
			name: "offline inventory fails closed", preferred: "custom-code", known: false,
			wantDetail: "inventory is unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok, detail := restoreManualModelPreference(test.preferred, test.manual, test.local, test.known)
			if got != test.want || ok != test.wantOK || (test.wantDetail != "" && !strings.Contains(detail, test.wantDetail)) {
				t.Fatalf("restore=%q ok=%v detail=%q", got, ok, detail)
			}
		})
	}
}

func TestStartupModelAuthoritiesTakePrecedenceOverManualPreference(t *testing.T) {
	if shouldRestoreManualModelPreference("qwen3.5:4b", false) {
		t.Fatal("explicit --model allowed saved preference override")
	}
	if shouldRestoreManualModelPreference("", true) {
		t.Fatal("agent-profile model allowed saved preference override")
	}
	if !shouldRestoreManualModelPreference("", false) {
		t.Fatal("ordinary startup did not admit saved manual preference")
	}
}

var _ config.ModelRouter = (*selectionRouterStub)(nil)
var _ availabilityAwareRouter = (*selectionRouterStub)(nil)
