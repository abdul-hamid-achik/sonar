package agent

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	mcpPkg "github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// cortexOptionalWorkspaceSchema mirrors the schema shape that produced the
// defect: `goal` is required, `workspace` is optional and untyped, and its
// description tells the model it already defaults sensibly — so the model
// omits it and the workspace-effectful contract never matched anything.
func cortexOptionalWorkspaceSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"goal": map[string]any{"type": "string"},
			"workspace": map[string]any{
				"description": "repository directory; defaults to the server working directory",
			},
		},
		"required": []any{"goal"},
	}
}

func workspacePinSnapshot(tools ...llm.ToolDef) mcpPkg.ToolSnapshot {
	return mcpPkg.ToolSnapshot{
		Epoch:            1,
		AvailableServers: []string{"cortex", "mcphub"},
		Tools:            tools,
	}
}

func newWorkspacePinAgent(t *testing.T) (*Agent, string) {
	t.Helper()
	workspace := t.TempDir()
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{Name: "cortex", Command: "cortex", Transport: "stdio"},
		{Name: "mcphub", Command: "/opt/homebrew/bin/mcphub", Transport: "stdio"},
	})
	return ag, workspace
}

// TestOptionalWorkspaceOmissionUsedToDefeatTheTrustCatalogue states the defect
// itself: a route the operator listed under workspace_effectful still failed to
// auto-authorize in AUTO purely because the model omitted an optional argument.
func TestOptionalWorkspaceOmissionUsedToDefeatTheTrustCatalogue(t *testing.T) {
	ag, workspace := newWorkspacePinAgent(t)
	definition := llm.ToolDef{Name: "cortex__cortex_open_task", Parameters: cortexOptionalWorkspaceSchema()}
	snapshot := workspacePinSnapshot(definition)
	omitted := llm.ToolCall{
		Name:      "cortex__cortex_open_task",
		Arguments: map[string]any{"goal": "ship the fix"},
	}

	contract, trusted := ag.trustedMCPContract(omitted)
	if !trusted || !contract.workspaceScoped || contract.effect != executionpkg.Effectful {
		t.Fatalf("route is not a workspace-effectful contract: %#v trusted=%v", contract, trusted)
	}
	if ag.authorityAutoApproves(AuthorityAutoScoped, omitted, executionpkg.KindMCP) {
		t.Fatal("unpinned call auto-approved; the containment check had nothing to verify")
	}

	pinned, ok := ag.pinCataloguedWorkspaceArgument(AuthorityAutoScoped, omitted, executionpkg.KindMCP, snapshot)
	if !ok {
		t.Fatal("catalogued workspace-effectful route with a declared workspace was not pinned")
	}
	if got := pinned.Arguments[workspaceArgumentKey]; got != workspace {
		t.Fatalf("pinned workspace = %v, want %q", got, workspace)
	}
	if got := pinned.Arguments["goal"]; got != "ship the fix" {
		t.Fatalf("pin disturbed a model-supplied argument: %v", got)
	}
	if !ag.authorityAutoApproves(AuthorityAutoScoped, pinned, executionpkg.KindMCP) {
		t.Fatal("pinned call still did not qualify for scoped AUTO authority")
	}
	// The pin must not reach back into the call the model actually made.
	if _, leaked := omitted.Arguments[workspaceArgumentKey]; leaked {
		t.Fatal("pin mutated the caller's argument map in place")
	}
}

