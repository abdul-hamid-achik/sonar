package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/capabilityadvisor"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	toolspkg "github.com/abdul-hamid-achik/sonar/internal/tools"
)

func admissionTestTool(name string, payloadBytes int) llm.ToolDef {
	return llm.ToolDef{
		Name:        name,
		Description: strings.Repeat("schema detail ", max(1, payloadBytes/14)),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{
					"type":        "string",
					"description": strings.Repeat("bounded input ", max(1, payloadBytes/14)),
				},
			},
			"required":             []string{"input"},
			"additionalProperties": false,
		},
	}
}

func admittedToolNames(defs []llm.ToolDef) map[string]struct{} {
	names := make(map[string]struct{}, len(defs))
	for _, def := range defs {
		names[def.Name] = struct{}{}
	}
	return names
}

func admittedToolNameList(defs []llm.ToolDef) []string {
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	return names
}

func trustedMCPHubNamespaces(names ...string) map[string]struct{} {
	trusted := make(map[string]struct{}, len(names))
	for _, name := range names {
		trusted[name] = struct{}{}
	}
	return trusted
}

func markTrustedMCPHubNamespace(agent *Agent, namespace string) {
	agent.mu.Lock()
	agent.trustedMCP = map[string]trustedMCPServer{
		namespace: {gateway: config.MCPTrustGatewayMCPHub},
	}
	agent.mu.Unlock()
}

func toolDefsByExactName(defs []llm.ToolDef, names ...string) []llm.ToolDef {
	byName := make(map[string]llm.ToolDef, len(defs))
	for _, def := range defs {
		byName[def.Name] = def
	}
	selected := make([]llm.ToolDef, 0, len(names))
	for _, name := range names {
		if def, ok := byName[name]; ok {
			selected = append(selected, def)
		}
	}
	return selected
}

func realisticMCPHubMetaToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		{
			Name:        "mcphub__mcphub_resolve_tool",
			Description: "Route the current goal or activity to the best hidden downstream capability and return a ranked recommendation with a ready-to-fill argument template.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":    map[string]any{"type": "string", "description": "Natural-language description of the current goal or activity."},
					"max_hits": map[string]any{"type": "integer", "description": "Maximum alternatives to return."},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "mcphub__mcphub_call_tool",
			Description: "Invoke one downstream tool through the lazy gateway. Pass the server, tool, and exact arguments; oversized results return a bounded retrieval receipt.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server":     map[string]any{"type": "string", "description": "Downstream server name."},
					"tool":       map[string]any{"type": "string", "description": "Downstream tool name or combined server-qualified name."},
					"arguments":  map[string]any{"type": "object", "description": "Arguments passed unchanged to the downstream tool."},
					"detach":     map[string]any{"type": "boolean", "description": "Run a long call in the background."},
					"timeout_ms": map[string]any{"type": "integer", "description": "Optional bounded call timeout in milliseconds."},
				},
				"required":             []string{"tool"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "mcphub__mcphub_describe_tool",
			Description: "Return one downstream tool's description and complete JSON input schema before constructing arguments.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server": map[string]any{"type": "string", "description": "Downstream server name."},
					"tool":   map[string]any{"type": "string", "description": "Downstream tool name."},
				},
				"required":             []string{"tool"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "mcphub__mcphub_search_tools",
			Description: "Search and rank hidden downstream tools from a natural-language capability or task description.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":    map[string]any{"type": "string", "description": "Capability or task context to search for."},
					"max_hits": map[string]any{"type": "integer", "description": "Maximum matches to return."},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "mcphub__mcphub_get_result",
			Description: "Retrieve a bounded page of a complete result previously stored by the gateway.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"callId": map[string]any{"type": "string", "description": "Opaque stored-result call identifier."},
					"cursor": map[string]any{"type": "integer", "description": "Zero-based byte cursor."},
				},
				"required":             []string{"callId"},
				"additionalProperties": false,
			},
		},
		{
			Name:        "mcphub__mcphub_poll_result",
			Description: "Poll a detached downstream call and return completion or another bounded receipt.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"callId": map[string]any{"type": "string", "description": "Opaque detached-call identifier."},
				},
				"required":             []string{"callId"},
				"additionalProperties": false,
			},
		},
	}
}

