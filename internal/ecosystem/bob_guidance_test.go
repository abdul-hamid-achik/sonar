package ecosystem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const bobV040FixtureRevision = "8639e51828ec1511ba6745a67b31e622470fa837"

// These documents are byte-for-byte copies of Bob v0.4.0's published
// consumer fixtures at bobV040FixtureRevision.
func TestBobV040PublishedGuidanceFixtures(t *testing.T) {
	t.Logf("Bob public fixture revision: %s", bobV040FixtureRevision)
	tests := []struct {
		file, tool string
		want       DomainState
		toolError  bool
		digest     ReceiptDigest
	}{
		{
			file: "context-clean-v1.json", tool: "bob__bob_context", want: DomainSucceeded,
			digest: ReceiptDigest{
				Kind: DigestBobContext, RecipeID: "go-agent-tool", RecipeVersion: 4, State: "clean",
				ContractDigest: "sha256:2c565f3bd95682441a97d52db15765d5c4b2ee2fe3bc077066af1440740d2e4d",
				ContextDigest:  "sha256:53d8fbfeeec21299e837eebaea50e2e5e2215ce25baeaed7ea3ce72cb3466f9c",
				PlanDigest:     "sha256:e6b5bf0443ee158baebcd0242153d2419bf3f74bc239b1e9c6f2ac9caa5fc7c7",
				ManagedFiles:   30, Capabilities: 14, ExtensionCount: 3, PlaybookCount: 7,
				Items: []string{"cli.command_files", "domain.packages", "terminal.additional_specs"},
			},
		},
		{
			file: "context-drift-v1.json", tool: "bob__bob_context", want: DomainDrift,
			digest: ReceiptDigest{
				Kind: DigestBobContext, RecipeID: "go-agent-tool", RecipeVersion: 4, State: "drifted",
				ContractDigest: "sha256:2c565f3bd95682441a97d52db15765d5c4b2ee2fe3bc077066af1440740d2e4d",
				ContextDigest:  "sha256:1329b47d3a81d2a7f34411a39c7a9e802d41435084c25e4f28a1fe785a654a9f",
				PlanDigest:     "sha256:ca6cd04c00940690322039e1d2106e08742fb1eda2caf48b1a0e79b0ff380e6a",
				ManagedFiles:   30, Capabilities: 14, ExtensionCount: 3, PlaybookCount: 7,
				Items: []string{"cli.command_files", "domain.packages", "terminal.additional_specs"}, FirstAction: "review_plan",
			},
		},
		{
			file: "context-conflict-v1.json", tool: "bob__bob_context", want: DomainConflict,
			digest: ReceiptDigest{
				Kind: DigestBobContext, RecipeID: "go-agent-tool", RecipeVersion: 4, State: "conflicted",
				ContractDigest: "sha256:2c565f3bd95682441a97d52db15765d5c4b2ee2fe3bc077066af1440740d2e4d",
				ContextDigest:  "sha256:ca6f00df7375cde65ab909aa9d2024a600f6d1d7ff6e05f212bdb8d85cfe9cec",
				PlanDigest:     "sha256:04fa5750c5f19fb73dffef4e78d0a3bff9286caa6e3152e23419af6a1f14278c",
				ConflictCount:  1, ManagedFiles: 30, Capabilities: 14, ExtensionCount: 3, PlaybookCount: 7,
				Items: []string{"cli.command_files", "domain.packages", "terminal.additional_specs"}, FirstAction: "review_plan",
			},
		},
		{
			file: "path-extension-v1.json", tool: "bob__bob_path", want: DomainSucceeded,
			digest: ReceiptDigest{
				Kind: DigestBobPath, Classification: "extension_point", State: "extension_point", Effect: "outside_bob_ownership",
				Items: []string{"cli.command_files", "add-cli-command"}, FirstAction: "show_playbook:add-cli-command",
				ExtensionCount: 1, PlaybookCount: 1,
			},
		},
		{
			file: "path-managed-v1.json", tool: "bob__bob_path", want: DomainAttention,
			digest: ReceiptDigest{
				Kind: DigestBobPath, Classification: "managed", State: "managed_in_sync", Effect: "will_conflict",
				Items: []string{"add-cli-command"}, FirstAction: "show_playbook:add-cli-command",
				PlaybookCount: 1, Exists: true,
			},
		},
		{
			file: "playbook-ready-v1.json", tool: "bob__bob_playbook", want: DomainSucceeded,
			digest: ReceiptDigest{
				Kind: DigestBobPlaybook, Count: 5, Target: "add-cli-command", RecipeID: "go-agent-tool", RecipeVersion: 4,
				State: "ready", Scope: "small", Risk: "medium", FirstAction: "create_command_file",
				Effect: "repository_mutation", Available: true,
			},
		},
		{
			file: "playbook-missing-input-v1.json", tool: "bob__bob_playbook", want: DomainBlocked, toolError: true,
			digest: ReceiptDigest{Kind: DigestBobFailure, Target: "input_invalid"},
		},
	}

	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			raw := readBobV040Fixture(t, test.file)
			got := ProjectReceipt(ProjectToolCall(test.tool, nil), RawReceipt{Structured: raw, ToolError: test.toolError})
			assertBobProjection(t, got, test.want, test.digest)
			assertBobPersistenceSafe(t, got, raw)
		})
	}

	t.Run("future schema", func(t *testing.T) {
		raw := readBobV040Fixture(t, "error-unsupported-schema-v1.json")
		got := ProjectReceipt(ProjectToolCall("bob__bob_context", nil), RawReceipt{Structured: raw})
		assertBobFailClosed(t, got, RawReceipt{Structured: raw})
	})
}

