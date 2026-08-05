package ui

import (
	"strings"
	"testing"
)

// privacy.local_only forbids hosted inference. The picker used to present that
// as a "review" the operator could perform: the row read "review required", the
// footer key hint read "review", and selecting produced "requires Ollama Cloud
// confirmation for this conversation". No confirmation surface existed —
// OverlayCloudConsent was declared but nothing ever opened it, and the field
// that would have recorded a grant was never set outside tests. The prompt was
// a dead end that read like a step.
//
// The rule itself is unchanged and still enforced; only the false affordance is
// gone. What replaces it has to name the setting, because that is the only
// thing the operator can actually change.
func TestCloudModelUnderLocalOnlyIsRefusedWithAnActionableReason(t *testing.T) {
	m := newTestModel(t)
	m.localOnly = true
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "qwen-cloud:latest", Source: OllamaModelCloud, Selectable: true, Fit: true,
	}}

	err := m.validateModelAdmission("qwen-cloud:latest")
	if err == nil {
		t.Fatal("a hosted model was admitted under privacy.local_only")
	}
	message := err.Error()
	if !strings.Contains(message, "local_only") {
		t.Errorf("refusal does not name the setting to change: %q", message)
	}
	for _, forbidden := range []string{"confirmation", "consent", "review"} {
		if strings.Contains(strings.ToLower(message), forbidden) {
			t.Errorf("refusal still implies a confirmation step (%q): %q", forbidden, message)
		}
	}
}

// Clearing the setting is the whole remedy: the same model must then admit.
func TestCloudModelAdmitsOnceLocalOnlyIsCleared(t *testing.T) {
	m := newTestModel(t)
	m.localOnly = false
	m.ollamaModels = []OllamaModelDescriptor{{
		Name: "qwen-cloud:latest", Source: OllamaModelCloud, Selectable: true, Fit: true,
	}}

	if err := m.validateModelAdmission("qwen-cloud:latest"); err != nil {
		t.Fatalf("a hosted model was refused with local_only off: %v", err)
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
