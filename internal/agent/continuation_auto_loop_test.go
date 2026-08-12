package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	registrypkg "github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

type autoLoopStatusArgs struct {
	TaskID string `json:"taskId"`
}

type autoLoopBobContextArgs struct {
	Workspace string `json:"workspace"`
	Profile   string `json:"profile"`
}

type autoLoopBobPathArgs struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
}

type autoLoopBobPlaybookArgs struct {
	Workspace string `json:"workspace"`
	ID        string `json:"id"`
	Operation string `json:"operation"`
}

type autoLoopBackend struct {
	mu              sync.Mutex
	calls           []string
	contextProfiles []string
	statusDocument  func(int) map[string]any
}

func (backend *autoLoopBackend) record(name string) int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.calls = append(backend.calls, name)
	return len(backend.calls)
}

func (backend *autoLoopBackend) count(name string) int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	count := 0
	for _, call := range backend.calls {
		if call == name {
			count++
		}
	}
	return count
}

func (backend *autoLoopBackend) snapshot() []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]string(nil), backend.calls...)
}

func (backend *autoLoopBackend) recordContext(profile string) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.calls = append(backend.calls, "bob_context")
	backend.contextProfiles = append(backend.contextProfiles, profile)
}

func (backend *autoLoopBackend) profiles() []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]string(nil), backend.contextProfiles...)
}

type autoLoopMutatingOutput struct {
	outputRecorder
	onStart func(string, map[string]any)
}

func (output *autoLoopMutatingOutput) ToolCallStart(_ string, name string, args map[string]any) {
	if output.onStart != nil {
		output.onStart(name, args)
	}
}

type mutateAutoContextHook struct{}

func (mutateAutoContextHook) Name() string { return "mutate-auto-context" }

func (mutateAutoContextHook) PreToolUse(_ context.Context, call *llm.ToolCall) (bool, string) {
	if call.Name == "bob__bob_context" {
		call.Arguments["profile"] = "full"
	}
	return false, ""
}

func (mutateAutoContextHook) PostToolUse(context.Context, llm.ToolCall, *string, bool) {}

func TestAutoReadOnlyContinuationDispatchesWithoutProviderGeneration(t *testing.T) {
	workspace := t.TempDir()
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(workspace, 7, "bob_context", map[string]any{
			"workspace": workspace,
			"profile":   "compact",
		})
	})
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "source", Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}}}, Done: true}},
		{{Text: "finished after the host continuation", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 2,
	})

	if err := agent.RunTurn(context.Background(), &outputRecorder{}, "turn_auto_dispatch"); err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("provider generations = %d, want source request and final answer only", client.calls)
	}
	if got := backend.snapshot(); !reflect.DeepEqual(got, []string{"cortex_status", "bob_context"}) {
		t.Fatalf("backend calls = %v, want exact source then host continuation", got)
	}

	var hostMessage *llm.Message
	for _, message := range agent.Messages() {
		if len(message.ToolCalls) == 1 && message.ToolCalls[0].Name == "bob__bob_context" {
			copy := message
			hostMessage = &copy
			break
		}
	}
	if hostMessage == nil || !hostMessage.HostOwned || hostMessage.Content != "" {
		t.Fatalf("host continuation assistant message = %#v, want host-owned call-only message", hostMessage)
	}

	var requested executionpkg.Event
	for _, event := range ledger.snapshot() {
		if event.Type == executionpkg.EventRequested && event.Identity.ToolName == "bob__bob_context" {
			requested = event
			break
		}
	}
	if requested.Identity.ToolName == "" || requested.Identity.ProviderCallID != "" ||
		!strings.Contains(requested.Detail, "host scheduled typed read-only continuation") {
		t.Fatalf("auto continuation request provenance = %#v", requested)
	}
	if got := eventTypesForTool(ledger.snapshot(), "bob__bob_context"); !reflect.DeepEqual(got, []executionpkg.EventType{
		executionpkg.EventRequested, executionpkg.EventApproved, executionpkg.EventStarted, executionpkg.EventCompleted,
	}) {
		t.Fatalf("auto continuation events = %v", got)
	}
	if requested.ArgumentsSHA256 == "" {
		t.Fatal("host continuation request omitted its canonical argument hash")
	}
}

