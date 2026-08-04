package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

func TestBobWorkspaceCandidateRequiresRootRegularManifest(t *testing.T) {
	workspace := t.TempDir()
	agent := New(nil, nil, 4096)
	agent.SetWorkDir(workspace)

	if _, ok := agent.probeBobWorkspaceCandidate(); ok {
		t.Fatal("workspace without bob.yaml became a Bob candidate")
	}
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", bobWorkspaceManifest), []byte("private: ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := agent.probeBobWorkspaceCandidate(); ok {
		t.Fatal("nested bob.yaml became a root workspace candidate")
	}

	target := filepath.Join(t.TempDir(), "bob.yaml")
	if err := os.WriteFile(target, []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(workspace, bobWorkspaceManifest)); err != nil {
		t.Fatal(err)
	}
	if _, ok := agent.probeBobWorkspaceCandidate(); ok {
		t.Fatal("symlink bob.yaml became a workspace candidate")
	}
	if err := os.Remove(filepath.Join(workspace, bobWorkspaceManifest)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, bobWorkspaceManifest), []byte("content is never parsed locally"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, ok := agent.probeBobWorkspaceCandidate()
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || candidate.workspace != canonicalWorkspace || candidate.manifestInfo == nil || !candidate.manifestInfo.Mode().IsRegular() {
		t.Fatalf("candidate = %#v, %t", candidate, ok)
	}
}

func TestBobWorkspaceBootstrapRouteIsExactAndFailClosed(t *testing.T) {
	fixture := newAutoContinuationFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.workspace, bobWorkspaceManifest), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, ok := fixture.ag.probeBobWorkspaceCandidate()
	if !ok {
		t.Fatal("expected Bob workspace candidate")
	}
	direct := bobContextDefinition("bob__bob_context")
	direct.Behavior = eligibleAutoBehavior()
	fixture.snapshot.Tools = []llm.ToolDef{direct}
	plan, ok := fixture.ag.planBobWorkspaceBootstrap(candidate, fixture.snapshot)
	if !ok || plan.call.Name != direct.Name || plan.call.Arguments["workspace"] != candidate.workspace ||
		plan.call.Arguments["profile"] != "compact" {
		t.Fatalf("direct plan = %#v, %t", plan, ok)
	}
	autoState := newAutoContinuationState(fixture.snapshot.Epoch, 2)
	if prepared := fixture.ag.prepareBobWorkspaceBootstrap(plan, fixture.snapshot, AuthorityAutoScoped, autoState); prepared == nil {
		t.Fatal("exact direct bootstrap was not prepared")
	}
	if replay := fixture.ag.prepareBobWorkspaceBootstrap(plan, fixture.snapshot, AuthorityAutoScoped, autoState); replay != nil {
		t.Fatalf("unchanged Bob candidate replayed automatically: %#v", replay)
	}

	pinned := bobContextDefinition("mcphub__bob__bob_context")
	pinned.Behavior = eligibleAutoBehavior()
	fixture.ag.mu.Lock()
	fixture.ag.trustedMCP = map[string]trustedMCPServer{
		"mcphub": {localOwner: "mcphub", gateway: config.MCPTrustGatewayMCPHub, contracts: map[string]mcpAuthorityContract{
			"bob__bob_context": {effect: executionpkg.EffectReadOnly, auto: true},
		}},
	}
	fixture.ag.mu.Unlock()
	fixture.snapshot.AvailableServers = []string{"mcphub"}
	fixture.snapshot.Tools = []llm.ToolDef{pinned}
	plan, ok = fixture.ag.planBobWorkspaceBootstrap(candidate, fixture.snapshot)
	if !ok || plan.call.Name != pinned.Name {
		t.Fatalf("pinned fallback = %#v, %t", plan, ok)
	}

	generic := bobContextDefinition("mcphub__mcphub_call_tool")
	generic.Behavior = eligibleAutoBehavior()
	fixture.snapshot.Tools = []llm.ToolDef{generic}
	if plan, ok := fixture.ag.planBobWorkspaceBootstrap(candidate, fixture.snapshot); ok {
		t.Fatalf("generic proxy gained bootstrap authority: %#v", plan)
	}

	second := bobContextDefinition("other__bob_context")
	second.Behavior = eligibleAutoBehavior()
	fixture.ag.mu.Lock()
	fixture.ag.trustedMCP = map[string]trustedMCPServer{
		"bob":   {localOwner: "bob", contracts: map[string]mcpAuthorityContract{"bob_context": {effect: executionpkg.EffectReadOnly, auto: true}}},
		"other": {localOwner: "bob", contracts: map[string]mcpAuthorityContract{"bob_context": {effect: executionpkg.EffectReadOnly, auto: true}}},
	}
	fixture.ag.mu.Unlock()
	fixture.snapshot.AvailableServers = []string{"bob", "other"}
	fixture.snapshot.Tools = []llm.ToolDef{direct, second}
	if plan, ok := fixture.ag.planBobWorkspaceBootstrap(candidate, fixture.snapshot); ok {
		t.Fatalf("ambiguous direct routes gained bootstrap authority: %#v", plan)
	}
}

