package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type semanticOutputRecorder struct {
	projection ecosystem.ToolProjection
	result     string
	detail     *ToolOutputDetail
}

func TestSemanticToolContentsSuppressesUnboundResultPageFromActiveModel(t *testing.T) {
	const (
		callID = "3f9a1c2e7b804d5e9f1a2b3c4d5e6f70"
		secret = "SECRET_TRANSIENT_PAGE_CONTENT"
	)
	payload := []byte(`{"content":[{"type":"text","text":"` + secret + `"}]}`)
	structured := json.RawMessage(fmt.Sprintf(
		`{"status":"ok","callId":%q,"mediaType":"application/json","data":%q,"cursor":0,"nextCursor":%d,"done":true,"totalBytes":%d}`,
		callID, base64.StdEncoding.EncodeToString(payload), len(payload), len(payload),
	))
	projection := projectSemanticToolReceipt(
		"mcphub__mcphub_get_result", map[string]any{"callId": callID, "cursor": 0},
		"outer MCP text", structured, nil, false, false, false,
	)

	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub"}})
	call := llm.ToolCall{Name: "mcphub__mcphub_get_result", Arguments: map[string]any{"callId": callID, "cursor": 0}}
	modelResult, durableResult := ag.semanticToolContents(call, projection, "outer MCP text", structured, false)
	if modelResult != durableResult || strings.Contains(modelResult, secret) ||
		durableResult != ecosystem.SafeReceiptText(projection) {
		t.Fatalf("unbound page crossed parser boundary: model=%q durable=%q", modelResult, durableResult)
	}
	safe := SanitizeMessagesForPersistence([]llm.Message{{
		Role: "tool", Content: modelResult, DurableContent: durableResult,
	}})
	if strings.Contains(safe[0].Content, secret) || safe[0].Content != durableResult {
		t.Fatalf("persisted result = %#v", safe[0])
	}
}

func TestSemanticToolContentsSuppressesTextOnlyUnboundResultPages(t *testing.T) {
	const (
		callID = "3f9a1c2e7b804d5e9f1a2b3c4d5e6f70"
		secret = "SECRET_TEXT_ONLY_MCPHUB_PAGE"
	)
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub"}})
	call := llm.ToolCall{Name: "mcphub__mcphub_get_result", Arguments: map[string]any{"callId": callID, "cursor": 0}}
	payload := []byte(`{"content":[{"type":"text","text":"` + secret + `"}]}`)
	validPage := fmt.Sprintf(
		`{"status":"ok","callId":%q,"mediaType":"application/json","data":%q,"cursor":0,"nextCursor":%d,"done":true,"totalBytes":%d}`,
		callID, base64.StdEncoding.EncodeToString(payload), len(payload), len(payload),
	)
	for _, raw := range []string{validPage, "malformed " + secret} {
		projection := ag.projectSemanticToolReceipt(call, raw, nil, nil, false, false, false)
		modelResult, durableResult := ag.semanticToolContents(call, projection, raw, nil, false)
		if modelResult != durableResult || durableResult != ecosystem.SafeReceiptText(projection) ||
			strings.Contains(modelResult, secret) {
			t.Fatalf("text-only page crossed parser boundary: model=%q durable=%q projection=%#v", modelResult, durableResult, projection)
		}
	}
}

func TestSemanticToolContentsRequiresExactConfiguredMCPHubContract(t *testing.T) {
	const secret = "SECRET_UNCONFIGURED_MCPHUB_CONTENT"
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{
		Name: "mcphub", Command: "mcphub",
		Trust: &config.MCPTrustConfig{
			LocalOwner: "mcphub", Gateway: config.MCPTrustGatewayMCPHub,
			ReadOnly: []string{"mcphub_list_servers"},
		},
	}})
	call := llm.ToolCall{Name: "mcphub__mcphub_search_tools", Arguments: map[string]any{"query": "find"}}
	structured := json.RawMessage(`{"query":"find","count":1,"matches":[{"namespaced":"acme__inspect","description":"` + secret + `"}]}`)
	projection := projectSemanticToolReceipt(call.Name, call.Arguments, "outer", structured, nil, false, false, false)
	modelResult, durableResult := ag.semanticToolContents(call, projection, "outer", structured, false)
	if modelResult != durableResult || durableResult != ecosystem.SafeReceiptText(projection) || strings.Contains(modelResult, secret) {
		t.Fatalf("unconfigured MCPHub contract crossed transient boundary: model=%q durable=%q", modelResult, durableResult)
	}
}