func TestToolAdmissionKeepsLazyGatewayAtSmallContexts(t *testing.T) {
	defs := append(toolspkg.AllToolDefs(), realisticMCPHubMetaToolDefs()...)
	requiredBootstrap := toolDefsByExactName(
		defs,
		"read",
		"grep",
		"mcphub__mcphub_resolve_tool",
		"mcphub__mcphub_call_tool",
	)
	recommendedBootstrap := append(
		append([]llm.ToolDef(nil), requiredBootstrap...),
		toolDefsByExactName(
			defs,
			"mcphub__mcphub_describe_tool",
			"mcphub__mcphub_search_tools",
		)...,
	)
	t.Logf(
		"lazy gateway bootstrap schema tokens: required=%d with_describe_search=%d",
		estimateToolDefinitionsPromptTokens(requiredBootstrap),
		estimateToolDefinitionsPromptTokens(recommendedBootstrap),
	)
	for _, numCtx := range []int{4_096, 8_192} {
		t.Run(fmt.Sprintf("%d", numCtx), func(t *testing.T) {
			budget := numCtx / maxToolSchemaContextShare
			admitted := admitToolDefsForSchemaBudget(defs, nil, budget, trustedMCPHubNamespaces("mcphub"))
			names := admittedToolNames(admitted)
			t.Logf(
				"num_ctx=%d budget=%d total_schema_tokens=%d admitted_schema_tokens=%d admitted=%v",
				numCtx,
				budget,
				estimateToolDefinitionsPromptTokens(defs),
				estimateToolDefinitionsPromptTokens(admitted),
				admittedToolNameList(admitted),
			)
			for _, name := range []string{
				"read",
				"grep",
				"mcphub__mcphub_resolve_tool",
				"mcphub__mcphub_call_tool",
				"mcphub__mcphub_describe_tool",
				"mcphub__mcphub_search_tools",
			} {
				if _, ok := names[name]; !ok {
					t.Errorf("num_ctx=%d omitted bootstrap tool %q", numCtx, name)
				}
			}
		})
	}
}

func TestLazyGatewayBootstrapIsAtomicAtItsExactBudget(t *testing.T) {
	defs := append(toolspkg.AllToolDefs(), realisticMCPHubMetaToolDefs()...)
	required := toolDefsByExactName(
		defs,
		"read",
		"grep",
		"mcphub__mcphub_resolve_tool",
		"mcphub__mcphub_call_tool",
	)
	exactBudget := estimateToolDefinitionsPromptTokens(required)
	if exactBudget <= 1 {
		t.Fatalf("invalid exact bootstrap budget %d", exactBudget)
	}

	if got := lazyGatewayBootstrapForBudget(required, exactBudget-1, trustedMCPHubNamespaces("mcphub")); len(got) != 0 {
		t.Fatalf("undersized budget admitted partial gateway bootstrap: %v", admittedToolNameList(got))
	}
	got := lazyGatewayBootstrapForBudget(required, exactBudget, trustedMCPHubNamespaces("mcphub"))
	if names := admittedToolNameList(got); !reflect.DeepEqual(names, admittedToolNameList(required)) {
		t.Fatalf("exact-budget bootstrap = %v, want %v", names, admittedToolNameList(required))
	}
}

func TestLazyGatewayBootstrapDoesNotMixGatewayAuthorities(t *testing.T) {
	defs := []llm.ToolDef{
		admissionTestTool("read", 20),
		admissionTestTool("grep", 20),
		admissionTestTool("first__mcphub_resolve_tool", 20),
		admissionTestTool("second__mcphub_call_tool", 20),
	}
	if got := lazyGatewayBootstrapForBudget(defs, 4_096, trustedMCPHubNamespaces("first")); len(got) != 0 {
		t.Fatalf("mixed gateway authorities produced bootstrap %v", admittedToolNameList(got))
	}
}

