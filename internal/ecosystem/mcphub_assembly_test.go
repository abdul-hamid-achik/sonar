package ecosystem

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type cortexV0141Fixture struct {
	Payload struct {
		IsError           bool            `json:"isError"`
		JSONText          string          `json:"jsonText"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	} `json:"payload"`
}

func TestMCPHubResultAssemblerMatchesDirectCortexSemantics(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		tool    string
	}{
		{name: "success", fixture: "mcp_shared_envelope_success.json", tool: "cortex_open_task"},
		{name: "structured rejection", fixture: "mcp_shared_envelope_rejection.json", tool: "cortex_begin_change"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := readCortexV0141Fixture(t, test.fixture)
			receipt := RawReceipt{Structured: fixture.Payload.StructuredContent, ToolError: fixture.Payload.IsError}
			direct := ProjectReceipt(ProjectToolCall("cortex__"+test.tool, nil), receipt)
			payload := serializedCallToolResult(t, fixture.Payload.StructuredContent,
				fixture.Payload.JSONText+strings.Repeat(" bounded", 900), fixture.Payload.IsError)

			assembler := NewMCPHubResultAssembler()
			const callID = "cortex-v0141-paged-result"
			rememberStoredResult(t, assembler, "mcphub", callID, "cortex", test.tool, payload)
			observation := feedResultPages(t, assembler, "mcphub", callID, payload, 1216)

			assertSemanticParity(t, observation, direct)
			assertLazyRoute(t, observation.Projection, "mcphub", callID, "cortex", test.tool)
			if observation.Transient != "" {
				t.Fatalf("Cortex paged result exposed unexpected transient content: %q", observation.Transient)
			}
		})
	}
}

func TestMCPHubResultAssemblerMatchesDirectCortexContinuationActions(t *testing.T) {
	structured := cortexPlanContinuationFixture()
	receipt := RawReceipt{Structured: structured}
	direct := ProjectReceipt(ProjectToolCall("cortex__cortex_plan", nil), receipt)
	directActions := ProjectContinuationActions(direct, receipt)
	if len(directActions) != 1 {
		t.Fatalf("direct continuation actions = %#v, want one", directActions)
	}

	payload := serializedCallToolResult(t, structured, "untrusted wrapper action prose", false)
	assembler := NewMCPHubResultAssembler()
	const callID = "cortex-action-paged-result"
	rememberStoredResult(t, assembler, "mcphub", callID, "cortex", "cortex_plan", payload)
	observation := feedResultPages(t, assembler, "mcphub", callID, payload, 137)

	assertSemanticParity(t, observation, direct)
	assertLazyRoute(t, observation.Projection, "mcphub", callID, "cortex", "cortex_plan")
	if !reflect.DeepEqual(observation.Actions, directActions) {
		t.Fatalf("paged Cortex actions differ from direct parser\npaged:  %#v\ndirect: %#v", observation.Actions, directActions)
	}
	if observation.Transient != "" {
		t.Fatalf("Cortex paged action result exposed unexpected transient content: %q", observation.Transient)
	}
}

func TestMCPHubResultAssemblerMatchesDirectBobSemanticsAndTransient(t *testing.T) {
	tests := []struct {
		name, file, operation string
		toolError             bool
		document              json.RawMessage
	}{
		{name: "context clean", file: "context-clean-v1.json", operation: "bob_context"},
		{name: "context drift", file: "context-drift-v1.json", operation: "bob_context"},
		{name: "context conflict", file: "context-conflict-v1.json", operation: "bob_context"},
		{name: "path extension", file: "path-extension-v1.json", operation: "bob_path"},
		{name: "path managed", file: "path-managed-v1.json", operation: "bob_path"},
		{name: "playbook ready", file: "playbook-ready-v1.json", operation: "bob_playbook"},
		{name: "playbook error", file: "playbook-missing-input-v1.json", operation: "bob_playbook", toolError: true},
		{name: "future schema", file: "error-unsupported-schema-v1.json", operation: "bob_context"},
		{name: "malformed context", operation: "bob_context", document: json.RawMessage(`{"schema_version":1,"ok":true}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			structured := test.document
			if test.file != "" {
				structured = readBobV040Fixture(t, test.file)
			}
			receipt := RawReceipt{Structured: structured, ToolError: test.toolError}
			direct := ProjectReceipt(ProjectToolCall("bob__"+test.operation, nil), receipt)
			directActions := ProjectContinuationActions(direct, receipt)
			directTransient, directTransientOK := TransientModelContent(direct, receipt)
			payload := serializedCallToolResult(t, structured, strings.Repeat("untrusted wrapper prose ", 400), test.toolError)

			assembler := NewMCPHubResultAssembler()
			callID := "bob-v040-paged-" + strings.ReplaceAll(test.name, " ", "-")
			rememberStoredResult(t, assembler, "mcphub", callID, "bob", test.operation, payload)
			observation := feedResultPages(t, assembler, "mcphub", callID, payload, 913)

			assertSemanticParity(t, observation, direct)
			assertLazyRoute(t, observation.Projection, "mcphub", callID, "bob", test.operation)
			if test.operation == "bob_context" && direct.DomainTyped && direct.Digest != nil && direct.Digest.Kind == DigestBobContext {
				if observation.Workspace != "/workspace" {
					t.Fatalf("paged Bob workspace = %q, want exact transient workspace", observation.Workspace)
				}
			} else if observation.Workspace != "" {
				t.Fatalf("non-context result exposed workspace %q", observation.Workspace)
			}
			if observation.Transient != directTransient {
				t.Fatalf("paged Bob transient differs from direct parser (available=%t)\npaged:  %q\ndirect: %q", directTransientOK, observation.Transient, directTransient)
			}
			if !reflect.DeepEqual(observation.Actions, directActions) {
				t.Fatalf("paged Bob actions differ from direct parser\npaged:  %#v\ndirect: %#v", observation.Actions, directActions)
			}
			for _, forbidden := range []string{"/workspace", `"argv"`, "untrusted wrapper prose"} {
				if strings.Contains(observation.Transient, forbidden) {
					t.Fatalf("paged Bob transient retained forbidden data %q: %q", forbidden, observation.Transient)
				}
			}
			if !directTransientOK && observation.Transient != "" {
				t.Fatalf("non-transient direct result gained paged transient content: %q", observation.Transient)
			}
		})
	}
}

