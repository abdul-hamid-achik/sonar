package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

type fakeExecutionLedger struct {
	mu          sync.Mutex
	events      []executionpkg.Event
	unresolved  []executionpkg.State
	fail        map[executionpkg.EventType]error
	onAppend    func(executionpkg.Event)
	listCursors []int64
	listErr     error
}

type normalizeExecutionArgsHook struct{}

func (normalizeExecutionArgsHook) Name() string { return "normalize-execution-args" }
func (normalizeExecutionArgsHook) PreToolUse(_ context.Context, call *llm.ToolCall) (bool, string) {
	call.Arguments["content"] = "normalized"
	return false, ""
}
func (normalizeExecutionArgsHook) PostToolUse(context.Context, llm.ToolCall, *string, bool) {}

type redactExecutionResultHook struct {
	secret string
}

func (redactExecutionResultHook) Name() string { return "redact-execution-result" }
func (redactExecutionResultHook) PreToolUse(context.Context, *llm.ToolCall) (bool, string) {
	return false, ""
}
func (h redactExecutionResultHook) PostToolUse(_ context.Context, _ llm.ToolCall, result *string, _ bool) {
	*result = strings.ReplaceAll(*result, h.secret, "[REDACTED]")
}

type terminalOrderingOutput struct {
	outputRecorder
	ledger               *fakeExecutionLedger
	resultBeforeTerminal bool
}

func (o *terminalOrderingOutput) ToolCallResult(callID, name, result string, isError bool, duration time.Duration) {
	events := o.ledger.snapshot()
	if len(events) == 0 || !events[len(events)-1].Type.Terminal() {
		o.resultBeforeTerminal = true
	}
	o.outputRecorder.ToolCallResult(callID, name, result, isError, duration)
}

func (l *fakeExecutionLedger) AppendExecutionEvent(_ context.Context, event executionpkg.Event) (executionpkg.Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := event.Validate(); err != nil {
		return executionpkg.Event{}, false, err
	}
	if err := l.fail[event.Type]; err != nil {
		return executionpkg.Event{}, false, err
	}
	event.ID = int64(len(l.events) + 1)
	l.events = append(l.events, event)
	if l.onAppend != nil {
		l.onAppend(event)
	}
	return event, true, nil
}

func (l *fakeExecutionLedger) ListExecutionRecoveryHazards(_ context.Context, _ int64, _ string, afterEventID int64, _ int) ([]executionpkg.State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listCursors = append(l.listCursors, afterEventID)
	if l.listErr != nil {
		return nil, l.listErr
	}
	states := make([]executionpkg.State, 0, len(l.unresolved))
	for _, state := range l.unresolved {
		if state.Latest.ID == 0 || state.Latest.ID > afterEventID {
			states = append(states, state)
		}
	}
	return states, nil
}

func (l *fakeExecutionLedger) snapshot() []executionpkg.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]executionpkg.Event(nil), l.events...)
}

func (l *fakeExecutionLedger) setUnresolved(states []executionpkg.State) {
	l.mu.Lock()
	l.unresolved = append([]executionpkg.State(nil), states...)
	l.mu.Unlock()
}

func (l *fakeExecutionLedger) setListError(err error) {
	l.mu.Lock()
	l.listErr = err
	l.mu.Unlock()
}

func (l *fakeExecutionLedger) recoveryCursors() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int64(nil), l.listCursors...)
}

type blockingRecoveryLedger struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *blockingRecoveryLedger) AppendExecutionEvent(_ context.Context, event executionpkg.Event) (executionpkg.Event, bool, error) {
	return event, true, nil
}

