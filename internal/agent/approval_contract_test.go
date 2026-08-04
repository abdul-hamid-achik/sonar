package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

func TestLargeWriteApprovalCarriesBoundedPreviewAndWritesAtomically(t *testing.T) {
	content := strings.Repeat("large approval content\n", 600)
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "large-write", Name: "write", Arguments: map[string]any{
			"path": "large.txt", "content": content,
		}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	ag.numCtx = 16_384
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	var prompts atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts.Add(1)
		if request.ArgumentsSHA256 == "" || request.ArgumentsSHA256 != request.Preview.ArgumentsSHA256 {
			t.Errorf("approval hash mismatch: request=%q preview=%q", request.ArgumentsSHA256, request.Preview.ArgumentsSHA256)
		}
		if request.Preview.Kind != permission.PreviewFileWrite {
			t.Errorf("preview kind = %q", request.Preview.Kind)
		}
		expectedPath, err := ag.resolvePath(filepath.Join(workDir, "large.txt"))
		if err != nil {
			t.Errorf("resolve expected preview path: %v", err)
		}
		if request.Preview.Path != expectedPath {
			t.Errorf("preview path = %q", request.Preview.Path)
		}
		if request.Preview.ByteSize != int64(len(content)) {
			t.Errorf("preview bytes = %d, want %d", request.Preview.ByteSize, len(content))
		}
		if request.Preview.ContentSHA256 != executionpkg.HashText(content) {
			t.Errorf("content hash = %q", request.Preview.ContentSHA256)
		}
		if len(request.Preview.Diff) <= 4096 {
			t.Errorf("large diff was unexpectedly constrained to the old UI limit: %d bytes", len(request.Preview.Diff))
		}
		// The host receives an inspection copy; accidental renderer mutation
		// must not alter the hash-bound operation that reaches the backend.
		request.Args["content"] = "renderer mutation"
		request.Response <- permission.AllowOnce()
	})
	ag.AddUserMessage("write the complete file")
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatal(err)
	}
	if prompts.Load() != 1 {
		t.Fatalf("approval prompts = %d, want 1", prompts.Load())
	}
	data, err := os.ReadFile(filepath.Join(workDir, "large.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Fatalf("write was split or truncated: got %d bytes, want %d", len(data), len(content))
	}
}

func TestHostRefusalIsFailedNotUserDenied(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "refused-write", Name: "write", Arguments: map[string]any{
			"path": "refused.txt", "content": "no",
		}}}, Done: true}},
		{{Text: "understood", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		request.Response <- permission.Refuse("approval_preview_unavailable", "preview renderer failed")
	})
	out := &outputRecorder{}
	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "refused.txt")); !os.IsNotExist(err) {
		t.Fatalf("host-refused write reached backend: %v", err)
	}
	events := ledger.snapshot()
	want := []executionpkg.EventType{
		executionpkg.EventRequested,
		executionpkg.EventApprovalRequested,
		executionpkg.EventFailed,
	}
	if got := executionEventTypes(events); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for _, event := range events {
		if event.Type == executionpkg.EventDenied {
			t.Fatalf("host refusal was recorded as user denial: %#v", event)
		}
	}
	if result := strings.Join(out.toolResults, "\n"); !strings.Contains(result, "approval_preview_unavailable") || !strings.Contains(result, "Do not retry unchanged") {
		t.Fatalf("host refusal result = %q", result)
	}
}

func TestRepeatedIdenticalHostRefusalStopsAfterTwoAttempts(t *testing.T) {
	args := map[string]any{"path": "never.txt", "content": "never"}
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "refusal-one", Name: "write", Arguments: args}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "refusal-two", Name: "write", Arguments: args}}, Done: true}},
		{{Text: "should not be reached", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	var prompts atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts.Add(1)
		request.Response <- permission.Refuse("approval_preview_unavailable", "preview renderer failed")
	})
	err := ag.Run(context.Background(), &outputRecorder{})
	if !errors.Is(err, ErrRepeatedHostRefusal) {
		t.Fatalf("Run error = %v, want ErrRepeatedHostRefusal", err)
	}
	if prompts.Load() != maxIdenticalHostRefusals {
		t.Fatalf("approval prompts = %d, want %d", prompts.Load(), maxIdenticalHostRefusals)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "never.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("repeated refused write reached backend: %v", statErr)
	}
	failed, denied := 0, 0
	for _, event := range ledger.snapshot() {
		switch event.Type {
		case executionpkg.EventFailed:
			failed++
		case executionpkg.EventDenied:
			denied++
		}
	}
	if failed != maxIdenticalHostRefusals || denied != 0 {
		t.Fatalf("terminal events failed=%d denied=%d", failed, denied)
	}
}

func TestTypedUserDenialPreservesDeniedLifecycle(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "user-denied", Name: "write", Arguments: map[string]any{
			"path": "denied.txt", "content": "denied",
		}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, _ := newLedgerAgent(t, client, nil, ledger)
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		request.Response <- permission.Deny()
	})
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatal(err)
	}
	events := ledger.snapshot()
	if got := executionEventTypes(events); len(got) != 3 || got[2] != executionpkg.EventDenied {
		t.Fatalf("events = %v", got)
	}
	if events[2].Approval != executionpkg.ApprovalUserDenied {
		t.Fatalf("approval = %q", events[2].Approval)
	}
}
