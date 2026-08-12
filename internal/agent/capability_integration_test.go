package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/capabilityadvisor"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

type capabilityCaptureClient struct {
	mu      sync.Mutex
	systems []string
}

func (client *capabilityCaptureClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	client.mu.Lock()
	client.systems = append(client.systems, options.System)
	client.mu.Unlock()
	return emit(llm.StreamChunk{Text: "done", Done: true, EvalCount: 1})
}

func (*capabilityCaptureClient) Ping() error   { return nil }
func (*capabilityCaptureClient) Model() string { return "capability-test" }
func (*capabilityCaptureClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func (client *capabilityCaptureClient) system() string {
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.systems) == 0 {
		return ""
	}
	return client.systems[len(client.systems)-1]
}

type staticCapabilityAdviser struct {
	mu       sync.Mutex
	result   capabilityadvisor.Result
	requests []capabilityadvisor.Request
}

func (advisor *staticCapabilityAdviser) Advise(_ context.Context, request capabilityadvisor.Request) capabilityadvisor.Result {
	advisor.mu.Lock()
	advisor.requests = append(advisor.requests, request)
	result := advisor.result
	advisor.mu.Unlock()
	return result
}

func (advisor *staticCapabilityAdviser) callCount() int {
	advisor.mu.Lock()
	defer advisor.mu.Unlock()
	return len(advisor.requests)
}

type capabilityOutputRecorder struct {
	outputRecorder
	routes []CapabilityRoute
}

type capabilityRegistryBackend struct {
	exposed     string
	resolutions map[string]string
	calls       int
}

func (backend *capabilityRegistryBackend) ResolveToolName(name string) (string, bool) {
	if backend.resolutions != nil {
		exposed, ok := backend.resolutions[name]
		return exposed, ok
	}
	return backend.exposed, backend.exposed != ""
}

func (backend *capabilityRegistryBackend) CallTool(context.Context, string, map[string]any) (*mcp.ToolResult, error) {
	backend.calls++
	return &mcp.ToolResult{}, nil
}

func (output *capabilityOutputRecorder) CapabilityRoute(route CapabilityRoute) {
	output.routes = append(output.routes, route)
}

func TestCapabilityHintIsTransientAndClearRouteIsAdvisory(t *testing.T) {
	client := &capabilityCaptureClient{}
	advisor := &staticCapabilityAdviser{result: capabilityadvisor.Result{
		Status: capabilityadvisor.StatusResolved, Attempted: true,
		Hint: &capabilityadvisor.Hint{
			Namespaced: "bob__bob_plan", Server: "bob", Tool: "bob_plan",
			RequiredFields: []string{"workspace"},
		},
	}}
	agent := New(client, nil, 0)
	agent.capabilityAdvisor = advisor
	agent.SetModeContext("test", BuildToolPolicy())
	agent.AddUserMessage("design the repository feature")
	output := &capabilityOutputRecorder{}
	activity := CapabilityActivity{
		ScopeID: "goal_1", Objective: "Implement authentication", Phase: "planning",
		CurrentActivity:     "Design repository changes before editing",
		DesiredOutcome:      "A verifiable implementation plan",
		AvailableInputKinds: []string{"workspace"}, NonTrivial: true,
	}

	if err := agent.RunTurnWithOptions(context.Background(), output, "turn_capability", TurnOptions{Capability: activity}); err != nil {
		t.Fatal(err)
	}
	system := client.system()
	for _, expected := range []string{
		"Host capability advisory", "advisory only", "bob__bob_plan",
		"Required argument fields: workspace", "mcphub_describe_tool", "runtime relationships",
		"mcphub_call_tool", "approval", "execution-ledger",
	} {
		if !strings.Contains(system, expected) {
			t.Fatalf("system prompt missing %q:\n%s", expected, system)
		}
	}
	wantRoute := CapabilityRoute{
		Phase: "planning", Status: CapabilityRouteResolved, Freshness: CapabilityRouteFresh,
		Server: "bob", Tool: "bob_plan", CandidateCount: 1,
	}
	if len(output.routes) != 1 || output.routes[0] != wantRoute {
		t.Fatalf("routes = %#v", output.routes)
	}
	for _, message := range agent.Messages() {
		if strings.Contains(message.Content, "Host capability advisory") || strings.Contains(message.DurableContent, "bob__bob_plan") {
			t.Fatalf("transient capability hint entered durable messages: %#v", message)
		}
	}
}