func TestBobV040GuidanceRouteParity(t *testing.T) {
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
			raw := test.document
			if test.file != "" {
				raw = readBobV040Fixture(t, test.file)
			}
			receipt := RawReceipt{Structured: raw, ToolError: test.toolError}
			direct := ProjectReceipt(ProjectToolCall("bob__"+test.operation, nil), receipt)
			pinned := ProjectReceipt(ProjectToolCall("mcphub__bob__"+test.operation, nil), receipt)
			lazy := ProjectReceipt(ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
				"server": "bob", "tool": test.operation,
			}), receipt)

			wantSemantic := direct
			wantSemantic.Route = ToolRoute{}
			for name, got := range map[string]ToolProjection{"pinned": pinned, "lazy": lazy} {
				gotSemantic := got
				gotSemantic.Route = ToolRoute{}
				if !reflect.DeepEqual(gotSemantic, wantSemantic) {
					t.Fatalf("%s semantic projection = %#v, want direct %#v", name, gotSemantic, wantSemantic)
				}
				if got.Route != (ToolRoute{Gateway: "mcphub", Server: "bob", Tool: test.operation, Lazy: true}) {
					t.Fatalf("%s route = %#v", name, got.Route)
				}
			}
			if direct.Route != (ToolRoute{Server: "bob", Tool: test.operation}) {
				t.Fatalf("direct route = %#v", direct.Route)
			}
		})
	}
}

func TestBobV040PathAttentionEffects(t *testing.T) {
	tests := []struct {
		name, classification, state, effect string
	}{
		{"managed conflict", "managed", "managed_in_sync", "will_conflict"},
		{"retired Bob file", "managed", "retired_owned", "reserved_for_bob"},
		{"reserved manifest change", "reserved", "reserved", "requires_manifest_change"},
		{"reserved for Bob", "reserved", "reserved", "reserved_for_bob"},
		{"unsafe reserved path", "reserved", "reserved", "unsafe"},
		{"unsafe unmanaged special file", "unmanaged", "special_file", "unsafe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := decodeBobV040Fixture(t, "path-managed-v1.json")
			path := document["path"].(map[string]any)
			path["classification"], path["state"], path["human_edit_effect"] = test.classification, test.state, test.effect
			raw := marshalBobFixture(t, document)
			got := ProjectReceipt(ProjectToolCall("bob__bob_path", nil), RawReceipt{Structured: raw})
			if got.Domain != DomainAttention || !got.DomainTyped || got.Evidence != EvidenceNone || got.Digest == nil || got.Digest.Effect != test.effect {
				t.Fatalf("projection = %#v, want typed attention for %s", got, test.effect)
			}
		})
	}
}

