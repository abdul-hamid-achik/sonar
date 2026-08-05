package permission

import (
	"context"
	"strings"
	"testing"
	"time"
)

// An AUTO run left alone stops at its first approval and spends the turn's
// entire remaining wall budget waiting for an answer that is not coming. The
// unattended outcome has to be a refusal, because a refusal lets dispatch
// continue with the rest of the turn while a cancellation ends it.
func TestUnansweredApprovalRefusesRatherThanCancels(t *testing.T) {
	never := func(ApprovalRequest) {} // no human, no answer

	response := ResolveApprovalContextWithTimeout(
		context.Background(), ApprovalRequest{ToolName: "bash"}, never, 20*time.Millisecond)

	if response.Decision != DecisionHostRefuse {
		t.Fatalf("decision = %q, want a host refusal", response.Decision)
	}
	if response.Allowed {
		t.Fatal("a timeout granted permission")
	}
	if response.Code != "approval_timeout" {
		t.Errorf("code = %q, want approval_timeout", response.Code)
	}
	// The model reads this. It has to say what happened and what not to do,
	// or the next iteration re-sends the same call.
	if !strings.Contains(response.Message, "Do not retry it unchanged") {
		t.Errorf("refusal does not tell the model how to proceed: %q", response.Message)
	}
}

// The timeout must not overtake a real decision, or a user who answers slowly
// has their approval discarded.
func TestAnAnsweredApprovalWinsOverTheTimeout(t *testing.T) {
	answer := func(request ApprovalRequest) {
		time.Sleep(10 * time.Millisecond)
		request.Response <- AllowOnce()
	}

	response := ResolveApprovalContextWithTimeout(
		context.Background(), ApprovalRequest{ToolName: "bash"}, answer, 2*time.Second)

	if !response.Allowed || response.Decision != DecisionAllowOnce {
		t.Fatalf("a slow but real approval was discarded: %#v", response)
	}
}

// Cancellation still ends the turn. A timeout is an unattended refusal of one
// call; ctrl-c is the user stopping everything, and conflating them would make
// an interrupted run continue.
func TestCancellationIsStillACancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	response := ResolveApprovalContextWithTimeout(
		ctx, ApprovalRequest{ToolName: "bash"}, func(ApprovalRequest) {}, time.Hour)

	if response.Decision != DecisionCancelled {
		t.Fatalf("decision = %q, want a cancellation", response.Decision)
	}
}

// Zero is the interactive default and must wait, not refuse immediately. A
// harness that refused every prompt the instant it appeared would be worse
// than the problem this solves.
func TestZeroTimeoutWaitsForTheHuman(t *testing.T) {
	answer := func(request ApprovalRequest) {
		time.Sleep(30 * time.Millisecond)
		request.Response <- AllowOnce()
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		response := ResolveApprovalContextWithTimeout(
			context.Background(), ApprovalRequest{ToolName: "bash"}, answer, timeout)
		if !response.Allowed {
			t.Fatalf("timeout %v did not wait for the answer: %#v", timeout, response)
		}
	}
}

// The plain entry point keeps its old behaviour exactly, so every existing
// caller waits as before.
func TestResolveApprovalContextStillWaitsIndefinitely(t *testing.T) {
	answer := func(request ApprovalRequest) {
		time.Sleep(30 * time.Millisecond)
		request.Response <- AllowOnce()
	}
	if response := ResolveApprovalContext(context.Background(), ApprovalRequest{ToolName: "bash"}, answer); !response.Allowed {
		t.Fatalf("the unbounded boundary stopped waiting: %#v", response)
	}
}

// A missing approval surface is still a refusal, and it must not be delayed by
// a timeout that has nothing to wait for.
func TestMissingCallbackRefusesImmediately(t *testing.T) {
	start := time.Now()
	response := ResolveApprovalContextWithTimeout(
		context.Background(), ApprovalRequest{ToolName: "bash"}, nil, time.Hour)
	if response.Decision != DecisionHostRefuse || response.Code != "approval_ui_unavailable" {
		t.Fatalf("missing UI = %#v", response)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a missing callback waited %v", elapsed)
	}
}
