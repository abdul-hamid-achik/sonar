package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

func TestValidateContinuationUsesExactRegistrySchemaAndHostAuthority(t *testing.T) {
	workspace := t.TempDir()
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "cortex", Command: "cortex"}})
	definition := cortexPlanDefinition("cortex__cortex_plan")
	snapshot := mcp.ToolSnapshot{Epoch: 7, AvailableServers: []string{"cortex"}, Tools: []llm.ToolDef{definition}}
	projection := continuationCortexProjection("cortex_status", "task_1")
	candidate := continuationPlanCandidate(workspace)

	validated, ok := ag.validateContinuation(
		llm.ToolCall{Name: "cortex__cortex_status"}, projection, candidate, snapshot,
	)
	if !ok || validated == nil {
		t.Fatal("exact continuation was rejected")
	}
	if validated.Call.Name != "cortex__cortex_plan" || validated.Effect != executionpkg.Effectful ||
		!reflect.DeepEqual(validated.Inputs, []string{"hypotheses", "uncertainty"}) || validated.Fingerprint == "" {
		t.Fatalf("validated continuation = %#v", validated)
	}
	context := validated.modelContext()
	for _, expected := range []string{"not saved", "not executed", "cortex__cortex_plan", "hypotheses", "uncertainty"} {
		if !strings.Contains(context, expected) {
			t.Fatalf("model context missing %q: %s", expected, context)
		}
	}
	for _, forbidden := range []string{"command", "rm -rf"} {
		if strings.Contains(context, forbidden) {
			t.Fatalf("model context retained forbidden %q: %s", forbidden, context)
		}
	}
}

func TestContinuationRejectsUntrustedSourceEvenWhenTargetIsTrusted(t *testing.T) {
	workspace := t.TempDir()
	registry := mcp.NewRegistry()
	t.Cleanup(registry.Close)
	ag := New(nil, registry, 4096)
	ag.SetWorkDir(workspace)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{Name: "cortex", Command: "env", Args: []string{"cortex"}},
		{Name: "bob", Command: "bob"},
	})
	sourceCall := llm.ToolCall{Name: "cortex__cortex_status"}
	if ag.continuationSourceAuthorized(sourceCall, false) {
		t.Fatal("untrusted Cortex wrapper gained continuation parser authority")
	}

	projection := continuationCortexProjection("cortex_status", "task_1")
	candidate := ecosystem.ContinuationAction{
		Source: "cortex", SourceOperation: "cortex_status", Tool: "bob_context",
		Arguments: map[string]ecosystem.BoundedValue{
			"workspace": {Kind: ecosystem.BoundedString, Text: workspace},
			"profile":   {Kind: ecosystem.BoundedString, Text: "compact"},
		},
		WorkspaceRef: workspace, TaskID: "task_1", SourceRevision: 1,
	}
	snapshot := registry.SnapshotTools()
	snapshot.AvailableServers = []string{"bob"}
	snapshot.Tools = []llm.ToolDef{bobContextDefinition("bob__bob_context")}
	if validated, ok := ag.validateContinuation(sourceCall, projection, candidate, snapshot); !ok || validated == nil {
		t.Fatal("test setup failed: separately trusted Bob target did not validate")
	}
	if got := ag.selectContinuationCandidate(
		sourceCall, projection, []ecosystem.ContinuationAction{candidate}, snapshot,
		false, true, newContinuationTurnState(snapshot.Epoch), false,
	); got != nil {
		t.Fatalf("untrusted source selected a trusted target: %#v", got)
	}
}

func TestContinuationBoundAssemblyRequiresExactTrustedGetResultSource(t *testing.T) {
	ag := New(nil, nil, 4096)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub"}})
	if !ag.continuationSourceAuthorized(llm.ToolCall{
		Name: "mcphub__mcphub_get_result",
	}, true) {
		t.Fatal("exact trusted MCPHub get_result page lost continuation source authority")
	}
	if ag.continuationSourceAuthorized(llm.ToolCall{
		Name: "mcphub__mcphub_call_tool", Arguments: map[string]any{
			"server": "cortex", "tool": "cortex_status", "arguments": map[string]any{},
		},
	}, true) {
		t.Fatal("bound assembly accepted a source other than exact get_result")
	}

	wrapped := New(nil, nil, 4096)
	wrapped.SetTrustedLocalMCPServers([]config.ServerConfig{{
		Name: "mcphub", Command: "env", Args: []string{"mcphub"},
	}})
	if wrapped.continuationSourceAuthorized(llm.ToolCall{Name: "mcphub__mcphub_get_result"}, true) {
		t.Fatal("wrapped MCPHub get_result gained continuation source authority")
	}
}