func TestPinCataloguedWorkspaceArgumentPreconditions(t *testing.T) {
	openTask := llm.ToolDef{Name: "cortex__cortex_open_task", Parameters: cortexOptionalWorkspaceSchema()}
	gatewayOpenTask := llm.ToolDef{
		Name: "mcphub__cortex__cortex_open_task", Parameters: cortexOptionalWorkspaceSchema(),
	}
	readOnly := llm.ToolDef{Name: "cortex__cortex_status", Parameters: cortexOptionalWorkspaceSchema()}
	uncatalogued := llm.ToolDef{Name: "cortex__cortex_purge", Parameters: cortexOptionalWorkspaceSchema()}
	noWorkspaceProperty := llm.ToolDef{
		Name: "cortex__cortex_open_task",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"goal": map[string]any{"type": "string"}},
			"required":   []any{"goal"},
		},
	}
	nonStringWorkspace := llm.ToolDef{
		Name: "cortex__cortex_open_task",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workspace": map[string]any{"type": "object"},
			},
		},
	}
	gatewayEnvelope := llm.ToolDef{
		Name: "mcphub__mcphub_call_tool",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"server":    map[string]any{"type": "string"},
				"tool":      map[string]any{"type": "string"},
				"arguments": map[string]any{"type": "object"},
				// Even if a gateway advertised this, it describes the envelope,
				// not the downstream target's inputs.
				"workspace": map[string]any{"type": "string"},
			},
		},
	}

	tests := []struct {
		name       string
		mode       AuthorityMode
		clearWork  bool
		denyTool   string
		definition llm.ToolDef
		call       llm.ToolCall
		wantPin    bool
	}{
		{
			name: "catalogued workspace-effectful route with declared workspace",
			mode: AuthorityAutoScoped, definition: openTask,
			call:    llm.ToolCall{Name: "cortex__cortex_open_task", Arguments: map[string]any{"goal": "g"}},
			wantPin: true,
		},
		{
			name: "pinned gateway route resolves the same downstream contract",
			mode: AuthorityAutoScoped, definition: gatewayOpenTask,
			call:    llm.ToolCall{Name: "mcphub__cortex__cortex_open_task", Arguments: map[string]any{"goal": "g"}},
			wantPin: true,
		},
		{
			name: "explicit nil workspace is an omission",
			mode: AuthorityAutoScoped, definition: openTask,
			call: llm.ToolCall{
				Name:      "cortex__cortex_open_task",
				Arguments: map[string]any{"goal": "g", "workspace": nil},
			},
			wantPin: true,
		},
		{
			name: "absent argument map still pins",
			mode: AuthorityAutoScoped, definition: openTask,
			call:    llm.ToolCall{Name: "cortex__cortex_open_task"},
			wantPin: true,
		},
		{
			name: "model-supplied workspace is never overwritten",
			mode: AuthorityAutoScoped, definition: openTask,
			call: llm.ToolCall{
				Name:      "cortex__cortex_open_task",
				Arguments: map[string]any{"goal": "g", "workspace": "/somewhere/else"},
			},
		},
		{
			name: "model-supplied empty workspace is still the model's value",
			mode: AuthorityAutoScoped, definition: openTask,
			call: llm.ToolCall{
				Name:      "cortex__cortex_open_task",
				Arguments: map[string]any{"goal": "g", "workspace": ""},
			},
		},
		{
			name: "schema without a workspace property is never invented",
			mode: AuthorityAutoScoped, definition: noWorkspaceProperty,
			call: llm.ToolCall{Name: "cortex__cortex_open_task", Arguments: map[string]any{"goal": "g"}},
		},
		{
			name: "schema declaring a non-string workspace is refused",
			mode: AuthorityAutoScoped, definition: nonStringWorkspace,
			call: llm.ToolCall{Name: "cortex__cortex_open_task", Arguments: map[string]any{"goal": "g"}},
		},
		{
			name: "tool absent from the turn snapshot is refused",
			mode: AuthorityAutoScoped, definition: readOnly,
			call: llm.ToolCall{Name: "cortex__cortex_open_task", Arguments: map[string]any{"goal": "g"}},
		},
		{
			name: "read-only catalogued route is never rewritten",
			mode: AuthorityAutoScoped, definition: readOnly,
			call: llm.ToolCall{Name: "cortex__cortex_status", Arguments: map[string]any{}},
		},
		{
			name: "uncatalogued route is never rewritten",
			mode: AuthorityAutoScoped, definition: uncatalogued,
			call: llm.ToolCall{Name: "cortex__cortex_purge", Arguments: map[string]any{"goal": "g"}},
		},
		{
			name: "lazy gateway envelope is excluded",
			mode: AuthorityAutoScoped, definition: gatewayEnvelope,
			call: llm.ToolCall{
				Name: "mcphub__mcphub_call_tool",
				Arguments: map[string]any{
					"server": "cortex", "tool": "cortex_open_task",
					"arguments": map[string]any{"goal": "g"},
				},
			},
		},
		{
			name: "NORMAL authority is unaffected",
			mode: AuthorityNormal, definition: openTask,
			call: llm.ToolCall{Name: "cortex__cortex_open_task", Arguments: map[string]any{"goal": "g"}},
		},
		{
			name: "PLAN authority is unaffected",
			mode: AuthorityPlan, definition: openTask,
			call: llm.ToolCall{Name: "cortex__cortex_open_task", Arguments: map[string]any{"goal": "g"}},
		},
		{
			name: "no active workspace is unaffected",
			mode: AuthorityAutoScoped, clearWork: true, definition: openTask,
			call: llm.ToolCall{Name: "cortex__cortex_open_task", Arguments: map[string]any{"goal": "g"}},
		},
		{
			name: "explicit deny is never rewritten into an authorization",
			mode: AuthorityAutoScoped, denyTool: "cortex__cortex_open_task", definition: openTask,
			call: llm.ToolCall{Name: "cortex__cortex_open_task", Arguments: map[string]any{"goal": "g"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ag, workspace := newWorkspacePinAgent(t)
			if tt.clearWork {
				ag.SetWorkDir("")
			}
			if tt.denyTool != "" {
				checker := permission.NewChecker(nil, false)
				if err := checker.SetPolicy(tt.denyTool, permission.PolicyDeny); err != nil {
					t.Fatal(err)
				}
				ag.SetPermissionChecker(checker)
			}
			before := maps.Clone(tt.call.Arguments)
			snapshot := workspacePinSnapshot(tt.definition)

			pinned, ok := ag.pinCataloguedWorkspaceArgument(tt.mode, tt.call, executionpkg.KindMCP, snapshot)
			if ok != tt.wantPin {
				t.Fatalf("pinned = %v, want %v (arguments %#v)", ok, tt.wantPin, pinned.Arguments)
			}
			if !reflect.DeepEqual(tt.call.Arguments, before) {
				t.Fatalf("caller arguments mutated: %#v, want %#v", tt.call.Arguments, before)
			}
			if !tt.wantPin {
				if !reflect.DeepEqual(pinned.Arguments, tt.call.Arguments) {
					t.Fatalf("refused pin still rewrote arguments: %#v", pinned.Arguments)
				}
				return
			}
			if got := pinned.Arguments[workspaceArgumentKey]; got != workspace {
				t.Fatalf("pinned workspace = %v, want %q", got, workspace)
			}
		})
	}
}