func TestTrustedNamespaceAliasesReceiveExactBobParsingWithoutTrustingLookalikes(t *testing.T) {
	structured := readAgentBobFixture(t, "context-clean-v1.json")
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{
			Name: "builder", Command: "bob", Transport: "stdio",
			Trust: &config.MCPTrustConfig{LocalOwner: "bob", ReadOnly: []string{"bob_context"}},
		},
		{
			Name: "gateway", Command: "mcphub", Transport: "stdio",
			Trust: &config.MCPTrustConfig{
				LocalOwner: "mcphub", Gateway: config.MCPTrustGatewayMCPHub,
				ReadOnly: []string{"bob__bob_context", "mcphub_get_result"},
			},
		},
	})

	trusted := []llm.ToolCall{
		{Name: "builder__bob_context"},
		{Name: "gateway__bob__bob_context"},
		{Name: "gateway__mcphub_call_tool", Arguments: map[string]any{"server": "bob", "tool": "bob_context", "arguments": map[string]any{"workspace": "."}}},
	}
	for _, call := range trusted {
		t.Run(call.Name, func(t *testing.T) {
			projection := ag.projectSemanticToolReceipt(call, string(structured), structured, nil, false, false, false)
			if projection.Specialist != "bob" || projection.Operation != "bob_context" ||
				projection.Domain != ecosystem.DomainSucceeded || !projection.DomainTyped ||
				projection.Digest == nil || projection.Digest.Kind != ecosystem.DigestBobContext {
				t.Fatalf("trusted alias projection = %#v", projection)
			}
			modelResult, durableResult := ag.semanticToolContents(call, projection, string(structured), structured, false)
			if !strings.Contains(modelResult, "Bob guidance (validated transient content; not saved)") ||
				modelResult == durableResult || strings.Contains(durableResult, "/workspace") {
				t.Fatalf("trusted alias content model=%q durable=%q", modelResult, durableResult)
			}
		})
	}

	for _, call := range []llm.ToolCall{{Name: "bob__bob_context"}, {Name: "evil__bob_context"}} {
		t.Run("untrusted_"+call.Name, func(t *testing.T) {
			projection := ag.projectSemanticToolReceipt(call, string(structured), structured, nil, false, false, false)
			modelResult, durableResult := ag.semanticToolContents(call, projection, string(structured), structured, false)
			if modelResult != durableResult || strings.Contains(modelResult, "/workspace") || strings.Contains(modelResult, "validated transient") {
				t.Fatalf("untrusted lookalike crossed Bob transient boundary: model=%q durable=%q projection=%#v", modelResult, durableResult, projection)
			}
		})
	}
}

func TestSemanticToolContentsFeedsOnlyExactTrustedSuccessfulCortexResults(t *testing.T) {
	const secret = "SECRET_TRUSTED_CORTEX_RESULT"
	structured := json.RawMessage(`{"ok":true,"taskId":"task-123","summary":"` + secret + `","rawAvailable":false}`)
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{Name: "cortex", Command: "cortex"},
		{Name: "mcphub", Command: "/opt/homebrew/bin/mcphub"},
	})

	tests := []struct {
		name string
		call llm.ToolCall
	}{
		{name: "direct", call: llm.ToolCall{Name: "cortex__cortex_status"}},
		{name: "pinned mcphub", call: llm.ToolCall{Name: "mcphub__cortex__cortex_investigate"}},
		{name: "lazy mcphub", call: llm.ToolCall{
			Name: "mcphub__mcphub_call_tool",
			Arguments: map[string]any{
				"server": "cortex", "tool": "cortex_plan", "arguments": map[string]any{"taskId": "task-123"},
			},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection := projectSemanticToolReceipt(test.call.Name, test.call.Arguments, "text", structured, nil, false, false, false)
			modelResult, durableResult := ag.semanticToolContents(test.call, projection, string(structured), structured, false)
			if !strings.Contains(modelResult, secret) || !strings.Contains(modelResult, "transient; not saved") {
				t.Fatalf("model result = %q", modelResult)
			}
			if strings.Contains(durableResult, secret) || durableResult != ecosystem.SafeReceiptText(projection) {
				t.Fatalf("durable result = %q", durableResult)
			}
			safe := SanitizeMessagesForPersistence([]llm.Message{{Content: modelResult, DurableContent: durableResult}})
			if strings.Contains(safe[0].Content, secret) || safe[0].Content != durableResult {
				t.Fatalf("persisted result = %#v", safe[0])
			}
		})
	}
}