func TestAutoReadOnlyOpaqueContextDispatchesBeforeProviderGeneration(t *testing.T) {
	workspace := t.TempDir()
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(workspace, 11, "bob_context", map[string]any{
			"workspace": workspace,
			"profile":   "compact",
		})
	})
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "finished after opaque continuation", Done: true}}}}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 2,
	})
	document := autoLoopCortexStatus(workspace, 11, "bob_context", map[string]any{
		"workspace": workspace,
		"profile":   "compact",
	})
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	continuation := agent.InterpretContinuationResult(
		llm.ToolCall{Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}},
		&registrypkg.ToolResult{Content: string(encoded), Structured: encoded},
	)
	if continuation == nil {
		t.Fatal("exact external source did not produce an opaque continuation")
		return
	}

	if err := agent.RunTurnWithOptions(context.Background(), &outputRecorder{}, "turn_auto_opaque", TurnOptions{
		Continuation: continuation,
	}); err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 {
		t.Fatalf("provider generations = %d, want only final response after host read", client.calls)
	}
	if got := backend.snapshot(); !reflect.DeepEqual(got, []string{"bob_context"}) {
		t.Fatalf("backend calls = %v, want opaque continuation before provider", got)
	}
	if !continuation.consumed {
		t.Fatal("successful opaque continuation claim was not consumed")
	}
}

func TestOpaqueContextAdmissionFailureDoesNotConsumeOrReserveLA2History(t *testing.T) {
	workspace := t.TempDir()
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(workspace, 12, "bob_context", map[string]any{
			"workspace": workspace,
			"profile":   "compact",
		})
	})
	client := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "must not be requested", Done: true}}}}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationSuggest, MaxAutoSteps: 2,
	})
	document := autoLoopCortexStatus(workspace, 12, "bob_context", map[string]any{
		"workspace": workspace,
		"profile":   "compact",
	})
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	continuation := agent.InterpretContinuationResult(
		llm.ToolCall{Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}},
		&registrypkg.ToolResult{Content: string(encoded), Structured: encoded},
	)
	if continuation == nil {
		t.Fatal("exact external source did not produce an opaque continuation")
		return
	}
	fingerprint := continuation.continuation.Fingerprint
	if fingerprint == "" {
		t.Fatal("opaque continuation omitted its LA-2 history fingerprint")
	}

	// Force the bounded admission check to reject the previewed LA-2 suggestion
	// before either its one-shot context or history fingerprint may commit.
	agent.numCtx = 64
	err = agent.RunTurnWithOptions(context.Background(), &outputRecorder{}, "turn_auto_opaque_context_full", TurnOptions{
		Limits:       TurnLimits{MaxEvalTokens: 8},
		Continuation: continuation,
	})
	if !errors.Is(err, ErrTurnContextBudgetExceeded) {
		t.Fatalf("RunTurnWithOptions error = %v, want ErrTurnContextBudgetExceeded", err)
	}
	if client.calls != 0 || len(backend.snapshot()) != 0 || len(ledger.snapshot()) != 0 {
		t.Fatalf("rejected admission crossed a runtime boundary: provider=%d backend=%v ledger=%v", client.calls, backend.snapshot(), ledger.snapshot())
	}

	continuation.mu.Lock()
	consumed := continuation.consumed
	continuation.mu.Unlock()
	agent.mu.RLock()
	_, la2Reserved := agent.continuationHistory.seenSet[fingerprint]
	agent.mu.RUnlock()
	autoSteps, autoFingerprints := agent.autoContinuationHistorySnapshot()
	if consumed || la2Reserved || autoSteps != 0 || len(autoFingerprints) != 0 {
		t.Fatalf(
			"rejected admission burned opaque continuation: consumed=%v LA-2-reserved=%v auto-history=%d %v",
			consumed, la2Reserved, autoSteps, autoFingerprints,
		)
	}
}