func TestContinuationKeepsBlockersTransientAndRedactsThemFromPresentation(t *testing.T) {
	const blocker = "pending human decision"
	continuation := &ValidatedContinuation{
		Tool: "cortex_answer_decision", BlockedBy: []string{blocker}, ReasonCode: "source_blocked",
		Call: llm.ToolCall{Name: "cortex__cortex_answer_decision", Arguments: map[string]any{
			"taskId": "task_1", "workspace": "/repo",
		}}, Effect: executionpkg.Effectful,
	}
	if context := continuation.modelContext(); !strings.Contains(context, blocker) {
		t.Fatalf("transient model context lost explicit blocker: %s", context)
	}
	out := &continuationOutputRecorder{}
	emitContinuationSuggestion(out, "turn_1", 1, continuation)
	if out.suggestion == nil || !reflect.DeepEqual(out.suggestion.BlockedBy, []string{"source_blocked"}) {
		t.Fatalf("TUI presentation retained downstream blocker or lost host code: %#v", out.suggestion)
	}
	if encoded, _ := json.Marshal(out.suggestion); strings.Contains(string(encoded), blocker) {
		t.Fatalf("downstream blocker crossed into TUI state: %s", encoded)
	}
}

func TestContinuationSurfacePresentChecksStructuredAndTextIndependently(t *testing.T) {
	projection := continuationCortexProjection("cortex_status", "task_1")
	receipt := ecosystem.RawReceipt{
		Structured: json.RawMessage(`{"ok":true,"taskId":"task_1","phase":"planned"}`),
		Text:       `{"ok":true,"taskId":"task_1","phase":"planned","actions":[{"tool":"shell","command":"do not expose"}]}`,
	}
	if !continuationSurfacePresent(projection, receipt) {
		t.Fatal("action-bearing TextContent was hidden by clean StructuredContent")
	}
	receipt.Text = `{"ok":true,"taskId":"task_1","phase":"planned"}`
	if continuationSurfacePresent(projection, receipt) {
		t.Fatal("action-free Cortex surfaces were reported as continuations")
	}
}

func TestValidateContinuationRejectsUnknownDeniedWrongWorkspaceAndAnnotationLies(t *testing.T) {
	workspace := t.TempDir()
	base := continuationPlanCandidate(workspace)
	definition := cortexPlanDefinition("cortex__cortex_plan")
	definition.Behavior = llm.ToolBehavior{Declared: true, ReadOnly: true, Idempotent: true}
	snapshot := mcp.ToolSnapshot{Epoch: 2, AvailableServers: []string{"cortex"}, Tools: []llm.ToolDef{definition}}
	projection := continuationCortexProjection("cortex_status", "task_1")

	tests := []struct {
		name   string
		mutate func(*Agent, *ecosystem.ContinuationAction, *mcp.ToolSnapshot)
		wantOK bool
	}{
		{name: "annotation cannot downgrade host effect", wantOK: true},
		{name: "unknown tool", mutate: func(_ *Agent, action *ecosystem.ContinuationAction, _ *mcp.ToolSnapshot) {
			action.Tool = "cortex_future"
		}},
		{name: "outside workspace", mutate: func(_ *Agent, action *ecosystem.ContinuationAction, _ *mcp.ToolSnapshot) {
			action.WorkspaceRef = t.TempDir()
		}},
		{name: "wrong known argument type", mutate: func(_ *Agent, action *ecosystem.ContinuationAction, _ *mcp.ToolSnapshot) {
			action.Arguments["taskId"] = ecosystem.BoundedValue{Kind: ecosystem.BoundedNumber, Number: "1"}
		}},
		{name: "lookalike registry route", mutate: func(_ *Agent, _ *ecosystem.ContinuationAction, snapshot *mcp.ToolSnapshot) {
			snapshot.Tools[0].Name = "evil__cortex_plan"
		}},
		{name: "explicit deny", mutate: func(agent *Agent, _ *ecosystem.ContinuationAction, _ *mcp.ToolSnapshot) {
			checker := permission.NewChecker(nil, false)
			if err := checker.SetPolicy("cortex__cortex_plan", permission.PolicyDeny); err != nil {
				t.Fatal(err)
			}
			agent.SetPermissionChecker(checker)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ag := New(nil, nil, 4096)
			ag.SetWorkDir(workspace)
			ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "cortex", Command: "cortex"}})
			action := base
			action.Arguments = cloneBoundedArguments(base.Arguments)
			localSnapshot := snapshot
			localSnapshot.Tools = append([]llm.ToolDef(nil), snapshot.Tools...)
			if test.mutate != nil {
				test.mutate(ag, &action, &localSnapshot)
			}
			validated, ok := ag.validateContinuation(
				llm.ToolCall{Name: "cortex__cortex_status"}, projection, action, localSnapshot,
			)
			if ok != test.wantOK || (validated != nil) != test.wantOK {
				t.Fatalf("validated=%#v ok=%v want=%v", validated, ok, test.wantOK)
			}
			if test.wantOK && validated.Effect != executionpkg.Effectful {
				t.Fatalf("server annotation altered effect: %#v", validated)
			}
		})
	}
}