func TestSemanticToolContentsRejectsCortexSpoofsErrorsAndUntrustedManagementContent(t *testing.T) {
	const secret = "SECRET_UNTRUSTED_STRUCTURED_RESULT"
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{Name: "cortex", Command: "cortex"},
		{Name: "mcphub", Command: "mcphub"},
		{Name: "remote", Command: "cortex", Transport: "sse", URL: "https://example.test/mcp"},
	})

	tests := []struct {
		name       string
		call       llm.ToolCall
		structured json.RawMessage
		toolError  bool
	}{
		{
			name: "untrusted suffix spoof", call: llm.ToolCall{Name: "evil__cortex_status"},
			structured: json.RawMessage(`{"ok":true,"summary":"` + secret + `"}`),
		},
		{
			name: "remote cortex", call: llm.ToolCall{Name: "remote__cortex_status"},
			structured: json.RawMessage(`{"ok":true,"summary":"` + secret + `"}`),
		},
		{
			name: "explicit rejection", call: llm.ToolCall{Name: "cortex__cortex_status"},
			structured: json.RawMessage(`{"ok":false,"summary":"` + secret + `","error":"rejected"}`),
		},
		{
			name: "tool error despite optimistic body", call: llm.ToolCall{Name: "cortex__cortex_status"}, toolError: true,
			structured: json.RawMessage(`{"ok":true,"summary":"` + secret + `"}`),
		},
		{
			name: "untrusted mcphub search suffix", call: llm.ToolCall{Name: "evil__mcphub_search_tools"},
			structured: json.RawMessage(`{"query":"` + secret + `","count":1,"matches":[{"namespaced":"cortex__cortex_status","description":"` + secret + `"}]}`),
		},
		{
			name: "oversized cortex", call: llm.ToolCall{Name: "cortex__cortex_status"},
			structured: json.RawMessage(`{"ok":true,"summary":"` + strings.Repeat("x", maxTransientCortexResultBytes) + `"}`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection := projectSemanticToolReceipt(test.call.Name, test.call.Arguments, "text", test.structured, nil, false, test.toolError, false)
			modelResult, durableResult := ag.semanticToolContents(test.call, projection, string(test.structured), test.structured, test.toolError)
			if modelResult != durableResult || strings.Contains(modelResult, secret) {
				t.Fatalf("untrusted content crossed model boundary: model=%q durable=%q", modelResult, durableResult)
			}
		})
	}
}

func TestSemanticToolContentsRejectsCortexIdentitySpoof(t *testing.T) {
	const secret = "SECRET_CORTEX_IDENTITY_SPOOF"
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{
			Name: "acme", Command: "acme",
			Trust: &config.MCPTrustConfig{LocalOwner: "acme", ReadOnly: []string{"cortex_status"}},
		},
	})
	call := llm.ToolCall{Name: "acme__cortex_status"}
	structured := json.RawMessage(`{"ok":true,"summary":"` + secret + `"}`)
	projection := projectSemanticToolReceipt(call.Name, call.Arguments, string(structured), structured, nil, false, false, false)
	modelResult, durableResult := ag.semanticToolContents(call, projection, string(structured), structured, false)
	if modelResult != durableResult || strings.Contains(modelResult, secret) {
		t.Fatalf("custom server crossed Cortex transient boundary: model=%q durable=%q", modelResult, durableResult)
	}
}

