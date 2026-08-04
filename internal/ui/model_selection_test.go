package ui

import (
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

func TestSelectingCurrentModelIsIdempotent(t *testing.T) {
	m := newTestModel(t)
	m.model = "qwen3.5:9b"
	m.entries = []ChatEntry{{Kind: "user", Content: "keep working"}}
	before := len(m.entries)

	firstSelection := m.switchSelectedModel("qwen3.5:9b")
	secondSelection := m.switchSelectedModel("qwen3.5:9b")
	if !firstSelection || !secondSelection {
		t.Fatal("selecting the active model failed")
	}
	if len(m.entries) != before {
		t.Fatalf("idempotent selection added transcript receipts: %#v", m.entries[before:])
	}
	if !m.modelPinned {
		t.Fatal("explicitly reselecting the active model did not pin it")
	}
}

func TestModelSelectionAddsAtMostOneConversationalReceipt(t *testing.T) {
	m := newTestModel(t)
	m.model = "qwen3.5:2b"
	m.entries = []ChatEntry{{Kind: "user", Content: "continue"}}

	firstSelection := m.switchSelectedModel("qwen3.5:9b")
	secondSelection := m.switchSelectedModel("qwen3.5:9b")
	if !firstSelection || !secondSelection {
		t.Fatal("model selection failed")
	}
	count := 0
	for _, entry := range m.entries {
		if entry.Kind == "system" && strings.HasPrefix(entry.Content, "Model · ") {
			count++
		}
		if strings.HasPrefix(entry.Content, "Model:") {
			t.Fatalf("legacy model receipt remained: %#v", entry)
		}
	}
	if count != 1 {
		t.Fatalf("model transition receipts = %d, want 1: %#v", count, m.entries)
	}
}

func TestEmptyStateModelSelectionUsesWelcomeInsteadOfTranscript(t *testing.T) {
	m := newTestModel(t)
	m.model = "qwen3.5:2b"
	if !m.switchSelectedModel("qwen3.5:9b") {
		t.Fatal("model selection failed")
	}
	if len(m.entries) != 0 {
		t.Fatalf("empty-state model switch added transcript noise: %#v", m.entries)
	}
	if m.model != "qwen3.5:9b" {
		t.Fatalf("selected model = %q", m.model)
	}
}

func TestModelAndProfileChromeSanitizesDisplayOnly(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = 120, 30
	rawModel := "qwen\x1b]52;c;MODEL_SECRET\x07\x1b[2J\n:cloud\u202e"
	rawProfile := "reviewer\x1b]0;PROFILE_SECRET\x07\u009b2J\nforged\u2066"
	m.model = rawModel
	m.agentProfile = rawProfile
	m.entries = []ChatEntry{{Kind: "user", Content: "working"}}

	var welcome strings.Builder
	m.renderWelcome(&welcome)
	outputs := map[string]string{
		"welcome":     welcome.String(),
		"status":      m.renderStatusLine(),
		"goal footer": m.renderGoalFooterStatus(GoalSummary{Objective: "ship", Phase: GoalPhaseActive}, m.chatPaneWidth()),
	}
	m.state = StateWaiting
	outputs["working status"] = m.renderWorkingLine()

	for name, rendered := range outputs {
		plain := ansi.Strip(rendered)
		for _, secret := range []string{"MODEL_SECRET", "PROFILE_SECRET"} {
			if strings.Contains(plain, secret) {
				t.Fatalf("%s retained OSC payload %q: %q", name, secret, plain)
			}
		}
		for _, character := range plain {
			if character == '\n' && name == "welcome" {
				continue
			}
			if unicode.IsControl(character) || isBidiControl(character) {
				t.Fatalf("%s retained unsafe rune %U: %q", name, character, plain)
			}
		}
	}
	if m.model != rawModel || m.agentProfile != rawProfile {
		t.Fatalf("display sanitization mutated runtime identity: model=%q profile=%q", m.model, m.agentProfile)
	}
}
