package agent

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

type autoContinuationFixture struct {
	ag                 *Agent
	registry           *mcp.Registry
	workspace          string
	sourceCall         llm.ToolCall
	projection         ecosystem.ToolProjection
	candidate          ecosystem.ContinuationAction
	snapshot           mcp.ToolSnapshot
	sourceAuthorized   bool
	sourceRouteVersion uint64
	authorityMode      AuthorityMode
	state              *autoContinuationState
}

func newAutoContinuationFixture(t *testing.T) *autoContinuationFixture {
	t.Helper()
	registry := mcp.NewRegistry()
	t.Cleanup(registry.Close)
	ag := New(nil, registry, 4096)
	workspace := t.TempDir()
	ag.SetWorkDir(workspace)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{Name: "cortex", Command: "cortex", Transport: "stdio"},
		{Name: "bob", Command: "bob", Transport: "stdio"},
	})
	definition := bobContextDefinition("bob__bob_context")
	definition.Behavior = eligibleAutoBehavior()
	epoch := registry.SnapshotTools().Epoch
	sourceCall := llm.ToolCall{Name: "cortex__cortex_status"}
	return &autoContinuationFixture{
		ag: ag, registry: registry, workspace: workspace, sourceCall: sourceCall,
		projection: continuationCortexProjection("cortex_status", "task_auto"),
		candidate: ecosystem.ContinuationAction{
			Source: "cortex", SourceOperation: "cortex_status", Tool: "bob_context",
			Arguments: map[string]ecosystem.BoundedValue{
				"workspace": {Kind: ecosystem.BoundedString, Text: workspace},
				"profile":   {Kind: ecosystem.BoundedString, Text: "compact"},
			},
			WorkspaceRef: workspace, TaskID: "task_auto", SourceRevision: 7,
		},
		snapshot: mcp.ToolSnapshot{
			Epoch: epoch, AvailableServers: []string{"bob"}, Tools: []llm.ToolDef{definition},
		},
		sourceAuthorized:   ag.continuationSourceAuthorized(sourceCall, false),
		sourceRouteVersion: ag.mcpRouteVersionSnapshot(),
		authorityMode:      AuthorityAutoScoped,
		state:              newAutoContinuationState(epoch, hardMaxAutoContinuationSteps),
	}
}

func eligibleAutoBehavior() llm.ToolBehavior {
	return llm.ToolBehavior{Declared: true, ReadOnly: true, Idempotent: true}
}

func (fixture *autoContinuationFixture) selectCandidate(candidates ...ecosystem.ContinuationAction) *preparedAutoContinuation {
	if candidates == nil {
		candidates = []ecosystem.ContinuationAction{fixture.candidate}
	}
	return fixture.ag.selectAutoReadOnlyContinuation(
		fixture.sourceCall, fixture.projection, candidates, fixture.snapshot,
		fixture.sourceAuthorized, fixture.sourceRouteVersion, fixture.authorityMode, fixture.state,
	)
}

func (fixture *autoContinuationFixture) validated(t *testing.T) *ValidatedContinuation {
	t.Helper()
	validated, ok := fixture.ag.validateContinuation(
		fixture.sourceCall, fixture.projection, fixture.candidate, fixture.snapshot,
	)
	if !ok || validated == nil {
		t.Fatal("auto continuation fixture did not validate")
	}
	return validated
}

func (fixture *autoContinuationFixture) opaqueContext(t *testing.T, validated *ValidatedContinuation) *ContinuationContext {
	t.Helper()
	sequence, ok := fixture.ag.observeContinuationFreshness(validated)
	if !ok {
		t.Fatal("auto continuation fixture did not establish source freshness")
	}
	return &ContinuationContext{
		owner: fixture.ag, registryEpoch: fixture.snapshot.Epoch,
		sourceRouteVersion: fixture.ag.mcpRouteVersionSnapshot(), issueSequence: sequence,
		sourceCall: cloneContinuationToolCall(fixture.sourceCall), continuation: *validated,
	}
}