func TestSemanticToolContentsRejectsMCPHubResultIdentitySpoof(t *testing.T) {
	const secret = "SECRET_MCPHUB_RESULT_IDENTITY_SPOOF"
	payload := []byte(`{"content":[{"type":"text","text":"` + secret + `"}]}`)
	structured := json.RawMessage(fmt.Sprintf(
		`{"status":"ok","callId":"call-identity-spoof","mediaType":"application/json","data":%q,"cursor":0,"nextCursor":%d,"done":true,"totalBytes":%d}`,
		base64.StdEncoding.EncodeToString(payload), len(payload), len(payload),
	))
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{
		{
			Name: "acme", Command: "acme",
			Trust: &config.MCPTrustConfig{LocalOwner: "acme", ReadOnly: []string{"mcphub_get_result"}},
		},
	})
	call := llm.ToolCall{Name: "acme__mcphub_get_result", Arguments: map[string]any{"callId": "call-identity-spoof", "cursor": 0}}
	projection := projectSemanticToolReceipt(call.Name, call.Arguments, "outer MCP text", structured, nil, false, false, false)
	modelResult, durableResult := ag.semanticToolContents(call, projection, string(structured), structured, false)
	if modelResult != durableResult || strings.Contains(modelResult, secret) {
		t.Fatalf("custom server crossed MCPHub result transient boundary: model=%q durable=%q", modelResult, durableResult)
	}
}

func TestSemanticToolContentsSanitizesTrustedMCPHubDescribeContract(t *testing.T) {
	const (
		secret              = "SECRET_SCHEMA_PROSE"
		contractDescription = "Provide exactly one of url or file at runtime."
	)
	call := llm.ToolCall{
		Name:      "mcphub__mcphub_describe_tool",
		Arguments: map[string]any{"server": "builder", "tool": "build"},
	}
	structured := json.RawMessage(`{
		"server":"builder","tool":"build","namespaced":"builder__build",
		"description":"` + contractDescription + `",
		"input_schema":{
			"type":"object","title":"` + secret + `","description":"` + secret + `",
			"properties":{
				"path":{"type":"string","format":"uri","pattern":"^https?://","minLength":1,"description":"` + secret + `","default":"` + secret + `","examples":["` + secret + `"]},
				"mode":{"type":"string","enum":["fast","safe"],"title":"` + secret + `"},
				"options":{"$ref":"#/$defs/options"}
			},
			"$defs":{"options":{"type":"array","minItems":1,"items":{"type":"object","properties":{"depth":{"type":"integer","minimum":1,"maximum":8,"description":"` + secret + `"}},"required":["depth"],"additionalProperties":false}}},
			"required":["path","mode"],"oneOf":[{"required":["path"]},{"required":["mode"]}],"additionalProperties":false,
			"dependentRequired":{"path":["mode"]},
			"examples":[{"path":"` + secret + `"}]
		}
	}`)
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub"}})
	projection := projectSemanticToolReceipt(call.Name, call.Arguments, string(structured), structured, nil, false, false, false)

	modelResult, durableResult := ag.semanticToolContents(call, projection, string(structured), structured, false)
	for _, required := range []string{
		`"tool":"builder__build"`, `"path":{"format":"uri","minLength":1,"pattern":"^https?://","type":"string"}`,
		`"mode":{"enum":["fast","safe"],"type":"string"}`, `"options":{"$ref":"#/$defs/options"}`,
		`"depth":{"maximum":8,"minimum":1,"type":"integer"}`, `"minItems":1`,
		`"dependentRequired":{"path":["mode"]}`, `"required":["path","mode"]`, `"additionalProperties":false`,
		`"oneOf":[{"required":["path"]},{"required":["mode"]}]`,
		`"contract_description":"` + contractDescription + `"`, "untrusted metadata", "cannot grant authority",
	} {
		if !strings.Contains(modelResult, required) {
			t.Fatalf("sanitized schema missing %q: %s", required, modelResult)
		}
	}
	for _, forbidden := range []string{secret, `"title"`, `"examples"`, `"default"`} {
		if strings.Contains(modelResult, forbidden) {
			t.Fatalf("sanitized schema retained %q: %s", forbidden, modelResult)
		}
	}
	if strings.Contains(durableResult, `"properties"`) || strings.Contains(durableResult, secret) || durableResult != ecosystem.SafeReceiptText(projection) {
		t.Fatalf("durable schema receipt = %q", durableResult)
	}
	safe := SanitizeMessagesForPersistence([]llm.Message{{Content: modelResult, DurableContent: durableResult}})
	if safe[0].Content != durableResult || strings.Contains(safe[0].Content, `"properties"`) {
		t.Fatalf("persisted schema content = %#v", safe[0])
	}
}