func (l *blockingRecoveryLedger) ListExecutionRecoveryHazards(ctx context.Context, _ int64, _ string, _ int64, _ int) ([]executionpkg.State, error) {
	l.once.Do(func() { close(l.started) })
	select {
	case <-l.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func executionEventTypes(events []executionpkg.Event) []executionpkg.EventType {
	types := make([]executionpkg.EventType, len(events))
	for i := range events {
		types[i] = events[i].Type
	}
	return types
}

func newLedgerAgent(t *testing.T, client llm.Client, registry *mcp.Registry, ledger *fakeExecutionLedger) (*Agent, string) {
	t.Helper()
	workDir := t.TempDir()
	ag := New(client, registry, 4096)
	ag.SetWorkDir(workDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.SetExecutionLedger(ledger)
	ag.SetExecutionSessionID(42, "")
	ag.RequireExecutionLedger(true)
	ag.AddUserMessage("execute the test tool")
	return ag, workDir
}

func TestMCPExecutionEffectIsAlwaysUnknownWithoutHostTrustPolicy(t *testing.T) {
	ag := New(nil, nil, 4096)
	for _, name := range []string{"cortex__status", "cortex__start_task", "mcphub__mcphub_call_tool"} {
		kind, effect := ag.executionKind(name)
		if kind != executionpkg.KindMCP || effect != executionpkg.EffectUnknown {
			t.Fatalf("executionKind(%q) = %q/%q, want MCP/outcome-unknown", name, kind, effect)
		}
	}
	if !mcpToolRequiresApproval() {
		t.Fatal("MCP dispatch bypassed the authorization path")
	}
}

func TestMCPAuthorizationPreservesConfiguredPolicyAndSkippedApprovalAuditReasons(t *testing.T) {
	tests := []struct {
		name         string
		checker      *permission.Checker
		callback     func(permission.ApprovalRequest)
		wantAllowed  bool
		wantApproval executionpkg.Approval
	}{
		{
			name: "skipped approval remains audited", checker: permission.NewChecker(nil, true),
			wantAllowed: true, wantApproval: executionpkg.ApprovalYolo,
		},
		{
			name: "default ask remains interactive", checker: permission.NewChecker(nil, false),
			callback:    func(request permission.ApprovalRequest) { request.Response <- permission.AllowOnce() },
			wantAllowed: true, wantApproval: executionpkg.ApprovalOnce,
		},
		{
			name: "explicit deny remains audited", checker: func() *permission.Checker {
				checker := permission.NewChecker(nil, false)
				if err := checker.SetPolicy("cortex__status", permission.PolicyDeny); err != nil {
					t.Fatal(err)
				}
				return checker
			}(), wantApproval: executionpkg.ApprovalPolicyDenied,
		},
		{
			name: "explicit deny wins over skipped approvals", checker: func() *permission.Checker {
				checker := permission.NewChecker(nil, true)
				if err := checker.SetPolicy("cortex__status", permission.PolicyDeny); err != nil {
					t.Fatal(err)
				}
				return checker
			}(), wantApproval: executionpkg.ApprovalPolicyDenied,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ag := New(nil, nil, 4096)
			ag.SetPermissionChecker(tt.checker)
			ag.SetApprovalCallback(tt.callback)
			decision, err := ag.decideToolAuthorization(context.Background(), AuthorityNormal, llm.ToolCall{
				ID: "call-cortex-status", Name: "cortex__status", Arguments: map[string]any{"taskId": "task-1"},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if decision.allowed != tt.wantAllowed || decision.approval != tt.wantApproval {
				t.Fatalf("authorization = allowed:%v approval:%q, want %v/%q", decision.allowed, decision.approval, tt.wantAllowed, tt.wantApproval)
			}
		})
	}
}

func TestExecutionLedgerOrdersEffectfulLifecycleAndIdentities(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID: "provider-write", Name: "write",
			Arguments: map[string]any{"path": "out.txt", "content": "ledger-secret"},
		}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	out := &outputRecorder{}
	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}

	events := ledger.snapshot()
	wantTypes := []executionpkg.EventType{
		executionpkg.EventRequested,
		executionpkg.EventApproved,
		executionpkg.EventStarted,
		executionpkg.EventCompleted,
	}
	if got := executionEventTypes(events); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("event order = %v, want %v", got, wantTypes)
	}
	identity := events[0].Identity
	if identity.ProviderCallID != "provider-write" || identity.CanonicalCallID != "provider-write" {
		t.Fatalf("provider/canonical IDs = %q/%q", identity.ProviderCallID, identity.CanonicalCallID)
	}
	for _, value := range []struct {
		name, prefix, value string
	}{
		{"run", "run_", identity.RunID},
		{"turn", "turn_", identity.TurnID},
		{"execution", "exec_", identity.ExecutionID},
		{"idempotency", "idem_", identity.IdempotencyKey},
	} {
		if !strings.HasPrefix(value.value, value.prefix) {
			t.Fatalf("%s identity %q lacks prefix %q", value.name, value.value, value.prefix)
		}
	}
	for _, event := range events[1:] {
		if event.Identity != identity {
			t.Fatalf("identity changed across lifecycle: %#v != %#v", event.Identity, identity)
		}
		if event.ArgumentsSHA256 != events[1].ArgumentsSHA256 {
			t.Fatal("effective argument hash changed after approval")
		}
	}
	if events[1].Approval != executionpkg.ApprovalYolo {
		t.Fatalf("approval = %q, want yolo", events[1].Approval)
	}
	for _, event := range events {
		if strings.Contains(event.Detail, "ledger-secret") || strings.Contains(event.ResultReceipt, "ledger-secret") {
			t.Fatal("raw tool arguments leaked into execution event")
		}
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "out.txt")); err != nil || string(data) != "ledger-secret" {
		t.Fatalf("write result = %q, %v", data, err)
	}
}

func TestExecutionLedgerRunTurnPreservesCallerTurnIdentity(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID: "provider-write", Name: "write",
			Arguments: map[string]any{"path": "out.txt", "content": "stable turn"},
		}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, _ := newLedgerAgent(t, client, nil, ledger)
	const turnID = "turn_goal_runtime_stable"
	if err := ag.RunTurn(context.Background(), &outputRecorder{}, turnID); err != nil {
		t.Fatal(err)
	}

	events := ledger.snapshot()
	if len(events) == 0 {
		t.Fatal("caller-owned turn produced no execution events")
	}
	for _, event := range events {
		if event.Identity.TurnID != turnID {
			t.Fatalf("execution event turn id = %q, want %q", event.Identity.TurnID, turnID)
		}
	}
}

func TestExecutionLedgerPostHookRedactsDurableUIAndModelReceipts(t *testing.T) {
	const secret = "super-secret-backend-value"
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID: "read-secret", Name: "read", Arguments: map[string]any{"path": "secret.txt"},
		}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	if err := os.WriteFile(filepath.Join(workDir, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	ag.AddToolHook(redactExecutionResultHook{secret: secret})
	out := &outputRecorder{}
	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}

	events := ledger.snapshot()
	if len(events) == 0 || events[len(events)-1].Type != executionpkg.EventCompleted {
		t.Fatalf("terminal events = %#v", events)
	}
	if receipt := events[len(events)-1].ResultReceipt; strings.Contains(receipt, secret) || !strings.Contains(receipt, "[REDACTED]") {
		t.Fatalf("durable receipt was not redacted: %q", receipt)
	}
	if receipt := strings.Join(out.toolResults, "\n"); strings.Contains(receipt, secret) || !strings.Contains(receipt, "[REDACTED]") {
		t.Fatalf("UI receipt was not redacted: %q", receipt)
	}
	var modelReceipt string
	for _, message := range ag.Messages() {
		if strings.Contains(message.Content, secret) {
			t.Fatalf("secret leaked into model message: %#v", message)
		}
		if message.Role == "tool" {
			modelReceipt += message.Content
		}
	}
	if !strings.Contains(modelReceipt, "[REDACTED]") {
		t.Fatalf("model tool receipt was not redacted: %q", modelReceipt)
	}
}