func TestSelectAutoReadOnlyContinuationRequiresCompleteExactReadContract(t *testing.T) {
	fixture := newAutoContinuationFixture(t)
	prepared := fixture.selectCandidate()
	if prepared == nil {
		t.Fatal("exact closed-world idempotent read was not prepared")
		return
	}
	if prepared.continuation.SourceDomain != ecosystem.DomainSucceeded ||
		prepared.continuation.Effect != executionpkg.EffectReadOnly ||
		prepared.continuation.BehaviorDigest == "" || prepared.continuation.SchemaDigest == "" {
		t.Fatalf("prepared continuation lost exact state: %#v", prepared.continuation)
	}
	first := prepared.detachedCall()
	if first.Name != "bob__bob_context" || first.Arguments["workspace"] != fixture.workspace {
		t.Fatalf("detached auto call = %#v", first)
	}
	first.Arguments["workspace"] = "mutated"
	if second := prepared.detachedCall(); second.Arguments["workspace"] != fixture.workspace {
		t.Fatalf("queued call arguments alias caller mutation: %#v", second)
	}
	steps, fingerprints := fixture.ag.autoContinuationHistorySnapshot()
	if steps != 1 || len(fingerprints) != 1 || fingerprints[0] != prepared.continuation.Fingerprint {
		t.Fatalf("auto reservation history = %d %#v", steps, fingerprints)
	}
}

func TestSelectAutoReadOnlyContinuationFailsClosedAcrossEligibilityInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *autoContinuationFixture)
	}{
		{name: "source unauthorized", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.sourceAuthorized = false }},
		{name: "source transport failed", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.projection.Transport = ecosystem.TransportFailed }},
		{name: "source domain attention", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.projection.Domain = ecosystem.DomainAttention }},
		{name: "source domain untyped", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.projection.DomainTyped = false }},
		{name: "wrong source role", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.projection.Role = ecosystem.RoleGeneral }},
		{name: "missing candidate", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.candidate = ecosystem.ContinuationAction{} }},
		{name: "missing input", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.candidate.Inputs = []string{"profile"} }},
		{name: "source blocker", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.candidate.BlockedBy = []string{"blocked"} }},
		{name: "normal authority", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.authorityMode = AuthorityNormal }},
		{name: "disabled budget", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.state.reset(f.snapshot.Epoch, 0) }},
		{name: "stale registry epoch", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.snapshot.Epoch++ }},
		{name: "server unavailable", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.snapshot.AvailableServers = nil }},
		{name: "duplicate registry definition", mutate: func(_ *testing.T, f *autoContinuationFixture) {
			f.snapshot.Tools = append(f.snapshot.Tools, f.snapshot.Tools[0])
		}},
		{name: "workspace mismatch", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.candidate.WorkspaceRef = t.TempDir() }},
		{name: "scope denied", mutate: func(_ *testing.T, f *autoContinuationFixture) { f.ag.SetMCPServerScope([]string{"cortex"}) }},
		{name: "permission denied", mutate: func(t *testing.T, f *autoContinuationFixture) {
			checker := permission.NewChecker(nil, false)
			if err := checker.SetPolicy("bob__bob_context", permission.PolicyDeny); err != nil {
				t.Fatal(err)
			}
			f.ag.SetPermissionChecker(checker)
		}},
		{name: "effectful host contract", mutate: func(_ *testing.T, f *autoContinuationFixture) {
			f.ag.mu.Lock()
			server := f.ag.trustedMCP["bob"]
			server.contracts["bob_context"] = mcpAuthorityContract{effect: executionpkg.Effectful, auto: true, workspaceScoped: true}
			f.ag.trustedMCP["bob"] = server
			f.ag.mu.Unlock()
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAutoContinuationFixture(t)
			test.mutate(t, fixture)
			if prepared := fixture.selectCandidate(); prepared != nil {
				t.Fatalf("ineligible continuation prepared: %#v", prepared.continuation)
			}
			if steps, _ := fixture.ag.autoContinuationHistorySnapshot(); steps != 0 {
				t.Fatalf("failed eligibility reserved %d global actions", steps)
			}
		})
	}

	t.Run("multiple candidates", func(t *testing.T) {
		fixture := newAutoContinuationFixture(t)
		if prepared := fixture.selectCandidate(fixture.candidate, fixture.candidate); prepared != nil {
			t.Fatal("one valid action was selected from an ambiguous candidate set")
		}
	})
}