func TestValidateContinuationPreservesDirectAndGatewayRouteIdentity(t *testing.T) {
	workspace := t.TempDir()
	action := continuationPlanCandidate(workspace)
	directProjection := continuationCortexProjection("cortex_status", "task_1")
	gatewayProjection := directProjection
	gatewayProjection.Route = ecosystem.ToolRoute{
		Gateway: "mcphub", Server: "cortex", Tool: "cortex_status", Lazy: true,
	}

	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{Name: "cortex", Command: "cortex"},
		{Name: "mcphub", Command: "mcphub"},
	})

	directOnly := mcp.ToolSnapshot{Epoch: 9, AvailableServers: []string{"cortex"}, Tools: []llm.ToolDef{
		cortexPlanDefinition("cortex__cortex_plan"),
	}}
	if validated, ok := ag.validateContinuation(
		llm.ToolCall{Name: "mcphub__cortex__cortex_status"}, gatewayProjection, action, directOnly,
	); ok || validated != nil {
		t.Fatalf("gateway continuation crossed onto direct route: %#v", validated)
	}

	gatewayOnly := mcp.ToolSnapshot{Epoch: 9, AvailableServers: []string{"mcphub"}, Tools: []llm.ToolDef{
		cortexPlanDefinition("mcphub__cortex__cortex_plan"),
	}}
	if validated, ok := ag.validateContinuation(
		llm.ToolCall{Name: "cortex__cortex_status"}, directProjection, action, gatewayOnly,
	); ok || validated != nil {
		t.Fatalf("direct continuation crossed onto gateway route: %#v", validated)
	}

	if validated, ok := ag.validateContinuation(
		llm.ToolCall{Name: "mcphub__cortex__cortex_status"}, gatewayProjection, action, gatewayOnly,
	); !ok || validated == nil || validated.Call.Name != "mcphub__cortex__cortex_plan" {
		t.Fatalf("exact gateway pin was rejected: %#v, %v", validated, ok)
	}
}

