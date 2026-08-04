package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	registrypkg "github.com/abdul-hamid-achik/sonar/internal/mcp"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

const (
	mcpPreflightHelperEnv   = "SONAR_TEST_MCP_PREFLIGHT_HELPER"
	mcpPreflightMarkerEnv   = "SONAR_TEST_MCP_PREFLIGHT_MARKER"
	mcpPreflightPaddingEnv  = "SONAR_TEST_MCP_PREFLIGHT_PADDING"
	mcpPreflightServerName  = "schema"
	mcpPreflightSuccessTool = "schema__investigate"
	mcpPreflightFailingTool = "schema__fail"
)

type mcpPreflightArgs struct {
	TaskID   string `json:"taskId"`
	Question string `json:"question"`
}

func TestMain(m *testing.M) {
	if os.Getenv(mcpPreflightHelperEnv) == "1" {
		runMCPPreflightHelper()
		return
	}
	os.Exit(m.Run())
}

func runMCPPreflightHelper() {
	server := mcp.NewServer(&mcp.Implementation{Name: "schema-test", Version: "1.0.0"}, nil)
	recordCall := func(name string) error {
		marker := os.Getenv(mcpPreflightMarkerEnv)
		file, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		if _, err := file.WriteString(name + "\n"); err != nil {
			_ = file.Close()
			return err
		}
		return file.Close()
	}
	mcp.AddTool(server, &mcp.Tool{Name: "investigate"}, func(_ context.Context, _ *mcp.CallToolRequest, _ mcpPreflightArgs) (*mcp.CallToolResult, any, error) {
		if err := recordCall("investigate"); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "investigated"}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "fail"}, func(_ context.Context, _ *mcp.CallToolRequest, _ mcpPreflightArgs) (*mcp.CallToolResult, any, error) {
		if err := recordCall("fail"); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "backend rejected after dispatch"}},
			IsError: true,
		}, nil, nil
	})
	padding, _ := strconv.Atoi(os.Getenv(mcpPreflightPaddingEnv))
	if padding > 0 {
		metaTools := []string{
			"mcphub_resolve_tool",
			"mcphub_search_tools",
			"mcphub_describe_tool",
			"mcphub_call_tool",
			"mcphub_get_result",
			"mcphub_poll_result",
		}
		for _, name := range metaTools {
			toolName := name
			mcp.AddTool(server, &mcp.Tool{
				Name:        toolName,
				Description: "Bounded MCPHub gateway operation used by the large-catalog context-admission fixture.",
			}, func(_ context.Context, _ *mcp.CallToolRequest, _ mcpPreflightArgs) (*mcp.CallToolResult, any, error) {
				if err := recordCall(toolName); err != nil {
					return nil, nil, err
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
			})
		}
		for i := 0; i < padding; i++ {
			toolName := fmt.Sprintf("catalog_%02d", i)
			mcp.AddTool(server, &mcp.Tool{
				Name: toolName,
				Description: strings.Repeat(
					"Specialist contract detail retained only to reproduce a realistically large MCP catalog. ",
					12,
				),
			}, func(_ context.Context, _ *mcp.CallToolRequest, _ mcpPreflightArgs) (*mcp.CallToolResult, any, error) {
				if err := recordCall(toolName); err != nil {
					return nil, nil, err
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
			})
		}
	}
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
}

func TestPreflightMCPToolArgumentsValidatesExactSchema(t *testing.T) {
	def := llm.ToolDef{Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"taskId":    map[string]any{"type": "string"},
			"question":  map[string]any{"type": "string"},
			"limit":     map[string]any{"type": "integer", "minimum": 1},
			"patterned": map[string]any{"type": "string", "pattern": "^[a-z]+$"},
			"choice":    map[string]any{"enum": []any{"safe"}},
		},
		"required":             []any{"taskId", "question"},
		"additionalProperties": false,
	}}
	tests := []struct {
		name       string
		args       map[string]any
		wantError  string
		wantDetail string
		forbidden  string
	}{
		{name: "valid", args: map[string]any{"taskId": "task-1", "question": "why", "limit": 2}},
		{name: "required", args: map[string]any{"question": "why"}, wantError: "input schema", wantDetail: "taskId"},
		{name: "type", args: map[string]any{"taskId": "task-1", "question": "why", "limit": "do-not-persist-this"}, wantError: "input schema", forbidden: "do-not-persist-this"},
		{name: "pattern", args: map[string]any{"taskId": "task-1", "question": "why", "patterned": "PATTERN-secret-must-not-escape"}, wantError: "input schema", forbidden: "PATTERN-secret-must-not-escape"},
		{name: "enum", args: map[string]any{"taskId": "task-1", "question": "why", "choice": "ENUM-secret-must-not-escape"}, wantError: "input schema", forbidden: "ENUM-secret-must-not-escape"},
		{name: "additionalProperties", args: map[string]any{"taskId": "task-1", "question": "why", "surprise": true}, wantError: "input schema"},
		{name: "full schema constraint", args: map[string]any{"taskId": "task-1", "question": "why", "limit": 0}, wantError: "input schema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := preflightMCPToolArguments(def, tt.args)
			if tt.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) || tt.wantDetail != "" && !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("preflight error = %v, want %q and %q", err, tt.wantError, tt.wantDetail)
			}
			if tt.forbidden != "" && strings.Contains(err.Error(), tt.forbidden) {
				t.Fatalf("preflight error leaked rejected argument: %v", err)
			}
		})
	}
}

