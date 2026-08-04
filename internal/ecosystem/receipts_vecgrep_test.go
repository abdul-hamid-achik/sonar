package ecosystem

import (
	"encoding/json"
	"testing"
)

func vecgrepReadiness(state string, indexed, fresh, matches bool, chunks int) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"state": state, "indexed": indexed, "fresh": fresh,
		"chunks": chunks, "profile_matches": matches,
	})
	return raw
}

func TestVecgrepReadinessProjection(t *testing.T) {
	ready := ProjectReceipt(ProjectToolCall("vecgrep__vecgrep_search", nil), RawReceipt{
		Structured: vecgrepReadiness("ready", true, true, true, 1200),
	})
	if ready.Domain != DomainSucceeded || !ready.DomainTyped || ready.Evidence != EvidenceCandidate {
		t.Fatalf("ready search projection = %+v", ready)
	}

	stale := ProjectReceipt(ProjectToolCall("vecgrep__vecgrep_search", nil), RawReceipt{
		Structured: vecgrepReadiness("stale", true, false, true, 1200),
	})
	if stale.Domain != DomainAttention || stale.Evidence != EvidenceStale || !stale.DomainTyped {
		t.Fatalf("stale search projection = %+v", stale)
	}

	// BlocksSearch states arrive with IsError=true and readiness attached;
	// the typed blocked state must survive the error coercion.
	blocked := ProjectReceipt(ProjectToolCall("vecgrep__vecgrep_search", nil), RawReceipt{
		Structured: vecgrepReadiness("profile_mismatch", true, true, false, 1200),
		ToolError:  true,
	})
	if blocked.Domain != DomainBlocked || !blocked.DomainTyped {
		t.Fatalf("profile mismatch projection = %+v", blocked)
	}

	empty := ProjectReceipt(ProjectToolCall("vecgrep__vecgrep_ensure", nil), RawReceipt{
		Structured: vecgrepReadiness("empty", false, false, true, 0),
	})
	if empty.Domain != DomainBlocked {
		t.Fatalf("empty index projection = %+v", empty)
	}

	unknown := ProjectReceipt(ProjectToolCall("vecgrep__vecgrep_status", nil), RawReceipt{
		Structured: vecgrepReadiness("unknown", true, false, true, 900),
	})
	if unknown.Domain != DomainAttention || unknown.Evidence != EvidenceNone {
		t.Fatalf("unknown freshness projection = %+v", unknown)
	}
}

func TestVecgrepProjectionFailsClosed(t *testing.T) {
	// A ready claim contradicting its own facts is not a recognized contract.
	contradictory := ProjectReceipt(ProjectToolCall("vecgrep__vecgrep_search", nil), RawReceipt{
		Structured: vecgrepReadiness("ready", true, false, true, 10),
	})
	if contradictory.Domain != DomainUnknown || contradictory.DomainTyped {
		t.Fatalf("contradictory ready must fail closed, got %+v", contradictory)
	}

	// Unknown state values and missing required fields stay unknown.
	badState := ProjectReceipt(ProjectToolCall("vecgrep__vecgrep_search", nil), RawReceipt{
		Structured: vecgrepReadiness("warming", true, true, true, 10),
	})
	if badState.Domain != DomainUnknown {
		t.Fatalf("unrecognized state must fail closed, got %+v", badState)
	}
	partial := ProjectReceipt(ProjectToolCall("vecgrep__vecgrep_status", nil), RawReceipt{
		Structured: json.RawMessage(`{"state":"ready","indexed":true}`),
	})
	if partial.Domain != DomainUnknown {
		t.Fatalf("incomplete readiness must fail closed, got %+v", partial)
	}

	// Tools without the readiness contract stay unknown instead of
	// inheriting a generic success.
	index := ProjectReceipt(ProjectToolCall("vecgrep__vecgrep_index", nil), RawReceipt{
		Structured: json.RawMessage(`{"chunks_indexed":42}`),
	})
	if index.Domain != DomainUnknown || index.DomainTyped {
		t.Fatalf("structured-less tool must stay unknown, got %+v", index)
	}
}

