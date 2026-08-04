package config

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyTask(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  TaskComplexity
	}{
		{name: "empty query", query: "", want: ComplexityMedium},
		{name: "simple what is", query: "what is Go", want: ComplexitySimple},

		// "create a function": medium "create" +1, "function" +1, advanced "create a" +3 = 5 → advanced
		{name: "create a function is advanced due to overlaps", query: "create a function", want: ComplexityAdvanced},

		// "debug this error across multiple files": complex "debug" +2, "error" +2, "bug" +2 (substring of debug),
		// "multiple" +2, "across" +2 = 10, medium "file" +1 = 11 → advanced
		{name: "debug across files is advanced", query: "debug this error across multiple files", want: ComplexityAdvanced},

		// "implement a full stack system with infrastructure": advanced "implement" +3, "full stack" +3, "system" +3,
		// "infrastructure" +3 = 12 → advanced
		{name: "advanced full stack system", query: "implement a full stack system with infrastructure", want: ComplexityAdvanced},

		// Boundary: "explain" → simple -2 → score -2 → simple
		{name: "boundary simple score -2", query: "explain", want: ComplexitySimple},

		// No indicators → score 0 → medium
		{name: "boundary medium score 0", query: "hello world", want: ComplexityMedium},

		// "create" → medium +1, but also matches advanced "create a"? No, "create" doesn't contain "create a".
		// So just +1 → medium
		{name: "boundary medium score 1", query: "create", want: ComplexityMedium},

		// "debug" alone: complex "debug" +2, "bug" +2 (substring) = 4 → complex
		{name: "debug alone is complex", query: "debug", want: ComplexityComplex},

		// "debug error": "debug" +2, "error" +2, "bug" +2 (substring of debug) = 6 → advanced
		{name: "debug error is advanced", query: "debug error", want: ComplexityAdvanced},

		// Word count >50 bonus (+2) with "debug": "debug" +2, "bug" +2 = 4, +2 word bonus = 6 → advanced
		{
			name:  "word count bonus over 50 with debug",
			query: strings.Repeat("word ", 51) + "debug",
			want:  ComplexityAdvanced,
		},

		// "why does this happen": "why" +1 = 1 → medium
		{name: "why bonus", query: "why does this happen", want: ComplexityMedium},

		// "reason for the crash": "reason" +1 = 1 → medium
		{name: "reason bonus", query: "reason for the crash", want: ComplexityMedium},

		// "how about we think...": no indicators, "how" + >10 words +1 = 1 → medium
		{name: "how with many words", query: "how about we think about the things that are happening right now in the code base", want: ComplexityMedium},

		// Case insensitivity
		{name: "case insensitive WHAT IS", query: "WHAT IS Go", want: ComplexitySimple},
		{name: "case insensitive EXPLAIN", query: "EXPLAIN this code", want: ComplexitySimple},

		// Pure simple: multiple simple indicators
		{name: "multiple simple indicators", query: "what is this simple quick search", want: ComplexitySimple},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyTask(tt.query)
			if got != tt.want {
				t.Errorf("ClassifyTask(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestRouter_GetFallbackChain(t *testing.T) {
	cfg := &ModelConfig{
		FallbackChain: []string{"a", "b", "c", "d"},
	}
	r := NewRouter(cfg)

	tests := []struct {
		name    string
		model   string
		wantLen int
		wantAll bool // true means expect full chain
	}{
		{name: "found at start", model: "a", wantLen: 4},
		{name: "found in middle", model: "c", wantLen: 2},
		{name: "found at end", model: "d", wantLen: 1},
		{name: "not found returns full chain", model: "unknown", wantLen: 4, wantAll: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.GetFallbackChain(tt.model)
			if len(got) != tt.wantLen {
				t.Errorf("GetFallbackChain(%q) returned %d items, want %d", tt.model, len(got), tt.wantLen)
			}
			if tt.wantAll && got[0] != "a" {
				t.Errorf("GetFallbackChain(%q) first element = %q, want %q", tt.model, got[0], "a")
			}
		})
	}
}

func TestRouter_GetModelForCapability(t *testing.T) {
	cfg := &ModelConfig{
		Models: []Model{
			{Name: "fast", Capability: CapabilitySimple},
			{Name: "mid", Capability: CapabilityMedium},
			{Name: "big", Capability: CapabilityComplex},
		},
		DefaultModel: "fallback",
	}
	r := NewRouter(cfg)

	tests := []struct {
		name       string
		capability ModelCapability
		want       string
	}{
		{name: "match simple", capability: CapabilitySimple, want: "fast"},
		{name: "match medium", capability: CapabilityMedium, want: "mid"},
		{name: "match complex", capability: CapabilityComplex, want: "big"},
		{name: "no match returns default", capability: CapabilityAdvanced, want: "fallback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.GetModelForCapability(tt.capability)
			if got != tt.want {
				t.Errorf("GetModelForCapability(%d) = %q, want %q", tt.capability, got, tt.want)
			}
		})
	}
}