func TestPreflightMCPToolArgumentsRejectsInvalidOrExternalSchema(t *testing.T) {
	tests := []llm.ToolDef{
		{Parameters: map[string]any{"type": 7}},
		{Parameters: map[string]any{"$ref": "https://example.invalid/schema.json"}},
	}
	for _, def := range tests {
		if err := preflightMCPToolArguments(def, map[string]any{}); err == nil || !strings.Contains(err.Error(), "input schema") {
			t.Fatalf("preflight error = %v, want invalid schema rejection", err)
		}
	}
}

func TestMCPInvalidArgumentsFailBeforeApprovalAndDispatchThenCanBeCorrected(t *testing.T) {
	const secret = "mcp-preflight-secret-must-not-escape"
	marker := filepath.Join(t.TempDir(), "calls")
	registry := newMCPPreflightRegistry(t, marker)
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "invalid", Name: mcpPreflightSuccessTool, Arguments: map[string]any{
			"taskId": "task-1", "question": map[string]any{"secret": secret},
		}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "corrected", Name: mcpPreflightSuccessTool, Arguments: map[string]any{
			"taskId": "task-1", "question": "what happened?",
		}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, _ := newLedgerAgent(t, client, registry, ledger)
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	var approvals atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		approvals.Add(1)
		request.Response <- permission.AllowOnce()
	})
	out := &outputRecorder{}
	if err := ag.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}

	if approvals.Load() != 1 {
		t.Fatalf("approval prompts = %d, want only the corrected call", approvals.Load())
	}
	if client.calls != 3 {
		t.Fatalf("provider calls = %d, want invalid call, correction, and final answer", client.calls)
	}
	wantEvents := []executionpkg.EventType{
		executionpkg.EventRequested,
		executionpkg.EventFailed,
		executionpkg.EventRequested,
		executionpkg.EventApprovalRequested,
		executionpkg.EventApproved,
		executionpkg.EventStarted,
		executionpkg.EventCompleted,
	}
	if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "investigate\n" {
		t.Fatalf("backend calls = %q, want only corrected call", data)
	}
	preflightResult := strings.Join(out.toolResults, "\n")
	for _, want := range []string{"failed preflight", "input schema"} {
		if !strings.Contains(preflightResult, want) {
			t.Fatalf("tool results = %q, missing %q", preflightResult, want)
		}
	}
	if strings.Contains(preflightResult, secret) {
		t.Fatalf("tool output leaked rejected MCP argument: %q", preflightResult)
	}
	for _, event := range ledger.snapshot() {
		if strings.Contains(event.ResultReceipt, secret) || strings.Contains(event.Detail, secret) {
			t.Fatalf("durable event leaked rejected MCP argument: %#v", event)
		}
	}
	for _, message := range ag.Messages() {
		if message.Role == "tool" && (strings.Contains(message.Content, secret) || strings.Contains(message.DurableContent, secret)) {
			t.Fatalf("provider-bound tool message leaked rejected MCP argument: %#v", message)
		}
	}
}