func TestValidateContinuationArgumentsPreservesSchemaWhileAllowingNamedMissingInputs(t *testing.T) {
	definition := cortexPlanDefinition("cortex__cortex_plan")
	validArgs := map[string]any{"taskId": "task_1", "workspace": "/repo"}
	tests := []struct {
		name   string
		args   map[string]any
		inputs []string
		want   continuationArgumentState
	}{
		{name: "named required", args: validArgs, inputs: []string{"hypotheses", "uncertainty"}, want: continuationArgumentsNeedInput},
		{name: "named optional too", args: validArgs, inputs: []string{"hypotheses", "uncertainty", "files"}, want: continuationArgumentsNeedInput},
		{name: "missing required omitted", args: validArgs, inputs: []string{"hypotheses"}, want: continuationArgumentsInvalid},
		{name: "unknown input", args: validArgs, inputs: []string{"hypotheses", "uncertainty", "secret"}, want: continuationArgumentsInvalid},
		{name: "duplicate input", args: validArgs, inputs: []string{"hypotheses", "uncertainty", "uncertainty"}, want: continuationArgumentsInvalid},
		{name: "input already supplied", args: map[string]any{"taskId": "task_1", "workspace": "/repo", "hypotheses": []any{}}, inputs: []string{"hypotheses", "uncertainty"}, want: continuationArgumentsInvalid},
		{name: "wrong known type", args: map[string]any{"taskId": 7, "workspace": "/repo"}, inputs: []string{"hypotheses", "uncertainty"}, want: continuationArgumentsInvalid},
		{name: "additional property", args: map[string]any{"taskId": "task_1", "workspace": "/repo", "private": "do not echo"}, inputs: []string{"hypotheses", "uncertainty"}, want: continuationArgumentsInvalid},
		{name: "ready", args: map[string]any{
			"taskId": "task_1", "workspace": "/repo", "uncertainty": "bounded",
			"hypotheses": []any{map[string]any{"statement": "x", "disproveBy": "y"}},
		}, want: continuationArgumentsReady},
	}
	before, _ := json.Marshal(definition.Parameters)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateContinuationArguments(definition, test.args, test.inputs)
			if got != test.want {
				t.Fatalf("state=%v error=%v want=%v", got, err, test.want)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatalf("validation leaked argument value: %v", err)
			}
		})
	}
	after, _ := json.Marshal(definition.Parameters)
	if string(before) != string(after) {
		t.Fatal("partial validation mutated the cached definition")
	}
}

func TestContinuationTurnStateSuppressesRepeatsStaleAndImmutableConflicts(t *testing.T) {
	state := newContinuationTurnState(3)
	base := &ValidatedContinuation{
		Source: "cortex", SourceOperation: "cortex_status", SourceTask: "task_1",
		SourceRevision: 7, Fingerprint: "first",
	}
	if !state.accept(base) || state.accept(base) {
		t.Fatal("repeat fingerprint was not suppressed")
	}
	stale := *base
	stale.SourceRevision = 6
	stale.Fingerprint = "stale"
	if state.accept(&stale) {
		t.Fatal("stale revision was accepted")
	}
	conflict := *base
	conflict.Fingerprint = "different-at-same-revision"
	if state.accept(&conflict) {
		t.Fatal("immutable same-revision conflict was accepted")
	}
	newer := *base
	newer.SourceRevision = 8
	newer.Fingerprint = "newer"
	if !state.accept(&newer) {
		t.Fatal("newer revision was rejected")
	}

	staleOtherOperation := newer
	staleOtherOperation.SourceOperation = "cortex_open_task"
	staleOtherOperation.SourceRevision = 7
	staleOtherOperation.Fingerprint = "stale-from-other-operation"
	if state.accept(&staleOtherOperation) {
		t.Fatal("stale task revision was accepted through a different Cortex operation")
	}
	conflictOtherOperation := newer
	conflictOtherOperation.SourceOperation = "cortex_handoff"
	conflictOtherOperation.Fingerprint = "same-revision-conflict-from-other-operation"
	if state.accept(&conflictOtherOperation) {
		t.Fatal("immutable task revision conflict was accepted through a different Cortex operation")
	}
}

func TestContinuationHistoryRetiresBobDigestsAndSpansTurns(t *testing.T) {
	state := newContinuationTurnState(1)
	first := &ValidatedContinuation{
		Source: "bob", SourceOperation: "bob_context", WorkspaceRef: "/repo",
		ContextDigest: "sha256:first", Fingerprint: "first",
	}
	second := *first
	second.ContextDigest = "sha256:second"
	second.Fingerprint = "second"
	if !state.accept(first) || !state.accept(&second) {
		t.Fatal("new Bob context digest was not accepted")
	}
	replayed := *first
	replayed.Fingerprint = "first-with-different-schema"
	if state.accept(&replayed) {
		t.Fatal("retired Bob context digest was accepted after a newer digest")
	}

	ag := New(nil, nil, 4096)
	candidate := &ValidatedContinuation{
		Source: "cortex", SourceOperation: "cortex_status", SourceTask: "task_1",
		WorkspaceRef: "/repo", SourceRevision: 3, Fingerprint: "same-next-step",
	}
	if !ag.acceptContinuationHistory(candidate) || ag.acceptContinuationHistory(candidate) {
		t.Fatal("agent-level continuation history did not suppress a cross-turn repeat")
	}
	ag.ClearHistory()
	if !ag.acceptContinuationHistory(candidate) {
		t.Fatal("new conversation retained the prior continuation history")
	}
}