func TestSelectAutoReadOnlyContinuationRequiresEveryBehaviorAssertion(t *testing.T) {
	tests := []struct {
		name     string
		behavior llm.ToolBehavior
	}{
		{name: "undeclared", behavior: llm.ToolBehavior{ReadOnly: true, Idempotent: true}},
		{name: "not read only", behavior: llm.ToolBehavior{Declared: true, Idempotent: true}},
		{name: "destructive", behavior: llm.ToolBehavior{Declared: true, ReadOnly: true, Destructive: true, Idempotent: true}},
		{name: "not idempotent", behavior: llm.ToolBehavior{Declared: true, ReadOnly: true}},
		{name: "open world", behavior: llm.ToolBehavior{Declared: true, ReadOnly: true, Idempotent: true, OpenWorld: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAutoContinuationFixture(t)
			fixture.snapshot.Tools[0].Behavior = test.behavior
			if prepared := fixture.selectCandidate(); prepared != nil {
				t.Fatalf("unsafe metadata prepared: %#v", test.behavior)
			}
		})
	}
}

func TestAutoContinuationBehaviorDigestBindsExactMetadata(t *testing.T) {
	base := llm.ToolDef{Name: "bob__bob_context", Behavior: eligibleAutoBehavior()}
	digest := autoContinuationBehaviorDigest(base)
	if digest == "" || digest != autoContinuationBehaviorDigest(base) {
		t.Fatal("behavior digest is empty or unstable")
	}
	mutations := []llm.ToolDef{
		{Name: "other__bob_context", Behavior: base.Behavior},
		{Name: base.Name, Behavior: llm.ToolBehavior{ReadOnly: true, Idempotent: true}},
		{Name: base.Name, Behavior: llm.ToolBehavior{Declared: true, Idempotent: true}},
		{Name: base.Name, Behavior: llm.ToolBehavior{Declared: true, ReadOnly: true, Destructive: true, Idempotent: true}},
		{Name: base.Name, Behavior: llm.ToolBehavior{Declared: true, ReadOnly: true}},
		{Name: base.Name, Behavior: llm.ToolBehavior{Declared: true, ReadOnly: true, Idempotent: true, OpenWorld: true}},
	}
	for _, mutation := range mutations {
		if got := autoContinuationBehaviorDigest(mutation); got == digest {
			t.Fatalf("behavior mutation retained digest: %#v", mutation)
		}
	}
}