func TestMCPPostDispatchAnsweredErrorTerminatesAsFailed(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "calls")
	registry := newMCPPreflightRegistry(t, marker)
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{
			ToolCalls: []llm.ToolCall{{ID: "failure", Name: mcpPreflightFailingTool, Arguments: map[string]any{
				"taskId": "task-1", "question": "fail after dispatch",
			}}},
			Done: true,
		}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, _ := newLedgerAgent(t, client, registry, ledger)
	if err := ag.Run(context.Background(), &outputRecorder{}); err != nil {
		t.Fatalf("answered MCP error stranded the session: %v", err)
	}
	wantEvents := []executionpkg.EventType{
		executionpkg.EventRequested,
		executionpkg.EventApproved,
		executionpkg.EventStarted,
		executionpkg.EventFailed,
	}
	if got := executionEventTypes(ledger.snapshot()); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %v, want %v", got, wantEvents)
	}
	for _, message := range ag.Messages() {
		if message.Role == "tool" && strings.Contains(message.Content, "OUTCOME UNKNOWN") {
			t.Fatalf("answered error wore the unverifiable-outcome framing: %#v", message)
		}
	}
	data, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "fail\n" {
		t.Fatalf("backend calls = %q, want dispatched failing call", data)
	}
}

func newMCPPreflightRegistry(t *testing.T, marker string) *registrypkg.Registry {
	return newMCPPreflightRegistryWithPadding(t, marker, 0)
}

