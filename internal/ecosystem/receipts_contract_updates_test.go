package ecosystem

import (
	"encoding/json"
	"testing"
)

func TestBobRecipeRefAcceptsGoAgentToolV5(t *testing.T) {
	for version, want := range map[int]bool{2: false, 3: true, 4: true, 5: true, 6: false} {
		raw, _ := json.Marshal(map[string]any{"id": "go-agent-tool", "version": version})
		if _, ok := validBobRecipeRef(raw); ok != want {
			t.Fatalf("go-agent-tool v%d accepted=%v, want %v", version, ok, want)
		}
	}
}

func TestBobRecipeRefAcceptsStackRecipes(t *testing.T) {
	for _, test := range []struct {
		id      string
		version int
		want    bool
	}{
		{"ts-app", 1, true},
		{"ts-app", 2, true},
		{"ts-app", 3, false},
		{"rust-cli", 2, true},
		{"static-web", 1, true},
		// Swift and Elixir only exist from contract version 2.
		{"swift-package", 1, false},
		{"swift-package", 2, true},
		{"elixir-app", 2, true},
		{"not-a-recipe", 2, false},
	} {
		raw, _ := json.Marshal(map[string]any{"id": test.id, "version": test.version})
		if _, ok := validBobRecipeRef(raw); ok != test.want {
			t.Fatalf("%s v%d accepted=%v, want %v", test.id, test.version, ok, test.want)
		}
	}
}

func bobStackManifestDocument(recipe, language, kind, packageManager, docs string, goreleaser bool) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"schema_version": 1,
		"recipe":         recipe,
		"product": map[string]any{
			"name": "storm-probe", "module": "", "description": "A hygiene-managed repository.",
			"visibility": "", "license": "",
		},
		"runtime": map[string]any{"language": language, "kind": kind, "package_manager": packageManager},
		"surfaces": map[string]any{
			"cli": false, "json": false, "mcp": false, "studio": false,
		},
		"integrations": map[string]any{
			"code_structure": "", "semantic_search": "", "terminal_verification": "",
			"browser_verification": "", "secrets": "", "artifacts": "",
		},
		"distribution": map[string]any{
			"github_actions": true, "goreleaser": goreleaser, "homebrew": false, "docs": docs,
		},
	})
	return raw
}

func TestBobStackManifestValidation(t *testing.T) {
	if !validBobManifest(bobStackManifestDocument("rust-cli", "rust", "cli", "", "none", false), "rust-cli") {
		t.Fatal("a canonical rust-cli manifest must validate")
	}
	if !validBobManifest(bobStackManifestDocument("ts-app", "typescript", "monorepo", "pnpm", "", false), "ts-app") {
		t.Fatal("ts-app with a closed-set package manager must validate")
	}
	if !validBobManifest(bobStackManifestDocument("swift-package", "swift", "package", "", "none", false), "swift-package") {
		t.Fatal("a canonical swift-package manifest must validate")
	}
	for name, document := range map[string]json.RawMessage{
		"wrong language":            bobStackManifestDocument("rust-cli", "go", "cli", "", "none", false),
		"wrong kind":                bobStackManifestDocument("python-app", "python", "gem", "", "none", false),
		"manager outside js family": bobStackManifestDocument("rust-cli", "rust", "cli", "cargo", "none", false),
		"unknown js manager":        bobStackManifestDocument("ts-app", "typescript", "app", "deno", "none", false),
		"goreleaser forbidden":      bobStackManifestDocument("rust-cli", "rust", "cli", "", "none", true),
		"docs site forbidden":       bobStackManifestDocument("rust-cli", "rust", "cli", "", "vitepress", false),
	} {
		if validBobManifest(document, "rust-cli") && name == "wrong language" {
			t.Fatalf("%s: stack manifest must fail closed", name)
		}
		var manifest struct {
			Recipe string `json:"recipe"`
		}
		_ = json.Unmarshal(document, &manifest)
		if validBobManifest(document, manifest.Recipe) {
			t.Fatalf("%s: stack manifest must fail closed", name)
		}
	}
}

func TestBobQualifiedPlanDigestBindsToLegacyDigest(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !validBobQualifiedPlanDigest("", digest) {
		t.Fatal("absent qualified digest must stay valid for older bob releases")
	}
	if !validBobQualifiedPlanDigest("sha256:"+digest, digest) {
		t.Fatal("matching qualified digest must be accepted")
	}
	if validBobQualifiedPlanDigest("sha256:"+digest, "f"+digest[1:]) {
		t.Fatal("a qualified digest disagreeing with the legacy field must fail closed")
	}
	if validBobQualifiedPlanDigest(digest, digest) {
		t.Fatal("an unprefixed qualified digest is not the documented spelling")
	}
}