func TestAutoContinuationAcceptsExactPinnedGatewayAndRejectsGenericProxy(t *testing.T) {
	t.Run("pinned gateway", func(t *testing.T) {
		fixture := newAutoContinuationFixture(t)
		fixture.ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub", Transport: "stdio"}})
		fixture.sourceCall = llm.ToolCall{Name: "mcphub__cortex__cortex_status"}
		fixture.projection.Route = ecosystem.ToolRoute{Gateway: "mcphub", Server: "cortex", Tool: "cortex_status", Lazy: true}
		definition := bobContextDefinition("mcphub__bob__bob_context")
		definition.Behavior = eligibleAutoBehavior()
		fixture.snapshot = mcp.ToolSnapshot{Epoch: fixture.registry.SnapshotTools().Epoch, AvailableServers: []string{"mcphub"}, Tools: []llm.ToolDef{definition}}
		fixture.state.reset(fixture.snapshot.Epoch, 2)
		fixture.sourceAuthorized = fixture.ag.continuationSourceAuthorized(fixture.sourceCall, false)
		fixture.sourceRouteVersion = fixture.ag.mcpRouteVersionSnapshot()
		prepared := fixture.selectCandidate()
		if prepared == nil || prepared.detachedCall().Name != "mcphub__bob__bob_context" {
			t.Fatalf("exact pinned gateway continuation = %#v", prepared)
		}
	})

	t.Run("generic call tool", func(t *testing.T) {
		fixture := newAutoContinuationFixture(t)
		fixture.ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub", Transport: "stdio"}})
		fixture.sourceCall = llm.ToolCall{Name: "mcphub__mcphub_call_tool", Arguments: map[string]any{
			"server": "cortex", "tool": "cortex_status", "arguments": map[string]any{},
		}}
		fixture.projection.Route = ecosystem.ToolRoute{Gateway: "mcphub", Server: "cortex", Tool: "cortex_status", Lazy: true}
		definition := bobContextDefinition("bob__bob_context")
		definition.Behavior = eligibleAutoBehavior()
		encoded, _ := json.Marshal(definition.Parameters)
		digest := sha256.Sum256(encoded)
		fixture.ag.mu.Lock()
		fixture.ag.continuationContracts[continuationContractKey{Gateway: "mcphub", Server: "bob", Tool: "bob_context"}.String()] = continuationContract{
			definition: definition, schemaDigest: digest, registryEpoch: fixture.registry.SnapshotTools().Epoch,
		}
		fixture.ag.mu.Unlock()
		generic := llm.ToolDef{Name: "mcphub__mcphub_call_tool", Parameters: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"server": map[string]any{"type": "string"}, "tool": map[string]any{"type": "string"},
				"arguments": map[string]any{"type": "object"},
			},
			"required": []any{"server", "tool", "arguments"},
		}, Behavior: eligibleAutoBehavior()}
		fixture.snapshot = mcp.ToolSnapshot{Epoch: fixture.registry.SnapshotTools().Epoch, AvailableServers: []string{"mcphub"}, Tools: []llm.ToolDef{generic}}
		fixture.state.reset(fixture.snapshot.Epoch, 2)
		fixture.sourceAuthorized = fixture.ag.continuationSourceAuthorized(fixture.sourceCall, false)
		fixture.sourceRouteVersion = fixture.ag.mcpRouteVersionSnapshot()
		validated, ok := fixture.ag.validateContinuation(fixture.sourceCall, fixture.projection, fixture.candidate, fixture.snapshot)
		if !ok || validated.Call.Name != "mcphub__mcphub_call_tool" {
			t.Fatalf("generic proxy fixture did not validate before LA-3 guard: %#v %v", validated, ok)
		}
		if prepared := fixture.selectCandidate(); prepared != nil {
			t.Fatalf("generic gateway proxy gained auto authority: %#v", prepared.continuation)
		}
	})
}

func TestOpaqueAutoClaimLeavesContextUnconsumedUntilAtomicSuccess(t *testing.T) {
	fixture := newAutoContinuationFixture(t)
	validated := fixture.validated(t)
	context := fixture.opaqueContext(t, validated)
	// The UI installs a turn-local renderer after GoalAdvisor issues an opaque
	// context. Renderer churn is not MCP source-route churn and must not stale it.
	fixture.ag.SetApprovalCallback(func(permission.ApprovalRequest) {})

	if got := fixture.ag.claimAutoReadOnlyContinuationContextWithSnapshot(context, fixture.snapshot, AuthorityAutoScoped, nil); got != nil {
		t.Fatal("disabled config unexpectedly claimed an opaque context")
	}
	if context.consumed {
		t.Fatal("disabled config consumed the opaque context")
	}
	if got := fixture.ag.claimAutoReadOnlyContinuationContextWithSnapshot(context, fixture.snapshot, AuthorityNormal, fixture.state); got != nil {
		t.Fatal("normal authority unexpectedly claimed an opaque context")
	}
	if context.consumed || fixture.state.steps != 0 {
		t.Fatalf("failed authority claim changed state: consumed=%v steps=%d", context.consumed, fixture.state.steps)
	}

	prepared := fixture.ag.claimAutoReadOnlyContinuationContextWithSnapshot(context, fixture.snapshot, AuthorityAutoScoped, fixture.state)
	if prepared == nil || !context.consumed || fixture.state.steps != 1 {
		t.Fatalf("successful opaque claim = %#v consumed=%v steps=%d", prepared, context.consumed, fixture.state.steps)
	}
	if replay := fixture.ag.claimAutoReadOnlyContinuationContextWithSnapshot(context, fixture.snapshot, AuthorityAutoScoped, fixture.state); replay != nil {
		t.Fatal("consumed opaque context replayed")
	}
}