func TestBobWorkspaceContextCacheIsBoundedGenerationFencedAndSessionScoped(t *testing.T) {
	fixture := newAutoContinuationFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.workspace, bobWorkspaceManifest), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, ok := fixture.ag.probeBobWorkspaceCandidate()
	if !ok {
		t.Fatal("expected Bob candidate")
	}
	call := llm.ToolCall{Name: "bob__bob_context", Arguments: map[string]any{
		"workspace": candidate.workspace, "profile": "compact",
	}}
	fixture.ag.mu.RLock()
	admission := bobContextAdmission{candidate: candidate, generation: fixture.ag.bobWorkspaceGeneration, valid: true}
	fixture.ag.mu.RUnlock()
	document := autoLoopBobFixture(t, candidate.workspace, "context-clean-v1.json")
	structured, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	receipt := ecosystem.RawReceipt{Structured: structured}
	projection := fixture.ag.projectSemanticToolReceipt(call, string(structured), structured, nil, false, false, false)
	fixture.ag.settleBobContextAdmission(nil, admission, projection, receipt, ecosystem.MCPHubResultObservation{}, executionpkg.EventCompleted)

	fixture.ag.mu.RLock()
	cache := fixture.ag.bobWorkspaceContext
	generation := fixture.ag.bobWorkspaceGeneration
	fixture.ag.mu.RUnlock()
	if cache == nil || cache.digest.Kind != ecosystem.DigestBobContext || cache.domain != ecosystem.DomainSucceeded {
		t.Fatalf("cache = %#v", cache)
	}
	prompt := fixture.ag.bobWorkspaceContextPrompt()
	if !strings.Contains(prompt, "convergence only") || strings.Contains(prompt, candidate.workspace) || strings.Contains(prompt, `"context"`) {
		t.Fatalf("bounded cache prompt = %q", prompt)
	}

	fixture.ag.ReplaceMessagesWithinSession([]llm.Message{{Role: "user", Content: "compacted"}})
	fixture.ag.mu.RLock()
	preserved := fixture.ag.bobWorkspaceContext != nil
	fixture.ag.mu.RUnlock()
	if !preserved {
		t.Fatal("within-session compaction erased Bob context")
	}

	// An in-flight response admitted under an older generation may not
	// repopulate after mutation/session invalidation.
	fixture.ag.mu.RLock()
	staleAdmission := bobContextAdmission{candidate: candidate, generation: fixture.ag.bobWorkspaceGeneration, valid: true}
	fixture.ag.mu.RUnlock()
	fixture.ag.invalidateBobWorkspaceContext(nil)
	fixture.ag.settleBobContextAdmission(nil, staleAdmission, projection, receipt, ecosystem.MCPHubResultObservation{}, executionpkg.EventCompleted)
	fixture.ag.mu.RLock()
	staleInstalled := fixture.ag.bobWorkspaceContext != nil
	currentGeneration := fixture.ag.bobWorkspaceGeneration
	fixture.ag.mu.RUnlock()
	if staleInstalled || currentGeneration <= generation {
		t.Fatalf("stale response installed=%t generation=%d prior=%d", staleInstalled, currentGeneration, generation)
	}

	// Reinstall, then cross the durable session boundary.
	fixture.ag.mu.RLock()
	admission = bobContextAdmission{candidate: candidate, generation: fixture.ag.bobWorkspaceGeneration, valid: true}
	fixture.ag.mu.RUnlock()
	fixture.ag.settleBobContextAdmission(nil, admission, projection, receipt, ecosystem.MCPHubResultObservation{}, executionpkg.EventCompleted)
	fixture.ag.SetExecutionSessionID(42, "")
	fixture.ag.mu.RLock()
	leaked := fixture.ag.bobWorkspaceContext != nil
	fixture.ag.mu.RUnlock()
	if leaked || fixture.ag.bobWorkspaceContextPrompt() != "" {
		t.Fatal("Bob context leaked across durable session boundary")
	}
}