func TestCapabilityHintComposesHitspecInlineFetchWithSeparatePersistence(t *testing.T) {
	agent := New(nil, nil, 0)
	agent.SetModeContext("test", BuildToolPolicy())
	activity := CapabilityActivity{DesiredOutcome: "A readable durable Markdown artifact"}
	hint := capabilityadvisor.Hint{
		Namespaced: "hitspec__hitspec_fetch", Server: "hitspec", Tool: "hitspec_fetch",
	}

	got := agent.formatCapabilityHint(activity, hint)
	for _, expected := range []string{
		"returns bounded content inline", "does not create a workspace file", "review the inline result",
		"separately authorized host action", "fcheap_save separately", "distinct effect and approval boundaries",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("Hitspec composition hint missing %q:\n%s", expected, got)
		}
	}
}

func TestCapabilityHintDescribesHitspecCaptureAsDurableFileCheapStash(t *testing.T) {
	agent := New(nil, nil, 0)
	agent.SetModeContext("test", BuildToolPolicy())
	activity := CapabilityActivity{DesiredOutcome: "A readable durable Markdown artifact"}
	hint := capabilityadvisor.Hint{
		Namespaced: "hitspec__hitspec_capture_webpage", Server: "hitspec", Tool: "hitspec_capture_webpage",
	}

	got := agent.formatCapabilityHint(activity, hint)
	for _, expected := range []string{
		"Known Hitspec v2.18 capture contract", "persists rendered Markdown as a durable file.cheap stash",
		"compact artifact receipt", "rather than the page body", "Indexing is requested and reported separately",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("Hitspec capture hint missing %q:\n%s", expected, got)
		}
	}
	if strings.Contains(got, "Markdown file.cheap stash") {
		t.Fatalf("Hitspec capture hint retained the malformed wording:\n%s", got)
	}
}

func TestCapabilityHintRecognizesMCPHubCleanHitspecNames(t *testing.T) {
	agent := New(nil, nil, 0)
	agent.SetModeContext("test", BuildToolPolicy())
	activity := CapabilityActivity{DesiredOutcome: "Public web research without persistence"}
	hint := capabilityadvisor.Hint{
		Namespaced: "hitspec__search_web", Server: "hitspec", Tool: "search_web",
	}
	got := agent.formatCapabilityHint(activity, hint)
	if !strings.Contains(got, "returns non-persisted discovery candidates") {
		t.Fatalf("clean Hitspec search hint lost host contract:\n%s", got)
	}

	ambiguous := hint
	ambiguous.Namespaced = "hitspec__capture_webpage"
	ambiguous.Tool = "capture_webpage"
	ambiguous.Alternatives = []string{"hitspec__fetch", "hitspec__search_web"}
	ambiguous.Ambiguous = true
	got = agent.formatCapabilityHint(activity, ambiguous)
	for _, expected := range []string{
		"persists rendered Markdown", "returns bounded content inline", "non-persisted discovery candidates",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("clean ambiguous Hitspec hint missing %q:\n%s", expected, got)
		}
	}
}

func TestAmbiguousHitspecWebRouteExplainsContractsWithoutSelectingCandidate(t *testing.T) {
	activity := CapabilityActivity{DesiredOutcome: "A readable durable Markdown artifact"}
	hint := capabilityadvisor.Hint{
		Namespaced: "hitspec__hitspec_capture_webpage", Server: "hitspec", Tool: "hitspec_capture_webpage",
		Alternatives: []string{"hitspec__hitspec_fetch", "hitspec__hitspec_search_web"}, Ambiguous: true,
	}

	got := formatCapabilityHint(activity, hint, true)
	for _, expected := range []string{
		"ambiguous route", "No capability has been selected", "hitspec_capture_webpage persists rendered Markdown as a durable file.cheap stash",
		"reports indexing separately",
		"hitspec_fetch returns bounded content inline and does not persist it",
		"hitspec_search_web returns non-persisted discovery candidates, not verified evidence",
		"requested outcome is durable", "route remains ambiguous", "mcphub_search_tools",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("ambiguous Hitspec hint missing %q:\n%s", expected, got)
		}
	}
	if strings.Contains(got, "invoke it through the visible mcphub_call_tool gateway") {
		t.Fatalf("ambiguous Hitspec hint selected a candidate:\n%s", got)
	}
}