func TestCodemapAnnotateAndDoctorProjection(t *testing.T) {
	created := ProjectReceipt(ProjectToolCall("codemap__codemap_annotate", nil), RawReceipt{
		Structured: json.RawMessage(`{"id":7,"kind":"note","target":"pkg.Func","matched":true,"action":"created","external_id":"incident:iss_1"}`),
	})
	if created.Domain != DomainSucceeded || !created.DomainTyped || created.Evidence != EvidenceSupported {
		t.Fatalf("annotate created projection = %+v", created)
	}

	unmatched := ProjectReceipt(ProjectToolCall("codemap__codemap_annotate", nil), RawReceipt{
		Structured: json.RawMessage(`{"id":8,"kind":"note","target":"pkg.Gone","matched":false,"action":"created"}`),
	})
	if unmatched.Domain != DomainAttention {
		t.Fatalf("unmatched annotate projection = %+v", unmatched)
	}

	badAction := ProjectReceipt(ProjectToolCall("codemap__codemap_annotate", nil), RawReceipt{
		Structured: json.RawMessage(`{"target":"pkg.Func","matched":true,"action":"replaced"}`),
	})
	if badAction.Domain != DomainUnknown {
		t.Fatalf("unknown annotate action must fail closed, got %+v", badAction)
	}

	healthy := ProjectReceipt(ProjectToolCall("codemap__codemap_doctor", nil), RawReceipt{
		Structured: json.RawMessage(`{"data_dir":"/x","checks":[{"name":"sqlite","ok":true},{"name":"gopls","ok":true}]}`),
	})
	if healthy.Domain != DomainSucceeded || !healthy.DomainTyped {
		t.Fatalf("healthy doctor projection = %+v", healthy)
	}

	failing := ProjectReceipt(ProjectToolCall("codemap__codemap_doctor", nil), RawReceipt{
		Structured: json.RawMessage(`{"checks":[{"name":"gopls","ok":false,"code":"lsp_shim","agent_fix":["codemap doctor"]}]}`),
	})
	if failing.Domain != DomainAttention {
		t.Fatalf("failing doctor projection = %+v", failing)
	}
}

func TestCodemapBatchImpactProjection(t *testing.T) {
	clean := ProjectReceipt(ProjectToolCall("codemap__codemap_impact", nil), RawReceipt{
		Structured: json.RawMessage(`{"project":"p","indexed":true,"requested":2,"processed":2,"truncated":false,"results":[{"symbol":"A"},{"symbol":"B"}]}`),
	})
	if clean.Domain != DomainSucceeded || !clean.DomainTyped {
		t.Fatalf("clean batch projection = %+v", clean)
	}

	partial := ProjectReceipt(ProjectToolCall("codemap__codemap_impact", nil), RawReceipt{
		Structured: json.RawMessage(`{"project":"p","indexed":true,"requested":2,"processed":2,"truncated":false,"results":[{"symbol":"A"},{"error":{"code":"symbol_not_found","message":"no symbol"}}]}`),
	})
	if partial.Domain != DomainAttention {
		t.Fatalf("partial batch projection = %+v", partial)
	}

	truncated := ProjectReceipt(ProjectToolCall("codemap__codemap_impact", nil), RawReceipt{
		Structured: json.RawMessage(`{"project":"p","indexed":true,"requested":30,"processed":25,"truncated":true,"results":[{"symbol":"A"}]}`),
	})
	if truncated.Domain != DomainAttention {
		t.Fatalf("truncated batch projection = %+v", truncated)
	}

	unindexed := ProjectReceipt(ProjectToolCall("codemap__codemap_impact", nil), RawReceipt{
		Structured: json.RawMessage(`{"project":"p","indexed":false,"requested":1,"processed":0,"truncated":false,"results":[]}`),
	})
	if unindexed.Domain != DomainBlocked {
		t.Fatalf("unindexed batch projection = %+v", unindexed)
	}
}
