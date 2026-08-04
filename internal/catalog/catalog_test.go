package catalog

import (
	"math"
	"testing"
)

// A broken embedded snapshot must be loud. Every other test here would pass
// vacuously against an empty catalog, so this one guards the rest.
func TestEmbeddedSnapshotLoads(t *testing.T) {
	if err := Err(); err != nil {
		t.Fatalf("embedded snapshot is unusable: %v", err)
	}
	providers := Providers()
	if len(providers) < 20 {
		t.Fatalf("snapshot has only %d providers; it looks truncated", len(providers))
	}
	models := 0
	for _, provider := range providers {
		models += len(provider.Models)
	}
	if models < 500 {
		t.Fatalf("snapshot has only %d models; it looks truncated", models)
	}
}

func TestProviderIDsAreOrderedAndUnique(t *testing.T) {
	ids := ProviderIDs()
	seen := make(map[ProviderID]struct{}, len(ids))
	for i, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate provider id %q", id)
		}
		seen[id] = struct{}{}
		if i > 0 && ids[i-1] > id {
			t.Fatalf("provider ids are not ordered: %q before %q", ids[i-1], id)
		}
	}
}

// These are the numbers sonar previously hard-coded from DeepSeek's published
// contract. Pinning them here proves the catalog is a faithful replacement for
// those constants rather than a plausible-looking substitute.
func TestDeepSeekFlashMetadataMatchesPublishedContract(t *testing.T) {
	model, ok := LookupModel("deepseek", "deepseek-v4-flash")
	if !ok {
		t.Fatal("deepseek-v4-flash is absent from the catalog")
	}
	for _, check := range []struct {
		field string
		got   float64
		want  float64
	}{
		{"cost_per_1m_in", model.CostPer1MIn, 0.14},
		{"cost_per_1m_out", model.CostPer1MOut, 0.28},
		{"cost_per_1m_in_cached", model.CostPer1MInCached, 0.0028},
		{"context_window", float64(model.ContextWindow), 1_000_000},
		{"default_max_tokens", float64(model.DefaultMaxTokens), 384_000},
	} {
		if math.Abs(check.got-check.want) > 1e-9 {
			t.Errorf("%s = %v, want %v", check.field, check.got, check.want)
		}
	}
	if !model.CanReason {
		t.Error("can_reason = false, want true")
	}
	if model.DefaultReasoningEffort != "high" {
		t.Errorf("default_reasoning_effort = %q, want high", model.DefaultReasoningEffort)
	}
	// DeepSeek V4 takes no images. sonar still exposes attachment UI, so this
	// assertion is the hook for gating it.
	if model.SupportsImages {
		t.Error("supports_attachments = true; the attachment gate assumes otherwise")
	}
}

func TestAPIKeyEnvReturnsNamesNotValues(t *testing.T) {
	if got := APIKeyEnv("deepseek"); got != "DEEPSEEK_API_KEY" {
		t.Errorf("deepseek key env = %q, want DEEPSEEK_API_KEY", got)
	}
	if got := APIKeyEnv("anthropic"); got != "ANTHROPIC_API_KEY" {
		t.Errorf("anthropic key env = %q, want ANTHROPIC_API_KEY", got)
	}
	// Cloud credential chains have no single variable; an empty result is the
	// correct answer, not a lookup failure to paper over.
	if got := APIKeyEnv("bedrock"); got != "" {
		t.Errorf("bedrock key env = %q, want empty", got)
	}
	if got := APIKeyEnv("not-a-provider"); got != "" {
		t.Errorf("unknown provider key env = %q, want empty", got)
	}
}

func TestLookupMisses(t *testing.T) {
	if _, ok := LookupProvider("not-a-provider"); ok {
		t.Error("unknown provider was found")
	}
	if _, ok := LookupModel("deepseek", "not-a-model"); ok {
		t.Error("unknown model was found")
	}
	if models := Models("not-a-provider"); models != nil {
		t.Errorf("unknown provider returned %d models", len(models))
	}
}

// Callers must not be able to corrupt the shared catalog through a returned
// slice, or one surface's filtering would silently reshape another's.
func TestReturnedSlicesAreCopies(t *testing.T) {
	first := Models("deepseek")
	if len(first) == 0 {
		t.Fatal("deepseek has no models")
	}
	original := first[0].ID
	first[0].ID = "mutated"
	if second := Models("deepseek"); second[0].ID != original {
		t.Fatalf("mutating a returned model leaked into the catalog: %q", second[0].ID)
	}

	ids := ProviderIDs()
	ids[0] = "mutated"
	if ProviderIDs()[0] == "mutated" {
		t.Fatal("mutating returned ids leaked into the catalog")
	}
}

func TestEstimateCostUSD(t *testing.T) {
	model, ok := LookupModel("deepseek", "deepseek-v4-flash")
	if !ok {
		t.Fatal("deepseek-v4-flash is absent from the catalog")
	}

	miss := EstimateCostUSD(model, 1_000_000, 0, 0)
	hit := EstimateCostUSD(model, 1_000_000, 1_000_000, 0)
	if miss <= hit {
		t.Fatalf("a cache miss (%f) must cost more than a hit (%f)", miss, hit)
	}
	if math.Abs(miss-0.14) > 1e-9 {
		t.Errorf("1M miss tokens = %f, want 0.14", miss)
	}
	if math.Abs(EstimateCostUSD(model, 0, 0, 1_000_000)-0.28) > 1e-9 {
		t.Error("1M output tokens did not price at 0.28")
	}
	// An over-reported cache count must clamp rather than credit the caller.
	if got := EstimateCostUSD(model, 10, 999, 0); got < 0 {
		t.Errorf("over-reported cache hits produced a negative cost: %f", got)
	}
	if got := EstimateCostUSD(model, -5, -5, -5); got != 0 {
		t.Errorf("negative token counts produced %f, want 0", got)
	}
}