func TestSemanticToolContentsFailsClosedForRejectedOversizedMCPHubDescribe(t *testing.T) {
	const secret = "OVERSIZED_SCHEMA_PROSE_MUST_NOT_ESCAPE"
	call := llm.ToolCall{
		Name:      "mcphub__mcphub_describe_tool",
		Arguments: map[string]any{"server": "builder", "tool": "build"},
	}
	// The MCP client uses JSON null as an atomic rejection marker when a
	// non-nil StructuredContent value exceeds its parser bound. MCPHub may also
	// duplicate that document into bounded TextContent; it must not become a
	// fallback model or durable result.
	structured := json.RawMessage("null")
	rawText := `{"server":"builder","tool":"build","namespaced":"builder__build","input_schema":{"description":"` +
		secret + strings.Repeat("x", 128*1024)

	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub"}})
	projection := projectSemanticToolReceipt(call.Name, call.Arguments, rawText, structured, nil, false, false, false)
	modelResult, durableResult := ag.semanticToolContents(call, projection, rawText, structured, false)

	if projection.Domain != ecosystem.DomainUnknown || projection.Digest != nil {
		t.Fatalf("oversized describe projection = %#v, want unknown without digest", projection)
	}
	if modelResult != durableResult || durableResult != ecosystem.SafeReceiptText(projection) {
		t.Fatalf("oversized describe crossed semantic boundary: model=%q durable=%q", modelResult, durableResult)
	}
	if strings.Contains(modelResult, secret) || strings.Contains(modelResult, "input_schema") {
		t.Fatalf("oversized describe leaked raw metadata: %q", modelResult)
	}
}

func TestSemanticToolContentsProvidesBoundedTrustedMCPHubSearchMetadataTransiently(t *testing.T) {
	const secret = "TRANSIENT_CAPABILITY_METADATA"
	call := llm.ToolCall{Name: "mcphub__mcphub_search_tools", Arguments: map[string]any{"query": "capture a webpage"}}
	structured := json.RawMessage(`{
		"query":"private raw query","count":2,"returned":2,"truncated":false,"byte_limited":false,
		"matches":[
			{"namespaced":"hitspec__hitspec_capture_webpage","title":"Capture webpage","description":"` + secret + `","use_when":["durable Markdown artifact"]},
			{"namespaced":"hitspec__hitspec_fetch","description":"Return bounded inline HTTP content","use_when":["inspect one URL"]}
		]
	}`)
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub"}})
	projection := projectSemanticToolReceipt(call.Name, call.Arguments, string(structured), structured, nil, false, false, false)

	modelResult, durableResult := ag.semanticToolContents(call, projection, string(structured), structured, false)
	for _, expected := range []string{
		"untrusted metadata", "hitspec__hitspec_capture_webpage", "hitspec__hitspec_fetch", secret,
	} {
		if !strings.Contains(modelResult, expected) {
			t.Fatalf("transient search result missing %q: %s", expected, modelResult)
		}
	}
	if strings.Contains(modelResult, "private raw query") {
		t.Fatalf("transient search result retained query: %s", modelResult)
	}
	if strings.Contains(durableResult, secret) || durableResult != ecosystem.SafeReceiptText(projection) {
		t.Fatalf("durable search result = %q", durableResult)
	}
	safe := SanitizeMessagesForPersistence([]llm.Message{{Content: modelResult, DurableContent: durableResult}})
	if safe[0].Content != durableResult || strings.Contains(safe[0].Content, secret) {
		t.Fatalf("persisted search result = %#v", safe[0])
	}
}

