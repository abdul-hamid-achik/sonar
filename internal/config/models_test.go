package config

import (
	"os"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelCapabilityYAML(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want ModelCapability
	}{
		{name: "name", yaml: "capability: complex\n", want: CapabilityComplex},
		{name: "legacy number", yaml: "capability: 1\n", want: CapabilityMedium},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var model Model
			if err := yaml.Unmarshal([]byte(tc.yaml), &model); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if model.Capability != tc.want {
				t.Fatalf("capability = %v, want %v", model.Capability, tc.want)
			}
		})
	}

	var model Model
	if err := yaml.Unmarshal([]byte("capability: enormous\n"), &model); err == nil {
		t.Fatal("expected invalid capability to fail")
	}
}

func TestExampleConfigParsesAndValidates(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config.example.yaml does not parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config.example.yaml is invalid: %v", err)
	}
}

func TestModel_IsSimpleTask(t *testing.T) {
	tests := []struct {
		name       string
		capability ModelCapability
		want       bool
	}{
		{name: "CapabilitySimple is simple", capability: CapabilitySimple, want: true},
		{name: "CapabilityMedium is simple", capability: CapabilityMedium, want: true},
		{name: "CapabilityComplex is not simple", capability: CapabilityComplex, want: false},
		{name: "CapabilityAdvanced is not simple", capability: CapabilityAdvanced, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{Capability: tt.capability}
			if got := m.IsSimpleTask(); got != tt.want {
				t.Errorf("Model{Capability: %d}.IsSimpleTask() = %v, want %v", tt.capability, got, tt.want)
			}
		})
	}
}

func TestModel_IsComplexTask(t *testing.T) {
	tests := []struct {
		name       string
		capability ModelCapability
		want       bool
	}{
		{name: "CapabilitySimple is not complex", capability: CapabilitySimple, want: false},
		{name: "CapabilityMedium is not complex", capability: CapabilityMedium, want: false},
		{name: "CapabilityComplex is complex", capability: CapabilityComplex, want: true},
		{name: "CapabilityAdvanced is complex", capability: CapabilityAdvanced, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Model{Capability: tt.capability}
			if got := m.IsComplexTask(); got != tt.want {
				t.Errorf("Model{Capability: %d}.IsComplexTask() = %v, want %v", tt.capability, got, tt.want)
			}
		})
	}
}

func TestModelConfig_GetModel(t *testing.T) {
	cfg := DefaultModelConfig()

	tests := []struct {
		name    string
		model   string
		wantErr bool
	}{
		{name: "found model", model: "qwen3.5:0.8b", wantErr: false},
		{name: "not found", model: "nonexistent", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cfg.GetModel(tt.model)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if got.Name != tt.model {
					t.Errorf("GetModel(%q).Name = %q, want %q", tt.model, got.Name, tt.model)
				}
			}
		})
	}
}

func TestDefaultModelsIncludeOrnithManualExclusiveProfile(t *testing.T) {
	cfg := DefaultModelConfig()
	model, err := cfg.GetModel("ornith:latest")
	if err != nil {
		t.Fatal(err)
	}

	if model.Family != FamilyQwen35 {
		t.Fatalf("family = %q, want %q", model.Family, FamilyQwen35)
	}
	if model.DisplayName != "Ornith 9B (exclusive)" || model.Size != "5.6GB Q4" || model.Parameters != "9 billion" {
		t.Fatalf("identity metadata = %#v", model)
	}
	if model.ContextSize != 262144 || model.Capability != CapabilityAdvanced {
		t.Fatalf("execution metadata = %#v", model)
	}
	if model.Default || !model.Exclusive {
		t.Fatalf("manual-exclusive flags = default:%v exclusive:%v", model.Default, model.Exclusive)
	}
	wantUseCases := []string{"advanced_coding", "multi_file_edits", "architecture", "deep_review", "explicit_profile"}
	if !reflect.DeepEqual(model.UseCases, wantUseCases) {
		t.Fatalf("use cases = %#v, want %#v", model.UseCases, wantUseCases)
	}
}

func TestDefaultModelsQuarantinePhiFromAgentRouting(t *testing.T) {
	cfg := DefaultModelConfig()
	model, err := cfg.GetModel("phi4-mini:latest")
	if err != nil {
		t.Fatal(err)
	}
	if model.Default || !model.Exclusive {
		t.Fatalf("Phi quarantine flags = default:%v exclusive:%v", model.Default, model.Exclusive)
	}
	wantUseCases := []string{"alternate_reasoning", "code_review", "explicit_profile"}
	if !reflect.DeepEqual(model.UseCases, wantUseCases) {
		t.Fatalf("Phi use cases = %#v, want %#v", model.UseCases, wantUseCases)
	}
	if model.Description != "Alternative compact reasoning profile; manual-only pending behavioral tool verification" {
		t.Fatalf("Phi description = %q", model.Description)
	}
	for _, fallback := range cfg.FallbackChain {
		if CanonicalModelName(fallback) == CanonicalModelName(model.Name) {
			t.Fatalf("manual-only Phi remained in automatic fallback chain %#v", cfg.FallbackChain)
		}
	}
}