func TestAutoReadOnlyPreflightRejectedHostContinuationDoesNotCountAsMalformedModelIteration(t *testing.T) {
	workspace := t.TempDir()
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(workspace, 13, "bob_context", map[string]any{
			"workspace": workspace,
			"profile":   "compact",
		})
	})
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "bad-model-call", Name: "bash", Arguments: map[string]any{}}}, Done: true}},
		{{Text: "recovered after one malformed model batch", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 2,
	})
	document := autoLoopCortexStatus(workspace, 13, "bob_context", map[string]any{
		"workspace": workspace,
		"profile":   "compact",
	})
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	continuation := agent.InterpretContinuationResult(
		llm.ToolCall{Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}},
		&registrypkg.ToolResult{Content: string(encoded), Structured: encoded},
	)
	if continuation == nil {
		t.Fatal("exact external source did not produce an opaque continuation")
		return
	}

	// The opaque type prevents this in production. Corrupt its private test-only
	// payload after exact construction so the host-scheduled batch reaches and is
	// wholly rejected by MCP argument preflight.
	continuation.mu.Lock()
	continuation.continuation.Call.Arguments["profile"] = 13
	continuation.mu.Unlock()

	err = agent.RunTurnWithOptions(context.Background(), &outputRecorder{}, "turn_auto_host_preflight", TurnOptions{
		Continuation: continuation,
	})
	if errors.Is(err, ErrMalformedToolLoop) {
		t.Fatalf("one malformed model batch inherited the host preflight rejection: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("provider generations = %d, want malformed batch then recovery", client.calls)
	}
	if got := backend.snapshot(); len(got) != 0 {
		t.Fatalf("preflight-rejected host continuation reached backend: %v", got)
	}
	hostEvents := eventsForTool(ledger.snapshot(), "bob__bob_context")
	if got := executionEventTypes(hostEvents); !reflect.DeepEqual(got, []executionpkg.EventType{
		executionpkg.EventRequested, executionpkg.EventFailed,
	}) {
		t.Fatalf("host continuation events = %v", got)
	}
	if len(hostEvents) != 2 || hostEvents[1].Approval != executionpkg.ApprovalNotApplicable ||
		!strings.Contains(hostEvents[1].Detail, "preflight rejected") {
		t.Fatalf("host continuation did not fail at preflight: %#v", hostEvents)
	}
	if got := eventTypesForTool(ledger.snapshot(), "bash"); !reflect.DeepEqual(got, []executionpkg.EventType{
		executionpkg.EventRequested, executionpkg.EventFailed,
	}) {
		t.Fatalf("single malformed model batch events = %v", got)
	}
}

func TestAutoReadOnlyContinuationRejectsHookArgumentMutation(t *testing.T) {
	workspace := t.TempDir()
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(workspace, 7, "bob_context", map[string]any{
			"workspace": workspace,
			"profile":   "compact",
		})
	})
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "source", Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}}}, Done: true}},
		{{Text: "continued after host refusal", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 2,
	})
	agent.AddToolHook(mutateAutoContextHook{})

	if err := agent.RunTurn(context.Background(), &outputRecorder{}, "turn_auto_hook_tamper"); err != nil {
		t.Fatal(err)
	}
	if client.calls != 2 {
		t.Fatalf("provider generations = %d, want source and post-refusal response", client.calls)
	}
	if backend.count("bob_context") != 0 {
		t.Fatalf("mutated host continuation reached backend: %v", backend.snapshot())
	}
	events := eventsForTool(ledger.snapshot(), "bob__bob_context")
	if got := executionEventTypes(events); !reflect.DeepEqual(got, []executionpkg.EventType{
		executionpkg.EventRequested, executionpkg.EventFailed,
	}) {
		t.Fatalf("mutated host continuation events = %v", got)
	}
	if len(events) != 2 || events[1].Approval != executionpkg.ApprovalHostRefused {
		t.Fatalf("mutated host continuation terminal event = %#v", events)
	}
}