func TestAmbiguousCapabilityHintDoesNotEmitSelectedRoute(t *testing.T) {
	client := &capabilityCaptureClient{}
	advisor := &staticCapabilityAdviser{result: capabilityadvisor.Result{
		Status: capabilityadvisor.StatusAmbiguous, Attempted: true,
		Hint: &capabilityadvisor.Hint{
			Namespaced: "bob__bob_plan", Server: "bob", Tool: "bob_plan", Ambiguous: true,
			Alternatives: []string{"cortex__cortex_plan"}, RequiredFields: []string{"workspace"},
		},
	}}
	agent := New(client, nil, 0)
	agent.capabilityAdvisor = advisor
	agent.SetModeContext("test", BuildToolPolicy())
	agent.AddUserMessage("plan the repository change")
	output := &capabilityOutputRecorder{}
	activity := CapabilityActivityFromPrompt("session_1", "plan the repository change", "planning", true)

	if err := agent.RunTurnWithOptions(context.Background(), output, "turn_ambiguous", TurnOptions{Capability: activity}); err != nil {
		t.Fatal(err)
	}
	wantRoute := CapabilityRoute{
		Phase: "planning", Status: CapabilityRouteAmbiguous, Freshness: CapabilityRouteFresh,
		CandidateCount: 2,
	}
	if len(output.routes) != 1 || output.routes[0] != wantRoute {
		t.Fatalf("ambiguous recommendation did not emit bounded state: %#v", output.routes)
	}
	system := client.system()
	if !strings.Contains(system, "ambiguous route") || !strings.Contains(system, "No capability has been selected") || !strings.Contains(system, "cortex__cortex_plan") {
		t.Fatalf("ambiguous hint was not explicit:\n%s", system)
	}
}

func TestCapabilityRouteEventsExposeBoundedStatusAndFreshness(t *testing.T) {
	activity := CapabilityActivity{
		ScopeID: "session_1", Objective: "Investigate project evidence", Phase: "research",
		CurrentActivity: "Inspect available evidence", DesiredOutcome: "A source-backed finding",
		AvailableInputKinds: []string{"workspace"}, NonTrivial: true,
	}
	tests := []struct {
		name   string
		result capabilityadvisor.Result
		want   CapabilityRoute
	}{
		{
			name: "cached resolved",
			result: capabilityadvisor.Result{
				Status: capabilityadvisor.StatusResolved, Cached: true, CatalogRevision: "catalog-7",
				Hint: &capabilityadvisor.Hint{Namespaced: "bob__plan", Server: "bob", Tool: "plan"},
			},
			want: CapabilityRoute{
				Phase: "research", Status: CapabilityRouteResolved, Freshness: CapabilityRouteCached,
				Server: "bob", Tool: "plan", CandidateCount: 1, CatalogRevision: "catalog-7",
			},
		},
		{
			name: "fresh no match",
			result: capabilityadvisor.Result{
				Status: capabilityadvisor.StatusNoMatch, Attempted: true, CatalogRevision: "catalog-8",
			},
			want: CapabilityRoute{
				Phase: "research", Status: CapabilityRouteNoMatch, Freshness: CapabilityRouteFresh,
				CatalogRevision: "catalog-8",
			},
		},
		{
			name:   "pre-dispatch unavailable",
			result: capabilityadvisor.Result{Status: capabilityadvisor.StatusUnavailable},
			want: CapabilityRoute{
				Phase: "research", Status: CapabilityRouteUnavailable, Freshness: CapabilityRouteFreshnessUnknown,
			},
		},
		{
			name:   "fresh invalid",
			result: capabilityadvisor.Result{Status: capabilityadvisor.StatusInvalid, Attempted: true},
			want: CapabilityRoute{
				Phase: "research", Status: CapabilityRouteInvalid, Freshness: CapabilityRouteFresh,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			advisor := &staticCapabilityAdviser{result: test.result}
			agent := New(&capabilityCaptureClient{}, nil, 0)
			agent.capabilityAdvisor = advisor
			output := &capabilityOutputRecorder{}
			text, hint := agent.resolveTurnCapability(context.Background(), output, activity)
			if len(output.routes) != 1 || output.routes[0] != test.want {
				t.Fatalf("routes = %#v, want %#v", output.routes, test.want)
			}
			if test.want.Status != CapabilityRouteResolved {
				if hint != nil {
					t.Fatalf("non-resolved route selected a hint: %#v", hint)
				}
				// Host may still attach a phase playbook (no selected route).
				if strings.Contains(text, "MCPHub recommends") || strings.Contains(text, "Host pre-fetched tool contract") {
					t.Fatalf("non-resolved route entered selected-route context: text=%q", text)
				}
			}
		})
	}
}