// TestPinCataloguedWorkspaceArgumentNeverAppliesToNonMCPKinds guards the kind
// gate: built-ins authorize on their own paths and must not gain an argument.
func TestPinCataloguedWorkspaceArgumentNeverAppliesToNonMCPKinds(t *testing.T) {
	ag, _ := newWorkspacePinAgent(t)
	snapshot := workspacePinSnapshot(llm.ToolDef{
		Name: "cortex__cortex_open_task", Parameters: cortexOptionalWorkspaceSchema(),
	})
	call := llm.ToolCall{Name: "cortex__cortex_open_task", Arguments: map[string]any{"goal": "g"}}
	for _, kind := range []executionpkg.Kind{executionpkg.KindBuiltin, executionpkg.KindMemory} {
		if _, ok := ag.pinCataloguedWorkspaceArgument(AuthorityAutoScoped, call, kind, snapshot); ok {
			t.Fatalf("kind %s was rewritten", kind)
		}
	}
}

// TestModelSuppliedWorkspaceEscapeStillPrompts proves the pin did not become a
// way to launder an out-of-workspace request: a value the model chose is left
// alone and fails containment exactly as before.
func TestModelSuppliedWorkspaceEscapeStillPrompts(t *testing.T) {
	ag, _ := newWorkspacePinAgent(t)
	snapshot := workspacePinSnapshot(llm.ToolDef{
		Name: "cortex__cortex_open_task", Parameters: cortexOptionalWorkspaceSchema(),
	})
	escape := llm.ToolCall{
		Name:      "cortex__cortex_open_task",
		Arguments: map[string]any{"goal": "g", "workspace": t.TempDir()},
	}
	if _, ok := ag.pinCataloguedWorkspaceArgument(AuthorityAutoScoped, escape, executionpkg.KindMCP, snapshot); ok {
		t.Fatal("an out-of-workspace model value was replaced instead of being judged")
	}
	if ag.authorityAutoApproves(AuthorityAutoScoped, escape, executionpkg.KindMCP) {
		t.Fatal("an out-of-workspace workspace argument auto-approved")
	}
}