func TestContinuationHistoryResetsAtConversationSessionAndWorkspaceBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		reset func(*Agent)
	}{
		{name: "replace conversation", reset: func(agent *Agent) { agent.ReplaceMessages(nil) }},
		{name: "change durable session", reset: func(agent *Agent) { agent.SetExecutionSessionID(42, "") }},
		{name: "change workspace", reset: func(agent *Agent) { agent.SetWorkspacePolicy("/other-repo", "") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ag := New(nil, nil, 4096)
			action := &ValidatedContinuation{
				Source: "cortex", SourceOperation: "cortex_status", SourceTask: "task_1",
				WorkspaceRef: "/repo", SourceRevision: 3, Fingerprint: "same-next-step",
			}
			if !ag.acceptContinuationHistory(action) || ag.acceptContinuationHistory(action) {
				t.Fatal("test setup did not establish a suppressed continuation")
			}
			test.reset(ag)
			if !ag.acceptContinuationHistory(action) {
				t.Fatal("new scope inherited the prior continuation suppression")
			}
		})
	}
}

func TestContinuationContextFailedValidationDoesNotBurnAndSuccessfulUseIsOneShot(t *testing.T) {
	ag := New(nil, nil, 4096)
	action := ValidatedContinuation{
		Source: "cortex", SourceOperation: "cortex_status", SourceTask: "task_1",
		WorkspaceRef: "/repo", SourceRevision: 7, Fingerprint: "opaque-context",
	}
	failed := &ContinuationContext{owner: ag, registryEpoch: 1, continuation: action}
	if got := consumeContinuationContext(ag, failed, func(*ValidatedContinuation) string { return "" }); got != "" {
		t.Fatalf("failed validation returned model context %q", got)
	}
	ag.mu.RLock()
	_, burned := ag.continuationHistory.seenSet[action.Fingerprint]
	ag.mu.RUnlock()
	if burned || failed.consumed {
		t.Fatalf("failed validation burned context: history=%v consumed=%v", burned, failed.consumed)
	}

	retry := &ContinuationContext{owner: ag, registryEpoch: 1, continuation: action}
	if got := consumeContinuationContext(ag, retry, func(*ValidatedContinuation) string { return "validated" }); got != "validated" {
		t.Fatalf("retry after failed validation = %q", got)
	}
	if !retry.consumed {
		t.Fatal("successful context was not marked consumed")
	}
	if got := consumeContinuationContext(ag, retry, func(*ValidatedContinuation) string { return "replayed" }); got != "" {
		t.Fatalf("same opaque context replayed as %q", got)
	}
	duplicate := &ContinuationContext{owner: ag, registryEpoch: 1, continuation: action}
	if got := consumeContinuationContext(ag, duplicate, func(*ValidatedContinuation) string { return "duplicate" }); got != "" {
		t.Fatalf("duplicate fingerprint replayed through a second context as %q", got)
	}
}

