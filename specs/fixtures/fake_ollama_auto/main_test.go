package main

import "testing"

func TestHasSuccessfulToolReceiptRequiresLatestExactIdentity(t *testing.T) {
	request := chatRequest{Messages: []chatMessage{
		{Role: "assistant", Content: "calling"},
		{Role: "tool", Content: "ok", ToolName: "bash", ToolCallID: "auto-safe-1"},
	}}
	if !hasSuccessfulToolReceipt(request, "auto-safe-1", "bash") {
		t.Fatal("latest exact successful receipt was rejected")
	}
	if hasSuccessfulToolReceipt(request, "auto-safe-2", "bash") {
		t.Fatal("an earlier receipt satisfied a later call identity")
	}

	request.Messages = append(request.Messages,
		chatMessage{Role: "assistant", Content: "calling again"},
		chatMessage{Role: "tool", Content: "exit status 1", ToolName: "bash", ToolCallID: "auto-safe-2"},
	)
	if hasSuccessfulToolReceipt(request, "auto-safe-1", "bash") {
		t.Fatal("a successful historical receipt was accepted after a newer tool result")
	}
	if hasSuccessfulToolReceipt(request, "auto-safe-2", "bash") {
		t.Fatal("a failed exact receipt was accepted")
	}

	request.Messages[len(request.Messages)-1].Content = "ok"
	if !hasSuccessfulToolReceipt(request, "auto-safe-2", "bash") {
		t.Fatal("latest exact successful second receipt was rejected")
	}
	if hasSuccessfulToolReceipt(request, "auto-safe-2", "read") {
		t.Fatal("tool-name mismatch was accepted")
	}
}