func TestExecutionLedgerRecordsInteractiveApprovalAndDenial(t *testing.T) {
	t.Run("approved", func(t *testing.T) {
		client := &scriptedClient{responses: [][]llm.StreamChunk{
			{{ToolCalls: []llm.ToolCall{{ID: "write", Name: "write", Arguments: map[string]any{"path": "ok", "content": "yes"}}}, Done: true}},
			{{Text: "done", Done: true}},
		}}
		ledger := &fakeExecutionLedger{}
		ag, _ := newLedgerAgent(t, client, nil, ledger)
		ag.SetPermissionChecker(permission.NewChecker(nil, false))
		ag.SetApprovalCallback(func(req permission.ApprovalRequest) {
			req.Response <- permission.ApprovalResponse{Allowed: true}
		})
		if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
			t.Fatal(err)
		}
		events := ledger.snapshot()
		want := []executionpkg.EventType{
			executionpkg.EventRequested, executionpkg.EventApprovalRequested,
			executionpkg.EventApproved, executionpkg.EventStarted, executionpkg.EventCompleted,
		}
		if got := executionEventTypes(events); !reflect.DeepEqual(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		if events[2].Approval != executionpkg.ApprovalOnce {
			t.Fatalf("approval = %q", events[2].Approval)
		}
	})

	t.Run("denied", func(t *testing.T) {
		client := &scriptedClient{responses: [][]llm.StreamChunk{
			{{ToolCalls: []llm.ToolCall{{ID: "write", Name: "write", Arguments: map[string]any{"path": "no", "content": "no"}}}, Done: true}},
			{{Text: "done", Done: true}},
		}}
		ledger := &fakeExecutionLedger{}
		ag, workDir := newLedgerAgent(t, client, nil, ledger)
		ag.SetPermissionChecker(permission.NewChecker(nil, false))
		ag.SetApprovalCallback(func(req permission.ApprovalRequest) {
			req.Response <- permission.ApprovalResponse{Allowed: false}
		})
		if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
			t.Fatal(err)
		}
		want := []executionpkg.EventType{executionpkg.EventRequested, executionpkg.EventApprovalRequested, executionpkg.EventDenied}
		if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		if _, err := os.Stat(filepath.Join(workDir, "no")); !os.IsNotExist(err) {
			t.Fatalf("denied write reached backend: %v", err)
		}
	})
}

func TestAutoBashApprovalRequestedLedgerDetailCarriesTheReason(t *testing.T) {
	run := func(t *testing.T, mode AuthorityMode, command string) ([]executionpkg.Event, bool) {
		t.Helper()
		client := &scriptedClient{responses: [][]llm.StreamChunk{
			{{ToolCalls: []llm.ToolCall{{ID: "bash", Name: "bash", Arguments: map[string]any{"command": command}}}, Done: true}},
			{{Text: "done", Done: true}},
		}}
		ledger := &fakeExecutionLedger{}
		ag, _ := newLedgerAgent(t, client, nil, ledger)
		ag.SetAuthorityMode(mode)
		ag.SetPermissionChecker(permission.NewChecker(nil, false))
		approvalAsked := false
		ag.SetApprovalCallback(func(req permission.ApprovalRequest) {
			approvalAsked = true
			req.Response <- permission.ApprovalResponse{Allowed: false}
		})
		if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
			t.Fatal(err)
		}
		return ledger.snapshot(), approvalAsked
	}

	t.Run("AUTO surfaces the rule that tripped", func(t *testing.T) {
		events, approvalAsked := run(t, AuthorityAutoScoped, "go build $(pwd)")
		if !approvalAsked {
			t.Fatal("non-admitted AUTO bash command bypassed interactive authorization")
		}
		want := []executionpkg.EventType{executionpkg.EventRequested, executionpkg.EventApprovalRequested, executionpkg.EventDenied}
		if got := executionEventTypes(events); !reflect.DeepEqual(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		if got := events[1].Detail; got != "interactive approval requested: dynamic shell syntax ($)" {
			t.Fatalf("approval requested detail = %q", got)
		}
	})

	t.Run("NORMAL keeps the bare request", func(t *testing.T) {
		events, approvalAsked := run(t, AuthorityNormal, "go build $(pwd)")
		if !approvalAsked {
			t.Fatal("NORMAL bash command bypassed interactive authorization")
		}
		if got := events[1].Detail; got != "interactive approval requested" {
			t.Fatalf("approval requested detail = %q", got)
		}
	})

	t.Run("AUTO admitted command skips approval", func(t *testing.T) {
		events, approvalAsked := run(t, AuthorityAutoScoped, "true")
		if approvalAsked {
			t.Fatal("admitted AUTO bash command opened an approval modal")
		}
		want := []executionpkg.EventType{
			executionpkg.EventRequested, executionpkg.EventApproved,
			executionpkg.EventStarted, executionpkg.EventCompleted,
		}
		if got := executionEventTypes(events); !reflect.DeepEqual(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		if events[1].Approval != executionpkg.ApprovalPolicy {
			t.Fatalf("AUTO approval audit = %q, want policy", events[1].Approval)
		}
	})
}

func TestExecutionLedgerBindsStartedToPostHookArguments(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID: "write", Name: "write", Arguments: map[string]any{"path": "normalized", "content": "original"},
		}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	ag.AddToolHook(normalizeExecutionArgsHook{})
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatal(err)
	}
	events := ledger.snapshot()
	if len(events) != 4 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].ArgumentsSHA256 == events[1].ArgumentsSHA256 {
		t.Fatal("requested hash did not preserve pre-hook arguments")
	}
	for _, event := range events[2:] {
		if event.ArgumentsSHA256 != events[1].ArgumentsSHA256 {
			t.Fatal("approved effective arguments changed before dispatch/terminal")
		}
	}
	data, err := os.ReadFile(filepath.Join(workDir, "normalized"))
	if err != nil || string(data) != "normalized" {
		t.Fatalf("backend used %q, %v", data, err)
	}
}