func TestContinuationContextConsumptionIsAtomic(t *testing.T) {
	ag := New(nil, nil, 4096)
	context := &ContinuationContext{owner: ag, registryEpoch: 1, continuation: ValidatedContinuation{
		Source: "cortex", SourceOperation: "cortex_status", SourceTask: "task_1",
		WorkspaceRef: "/repo", SourceRevision: 8, Fingerprint: "atomic-context",
	}}
	const consumers = 16
	start := make(chan struct{})
	var admitted atomic.Int32
	var wait sync.WaitGroup
	wait.Add(consumers)
	for range consumers {
		go func() {
			defer wait.Done()
			<-start
			if consumeContinuationContext(ag, context, func(*ValidatedContinuation) string { return "validated" }) != "" {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if got := admitted.Load(); got != 1 {
		t.Fatalf("concurrent opaque context admissions = %d, want 1", got)
	}
}

func TestRememberContinuationContractIsExactGatewayScopedAndLazyOnly(t *testing.T) {
	registry := mcp.NewRegistry()
	t.Cleanup(registry.Close)
	ag := New(nil, registry, 4096)
	workspace := t.TempDir()
	ag.SetWorkDir(workspace)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub"}})
	schema := bobContextDefinition("bob__bob_context").Parameters
	schemaJSON, _ := json.Marshal(schema)
	structured := json.RawMessage(`{"server":"bob","tool":"bob_context","namespaced":"bob__bob_context","description":"read context","input_schema":` + string(schemaJSON) + `}`)
	projection := ecosystem.ToolProjection{
		Specialist: "mcphub", Operation: "mcphub_describe_tool", Role: ecosystem.RoleGateway,
		Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainSucceeded, DomainTyped: true,
		Route:  ecosystem.ToolRoute{Server: "mcphub", Tool: "mcphub_describe_tool"},
		Digest: &ecosystem.ReceiptDigest{Kind: ecosystem.DigestMCPHubDescribe, Target: "bob__bob_context"},
	}
	describeCall := llm.ToolCall{Name: "mcphub__mcphub_describe_tool", Arguments: map[string]any{"server": "bob", "tool": "bob_context"}}
	epoch := registry.SnapshotTools().Epoch
	sourceSnapshot := mcp.ToolSnapshot{Epoch: epoch, AvailableServers: []string{"mcphub"}}
	if !ag.rememberContinuationContract(describeCall, projection, structured, sourceSnapshot) {
		t.Fatal("exact describe contract was not cached")
	}
	if _, _, ok := ag.continuationContract(continuationContractKey{Gateway: "other", Server: "bob", Tool: "bob_context"}, epoch); ok {
		t.Fatal("contract crossed gateway namespaces")
	}
	if _, _, ok := ag.continuationContract(continuationContractKey{Gateway: "mcphub", Server: "bob", Tool: "bob_context"}, epoch+1); ok {
		t.Fatal("contract survived registry epoch change")
	}

	generic := llm.ToolDef{Name: "mcphub__mcphub_call_tool", Parameters: map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"server": map[string]any{"type": "string"}, "tool": map[string]any{"type": "string"},
			"arguments": map[string]any{"type": "object"},
		}, "required": []any{"server", "tool", "arguments"},
	}}
	projection = continuationCortexProjection("cortex_status", "task_1")
	projection.Route = ecosystem.ToolRoute{Gateway: "mcphub", Server: "cortex", Tool: "cortex_status", Lazy: true}
	action := ecosystem.ContinuationAction{
		Source: "cortex", SourceOperation: "cortex_status", Tool: "bob_context",
		Arguments: map[string]ecosystem.BoundedValue{
			"workspace": {Kind: ecosystem.BoundedString, Text: workspace},
			"profile":   {Kind: ecosystem.BoundedString, Text: "compact"},
		}, WorkspaceRef: workspace, TaskID: "task_1", SourceRevision: 7,
	}
	snapshot := mcp.ToolSnapshot{Epoch: epoch, AvailableServers: []string{"mcphub"}, Tools: []llm.ToolDef{generic}}
	validated, ok := ag.validateContinuation(
		llm.ToolCall{Name: "mcphub__mcphub_call_tool"}, projection, action, snapshot,
	)
	if !ok || validated.Call.Name != "mcphub__mcphub_call_tool" || validated.Tool != "bob_context" {
		t.Fatalf("lazy validation = %#v, %v", validated, ok)
	}
	if !continuationSchemaStillCurrent(ag, validated, snapshot) {
		t.Fatal("valid lazy continuation was compared against the generic gateway wrapper schema")
	}
	delete(ag.continuationContracts, continuationContractKey{Gateway: "mcphub", Server: "bob", Tool: "bob_context"}.String())
	if continuationSchemaStillCurrent(ag, validated, snapshot) {
		t.Fatal("lazy continuation survived removal of its exact cached downstream schema")
	}
	if validated, ok = ag.validateContinuation(llm.ToolCall{Name: "mcphub__mcphub_call_tool"}, projection, action, snapshot); ok || validated != nil {
		t.Fatalf("lazy action passed without exact cached schema: %#v", validated)
	}
}

func TestRememberContinuationContractRejectsStaleRegistryEpoch(t *testing.T) {
	registry := mcp.NewRegistry()
	t.Cleanup(registry.Close)
	ag := New(nil, registry, 4096)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub"}})
	schemaJSON, _ := json.Marshal(bobContextDefinition("bob__bob_context").Parameters)
	structured := json.RawMessage(`{"server":"bob","tool":"bob_context","namespaced":"bob__bob_context","input_schema":` + string(schemaJSON) + `}`)
	projection := ecosystem.ToolProjection{
		Specialist: "mcphub", Operation: "mcphub_describe_tool", Role: ecosystem.RoleGateway,
		Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainSucceeded, DomainTyped: true,
		Route:  ecosystem.ToolRoute{Server: "mcphub", Tool: "mcphub_describe_tool"},
		Digest: &ecosystem.ReceiptDigest{Kind: ecosystem.DigestMCPHubDescribe, Target: "bob__bob_context"},
	}
	call := llm.ToolCall{Name: "mcphub__mcphub_describe_tool", Arguments: map[string]any{"server": "bob", "tool": "bob_context"}}
	if ag.rememberContinuationContract(call, projection, structured, mcp.ToolSnapshot{
		Epoch: registry.SnapshotTools().Epoch + 1, AvailableServers: []string{"mcphub"},
	}) {
		t.Fatal("describe response was cached under an epoch it did not originate from")
	}
}

func continuationCortexProjection(operation, taskID string) ecosystem.ToolProjection {
	return ecosystem.ToolProjection{
		Specialist: "cortex", Operation: operation, Role: ecosystem.RoleCoordination,
		Transport: ecosystem.TransportSucceeded, Domain: ecosystem.DomainSucceeded, DomainTyped: true,
		Route: ecosystem.ToolRoute{Server: "cortex", Tool: operation}, Evidence: ecosystem.EvidenceNone,
		Digest: &ecosystem.ReceiptDigest{Kind: ecosystem.DigestCortexReceipt, Target: taskID},
	}
}

func continuationPlanCandidate(workspace string) ecosystem.ContinuationAction {
	return ecosystem.ContinuationAction{
		Source: "cortex", SourceOperation: "cortex_status", Tool: "cortex_plan",
		Arguments: map[string]ecosystem.BoundedValue{
			"taskId":    {Kind: ecosystem.BoundedString, Text: "task_1"},
			"workspace": {Kind: ecosystem.BoundedString, Text: workspace},
		}, Inputs: []string{"hypotheses", "uncertainty"}, WorkspaceRef: workspace,
		TaskID: "task_1", SourceRevision: 7,
	}
}

func cortexPlanDefinition(name string) llm.ToolDef {
	return llm.ToolDef{Name: name, Parameters: map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"taskId": map[string]any{"type": "string"}, "workspace": map[string]any{"type": "string"},
			"uncertainty": map[string]any{"type": "string"}, "files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"hypotheses": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"statement": map[string]any{"type": "string"}, "disproveBy": map[string]any{"type": "string"}},
				"required":   []any{"statement", "disproveBy"},
			}},
		},
		"required": []any{"taskId", "workspace", "hypotheses", "uncertainty"},
	}}
}