func TestLazyGatewayBootstrapRejectsUntrustedImpostor(t *testing.T) {
	defs := []llm.ToolDef{
		admissionTestTool("read", 20),
		admissionTestTool("grep", 20),
		admissionTestTool("aaa__mcphub_resolve_tool", 20),
		admissionTestTool("aaa__mcphub_call_tool", 20),
		admissionTestTool("mcphub__mcphub_resolve_tool", 20),
		admissionTestTool("mcphub__mcphub_call_tool", 20),
	}
	want := toolDefsByExactName(
		defs,
		"read", "grep",
		"mcphub__mcphub_resolve_tool", "mcphub__mcphub_call_tool",
	)
	got := lazyGatewayBootstrapForBudget(defs, estimateToolDefinitionsPromptTokens(want), trustedMCPHubNamespaces("mcphub"))
	if names := admittedToolNameList(got); !reflect.DeepEqual(names, admittedToolNameList(want)) {
		t.Fatalf("trusted lazy bootstrap = %v, want %v", names, admittedToolNameList(want))
	}
	if got := lazyGatewayBootstrapForBudget(defs, estimateToolDefinitionsPromptTokens(want), nil); len(got) != 0 {
		t.Fatalf("untrusted namespace produced lazy bootstrap %v", admittedToolNameList(got))
	}
}

func TestAdmitToolDefsKeepsCompleteCatalogWhenItFits(t *testing.T) {
	defs := []llm.ToolDef{
		admissionTestTool("specialist__inspect", 20),
		admissionTestTool("read", 20),
	}
	budget := estimateToolDefinitionsPromptTokens(defs)
	got := admitToolDefsForSchemaBudget(defs, nil, budget, nil)
	if len(got) != len(defs) || got[0].Name != defs[0].Name || got[1].Name != defs[1].Name {
		t.Fatalf("catalog changed despite fitting exactly: %#v", got)
	}
}

func TestMeasuredToolSchemaEstimateMatchesCanonicalJSON(t *testing.T) {
	defs := []llm.ToolDef{
		admissionTestTool("ascii", 120),
		{
			Name:        "unicode",
			Description: "Herramienta para búsquedas: café, 東京 y emoji ✅",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"consulta": map[string]any{"type": "string", "description": "búsqueda precisa"},
				},
			},
		},
	}
	measured, ok := measureToolDefinitions(defs)
	if !ok {
		t.Fatal("measureToolDefinitions rejected JSON-serializable definitions")
	}
	got := estimateMeasuredToolDefinitionsPromptTokens(measured)
	want := estimateToolDefinitionsPromptTokens(defs)
	if got != want {
		t.Fatalf("measured schema estimate = %d, want canonical slice estimate %d", got, want)
	}
}

func TestMeasuredAdmissionMatchesLegacySelection(t *testing.T) {
	defs := append([]llm.ToolDef(nil), realisticMCPHubMetaToolDefs()...)
	defs = append(defs,
		admissionTestTool("read", 80),
		admissionTestTool("grep", 80),
		admissionTestTool("specialist__unicode", 600),
	)
	defs[len(defs)-1].Description = "análisis de café 東京 " + defs[len(defs)-1].Description
	for index := 0; index < 24; index++ {
		defs = append(defs, admissionTestTool(fmt.Sprintf("specialist__tool_%02d", index), 420))
	}
	hint := &capabilityadvisor.Hint{Namespaced: "specialist__tool_07", Server: "specialist", Tool: "tool_07"}
	for _, budget := range []int{256, 512, 1_024, 2_048, 4_096, 8_192} {
		got := admittedToolNameList(admitToolDefsForSchemaBudget(defs, hint, budget, trustedMCPHubNamespaces("mcphub")))
		want := admittedToolNameList(admitToolDefsForSchemaBudgetLegacy(defs, hint, budget, trustedMCPHubNamespaces("mcphub")))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("budget %d admission = %v, want legacy selection %v", budget, got, want)
		}
	}
}

func BenchmarkToolSchemaAdmissionLargeCatalog(b *testing.B) {
	defs := append([]llm.ToolDef(nil), realisticMCPHubMetaToolDefs()...)
	defs = append(defs, toolspkg.AllToolDefs()...)
	for index := 0; index < 128; index++ {
		defs = append(defs, admissionTestTool(fmt.Sprintf("specialist__tool_%03d", index), 1_200))
	}
	hint := &capabilityadvisor.Hint{Namespaced: "specialist__tool_064", Server: "specialist", Tool: "tool_064"}
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		_ = admitToolDefsForSchemaBudget(defs, hint, 4_096, trustedMCPHubNamespaces("mcphub"))
	}
}