func TestExecutionLedgerCancellationClosesQueuedCall(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "one", Name: "write", Arguments: map[string]any{"path": "one", "content": "one"}},
					{ID: "two", Name: "write", Arguments: map[string]any{"path": "two", "content": "two"}},
				},
				Done: true,
			},
		},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag.AddToolHook(&cancelAfterFirstToolHook{cancel: cancel})
	err := ag.Run(ctx, &outputRecorder{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	want := []executionpkg.EventType{
		executionpkg.EventRequested, executionpkg.EventRequested,
		executionpkg.EventApproved, executionpkg.EventStarted, executionpkg.EventCompleted,
		executionpkg.EventCancelled,
	}
	if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(workDir, "one")); err != nil {
		t.Fatalf("first write missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "two")); !os.IsNotExist(err) {
		t.Fatalf("queued write was dispatched: %v", err)
	}
}

func TestExecutionLedgerCancelledReadErrorIsCancelledNotFailed(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID: "cancelled-read", Name: "read", Arguments: map[string]any{"path": "missing.txt"},
		}}, Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, _ := newLedgerAgent(t, client, nil, ledger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag.AddToolHook(&cancelAfterFirstToolHook{cancel: cancel})
	if err := ag.Run(ctx, &outputRecorder{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	want := []executionpkg.EventType{
		executionpkg.EventRequested,
		executionpkg.EventApproved,
		executionpkg.EventStarted,
		executionpkg.EventCancelled,
	}
	if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestExecutionLedgerRejectsOversizedToolBatchBeforeLifecycle(t *testing.T) {
	toolCalls := make([]llm.ToolCall, maxToolCallsPerResponse+1)
	for i := range toolCalls {
		toolCalls[i] = llm.ToolCall{
			ID:        "oversized",
			Name:      "write",
			Arguments: map[string]any{"path": "never", "content": "never"},
		}
	}
	client := &scriptedClient{responses: [][]llm.StreamChunk{{
		{ToolCalls: toolCalls[:32]},
		{ToolCalls: toolCalls[32:], Done: true},
	}}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	err := ag.Run(context.Background(), &outputRecorder{})
	if err == nil || !strings.Contains(err.Error(), "maximum per response") {
		t.Fatalf("Run error = %v", err)
	}
	if events := ledger.snapshot(); len(events) != 0 {
		t.Fatalf("oversized batch reached execution ledger: %#v", events)
	}
	if _, err := os.Stat(filepath.Join(workDir, "never")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized batch reached backend: %v", err)
	}
}

func TestExecutionLedgerPreservesDuplicateProviderIDs(t *testing.T) {
	workDir := t.TempDir()
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{
			{ID: "duplicate", Name: "read", Arguments: map[string]any{"path": "one"}},
			{ID: "duplicate", Name: "read", Arguments: map[string]any{"path": "two"}},
		}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag := New(client, nil, 4096)
	ag.SetWorkDir(workDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.SetExecutionLedger(ledger)
	ag.SetExecutionSessionID(42, "")
	ag.RequireExecutionLedger(true)
	ag.AddUserMessage("read both")
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatal(err)
	}
	events := ledger.snapshot()
	var requested []executionpkg.Event
	for _, event := range events {
		if event.Type == executionpkg.EventRequested {
			requested = append(requested, event)
		}
	}
	if len(requested) != 2 {
		t.Fatalf("requested events = %d", len(requested))
	}
	if requested[0].Identity.ProviderCallID != "duplicate" || requested[1].Identity.ProviderCallID != "duplicate" {
		t.Fatalf("raw provider IDs were not preserved: %#v", requested)
	}
	if requested[0].Identity.CanonicalCallID == requested[1].Identity.CanonicalCallID || requested[0].Identity.ExecutionID == requested[1].Identity.ExecutionID {
		t.Fatal("duplicate provider calls did not receive unique canonical/execution identities")
	}
}

func TestExecutionLedgerRequiresExplicitRecheckBeforeAUserRequestedRun(t *testing.T) {
	identity := executionpkg.Identity{
		SessionID: 42, WorkspaceID: "ignored", RunID: "run_old", TurnID: "turn_old",
		ExecutionID: "exec_old", IdempotencyKey: "idem_old", CanonicalCallID: "call_old",
		ToolName: "bash", Iteration: 1, Ordinal: 1, Kind: executionpkg.KindBuiltin, EffectClass: executionpkg.EffectUnknown,
	}
	ledger := &fakeExecutionLedger{unresolved: []executionpkg.State{{
		Identity: identity,
		Latest:   executionpkg.Event{Identity: identity, Type: executionpkg.EventStarted},
	}}}
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "unused", Done: true}}, {{Text: "after reset", Done: true}}}}
	ag, _ := newLedgerAgent(t, client, nil, ledger)

	err := ag.Run(context.Background(), &outputRecorder{})
	var unresolved *UnresolvedExecutionError
	if !errors.As(err, &unresolved) || unresolved.ExecutionID != "exec_old" {
		t.Fatalf("Run error = %T %v", err, err)
	}
	ledger.setUnresolved(nil)
	if err := ag.Run(context.Background(), &outputRecorder{}); !errors.As(err, &unresolved) {
		t.Fatalf("latched Run error = %T %v, want unresolved until explicit recheck", err, err)
	}
	if err := ag.RecheckExecutionRecovery(); err != nil {
		t.Fatal(err)
	}
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatalf("same session did not observe explicit durable recheck: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("provider calls after durable reconciliation = %d", client.calls)
	}
	if got := ledger.recoveryCursors(); !reflect.DeepEqual(got, []int64{0, 0}) {
		t.Fatalf("latched Run queried durable recovery without explicit recheck: %v", got)
	}
}