func TestBobWorkspaceStoredPageAdmissionRequiresExactCallID(t *testing.T) {
	fixture := newAutoContinuationFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.workspace, bobWorkspaceManifest), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, ok := fixture.ag.probeBobWorkspaceCandidate()
	if !ok {
		t.Fatal("expected Bob candidate")
	}
	fixture.ag.mu.RLock()
	admission := bobContextAdmission{candidate: candidate, generation: fixture.ag.bobWorkspaceGeneration, valid: true}
	fixture.ag.mu.RUnlock()
	const callID = "bob-workspace-stored"
	stored := agentMCPHubStoredDocument(t, callID, "bob", "bob_context", 128)
	storedProjection := projectSemanticToolReceipt("mcphub__bob__bob_context", nil, "", stored, nil, false, false, false)
	if resolved := fixture.ag.resolveBobContextAdmission(admission, storedProjection, ecosystem.MCPHubResultObservation{}); resolved.valid {
		t.Fatalf("stored receipt returned admission before complete page: %#v", resolved)
	}

	document := autoLoopBobFixture(t, candidate.workspace, "context-clean-v1.json")
	structured, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	complete := projectSemanticToolReceipt("bob__bob_context", nil, string(structured), structured, nil, false, false, false)
	complete.Route = ecosystem.ToolRoute{Gateway: "mcphub", Server: "bob", Tool: "bob_context", CallID: callID, Lazy: true}
	complete = complete.Normalize()
	resolved := fixture.ag.resolveBobContextAdmission(bobContextAdmission{}, complete, ecosystem.MCPHubResultObservation{
		Projection: complete, Bound: true, Complete: true, Workspace: candidate.workspace,
	})
	if !resolved.valid || resolved.generation != admission.generation {
		t.Fatalf("exact complete page did not recover admission: %#v", resolved)
	}

	complete.Route.CallID = "wrong-call"
	if wrong := fixture.ag.resolveBobContextAdmission(bobContextAdmission{}, complete, ecosystem.MCPHubResultObservation{
		Projection: complete, Bound: true, Complete: true, Workspace: candidate.workspace,
	}); wrong.valid {
		t.Fatalf("wrong call ID recovered admission: %#v", wrong)
	}
}

func TestBobWorkspaceMutationStartedInvalidatesBeforeFailedBackend(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, bobWorkspaceManifest), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	client := &bobMutationCaptureClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "write", Name: "write", Arguments: map[string]any{
			"path": canonicalWorkspace, "content": "cannot replace a directory",
		}}}, Done: true}},
		{{Text: "handled failed write", Done: true}},
	}}
	registry := mcp.NewRegistry()
	t.Cleanup(registry.Close)
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, canonicalWorkspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationOff, MaxAutoSteps: 2,
	})
	candidate, ok := agent.probeBobWorkspaceCandidate()
	if !ok {
		t.Fatal("expected Bob candidate")
	}
	agent.mu.Lock()
	before := agent.bobWorkspaceGeneration
	cleanDocument := autoLoopBobFixture(t, canonicalWorkspace, "context-clean-v1.json")
	cleanStructured, err := json.Marshal(cleanDocument)
	if err != nil {
		t.Fatal(err)
	}
	cleanProjection := projectSemanticToolReceipt("bob__bob_context", nil, string(cleanStructured), cleanStructured, nil, false, false, false)
	if cleanProjection.Digest == nil {
		t.Fatal("clean fixture did not produce Bob context digest")
	}
	agent.bobWorkspaceContext = &bobWorkspaceContextCache{
		candidate: candidate, domain: ecosystem.DomainSucceeded,
		digest: *cloneBobDigest(cleanProjection.Digest),
	}
	agent.mu.Unlock()

	if err := agent.RunTurn(context.Background(), &outputRecorder{}, "turn_bob_mutation_failed"); err != nil {
		t.Fatal(err)
	}
	if got := eventTypesForTool(ledger.snapshot(), "write"); len(got) != 4 ||
		got[0] != executionpkg.EventRequested || got[1] != executionpkg.EventApproved ||
		got[2] != executionpkg.EventStarted || got[3] != executionpkg.EventFailed {
		t.Fatalf("failed mutation lifecycle = %v", got)
	}
	agent.mu.RLock()
	cache := agent.bobWorkspaceContext
	after := agent.bobWorkspaceGeneration
	agent.mu.RUnlock()
	if cache != nil || after <= before {
		t.Fatalf("failed started mutation retained cache=%#v generation=%d before=%d", cache, after, before)
	}
	if len(client.systems) != 2 || !strings.Contains(client.systems[0], "Bob repository context") ||
		strings.Contains(client.systems[1], "Bob repository context") || strings.Contains(client.systems[1], "Bob repository clean") {
		t.Fatalf("mutation systems retained stale Bob context: %#v", client.systems)
	}
}