func TestCortexHandoffProjection(t *testing.T) {
	handoff := ProjectReceipt(ProjectToolCall("cortex__cortex_handoff", nil), RawReceipt{
		Structured: json.RawMessage(`{"schemaVersion":1,"generatedAt":"2026-07-27T00:00:00Z","taskId":"task_a1","revision":8,"goal":"repair callback contract","phase":"complete","mode":"change","risk":"low","actor":"agent-a"}`),
	})
	if handoff.Domain != DomainSucceeded || !handoff.DomainTyped || handoff.Evidence != EvidenceNone {
		t.Fatalf("handoff projection = %+v", handoff)
	}
	if handoff.Digest == nil || handoff.Digest.Kind != DigestCortexReceipt || handoff.Digest.Target != "task_a1" {
		t.Fatalf("handoff digest = %+v", handoff.Digest)
	}

	wrongVersion := ProjectReceipt(ProjectToolCall("cortex__cortex_handoff", nil), RawReceipt{
		Structured: json.RawMessage(`{"schemaVersion":2,"taskId":"task_a1","revision":8,"phase":"complete"}`),
	})
	if wrongVersion.Domain != DomainUnknown {
		t.Fatalf("future handoff schema must fail closed, got %+v", wrongVersion)
	}

	missingPhase := ProjectReceipt(ProjectToolCall("cortex__cortex_handoff", nil), RawReceipt{
		Structured: json.RawMessage(`{"schemaVersion":1,"taskId":"task_a1","revision":8}`),
	})
	if missingPhase.Domain != DomainUnknown {
		t.Fatalf("incomplete handoff must fail closed, got %+v", missingPhase)
	}
}

func TestMCPHubDetachedCallLifecycle(t *testing.T) {
	accepted := ProjectReceipt(ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "hitspec", "tool": "capture_webpage",
	}), RawReceipt{
		Structured: json.RawMessage(`{"status":"accepted","callId":"call_9f","server":"hitspec","tool":"capture_webpage","namespaced":"hitspec__capture_webpage","timeoutMs":600000,"nextAction":"mcphub_poll_result"}`),
	})
	if accepted.Domain != DomainPending || !accepted.DomainTyped {
		t.Fatalf("accepted projection = %+v", accepted)
	}
	if accepted.Digest == nil || accepted.Digest.Kind != DigestMCPHubDetached || accepted.Digest.State != "accepted" {
		t.Fatalf("accepted digest = %+v", accepted.Digest)
	}
	if accepted.Route.CallID != "call_9f" {
		t.Fatalf("accepted route = %+v", accepted.Route)
	}

	pending := ProjectReceipt(ProjectToolCall("mcphub__mcphub_poll_result", map[string]any{"callId": "call_9f"}), RawReceipt{
		Structured: json.RawMessage(`{"status":"pending","callId":"call_9f","namespaced":"hitspec__capture_webpage","elapsedMs":1500,"hint":"poll again"}`),
	})
	if pending.Domain != DomainPending || pending.Digest == nil || pending.Digest.State != "pending" {
		t.Fatalf("pending projection = %+v digest=%+v", pending, pending.Digest)
	}

	failed := ProjectReceipt(ProjectToolCall("mcphub__mcphub_poll_result", map[string]any{"callId": "call_9f"}), RawReceipt{
		Structured: json.RawMessage(`{"status":"failed","callId":"call_9f","namespaced":"hitspec__capture_webpage","error":"downstream exited","elapsedMs":9000}`),
	})
	if failed.Domain != DomainFailed || failed.Digest == nil || failed.Digest.State != "failed" {
		t.Fatalf("failed projection = %+v digest=%+v", failed, failed.Digest)
	}

	lost := ProjectReceipt(ProjectToolCall("mcphub__mcphub_poll_result", map[string]any{"callId": "call_gone"}), RawReceipt{
		Structured: json.RawMessage(`{"status":"unknown","callId":"call_gone","reason":"call id not found"}`),
	})
	if lost.Domain != DomainAttention || lost.Digest == nil || lost.Digest.State != "unknown" {
		t.Fatalf("unknown projection = %+v digest=%+v", lost, lost.Digest)
	}
}