func TestBobV040GuidanceTransientContentIsBoundedAndSanitized(t *testing.T) {
	tests := []struct {
		file, tool string
		forbidden  []string
	}{
		{
			file: "context-drift-v1.json", tool: "bob__bob_context",
			forbidden: []string{"/workspace", "cmd/acme/main.go", "internal/cli/root.go", `"argv"`, `"cwd"`, "bob plan"},
		},
		{
			file: "path-managed-v1.json", tool: "bob__bob_path",
			forbidden: []string{"/workspace", "internal/cli/root.go", `"path"`, `"argv"`, `"cwd"`, "bob playbook show"},
		},
		{
			file: "playbook-ready-v1.json", tool: "bob__bob_playbook",
			forbidden: []string{"/workspace", "internal/cli/hello.go", "internal/cli/hello_test.go", `"argv"`, `"paths"`, `"values"`, `"command_name":"hello"`, "go test ./...", "Create the command implementation"},
		},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			raw := readBobV040Fixture(t, test.file)
			receipt := RawReceipt{Structured: raw}
			projection := ProjectReceipt(ProjectToolCall(test.tool, nil), receipt)
			transient, ok := TransientModelContent(projection, receipt)
			if !ok {
				t.Fatalf("validated Bob fixture did not expose transient model content: %#v", projection)
			}
			prefix, payload, found := strings.Cut(transient, "\n")
			if !found || prefix != "Bob guidance (validated transient content; not saved)" || !json.Valid([]byte(payload)) {
				t.Fatalf("transient content is not the bounded Bob envelope: %q", transient)
			}
			if len(payload) > maxTransientBobGuidanceBytes {
				t.Fatalf("transient payload = %d bytes, cap = %d", len(payload), maxTransientBobGuidanceBytes)
			}
			for _, marker := range test.forbidden {
				if strings.Contains(transient, marker) {
					t.Fatalf("transient content leaked %q: %s", marker, transient)
				}
			}
		})
	}
}