func BenchmarkToolSchemaAdmissionLargeCatalogLegacy(b *testing.B) {
	defs := append([]llm.ToolDef(nil), realisticMCPHubMetaToolDefs()...)
	defs = append(defs, toolspkg.AllToolDefs()...)
	for index := 0; index < 128; index++ {
		defs = append(defs, admissionTestTool(fmt.Sprintf("specialist__tool_%03d", index), 1_200))
	}
	hint := &capabilityadvisor.Hint{Namespaced: "specialist__tool_064", Server: "specialist", Tool: "tool_064"}
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		_ = admitToolDefsForSchemaBudgetLegacy(defs, hint, 4_096, trustedMCPHubNamespaces("mcphub"))
	}
}

func TestAdmitToolDefsPrioritizesCoreGatewayAndVisibleRecommendation(t *testing.T) {
	coreNames := []string{
		"read",
		"mcphub__mcphub_describe_tool",
		"mcphub__mcphub_call_tool",
		"bob__bob_plan",
	}
	defs := make([]llm.ToolDef, 0, len(coreNames)+20)
	for i := 0; i < 20; i++ {
		defs = append(defs, admissionTestTool(fmt.Sprintf("specialist__tool_%02d", i), 180))
	}
	for _, name := range coreNames {
		defs = append(defs, admissionTestTool(name, 40))
	}
	defs = append(defs, admissionTestTool("memory_recall", 40))

	coreDefs := defs[len(defs)-len(coreNames)-1 : len(defs)-1]
	budget := estimateToolDefinitionsPromptTokens(coreDefs) + 16
	hint := &capabilityadvisor.Hint{
		Namespaced: "bob__bob_plan",
		Server:     "bob",
		Tool:       "bob_plan",
	}
	got := admitToolDefsForSchemaBudget(defs, hint, budget, trustedMCPHubNamespaces("mcphub"))
	names := admittedToolNames(got)
	for _, name := range coreNames {
		if _, ok := names[name]; !ok {
			t.Errorf("admission omitted prioritized tool %q from %#v", name, got)
		}
	}
	for name := range names {
		if strings.HasPrefix(name, "specialist__tool_") {
			t.Errorf("admission kept lower-priority specialist %q", name)
		}
	}
}

func TestAmbiguousCapabilityDoesNotPromoteCandidate(t *testing.T) {
	hint := &capabilityadvisor.Hint{
		Namespaced: "bob__bob_plan",
		Server:     "bob",
		Tool:       "bob_plan",
		Ambiguous:  true,
	}
	if matchesCapabilityRecommendation("bob__bob_plan", hint) {
		t.Fatal("ambiguous capability candidate was treated as a selected recommendation")
	}
}

func TestPinnedGatewayRouteMatchesVisibleCapabilityRecommendation(t *testing.T) {
	hint := &capabilityadvisor.Hint{
		Namespaced: "bob__bob_plan",
		Server:     "bob",
		Tool:       "bob_plan",
	}
	if !matchesCapabilityRecommendation("mcphub__bob__bob_plan", hint) {
		t.Fatal("visible pinned gateway route did not match the recommended downstream identity")
	}
}

func TestToolAdmissionReexpandsFromTurnCatalogAfterPressureClears(t *testing.T) {
	agent := New(&toolAdmissionCaptureClient{}, nil, 4_096)
	agent.SetWorkDir(t.TempDir())
	agent.AddUserMessage(strings.Repeat("history pressure ", 600))

	available := []llm.ToolDef{
		admissionTestTool("read", 100),
		admissionTestTool("mcphub__mcphub_resolve_tool", 100),
		admissionTestTool("mcphub__mcphub_call_tool", 100),
		admissionTestTool("specialist__large", 4_000),
	}
	runtime := &turnRuntime{
		a:              agent,
		out:            &outputRecorder{},
		turnNumCtx:     4_096,
		turnModel:      "qwen3.5:2b",
		turnFilesystem: filesystemContext{workDir: agent.activeWorkDir()},
		tools:          append([]llm.ToolDef(nil), available...),
	}
	runtime.rebuildSystem(context.Background())

	narrowed := runtime.admitToolSchemasForContext(context.Background())
	if !narrowed.Applied || narrowed.OmittedTools == 0 {
		t.Fatalf("pressure admission = %#v, want a narrowed catalog", narrowed)
	}

	agent.ReplaceMessagesWithinSession([]llm.Message{{
		Role: "user", Content: "Continue with the now-compact history.",
	}})
	expanded := runtime.admitToolSchemasForContext(context.Background())
	if expanded.Applied {
		t.Fatalf("post-compaction admission unexpectedly remained narrowed: %#v", expanded)
	}
	if len(runtime.tools) != len(available) {
		t.Fatalf("post-compaction tools = %d, want complete catalog of %d", len(runtime.tools), len(available))
	}
	for index := range available {
		if runtime.tools[index].Name != available[index].Name {
			t.Fatalf("post-compaction tool %d = %q, want %q", index, runtime.tools[index].Name, available[index].Name)
		}
	}
}

