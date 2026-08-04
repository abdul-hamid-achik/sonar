package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

type dispatchedBuiltinCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (counter *dispatchedBuiltinCounter) Name() string { return "dispatched-builtins" }

func (*dispatchedBuiltinCounter) PreToolUse(context.Context, *llm.ToolCall) (bool, string) {
	return false, ""
}

func (counter *dispatchedBuiltinCounter) PostToolUse(_ context.Context, call llm.ToolCall, _ *string, _ bool) {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.counts == nil {
		counter.counts = make(map[string]int)
	}
	counter.counts[call.Name]++
}

func (counter *dispatchedBuiltinCounter) count(name string) int {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	return counter.counts[name]
}

func TestRunSuppressesIdenticalBuiltinReadWithoutStateChange(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "probe.txt"), []byte("7"), 0o600); err != nil {
		t.Fatal(err)
	}
	readArgs := map[string]any{"path": "probe.txt"}
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "read-1", Name: "read", Arguments: readArgs}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "read-2", Name: "read", Arguments: readArgs}}, Done: true}},
		{{Text: "The file contains 7.", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	counter := &dispatchedBuiltinCounter{}
	ag := New(client, nil, 4096)
	ag.SetWorkDir(workDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetExecutionLedger(ledger)
	ag.SetExecutionSessionID(42, "")
	ag.AddToolHook(counter)
	ag.AddUserMessage("Read probe.txt and report its value.")
	out := &outputRecorder{}

	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := counter.count("read"); got != 1 {
		t.Fatalf("read backend dispatches = %d, want 1", got)
	}
	if len(out.toolResults) != 2 || out.toolResults[0] != "7" || out.toolResults[1] != repeatedBuiltinCorrection {
		t.Fatalf("tool receipts = %#v", out.toolResults)
	}

	var repeatedEvents []executionpkg.Event
	for _, event := range ledger.snapshot() {
		if event.Identity.CanonicalCallID == "read-2" {
			repeatedEvents = append(repeatedEvents, event)
		}
	}
	if len(repeatedEvents) != 2 || repeatedEvents[0].Type != executionpkg.EventRequested || repeatedEvents[1].Type != executionpkg.EventFailed {
		t.Fatalf("repeated call lifecycle = %#v, want requested -> failed", repeatedEvents)
	}
	if repeatedEvents[1].Approval != executionpkg.ApprovalNotApplicable ||
		!strings.Contains(repeatedEvents[1].Detail, "suppressed before dispatch") ||
		repeatedEvents[1].ResultReceipt != repeatedBuiltinCorrection {
		t.Fatalf("repeated call terminal receipt = %#v", repeatedEvents[1])
	}
	for _, event := range repeatedEvents {
		if event.Type == executionpkg.EventApproved || event.Type == executionpkg.EventStarted {
			t.Fatalf("suppressed call crossed dispatch barrier: %#v", repeatedEvents)
		}
	}

	messages := ag.Messages()
	var correction llm.Message
	for _, message := range messages {
		if message.Role == "tool" && message.ToolCallID == "read-2" {
			correction = message
			break
		}
	}
	if correction.Content != repeatedBuiltinCorrection || correction.ToolName != "read" {
		t.Fatalf("model correction message = %#v", correction)
	}
}

func TestRunAllowsIdenticalBuiltinReadAfterStateChangingDispatch(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "probe.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	readArgs := map[string]any{"path": "probe.txt"}
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "read-before", Name: "read", Arguments: readArgs}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{
			ID: "write-between", Name: "write", Arguments: map[string]any{"path": "probe.txt", "content": "after"},
		}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "read-after", Name: "read", Arguments: readArgs}}, Done: true}},
		{{Text: "The updated file contains after.", Done: true}},
	}}
	counter := &dispatchedBuiltinCounter{}
	ag := New(client, nil, 4096)
	ag.SetWorkDir(workDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.AddToolHook(counter)
	ag.AddUserMessage("Read, update, and read probe.txt again.")
	out := &outputRecorder{}

	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := counter.count("read"); got != 2 {
		t.Fatalf("read backend dispatches = %d, want 2", got)
	}
	if got := counter.count("write"); got != 1 {
		t.Fatalf("write backend dispatches = %d, want 1", got)
	}
	if len(out.toolResults) != 3 || out.toolResults[0] != "before" || out.toolResults[2] != "after" {
		t.Fatalf("tool receipts = %#v", out.toolResults)
	}
	if strings.Contains(strings.Join(out.toolResults, "\n"), "SUPPRESSED") {
		t.Fatalf("post-mutation read was suppressed: %#v", out.toolResults)
	}
}
