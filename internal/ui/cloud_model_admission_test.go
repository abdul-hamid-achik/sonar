package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalOnlyNeverGatesInferenceInSonar pins a boundary this fork does not
// have, which is why the test asserts an absence.
//
// privacy.local_only bounds TOOL endpoints — the MCP layer really enforces it,
// with its own resolver and dialer in internal/mcp/http_policy.go. It cannot
// bound inference here: sonar reaches hosted providers exclusively,
// ProviderProfile.IsRemote() is constant true, and a rule refusing a hosted
// model could only ever refuse every model the harness supports.
//
// The picker carried that rule anyway, copied from local-agent — where it IS
// correct, because Ollama runs models on the machine and there is a local
// alternative to fall back to. Here it contradicted both AGENTS.md and
// internal/llm's TestSwitchProviderIgnoresLocalOnly. The copy is gone, and the
// divergence between the two harnesses is now a decision rather than drift.
func TestLocalOnlyNeverGatesInferenceInSonar(t *testing.T) {
	m := newTestModel(t)
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "qwen-cloud:latest", Source: OllamaModelCloud, Selectable: true, Fit: true,
	}}
	if err := m.validateModelAdmission("qwen-cloud:latest"); err != nil {
		t.Fatalf("a hosted model was refused by the picker: %v", err)
	}
}

// TestNoLocalOnlyInferenceGateReturns is the guard that keeps the copy from
// coming back. The rule reads as a safety feature, so the next person to sync a
// file from local-agent has every reason to restore it — and nothing in a
// hosted-only harness would fail if they did, because the branch is simply
// never taken until someone configures local_only and then cannot pick a model.
//
// A source scan is the only thing that fails at the moment the copy lands.
func TestNoLocalOnlyInferenceGateReturns(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for lineNumber, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "localOnly") && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				t.Errorf("%s:%d reintroduces a local-only gate in the UI; privacy.local_only bounds tool endpoints, and this fork has no local inference to fall back to",
					name, lineNumber+1)
			}
		}
	}
}

// The picker must not offer a decision the harness cannot carry out. "review"
// was the visible half of a contract whose other half had been deleted.
func TestModelPickerNeverOffersAReviewAffordance(t *testing.T) {
	descriptor := OllamaModelDescriptor{
		Name: "qwen-cloud:latest", Source: OllamaModelCloud, Selectable: true, Fit: true,
	}
	if state := modelRowState(descriptor, false, false); state == "review" {
		t.Error("a model row still reports the removed review state")
	}
	if state := modelSelectionState(descriptor); strings.Contains(state, "review") {
		t.Errorf("model selection state still mentions review: %q", state)
	}
	if state := modelSelectionState(descriptor); strings.Contains(state, "consent") {
		t.Errorf("model selection state still mentions consent: %q", state)
	}
	// An unavailable model still explains itself; it just never does so as a
	// pending confirmation.
	unavailable := OllamaModelDescriptor{Name: "too-big:latest", Selectable: false, Fit: false}
	if reason := modelDecisionReason(unavailable); reason == "" {
		t.Error("an unavailable model gives no reason at all")
	} else if strings.Contains(strings.ToLower(reason), "confirmation") {
		t.Errorf("decision reason still implies a confirmation: %q", reason)
	}
}