type toolAdmissionCaptureClient struct {
	mu      sync.Mutex
	options []llm.ChatOptions
}

func (client *toolAdmissionCaptureClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	client.mu.Lock()
	client.options = append(client.options, options)
	client.mu.Unlock()
	return emit(llm.StreamChunk{Text: "done", Done: true, EvalCount: 1, PromptEvalCount: 1})
}

func (*toolAdmissionCaptureClient) Ping() error   { return nil }
func (*toolAdmissionCaptureClient) Model() string { return "phi4-mini:latest" }
func (*toolAdmissionCaptureClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func (client *toolAdmissionCaptureClient) lastOptions() (llm.ChatOptions, bool) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.options) == 0 {
		return llm.ChatOptions{}, false
	}
	return client.options[len(client.options)-1], true
}

type compactionAdmissionClient struct {
	calls int
}

func (client *compactionAdmissionClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	client.calls++
	return emit(llm.StreamChunk{Text: "compact recap", Done: true, EvalCount: 3, PromptEvalCount: 2_900})
}

func (*compactionAdmissionClient) Ping() error   { return nil }
func (*compactionAdmissionClient) Model() string { return "qwen3.5:2b" }
func (*compactionAdmissionClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func TestSystemAdmissionReexpandsToolsAfterHistoryCompaction(t *testing.T) {
	client := &compactionAdmissionClient{}
	agent := New(client, nil, 4_096)
	agent.SetWorkDir(t.TempDir())
	for index := 0; index < 2; index++ {
		agent.AppendMessage(llm.Message{Role: "user", Content: strings.Repeat("older user context ", 170)})
		agent.AppendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("older assistant context ", 170)})
	}
	agent.AppendMessage(llm.Message{Role: "user", Content: "Recent question one."})
	agent.AppendMessage(llm.Message{Role: "assistant", Content: "Recent answer one."})
	agent.AppendMessage(llm.Message{Role: "user", Content: "Recent question two."})
	agent.AppendMessage(llm.Message{Role: "assistant", Content: "Recent answer two."})

	available := []llm.ToolDef{
		admissionTestTool("read", 100),
		admissionTestTool("mcphub__mcphub_resolve_tool", 100),
		admissionTestTool("mcphub__mcphub_call_tool", 100),
		admissionTestTool("specialist__large", 2_000),
	}
	runtime := &turnRuntime{
		a:                   agent,
		out:                 &outputRecorder{},
		turnNumCtx:          4_096,
		turnModel:           client.Model(),
		availableTools:      append([]llm.ToolDef(nil), available...),
		tools:               append([]llm.ToolDef(nil), available...),
		turnFilesystem:      filesystemContext{workDir: agent.activeWorkDir()},
		compactionForbidden: false,
	}
	runtime.rebuildSystem(context.Background())
	if !shouldCompactForContext(runtime.estimatedPromptTokens(), runtime.turnNumCtx) {
		t.Fatalf("fixture prompt = %d tokens, want pre-compaction pressure", runtime.estimatedPromptTokens())
	}

	if err := runtime.admitSystemPrompt(context.Background()); err != nil {
		t.Fatalf("system admission after compaction: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("compaction provider calls = %d, want 1", client.calls)
	}
	if len(runtime.tools) != len(available) {
		t.Fatalf("post-compaction tools = %d, want complete catalog of %d", len(runtime.tools), len(available))
	}
	if messages := agent.Messages(); len(messages) != keepMessages+1 || !strings.HasPrefix(messages[0].Content, conversationSummaryPrefix) {
		t.Fatalf("post-compaction messages = %#v", messages)
	}
}