func TestFileCheapReadOperationsProjection(t *testing.T) {
	list := ProjectReceipt(ProjectToolCall("fcheap__fcheap_list", nil), RawReceipt{
		Structured: json.RawMessage(`{"result":[{"id":"stash_a","name":"a","tags":["x"],"file_count":3,"total_size":900,"created_at":"2026-07-27T00:00:00Z","bundle_type":"generic"}]}`),
	})
	if list.Domain != DomainSucceeded || !list.DomainTyped || list.Evidence != EvidenceSupported {
		t.Fatalf("list projection = %+v", list)
	}
	emptyList := ProjectReceipt(ProjectToolCall("fcheap__fcheap_list", nil), RawReceipt{
		Structured: json.RawMessage(`{"result":[]}`),
	})
	if emptyList.Domain != DomainSucceeded {
		t.Fatalf("empty list projection = %+v", emptyList)
	}
	badEntry := ProjectReceipt(ProjectToolCall("fcheap__fcheap_list", nil), RawReceipt{
		Structured: json.RawMessage(`{"result":[{"id":"../escape","file_count":1,"total_size":1,"created_at":"x"}]}`),
	})
	if badEntry.Domain != DomainUnknown {
		t.Fatalf("malformed list entry must fail closed, got %+v", badEntry)
	}

	info := ProjectReceipt(ProjectToolCall("fcheap__fcheap_info", nil), RawReceipt{
		Structured: json.RawMessage(`{"schema_version":"1.0","id":"stash_a","created_at":"2026-07-27T00:00:00Z","file_count":3,"total_size":900,"content_hash":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`),
	})
	if info.Domain != DomainSucceeded || !info.DomainTyped {
		t.Fatalf("info projection = %+v", info)
	}

	indexed := ProjectReceipt(ProjectToolCall("fcheap__fcheap_analyze", nil), RawReceipt{
		Structured: json.RawMessage(`{"stash_id":"stash_a","status":"indexed","bundle_type":"monitor.incident","files_indexed":6}`),
	})
	if indexed.Domain != DomainSucceeded || !indexed.DomainTyped {
		t.Fatalf("analyze projection = %+v", indexed)
	}
	searchFailed := ProjectReceipt(ProjectToolCall("fcheap__fcheap_analyze", nil), RawReceipt{
		Structured: json.RawMessage(`{"stash_id":"stash_a","status":"indexed","bundle_type":"generic","files_indexed":6,"search_error":"embedding provider unavailable"}`),
	})
	if searchFailed.Domain != DomainAttention {
		t.Fatalf("analyze with search_error projection = %+v", searchFailed)
	}

	dropped := ProjectReceipt(ProjectToolCall("fcheap__fcheap_drop", nil), RawReceipt{
		Structured: json.RawMessage(`{"stash_id":"stash_a","status":"dropped","failed":[]}`),
	})
	if dropped.Domain != DomainSucceeded {
		t.Fatalf("drop projection = %+v", dropped)
	}
	droppedPartial := ProjectReceipt(ProjectToolCall("fcheap__fcheap_drop", nil), RawReceipt{
		Structured: json.RawMessage(`{"stash_id":"stash_a","status":"dropped_with_failures","failed":[{"id":"stash_a","stage":"index","error":"vecgrep offline"}]}`),
		ToolError:  true,
	})
	if droppedPartial.Domain != DomainAttention {
		t.Fatalf("partial drop projection = %+v", droppedPartial)
	}
	inconsistentDrop := ProjectReceipt(ProjectToolCall("fcheap__fcheap_drop", nil), RawReceipt{
		Structured: json.RawMessage(`{"stash_id":"stash_a","status":"dropped","failed":[{"id":"stash_a"}]}`),
	})
	if inconsistentDrop.Domain != DomainUnknown {
		t.Fatalf("dropped-with-nonempty-failures must fail closed, got %+v", inconsistentDrop)
	}

	ttl := ProjectReceipt(ProjectToolCall("fcheap__fcheap_ttl", nil), RawReceipt{
		Structured: json.RawMessage(`{"stash_id":"stash_a","expires_at":""}`),
	})
	if ttl.Domain != DomainSucceeded {
		t.Fatalf("cleared ttl projection = %+v", ttl)
	}
}

func TestMCPHubDetachedEnvelopeFailsClosed(t *testing.T) {
	// An acceptance without a positive timeout is not the published contract.
	badTimeout := ProjectReceipt(ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "hitspec", "tool": "capture_webpage",
	}), RawReceipt{
		Structured: json.RawMessage(`{"status":"accepted","callId":"call_9f","timeoutMs":0}`),
	})
	if badTimeout.Digest != nil && badTimeout.Digest.Kind == DigestMCPHubDetached {
		t.Fatalf("invalid acceptance must not produce a detached digest: %+v", badTimeout)
	}

	// Poll-state envelopes are only trusted on the exact management route,
	// never when echoed by an arbitrary downstream tool.
	echoed := ProjectReceipt(ProjectToolCall("hitspec__hitspec_fetch", nil), RawReceipt{
		Structured: json.RawMessage(`{"status":"pending","callId":"call_9f","elapsedMs":10}`),
	})
	if echoed.Digest != nil && echoed.Digest.Kind == DigestMCPHubDetached {
		t.Fatalf("downstream echo must not gain the detached parser: %+v", echoed)
	}
}
