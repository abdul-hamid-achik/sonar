package ui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

// Answering an approval by speaking, with the microphone you opened yourself.
//
// The harness never opens it. That was the decision, and it is the one that
// shapes everything here: the alternative — a microphone that opens itself when
// a tool needs permission — puts the harness in charge of when the room is
// recorded, and this codebase says plainly that a microphone opened by anything
// other than a deliberate act is a privacy problem rather than a convenience.
// So this only exists in the seconds after somebody pressed the dictation key
// while a prompt happened to be waiting.
//
// That choice pays for itself twice. The person is at the keyboard, so they can
// SEE what they are approving — which is the objection that made naming the
// command aloud necessary elsewhere and unnecessary here. And the keyboard is
// right there, so excluding the dangerous cases costs one keypress in exactly
// the situations where a keypress is cheap and a mistake is not.
//
// # What is still refused, and why the keyboard being present does not change it
//
//   - Every session and durable scope. Widening a scope is a policy change, and
//     a policy change asks for more than a phrase a transcriber guessed at. The
//     refusal is structural: this file can only ever produce AllowOnce or Deny,
//     so there is no path to a wider grant to forget to close.
//   - Anything whose preview carries a destructive warning. `rm -rf` approved by
//     a mis-hearing is not recoverable, and the y key is one reach away.
//
// # Why approving needs a distinctive word and denying does not
//
// The transcriber is a local `base` Whisper model with no confidence scores, so
// length and distinctiveness ARE the confidence threshold. Errors in the deny
// direction cost a refusal the model routes around — the same shape as
// approval_timeout, which can only ever withhold permission. Errors in the
// allow direction cost a command nobody asked for. The vocabulary is asymmetric
// because the mistakes are.

// voiceApprovalIntent is what a spoken answer can mean. The type is the
// enforcement: there is no session-scope member to reach.
type voiceApprovalIntent int

const (
	voiceApprovalNone voiceApprovalIntent = iota
	voiceApprovalOnce
	voiceApprovalDeny
)

// voiceApprovalPhrases is the closed vocabulary.
//
// "sí" and a bare "no" are deliberately absent from both sides. They are the
// two most likely things to be dictated by accident, the shortest words a
// transcriber can invent, and the ones a person says to somebody else in the
// room while the microphone is open.
var voiceApprovalPhrases = map[string]voiceApprovalIntent{
	// Imperatives only. "aprobado" and "approved" were here and are gone: they
	// are adjectives somebody says ABOUT something — "ya está aprobado" — while
	// a microphone happens to be open, and the whole defence in this direction
	// is that the phrase is one nobody utters by accident.
	"aprobalo": voiceApprovalOnce, "apruébalo": voiceApprovalOnce,
	"apruebalo":  voiceApprovalOnce,
	"approve it": voiceApprovalOnce, "approve this": voiceApprovalOnce,

	"denegalo": voiceApprovalDeny, "rechazalo": voiceApprovalDeny,
	"recházalo": voiceApprovalDeny, "no lo hagas": voiceApprovalDeny,
	"deny": voiceApprovalDeny, "denied": voiceApprovalDeny,
	"reject it": voiceApprovalDeny, "don't do it": voiceApprovalDeny,
}

// voiceApprovalFor matches a whole transcription. Anything else is dictation.
func voiceApprovalFor(transcript string) voiceApprovalIntent {
	normalized := strings.ToLower(strings.TrimSpace(transcript))
	normalized = strings.Trim(normalized, ".,;:!?¡¿\"'")
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		return voiceApprovalNone
	}
	return voiceApprovalPhrases[normalized]
}

// voiceApprovalEligible reports whether the waiting request may be answered by
// voice at all.
func (m *Model) voiceApprovalEligible() bool {
	if m == nil || m.pendingApproval == nil {
		return false
	}
	preview := m.pendingApproval.Preview
	command := preview.Command
	if command == "" {
		if raw, ok := m.pendingApproval.Args["command"].(string); ok {
			command = raw
		}
	}
	// A tool whose danger IS its command must have one to be judged. Empty
	// meant eligible before, so a bash request whose command never reached the
	// preview — a malformed one, or a shape this host does not know — failed
	// OPEN on the one tool where failing open is worst.
	if command == "" && commandBearingTool(m.pendingApproval.ToolName) {
		return false
	}
	// The host already decided which commands cause durable damage, and this
	// reuses that decision rather than inventing a second list that could
	// disagree with the one the prompt shows.
	return permission.DestructiveCommandWarning(command) == ""
}

// commandBearingTool reports whether this tool's request carries a shell
// command, and therefore whether an absent one is missing rather than absent.
//
// `write` and `edit` legitimately have no command; `bash` never legitimately
// has none. The distinction is what keeps the fail-closed rule from refusing
// every ordinary file write by voice.
func commandBearingTool(name string) bool {
	return strings.TrimSpace(name) == "bash"
}

// answerApprovalByVoice resolves the waiting prompt, or reports why it did not.
//
// Every outcome is written into the transcript, quoting what was heard. A
// decision nobody can audit is worse than one made with a key, and the whole
// reason this is answerable by voice is that the person could not reach the
// keyboard comfortably — so the record has to say what the microphone thought
// it heard, not just what was decided.
func (m *Model) answerApprovalByVoice(heard string) tea.Cmd {
	if m.pendingApproval == nil {
		return nil
	}
	// Derived HERE rather than taken from the caller. This function grants
	// permission, and a signature that accepts a decision alongside the words
	// it supposedly came from is one where the two can disagree — pass
	// voiceApprovalOnce with "sí" and it granted, though "sí" is deliberately
	// in neither list. The transcription is the only input; the decision is
	// this function's to make.
	intent := voiceApprovalFor(heard)
	if intent == voiceApprovalNone {
		// An unmatched utterance is not a denial. It used to become one, which
		// turns every unrecognised sentence spoken near an open microphone into
		// an answer nobody gave.
		return nil
	}
	heard = sanitizeTerminalSingleLine(strings.TrimSpace(heard))
	if intent == voiceApprovalOnce && !m.voiceApprovalEligible() {
		// Refused rather than downgraded. Offering "I heard approve, so here is
		// a denial" would answer a question nobody asked.
		m.entries = append(m.entries, ChatEntry{
			Kind: "system",
			Content: "Heard \"" + heard + "\", but this request changes something " +
				"durable and cannot be approved by voice. Answer it from the keyboard.",
		})
		m.invalidateEntryCache()
		m.refreshTranscript()
		return m.setFooterNotice(noticeWarning,
			"This one needs the keyboard: voice cannot approve a destructive command.", 6*time.Second)
	}

	decision := "Denied by voice"
	response := permission.Deny()
	if intent == voiceApprovalOnce {
		decision = "Approved once by voice"
		response = permission.AllowOnce()
	}
	m.entries = append(m.entries, ChatEntry{
		Kind:    "system",
		Content: decision + " (heard: \"" + heard + "\").",
	})
	m.invalidateEntryCache()
	// Through the ordinary resolution path, so the scope stays exact-request and
	// the argument hash binding is untouched. There is no second approval
	// surface to keep in step with the first.
	m.resolvePendingApproval(response)
	return nil
}