func newMCPPreflightRegistryWithPadding(t *testing.T, marker string, padding int) *registrypkg.Registry {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	registry := registrypkg.NewRegistry()
	t.Cleanup(registry.Close)
	count, err := registry.ConnectServer(context.Background(), config.ServerConfig{
		Name:      mcpPreflightServerName,
		Command:   executable,
		Transport: "stdio",
		Env: []string{
			mcpPreflightHelperEnv + "=1",
			mcpPreflightMarkerEnv + "=" + marker,
			mcpPreflightPaddingEnv + "=" + strconv.Itoa(padding),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCount := 2
	if padding > 0 {
		wantCount += padding + 6
	}
	if count != wantCount {
		t.Fatalf("discovered tools = %d, want %d", count, wantCount)
	}
	return registry
}

func TestPreflightUnknownMCPToolSuggestsUniqueNamespacedName(t *testing.T) {
	registry := newMCPPreflightRegistry(t, filepath.Join(t.TempDir(), "calls"))
	ag := New(nil, registry, 4096)
	t.Cleanup(ag.Close)

	err := ag.preflightToolCall(executionpkg.KindMCP, llm.ToolCall{Name: "investigate"})
	if err == nil {
		t.Fatal("expected bare remote name to fail preflight")
	}
	wantSuggestion := `unknown MCP tool "investigate"; use exact namespaced name "schema__investigate"`
	if err.Error() != wantSuggestion {
		t.Fatalf("preflight error = %q, want %q", err.Error(), wantSuggestion)
	}

	// Do not rewrite identity: exact namespaced name must still be required.
	if err := ag.preflightToolCall(executionpkg.KindMCP, llm.ToolCall{
		Name: "schema__investigate",
		Arguments: map[string]any{
			"taskId": "task-1", "question": "why",
		},
	}); err != nil {
		t.Fatalf("exact namespaced tool failed preflight: %v", err)
	}

	err = ag.preflightToolCall(executionpkg.KindMCP, llm.ToolCall{Name: "totally_missing_tool"})
	if err == nil {
		t.Fatal("expected completely unknown tool to fail")
	}
	if !strings.Contains(err.Error(), `unknown MCP tool "totally_missing_tool"`) {
		t.Fatalf("preflight error = %v, want plain unknown", err)
	}
	if strings.Contains(err.Error(), "use exact namespaced name") {
		t.Fatalf("unresolvable tool should not invent a suggestion: %v", err)
	}
}

func TestPreflightLazyMCPHubCallRequiresExactTarget(t *testing.T) {
	// Padding installs schema__mcphub_call_tool with a taskId/question object
	// schema. Host trust treats that namespace as MCPHub so the lazy-target
	// gate runs after schema validation — without rewriting the call name.
	registry := newMCPPreflightRegistryWithPadding(t, filepath.Join(t.TempDir(), "calls"), 1)
	ag := New(nil, registry, 4096)
	t.Cleanup(ag.Close)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{
		Name:    mcpPreflightServerName,
		Command: "/opt/homebrew/bin/mcphub",
	}})

	callName := mcpPreflightServerName + "__mcphub_call_tool"
	// Schema-valid args still lack an exact lazy MCPHub target (server+tool).
	err := ag.preflightToolCall(executionpkg.KindMCP, llm.ToolCall{
		Name:      callName,
		Arguments: map[string]any{"taskId": "task-1", "question": "route me"},
	})
	if err == nil {
		t.Fatal("expected incomplete lazy call_tool target to fail preflight")
	}
	if !strings.Contains(err.Error(), "mcphub_call_tool requires exact server and tool") {
		t.Fatalf("preflight error = %v, want exact target requirement", err)
	}

	// Without gateway trust the host lazy gate does not fire; schema alone
	// accepts these args (the padding helper has no server/tool fields).
	untrusted := New(nil, registry, 4096)
	t.Cleanup(untrusted.Close)
	if err := untrusted.preflightToolCall(executionpkg.KindMCP, llm.ToolCall{
		Name:      callName,
		Arguments: map[string]any{"taskId": "task-1", "question": "route me"},
	}); err != nil {
		t.Fatalf("untrusted call_tool spelling should only face schema: %v", err)
	}
}

func TestLazyMCPHubMissingTargetTerminalIsFailedNotOutcomeUnknown(t *testing.T) {
	ag := New(nil, nil, 4096)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{Name: "mcphub", Command: "/opt/homebrew/bin/mcphub"},
	})
	call := llm.ToolCall{
		Name:      "mcphub__mcphub_call_tool",
		Arguments: map[string]any{"tool": "cortex_status"},
	}
	projection := projectSemanticToolReceipt(
		call.Name, call.Arguments, "need server and tool", nil, nil, false, true, false,
	)
	if !ag.executionOutcomeAnswered(call, executionpkg.KindMCP, executionpkg.EffectUnknown, "need server and tool", false, projection) {
		t.Fatal("incomplete lazy call_tool error should be an answered gateway failure")
	}
	terminal := terminalExecutionEventType(executionpkg.EffectUnknown, true, true, nil)
	if terminal != executionpkg.EventFailed {
		t.Fatalf("terminal = %s, want failed (not outcome_unknown)", terminal)
	}
}

func TestToolAvailabilitySeparatesLocalAndConnectedMCPTools(t *testing.T) {
	registry := newMCPPreflightRegistry(t, filepath.Join(t.TempDir(), "calls.log"))
	ag := New(&capabilityCaptureClient{}, registry, 4096)
	t.Cleanup(ag.Close)

	availability := ag.ToolAvailability()
	if availability.Local <= 0 {
		t.Fatalf("local tool availability = %#v, want built-ins", availability)
	}
	if availability.MCPConnected != 2 || availability.MCPRetained != 2 {
		t.Fatalf("connected MCP availability = %#v, want 2 connected/retained", availability)
	}
	if got := ag.ToolCount(); got != availability.Local+availability.MCPRetained {
		t.Fatalf("visible ToolCount = %d, want local + retained = %d", got, availability.Local+availability.MCPRetained)
	}

	ag.SetMCPServerScope([]string{"outside-profile"})
	restricted := ag.ToolAvailability()
	if restricted.MCPConnected != 0 || restricted.MCPRetained != 0 || restricted.Local != availability.Local {
		t.Fatalf("profile-scoped availability = %#v, want local-only %#v", restricted, availability)
	}
}