func TestOpaqueAutoClaimRevalidatesSourceSchemaBehaviorAndHostState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *autoContinuationFixture, *ValidatedContinuation)
	}{
		{name: "source domain changed", mutate: func(_ *testing.T, _ *autoContinuationFixture, c *ValidatedContinuation) {
			c.SourceDomain = ecosystem.DomainAttention
		}},
		{name: "schema changed", mutate: func(_ *testing.T, f *autoContinuationFixture, _ *ValidatedContinuation) {
			f.snapshot.Tools[0].Parameters["properties"].(map[string]any)["new"] = map[string]any{"type": "string"}
		}},
		{name: "behavior changed", mutate: func(_ *testing.T, f *autoContinuationFixture, _ *ValidatedContinuation) {
			f.snapshot.Tools[0].Behavior.OpenWorld = true
		}},
		{name: "registry epoch changed", mutate: func(_ *testing.T, f *autoContinuationFixture, _ *ValidatedContinuation) { f.snapshot.Epoch++ }},
		{name: "scope changed", mutate: func(_ *testing.T, f *autoContinuationFixture, _ *ValidatedContinuation) {
			f.ag.SetMCPServerScope([]string{"cortex"})
		}},
		{name: "source-only scope changed", mutate: func(_ *testing.T, f *autoContinuationFixture, _ *ValidatedContinuation) {
			f.ag.SetMCPServerScope([]string{"bob"})
		}},
		{name: "explicit deny added", mutate: func(t *testing.T, f *autoContinuationFixture, _ *ValidatedContinuation) {
			checker := permission.NewChecker(nil, false)
			if err := checker.SetPolicy("bob__bob_context", permission.PolicyDeny); err != nil {
				t.Fatal(err)
			}
			f.ag.SetPermissionChecker(checker)
		}},
		{name: "source explicit deny added", mutate: func(t *testing.T, f *autoContinuationFixture, _ *ValidatedContinuation) {
			checker := permission.NewChecker(nil, false)
			if err := checker.SetPolicy("cortex__cortex_status", permission.PolicyDeny); err != nil {
				t.Fatal(err)
			}
			f.ag.SetPermissionChecker(checker)
		}},
		{name: "workspace changed", mutate: func(t *testing.T, f *autoContinuationFixture, _ *ValidatedContinuation) { f.ag.SetWorkDir(t.TempDir()) }},
		{name: "trust changed", mutate: func(_ *testing.T, f *autoContinuationFixture, _ *ValidatedContinuation) {
			f.ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "cortex", Command: "cortex", Transport: "stdio"}})
		}},
		{name: "source-only trust changed", mutate: func(_ *testing.T, f *autoContinuationFixture, _ *ValidatedContinuation) {
			f.ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "bob", Command: "bob", Transport: "stdio"}})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAutoContinuationFixture(t)
			validated := fixture.validated(t)
			context := fixture.opaqueContext(t, validated)
			test.mutate(t, fixture, &context.continuation)
			if got := fixture.ag.claimAutoReadOnlyContinuationContextWithSnapshot(context, fixture.snapshot, AuthorityAutoScoped, fixture.state); got != nil {
				t.Fatalf("stale opaque context claimed: %#v", got.continuation)
			}
			if context.consumed || fixture.state.steps != 0 {
				t.Fatalf("failed current-state validation burned context: consumed=%v steps=%d", context.consumed, fixture.state.steps)
			}
		})
	}
}