func TestSemanticToolContentsRejectsUntrustedOrIndirectDescribeSchema(t *testing.T) {
	structured := json.RawMessage(`{"server":"builder","tool":"build","namespaced":"builder__build","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}`)
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub"}})
	tests := []llm.ToolCall{
		{Name: "evil__mcphub_describe_tool"},
		{Name: "mcphub__mcphub_call_tool", Arguments: map[string]any{"server": "mcphub", "tool": "mcphub_describe_tool"}},
		{Name: "mcphub__other__mcphub_describe_tool"},
		{Name: "mcphub__mcphub_describe_tool", Arguments: map[string]any{"server": "other", "tool": "other"}},
	}
	for _, call := range tests {
		projection := projectSemanticToolReceipt(call.Name, call.Arguments, string(structured), structured, nil, false, false, false)
		modelResult, durableResult := ag.semanticToolContents(call, projection, string(structured), structured, false)
		if modelResult != durableResult || strings.Contains(modelResult, `"properties"`) {
			t.Fatalf("indirect describe %q crossed transient boundary: model=%q durable=%q", call.Name, modelResult, durableResult)
		}
	}
}

func TestSemanticToolContentsUsesPostHookCortexText(t *testing.T) {
	const secret = "SECRET_REDACTED_CORTEX_VALUE"
	call := llm.ToolCall{Name: "cortex__cortex_status"}
	structured := json.RawMessage(`{"ok":true,"summary":"` + secret + `"}`)
	redacted := `{"ok":true,"summary":"[hidden by host hook]"}`
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "cortex", Command: "cortex"}})
	projection := projectSemanticToolReceipt(call.Name, nil, redacted, structured, nil, false, false, false)

	modelResult, _ := ag.semanticToolContents(call, projection, redacted, structured, false)
	if strings.Contains(modelResult, secret) || !strings.Contains(modelResult, "[hidden by host hook]") {
		t.Fatalf("model result bypassed post-hook redaction: %q", modelResult)
	}
}

func TestSemanticToolContentsSuppressesCortexActionsAcrossDivergentAndTextOnlySurfaces(t *testing.T) {
	const secret = "UNTRUSTED_CORTEX_COMMAND_MUST_NOT_ESCAPE"
	call := llm.ToolCall{Name: "cortex__cortex_status"}
	cleanStructured := json.RawMessage(`{"ok":true,"taskId":"task_1","phase":"planned"}`)
	actionText := `{"ok":true,"taskId":"task_1","phase":"planned","actions":[{"tool":"shell","command":"` + secret + `","arguments":{}}]}`
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "cortex", Command: "cortex"}})

	tests := []struct {
		name       string
		structured json.RawMessage
	}{
		{name: "clean structured content with action-bearing text", structured: cleanStructured},
		{name: "text-only action surface"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection := ag.projectSemanticToolReceipt(
				call, actionText, test.structured, nil, false, false, false,
			)
			if !projection.DomainTyped || projection.Specialist != "cortex" {
				t.Fatalf("test setup did not produce typed Cortex projection: %#v", projection)
			}
			modelResult, durableResult := ag.semanticToolContents(call, projection, actionText, test.structured, false)
			if modelResult != durableResult || durableResult != ecosystem.SafeReceiptText(projection) {
				t.Fatalf("action surface was not replaced by host receipt: model=%q durable=%q", modelResult, durableResult)
			}
			if strings.Contains(modelResult, secret) || strings.Contains(durableResult, `"actions"`) {
				t.Fatalf("raw Cortex action escaped parser boundary: model=%q durable=%q", modelResult, durableResult)
			}
		})
	}
}