func TestExecutionLedgerStrictModeRefusesPersistedUnknownOutcome(t *testing.T) {
	identity := executionpkg.Identity{
		SessionID: 42, WorkspaceID: "ignored", RunID: "run_old", TurnID: "turn_old",
		ExecutionID: "exec_unknown", IdempotencyKey: "idem_unknown", CanonicalCallID: "call_unknown",
		ToolName: "server__mutate", Iteration: 1, Ordinal: 1, Kind: executionpkg.KindMCP, EffectClass: executionpkg.EffectUnknown,
	}
	ledger := &fakeExecutionLedger{unresolved: []executionpkg.State{{
		Identity: identity,
		Latest: executionpkg.Event{
			Identity: identity,
			Type:     executionpkg.EventOutcomeUnknown,
		},
	}}}
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "must not run", Done: true}}}}
	ag, _ := newLedgerAgent(t, client, nil, ledger)
	err := ag.Run(context.Background(), &outputRecorder{})
	var unresolved *UnresolvedExecutionError
	if !errors.As(err, &unresolved) || unresolved.ExecutionID != "exec_unknown" {
		t.Fatalf("Run error = %T %v", err, err)
	}
	if client.calls != 0 {
		t.Fatalf("provider ran with persisted unknown outcome: %d", client.calls)
	}
}

func TestRecheckExecutionRecoveryRetainsCursorAndQueriesBeforeProvider(t *testing.T) {
	identity := executionpkg.Identity{
		SessionID: 42, WorkspaceID: "ignored", RunID: "run_recheck", TurnID: "turn_recheck",
		ExecutionID: "exec_recheck", IdempotencyKey: "idem_recheck", CanonicalCallID: "call_recheck",
		ToolName: "server__mutate", Iteration: 1, Ordinal: 1, Kind: executionpkg.KindMCP, EffectClass: executionpkg.EffectUnknown,
	}
	ledger := &fakeExecutionLedger{unresolved: []executionpkg.State{{
		Identity: identity,
		Latest:   executionpkg.Event{Identity: identity, Type: executionpkg.EventOutcomeUnknown},
	}}}
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "after explicit retry", Done: true}}}}
	ag, _ := newLedgerAgent(t, client, nil, ledger)
	ag.SetExecutionSnapshotCursor(23)

	err := ag.Run(context.Background(), &outputRecorder{})
	var unresolved *UnresolvedExecutionError
	if !errors.As(err, &unresolved) {
		t.Fatalf("first Run error = %T %v", err, err)
	}
	messagesBefore := ag.Messages()
	if got := ledger.recoveryCursors(); !reflect.DeepEqual(got, []int64{23}) {
		t.Fatalf("initial recovery cursors = %v", got)
	}

	ledger.setUnresolved(nil)
	if err := ag.RecheckExecutionRecovery(); err != nil {
		t.Fatalf("RecheckExecutionRecovery() error = %v", err)
	}
	if client.calls != 0 {
		t.Fatalf("recheck scheduled %d provider calls", client.calls)
	}
	ag.mu.RLock()
	gotSession, gotCursor, gotLatch := ag.executionSessionID, ag.executionCursor, ag.unresolvedExecution
	ag.mu.RUnlock()
	if gotSession != 42 || gotCursor != 23 || gotLatch != nil {
		t.Fatalf("execution state after recheck = session %d cursor %d latch %#v", gotSession, gotCursor, gotLatch)
	}
	if got := ag.Messages(); !reflect.DeepEqual(got, messagesBefore) {
		t.Fatalf("recheck changed messages: got %#v want %#v", got, messagesBefore)
	}
	if got := ledger.recoveryCursors(); !reflect.DeepEqual(got, []int64{23}) {
		t.Fatalf("recheck queried outside Run: %v", got)
	}

	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatalf("explicit Run after durable reconciliation = %v", err)
	}
	if got := ledger.recoveryCursors(); !reflect.DeepEqual(got, []int64{23, 23}) {
		t.Fatalf("recovery cursors after explicit Run = %v", got)
	}
	if client.calls != 1 {
		t.Fatalf("provider calls after explicit Run = %d", client.calls)
	}
}