func TestOpaqueAutoClaimRejectsOlderContextAfterNewerIssue(t *testing.T) {
	fixture := newAutoContinuationFixture(t)
	older := fixture.validated(t)
	olderContext := fixture.opaqueContext(t, older)

	fixture.candidate.SourceRevision++
	newer := fixture.validated(t)
	newerContext := fixture.opaqueContext(t, newer)

	if got := fixture.ag.claimAutoReadOnlyContinuationContextWithSnapshot(
		olderContext, fixture.snapshot, AuthorityAutoScoped, fixture.state,
	); got != nil {
		t.Fatalf("older issued Cortex revision claimed after newer observation: %#v", got.continuation)
	}
	if olderContext.consumed || fixture.state.steps != 0 {
		t.Fatalf("stale claim changed state: consumed=%v steps=%d", olderContext.consumed, fixture.state.steps)
	}
	if got := fixture.ag.claimAutoReadOnlyContinuationContextWithSnapshot(
		newerContext, fixture.snapshot, AuthorityAutoScoped, fixture.state,
	); got == nil {
		t.Fatal("newest issued Cortex revision was not claimable")
	}
}

func TestContinuationFreshnessRetiresOlderBobDigest(t *testing.T) {
	ag := New(nil, nil, 4096)
	older := &ValidatedContinuation{
		Source: "bob", SourceOperation: "bob_context", WorkspaceRef: "/repo",
		ContextDigest: "bob-context-a", Fingerprint: "bob-action-a",
	}
	olderSequence, ok := ag.observeContinuationFreshness(older)
	if !ok {
		t.Fatal("first Bob digest was not observed")
	}
	newer := cloneValidatedContinuation(older)
	newer.ContextDigest = "bob-context-b"
	newer.Fingerprint = "bob-action-b"
	newerSequence, ok := ag.observeContinuationFreshness(&newer)
	if !ok {
		t.Fatal("newer Bob digest was not observed")
	}
	if ag.continuationFreshnessCurrent(older, olderSequence) {
		t.Fatal("older Bob digest remained current after newer observation")
	}
	if !ag.continuationFreshnessCurrent(&newer, newerSequence) {
		t.Fatal("newer Bob digest was not current")
	}
	if _, ok := ag.observeContinuationFreshness(older); ok {
		t.Fatal("retired Bob digest was re-admitted")
	}
}

func TestAutoContinuationBudgetAndAgentHistorySpanTurns(t *testing.T) {
	fixture := newAutoContinuationFixture(t)
	fixture.state.reset(fixture.snapshot.Epoch, 99)
	if first := fixture.selectCandidate(); first == nil {
		t.Fatal("first auto continuation was rejected")
	}

	// A fresh turn budget must not replay the same source revision in the same
	// durable Agent session.
	fixture.state = newAutoContinuationState(fixture.snapshot.Epoch, 2)
	if duplicate := fixture.selectCandidate(); duplicate != nil {
		t.Fatal("same source revision auto-ran again across turns")
	}

	fixture.candidate.SourceRevision = 8
	if second := fixture.selectCandidate(); second == nil {
		t.Fatal("newer source revision was not admitted")
	}
	fixture.candidate.SourceRevision = 9
	if third := fixture.selectCandidate(); third == nil {
		t.Fatal("second step in the new chain was rejected")
	}
	fixture.candidate.SourceRevision = 10
	if overflow := fixture.selectCandidate(); overflow != nil {
		t.Fatal("hard two-step budget was exceeded")
	}
	if fixture.state.steps != hardMaxAutoContinuationSteps || len(fixture.state.seen) != hardMaxAutoContinuationSteps {
		t.Fatalf("bounded chain state = steps %d seen %d", fixture.state.steps, len(fixture.state.seen))
	}

	fixture.state.reset(fixture.snapshot.Epoch, 2)
	if afterReset := fixture.selectCandidate(); afterReset == nil {
		t.Fatal("explicit new-chain reset did not restore budget for a newer revision")
	}
}