func TestTrivialChatAndResolverFailureDoNotBlockProvider(t *testing.T) {
	t.Run("trivial chat skips advisor", func(t *testing.T) {
		client := &capabilityCaptureClient{}
		advisor := &staticCapabilityAdviser{result: capabilityadvisor.Result{Status: capabilityadvisor.StatusResolved}}
		agent := New(client, nil, 0)
		agent.capabilityAdvisor = advisor
		agent.AddUserMessage("hello")
		if err := agent.Run(context.Background(), &capabilityOutputRecorder{}); err != nil {
			t.Fatal(err)
		}
		if advisor.callCount() != 0 || strings.Contains(client.system(), "Host capability advisory") {
			t.Fatalf("trivial chat resolved capabilities: calls=%d", advisor.callCount())
		}
	})

	t.Run("MCP-disabled turn skips advisor", func(t *testing.T) {
		client := &capabilityCaptureClient{}
		advisor := &staticCapabilityAdviser{result: capabilityadvisor.Result{
			Status:    capabilityadvisor.StatusResolved,
			Attempted: true,
			Hint: &capabilityadvisor.Hint{
				Namespaced: "bob__bob_plan", Server: "bob", Tool: "bob_plan",
			},
		}}
		agent := New(client, nil, 0)
		agent.capabilityAdvisor = advisor
		agent.SetModeContext("test", AskToolPolicy())
		agent.AddUserMessage("investigate the repository failure and verify the cause")
		output := &capabilityOutputRecorder{}
		if err := agent.Run(context.Background(), output); err != nil {
			t.Fatal(err)
		}
		if advisor.callCount() != 0 || len(output.routes) != 0 || strings.Contains(client.system(), "Host capability advisory") {
			t.Fatalf(
				"MCP-disabled turn resolved capabilities: calls=%d routes=%#v system=%q",
				advisor.callCount(), output.routes, client.system(),
			)
		}
	})

	t.Run("resolver unavailable continues", func(t *testing.T) {
		client := &capabilityCaptureClient{}
		advisor := &staticCapabilityAdviser{result: capabilityadvisor.Result{Status: capabilityadvisor.StatusUnavailable, Attempted: true}}
		agent := New(client, nil, 0)
		agent.capabilityAdvisor = advisor
		agent.AddUserMessage("investigate the repository failure and verify the cause")
		if err := agent.Run(context.Background(), &capabilityOutputRecorder{}); err != nil {
			t.Fatal(err)
		}
		if advisor.callCount() != 1 || strings.Contains(client.system(), "MCPHub recommends") {
			t.Fatalf("unavailable resolver altered the turn: calls=%d system=%q", advisor.callCount(), client.system())
		}
	})
}

func TestFailedRecommendedGatewayRouteIsReconsidered(t *testing.T) {
	advisor := &staticCapabilityAdviser{result: capabilityadvisor.Result{
		Status: capabilityadvisor.StatusResolved, Attempted: true,
		Hint: &capabilityadvisor.Hint{
			Namespaced: "hitspec__hitspec_capture_webpage", Server: "hitspec", Tool: "hitspec_capture_webpage",
			RequiredFields: []string{"url"},
		},
	}}
	agent := New(&capabilityCaptureClient{}, nil, 0)
	agent.capabilityAdvisor = advisor
	activity := CapabilityActivityFromPrompt(
		"session_1", "capture https://example.com as Markdown", "research", true,
	)
	_, hint := agent.resolveTurnCapability(context.Background(), &capabilityOutputRecorder{}, activity)
	if hint == nil {
		t.Fatal("first recommendation was not resolved")
		return
	}
	if !agent.markCapabilityRouteFailed(activity, "mcphub__mcphub_call_tool", map[string]any{
		"server": "hitspec", "tool": "hitspec_capture_webpage", "arguments": map[string]any{"url": "https://example.com"},
	}, hint) {
		t.Fatal("exact failed recommendation was not marked for reconsideration")
	}
	otherActivity := CapabilityActivityFromPrompt(
		"session_1", "capture https://example.org as Markdown", "research", true,
	)
	if otherActivity.CurrentActivity != activity.CurrentActivity {
		t.Fatalf("test requires identical public projection: first=%#v other=%#v", activity, otherActivity)
	}
	_, _ = agent.resolveTurnCapability(context.Background(), &capabilityOutputRecorder{}, otherActivity)
	_, _ = agent.resolveTurnCapability(context.Background(), &capabilityOutputRecorder{}, activity)

	advisor.mu.Lock()
	defer advisor.mu.Unlock()
	if len(advisor.requests) != 3 || advisor.requests[0].Reconsider || advisor.requests[1].Reconsider || !advisor.requests[2].Reconsider {
		t.Fatalf("requests = %#v", advisor.requests)
	}
}