func TestRouter_SelectModel(t *testing.T) {
	cfg := DefaultModelConfig()
	r := NewRouter(&cfg)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		// "what is Go" → simple → first model
		{name: "simple query selects first model", query: "what is Go", want: cfg.Models[0].Name},
		// "debug" → complex → complex-capable model
		{name: "complex query selects complex model", query: "debug", want: "qwen3.5:4b"},
		// "implement a system" → advanced → highest memory-safe tier (4B)
		{name: "advanced query selects safe top tier", query: "implement a full stack system", want: "qwen3.5:4b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.SelectModel(tt.query)
			if got != tt.want {
				t.Errorf("SelectModel(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestRouter_RecordOverrideDoesNotDeadlock(t *testing.T) {
	cfg := DefaultModelConfig()
	r := NewRouter(&cfg)
	done := make(chan struct{})

	go func() {
		r.RecordOverride("explain this function", "qwen3.5:4b")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RecordOverride deadlocked")
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.overrideLog) != 1 {
		t.Fatalf("override log length = %d, want 1", len(r.overrideLog))
	}
}

func TestRouterUsesOnlyDiscoveredLocalModels(t *testing.T) {
	cfg := DefaultModelConfig()
	r := NewRouter(&cfg)
	r.SetAvailableModels([]string{"qwen3.5:2b"})

	for _, query := range []string{"what is Go", "implement a full stack system"} {
		if got := r.SelectModel(query); got != "qwen3.5:2b" {
			t.Errorf("SelectModel(%q) = %q, want installed 2B fallback", query, got)
		}
	}
	if got := r.SelectModelForMode("implement a system", ModeBuildContext); got != "qwen3.5:2b" {
		t.Errorf("BUILD fallback = %q, want installed 2B", got)
	}
}

func TestRouterNeverAutoSelectsInstalledExclusiveOrnith(t *testing.T) {
	cfg := DefaultModelConfig()
	r := NewRouter(&cfg)
	r.SetAvailableModels([]string{"ornith:latest"})

	for _, query := range []string{"what is Go", "implement a full stack system"} {
		if got := r.SelectModelForMode(query, ModeBuildContext); got != "" {
			t.Fatalf("auto-router selected exclusive Ornith for %q: %q", query, got)
		}
	}
	if got := r.ResolveAvailableModel("ornith:latest"); got != "ornith:latest" {
		t.Fatalf("explicit installed Ornith resolved to %q", got)
	}
}

func TestRouterCustomExclusiveDefaultRequiresExplicitResolution(t *testing.T) {
	cfg := ModelConfig{
		AutoSelect:    true,
		DefaultModel:  "ornith:latest",
		FallbackChain: []string{"ornith:latest"},
		Models: []Model{{
			Name: "ornith:latest", Capability: CapabilitySimple, Exclusive: true,
		}},
	}
	r := NewRouter(&cfg)
	r.SetAvailableModels([]string{"ornith:latest"})

	if got := r.SelectModel("what is Go"); got != "" {
		t.Fatalf("custom auto-router selected exclusive default: %q", got)
	}
	if got := r.ResolveAvailableModel("ornith:latest"); got != "ornith:latest" {
		t.Fatalf("explicit exclusive resolution = %q", got)
	}
}

func TestRouterReturnsNoModelForKnownEmptyInventory(t *testing.T) {
	cfg := DefaultModelConfig()
	r := NewRouter(&cfg)
	r.SetAvailableModels([]string{})

	if got := r.ResolveAvailableModel(cfg.DefaultModel); got != "" {
		t.Fatalf("empty inventory resolved to %q, want no model", got)
	}
	if got := r.SelectModel("explain Go"); got != "" {
		t.Fatalf("empty inventory selected %q, want no model", got)
	}
}

func TestRouterCanonicalizesAvailableModelTags(t *testing.T) {
	cfg := &ModelConfig{
		Models:        []Model{{Name: "llama3", Capability: CapabilityMedium}},
		DefaultModel:  "llama3",
		FallbackChain: []string{"llama3"},
	}
	r := NewRouter(cfg)
	r.SetAvailableModels([]string{"llama3:latest"})
	if got := r.ResolveAvailableModel("llama3"); got != "llama3" {
		t.Fatalf("canonical available model resolved to %q", got)
	}
	if got := r.GetModelForCapability(CapabilityMedium); got != "llama3" {
		t.Fatalf("canonical capability model resolved to %q", got)
	}
}