func TestRecheckExecutionRecoveryCannotBypassPersistingHazard(t *testing.T) {
	identity := executionpkg.Identity{
		SessionID: 42, WorkspaceID: "ignored", RunID: "run_still_unknown", TurnID: "turn_still_unknown",
		ExecutionID: "exec_still_unknown", IdempotencyKey: "idem_still_unknown", CanonicalCallID: "call_still_unknown",
		ToolName: "write", Iteration: 1, Ordinal: 1, Kind: executionpkg.KindBuiltin, EffectClass: executionpkg.Effectful,
	}
	ledger := &fakeExecutionLedger{unresolved: []executionpkg.State{{
		Identity: identity,
		Latest:   executionpkg.Event{Identity: identity, Type: executionpkg.EventOutcomeUnknown},
	}}}
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "must not run", Done: true}}}}
	ag, _ := newLedgerAgent(t, client, nil, ledger)
	ag.SetExecutionSnapshotCursor(11)

	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if err := ag.RecheckExecutionRecovery(); err != nil {
				t.Fatalf("RecheckExecutionRecovery() error = %v", err)
			}
		}
		err := ag.Run(context.Background(), &outputRecorder{})
		var unresolved *UnresolvedExecutionError
		if !errors.As(err, &unresolved) || unresolved.ExecutionID != identity.ExecutionID {
			t.Fatalf("Run %d error = %T %v", attempt+1, err, err)
		}
	}
	if got := ledger.recoveryCursors(); !reflect.DeepEqual(got, []int64{11, 11}) {
		t.Fatalf("recovery cursors = %v", got)
	}
	if client.calls != 0 {
		t.Fatalf("provider bypassed persistent hazard: %d calls", client.calls)
	}
}

func TestRecheckExecutionRecoveryFailsClosedOnProjectionError(t *testing.T) {
	identity := executionpkg.Identity{
		SessionID: 42, WorkspaceID: "ignored", RunID: "run_corrupt", TurnID: "turn_corrupt",
		ExecutionID: "exec_corrupt", IdempotencyKey: "idem_corrupt", CanonicalCallID: "call_corrupt",
		ToolName: "write", Iteration: 1, Ordinal: 1, Kind: executionpkg.KindBuiltin, EffectClass: executionpkg.Effectful,
	}
	ledger := &fakeExecutionLedger{unresolved: []executionpkg.State{{
		Identity: identity,
		Latest:   executionpkg.Event{Identity: identity, Type: executionpkg.EventOutcomeUnknown},
	}}}
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "must not run", Done: true}}}}
	ag, _ := newLedgerAgent(t, client, nil, ledger)

	if err := ag.Run(context.Background(), &outputRecorder{}); err == nil {
		t.Fatal("first Run accepted an unresolved execution")
	}
	ledger.setUnresolved(nil)
	ledger.setListError(errors.New("corrupt effective projection"))
	if err := ag.RecheckExecutionRecovery(); err != nil {
		t.Fatalf("RecheckExecutionRecovery() error = %v", err)
	}
	err := ag.Run(context.Background(), &outputRecorder{})
	if err == nil || !strings.Contains(err.Error(), "corrupt effective projection") {
		t.Fatalf("Run after projection corruption = %v", err)
	}
	if client.calls != 0 {
		t.Fatalf("provider ran after projection error: %d", client.calls)
	}
}

func TestRecheckExecutionRecoveryRejectsActiveTurnAndSchedulesNothing(t *testing.T) {
	ledger := &blockingRecoveryLedger{started: make(chan struct{}), release: make(chan struct{})}
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "done", Done: true}}}}
	ag := New(client, nil, 4096)
	ag.SetWorkDir(t.TempDir())
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.SetExecutionLedger(ledger)
	ag.SetExecutionSessionID(42, "")
	ag.RequireExecutionLedger(true)
	ag.AddUserMessage("wait at recovery query")

	done := make(chan error, 1)
	go func() { done <- ag.Run(context.Background(), &outputRecorder{}) }()
	<-ledger.started
	recheckErr := ag.RecheckExecutionRecovery()
	providerCallsBeforeRelease := client.calls
	close(ledger.release)
	runErr := <-done

	if !errors.Is(recheckErr, ErrExecutionRecoveryRecheckDuringTurn) {
		t.Fatalf("active-turn recheck error = %v", recheckErr)
	}
	if providerCallsBeforeRelease != 0 {
		t.Fatalf("recheck scheduled provider work while turn was blocked: %d", providerCallsBeforeRelease)
	}
	if runErr != nil {
		t.Fatalf("original Run error = %v", runErr)
	}
	if client.calls != 1 {
		t.Fatalf("original Run provider calls = %d", client.calls)
	}
}