func TestFirstTurnAt16KAdmitsSchemasBeforeProvider(t *testing.T) {
	client := &toolAdmissionCaptureClient{}
	agent := New(client, nil, 16_384)
	agent.SetWorkDir(t.TempDir())
	agent.AddUserMessage("Review this repository.")

	available := append([]llm.ToolDef(nil), agent.toolsBuiltinToolDefs()...)
	for _, operation := range []string{
		"mcphub_resolve_tool",
		"mcphub_search_tools",
		"mcphub_describe_tool",
		"mcphub_call_tool",
		"mcphub_get_result",
		"mcphub_poll_result",
	} {
		available = append(available, admissionTestTool("mcphub__"+operation, 120))
	}
	for i := 0; i < 48; i++ {
		available = append(available, admissionTestTool(fmt.Sprintf("specialist__tool_%02d", i), 420))
	}

	runtime := &turnRuntime{
		a:                       agent,
		out:                     &outputRecorder{},
		turnID:                  "turn_schema_admission",
		turnNumCtx:              16_384,
		turnModel:               client.Model(),
		turnFilesystem:          filesystemContext{workDir: agent.activeWorkDir()},
		tools:                   available,
		trustedMCPHubNamespaces: trustedMCPHubNamespaces("mcphub"),
	}
	runtime.rebuildSystem(context.Background())
	before := runtime.estimatedPromptTokens()
	if !shouldCompactForContext(before, runtime.turnNumCtx) {
		t.Fatalf("fixture prompt = %d tokens, want pressure above 75%% of 16384", before)
	}

	breakdown := runtime.admitToolSchemasForContext(context.Background())
	t.Logf("synthetic admission breakdown: %+v", breakdown)
	if !breakdown.Applied || breakdown.OmittedTools == 0 {
		t.Fatalf("admission breakdown = %#v, want a narrowed catalog", breakdown)
	}
	if shouldCompactForContext(runtime.estimatedPromptTokens(), runtime.turnNumCtx) {
		t.Fatalf("admitted first-turn prompt still exceeds target: %#v", breakdown)
	}
	names := admittedToolNames(runtime.tools)
	for _, name := range []string{
		"read", "grep", "glob", "ls", "find", "bash", "exists", "write", "edit",
		"mcphub__mcphub_describe_tool", "mcphub__mcphub_call_tool",
		"mcphub__mcphub_get_result", "mcphub__mcphub_poll_result",
		"mcphub__mcphub_search_tools", "mcphub__mcphub_resolve_tool",
	} {
		if _, ok := names[name]; !ok {
			t.Errorf("first-turn admission omitted essential %q", name)
		}
	}

	runtime.maxIters = 1
	runtime.autoProgress = newAutoTurnProgress()
	if _, _, _, err := runtime.providerStage(context.Background(), 0); err != nil {
		t.Fatalf("provider stage rejected admitted first turn: %v", err)
	}
	options, ok := client.lastOptions()
	if !ok {
		t.Fatal("provider did not receive the admitted first turn")
	}
	if len(options.Tools) != breakdown.AdmittedTools {
		t.Fatalf("provider tools = %d, admission = %d", len(options.Tools), breakdown.AdmittedTools)
	}
	if strings.Contains(options.System, "specialist__tool_") || strings.Contains(options.System, "mcphub__mcphub_call_tool") {
		t.Fatalf("system prompt duplicated native tool catalog:\n%s", options.System)
	}
}