func TestAutoReadOnlyContinuationOutputCannotMutateDispatchArguments(t *testing.T) {
	workspace := t.TempDir()
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(workspace, 7, "bob_context", map[string]any{
			"workspace": workspace,
			"profile":   "compact",
		})
	})
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "source", Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}}}, Done: true}},
		{{Text: "continued after immutable dispatch", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 2,
	})
	output := &autoLoopMutatingOutput{onStart: func(name string, args map[string]any) {
		if name == "bob__bob_context" {
			args["profile"] = "full"
			args["workspace"] = "/tmp/escaped"
		}
	}}

	if err := agent.RunTurn(context.Background(), output, "turn_auto_output_tamper"); err != nil {
		t.Fatal(err)
	}
	if got := backend.profiles(); !reflect.DeepEqual(got, []string{"compact"}) {
		t.Fatalf("backend profiles = %v, output mutated approved dispatch", got)
	}
	events := eventsForTool(ledger.snapshot(), "bob__bob_context")
	if len(events) == 0 {
		t.Fatal("host continuation produced no durable execution events")
	}
	requested := events[0]
	wantHash, err := executionpkg.HashCanonicalArguments(map[string]any{"workspace": workspace, "profile": "compact"})
	if err != nil {
		t.Fatal(err)
	}
	if requested.ArgumentsSHA256 != wantHash {
		t.Fatalf("requested hash = %s, want immutable %s", requested.ArgumentsSHA256, wantHash)
	}
}

func TestAutoReadOnlyContinuationStopsWhenRegistryChangesAtOutputBoundary(t *testing.T) {
	workspace := t.TempDir()
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(workspace, 7, "bob_context", map[string]any{
			"workspace": workspace,
			"profile":   "compact",
		})
	})
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "source", Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}}}, Done: true}},
		{{Text: "continued after stale continuation stopped", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 2,
	})
	output := &autoLoopMutatingOutput{onStart: func(name string, _ map[string]any) {
		if name == "bob__bob_context" {
			registry.Close()
		}
	}}

	if err := agent.RunTurn(context.Background(), output, "turn_auto_registry_change"); err != nil {
		t.Fatal(err)
	}
	if backend.count("bob_context") != 0 {
		t.Fatalf("stale registry call reached backend: %v", backend.snapshot())
	}
	if got := eventTypesForTool(ledger.snapshot(), "bob__bob_context"); !reflect.DeepEqual(got, []executionpkg.EventType{
		executionpkg.EventRequested, executionpkg.EventApproved, executionpkg.EventStarted, executionpkg.EventFailed,
	}) {
		t.Fatalf("stale continuation events = %v", got)
	}
}

func TestAutoReadOnlyContinuationStopsWhenHostPolicyChangesAtOutputBoundary(t *testing.T) {
	for _, policyChange := range []string{"scope", "permission"} {
		t.Run(policyChange, func(t *testing.T) {
			workspace := t.TempDir()
			backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
				return autoLoopCortexStatus(workspace, 7, "bob_context", map[string]any{
					"workspace": workspace,
					"profile":   "compact",
				})
			})
			client := &scriptedClient{responses: [][]llm.StreamChunk{
				{{ToolCalls: []llm.ToolCall{{ID: "source", Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}}}, Done: true}},
				{{Text: "continued after policy revocation", Done: true}},
			}}
			ledger := &fakeExecutionLedger{}
			agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
				Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 2,
			})
			checker := permission.NewChecker(nil, false)
			agent.SetPermissionChecker(checker)
			output := &autoLoopMutatingOutput{onStart: func(name string, _ map[string]any) {
				if name != "bob__bob_context" {
					return
				}
				switch policyChange {
				case "scope":
					agent.DenyAllMCPTools()
				case "permission":
					if err := checker.SetPolicy(name, permission.PolicyDeny); err != nil {
						t.Errorf("set deny policy: %v", err)
					}
				}
			}}

			if err := agent.RunTurn(context.Background(), output, "turn_auto_policy_change"); err != nil {
				t.Fatal(err)
			}
			if backend.count("bob_context") != 0 {
				t.Fatalf("revoked continuation reached backend: %v", backend.snapshot())
			}
			if got := eventTypesForTool(ledger.snapshot(), "bob__bob_context"); !reflect.DeepEqual(got, []executionpkg.EventType{
				executionpkg.EventRequested, executionpkg.EventApproved, executionpkg.EventStarted, executionpkg.EventFailed,
			}) {
				t.Fatalf("revoked continuation events = %v", got)
			}
		})
	}
}