func TestModelConfig_GetDefaultModel(t *testing.T) {
	tests := []struct {
		name string
		cfg  ModelConfig
		want string // empty means nil expected
	}{
		{
			name: "model with Default=true",
			cfg: ModelConfig{
				Models: []Model{
					{Name: "a", Default: false},
					{Name: "b", Default: true},
					{Name: "c", Default: false},
				},
			},
			want: "b",
		},
		{
			name: "no default returns last",
			cfg: ModelConfig{
				Models: []Model{
					{Name: "a", Default: false},
					{Name: "b", Default: false},
				},
			},
			want: "b",
		},
		{
			name: "empty slice returns nil",
			cfg:  ModelConfig{Models: []Model{}},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.GetDefaultModel()
			if tt.want == "" {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
			} else {
				if got == nil {
					t.Fatal("expected non-nil model, got nil")
				}
				if got.Name != tt.want {
					t.Errorf("GetDefaultModel().Name = %q, want %q", got.Name, tt.want)
				}
			}
		})
	}
}

func TestModelConfig_SelectModelForTask(t *testing.T) {
	cfg := DefaultModelConfig()

	tests := []struct {
		name       string
		complexity string
		autoSelect bool
		want       string
	}{
		{name: "auto simple", complexity: "simple", autoSelect: true, want: "qwen3.5:0.8b"},
		{name: "auto medium", complexity: "medium", autoSelect: true, want: "qwen3.5:2b"},
		{name: "auto complex", complexity: "complex", autoSelect: true, want: "qwen3.5:4b"},
		// "advanced" maps to the highest memory-safe tier (4B complex), since
		// no larger local model is allowed on 16GB.
		{name: "auto advanced", complexity: "advanced", autoSelect: true, want: "qwen3.5:4b"},
		{name: "no autoselect simple", complexity: "simple", autoSelect: false, want: cfg.DefaultModel},
		{name: "no autoselect complex", complexity: "complex", autoSelect: false, want: cfg.DefaultModel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg.AutoSelect = tt.autoSelect
			got := cfg.SelectModelForTask(tt.complexity)
			if got != tt.want {
				t.Errorf("SelectModelForTask(%q) = %q, want %q", tt.complexity, got, tt.want)
			}
		})
	}
}

func TestModelConfigAutoSelectNeverReturnsExclusiveFallback(t *testing.T) {
	exclusiveOnly := ModelConfig{
		AutoSelect:   true,
		DefaultModel: "ornith:latest",
		Models: []Model{{
			Name: "ornith:latest", Capability: CapabilitySimple, Exclusive: true,
		}},
	}
	for _, complexity := range []string{"simple", "unknown"} {
		if got := exclusiveOnly.SelectModelForTask(complexity); got != "" {
			t.Fatalf("exclusive-only %s routing selected %q", complexity, got)
		}
	}

	withSafeFallback := exclusiveOnly
	withSafeFallback.Models = append(withSafeFallback.Models, Model{
		Name: "qwen3.5:2b", Capability: CapabilityMedium,
	})
	if got := withSafeFallback.SelectModelForTask("simple"); got != "qwen3.5:2b" {
		t.Fatalf("safe fallback = %q, want qwen3.5:2b", got)
	}
}

func TestAvailableLocalChatModelsUsesSafeCatalogOrder(t *testing.T) {
	cfg := DefaultModelConfig()
	discovered := []string{
		"unknown-local:latest",
		"nomic-embed-text",
		"gemma4:e4b",
		"gemma4:e2b",
		"qwen3.5:9b",
		"ornith:latest",
		"qwen3.5:4b",
		"qwen3.5:0.8b",
		"qwen3.5:2b",
	}

	got := cfg.AvailableLocalChatModels(discovered)
	want := []string{
		"qwen3.5:0.8b",
		"qwen3.5:2b",
		"qwen3.5:4b",
		"qwen3.5:9b",
		"ornith:latest",
		"gemma4:e2b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chat models = %#v, want %#v", got, want)
	}
}

func TestAvailableLocalChatModelsRequiresOrnithInstalled(t *testing.T) {
	cfg := DefaultModelConfig()
	if got := cfg.AvailableLocalChatModels([]string{"qwen3.5:2b"}); !reflect.DeepEqual(got, []string{"qwen3.5:2b"}) {
		t.Fatalf("absent Ornith leaked into discovery: %#v", got)
	}
	if got := cfg.AvailableLocalChatModels([]string{"ornith:latest"}); !reflect.DeepEqual(got, []string{"ornith:latest"}) {
		t.Fatalf("installed Ornith was not exposed for manual selection: %#v", got)
	}
}

func TestCheckModelAvailableLocally(t *testing.T) {
	discovered := []string{"qwen3.5:2b", "gemma4:e2b", "llama3:latest"}
	if err := CheckModelAvailableLocally("gemma4:e2b", discovered); err != nil {
		t.Fatalf("installed model rejected: %v", err)
	}
	if err := CheckModelAvailableLocally("qwen3.5:cloud", discovered); err == nil {
		t.Fatal("model absent from local inventory was accepted")
	}
	if err := CheckModelAvailableLocally("llama3", discovered); err != nil {
		t.Fatalf("implicit :latest model rejected: %v", err)
	}
}

func TestAvailableLocalChatModelsMatchesImplicitLatestTag(t *testing.T) {
	cfg := ModelConfig{Models: []Model{{Name: "llama3", Family: FamilyLlama}}}
	if got := cfg.AvailableLocalChatModels([]string{"llama3:latest"}); !reflect.DeepEqual(got, []string{"llama3"}) {
		t.Fatalf("chat models = %#v, want implicit-tag catalog entry", got)
	}
}
