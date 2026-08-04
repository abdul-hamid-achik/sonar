package goal

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestInferDraftPreservesPromptAndReturnsDeterministicSuggestions(t *testing.T) {
	prompt := "  Improve Cortex recovery\nwithout dispatching work.  "
	budget := BudgetLimits{MaxContinuationTurns: 8, MaxEvalTokens: 12_000, MaxWallTime: 30 * time.Minute}

	first, err := InferDraft(prompt, budget)
	if err != nil {
		t.Fatal(err)
	}
	second, err := InferDraft(prompt, budget)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("draft inference is not deterministic:\nfirst: %#v\nsecond: %#v", first, second)
	}
	if first.SourcePrompt != prompt {
		t.Fatalf("source prompt = %q, want exact %q", first.SourcePrompt, prompt)
	}
	if first.Objective != strings.TrimSpace(prompt) {
		t.Fatalf("objective = %q", first.Objective)
	}
	if first.Source != DraftSourceDeterministic || first.Version != GoalDraftVersion {
		t.Fatalf("draft identity = version %d source %q", first.Version, first.Source)
	}
	if len(first.AcceptanceCriteria) != 2 || first.Budget != budget {
		t.Fatalf("suggestions = %#v", first)
	}
	for _, criterion := range first.AcceptanceCriteria {
		if !strings.Contains(criterion, "Cortex recovery") || !strings.Contains(criterion, "without dispatching work") {
			t.Fatalf("criterion is not prompt-specific: %q", criterion)
		}
		if strings.Contains(criterion, "The requested outcome is complete") || strings.Contains(criterion, "Relevant verification passes") {
			t.Fatalf("criterion retained canned copy: %q", criterion)
		}
	}
	if first.NeedsFollowUp || first.FollowUpPrompt != "" {
		t.Fatalf("concrete prompt unexpectedly needs follow-up: %#v", first)
	}
}

func TestInferDraftRequestsOneContextualFollowUpForAmbiguousPrompts(t *testing.T) {
	for _, prompt := range []string{"fix it", "make this better", "continue"} {
		t.Run(prompt, func(t *testing.T) {
			draft, err := InferDraft(prompt, BudgetLimits{MaxWallTime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if !draft.NeedsFollowUp || !strings.Contains(draft.FollowUpPrompt, "concrete behavior or artifact") {
				t.Fatalf("ambiguous draft = %#v", draft)
			}
			if len(draft.AcceptanceCriteria) != 2 || !strings.Contains(draft.AcceptanceCriteria[0], prompt) {
				t.Fatalf("ambiguous draft lost editable prompt context: %#v", draft)
			}
		})
	}

	for _, prompt := range []string{"fix parser", "polish the model picker", "make Shift+Tab cycle modes"} {
		t.Run(prompt, func(t *testing.T) {
			draft, err := InferDraft(prompt, BudgetLimits{MaxWallTime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if draft.NeedsFollowUp || draft.FollowUpPrompt != "" {
				t.Fatalf("concrete draft unexpectedly needs follow-up: %#v", draft)
			}
		})
	}
}

func TestInferDraftCriteriaAreBoundedAndUTF8Safe(t *testing.T) {
	prompt := "Improve 模型 picker " + strings.Repeat("界", maxDraftCriterionExcerptBytes)
	draft, err := InferDraft(prompt, BudgetLimits{MaxContinuationTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, criterion := range draft.AcceptanceCriteria {
		if !utf8.ValidString(criterion) {
			t.Fatalf("criterion is invalid UTF-8: %q", criterion)
		}
		if len(criterion) > MaxCriterionBytes {
			t.Fatalf("criterion bytes = %d, max %d", len(criterion), MaxCriterionBytes)
		}
		if !strings.Contains(criterion, "模型 picker") || !strings.HasSuffix(criterion, "…") {
			t.Fatalf("criterion did not preserve a bounded prompt-specific excerpt: %q", criterion)
		}
	}
}

func TestInferDraftRejectsInvalidInputAndBudget(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		budget BudgetLimits
	}{
		{name: "empty", prompt: " \n\t "},
		{name: "nul", prompt: "ship\x00now"},
		{name: "invalid utf8", prompt: string([]byte{0xff})},
		{name: "too large", prompt: strings.Repeat("x", maxDraftSourceBytes+1)},
		{name: "negative budget", prompt: "ship", budget: BudgetLimits{MaxEvalTokens: -1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := InferDraft(test.prompt, test.budget); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestReviewedSpecRequiresExplicitValidReview(t *testing.T) {
	draft, err := InferDraft("Ship the recovery flow", BudgetLimits{MaxContinuationTurns: 8})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := draft.ReviewedSpec(42, "  Ship safely  ", []string{" Tests pass ", "Recovery is demonstrated"}, BudgetLimits{MaxContinuationTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	if spec.SessionID != 42 || spec.Objective != "Ship safely" || spec.Budget.MaxContinuationTurns != 3 {
		t.Fatalf("spec = %#v", spec)
	}
	want := []AcceptanceCriterion{
		{ID: "criterion_1", Description: "Tests pass"},
		{ID: "criterion_2", Description: "Recovery is demonstrated"},
	}
	if !reflect.DeepEqual(spec.AcceptanceCriteria, want) {
		t.Fatalf("criteria = %#v, want %#v", spec.AcceptanceCriteria, want)
	}
	if !spec.Cortex.Empty() || spec.ID != "" {
		t.Fatalf("review unexpectedly granted external authority: %#v", spec)
	}

	for _, test := range []struct {
		name      string
		draft     Draft
		sessionID int64
		objective string
		criteria  []string
		budget    BudgetLimits
	}{
		{name: "unsupported draft", draft: Draft{}, sessionID: 1, objective: "ship", criteria: []string{"done"}},
		{name: "missing session", draft: draft, objective: "ship", criteria: []string{"done"}},
		{name: "missing objective", draft: draft, sessionID: 1, criteria: []string{"done"}},
		{name: "missing criteria", draft: draft, sessionID: 1, objective: "ship"},
		{name: "blank criterion", draft: draft, sessionID: 1, objective: "ship", criteria: []string{" "}},
		{name: "invalid budget", draft: draft, sessionID: 1, objective: "ship", criteria: []string{"done"}, budget: BudgetLimits{MaxWallTime: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.draft.ReviewedSpec(test.sessionID, test.objective, test.criteria, test.budget); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}
}