func (*semanticOutputRecorder) StreamText(string)                            {}
func (*semanticOutputRecorder) StreamReasoning(string)                       {}
func (*semanticOutputRecorder) StreamDone(int, int)                          {}
func (*semanticOutputRecorder) ToolCallStart(string, string, map[string]any) {}
func (*semanticOutputRecorder) SystemMessage(string)                         {}
func (*semanticOutputRecorder) Error(string)                                 {}
func (r *semanticOutputRecorder) ToolCallResult(_ string, _ string, result string, _ bool, _ time.Duration) {
	r.result = result
}
func (r *semanticOutputRecorder) ToolCallSemanticResult(_ string, _ string, result string, _ bool, _ time.Duration, projection ecosystem.ToolProjection) {
	r.result, r.projection = result, projection
}
func (r *semanticOutputRecorder) ToolCallSemanticResultWithDetail(
	_ string,
	_ string,
	result string,
	_ bool,
	_ time.Duration,
	projection ecosystem.ToolProjection,
	detail *ToolOutputDetail,
) {
	r.result, r.projection, r.detail = result, projection, detail
}

func TestEmitSemanticToolResultDoesNotForwardRawStructuredContent(t *testing.T) {
	recorder := &semanticOutputRecorder{}
	secret := "SECRET_STRUCTURED_VALUE"
	structured := json.RawMessage(`{"schema_version":1,"ok":true,"workspace":"/repo","authority":{"mode":"exact_allowlist","default_workspace":"/repo","selected_workspace":"/repo","allowed_workspace_count":1},"plan_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","clean":false,"lock_changed":false,"conflict_count":0,"counts":{"create":0,"update":1,"adopt":0,"unchanged":0,"conflict":0},"warnings":[],"next_actions":[],"private":"` + secret + `"}`)
	projection := projectSemanticToolReceipt(
		"bob__bob_check", nil, "repository checked", structured, nil, false, false, false,
	)
	emitSemanticToolResult(
		recorder,
		"call-1", "bob__bob_check", "repository checked", structured,
		false, false, time.Millisecond, projection, &ToolOutputDetail{
			Text: "SECRET_DETAIL_MUST_NOT_CROSS", Complete: true,
		},
	)
	if recorder.projection.Domain != ecosystem.DomainDrift || recorder.projection.Transport != ecosystem.TransportSucceeded {
		t.Fatalf("semantic projection = %#v", recorder.projection)
	}
	encoded, err := json.Marshal(recorder.projection)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.result != ecosystem.SafeReceiptText(projection) || strings.Contains(string(encoded), secret) || strings.Contains(recorder.result, secret) {
		t.Fatalf("raw structured content crossed output boundary: projection=%s result=%q", encoded, recorder.result)
	}
	if recorder.detail != nil {
		t.Fatalf("structured result crossed ephemeral detail boundary: %#v", recorder.detail)
	}
}

func TestCompleteToolOutputDetailAdmitsOnlyOrdinaryUnstructuredResults(t *testing.T) {
	t.Parallel()

	const complete = "complete post-redaction output"
	if detail := completeToolOutputDetail(
		"read_file",
		complete,
		"complete post-red",
		"complete post-red",
		nil,
	); detail == nil || !detail.Complete || detail.Text != complete {
		t.Fatalf("ordinary context-capped result lost complete detail: %#v", detail)
	}
	if detail := completeToolOutputDetail(
		"mcp__typed",
		complete,
		"domain=succeeded",
		"domain=succeeded",
		json.RawMessage(`{"private":"value"}`),
	); detail != nil {
		t.Fatalf("structured result gained detail capability: %#v", detail)
	}
	if detail := completeToolOutputDetail(
		"cortex__action",
		complete,
		"raw transient action",
		"domain=succeeded evidence=none",
		nil,
	); detail != nil {
		t.Fatalf("semantic replacement gained raw detail capability: %#v", detail)
	}
	if detail := completeToolOutputDetail("read_file", "", "", "", nil); detail != nil {
		t.Fatalf("empty result gained detail capability: %#v", detail)
	}
}