func TestContinuationModesSuggestAndOffNeverAutoDispatch(t *testing.T) {
	for _, mode := range []config.ContinuationMode{config.ContinuationSuggest, config.ContinuationOff} {
		t.Run(string(mode), func(t *testing.T) {
			workspace := t.TempDir()
			backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
				return autoLoopCortexStatus(workspace, 7, "bob_context", map[string]any{
					"workspace": workspace,
					"profile":   "compact",
				})
			})
			client := &scriptedClient{responses: [][]llm.StreamChunk{
				{{ToolCalls: []llm.ToolCall{{ID: "source", Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}}}, Done: true}},
				{{Text: "finished without automatic dispatch", Done: true}},
			}}
			ledger := &fakeExecutionLedger{}
			agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
				Mode: mode, MaxAutoSteps: 2,
			})

			if err := agent.RunTurn(context.Background(), &outputRecorder{}, "turn_mode_"+string(mode)); err != nil {
				t.Fatal(err)
			}
			if client.calls != 2 || backend.count("cortex_status") != 1 || backend.count("bob_context") != 0 {
				t.Fatalf("mode %s provider=%d backend=%v", mode, client.calls, backend.snapshot())
			}
			for _, message := range agent.Messages() {
				if message.HostOwned && len(message.ToolCalls) > 0 {
					t.Fatalf("mode %s created host continuation message: %#v", mode, message)
				}
			}
		})
	}
}

func TestAutoReadOnlyContinuationBudgetStopsASecondRead(t *testing.T) {
	workspace := t.TempDir()
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(workspace, 7, "bob_path", map[string]any{
			"workspace": workspace,
			"path":      "internal/cli/hello.go",
		})
	})
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "source", Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}}}, Done: true}},
		{{Text: "budget stopped the next suggested read", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 1,
	})

	if err := agent.RunTurn(context.Background(), &outputRecorder{}, "turn_auto_budget"); err != nil {
		t.Fatal(err)
	}
	if got := backend.snapshot(); !reflect.DeepEqual(got, []string{"cortex_status", "bob_path"}) {
		t.Fatalf("bounded backend calls = %v, want no second bob_playbook auto step", got)
	}
	if backend.count("bob_playbook") != 0 || client.calls != 2 {
		t.Fatalf("budget leaked continuation: provider=%d backend=%v", client.calls, backend.snapshot())
	}
	if got := eventTypesForTool(ledger.snapshot(), "bob_playbook"); len(got) != 0 {
		t.Fatalf("over-budget continuation created durable intent: %v", got)
	}
}

func TestAutoReadOnlyContinuationPreservesFinalProviderIteration(t *testing.T) {
	for _, tc := range []struct {
		name             string
		maxIterations    int
		wantBobCalls     int
		wantProviderRuns int
	}{
		{name: "two iterations falls back to suggestion", maxIterations: 2, wantBobCalls: 0, wantProviderRuns: 2},
		{name: "three iterations admits one read and final answer", maxIterations: 3, wantBobCalls: 1, wantProviderRuns: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
				return autoLoopCortexStatus(workspace, 7, "bob_context", map[string]any{
					"workspace": workspace,
					"profile":   "compact",
				})
			})
			client := &scriptedClient{responses: [][]llm.StreamChunk{
				{{ToolCalls: []llm.ToolCall{{ID: "source", Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}}}, Done: true}},
				{{Text: "final provider answer", Done: true}},
			}}
			ledger := &fakeExecutionLedger{}
			agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
				Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 2,
			})
			agent.SetToolsConfig(config.ToolsConfig{MaxIterations: tc.maxIterations, AutoMaxIterations: tc.maxIterations})

			if err := agent.RunTurn(context.Background(), &outputRecorder{}, "turn_auto_final_provider"); err != nil {
				t.Fatal(err)
			}
			if got := backend.count("bob_context"); got != tc.wantBobCalls {
				t.Fatalf("bob context calls = %d, want %d (backend=%v)", got, tc.wantBobCalls, backend.snapshot())
			}
			if client.calls != tc.wantProviderRuns {
				t.Fatalf("provider generations = %d, want %d", client.calls, tc.wantProviderRuns)
			}
		})
	}
}

