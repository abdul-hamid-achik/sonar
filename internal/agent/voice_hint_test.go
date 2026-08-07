package agent

import (
	"context"
	"strings"
	"testing"
)

// The spoken-close instruction has to reach the model, and nothing checked it.
//
// This is the one part of the voice feature with no measurement behind it: the
// digest exists only if the model writes it, the model writes it only if asked,
// and the asking travels UI → Agent → turnRuntime → prompt assembly. Any link
// in that could break silently — the feature would simply never produce a
// digest, and the fallback (reading the whole answer) is exactly what it looked
// like before the feature existed. A silent no-op is the hardest failure to
// notice, so the wiring is pinned rather than trusted.
func TestTheVoiceHintReachesTheSystemPrompt(t *testing.T) {
	const hint = "Spoken output is on: end with <!--spoken: ...-->."

	prompt := buildSystemPrompt(context.Background(), systemPromptOptions{VoiceHint: hint})
	if !strings.Contains(prompt, hint) {
		t.Fatalf("the voice hint never reached the assembled prompt:\n%s", prompt)
	}

	// And it costs nothing when voice is off, which is the default and the
	// common case: no stray section, no blank paragraph.
	silent := buildSystemPrompt(context.Background(), systemPromptOptions{})
	if strings.Contains(silent, "spoken") {
		t.Fatalf("a session with voice off still carries the hint:\n%s", silent)
	}

	// It shares the mode prefix's slot, so the two must coexist rather than
	// overwrite one another — a PLAN turn with voice on needs both.
	both := buildSystemPrompt(context.Background(), systemPromptOptions{
		ModePrefix: "PLAN MODE: read-only.",
		VoiceHint:  hint,
	})
	if !strings.Contains(both, "PLAN MODE") || !strings.Contains(both, hint) {
		t.Fatalf("the mode prefix and the voice hint displaced each other:\n%s", both)
	}
}

// The agent carries the hint from the setter to the turn that uses it.
func TestSetVoiceHintSurvivesToTheTurn(t *testing.T) {
	agent := &Agent{}
	agent.SetVoiceHint("read this out")
	if agent.voiceHint != "read this out" {
		t.Fatalf("the hint did not reach the agent: %q", agent.voiceHint)
	}
	// Empty removes it, which is what /voice answer off has to do on the next
	// dispatch rather than on the next restart.
	agent.SetVoiceHint("")
	if agent.voiceHint != "" {
		t.Fatalf("the hint outlived being cleared: %q", agent.voiceHint)
	}
}