func TestFailedRouteReconsiderationRefreshesTransientContextOnce(t *testing.T) {
	advisor := &staticCapabilityAdviser{result: capabilityadvisor.Result{
		Status: capabilityadvisor.StatusNoMatch, Attempted: true, CatalogRevision: "catalog-2",
	}}
	agent := New(&capabilityCaptureClient{}, nil, 0)
	agent.capabilityAdvisor = advisor
	activity := CapabilityActivityFromPrompt("session_1", "inspect bug.mp4 and diagnose the failure", "research", true)
	failedHint := &capabilityadvisor.Hint{
		Namespaced: "vidtrace__inspect_video", Server: "vidtrace", Tool: "inspect_video",
	}
	if !agent.markCapabilityRouteFailed(activity, "mcphub__mcphub_call_tool", map[string]any{
		"server": "vidtrace", "tool": "inspect_video", "arguments": map[string]any{"path": "/private/bug.mp4"},
	}, failedHint) {
		t.Fatal("failed route was not eligible for a bounded refresh")
	}
	output := &capabilityOutputRecorder{}
	text, hint := agent.resolveTurnCapability(context.Background(), output, activity)
	if hint != nil || strings.Contains(text, "MCPHub recommends") || strings.Contains(text, "vidtrace") {
		t.Fatalf("no-match refresh retained stale advisory: text=%q hint=%#v", text, hint)
	}
	if advisor.callCount() != 1 {
		t.Fatalf("refresh calls = %d, want 1", advisor.callCount())
	}
	advisor.mu.Lock()
	request := advisor.requests[0]
	advisor.mu.Unlock()
	if !request.Reconsider {
		t.Fatalf("refresh request did not force reconsideration: %#v", request)
	}
	want := CapabilityRoute{
		Phase: activity.Phase, Status: CapabilityRouteNoMatch, Freshness: CapabilityRouteFresh,
		CatalogRevision: "catalog-2", Reconsidered: true,
	}
	if len(output.routes) != 1 || output.routes[0] != want {
		t.Fatalf("refresh route event = %#v, want %#v", output.routes, want)
	}
	got := composeCapabilityContext(text, "base context")
	if strings.Contains(got, "vidtrace") || strings.Contains(got, "MCPHub recommends") {
		t.Fatalf("stale selected route survived refresh: %q", got)
	}
	if !strings.Contains(got, "base context") {
		t.Fatalf("base context lost during refresh: %q", got)
	}
}

func TestCapabilityResolverRequiresTrustedLocalMCPHub(t *testing.T) {
	makeRegistry := func(server config.ServerConfig) (*Agent, *capabilityRegistryBackend, scopedCapabilityRegistry) {
		agent := New(nil, nil, 0)
		agent.SetTrustedLocalMCPServers([]config.ServerConfig{server})
		backend := &capabilityRegistryBackend{exposed: server.Name + "__mcphub_resolve_tool"}
		return agent, backend, scopedCapabilityRegistry{agent: agent, backend: backend}
	}

	for _, test := range []struct {
		name   string
		server config.ServerConfig
	}{
		{name: "remote impostor", server: config.ServerConfig{Name: "gateway", Command: "mcphub", Transport: "streamable-http", URL: "https://example.test/mcp"}},
		{name: "wrapper impostor", server: config.ServerConfig{Name: "gateway", Command: "/bin/sh"}},
		{name: "lookalike binary", server: config.ServerConfig{Name: "gateway", Command: "mcphub-wrapper"}},
		{
			name: "trusted gateway without resolver contract",
			server: config.ServerConfig{
				Name: "gateway", Command: "mcphub",
				Trust: &config.MCPTrustConfig{
					LocalOwner: "mcphub", Gateway: config.MCPTrustGatewayMCPHub,
					ReadOnly: []string{"mcphub_list_servers"},
				},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, backend, registry := makeRegistry(test.server)
			if exposed, ok := registry.ResolveToolName("mcphub_resolve_tool"); ok || exposed != "" {
				t.Fatalf("untrusted resolver was exposed: %q", exposed)
			}
			if backend.calls != 0 {
				t.Fatalf("untrusted backend received %d calls", backend.calls)
			}
		})
	}

	agent, backend, registry := makeRegistry(config.ServerConfig{Name: "gateway", Command: "/opt/homebrew/bin/mcphub", Transport: "stdio"})
	exposed, ok := registry.ResolveToolName("mcphub_resolve_tool")
	if !ok || exposed != "gateway__mcphub_resolve_tool" {
		t.Fatalf("trusted local resolver = %q, %v", exposed, ok)
	}
	if _, err := registry.CallTool(context.Background(), exposed, map[string]any{"query": "bounded", "max_hits": 5}); err != nil || backend.calls != 1 {
		t.Fatalf("trusted resolver call: calls=%d err=%v", backend.calls, err)
	}
	for _, test := range []struct {
		name     string
		statuses []mcp.ConnectionStatus
		want     CapabilityRoutingHostState
	}{
		{
			name: "connected trusted namespace",
			statuses: []mcp.ConnectionStatus{
				{Name: "other", Connected: true},
				{Name: "gateway", Connected: true, ToolCount: 4},
			},
			want: CapabilityRoutingHostReady,
		},
		{
			name: "retained resolver on disconnected namespace",
			statuses: []mcp.ConnectionStatus{
				{Name: "gateway", Connected: false, ToolCount: 4, LastError: "private transport failure"},
			},
			want: CapabilityRoutingHostServerUnavailable,
		},
		{
			name:     "retained resolver without live status row",
			statuses: []mcp.ConnectionStatus{{Name: "other", Connected: true}},
			want:     CapabilityRoutingHostServerUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := capabilityRoutingHostState(agent, backend, test.statuses); got != test.want {
				t.Fatalf("routing state = %v, want %v", got, test.want)
			}
		})
	}
	agent.DenyAllMCPTools()
	if exposed, ok := registry.ResolveToolName("mcphub_resolve_tool"); ok || exposed != "" {
		t.Fatalf("deny-all scope exposed resolver: %q", exposed)
	}
	if got := capabilityRoutingHostState(agent, backend, []mcp.ConnectionStatus{{Name: "gateway", Connected: true}}); got != CapabilityRoutingHostUnavailable {
		t.Fatalf("explicit deny routing state = %v, want unavailable", got)
	}
}