func TestAutoReadOnlyContinuationRepeatDoesNotLoop(t *testing.T) {
	workspace := t.TempDir()
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(workspace, 7, "bob_context", map[string]any{
			"workspace": workspace,
			"profile":   "compact",
		})
	})
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "source-1", Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "source-2", Name: "cortex__cortex_status", Arguments: map[string]any{"taskId": "task_auto"}}}, Done: true}},
		{{Text: "same revision was not replayed", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 2,
	})

	if err := agent.RunTurn(context.Background(), &outputRecorder{}, "turn_auto_repeat"); err != nil {
		t.Fatal(err)
	}
	if client.calls != 3 || backend.count("cortex_status") != 2 || backend.count("bob_context") != 1 {
		t.Fatalf("repeat guard provider=%d backend=%v", client.calls, backend.snapshot())
	}
	if got := eventTypesForTool(ledger.snapshot(), "bob__bob_context"); len(got) != 4 {
		t.Fatalf("repeat created more than one host continuation lifecycle: %v", got)
	}
}

func newAutoLoopAgent(
	t *testing.T,
	workspace string,
	client llm.Client,
	registry *registrypkg.Registry,
	ledger *fakeExecutionLedger,
	continuations config.ContinuationsConfig,
) *Agent {
	t.Helper()
	agent := New(client, registry, 8192)
	t.Cleanup(agent.Close)
	agent.SetWorkDir(workspace)
	agent.SetModeContext("test", BuildToolPolicy())
	agent.SetAuthorityMode(AuthorityAutoScoped)
	agent.SetContinuationsConfig(continuations)
	agent.SetPermissionChecker(permission.NewChecker(nil, false))
	agent.SetExecutionLedger(ledger)
	agent.SetExecutionSessionID(42, "")
	agent.RequireExecutionLedger(true)
	agent.AddUserMessage("exercise exact typed continuation handling")

	// The HTTP fixture is used only to keep this test in-process. Production
	// trust resolution deliberately requires local STDIO. Seed the same exact,
	// host-resolved catalog here so the loop test exercises runtime authority
	// without weakening that configuration boundary.
	agent.mu.Lock()
	agent.trustedMCP = map[string]trustedMCPServer{
		"cortex": {
			localOwner: "cortex",
			contracts: map[string]mcpAuthorityContract{
				"cortex_status": {effect: executionpkg.EffectReadOnly, auto: true},
			},
		},
		"bob": {
			localOwner: "bob",
			contracts: map[string]mcpAuthorityContract{
				"bob_context":  {effect: executionpkg.EffectReadOnly, auto: true},
				"bob_path":     {effect: executionpkg.EffectReadOnly, auto: true},
				"bob_playbook": {effect: executionpkg.EffectReadOnly, auto: true},
			},
		},
	}
	agent.mu.Unlock()
	return agent
}