func TestMCPHubResultAssemblerPreservesStoredIsError(t *testing.T) {
	structured := readBobV040Fixture(t, "context-clean-v1.json")
	direct := ProjectReceipt(ProjectToolCall("bob__bob_context", nil), RawReceipt{
		Structured: structured,
		ToolError:  true,
	})
	payload := serializedCallToolResult(t, structured, "optimistic wrapper prose", true)

	assembler := NewMCPHubResultAssembler()
	const callID = "bob-is-error-result"
	rememberStoredResult(t, assembler, "mcphub", callID, "bob", "bob_context", payload)
	observation := feedResultPages(t, assembler, "mcphub", callID, payload, 701)

	assertSemanticParity(t, observation, direct)
	if observation.Projection.Domain != DomainFailed || observation.Projection.DomainTyped {
		t.Fatalf("stored isError was overridden by optimistic structure: %#v", observation.Projection)
	}
	if observation.Transient != "" {
		t.Fatalf("failed isError result exposed transient content: %q", observation.Transient)
	}
}

func TestMCPHubResultAssemblerBindsExactNamespaceAndRoute(t *testing.T) {
	structured := readBobV040Fixture(t, "context-clean-v1.json")
	payload := serializedCallToolResult(t, structured, "ignored", false)
	const callID = "route-bound-result"

	t.Run("namespace isolates identical call IDs", func(t *testing.T) {
		assembler := NewMCPHubResultAssembler()
		rememberStoredResult(t, assembler, "mcphub", callID, "bob", "bob_context", payload)
		projection, receipt := resultPage(t, callID, payload, 0, min(512, len(payload)))

		wrong := assembler.ObservePage("relay", 0, projection, receipt)
		if wrong.Bound || wrong.Complete {
			t.Fatalf("wrong namespace claimed bound result: %#v", wrong)
		}
		correct := assembler.ObservePage("mcphub", 0, projection, receipt)
		if !correct.Bound || correct.Complete || correct.Projection.Domain != DomainAttention {
			t.Fatalf("correct namespace did not consume first page: %#v", correct)
		}
	})

	t.Run("response route cannot replace dispatch route", func(t *testing.T) {
		assembler := NewMCPHubResultAssembler()
		projection := storedResultProjection(t, callID, "bob", "bob_context", len(payload))
		if assembler.Remember("mcphub", "cortex", "cortex_status", projection) {
			t.Fatal("mismatched host route was accepted")
		}
		if count := assemblyCount(assembler); count != 0 {
			t.Fatalf("mismatched host route allocated %d assemblies", count)
		}
	})

	t.Run("bound Bob route never inherits Cortex parser", func(t *testing.T) {
		fixture := readCortexV0141Fixture(t, "mcp_shared_envelope_success.json")
		wrongPayload := serializedCallToolResult(t, fixture.Payload.StructuredContent, fixture.Payload.JSONText, false)
		assembler := NewMCPHubResultAssembler()
		rememberStoredResult(t, assembler, "mcphub", callID, "bob", "bob_context", wrongPayload)
		observation := feedResultPages(t, assembler, "mcphub", callID, wrongPayload, 503)
		if !observation.Bound || !observation.Complete || observation.Projection.Domain != DomainUnknown ||
			observation.Projection.DomainTyped || observation.Projection.Specialist != "bob" || observation.Transient != "" {
			t.Fatalf("route-confused result did not fail closed as Bob: %#v", observation)
		}
		assertLazyRoute(t, observation.Projection, "mcphub", callID, "bob", "bob_context")
	})
}

