package ui

import (
	"strings"
	"testing"
)

// The report must describe the hosted reality: the provider owns the window,
// and there is no knob here. Its predecessor tested num_ctx auto/set/save
// machinery that could only run against a ModelManager shape startup no
// longer constructs — the RemoteProvider check refused every real provider.
func TestContextWindowStatusReportsTheHostedReality(t *testing.T) {
	m := newTestModel(t)
	m.model = "deepseek-v4-flash"
	m.numCtx = 131072
	m.promptTokens = 32768

	report := m.contextWindowStatusReport()
	for _, want := range []string{"deepseek-v4-flash", "131072", "32768", "25%", "provider owns this window"} {
		if !strings.Contains(report, want) {
			t.Errorf("status report missing %q:\n%s", want, report)
		}
	}
	// The vocabulary of the removed local-runtime knob must not resurface: a
	// report that names num_ctx or a tuning verb is advertising a control
	// that cannot exist against a hosted provider.
	for _, gone := range []string{"num_ctx", "Ollama", "RAM", "/context auto", "/context set", "/context save"} {
		if strings.Contains(report, gone) {
			t.Errorf("status report resurrected %q:\n%s", gone, report)
		}
	}
}

func TestContextWindowStatusAdmitsAnUnknownWindow(t *testing.T) {
	m := newTestModel(t)
	m.numCtx = 0

	report := m.contextWindowStatusReport()
	if !strings.Contains(report, "unknown") {
		t.Fatalf("zero window should read as unknown, got:\n%s", report)
	}
}