type bobBootstrapCaptureClient struct {
	systems []string
}

type bobMutationCaptureClient struct {
	responses [][]llm.StreamChunk
	calls     int
	systems   []string
}

func (client *bobMutationCaptureClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	client.systems = append(client.systems, options.System)
	if client.calls >= len(client.responses) {
		return nil
	}
	response := client.responses[client.calls]
	client.calls++
	for _, chunk := range response {
		if err := emit(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (*bobMutationCaptureClient) Ping() error   { return nil }
func (*bobMutationCaptureClient) Model() string { return "test-model" }
func (*bobMutationCaptureClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func (client *bobBootstrapCaptureClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	client.systems = append(client.systems, options.System)
	return emit(llm.StreamChunk{Text: "done", Done: true})
}

func (*bobBootstrapCaptureClient) Ping() error   { return nil }
func (*bobBootstrapCaptureClient) Model() string { return "test-model" }
func (*bobBootstrapCaptureClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type bobBootstrapOutput struct {
	outputRecorder
	suggestions []*ContinuationSuggestion
}

func (output *bobBootstrapOutput) ContinuationSuggestion(_ string, _ uint64, suggestion *ContinuationSuggestion) {
	output.suggestions = append(output.suggestions, suggestion)
}

func TestBobWorkspaceSuggestionDoesNotDispatch(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, bobWorkspaceManifest), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(workspace, 1, "bob_context", map[string]any{
			"workspace": workspace, "profile": "compact",
		})
	})
	client := &bobBootstrapCaptureClient{}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, workspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationSuggest, MaxAutoSteps: 2,
	})
	output := &bobBootstrapOutput{}
	if err := agent.RunTurn(context.Background(), output, "turn_bob_workspace_suggest"); err != nil {
		t.Fatal(err)
	}
	if backend.count("bob_context") != 0 || len(eventsForTool(ledger.snapshot(), "bob__bob_context")) != 0 {
		t.Fatalf("suggest mode dispatched Bob: backend=%v events=%v", backend.snapshot(), ledger.snapshot())
	}
	if len(output.suggestions) != 1 || output.suggestions[0] == nil || output.suggestions[0].Tool != "bob_context" {
		t.Fatalf("suggestions = %#v", output.suggestions)
	}
	if len(client.systems) != 1 || !strings.Contains(client.systems[0], "filename is not proof of validity") ||
		!strings.Contains(client.systems[0], "bob__bob_context") {
		t.Fatalf("bootstrap system hint = %#v", client.systems)
	}
}