func TestMCPHubResultAssemblerRejectsBrokenPageChains(t *testing.T) {
	fixture := readCortexV0141Fixture(t, "mcp_shared_envelope_success.json")
	payload := serializedCallToolResult(t, fixture.Payload.StructuredContent,
		fixture.Payload.JSONText+strings.Repeat(" bounded", 500), false)
	const (
		callID   = "cortex-invalid-page-sequence"
		pageSize = 1216
	)

	tests := []struct {
		name string
		run  func(*testing.T, *MCPHubResultAssembler) MCPHubResultObservation
	}{
		{
			name: "requested cursor differs from page cursor",
			run: func(t *testing.T, assembler *MCPHubResultAssembler) MCPHubResultObservation {
				projection, receipt := resultPage(t, callID, payload, 0, pageSize)
				return assembler.ObservePage("mcphub", 1, projection, receipt)
			},
		},
		{
			name: "out of order",
			run: func(t *testing.T, assembler *MCPHubResultAssembler) MCPHubResultObservation {
				projection, receipt := resultPage(t, callID, payload, pageSize, min(2*pageSize, len(payload)))
				return assembler.ObservePage("mcphub", pageSize, projection, receipt)
			},
		},
		{
			name: "duplicate page",
			run: func(t *testing.T, assembler *MCPHubResultAssembler) MCPHubResultObservation {
				projection, receipt := resultPage(t, callID, payload, 0, pageSize)
				first := assembler.ObservePage("mcphub", 0, projection, receipt)
				if !first.Bound || first.Complete || first.Projection.Domain != DomainAttention {
					t.Fatalf("first page projection = %#v", first)
				}
				return assembler.ObservePage("mcphub", 0, projection, receipt)
			},
		},
		{
			name: "total drift",
			run: func(t *testing.T, assembler *MCPHubResultAssembler) MCPHubResultObservation {
				projection, receipt := resultPageWithEnvelope(t, callID, payload[:pageSize], 0, pageSize, len(payload)+1, false)
				return assembler.ObservePage("mcphub", 0, projection, receipt)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assembler := NewMCPHubResultAssembler()
			rememberStoredResult(t, assembler, "mcphub", callID, "cortex", "cortex_open_task", payload)
			observation := test.run(t, assembler)
			if !observation.Bound || observation.Complete || observation.Projection.Domain != DomainFailed ||
				!observation.Projection.DomainTyped || observation.Projection.Evidence != EvidenceNone {
				t.Fatalf("broken chain did not fail closed: %#v", observation)
			}
			if count := assemblyCount(assembler); count != 0 {
				t.Fatalf("broken chain retained %d assemblies", count)
			}
		})
	}
}