func TestAutoContinuationHistorySurvivesInSessionTranscriptRewrite(t *testing.T) {
	fixture := newAutoContinuationFixture(t)
	if reserved := fixture.selectCandidate(); reserved == nil {
		t.Fatal("initial auto continuation was not reserved")
	}

	fixture.ag.ReplaceMessagesWithinSession([]llm.Message{{Role: "user", Content: "rewritten transcript"}})
	messages := fixture.ag.Messages()
	if len(messages) != 1 || messages[0].Content != "rewritten transcript" {
		t.Fatalf("in-session transcript rewrite = %#v", messages)
	}

	// Use a fresh chain so only the Agent's session-scope reservation can reject
	// this otherwise identical action.
	fixture.state = newAutoContinuationState(fixture.snapshot.Epoch, hardMaxAutoContinuationSteps)
	if duplicate := fixture.selectCandidate(); duplicate != nil {
		t.Fatal("in-session transcript rewrite erased auto continuation replay history")
	}
	if count, _ := fixture.ag.autoContinuationHistorySnapshot(); count != 1 {
		t.Fatalf("in-session transcript rewrite retained %d auto reservations, want 1", count)
	}
}

func TestAutoContinuationHistoryResetsOnlyAtConversationSessionWorkspaceBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		reset func(*Agent)
	}{
		{name: "clear history", reset: func(agent *Agent) { agent.ClearHistory() }},
		{name: "replace messages", reset: func(agent *Agent) { agent.ReplaceMessages(nil) }},
		{name: "durable session", reset: func(agent *Agent) { agent.SetExecutionSessionID(42, "") }},
		{name: "workspace policy", reset: func(agent *Agent) { agent.SetWorkspacePolicy("/other", "") }},
		{name: "workdir", reset: func(agent *Agent) { agent.SetWorkDir("/other") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAutoContinuationFixture(t)
			validated := fixture.validated(t)
			fixture.ag.mu.Lock()
			fixture.ag.autoContinuationHistory.reserve(validated)
			fixture.ag.mu.Unlock()
			test.reset(fixture.ag)
			if count, _ := fixture.ag.autoContinuationHistorySnapshot(); count != 0 {
				t.Fatalf("boundary retained %d auto reservations", count)
			}
		})
	}

	fixture := newAutoContinuationFixture(t)
	validated := fixture.validated(t)
	fixture.ag.mu.Lock()
	fixture.ag.autoContinuationHistory.reserve(validated)
	fixture.ag.mu.Unlock()
	fixture.ag.SetWorkDir(fixture.workspace)
	if count, _ := fixture.ag.autoContinuationHistorySnapshot(); count != 1 {
		t.Fatal("idempotent workspace setter reset auto history")
	}
}

func TestOpaqueAutoClaimIsAtomicUnderConcurrency(t *testing.T) {
	fixture := newAutoContinuationFixture(t)
	validated := fixture.validated(t)
	context := fixture.opaqueContext(t, validated)
	const claimers = 16
	start := make(chan struct{})
	var wins atomic.Int32
	var wait sync.WaitGroup
	wait.Add(claimers)
	for range claimers {
		go func() {
			defer wait.Done()
			<-start
			if fixture.ag.claimAutoReadOnlyContinuationContextWithSnapshot(
				context, fixture.snapshot, AuthorityAutoScoped, fixture.state,
			) != nil {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if got := wins.Load(); got != 1 {
		t.Fatalf("concurrent opaque auto claims = %d, want 1", got)
	}
	if !context.consumed || fixture.state.steps != 1 {
		t.Fatalf("atomic claim state: consumed=%v steps=%d", context.consumed, fixture.state.steps)
	}
}
