package agent

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const agentMCPHubRawSecret = "RAW_MCPHUB_RESULT_MUST_NOT_PERSIST"

func TestAgentBindsExactTrustedPinnedAndLazyMCPHubStoredResults(t *testing.T) {
	structured := readAgentBobFixture(t, "context-clean-v1.json")
	payload := agentStoredCallToolResult(t, structured, false)

	tests := []struct {
		name string
		call llm.ToolCall
	}{
		{
			name: "pinned route",
			call: llm.ToolCall{Name: "mcphub__bob__bob_context"},
		},
		{
			name: "lazy route",
			call: llm.ToolCall{
				Name: "mcphub__mcphub_call_tool",
				Arguments: map[string]any{
					"server": "bob", "tool": "bob_context",
					"arguments": map[string]any{"workspace": "."},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ag := newTrustedMCPHubAssemblyAgent()
			const callID = "bob-context-paged-result"
			stored := agentMCPHubStoredDocument(t, callID, "bob", "bob_context", len(payload))
			projection := projectSemanticToolReceipt(test.call.Name, test.call.Arguments, "", stored, nil, false, false, false)
			observation := ag.projectMCPHubResultAssembly(test.call, projection, stored, false)
			if !observation.Bound || observation.Complete || observation.Transient != "" {
				t.Fatalf("stored observation = %#v", observation)
			}
			if observation.Projection.Digest == nil || observation.Projection.Digest.Kind != ecosystem.DigestMCPHubStored {
				t.Fatalf("stored projection = %#v", observation.Projection)
			}

			final := feedAgentMCPHubPages(t, ag, callID, payload, 911)
			if !final.Bound || !final.Complete {
				t.Fatalf("final observation = %#v", final)
			}
			got := final.Projection
			if got.Specialist != "bob" || got.Operation != "bob_context" || got.Role != ecosystem.RoleBuild ||
				got.Domain != ecosystem.DomainSucceeded || !got.DomainTyped || got.Evidence != ecosystem.EvidenceNone ||
				got.Digest == nil || got.Digest.Kind != ecosystem.DigestBobContext {
				t.Fatalf("final projection = %#v", got)
			}
			if got.Route.Gateway != "mcphub" || got.Route.Server != "bob" || got.Route.Tool != "bob_context" ||
				got.Route.CallID != callID || !got.Route.Lazy {
				t.Fatalf("final route = %#v", got.Route)
			}
			if !strings.Contains(final.Transient, "Bob guidance (validated transient content; not saved)") ||
				!strings.Contains(final.Transient, `"contract":"bob_context.v1"`) ||
				!strings.Contains(final.Transient, "go-agent-tool") {
				t.Fatalf("complete transient = %q", final.Transient)
			}
			if final.Workspace != "/workspace" {
				t.Fatalf("complete transient workspace = %q", final.Workspace)
			}
			for _, forbidden := range []string{agentMCPHubRawSecret, "/workspace", "create_patterns", "argv"} {
				if strings.Contains(final.Transient, forbidden) {
					t.Fatalf("complete transient contains %q: %q", forbidden, final.Transient)
				}
				if strings.Contains(ecosystem.SafeReceiptText(got), forbidden) {
					t.Fatalf("durable projection contains %q: %q", forbidden, ecosystem.SafeReceiptText(got))
				}
			}
		})
	}
}

func TestAgentDoesNotBindUntrustedMCPHubLookalike(t *testing.T) {
	ag := newTrustedMCPHubAssemblyAgent()
	payload := agentStoredCallToolResult(t, readAgentBobFixture(t, "context-clean-v1.json"), false)
	const callID = "untrusted-bob-context"
	stored := agentMCPHubStoredDocument(t, callID, "bob", "bob_context", len(payload))
	call := llm.ToolCall{Name: "evil__bob__bob_context"}
	projection := projectSemanticToolReceipt(call.Name, call.Arguments, "", stored, nil, false, false, false)
	observation := ag.projectMCPHubResultAssembly(call, projection, stored, false)
	if observation.Bound || observation.Complete || observation.Transient != "" {
		t.Fatalf("untrusted stored call bound an assembly: %#v", observation)
	}

	final := feedAgentMCPHubPages(t, ag, callID, payload, len(payload))
	if final.Bound || final.Complete || final.Transient != "" {
		t.Fatalf("unbound pages gained parser authority: %#v", final)
	}
	if final.Projection.Specialist != "mcphub" || final.Projection.Digest == nil ||
		final.Projection.Digest.Kind != ecosystem.DigestMCPHubPage {
		t.Fatalf("unbound page was reclassified: %#v", final.Projection)
	}
}

func TestAgentBindsStoredReceiptEvenWhenMCPHubPropagatesIsError(t *testing.T) {
	ag := newTrustedMCPHubAssemblyAgent()
	structured := readAgentBobFixture(t, "playbook-missing-input-v1.json")
	payload := agentStoredCallToolResult(t, structured, true)
	const callID = "bob-playbook-error-result"
	stored := agentMCPHubStoredDocument(t, callID, "bob", "bob_playbook", len(payload))
	call := llm.ToolCall{
		Name: "mcphub__mcphub_call_tool",
		Arguments: map[string]any{
			"server": "bob", "tool": "bob_playbook",
			"arguments": map[string]any{"operation": "plan", "id": "add-cli-command"},
		},
	}
	projection := projectSemanticToolReceipt(call.Name, call.Arguments, "", stored, nil, false, true, false)
	observation := ag.projectMCPHubResultAssembly(call, projection, stored, true)
	if !observation.Bound || observation.Complete {
		t.Fatalf("stored IsError receipt was not bound: %#v", observation)
	}

	final := feedAgentMCPHubPages(t, ag, callID, payload, 733)
	if !final.Bound || !final.Complete || final.Transient != "" {
		t.Fatalf("completed error observation = %#v", final)
	}
	got := final.Projection
	if got.Specialist != "bob" || got.Operation != "bob_playbook" || got.Domain != ecosystem.DomainBlocked ||
		!got.DomainTyped || got.Evidence != ecosystem.EvidenceNone || got.Digest == nil ||
		got.Digest.Kind != ecosystem.DigestBobFailure || got.Digest.Target != "input_invalid" {
		t.Fatalf("completed Bob error projection = %#v", got)
	}
}

func TestAgentBoundPartialPageSignalsRawSuppression(t *testing.T) {
	ag := newTrustedMCPHubAssemblyAgent()
	payload := agentStoredCallToolResult(t, readAgentBobFixture(t, "context-clean-v1.json"), false)
	const callID = "bob-context-partial"
	stored := agentMCPHubStoredDocument(t, callID, "bob", "bob_context", len(payload))
	storedCall := llm.ToolCall{Name: "mcphub__bob__bob_context"}
	storedProjection := projectSemanticToolReceipt(storedCall.Name, nil, "", stored, nil, false, false, false)
	if observation := ag.projectMCPHubResultAssembly(storedCall, storedProjection, stored, false); !observation.Bound {
		t.Fatalf("stored receipt was not bound: %#v", observation)
	}

	next := min(256, len(payload)-1)
	page := agentMCPHubPageDocument(t, callID, payload, 0, next)
	pageCall := llm.ToolCall{Name: "mcphub__mcphub_get_result", Arguments: map[string]any{"callId": callID, "cursor": 0}}
	projection := projectSemanticToolReceipt(pageCall.Name, pageCall.Arguments, string(page), page, nil, false, false, false)
	observation := ag.projectMCPHubResultAssembly(pageCall, projection, page, false)
	if !observation.Bound || observation.Complete || observation.Transient != "" {
		t.Fatalf("partial observation = %#v", observation)
	}
	if observation.Projection.Domain != ecosystem.DomainAttention || !observation.Projection.DomainTyped {
		t.Fatalf("partial projection = %#v", observation.Projection)
	}
	durable := ecosystem.SafeReceiptText(observation.Projection)
	if strings.Contains(durable, agentMCPHubRawSecret) || strings.Contains(durable, base64.StdEncoding.EncodeToString([]byte(agentMCPHubRawSecret))) {
		t.Fatalf("partial durable receipt leaked payload: %q", durable)
	}
}

func TestAgentTrustedMCPHubAliasMatchesPinnedAndLazyPagedBobRoutes(t *testing.T) {
	structured := readAgentBobFixture(t, "context-clean-v1.json")
	payload := agentStoredCallToolResult(t, structured, false)
	for _, storedCall := range []llm.ToolCall{
		{Name: "gateway__bob__bob_context"},
		{Name: "gateway__mcphub_call_tool", Arguments: map[string]any{"server": "bob", "tool": "bob_context", "arguments": map[string]any{"workspace": "."}}},
	} {
		t.Run(storedCall.Name, func(t *testing.T) {
			ag := newTrustedMCPHubAliasAssemblyAgent()
			const callID = "alias-bob-context-paged"
			stored := agentMCPHubStoredDocument(t, callID, "bob", "bob_context", len(payload))
			projection := ag.projectSemanticToolReceipt(storedCall, "", stored, nil, false, false, false)
			if projection.Route.Gateway != "gateway" || projection.Digest == nil || projection.Digest.Kind != ecosystem.DigestMCPHubStored {
				t.Fatalf("alias stored projection = %#v", projection)
			}
			if observation := ag.projectMCPHubResultAssembly(storedCall, projection, stored, false); !observation.Bound {
				t.Fatalf("alias stored result was not bound: %#v", observation)
			}
			final := feedAgentMCPHubPagesViaNamespace(t, ag, "gateway", callID, payload, 777)
			if !final.Bound || !final.Complete || final.Projection.Specialist != "bob" ||
				final.Projection.Domain != ecosystem.DomainSucceeded || final.Projection.Route.Gateway != "gateway" ||
				final.Projection.Digest == nil || final.Projection.Digest.Kind != ecosystem.DigestBobContext ||
				!strings.Contains(final.Transient, `"contract":"bob_context.v1"`) {
				t.Fatalf("alias completed observation = %#v transient=%q", final, final.Transient)
			}
		})
	}
}

func TestAgentRejectsAnsweredErroredAndMalformedBoundPages(t *testing.T) {
	payload := agentStoredCallToolResult(t, readAgentBobFixture(t, "context-clean-v1.json"), false)
	for _, test := range []struct {
		name      string
		page      func(*testing.T, string, []byte, int) json.RawMessage
		toolError bool
	}{
		{
			name: "outer IsError cannot promote optimistic final page", toolError: true,
			page: func(t *testing.T, callID string, payload []byte, cursor int) json.RawMessage {
				return agentMCPHubPageDocument(t, callID, payload, cursor, len(payload))
			},
		},
		{
			name: "malformed answered page tears down chain",
			page: func(t *testing.T, callID string, payload []byte, cursor int) json.RawMessage {
				return agentMarshalDocument(t, map[string]any{
					"status": "ok", "callId": callID, "mediaType": "application/json", "data": "%%%",
					"cursor": cursor, "nextCursor": len(payload), "done": true, "totalBytes": len(payload),
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ag := newTrustedMCPHubAssemblyAgent()
			const callID = "rejected-bob-page"
			storedCall := llm.ToolCall{Name: "mcphub__bob__bob_context"}
			stored := agentMCPHubStoredDocument(t, callID, "bob", "bob_context", len(payload))
			storedProjection := ag.projectSemanticToolReceipt(storedCall, "", stored, nil, false, false, false)
			if observation := ag.projectMCPHubResultAssembly(storedCall, storedProjection, stored, false); !observation.Bound {
				t.Fatalf("stored result was not bound: %#v", observation)
			}

			cursor := min(256, len(payload)-1)
			partial := agentMCPHubPageDocument(t, callID, payload, 0, cursor)
			partialCall := llm.ToolCall{Name: "mcphub__mcphub_get_result", Arguments: map[string]any{"callId": callID, "cursor": 0}}
			partialProjection := ag.projectSemanticToolReceipt(partialCall, "", partial, nil, false, false, false)
			if observation := ag.projectMCPHubResultAssembly(partialCall, partialProjection, partial, false); !observation.Bound {
				t.Fatalf("partial page was not bound: %#v", observation)
			}

			rejectedPage := test.page(t, callID, payload, cursor)
			rejectedCall := llm.ToolCall{Name: "mcphub__mcphub_get_result", Arguments: map[string]any{"callId": callID, "cursor": cursor}}
			rejectedProjection := ag.projectSemanticToolReceipt(rejectedCall, "", rejectedPage, nil, false, test.toolError, false)
			rejected := ag.projectMCPHubResultAssembly(rejectedCall, rejectedProjection, rejectedPage, test.toolError)
			if !rejected.Bound || rejected.Complete || rejected.Projection.Domain != ecosystem.DomainFailed || !rejected.Projection.DomainTyped {
				t.Fatalf("answered rejection did not fail and discard exact chain: %#v", rejected)
			}

			corrected := agentMCPHubPageDocument(t, callID, payload, cursor, len(payload))
			correctedProjection := ag.projectSemanticToolReceipt(rejectedCall, "", corrected, nil, false, false, false)
			unbound := ag.projectMCPHubResultAssembly(rejectedCall, correctedProjection, corrected, false)
			if unbound.Bound || unbound.Complete || unbound.Transient != "" {
				t.Fatalf("discarded chain resumed after rejection: %#v", unbound)
			}
			modelResult, durableResult := ag.semanticToolContents(rejectedCall, correctedProjection, string(corrected), corrected, false)
			if modelResult != durableResult || strings.Contains(modelResult, agentMCPHubRawSecret) {
				t.Fatalf("unbound corrected page escaped: model=%q durable=%q", modelResult, durableResult)
			}
		})
	}
}

func newTrustedMCPHubAssemblyAgent() *Agent {
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{Name: "mcphub", Command: "mcphub"}})
	return ag
}

func newTrustedMCPHubAliasAssemblyAgent() *Agent {
	ag := New(nil, nil, 8192)
	ag.SetTrustedLocalMCPServers([]config.ServerConfig{{
		Name: "gateway", Command: "mcphub", Transport: "stdio",
		Trust: &config.MCPTrustConfig{
			LocalOwner: "mcphub", Gateway: config.MCPTrustGatewayMCPHub,
			ReadOnly: []string{"bob__bob_context", "mcphub_get_result"},
		},
	}})
	return ag
}

func readAgentBobFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("../ecosystem/testdata/bob_v040/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func agentStoredCallToolResult(t *testing.T, structured json.RawMessage, isError bool) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": agentMCPHubRawSecret + " /workspace/private command_name argv create_patterns",
		}},
		"structuredContent": structured,
		"isError":           isError,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func agentMCPHubStoredDocument(t *testing.T, callID, server, tool string, originalBytes int) json.RawMessage {
	t.Helper()
	budget := min(4096, originalBytes-1)
	if budget <= 0 {
		t.Fatalf("stored result test size %d cannot exceed a positive budget", originalBytes)
	}
	document, err := json.Marshal(map[string]any{
		"status": "stored", "callId": callID, "server": server, "tool": tool,
		"namespaced": server + "__" + tool, "originalBytes": originalBytes, "budgetBytes": budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func agentMCPHubPageDocument(t *testing.T, callID string, payload []byte, cursor, next int) json.RawMessage {
	t.Helper()
	document, err := json.Marshal(map[string]any{
		"status": "ok", "callId": callID, "mediaType": "application/json",
		"data":   base64.StdEncoding.EncodeToString(payload[cursor:next]),
		"cursor": cursor, "nextCursor": next, "done": next == len(payload), "totalBytes": len(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func agentMarshalDocument(t *testing.T, value any) json.RawMessage {
	t.Helper()
	document, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func feedAgentMCPHubPages(t *testing.T, ag *Agent, callID string, payload []byte, pageBytes int) ecosystem.MCPHubResultObservation {
	return feedAgentMCPHubPagesViaNamespace(t, ag, "mcphub", callID, payload, pageBytes)
}

func feedAgentMCPHubPagesViaNamespace(t *testing.T, ag *Agent, namespace, callID string, payload []byte, pageBytes int) ecosystem.MCPHubResultObservation {
	t.Helper()
	var observation ecosystem.MCPHubResultObservation
	for cursor := 0; cursor < len(payload); {
		next := min(cursor+pageBytes, len(payload))
		page := agentMCPHubPageDocument(t, callID, payload, cursor, next)
		call := llm.ToolCall{Name: namespace + "__mcphub_get_result", Arguments: map[string]any{"callId": callID, "cursor": cursor}}
		projection := ag.projectSemanticToolReceipt(call, "", page, nil, false, false, false)
		observation = ag.projectMCPHubResultAssembly(call, projection, page, false)
		cursor = next
	}
	return observation
}
