package main

import (
	"testing"

	"github.com/abdul-hamid-achik/sonar/specs/fixtures/openaiwire"
)

func TestHasSuccessfulToolReceiptRequiresLatestExactIdentity(t *testing.T) {
	request := openaiwire.ChatRequest{Messages: []openaiwire.Message{
		{Role: "assistant", Content: "calling"},
		{Role: "tool", Content: "ok", Name: "bash", ToolCallID: "auto-safe-1"},
	}}
	if !openaiwire.HasSuccessfulToolReceipt(request, "auto-safe-1", "bash") {
		t.Fatal("latest exact successful receipt was rejected")
	}
	if openaiwire.HasSuccessfulToolReceipt(request, "auto-safe-2", "bash") {
		t.Fatal("an earlier receipt satisfied a later call identity")
	}

	request.Messages = append(request.Messages,
		openaiwire.Message{Role: "assistant", Content: "calling again"},
		openaiwire.Message{Role: "tool", Content: "exit status 1", Name: "bash", ToolCallID: "auto-safe-2"},
	)
	if openaiwire.HasSuccessfulToolReceipt(request, "auto-safe-1", "bash") {
		t.Fatal("a successful historical receipt was accepted after a newer tool result")
	}
	if openaiwire.HasSuccessfulToolReceipt(request, "auto-safe-2", "bash") {
		t.Fatal("a failed exact receipt was accepted")
	}

	request.Messages[len(request.Messages)-1].Content = "ok"
	if !openaiwire.HasSuccessfulToolReceipt(request, "auto-safe-2", "bash") {
		t.Fatal("latest exact successful second receipt was rejected")
	}
	if openaiwire.HasSuccessfulToolReceipt(request, "auto-safe-2", "read") {
		t.Fatal("tool-name mismatch was accepted")
	}
}
