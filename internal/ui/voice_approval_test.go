package ui

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

// The vocabulary is asymmetric because the mistakes are.
//
// An error in the deny direction costs a refusal the model routes around — the
// same shape as approval_timeout, which can only ever withhold permission. An
// error in the allow direction costs a command nobody asked for. The
// transcriber is a local `base` model with no confidence scores, so length and
// distinctiveness ARE the threshold.
func TestSpokenApprovalNeedsADistinctiveWord(t *testing.T) {
	for phrase, want := range map[string]voiceApprovalIntent{
		"aprobalo":    voiceApprovalOnce,
		"Aprobalo.":   voiceApprovalOnce,
		"approve it":  voiceApprovalOnce,
		"denegalo":    voiceApprovalDeny,
		"no lo hagas": voiceApprovalDeny,
		"deny":        voiceApprovalDeny,
	} {
		if got := voiceApprovalFor(phrase); got != want {
			t.Errorf("voiceApprovalFor(%q) = %d, want %d", phrase, got, want)
		}
	}

	// The words most likely to be dictated by accident, invented by a
	// transcriber, or said to somebody else in the room are in neither list —
	// including the passive forms, which somebody utters ABOUT something ("ya
	// está aprobado") rather than as an instruction.
	for _, dangerous := range []string{"sí", "si", "no", "yes", "ok", "dale", "aprobado", "approved"} {
		if got := voiceApprovalFor(dangerous); got != voiceApprovalNone {
			t.Errorf("%q was accepted as an approval answer (%d)", dangerous, got)
		}
	}
	// And a sentence that merely contains one is dictation.
	for _, dictation := range []string{
		"aprobalo cuando termines de revisar el diff",
		"no lo hagas todavía, primero corré los tests",
	} {
		if got := voiceApprovalFor(dictation); got != voiceApprovalNone {
			t.Errorf("dictation was taken as an answer (%d): %q", got, dictation)
		}
	}
}

// Voice can never widen a scope, and the type is what enforces it.
//
// There is no session-scope member to reach, so no code path can produce one
// and no future edit can add one by accident without changing the type.
func TestSpokenApprovalCannotWidenAScope(t *testing.T) {
	seen := map[voiceApprovalIntent]bool{}
	for _, intent := range voiceApprovalPhrases {
		seen[intent] = true
	}
	for intent := range seen {
		if intent != voiceApprovalOnce && intent != voiceApprovalDeny {
			t.Fatalf("the vocabulary reaches intent %d, which is neither once nor deny", intent)
		}
	}
}

// A destructive request is refused rather than downgraded.
//
// The keyboard is one reach away — that is the whole premise of this feature,
// since the microphone only opened because somebody pressed a key — so
// excluding the unrecoverable cases costs a keypress exactly where a keypress
// is cheap and a mistake is not.
func TestVoiceCannotApproveSomethingDestructive(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.pendingApproval = &ToolApprovalMsg{
		ToolName: "bash",
		Args:     map[string]any{"command": "rm -rf ./build"},
		Preview:  permission.ApprovalPreview{Command: "rm -rf ./build"},
	}
	if m.voiceApprovalEligible() {
		t.Fatal("a destructive command was eligible for a spoken approval")
	}

	m.answerApprovalByVoice("aprobalo")
	if m.pendingApproval == nil {
		t.Fatal("a destructive request was resolved by voice")
	}
	// And the refusal is on the record with what was heard.
	last := m.entries[len(m.entries)-1]
	if !strings.Contains(last.Content, "aprobalo") || !strings.Contains(last.Content, "keyboard") {
		t.Fatalf("the refusal does not say what was heard or what to do: %q", last.Content)
	}

	// An ordinary request is eligible.
	m.pendingApproval = &ToolApprovalMsg{
		ToolName: "write",
		Preview:  permission.ApprovalPreview{Path: "internal/ui/voice.go"},
	}
	if !m.voiceApprovalEligible() {
		t.Fatal("an ordinary write was refused a spoken approval")
	}
}

// The resolver derives the decision from the words, and refuses what it does
// not recognise.
//
// It used to take an intent alongside the transcript it supposedly came from —
// a signature where the two can disagree — and an unmatched utterance became a
// DENIAL, which turns every unrecognised sentence spoken near an open
// microphone into an answer nobody gave.
func TestTheResolverTrustsOnlyTheWords(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.pendingApproval = &ToolApprovalMsg{ToolName: "write", RequestID: "r1"}

	// "sí" is in neither list, so it resolves nothing in either direction.
	if cmd := m.answerApprovalByVoice("sí"); cmd != nil {
		t.Fatal("an unmatched utterance produced an answer")
	}
	if m.pendingApproval == nil {
		t.Fatal("an unmatched utterance resolved the approval")
	}
	if len(m.entries) != 0 {
		t.Fatalf("an unmatched utterance wrote a decision to the record: %+v", m.entries)
	}
}

// A bash request whose command cannot be read is refused rather than allowed.
//
// Empty used to mean eligible, so a request whose command never reached the
// preview failed OPEN on the one tool where failing open is worst.
func TestAnUnreadableCommandIsNotEligible(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.pendingApproval = &ToolApprovalMsg{ToolName: "bash", RequestID: "r1"}
	if m.voiceApprovalEligible() {
		t.Fatal("a bash request with no readable command was eligible")
	}
	// A write has no command by nature, and must stay eligible or every
	// ordinary file edit stops being answerable by voice.
	m.pendingApproval = &ToolApprovalMsg{ToolName: "write", RequestID: "r2"}
	if !m.voiceApprovalEligible() {
		t.Fatal("an ordinary write was refused for having no command")
	}
}

// Words spoken about one request may not answer the next one.
//
// Transcription takes seconds. An approval can be answered from the keyboard
// and replaced by the following one while it runs, and without an identity
// check the words resolved whatever happened to be on screen when they landed —
// a grant for something the speaker never saw.
func TestASpokenAnswerBelongsToTheRequestItWasSpokenAbout(t *testing.T) {
	m := voiceTestModel(t, true, false, false)
	m.voiceInput = &voiceInputState{token: 1, approvalAtStart: "request-A"}
	// A resolved from the keyboard; B is now waiting.
	m.pendingApproval = &ToolApprovalMsg{ToolName: "write", RequestID: "request-B"}

	m.handleVoiceTranscript(VoiceTranscriptMsg{Token: 1, Text: "aprobalo"})

	if m.pendingApproval == nil {
		t.Fatal("an answer meant for the previous request resolved this one")
	}
	// It falls through to dictation instead, which is the honest outcome: the
	// words reach the composer where they can be read.
	if strings.TrimSpace(m.input.Value()) == "" {
		t.Fatal("the transcription was neither an answer nor a draft")
	}
}