func TestCapabilityResolverFiltersTrustAndScopeBeforeAmbiguity(t *testing.T) {
	const resolver = "mcphub_resolve_tool"
	backend := &capabilityRegistryBackend{resolutions: map[string]string{
		"trusted__" + resolver:   "trusted__" + resolver,
		"untrusted__" + resolver: "untrusted__" + resolver,
	}}
	agent := New(nil, nil, 0)
	agent.SetTrustedLocalMCPServers([]config.ServerConfig{
		{Name: "trusted", Command: "/opt/homebrew/bin/mcphub", Transport: "stdio"},
		{Name: "untrusted", Command: "mcphub-wrapper", Transport: "stdio"},
	})
	registry := scopedCapabilityRegistry{agent: agent, backend: backend}

	if exposed, ok := registry.ResolveToolName(resolver); !ok || exposed != "trusted__"+resolver {
		t.Fatalf("trusted resolver with untrusted name collision = %q, %v", exposed, ok)
	}

	backend.resolutions["second__"+resolver] = "second__" + resolver
	agent.SetTrustedLocalMCPServers([]config.ServerConfig{
		{Name: "trusted", Command: "/opt/homebrew/bin/mcphub", Transport: "stdio"},
		{Name: "second", Command: "/usr/local/bin/mcphub", Transport: "stdio"},
	})
	if exposed, ok := registry.ResolveToolName(resolver); ok || exposed != "" {
		t.Fatalf("multiple eligible trusted resolvers did not fail closed: %q, %v", exposed, ok)
	}

	agent.SetMCPServerScope([]string{"second"})
	if exposed, ok := registry.ResolveToolName(resolver); !ok || exposed != "second__"+resolver {
		t.Fatalf("scoped trusted resolver = %q, %v", exposed, ok)
	}
}

func TestCapabilityRouteOutcomeFailureIsSemantic(t *testing.T) {
	for _, domain := range []ecosystem.DomainState{
		ecosystem.DomainFailed, ecosystem.DomainBlocked, ecosystem.DomainConflict, ecosystem.DomainDrift,
	} {
		if !capabilityRouteOutcomeFailed(ecosystem.ToolProjection{Transport: ecosystem.TransportSucceeded, Domain: domain}, false) {
			t.Fatalf("domain %q did not invalidate the recommendation", domain)
		}
	}
	for _, domain := range []ecosystem.DomainState{ecosystem.DomainSucceeded, ecosystem.DomainUnknown, ecosystem.DomainAttention} {
		if capabilityRouteOutcomeFailed(ecosystem.ToolProjection{Transport: ecosystem.TransportSucceeded, Domain: domain}, false) {
			t.Fatalf("domain %q invalidated a non-failed route", domain)
		}
	}
}