func TestExecutionLedgerCompletedAfterSnapshotBlocksUntilCursorAdvances(t *testing.T) {
	identity := executionpkg.Identity{
		SessionID: 42, WorkspaceID: "ignored", RunID: "run_complete", TurnID: "turn_complete",
		ExecutionID: "exec_complete", IdempotencyKey: "idem_complete", CanonicalCallID: "call_complete",
		ToolName: "write", Iteration: 1, Ordinal: 1, Kind: executionpkg.KindBuiltin, EffectClass: executionpkg.Effectful,
	}
	ledger := &fakeExecutionLedger{unresolved: []executionpkg.State{{
		Identity: identity,
		Latest: executionpkg.Event{
			ID:       17,
			Identity: identity,
			Type:     executionpkg.EventCompleted,
		},
	}}}
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "after projection", Done: true}}}}
	ag, _ := newLedgerAgent(t, client, nil, ledger)
	ag.SetExecutionSnapshotCursor(10)
	err := ag.Run(context.Background(), &outputRecorder{})
	var unresolved *UnresolvedExecutionError
	if !errors.As(err, &unresolved) || unresolved.ExecutionID != "exec_complete" || unresolved.SnapshotCursor != 10 || unresolved.EventType != executionpkg.EventCompleted {
		t.Fatalf("Run error = %T %#v", err, unresolved)
	}
	if client.calls != 0 {
		t.Fatalf("provider ran before completed effect was projected: %d", client.calls)
	}

	ag.SetExecutionSnapshotCursor(17)
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatalf("cursor advance did not admit projected effect: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("provider calls after cursor advance = %d", client.calls)
	}
}

func TestExecutionLedgerStartedWriteFailurePreventsDispatch(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{
			{
				ToolCalls: []llm.ToolCall{{ID: "write", Name: "write", Arguments: map[string]any{"path": "blocked", "content": "no"}}},
				Done:      true,
			},
		},
	}}
	ledger := &fakeExecutionLedger{fail: map[executionpkg.EventType]error{executionpkg.EventStarted: errors.New("disk full")}}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	err := ag.Run(context.Background(), &outputRecorder{})
	if err == nil || !strings.Contains(err.Error(), "before dispatch") {
		t.Fatalf("Run error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "blocked")); !os.IsNotExist(err) {
		t.Fatalf("write ran after started-event failure: %v", err)
	}
	want := []executionpkg.EventType{executionpkg.EventRequested, executionpkg.EventApproved}
	if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestExecutionLedgerCancellationAfterIntentDoesNotStartBackend(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID: "write", Name: "write", Arguments: map[string]any{"path": "never", "content": "never"},
		}}, Done: true}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ledger := &fakeExecutionLedger{}
	ledger.onAppend = func(event executionpkg.Event) {
		if event.Type == executionpkg.EventStarted {
			cancel()
		}
	}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	out := &outputRecorder{}
	err := ag.Run(ctx, out)
	var unresolved *UnresolvedExecutionError
	if !errors.As(err, &unresolved) {
		t.Fatalf("Run error = %T %v", err, err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "never")); !os.IsNotExist(err) {
		t.Fatalf("backend started after cancellation at dispatch intent: %v", err)
	}
	want := []executionpkg.EventType{
		executionpkg.EventRequested,
		executionpkg.EventApproved,
		executionpkg.EventStarted,
		executionpkg.EventOutcomeUnknown,
	}
	if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if len(out.toolResults) != 1 || !strings.Contains(out.toolResults[0], "OUTCOME UNKNOWN") {
		t.Fatalf("receipt = %#v", out.toolResults)
	}
}

func TestExecutionLedgerHostKilledDispatchLatchesOutcomeUnknown(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{
			{ID: "bash", Name: "bash", Arguments: map[string]any{"command": "sleep 5", "timeout": 1}},
			{ID: "write", Name: "write", Arguments: map[string]any{"path": "must-not-run", "content": "no"}},
		}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	out := &terminalOrderingOutput{ledger: ledger}
	err := ag.Run(context.Background(), out)
	var unresolved *UnresolvedExecutionError
	if !errors.As(err, &unresolved) || unresolved.ToolName != "bash" {
		t.Fatalf("Run error = %T %v", err, err)
	}
	want := []executionpkg.EventType{
		executionpkg.EventRequested,
		executionpkg.EventRequested,
		executionpkg.EventApproved,
		executionpkg.EventStarted,
		executionpkg.EventOutcomeUnknown,
		executionpkg.EventCancelled,
	}
	if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if out.resultBeforeTerminal {
		t.Fatal("UI result was emitted before the terminal ledger append")
	}
	if len(out.toolResults) != 2 || !strings.Contains(out.toolResults[0], "OUTCOME UNKNOWN") || !strings.Contains(out.toolResults[1], "NOT DISPATCHED") {
		t.Fatalf("UI receipt = %#v", out.toolResults)
	}
	if _, err := os.Stat(filepath.Join(workDir, "must-not-run")); !os.IsNotExist(err) {
		t.Fatalf("later tool ran after unknown outcome: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("provider continued after unknown outcome: calls=%d", client.calls)
	}
	if err := ag.Run(context.Background(), &outputRecorder{}); !errors.As(err, &unresolved) {
		t.Fatalf("latched unknown outcome resumed: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("latched provider continued: calls=%d", client.calls)
	}
}

func TestExecutionLedgerAnsweredEffectErrorFailsWithoutStranding(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{
			{ID: "bash", Name: "bash", Arguments: map[string]any{"command": "exit 7"}},
			{ID: "write", Name: "write", Arguments: map[string]any{"path": "still-runs", "content": "yes"}},
		}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	out := &terminalOrderingOutput{ledger: ledger}
	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatalf("answered backend error stranded the session: %v", err)
	}
	want := []executionpkg.EventType{
		executionpkg.EventRequested,
		executionpkg.EventRequested,
		executionpkg.EventApproved,
		executionpkg.EventStarted,
		executionpkg.EventFailed,
		executionpkg.EventApproved,
		executionpkg.EventStarted,
		executionpkg.EventCompleted,
	}
	if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if out.resultBeforeTerminal {
		t.Fatal("UI result was emitted before the terminal ledger append")
	}
	if len(out.toolResults) != 2 || strings.Contains(out.toolResults[0], "OUTCOME UNKNOWN") {
		t.Fatalf("UI receipt = %#v", out.toolResults)
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "still-runs")); err != nil || string(data) != "yes" {
		t.Fatalf("later tool skipped after answered error: %q, %v", data, err)
	}
	if client.calls != 2 {
		t.Fatalf("provider halted after answered error: calls=%d", client.calls)
	}
}

