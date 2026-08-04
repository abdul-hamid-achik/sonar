package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type ollamaModelShowerStub struct {
	info  llm.OllamaModelInfo
	calls int
}

func (s *ollamaModelShowerStub) ShowOllamaModel(context.Context, string) (llm.OllamaModelInfo, error) {
	s.calls++
	return s.info, nil
}

func TestOllamaChatModelSetsKeepCloudManualAndAutomaticRoutingLocal(t *testing.T) {
	models := []llm.OllamaModel{
		{Name: "custom-code", Location: llm.OllamaModelLocationLocal, SizeBytes: 2 << 30, Capabilities: []string{"completion", "tools"}},
		{Name: "embed", Location: llm.OllamaModelLocationLocal, SizeBytes: 1 << 20, Capabilities: []string{"embedding"}},
		{Name: "cloud-code", Location: llm.OllamaModelLocationCloud, ContextLength: 262144, Capabilities: []string{"completion", "tools"}},
		{Name: "cloud-unknown", Location: llm.OllamaModelLocationCloud, Capabilities: []string{"completion", "tools"}},
		{Name: "private-remote", Location: llm.OllamaModelLocationRemote, Capabilities: []string{"completion", "tools"}},
		{Name: "unknown", Location: llm.OllamaModelLocationLocal, SizeBytes: 1 << 30},
	}
	if got := manuallySelectableOllamaChatModels(models, true); !reflect.DeepEqual(got, []string{"custom-code"}) {
		t.Fatalf("local-only models = %#v", got)
	}
	if got := manuallySelectableOllamaChatModels(models, false); !reflect.DeepEqual(got, []string{"custom-code", "cloud-code"}) {
		t.Fatalf("manual mixed models = %#v", got)
	}
	if got := autoRoutableOllamaChatModels(models, nil); !reflect.DeepEqual(got, []string{"custom-code"}) {
		t.Fatalf("automatic models = %#v, want local only", got)
	}
}

func TestAutoRoutableOllamaChatModelsExcludeConfiguredExclusiveProfiles(t *testing.T) {
	models := []llm.OllamaModel{
		{Name: "qwen3.5:2b", Location: llm.OllamaModelLocationLocal, SizeBytes: 2 << 30, Capabilities: []string{"completion", "tools"}},
		{Name: "phi4-mini:latest", Location: llm.OllamaModelLocationLocal, SizeBytes: 2 << 30, Capabilities: []string{"completion", "tools"}},
		{Name: "ornith:latest", Location: llm.OllamaModelLocationLocal, SizeBytes: 5 << 30, Capabilities: []string{"completion", "tools"}},
		{Name: "gemma4:e2b", Location: llm.OllamaModelLocationLocal, SizeBytes: 7 << 30, Capabilities: []string{"completion", "tools"}},
	}
	cfg := config.DefaultModelConfig()

	got := autoRoutableOllamaChatModels(models, &cfg)
	if !reflect.DeepEqual(got, []string{"qwen3.5:2b"}) {
		t.Fatalf("automatic models = %#v, want only non-exclusive Qwen", got)
	}
}

func TestConfiguredChatModelExcludesCanonicalEmbedTag(t *testing.T) {
	cfg := config.ModelConfig{
		Models:     []config.Model{{Name: "nomic-embed-text:latest"}},
		EmbedModel: "nomic-embed-text",
	}
	if configuredChatModel(&cfg, "nomic-embed-text:latest") {
		t.Fatal("canonical embed model promoted to chat fallback")
	}
}

func TestEnrichOllamaCapabilitiesFetchesNativeContextWhenCapabilitiesAlreadyKnown(t *testing.T) {
	shower := &ollamaModelShowerStub{info: llm.OllamaModelInfo{
		Capabilities:  []string{"completion", "tools", "thinking"},
		NativeContext: 1_048_576,
	}}
	models := []llm.OllamaModel{{
		Name: "deepseek-v4-flash:cloud", Location: llm.OllamaModelLocationCloud,
		Capabilities: []string{"completion", "tools", "thinking"},
	}}

	got := enrichOllamaCapabilities(context.Background(), shower, models, nil)
	if shower.calls != 1 {
		t.Fatalf("ShowOllamaModel calls = %d, want 1", shower.calls)
	}
	if got[0].ContextLength != 1_048_576 {
		t.Fatalf("native context = %d, want 1048576", got[0].ContextLength)
	}
}

func TestEnrichOllamaCapabilitiesPreservesAuthoritativeTagCapabilities(t *testing.T) {
	shower := &ollamaModelShowerStub{info: llm.OllamaModelInfo{
		Capabilities:  []string{"completion", "tools"},
		NativeContext: 8_192,
	}}
	models := []llm.OllamaModel{{
		Name: "embed-only", Location: llm.OllamaModelLocationLocal,
		Capabilities: []string{"embedding"},
	}}

	got := enrichOllamaCapabilities(context.Background(), shower, models, nil)
	if !reflect.DeepEqual(got[0].Capabilities, []string{"embedding"}) {
		t.Fatalf("tag capabilities replaced during context enrichment: %#v", got[0].Capabilities)
	}
	if got[0].ContextLength != 8_192 {
		t.Fatalf("native context = %d, want 8192", got[0].ContextLength)
	}
}