func TestCapabilityActivityPrivacyAndClassification(t *testing.T) {
	activity := CapabilityActivityFromPrompt(
		"session_1", "capture https://example.com/page?token=private as Markdown", "working", true,
	)
	if !activity.NonTrivial || activity.Phase != "research" || !containsAnyWord(strings.Join(activity.AvailableInputKinds, ","), "url", "workspace") {
		t.Fatalf("activity = %#v", activity)
	}
	request := activity.request(false)
	if request.Activity.Objective == "" {
		t.Fatal("safe URL activity was rejected")
	}
	projected := strings.Join([]string{
		request.Activity.Objective, request.Activity.CurrentActivity, request.Activity.DesiredOutcome,
	}, " ")
	for _, private := range []string{"example.com", "token", "private"} {
		if strings.Contains(strings.ToLower(projected), private) {
			t.Fatalf("host projection retained private prompt text %q: %q", private, projected)
		}
	}

	raw := "review this CSV: Alice,alice@example.com,123-45-6789"
	rawActivity := CapabilityActivityFromPrompt("session_1", raw, "working", true)
	if !rawActivity.NonTrivial {
		t.Fatal("non-trivial raw-data review was not classified")
	}
	rawProjection := strings.Join([]string{rawActivity.Objective, rawActivity.CurrentActivity, rawActivity.DesiredOutcome}, " ")
	for _, private := range []string{"alice", "example.com", "123-45-6789", "csv:"} {
		if strings.Contains(strings.ToLower(rawProjection), private) {
			t.Fatalf("raw one-line data crossed the resolver projection: %q", rawProjection)
		}
	}

	multiline := CapabilityActivityFromPrompt(
		"session_1", "Implement contextual MCP routing.\nKeep raw task details private.\nVerify the selected tool contract.", "working", true,
	)
	if !multiline.NonTrivial {
		t.Fatalf("multiline task did not receive host-side capability routing: %#v", multiline)
	}
	multilineProjection := multiline.Objective + multiline.CurrentActivity + multiline.DesiredOutcome
	for _, private := range []string{"contextual", "raw task details", "selected tool contract"} {
		if strings.Contains(strings.ToLower(multilineProjection), private) {
			t.Fatalf("multiline raw wording crossed resolver projection: %q", multilineProjection)
		}
	}

	if got := CapabilityActivityFromPrompt("session_1", "I was wondering whether cats generally enjoy sunny windows in the afternoon", "working", true); got.NonTrivial {
		t.Fatalf("long casual chat triggered capability resolution: %#v", got)
	}

	for _, unsafe := range []string{
		"hello\nraw file body", "API_KEY=private", `{"raw":"document"}`, "Authorization: Bearer private-token",
	} {
		if got := CapabilityActivityFromPrompt("session_1", unsafe, "working", true); got.NonTrivial || got.Objective != "" {
			t.Fatalf("unsafe prompt was projected: %#v", got)
		}
	}
}

func TestCapabilityActivityProjectsExternalFileFacetsWithoutPaths(t *testing.T) {
	tests := []struct {
		prompt string
		kinds  []string
		secret []string
	}{
		{
			prompt: "analyze /Users/alice/incident/private-bug.mp4 and diagnose the visible failure",
			kinds:  []string{"external_file", "video"},
			secret: []string{"alice", "incident", "private-bug", "/users/"},
		},
		{
			prompt: "inspect screenshot-customer-42.jpg for the rendering defect",
			kinds:  []string{"external_file", "image"},
			secret: []string{"screenshot-customer-42", ".jpg"},
		},
		{
			prompt: "analyze interview-private.m4a and verify the audio issue",
			kinds:  []string{"external_file", "audio"},
			secret: []string{"interview-private", ".m4a"},
		},
		{
			prompt: "review /private/contracts/acquisition-secret.pdf and identify the document issue",
			kinds:  []string{"external_file", "document"},
			secret: []string{"contracts", "acquisition-secret", "/private/"},
		},
	}
	for _, test := range tests {
		t.Run(test.kinds[1], func(t *testing.T) {
			activity := CapabilityActivityFromPrompt("session_external", test.prompt, "research", false)
			if !activity.NonTrivial {
				t.Fatalf("external file was not classified: %#v", activity)
			}
			for _, kind := range test.kinds {
				if !containsAnyWord(strings.Join(activity.AvailableInputKinds, ","), kind) {
					t.Fatalf("input kinds %v missing %q", activity.AvailableInputKinds, kind)
				}
			}
			request := activity.request(false)
			projected := strings.ToLower(strings.Join([]string{
				request.Activity.Objective, request.Activity.CurrentActivity, request.Activity.DesiredOutcome,
			}, " "))
			for _, private := range test.secret {
				if strings.Contains(projected, private) {
					t.Fatalf("private path/name fragment %q entered resolver projection %q", private, projected)
				}
			}
		})
	}

	codePath := CapabilityActivityFromPrompt("session_external", "fix internal/agent/private_handler.go", "implementation", true)
	if containsAnyWord(strings.Join(codePath.AvailableInputKinds, ","), "external_file") {
		t.Fatalf("workspace source path was mislabeled as external: %#v", codePath.AvailableInputKinds)
	}
	for _, unsafe := range []string{
		`{"path":"/private/incident.mp4"}`,
		"analyze /private/incident.mp4 with API_KEY=private",
	} {
		if got := CapabilityActivityFromPrompt("session_external", unsafe, "research", false); got.NonTrivial {
			t.Fatalf("unsafe external-file prompt crossed fail-closed boundary: %#v", got)
		}
	}
}