func TestMCPHubResultAssemblerRejectsPrematureDone(t *testing.T) {
	structured := readBobV040Fixture(t, "context-clean-v1.json")
	payload := serializedCallToolResult(t, structured, "ignored", false)
	const callID = "premature-done-result"
	assembler := NewMCPHubResultAssembler()
	rememberStoredResult(t, assembler, "mcphub", callID, "bob", "bob_context", payload)

	pageEnd := min(512, len(payload)-1)
	projection, receipt := resultPageWithEnvelope(t, callID, payload[:pageEnd], 0, pageEnd, len(payload), true)
	// ProjectReceipt already rejects the contradictory done flag. Supply the
	// matching bounded page metadata to exercise the assembler's independent
	// envelope validation and teardown path.
	projection.Digest = &ReceiptDigest{
		Kind: DigestMCPHubPage, Cursor: 0, NextCursor: int64(pageEnd), TotalBytes: int64(len(payload)),
		PageBytes: int64(pageEnd), Done: true,
	}
	observation := assembler.ObservePage("mcphub", 0, projection, receipt)
	if !observation.Bound || observation.Complete || observation.Projection.Domain != DomainFailed ||
		!observation.Projection.DomainTyped || assemblyCount(assembler) != 0 {
		t.Fatalf("premature done did not fail closed and discard state: %#v", observation)
	}
}