type workspacePinObserverHook struct {
	preArguments  map[string]any
	postArguments map[string]any
}

func (*workspacePinObserverHook) Name() string { return "workspace-pin-observer" }

func (h *workspacePinObserverHook) PreToolUse(_ context.Context, call *llm.ToolCall) (bool, string) {
	h.preArguments = maps.Clone(call.Arguments)
	return false, ""
}

func (h *workspacePinObserverHook) PostToolUse(_ context.Context, call llm.ToolCall, _ *string, _ bool) {
	h.postArguments = maps.Clone(call.Arguments)
}

type toolStartRecorder struct {
	outputRecorder
	startArguments map[string]any
}

func (r *toolStartRecorder) ToolCallStart(_ string, _ string, arguments map[string]any) {
	r.startArguments = maps.Clone(arguments)
}

// TestPinnedWorkspaceArgumentsAreTheBytesEveryBoundaryObserves pins the
// ordering contract. The rewrite must land after the hook boundary and before
// the arguments hash, so the durable ledger, the UI, and the backend all agree
// on one payload — and no approval modal opens at all.
func TestPinnedWorkspaceArgumentsAreTheBytesEveryBoundaryObserves(t *testing.T) {
	var backendMu sync.Mutex
	var backendArguments map[string]any
	downstream := sdk.NewServer(&sdk.Implementation{Name: "cortex", Version: "1"}, nil)
	downstream.AddTool(
		&sdk.Tool{Name: "cortex_open_task", InputSchema: cortexOptionalWorkspaceSchema()},
		func(_ context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			backendMu.Lock()
			defer backendMu.Unlock()
			if err := json.Unmarshal(request.Params.Arguments, &backendArguments); err != nil {
				return nil, err
			}
			return &sdk.CallToolResult{
				Content: []sdk.Content{&sdk.TextContent{Text: "opened task"}},
			}, nil
		},
	)
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return downstream }, nil)
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)

	ledger := &fakeExecutionLedger{}
	registry := mcpPkg.NewRegistry()
	t.Cleanup(registry.Close)
	if _, err := registry.ConnectServer(context.Background(), config.ServerConfig{
		Name: "cortex", Transport: "streamable-http", URL: backend.URL,
	}); err != nil {
		t.Fatal(err)
	}
	ag, workspace := newLedgerAgent(t, nil, registry, ledger)
	// Trust is keyed by namespace and, by policy, only ever resolves for a
	// local stdio owner. The in-process backend above stands in for that
	// server's transport so the dispatch path can be exercised for real.
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{Name: "cortex", Command: "cortex", Transport: "stdio"},
	})
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	prompts := 0
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts++
		request.Response <- permission.Deny()
	})
	hook := &workspacePinObserverHook{}
	ag.AddToolHook(hook)
	ui := &toolStartRecorder{}

	ctx := context.Background()
	execRuntime, err := ag.executionRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	modelArguments := map[string]any{"goal": "ship the fix"}
	call := llm.ToolCall{ID: "call_open", Name: "cortex__cortex_open_task", Arguments: modelArguments}
	tracked, err := ag.newTrackedExecutionsWithRequestDetail(
		ctx, execRuntime, "turn_pin", 1, []llm.ToolCall{call}, []string{"provider_1"}, "provider requested tool",
	)
	if err != nil {
		t.Fatal(err)
	}
	originalHash, err := executionpkg.HashCanonicalArguments(modelArguments)
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := map[string]any{"goal": "ship the fix", "workspace": workspace}
	pinnedHash, err := executionpkg.HashCanonicalArguments(wantArguments)
	if err != nil {
		t.Fatal(err)
	}
	if originalHash == pinnedHash {
		t.Fatal("test cannot distinguish pinned from unpinned arguments")
	}

	rt := &turnRuntime{
		a: ag, out: ui, turnID: "turn_pin", turnNumCtx: 4096,
		authorityMode:         AuthorityAutoScoped,
		turnToolPolicy:        BuildToolPolicy(),
		execRuntime:           execRuntime,
		turnMCPSnapshot:       registry.SnapshotTools(),
		continuationsConfig:   config.ContinuationsConfig{Mode: config.ContinuationOff},
		maxIters:              8,
		autoProgress:          newAutoTurnProgress(),
		hostRefusalCounts:     make(map[string]int),
		completedBuiltinCalls: make(map[string]struct{}),
	}
	toolCalls := []llm.ToolCall{call}
	if _, dispatchErr := rt.dispatchStage(ctx, 0, toolCalls, tracked); dispatchErr != nil {
		t.Fatal(dispatchErr)
	}

	backendMu.Lock()
	dispatched := backendArguments
	backendMu.Unlock()
	if !reflect.DeepEqual(dispatched, wantArguments) {
		t.Fatalf("backend received %#v, want %#v", dispatched, wantArguments)
	}
	if prompts != 0 {
		t.Fatalf("catalogued workspace-effectful route opened %d approval modal(s)", prompts)
	}
	if !reflect.DeepEqual(hook.preArguments, modelArguments) {
		t.Fatalf("pre-tool hook observed %#v, want the model's own %#v", hook.preArguments, modelArguments)
	}
	if !reflect.DeepEqual(hook.postArguments, wantArguments) {
		t.Fatalf("dispatched arguments = %#v, want %#v", hook.postArguments, wantArguments)
	}
	if !reflect.DeepEqual(ui.startArguments, wantArguments) {
		t.Fatalf("UI arguments = %#v, want %#v", ui.startArguments, wantArguments)
	}
	if !reflect.DeepEqual(toolCalls[0].Arguments, wantArguments) {
		t.Fatalf("transcript arguments = %#v, want %#v", toolCalls[0].Arguments, wantArguments)
	}

	events := ledger.snapshot()
	if len(events) < 2 {
		t.Fatalf("ledger recorded %d event(s)", len(events))
	}
	approvals := 0
	for _, event := range events {
		switch event.Type {
		case executionpkg.EventRequested:
			// The request event is the model's own ask, recorded before the pin.
			if event.ArgumentsSHA256 != originalHash {
				t.Fatalf("requested event hash = %q, want the model's %q", event.ArgumentsSHA256, originalHash)
			}
		case executionpkg.EventApprovalRequested:
			t.Fatal("an approval was requested for a catalogued workspace-effectful route")
		default:
			if event.ArgumentsSHA256 != pinnedHash {
				t.Fatalf("%s event hash = %q, want the pinned %q", event.Type, event.ArgumentsSHA256, pinnedHash)
			}
		}
		if event.Type == executionpkg.EventApproved {
			approvals++
			if event.Approval != executionpkg.ApprovalPolicy {
				t.Fatalf("approval reason = %q, want policy", event.Approval)
			}
		}
	}
	if approvals != 1 {
		t.Fatalf("ledger recorded %d approved event(s), want 1", approvals)
	}
}
