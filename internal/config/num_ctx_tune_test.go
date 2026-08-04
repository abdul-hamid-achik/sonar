package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecommendNumCtxPrefersAgentSweetSpotOn16GB(t *testing.T) {
	t.Setenv("SONAR_ALLOW_LARGE_MODELS", "1")
	const total = 16 << 30
	const weight = 5600 << 20 // ~5.5 GiB ornith-class
	rec := RecommendNumCtx(total, weight, 262_144, 16_384)
	if rec.Recommended != preferredAgentNumCtx {
		t.Fatalf("recommended = %d, want %d", rec.Recommended, preferredAgentNumCtx)
	}
	if rec.MaxSafe < preferredAgentNumCtx {
		t.Fatalf("max safe = %d, want >= %d", rec.MaxSafe, preferredAgentNumCtx)
	}
}

func TestRecommendNumCtxClampsWithoutLargeModelsEnv(t *testing.T) {
	t.Setenv("SONAR_ALLOW_LARGE_MODELS", "")
	rec := RecommendNumCtx(16<<30, 5600<<20, 262_144, 16_384)
	if rec.Recommended != safeMaxNumCtx {
		t.Fatalf("recommended without allow = %d, want %d", rec.Recommended, safeMaxNumCtx)
	}
	if !rec.ClampedByEnv {
		t.Fatal("expected ClampedByEnv")
	}
}

func TestRecommendNumCtxAllowsLargeWithEnv(t *testing.T) {
	t.Setenv("SONAR_ALLOW_LARGE_MODELS", "1")
	rec := RecommendNumCtx(16<<30, 5600<<20, 262_144, 16_384)
	if rec.Recommended != preferredAgentNumCtx {
		t.Fatalf("recommended with allow = %d, want %d", rec.Recommended, preferredAgentNumCtx)
	}
	if rec.ClampedByEnv {
		t.Fatal("did not expect ClampedByEnv")
	}
}

func TestParseNumCtxArg(t *testing.T) {
	for _, test := range []struct {
		in   string
		want int
	}{
		{in: "98304", want: 98_304},
		{in: "96k", want: 96 * 1024},
		{in: "64K", want: 64 * 1024},
	} {
		got, err := ParseNumCtxArg(test.in)
		if err != nil || got != test.want {
			t.Fatalf("ParseNumCtxArg(%q) = %d, %v want %d", test.in, got, err, test.want)
		}
	}
	if _, err := ParseNumCtxArg("nope"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestUpdateOllamaNumCtxFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := "ollama:\n  model: ornith:latest\n  num_ctx: 16384\nprivacy:\n  local_only: true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateOllamaNumCtxFile(path, 98_304); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "num_ctx: 98304") {
		t.Fatalf("updated file = %s", data)
	}
	if !strings.Contains(string(data), "privacy:") {
		t.Fatal("expected surrounding keys preserved")
	}
}