func TestExecutionLedgerTerminalFailureStopsBatchAndLatchesSession(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "one", Name: "write", Arguments: map[string]any{"path": "one", "content": "one"}},
					{ID: "two", Name: "write", Arguments: map[string]any{"path": "two", "content": "two"}},
				},
				Done: true,
			},
		},
		{{Text: "new session", Done: true}},
	}}
	ledger := &fakeExecutionLedger{fail: map[executionpkg.EventType]error{executionpkg.EventCompleted: errors.New("terminal write failed")}}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	out := &outputRecorder{}
	err := ag.Run(context.Background(), out)
	var unresolved *UnresolvedExecutionError
	if !errors.As(err, &unresolved) || unresolved.ToolName != "write" {
		t.Fatalf("Run error = %T %v", err, err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "one")); err != nil {
		t.Fatalf("first backend did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "two")); !os.IsNotExist(err) {
		t.Fatalf("second backend ran after terminal failure: %v", err)
	}
	if !strings.Contains(strings.Join(out.toolResults, "\n"), "OUTCOME UNKNOWN") {
		t.Fatalf("missing unknown receipt: %#v", out.toolResults)
	}
	if err := ag.Run(context.Background(), &outputRecorder{}); !errors.As(err, &unresolved) {
		t.Fatalf("latched session resumed: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("provider resumed in latched session: calls=%d", client.calls)
	}

	ag.SetExecutionSessionID(43, "")
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatalf("new session did not clear terminal latch: %v", err)
	}
}

func TestExecutionPreflightRejectsInvalidAndUnavailableToolsBeforeStarted(t *testing.T) {
	tests := []struct {
		name     string
		registry *mcp.Registry
		call     llm.ToolCall
	}{
		{name: "invalid builtin", call: llm.ToolCall{ID: "write", Name: "write", Arguments: map[string]any{"content": "missing path"}}},
		{name: "memory unavailable", call: llm.ToolCall{ID: "memory", Name: "memory_save", Arguments: map[string]any{"content": "fact"}}},
		{name: "nil MCP registry", call: llm.ToolCall{ID: "mcp", Name: "server__mutate", Arguments: map[string]any{}}},
		{name: "unknown MCP route", registry: mcp.NewRegistry(), call: llm.ToolCall{ID: "mcp", Name: "server__mutate", Arguments: map[string]any{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.registry != nil {
				t.Cleanup(tt.registry.Close)
			}
			client := &scriptedClient{responses: [][]llm.StreamChunk{
				{{ToolCalls: []llm.ToolCall{tt.call}, Done: true}},
				{{Text: "done", Done: true}},
			}}
			ledger := &fakeExecutionLedger{}
			ag, _ := newLedgerAgent(t, client, tt.registry, ledger)
			if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
				t.Fatal(err)
			}
			want := []executionpkg.EventType{executionpkg.EventRequested, executionpkg.EventFailed}
			if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, want) {
				t.Fatalf("events = %v, want %v", got, want)
			}
		})
	}
}

func TestExecutionStopsConsecutiveMalformedToolIterations(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "bad-1", Name: "bash", Arguments: map[string]any{}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "bad-2", Name: "find", Arguments: map[string]any{}}}, Done: true}},
		{{Text: "must not be requested", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, _ := newLedgerAgent(t, client, nil, ledger)
	err := ag.Run(context.Background(), &outputRecorder{})
	if !errors.Is(err, ErrMalformedToolLoop) {
		t.Fatalf("Run error = %v, want ErrMalformedToolLoop", err)
	}
	if client.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", client.calls)
	}
	want := []executionpkg.EventType{
		executionpkg.EventRequested, executionpkg.EventFailed,
		executionpkg.EventRequested, executionpkg.EventFailed,
	}
	if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRequireExecutionLedgerFailsBeforeProviderWithoutLedger(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "must not run", Done: true}}}}
	ag := New(client, nil, 4096)
	ag.SetWorkDir(t.TempDir())
	ag.RequireExecutionLedger(true)
	ag.SetExecutionSessionID(42, "")
	ag.AddUserMessage("hello")
	err := ag.Run(context.Background(), &outputRecorder{})
	if !errors.Is(err, ErrExecutionLedgerRequired) {
		t.Fatalf("Run error = %v", err)
	}
	if client.calls != 0 {
		t.Fatalf("provider called without required ledger: %d", client.calls)
	}
}

func TestExecutionLedgerDBStoreConformsToAgentContract(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "execution.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workDir := t.TempDir()
	ag := New(&scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "write", Name: "write", Arguments: map[string]any{"path": "db", "content": "ok"}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}, nil, 4096)
	ag.SetWorkDir(workDir)
	workspaceID, err := ag.checkpointWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{Title: "execution", WorkspaceID: workspaceID})
	if err != nil {
		t.Fatal(err)
	}
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.SetExecutionLedger(store)
	ag.SetExecutionSessionID(session.ID, "")
	ag.RequireExecutionLedger(true)
	ag.AddUserMessage("write")
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatalf("agent/store lifecycle mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "db")); err != nil {
		t.Fatalf("backend did not run: %v", err)
	}
}
