package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/supervisor"
)

func TestParseRootOptionsAcceptsReceiptIdentityFlags(t *testing.T) {
	var stderr, stdout bytes.Buffer
	options, err := parseRootOptions("sonar", []string{
		"--json", "--run-id", "run_ext", "--turn-id", "turn_ext", "--actor", "chalupa/step-solve",
		"-p", "hello",
	}, &stderr, &stdout)
	if err != nil {
		t.Fatalf("parseRootOptions: %v", err)
	}
	if !options.jsonReceipt {
		t.Fatal("--json should set jsonReceipt")
	}
	if options.runID != "run_ext" || options.turnID != "turn_ext" || options.actor != "chalupa/step-solve" {
		t.Fatalf("identity flags = %q %q %q", options.runID, options.turnID, options.actor)
	}
	if !options.promptProvided {
		t.Fatal("prompt should be marked provided")
	}
}

func TestResolveExternalTurnIdentityPrefersFlagsOverEnvironment(t *testing.T) {
	t.Setenv("SONAR_RUN_ID", "run_env")
	t.Setenv("SONAR_TURN_ID", "turn_env")
	t.Setenv("SONAR_ACTOR", "env-actor")

	runID, turnID, actor, err := resolveExternalTurnIdentity(rootOptions{runID: "run_flag"})
	if err != nil {
		t.Fatalf("resolveExternalTurnIdentity: %v", err)
	}
	if runID != "run_flag" {
		t.Fatalf("runID = %q, want the flag to win", runID)
	}
	if turnID != "turn_env" || actor != "env-actor" {
		t.Fatalf("env fallbacks = %q %q", turnID, actor)
	}
}

func TestResolveExternalTurnIdentityRejectsInvalidShapes(t *testing.T) {
	for _, test := range []struct {
		name    string
		options rootOptions
	}{
		{"oversized run id", rootOptions{runID: strings.Repeat("r", 200)}},
		{"oversized turn id", rootOptions{turnID: strings.Repeat("t", 200)}},
		{"oversized actor", rootOptions{actor: strings.Repeat("a", 300)}},
		{"non-utf8 actor", rootOptions{actor: "bad\xff"}},
	} {
		if _, _, _, err := resolveExternalTurnIdentity(test.options); err == nil {
			t.Fatalf("%s: expected a shape error", test.name)
		}
	}
}

func TestBoundedReceiptErrorCapsOnRuneBoundary(t *testing.T) {
	if got := boundedReceiptError(nil); got != "" {
		t.Fatalf("nil error should produce empty text, got %q", got)
	}
	long := strings.Repeat("é", maxReceiptErrorBytes)
	bounded := boundedReceiptError(&stringError{long})
	if len(bounded) > maxReceiptErrorBytes {
		t.Fatalf("bounded error is %d bytes, want <= %d", len(bounded), maxReceiptErrorBytes)
	}
	if !strings.HasPrefix(long, bounded) || bounded == "" {
		t.Fatal("bounded error must be a non-empty rune-safe prefix")
	}
}

type stringError struct{ msg string }

func (e *stringError) Error() string { return e.msg }

func TestReceiptDecisionProjectsSupervisorVerdict(t *testing.T) {
	projected := receiptDecision(supervisor.Decision{
		Action:   supervisor.ActionStop,
		Reason:   supervisor.StopBudget,
		Detail:   "goal budget exhausted",
		IssueIDs: []string{"item_1", "item_2"},
	})
	if projected.Action != "stop" || projected.Reason != "budget_exhausted" {
		t.Fatalf("projection = %+v", projected)
	}
	if len(projected.IssueIDs) != 2 || projected.IssueIDs[0] != "item_1" {
		t.Fatalf("issue ids = %v", projected.IssueIDs)
	}
	if projected.Detail != "goal budget exhausted" {
		t.Fatalf("detail = %q", projected.Detail)
	}
}