func TestBobWorkspaceAutoBootstrapUsesLedgerBeforeProviderAndCachesDigest(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, bobWorkspaceManifest), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	backend, registry := newAutoLoopRegistry(t, canonicalWorkspace, func(_ int) map[string]any {
		return autoLoopCortexStatus(canonicalWorkspace, 1, "bob_context", map[string]any{
			"workspace": canonicalWorkspace, "profile": "compact",
		})
	})
	client := &bobBootstrapCaptureClient{}
	ledger := &fakeExecutionLedger{}
	agent := newAutoLoopAgent(t, canonicalWorkspace, client, registry, ledger, config.ContinuationsConfig{
		Mode: config.ContinuationAutoReadOnly, MaxAutoSteps: 2,
	})
	if err := agent.RunTurn(context.Background(), &bobBootstrapOutput{}, "turn_bob_workspace_auto"); err != nil {
		t.Fatal(err)
	}
	if backend.count("bob_context") != 1 {
		t.Fatalf("backend calls = %v", backend.snapshot())
	}
	if got := eventTypesForTool(ledger.snapshot(), "bob__bob_context"); len(got) != 4 ||
		got[0] != executionpkg.EventRequested || got[1] != executionpkg.EventApproved ||
		got[2] != executionpkg.EventStarted || got[3] != executionpkg.EventCompleted {
		t.Fatalf("bootstrap lifecycle = %v", got)
	}
	agent.mu.RLock()
	cache := agent.bobWorkspaceContext
	agent.mu.RUnlock()
	if cache == nil || cache.digest.Kind != ecosystem.DigestBobContext || cache.domain != ecosystem.DomainSucceeded {
		t.Fatalf("installed cache = %#v", cache)
	}
	standardCall := llm.ToolCall{Name: "bob__bob_context", Arguments: map[string]any{
		"workspace": canonicalWorkspace, "profile": "standard",
	}}
	standardAdmission := agent.captureBobContextAdmission(standardCall)
	if !standardAdmission.valid {
		t.Fatal("schema-supported standard profile was not admitted for cache refresh")
	}
	driftDocument := autoLoopBobFixture(t, canonicalWorkspace, "context-drift-v1.json")
	driftContext := driftDocument["context"].(map[string]any)
	driftContext["profile"] = "standard"
	driftContext["truncation"].(map[string]any)["profile"] = "standard"
	driftContext["truncation"].(map[string]any)["byte_limit"] = 24 << 10
	driftStructured, err := json.Marshal(driftDocument)
	if err != nil {
		t.Fatal(err)
	}
	driftReceipt := ecosystem.RawReceipt{Structured: driftStructured}
	driftProjection := agent.projectSemanticToolReceipt(standardCall, string(driftStructured), driftStructured, nil, false, false, false)
	agent.settleBobContextAdmission(nil, standardAdmission, driftProjection, driftReceipt, ecosystem.MCPHubResultObservation{}, executionpkg.EventCompleted)
	agent.mu.RLock()
	refreshed := agent.bobWorkspaceContext
	agent.mu.RUnlock()
	if refreshed == nil || refreshed.domain != ecosystem.DomainDrift || refreshed.digest.State != "drifted" {
		t.Fatalf("standard-profile refresh = %#v", refreshed)
	}
	if len(client.systems) != 1 || strings.Contains(client.systems[0], "filename is not proof of validity") {
		t.Fatalf("queued bootstrap suggestion leaked into provider prompt: %#v", client.systems)
	}
	for _, message := range SanitizeMessagesForPersistence(agent.Messages()) {
		if strings.Contains(message.Content, canonicalWorkspace) {
			t.Fatalf("durable message leaked workspace: %#v", message)
		}
		for _, call := range message.ToolCalls {
			if got := FormatToolArgsForTool(call.Name, call.Arguments); strings.Contains(got, canonicalWorkspace) {
				t.Fatalf("durable tool arguments leaked workspace: %#v", call)
			}
		}
	}
}

func TestBobWorkspaceContinuationOffAddsNoHintOrDispatch(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, bobWorkspaceManifest), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, registry := newAutoLoopRegistry(t, workspace, func(_ int) map[string]any { return map[string]any{} })
	client := &bobBootstrapCaptureClient{}
	agent := newAutoLoopAgent(t, workspace, client, registry, &fakeExecutionLedger{}, config.ContinuationsConfig{
		Mode: config.ContinuationOff, MaxAutoSteps: 2,
	})
	if err := agent.RunTurn(context.Background(), &bobBootstrapOutput{}, "turn_bob_workspace_off"); err != nil {
		t.Fatal(err)
	}
	if backend.count("bob_context") != 0 || len(client.systems) != 1 || strings.Contains(client.systems[0], "bob.yaml") {
		t.Fatalf("off mode backend=%v systems=%#v", backend.snapshot(), client.systems)
	}
}

var _ llm.Client = (*bobBootstrapCaptureClient)(nil)
var _ llm.Client = (*bobMutationCaptureClient)(nil)
var _ Output = (*bobBootstrapOutput)(nil)
var _ ContinuationOutput = (*bobBootstrapOutput)(nil)