func TestCapabilityActivityTracksPrivateMaterialChangesWithoutSendingThem(t *testing.T) {
	first := CapabilityActivityFromPrompt("session_1", "implement authentication", "implementation", true)
	second := CapabilityActivityFromPrompt("session_1", "implement logging", "implementation", true)
	cosmetic := CapabilityActivityFromPrompt("session_1", "IMPLEMENT, authentication!!!", "implementation", true)
	if !first.NonTrivial || !second.NonTrivial || !cosmetic.NonTrivial {
		t.Fatalf("activities were not classified: first=%#v second=%#v cosmetic=%#v", first, second, cosmetic)
	}
	if first.request(false).CacheDiscriminator == second.request(false).CacheDiscriminator {
		t.Fatal("materially different ordinary tasks share a cache discriminator")
	}
	if first.request(false).CacheDiscriminator != cosmetic.request(false).CacheDiscriminator {
		t.Fatal("case and punctuation-only changes did not reuse the cache discriminator")
	}
	if !containsAnyWord(strings.Join(first.IntentTags, ","), "security", "references") ||
		!containsAnyWord(strings.Join(second.IntentTags, ","), "observability", "telemetry") {
		t.Fatalf("materially different tasks lost allowlisted routing signal: first=%v second=%v", first.IntentTags, second.IntentTags)
	}
	if strings.Join(first.IntentTags, "\x00") == strings.Join(second.IntentTags, "\x00") {
		t.Fatalf("materially different tasks share intent facets: %v", first.IntentTags)
	}
	for _, request := range []capabilityadvisor.Request{first.request(false), second.request(false)} {
		projection := request.Activity.Objective + request.Activity.CurrentActivity + request.Activity.DesiredOutcome + strings.Join(request.Activity.IntentTags, " ")
		if strings.Contains(strings.ToLower(projection), "authentication") || strings.Contains(strings.ToLower(projection), "logging") {
			t.Fatalf("private task wording entered resolver activity: %q", projection)
		}
	}
}

func TestCapabilityActivityFingerprintIncludesMaterialPromptTail(t *testing.T) {
	prefix := "implement repository support " + strings.Repeat("with bounded workspace context ", 24)
	first := CapabilityActivityFromPrompt("session_1", prefix+"for authentication", "implementation", true)
	second := CapabilityActivityFromPrompt("session_1", prefix+"for audit logging", "implementation", true)
	if !first.NonTrivial || !second.NonTrivial {
		t.Fatalf("long activities were not classified: first=%#v second=%#v", first, second)
	}
	if first.request(false).CacheDiscriminator == second.request(false).CacheDiscriminator {
		t.Fatal("material prompt changes beyond the resolver projection bound reused the cache discriminator")
	}
	for _, request := range []capabilityadvisor.Request{first.request(false), second.request(false)} {
		projection := request.Activity.Objective + request.Activity.CurrentActivity + request.Activity.DesiredOutcome
		for _, private := range []string{"authentication", "audit logging", "bounded workspace context"} {
			if strings.Contains(strings.ToLower(projection), private) {
				t.Fatalf("private prompt tail entered resolver activity: %q", projection)
			}
		}
	}
}

func TestDurableGoalCapabilityHonorsHostPhaseAndActivityChanges(t *testing.T) {
	for _, phase := range []string{"research", "planning", "implementation", "verification"} {
		activity := DurableGoalCapabilityActivity("goal_1", "Implement authentication", phase, "cortex_"+phase)
		if !activity.NonTrivial || activity.Phase != phase {
			t.Fatalf("phase %q projected as %#v", phase, activity)
		}
	}
	first := DurableGoalCapabilityActivity("goal_1", "Implement authentication", "implementation", "cortex_edit")
	second := DurableGoalCapabilityActivity("goal_1", "Implement authentication", "implementation", "cortex_test")
	if first.request(false).CacheDiscriminator == second.request(false).CacheDiscriminator {
		t.Fatal("same-phase durable action transition reused the cache discriminator")
	}
}

func TestHeadlessCapabilityRouteStaysOnStderr(t *testing.T) {
	var stdout, stderr strings.Builder
	output := newHeadlessOutput(&stdout, &stderr)
	output.CapabilityRoute(CapabilityRoute{
		Phase: "research", Status: CapabilityRouteResolved, Freshness: CapabilityRouteFresh,
		Server: "hitspec", Tool: "hitspec_capture_webpage", CandidateCount: 1,
	})
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "advisory") || !strings.Contains(stderr.String(), "hitspec__hitspec_capture_webpage") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