func bobContextDefinition(name string) llm.ToolDef {
	return llm.ToolDef{Name: name, Parameters: map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"workspace": map[string]any{"type": "string"},
			"profile":   map[string]any{"type": "string", "enum": []any{"compact", "standard", "full"}},
		}, "required": []any{"workspace", "profile"},
	}}
}

func cloneBoundedArguments(values map[string]ecosystem.BoundedValue) map[string]ecosystem.BoundedValue {
	clone := make(map[string]ecosystem.BoundedValue, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

type continuationOutputRecorder struct {
	suggestion *ContinuationSuggestion
}

func (*continuationOutputRecorder) StreamText(string)                                          {}
func (*continuationOutputRecorder) StreamReasoning(string)                                     {}
func (*continuationOutputRecorder) StreamDone(int, int)                                        {}
func (*continuationOutputRecorder) ToolCallStart(string, string, map[string]any)               {}
func (*continuationOutputRecorder) ToolCallResult(string, string, string, bool, time.Duration) {}
func (*continuationOutputRecorder) SystemMessage(string)                                       {}
func (*continuationOutputRecorder) Error(string)                                               {}
func (r *continuationOutputRecorder) ContinuationSuggestion(_ string, _ uint64, suggestion *ContinuationSuggestion) {
	r.suggestion = suggestion
}