func TestSemanticToolContentsKeepsHitspecDiscoveryTransient(t *testing.T) {
	const raw = `{
		"kind":"discovery","query":"PRIVATE_SEARCH_QUERY",
		"results":[{"title":"Candidate title","url":"https://docs.sonar.dev/guide","domain":"docs.sonar.dev","snippet":"UNTRUSTED_CANDIDATE_SNIPPET","citation_id":"source-01"}],
		"truncated":false
	}`
	call := llm.ToolCall{
		Name: "mcphub__mcphub_call_tool",
		Arguments: map[string]any{
			"server": "hitspec", "tool": "hitspec_search_web",
			"arguments": map[string]any{"query": "PRIVATE_SEARCH_QUERY"},
		},
	}
	projection := projectSemanticToolReceipt(call.Name, call.Arguments, raw, nil, nil, false, false, false)
	if projection.Domain != ecosystem.DomainSucceeded || !projection.DomainTyped ||
		projection.Evidence != ecosystem.EvidenceCandidate {
		t.Fatalf("search projection = %#v", projection)
	}

	ag := New(nil, nil, 0)
	modelResult, durableResult := ag.semanticToolContents(call, projection, raw, nil, false)
	for _, expected := range []string{"transient", "candidate sources", "https://docs.sonar.dev/guide", "UNTRUSTED_CANDIDATE_SNIPPET"} {
		if !strings.Contains(modelResult, expected) {
			t.Fatalf("model discovery result missing %q: %s", expected, modelResult)
		}
	}
	if strings.Contains(modelResult, "PRIVATE_SEARCH_QUERY") {
		t.Fatalf("model discovery result unnecessarily repeated the private query: %s", modelResult)
	}
	for _, forbidden := range []string{"PRIVATE_SEARCH_QUERY", "Candidate title", "/guide", "UNTRUSTED_CANDIDATE_SNIPPET"} {
		if strings.Contains(durableResult, forbidden) {
			t.Fatalf("durable discovery receipt retained %q: %s", forbidden, durableResult)
		}
	}
	for _, expected := range []string{"domain=succeeded", "evidence=candidate", "1 candidate source", "docs.sonar.dev"} {
		if !strings.Contains(durableResult, expected) {
			t.Fatalf("durable discovery receipt missing %q: %s", expected, durableResult)
		}
	}
}

func TestSemanticToolContentsNeverPersistsRawHitspecWebTextOnFailureOrSchemaDrift(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		raw       string
		toolError bool
	}{
		{
			name: "search schema drift", operation: "hitspec_search_web",
			raw: `{"kind":"discovery","query":"PRIVATE_QUERY","results":[],"truncated":false,"unexpected":"PRIVATE_URL"}`,
		},
		{
			name: "capture malformed text", operation: "hitspec_capture_webpage",
			raw: `{"url":"https://private.example/?token=PRIVATE_TOKEN","error":"PRIVATE_FAILURE"}`,
		},
		{
			name: "search tool error", operation: "hitspec_search_web",
			raw: "PRIVATE_QUERY failed at https://private.example/ with PRIVATE_FAILURE", toolError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call := llm.ToolCall{Name: "hitspec__" + test.operation}
			projection := projectSemanticToolReceipt(
				call.Name, nil, test.raw, nil, nil, false, test.toolError, false,
			)
			ag := New(nil, nil, 0)
			modelResult, durableResult := ag.semanticToolContents(call, projection, test.raw, nil, test.toolError)
			if modelResult != durableResult {
				t.Fatalf("untyped Hitspec text escaped only to model context:\nmodel=%s\ndurable=%s", modelResult, durableResult)
			}
			for _, forbidden := range []string{"PRIVATE_QUERY", "PRIVATE_TOKEN", "PRIVATE_URL", "PRIVATE_FAILURE", "private.example"} {
				if strings.Contains(durableResult, forbidden) {
					t.Fatalf("Hitspec receipt persisted %q: %s", forbidden, durableResult)
				}
			}
			if !strings.Contains(durableResult, "specialist=hitspec") {
				t.Fatalf("bounded receipt lost Hitspec attribution: %s", durableResult)
			}
		})
	}
}