func TestRunFirstTurnAt16KWithLargeMCPRegistry(t *testing.T) {
	registry := newMCPPreflightRegistryWithPadding(t, filepath.Join(t.TempDir(), "calls.log"), 48)
	client := &toolAdmissionCaptureClient{}
	agent := New(client, registry, 16_384)
	t.Cleanup(agent.Close)
	markTrustedMCPHubNamespace(agent, mcpPreflightServerName)
	agent.SetWorkDir(t.TempDir())
	agent.SetModeContext("", BuildToolPolicy())
	agent.AddUserMessage("Review this repository and identify the most important issue.")

	available := agent.ToolCount()
	if available < 60 {
		t.Fatalf("large-catalog fixture exposes %d tools, want at least 60", available)
	}
	if err := agent.RunTurn(context.Background(), &outputRecorder{}, "turn_first_16k_large_catalog"); err != nil {
		t.Fatalf("first turn was rejected before inference: %v", err)
	}
	options, ok := client.lastOptions()
	if !ok {
		t.Fatal("provider did not receive the first turn")
	}
	if len(options.Tools) >= available {
		t.Fatalf("provider received %d of %d tools, want pressure-driven admission", len(options.Tools), available)
	}
	t.Logf(
		"registry admission: available=%d admitted=%d host_prompt_tokens=%d schema_tokens=%d limit=%d",
		available,
		len(options.Tools),
		estimateHostPromptTokens(options.System, options.Tools)+estimateMessagesPromptTokens(options.Messages),
		estimateToolDefinitionsPromptTokens(options.Tools),
		16_384,
	)
	if shouldCompactForContext(
		estimateHostPromptTokens(options.System, options.Tools)+estimateMessagesPromptTokens(options.Messages),
		16_384,
	) {
		t.Fatalf("provider received an oversized first-turn prompt with %d admitted tools", len(options.Tools))
	}
	names := admittedToolNames(options.Tools)
	for _, name := range []string{
		"read", "grep", "glob", "ls", "find", "bash", "exists", "write", "edit",
		mcpPreflightServerName + "__mcphub_describe_tool",
		mcpPreflightServerName + "__mcphub_call_tool",
		mcpPreflightServerName + "__mcphub_get_result",
		mcpPreflightServerName + "__mcphub_poll_result",
		mcpPreflightServerName + "__mcphub_search_tools",
		mcpPreflightServerName + "__mcphub_resolve_tool",
	} {
		if _, ok := names[name]; !ok {
			t.Errorf("provider first turn omitted essential %q", name)
		}
	}
}

func TestEstimatedPromptTokensClampsToBothFloors(t *testing.T) {
	tests := []struct {
		name         string
		systemLen    int // controls heuristic estimate (chars/4)
		lastPrompt   int
		receiptFloor int
		wantMin      int
	}{
		{name: "heuristic dominates", systemLen: 20_000, lastPrompt: 1_000, receiptFloor: 2_000, wantMin: 5_000},
		{name: "lastPrompt floor dominates", systemLen: 400, lastPrompt: 4_000, receiptFloor: 2_000, wantMin: 4_000},
		{name: "receipt floor dominates", systemLen: 400, lastPrompt: 2_000, receiptFloor: 6_000, wantMin: 6_000},
		{name: "receipt beats lastPrompt", systemLen: 200, lastPrompt: 3_000, receiptFloor: 3_500, wantMin: 3_500},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &toolAdmissionCaptureClient{}
			agent := New(client, nil, 16_384)
			agent.SetWorkDir(t.TempDir())
			if test.receiptFloor > 0 {
				if err := agent.RestoreContextPromptFloor(ContextPromptFloor{
					Tokens:        test.receiptFloor,
					HostTokens:    0,
					MessageTokens: 0,
					Model:         client.Model(),
				}); err != nil {
					t.Fatal(err)
				}
			}
			rt := &turnRuntime{
				a:                agent,
				turnModel:        client.Model(),
				turnNumCtx:       16_384,
				system:           strings.Repeat("x ", test.systemLen/2),
				lastPromptTokens: test.lastPrompt,
			}
			got := rt.estimatedPromptTokens()
			if got < test.wantMin {
				t.Fatalf("estimatedPromptTokens() = %d, want >= %d (floor not applied)", got, test.wantMin)
			}
		})
	}
}

func TestEstimatedPromptTokensIgnoresMismatchedModelFloor(t *testing.T) {
	client := &toolAdmissionCaptureClient{}
	agent := New(client, nil, 16_384)
	agent.SetWorkDir(t.TempDir())
	if err := agent.RestoreContextPromptFloor(ContextPromptFloor{
		Tokens: 9_000,
		Model:  "some-other-model",
	}); err != nil {
		t.Fatal(err)
	}
	rt := &turnRuntime{
		a:                agent,
		turnModel:        client.Model(),
		turnNumCtx:       16_384,
		system:           "short prompt",
		lastPromptTokens: 0,
	}
	got := rt.estimatedPromptTokens()
	if got >= 9_000 {
		t.Fatalf("estimatedPromptTokens() = %d, should ignore mismatched model floor", got)
	}
}