func TestBobContextWorkspaceIsTransientAndExact(t *testing.T) {
	raw := readBobV040Fixture(t, "context-clean-v1.json")
	receipt := RawReceipt{Structured: raw}
	projection := ProjectReceipt(ProjectToolCall("bob__bob_context", nil), receipt)
	workspace, ok := BobContextWorkspace(projection, receipt)
	if !ok || workspace != "/workspace" {
		t.Fatalf("workspace = %q, %t; want exact transient workspace", workspace, ok)
	}
	if strings.Contains(SafeReceiptText(projection), workspace) {
		t.Fatalf("durable receipt leaked workspace: %q", SafeReceiptText(projection))
	}

	for name, mutate := range map[string]func(*ToolProjection, *RawReceipt){
		"foreign projection": func(p *ToolProjection, _ *RawReceipt) { p.Specialist = "other" },
		"digest mismatch": func(p *ToolProjection, _ *RawReceipt) {
			p.Digest.ContextDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
		"malformed receipt": func(_ *ToolProjection, r *RawReceipt) {
			r.Structured = json.RawMessage(`{"schema_version":1,"ok":true}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := projection
			digest := *projection.Digest
			candidate.Digest = &digest
			candidateReceipt := receipt
			mutate(&candidate, &candidateReceipt)
			if workspace, ok := BobContextWorkspace(candidate, candidateReceipt); ok || workspace != "" {
				t.Fatalf("untrusted workspace = %q, %t", workspace, ok)
			}
		})
	}
}

func TestBobV040ContextDigestAndTransientListsAreBounded(t *testing.T) {
	document := decodeBobV040Fixture(t, "context-clean-v1.json")
	context := document["context"].(map[string]any)
	points := context["extension_points"].([]any)
	for i := 0; i < 24; i++ {
		points = append(points, map[string]any{
			"id": "extra.extension." + twoDigits(i), "ownership": "human",
			"create_patterns": []any{"extra/<file>.go"},
		})
	}
	context["extension_points"] = points
	context["truncation"] = map[string]any{
		"byte_limit": 6144, "profile": "compact", "truncated": true,
		"omitted": map[string]any{"extension_points": 1},
	}
	raw := marshalBobFixture(t, document)
	receipt := RawReceipt{Structured: raw}
	projection := ProjectReceipt(ProjectToolCall("bob__bob_context", nil), receipt)
	if projection.Digest == nil || len(projection.Digest.Items) != maxProjectionDigestItems ||
		projection.Digest.ExtensionCount != int64(len(points)) || !projection.Digest.Truncated {
		t.Fatalf("bounded context digest = %#v", projection.Digest)
	}
	transient, ok := TransientModelContent(projection, receipt)
	if !ok {
		t.Fatal("bounded context fixture lost transient content")
	}
	_, payload, _ := strings.Cut(transient, "\n")
	var decoded struct {
		ExtensionPoints []string `json:"extension_points"`
		Truncated       bool     `json:"truncated"`
	}
	if json.Unmarshal([]byte(payload), &decoded) != nil || len(decoded.ExtensionPoints) != maxTransientBobExtensionPoints || !decoded.Truncated {
		t.Fatalf("bounded transient context = %#v", decoded)
	}
}

func TestBobV040GuidanceContractsFailClosed(t *testing.T) {
	tests := []struct {
		name, file, tool string
		mutate           func(map[string]any)
	}{
		{
			name: "context repository state contradicts clean bit", file: "context-clean-v1.json", tool: "bob__bob_context",
			mutate: func(document map[string]any) {
				document["context"].(map[string]any)["repository"].(map[string]any)["clean"] = false
			},
		},
		{
			name: "context missing required digest", file: "context-clean-v1.json", tool: "bob__bob_context",
			mutate: func(document map[string]any) {
				delete(document["context"].(map[string]any), "context_digest")
			},
		},
		{
			name: "context future repository enum", file: "context-clean-v1.json", tool: "bob__bob_context",
			mutate: func(document map[string]any) {
				document["context"].(map[string]any)["repository"].(map[string]any)["state"] = "future_state"
			},
		},
		{
			name: "path authority selects another workspace", file: "path-managed-v1.json", tool: "bob__bob_path",
			mutate: func(document map[string]any) {
				document["authority"].(map[string]any)["selected_workspace"] = "/other"
			},
		},
		{
			name: "path future effect", file: "path-managed-v1.json", tool: "bob__bob_path",
			mutate: func(document map[string]any) {
				document["path"].(map[string]any)["human_edit_effect"] = "future_effect"
			},
		},
		{
			name: "path exposed ID is path-like", file: "path-extension-v1.json", tool: "bob__bob_path",
			mutate: func(document map[string]any) {
				document["path"].(map[string]any)["extension_points"] = []any{"../../private"}
			},
		},
		{
			name: "playbook operation and payload disagree", file: "playbook-ready-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				document["operation"] = "show"
			},
		},
		{
			name: "playbook dependency ID is path-like", file: "playbook-ready-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				steps := document["plan"].(map[string]any)["playbook"].(map[string]any)["steps"].([]any)
				steps[1].(map[string]any)["depends_on"] = []any{"../create_command_file"}
			},
		},
		{
			name: "future guidance error code", file: "playbook-missing-input-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				document["error"].(map[string]any)["code"] = "future_error"
			},
		},
		{
			name: "path-like guidance error code", file: "playbook-missing-input-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				document["error"].(map[string]any)["code"] = "input/invalid"
			},
		},
		{
			name: "future operation cannot carry playbook failure", file: "playbook-missing-input-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				document["operation"] = "future"
				document["error"].(map[string]any)["code"] = "playbook_failed"
			},
		},
		{
			name: "future playbook step kind", file: "playbook-ready-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				steps := document["plan"].(map[string]any)["playbook"].(map[string]any)["steps"].([]any)
				steps[0].(map[string]any)["kind"] = "future_kind"
			},
		},
		{
			name: "future playbook input type", file: "playbook-ready-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				inputs := document["plan"].(map[string]any)["playbook"].(map[string]any)["inputs"].([]any)
				inputs[0].(map[string]any)["type"] = "future_type"
			},
		},
		{
			name: "plan success omits required input", file: "playbook-ready-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				document["plan"].(map[string]any)["values"] = map[string]any{}
			},
		},
		{
			name: "plan success includes unknown input", file: "playbook-ready-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				document["plan"].(map[string]any)["values"].(map[string]any)["future_input"] = "value"
			},
		},
		{
			name: "plan success contains repository root path", file: "playbook-ready-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				input := document["plan"].(map[string]any)["playbook"].(map[string]any)["inputs"].([]any)[0].(map[string]any)
				input["type"] = "repository_path"
				input["validation"] = "safe-relative-path"
				document["plan"].(map[string]any)["values"].(map[string]any)["command_name"] = "."
			},
		},
		{
			name: "playbook duplicates step IDs", file: "playbook-ready-v1.json", tool: "bob__bob_playbook",
			mutate: func(document map[string]any) {
				steps := document["plan"].(map[string]any)["playbook"].(map[string]any)["steps"].([]any)
				steps[1].(map[string]any)["id"] = steps[0].(map[string]any)["id"]
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := decodeBobV040Fixture(t, test.file)
			test.mutate(document)
			raw := marshalBobFixture(t, document)
			receipt := RawReceipt{Structured: raw}
			got := ProjectReceipt(ProjectToolCall(test.tool, nil), receipt)
			assertBobFailClosed(t, got, receipt)
		})
	}
}

func TestBobV040PlaybookInputInvalidMayEchoRejectedOperation(t *testing.T) {
	for _, operation := range []string{"", "future"} {
		t.Run(operation, func(t *testing.T) {
			document := decodeBobV040Fixture(t, "playbook-missing-input-v1.json")
			document["operation"] = operation
			receipt := RawReceipt{Structured: marshalBobFixture(t, document), ToolError: true}
			got := ProjectReceipt(ProjectToolCall("bob__bob_playbook", nil), receipt)
			if got.Domain != DomainBlocked || !got.DomainTyped || got.Digest == nil ||
				got.Digest.Kind != DigestBobFailure || got.Digest.Target != "input_invalid" {
				t.Fatalf("input-invalid operation %q projection = %#v", operation, got)
			}
		})
	}
}

func TestBobV040PlaybookListAndShowExactSchemas(t *testing.T) {
	base := decodeBobV040Fixture(t, "playbook-ready-v1.json")
	plan := base["plan"].(map[string]any)
	playbook := plan["playbook"].(map[string]any)

	show := decodeBobV040Fixture(t, "playbook-ready-v1.json")
	showPlan := show["plan"].(map[string]any)
	delete(showPlan, "values")
	showPlan["truncation"].(map[string]any)["profile"] = "show"
	show["operation"] = "show"
	show["show"] = showPlan
	delete(show, "plan")

	list := map[string]any{
		"schema_version": float64(1), "ok": true, "operation": "list", "authority": base["authority"],
		"list": map[string]any{
			"schema_version": float64(1), "workspace": plan["workspace"], "recipe": plan["recipe"],
			"playbooks": []any{map[string]any{
				"id": playbook["id"], "applicable": true, "available": true, "blocked_by": []any{},
				"required_inputs": []any{"command_name"}, "scope_class": "metadata_only", "risk": "medium",
			}},
			"truncation": map[string]any{"profile": "list", "byte_limit": float64(8192), "truncated": false, "omitted": map[string]any{}},
		},
	}

	for name, test := range map[string]struct {
		document map[string]any
		state    string
	}{"list": {document: list, state: "list"}, "show": {document: show, state: "ready"}} {
		t.Run(name, func(t *testing.T) {
			receipt := RawReceipt{Structured: marshalBobFixture(t, test.document)}
			got := ProjectReceipt(ProjectToolCall("bob__bob_playbook", nil), receipt)
			if got.Domain != DomainSucceeded || !got.DomainTyped || got.Evidence != EvidenceNone ||
				got.Digest == nil || got.Digest.Kind != DigestBobPlaybook || got.Digest.State != test.state {
				t.Fatalf("%s projection = %#v", name, got)
			}
			if transient, ok := TransientModelContent(got, receipt); !ok || !strings.Contains(transient, `"operation":"`+name+`"`) {
				t.Fatalf("%s transient = %q, ok=%v", name, transient, ok)
			}
		})
	}
}

func TestBobV040BlockedPlaybookMapsToBlocked(t *testing.T) {
	document := decodeBobV040Fixture(t, "playbook-ready-v1.json")
	playbook := document["plan"].(map[string]any)["playbook"].(map[string]any)
	playbook["available"] = false
	playbook["blocked_by"] = []any{"prerequisite_missing"}
	receipt := RawReceipt{Structured: marshalBobFixture(t, document)}
	got := ProjectReceipt(ProjectToolCall("bob__bob_playbook", nil), receipt)
	if got.Domain != DomainBlocked || !got.DomainTyped || got.Evidence != EvidenceNone ||
		got.Digest == nil || got.Digest.Kind != DigestBobPlaybook {
		t.Fatalf("blocked playbook projection = %#v", got)
	}
	assertBobPersistenceSafe(t, got, receipt.Structured)
}

func TestBobV040OversizedSanitizedTransientIsRejected(t *testing.T) {
	document := decodeBobV040Fixture(t, "playbook-ready-v1.json")
	playbook := document["plan"].(map[string]any)["playbook"].(map[string]any)
	template := playbook["steps"].([]any)[0].(map[string]any)
	stepIDs := make([]string, maxTransientBobSteps)
	for index := range stepIDs {
		stepIDs[index] = fmt.Sprintf("step_%02d_%s", index, strings.Repeat("x", 80))
	}
	steps := make([]any, 0, maxTransientBobSteps)
	for index := 0; index < maxTransientBobSteps; index++ {
		step := make(map[string]any, len(template))
		for key, value := range template {
			step[key] = value
		}
		step["id"] = stepIDs[index]
		dependencies := make([]any, 0, 6)
		for dependency := 0; dependency < 6; dependency++ {
			dependencies = append(dependencies, stepIDs[dependency])
		}
		step["depends_on"] = dependencies
		steps = append(steps, step)
	}
	playbook["steps"] = steps
	receipt := RawReceipt{Structured: marshalBobFixture(t, document)}
	if len(receipt.Structured) >= maxBobGuidanceDocumentBytes {
		t.Fatalf("test receipt unexpectedly exceeds parser bound: %d", len(receipt.Structured))
	}
	got := ProjectReceipt(ProjectToolCall("bob__bob_playbook", nil), receipt)
	if got.Domain != DomainSucceeded || !got.DomainTyped || got.Digest == nil {
		t.Fatalf("valid bounded playbook projection = %#v", got)
	}
	if transient, ok := TransientModelContent(got, receipt); ok || transient != "" {
		t.Fatalf("oversized sanitized transient escaped: bytes=%d ok=%v", len(transient), ok)
	}
}

func TestBobGuidanceTruncationCannotOverflowIntoUntruncated(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	raw := json.RawMessage(fmt.Sprintf(`{"profile":"compact","byte_limit":6144,"truncated":false,"omitted":{"a":%d,"b":%d}}`, maximum, maximum))
	if validBobGuidanceTruncation(raw, "compact", 6144) {
		t.Fatal("overflowing omitted counts were accepted as untruncated")
	}
}

func TestBobV040GuidanceWrongIdentityFailsClosed(t *testing.T) {
	raw := readBobV040Fixture(t, "context-clean-v1.json")
	receipt := RawReceipt{Structured: raw}
	tests := []ToolProjection{
		ProjectToolCall("other__bob_context", nil),
		ProjectToolCall("bob__bob_path", nil),
		ProjectToolCall("Bob__bob_context", nil),
		ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{"server": "other", "tool": "bob_context"}),
		ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{"server": "bob", "tool": "Bob_context"}),
	}
	for _, call := range tests {
		t.Run(call.Route.Server+"__"+call.Route.Tool, func(t *testing.T) {
			assertBobFailClosed(t, ProjectReceipt(call, receipt), receipt)
		})
	}
}

func TestBobReceiptAlwaysClearsSeededEvidence(t *testing.T) {
	for _, test := range []struct {
		name, file string
		toolError  bool
	}{
		{name: "success", file: "context-clean-v1.json"},
		{name: "typed failure", file: "playbook-missing-input-v1.json", toolError: true},
		{name: "future schema", file: "error-unsupported-schema-v1.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := "bob_context"
			if test.file == "playbook-missing-input-v1.json" {
				operation = "bob_playbook"
			}
			projection := ProjectToolCall("bob__"+operation, nil)
			projection.Evidence = EvidenceVerified
			got := ProjectReceipt(projection, RawReceipt{Structured: readBobV040Fixture(t, test.file), ToolError: test.toolError})
			if got.Evidence != EvidenceNone {
				t.Fatalf("Bob retained caller-seeded evidence: %#v", got)
			}
		})
	}
}

func assertBobProjection(t *testing.T, got ToolProjection, want DomainState, digest ReceiptDigest) {
	t.Helper()
	digest = normalizeReceiptDigest(digest)
	if got.Transport != TransportSucceeded || got.Domain != want || !got.DomainTyped || got.Evidence != EvidenceNone {
		t.Fatalf("projection = %#v, want transport=succeeded domain=%s typed=true evidence=none", got, want)
	}
	if got.Digest == nil || !reflect.DeepEqual(*got.Digest, digest) {
		t.Fatalf("digest = %#v, want %#v", got.Digest, digest)
	}
}

func assertBobFailClosed(t *testing.T, got ToolProjection, receipt RawReceipt) {
	t.Helper()
	if got.Domain != DomainUnknown || got.DomainTyped || got.Evidence != EvidenceNone || got.Digest != nil {
		t.Fatalf("contract did not fail closed: %#v", got)
	}
	if transient, ok := TransientModelContent(got, receipt); ok || transient != "" {
		t.Fatalf("failed contract exposed transient content: %q", transient)
	}
}

func assertBobPersistenceSafe(t *testing.T, projection ToolProjection, raw []byte) {
	t.Helper()
	persisted, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	combined := string(persisted) + "\n" + SafeReceiptText(projection)
	for _, marker := range []string{"/workspace", "internal/cli/root.go", "internal/cli/hello.go", `"argv"`, `"values"`, "missing required inputs: command_name"} {
		if strings.Contains(string(raw), marker) && strings.Contains(combined, marker) {
			t.Fatalf("durable projection leaked %q: %s", marker, combined)
		}
	}
	if strings.Contains(combined, string(raw)) {
		t.Fatal("raw Bob receipt escaped into durable state")
	}
}

func readBobV040Fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "bob_v040", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodeBobV040Fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(readBobV040Fixture(t, name), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func marshalBobFixture(t *testing.T, document map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func twoDigits(value int) string {
	return string([]byte{'0' + byte(value/10), '0' + byte(value%10)})
}