func TestMCPHubResultAssemblerRejectsMalformedSerializedCallToolResults(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "malformed JSON", payload: []byte(`{"content":[],"structuredContent":`)},
		{name: "missing structured content", payload: []byte(`{"content":[{"type":"text","text":"ignored"}],"isError":false}`)},
		{name: "content is not an array", payload: []byte(`{"content":{},"structuredContent":{"ok":true},"isError":false}`)},
		{name: "structured content is not an object", payload: []byte(`{"content":[],"structuredContent":[],"isError":false}`)},
		{name: "meta is not an object", payload: []byte(`{"_meta":[],"content":[],"structuredContent":{"ok":true},"isError":false}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assembler := NewMCPHubResultAssembler()
			const callID = "malformed-call-tool-result"
			rememberStoredResult(t, assembler, "mcphub", callID, "cortex", "cortex_open_task", test.payload)
			observation := feedResultPages(t, assembler, "mcphub", callID, test.payload, 31)
			if !observation.Bound || !observation.Complete || observation.Projection.Domain != DomainUnknown ||
				observation.Projection.DomainTyped || observation.Projection.Evidence != EvidenceNone || observation.Transient != "" {
				t.Fatalf("malformed wrapper did not remain unknown: %#v", observation)
			}
			assertLazyRoute(t, observation.Projection, "mcphub", callID, "cortex", "cortex_open_task")
		})
	}
}

func TestMCPHubResultAssemblerBoundsAndDiscardsState(t *testing.T) {
	t.Run("active assemblies evict oldest namespace-call binding", func(t *testing.T) {
		assembler := NewMCPHubResultAssembler()
		for index := 0; index < maxMCPHubActiveAssemblies+1; index++ {
			callID := "bounded-result-" + string(rune('a'+index))
			projection := storedResultProjection(t, callID, "cortex", "cortex_status", 64)
			if !assembler.Remember("mcphub", "cortex", "cortex_status", projection) {
				t.Fatalf("Remember(%q) failed", callID)
			}
		}
		assembler.mu.Lock()
		defer assembler.mu.Unlock()
		if len(assembler.entries) != maxMCPHubActiveAssemblies {
			t.Fatalf("active assemblies = %d, want %d", len(assembler.entries), maxMCPHubActiveAssemblies)
		}
		if _, retained := assembler.entries[mcphubResultKey{namespace: "mcphub", callID: "bounded-result-a"}]; retained {
			t.Fatal("oldest result was not evicted")
		}
	})

	t.Run("oversized total never allocates", func(t *testing.T) {
		assembler := NewMCPHubResultAssembler()
		projection := storedResultProjection(t, "too-large", "cortex", "cortex_status", maxMCPHubAssembledResultBytes+1)
		if assembler.Remember("mcphub", "cortex", "cortex_status", projection) {
			t.Fatal("oversized stored result was accepted")
		}
		if count := assemblyCount(assembler); count != 0 {
			t.Fatalf("oversized result allocated %d assemblies", count)
		}
	})

	t.Run("unavailable result discards and zeroes partial bytes", func(t *testing.T) {
		assembler := NewMCPHubResultAssembler()
		payload := []byte(strings.Repeat("sensitive-result-byte", 80))
		const callID = "expired-stored-result"
		rememberStoredResult(t, assembler, "mcphub", callID, "cortex", "cortex_status", payload)
		projection, receipt := resultPage(t, callID, payload, 0, 512)
		first := assembler.ObservePage("mcphub", 0, projection, receipt)
		if !first.Bound || first.Complete {
			t.Fatalf("first page not retained: %#v", first)
		}
		partial := assemblyDataAlias(t, assembler, "mcphub", callID)
		unavailable := ProjectReceipt(ProjectToolCall("mcphub__mcphub_get_result", map[string]any{"callId": callID}), RawReceipt{
			Structured: json.RawMessage(`{"status":"unavailable","callId":"expired-stored-result","reason":"expired"}`),
		})
		observation := assembler.ObservePage("mcphub", 512, unavailable, RawReceipt{})
		if !observation.Bound || observation.Complete || assemblyCount(assembler) != 0 {
			t.Fatalf("unavailable result did not discard assembly: %#v", observation)
		}
		assertZeroed(t, partial)
	})

	t.Run("reset zeroes every partial result", func(t *testing.T) {
		assembler := NewMCPHubResultAssembler()
		payload := []byte(strings.Repeat("reset-sensitive-byte", 80))
		const callID = "reset-stored-result"
		rememberStoredResult(t, assembler, "mcphub", callID, "cortex", "cortex_status", payload)
		projection, receipt := resultPage(t, callID, payload, 0, 512)
		if observation := assembler.ObservePage("mcphub", 0, projection, receipt); !observation.Bound {
			t.Fatalf("first page was not bound: %#v", observation)
		}
		partial := assemblyDataAlias(t, assembler, "mcphub", callID)
		assembler.Reset()
		if count := assemblyCount(assembler); count != 0 {
			t.Fatalf("Reset retained %d assemblies", count)
		}
		assertZeroed(t, partial)
	})

	t.Run("completed parse zeroes assembled backing bytes", func(t *testing.T) {
		structured := readBobV040Fixture(t, "context-clean-v1.json")
		payload := serializedCallToolResult(t, structured, "ignored", false)
		assembler := NewMCPHubResultAssembler()
		const callID = "completed-zeroed-result"
		rememberStoredResult(t, assembler, "mcphub", callID, "bob", "bob_context", payload)
		projection, receipt := resultPage(t, callID, payload, 0, min(512, len(payload)-1))
		first := assembler.ObservePage("mcphub", 0, projection, receipt)
		if !first.Bound || first.Complete {
			t.Fatalf("first page = %#v", first)
		}
		entry := assemblyEntry(t, assembler, "mcphub", callID)
		observation := feedResultPagesFrom(t, assembler, "mcphub", callID, payload, 512, int(entry.next))
		if !observation.Complete {
			t.Fatalf("result did not complete: %#v", observation)
		}
		assertZeroed(t, entry.data)
	})
}

func assertSemanticParity(t *testing.T, observation MCPHubResultObservation, direct ToolProjection) {
	t.Helper()
	if !observation.Bound || !observation.Complete {
		t.Fatalf("paged observation = %#v, want bound complete result", observation)
	}
	got := observation.Projection
	if got.Specialist != direct.Specialist || got.Operation != direct.Operation || got.Role != direct.Role ||
		got.Transport != direct.Transport || got.Domain != direct.Domain || got.DomainTyped != direct.DomainTyped ||
		got.Evidence != direct.Evidence || !reflect.DeepEqual(got.Digest, direct.Digest) ||
		!reflect.DeepEqual(got.Artifact, direct.Artifact) {
		t.Fatalf("paged semantic projection differs from direct parser\npaged: %#v\ndirect: %#v", got, direct)
	}
}

func assertLazyRoute(t *testing.T, projection ToolProjection, namespace, callID, server, tool string) {
	t.Helper()
	want := ToolRoute{Gateway: namespace, Server: server, Tool: tool, CallID: callID, Lazy: true}
	if projection.Route != want {
		t.Fatalf("route = %#v, want %#v", projection.Route, want)
	}
}

func rememberStoredResult(t *testing.T, assembler *MCPHubResultAssembler, namespace, callID, server, tool string, payload []byte) {
	t.Helper()
	projection := storedResultProjection(t, callID, server, tool, len(payload))
	if !assembler.Remember(namespace, server, tool, projection) {
		t.Fatalf("Remember(%q, %q, %q, %q) failed for %d bytes", namespace, callID, server, tool, len(payload))
	}
}

func storedResultProjection(t *testing.T, callID, server, tool string, size int) ToolProjection {
	t.Helper()
	budget := min(4096, size-1)
	if budget <= 0 {
		t.Fatalf("stored result test size %d cannot exceed a positive budget", size)
	}
	document, err := json.Marshal(map[string]any{
		"status": "stored", "callId": callID, "server": server, "tool": tool,
		"namespaced": server + "__" + tool, "originalBytes": size, "budgetBytes": budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	projection := ProjectReceipt(ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": server, "tool": tool,
	}), RawReceipt{Structured: document})
	if projection.Digest == nil || projection.Digest.Kind != DigestMCPHubStored {
		t.Fatalf("stored projection = %#v", projection)
	}
	return projection
}

func feedResultPages(t *testing.T, assembler *MCPHubResultAssembler, namespace, callID string, payload []byte, pageSize int) MCPHubResultObservation {
	t.Helper()
	return feedResultPagesFrom(t, assembler, namespace, callID, payload, pageSize, 0)
}

func feedResultPagesFrom(t *testing.T, assembler *MCPHubResultAssembler, namespace, callID string, payload []byte, pageSize, start int) MCPHubResultObservation {
	t.Helper()
	if pageSize <= 0 || pageSize > maxMCPHubResultPageBytes {
		t.Fatalf("invalid test page size %d", pageSize)
	}
	var final MCPHubResultObservation
	for cursor := start; cursor < len(payload); {
		next := min(cursor+pageSize, len(payload))
		projection, receipt := resultPage(t, callID, payload, cursor, next)
		final = assembler.ObservePage(namespace, int64(cursor), projection, receipt)
		if next < len(payload) && (!final.Bound || final.Complete || final.Projection.Domain != DomainAttention) {
			t.Fatalf("partial page %d-%d observation = %#v", cursor, next, final)
		}
		cursor = next
	}
	return final
}

func resultPage(t *testing.T, callID string, payload []byte, cursor, next int) (ToolProjection, RawReceipt) {
	t.Helper()
	return resultPageWithEnvelope(t, callID, payload[cursor:next], cursor, next, len(payload), next == len(payload))
}

func resultPageWithEnvelope(t *testing.T, callID string, page []byte, cursor, next, total int, done bool) (ToolProjection, RawReceipt) {
	t.Helper()
	document, err := json.Marshal(map[string]any{
		"status": "ok", "callId": callID, "mediaType": "application/json",
		"data": base64.StdEncoding.EncodeToString(page), "cursor": cursor,
		"nextCursor": next, "done": done, "totalBytes": total,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := RawReceipt{Structured: document}
	projection := ProjectReceipt(ProjectToolCall("mcphub__mcphub_get_result", map[string]any{
		"callId": callID, "cursor": cursor,
	}), receipt)
	return projection, receipt
}

func serializedCallToolResult(t *testing.T, structured json.RawMessage, text string, toolError bool) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"content":           []map[string]any{{"type": "text", "text": text}},
		"structuredContent": structured,
		"isError":           toolError,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func assemblyCount(assembler *MCPHubResultAssembler) int {
	assembler.mu.Lock()
	defer assembler.mu.Unlock()
	return len(assembler.entries)
}

func assemblyEntry(t *testing.T, assembler *MCPHubResultAssembler, namespace, callID string) *mcphubResultAssembly {
	t.Helper()
	assembler.mu.Lock()
	defer assembler.mu.Unlock()
	entry := assembler.entries[mcphubResultKey{namespace: namespace, callID: callID}]
	if entry == nil {
		t.Fatalf("assembly %s/%s not found", namespace, callID)
	}
	return entry
}

func assemblyDataAlias(t *testing.T, assembler *MCPHubResultAssembler, namespace, callID string) []byte {
	t.Helper()
	entry := assemblyEntry(t, assembler, namespace, callID)
	alias := entry.data
	if len(alias) == 0 {
		t.Fatal("assembly has no partial bytes")
	}
	return alias
}

func assertZeroed(t *testing.T, data []byte) {
	t.Helper()
	for index, value := range data {
		if value != 0 {
			t.Fatalf("transient byte %d was not zeroed: %d", index, value)
		}
	}
}

func readCortexV0141Fixture(t *testing.T, name string) cortexV0141Fixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/cortex_v0141/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var fixture cortexV0141Fixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}
