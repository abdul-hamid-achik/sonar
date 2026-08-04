package goal

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// GoalDraftVersion identifies the deterministic definition projected before
	// a durable goal exists. Drafts have no execution authority by themselves
	// and are never persisted as Runtime snapshots.
	GoalDraftVersion = 1

	maxDraftSourceBytes = MaxObjectiveBytes
	// Draft criteria quote only a compact, human-readable projection of the
	// objective. The complete objective remains authoritative in Draft.Objective.
	maxDraftCriterionExcerptBytes = 768
)

// DraftSource records how a review-only goal suggestion was produced.
type DraftSource string

const (
	DraftSourceDeterministic DraftSource = "deterministic"
)

// Draft is a bounded definition suggestion derived from a user's prompt.
// SourcePrompt preserves the exact submitted bytes so presentation code can
// restore the composer without silently replacing the user's words. A caller
// may accept a concrete draft only when it also holds an explicit typed goal
// creation intent; otherwise it must ask the contextual follow-up. InferDraft
// itself never creates or dispatches a Runtime.
type Draft struct {
	Version            int
	Source             DraftSource
	SourcePrompt       string
	Objective          string
	AcceptanceCriteria []string
	Budget             BudgetLimits
	// NeedsFollowUp is true only when the prompt lacks a concrete target. The
	// caller can then ask FollowUpPrompt instead of forcing every goal through a
	// generic multi-field review.
	NeedsFollowUp  bool
	FollowUpPrompt string
}

// InferDraft returns conservative local suggestions without consulting a
// provider, Cortex, the filesystem, or the network. This makes AUTO entry
// deterministic and keeps inference outside the execution authority boundary.
func InferDraft(prompt string, defaults BudgetLimits) (Draft, error) {
	if !utf8.ValidString(prompt) {
		return Draft{}, fmt.Errorf("%w: goal prompt is not valid UTF-8", ErrInvalid)
	}
	if len(prompt) > maxDraftSourceBytes {
		return Draft{}, fmt.Errorf("%w: goal prompt exceeds %d bytes", ErrInvalid, maxDraftSourceBytes)
	}
	if strings.IndexByte(prompt, 0) >= 0 {
		return Draft{}, fmt.Errorf("%w: goal prompt contains a NUL byte", ErrInvalid)
	}
	if err := defaults.Validate(); err != nil {
		return Draft{}, err
	}

	objective := strings.TrimSpace(prompt)
	if objective == "" {
		return Draft{}, fmt.Errorf("%w: goal prompt is required", ErrInvalid)
	}

	criteria := inferDraftCriteria(objective)
	for index, criterion := range criteria {
		if err := validateText(fmt.Sprintf("draft acceptance criterion %d", index+1), criterion, MaxCriterionBytes, true); err != nil {
			return Draft{}, err
		}
	}
	needsFollowUp := draftPromptNeedsFollowUp(objective)
	followUp := ""
	if needsFollowUp {
		followUp = "What concrete behavior or artifact should change, and what observable result would prove it?"
	}

	return Draft{
		Version:            GoalDraftVersion,
		Source:             DraftSourceDeterministic,
		SourcePrompt:       prompt,
		Objective:          objective,
		AcceptanceCriteria: criteria,
		Budget:             defaults,
		NeedsFollowUp:      needsFollowUp,
		FollowUpPrompt:     followUp,
	}, nil
}

func inferDraftCriteria(objective string) []string {
	excerpt := boundDraftText(strings.Join(strings.Fields(objective), " "), maxDraftCriterionExcerptBytes)
	return []string{
		"Demonstrate every requested outcome and constraint in the objective: " + excerpt,
		"Record passing evidence from focused or reproducible verification of the requested outcome: " + excerpt,
	}
}

// draftPromptNeedsFollowUp deliberately recognizes only obvious ambiguity. A
// short but concrete request such as "fix parser" remains actionable, while
// deictic requests such as "fix it" ask one contextual question before AUTO.
func draftPromptNeedsFollowUp(objective string) bool {
	words := strings.Fields(strings.ToLower(objective))
	if len(words) < 2 {
		return true
	}
	meaningfulTargets := 0
	for _, word := range words {
		word = strings.Trim(word, ".,:;!?()[]{}\"'`")
		if word == "" || draftPromptStopWord(word) {
			continue
		}
		meaningfulTargets++
	}
	return meaningfulTargets == 0
}

func draftPromptStopWord(word string) bool {
	switch word {
	case "a", "an", "and", "better", "can", "change", "clean", "complete", "could", "create", "do", "everything", "finish", "fix", "handle", "i", "implement", "improve", "it", "make", "more", "on", "please", "polish", "review", "ship", "something", "stuff", "that", "the", "thing", "this", "to", "update", "we", "work", "would", "you":
		return true
	default:
		return false
	}
}

func boundDraftText(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	const suffix = "…"
	limit := maxBytes - len(suffix)
	if limit <= 0 {
		return strings.Repeat(".", maxBytes)
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return strings.TrimSpace(value[:limit]) + suffix
}

// ReviewedSpec converts explicitly reviewed values into the immutable portion
// of a Runtime spec. It deliberately requires the caller to supply a durable
// session ID and never creates a Runtime or dispatches work.
func (d Draft) ReviewedSpec(sessionID int64, objective string, criteria []string, budget BudgetLimits) (Spec, error) {
	if d.Version != GoalDraftVersion || d.Source != DraftSourceDeterministic {
		return Spec{}, fmt.Errorf("%w: unsupported goal draft", ErrInvalid)
	}
	if sessionID <= 0 {
		return Spec{}, fmt.Errorf("%w: session id must be positive", ErrInvalid)
	}
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return Spec{}, fmt.Errorf("%w: objective is required", ErrInvalid)
	}
	if len(criteria) == 0 || len(criteria) > MaxCriteria {
		return Spec{}, fmt.Errorf("%w: acceptance criteria count must be between 1 and %d", ErrInvalid, MaxCriteria)
	}
	accepted := make([]AcceptanceCriterion, 0, len(criteria))
	for index, description := range criteria {
		description = strings.TrimSpace(description)
		if description == "" {
			return Spec{}, fmt.Errorf("%w: acceptance criterion %d is required", ErrInvalid, index+1)
		}
		accepted = append(accepted, AcceptanceCriterion{
			ID:          fmt.Sprintf("criterion_%d", index+1),
			Description: description,
		})
	}
	if err := budget.Validate(); err != nil {
		return Spec{}, err
	}

	spec := Spec{SessionID: sessionID, Objective: objective, AcceptanceCriteria: accepted, Budget: budget}
	if err := validateSpec(spec, false); err != nil {
		return Spec{}, err
	}
	return spec, nil
}