func newAutoLoopRegistry(
	t *testing.T,
	workspace string,
	statusDocument func(int) map[string]any,
) (*autoLoopBackend, *registrypkg.Registry) {
	t.Helper()
	backend := &autoLoopBackend{statusDocument: statusDocument}
	safeAnnotations := autoLoopReadAnnotations()
	bobContextDocument := autoLoopBobFixture(t, workspace, "context-clean-v1.json")
	bobPathDocument := autoLoopBobFixture(t, workspace, "path-extension-v1.json")
	bobPlaybookDocument := autoLoopBobFixture(t, workspace, "playbook-ready-v1.json")

	cortexServer := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "cortex-loop-test", Version: "1.0.0"}, nil)
	sdkmcp.AddTool(cortexServer, &sdkmcp.Tool{Name: "cortex_status", Annotations: safeAnnotations},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, _ autoLoopStatusArgs) (*sdkmcp.CallToolResult, any, error) {
			call := backend.record("cortex_status")
			return autoLoopJSONResult(backend.statusDocument(call)), nil, nil
		})
	cortexHTTP := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return cortexServer },
		&sdkmcp.StreamableHTTPOptions{JSONResponse: true},
	))
	t.Cleanup(cortexHTTP.Close)

	bobServer := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "bob-loop-test", Version: "1.0.0"}, nil)
	sdkmcp.AddTool(bobServer, &sdkmcp.Tool{Name: "bob_context", Annotations: autoLoopReadAnnotations()},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, args autoLoopBobContextArgs) (*sdkmcp.CallToolResult, any, error) {
			backend.recordContext(args.Profile)
			return autoLoopJSONResult(bobContextDocument), nil, nil
		})
	sdkmcp.AddTool(bobServer, &sdkmcp.Tool{Name: "bob_path", Annotations: autoLoopReadAnnotations()},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, _ autoLoopBobPathArgs) (*sdkmcp.CallToolResult, any, error) {
			backend.record("bob_path")
			return autoLoopJSONResult(bobPathDocument), nil, nil
		})
	sdkmcp.AddTool(bobServer, &sdkmcp.Tool{Name: "bob_playbook", Annotations: autoLoopReadAnnotations()},
		func(_ context.Context, _ *sdkmcp.CallToolRequest, _ autoLoopBobPlaybookArgs) (*sdkmcp.CallToolResult, any, error) {
			backend.record("bob_playbook")
			return autoLoopJSONResult(bobPlaybookDocument), nil, nil
		})
	bobHTTP := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(
		func(*http.Request) *sdkmcp.Server { return bobServer },
		&sdkmcp.StreamableHTTPOptions{JSONResponse: true},
	))
	t.Cleanup(bobHTTP.Close)

	registry := registrypkg.NewRegistry()
	t.Cleanup(registry.Close)
	for _, server := range []config.ServerConfig{
		{Name: "cortex", Transport: "streamable-http", URL: cortexHTTP.URL},
		{Name: "bob", Transport: "streamable-http", URL: bobHTTP.URL},
	} {
		if _, err := registry.ConnectServer(context.Background(), server); err != nil {
			t.Fatalf("connect %s test server: %v", server.Name, err)
		}
	}
	return backend, registry
}

func autoLoopReadAnnotations() *sdkmcp.ToolAnnotations {
	closedWorld := false
	nonDestructive := false
	return &sdkmcp.ToolAnnotations{
		ReadOnlyHint: true, DestructiveHint: &nonDestructive,
		IdempotentHint: true, OpenWorldHint: &closedWorld,
	}
}

func autoLoopCortexStatus(workspace string, revision uint64, tool string, arguments map[string]any) map[string]any {
	return map[string]any{
		"ok":       true,
		"taskId":   "task_auto",
		"phase":    "planned",
		"revision": revision,
		"workspace": map[string]any{
			"root": workspace,
		},
		"actions": []any{map[string]any{
			"tool":      tool,
			"arguments": arguments,
		}},
		"rawAvailable": false,
	}
}

func autoLoopJSONResult(document map[string]any) *sdkmcp.CallToolResult {
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return &sdkmcp.CallToolResult{
		Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(encoded)}},
		StructuredContent: document,
	}
}

func autoLoopBobFixture(t *testing.T, workspace, name string) map[string]any {
	t.Helper()
	path := filepath.Join("..", "ecosystem", "testdata", "bob_v040", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("/workspace"), []byte(workspace))
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func eventsForTool(events []executionpkg.Event, tool string) []executionpkg.Event {
	filtered := make([]executionpkg.Event, 0, len(events))
	for _, event := range events {
		if event.Identity.ToolName == tool {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func eventTypesForTool(events []executionpkg.Event, tool string) []executionpkg.EventType {
	return executionEventTypes(eventsForTool(events, tool))
}
