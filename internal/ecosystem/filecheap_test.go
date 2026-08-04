package ecosystem

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const (
	fileCheapTestStashID = "report_20260713_123456.000000000_0123456789abcdef01234567"
	fileCheapTestHash    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestProjectFileCheapSaveReceiptRecognizesDirectManifestContract(t *testing.T) {
	projection := ProjectToolCall("filecheap__fcheap_save", nil)
	got := ProjectReceipt(projection, RawReceipt{
		Structured: json.RawMessage(fileCheapSaveDocument("")),
	})

	if got.Specialist != "filecheap" || got.Role != RoleArtifact || got.Transport != TransportSucceeded ||
		got.Domain != DomainSucceeded || got.Evidence != EvidenceSupported || !got.Successful() {
		t.Fatalf("save projection = %#v", got)
	}
	assertFileCheapArtifact(t, got.Artifact)
	if got.Evidence == EvidenceVerified {
		t.Fatal("a save receipt must never become verified evidence")
	}
	if summary := got.SummaryText(); summary != fileCheapStashURI(fileCheapTestStashID)+" · 2 files · 1234 bytes" {
		t.Fatalf("summary = %q", summary)
	}
}

func TestProjectFileCheapSaveReceiptRecognizesMCPHubRoute(t *testing.T) {
	projection := ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "fcheap",
		"tool":   "fcheap_save",
		"arguments": map[string]any{
			"path": "/private/value-that-must-not-persist",
		},
	})
	got := ProjectReceipt(projection, RawReceipt{
		Structured: json.RawMessage(fileCheapSaveDocument("")),
	})

	if got.Specialist != "fcheap" || got.Route.Gateway != "mcphub" || !got.Route.Lazy ||
		got.Domain != DomainSucceeded || got.Evidence != EvidenceSupported {
		t.Fatalf("routed save projection = %#v", got)
	}
	assertFileCheapArtifact(t, got.Artifact)
	assertProjectionOmits(t, got, "/private/value-that-must-not-persist")
}

func TestProjectFileCheapSaveReceiptProjectsWarningsWithoutLeakingDetails(t *testing.T) {
	const (
		warningSecret = "SECRET_WARNING_DETAIL"
		findingSecret = "SECRET_FINDING_PATH"
		indexSecret   = "SECRET_INDEX_ERROR"
	)
	warning := ProjectReceipt(ProjectToolCall("fcheap__fcheap_save", nil), RawReceipt{
		Structured: json.RawMessage(fileCheapSaveDocument("," +
			`"secrets_warning":"` + warningSecret + `",` +
			`"secrets":[{"file":"` + findingSecret + `","rule":"credential"}]`)),
	})
	if warning.Domain != DomainAttention || warning.Evidence != EvidenceSupported ||
		warning.Artifact == nil || !warning.Artifact.SecretsWarning || warning.Artifact.IndexingFailed ||
		warning.Successful() || !warning.NeedsAttention() {
		t.Fatalf("secrets warning projection = %#v", warning)
	}
	for _, forbidden := range []string{warningSecret, findingSecret, "/private/source/SECRET_SOURCE", "SECRET_FILE_HASH"} {
		assertProjectionOmits(t, warning, forbidden)
	}
	if summary := warning.SummaryText(); !strings.Contains(summary, "potential secrets need review") {
		t.Fatalf("warning summary = %q", summary)
	}

	indexFailure := ProjectReceipt(ProjectToolCall("fcheap__fcheap_save", nil), RawReceipt{
		Structured: json.RawMessage(fileCheapSaveDocument("," + `"index_error":"` + indexSecret + `"`)),
	})
	if indexFailure.Domain != DomainSucceeded || indexFailure.Evidence != EvidenceSupported ||
		indexFailure.Artifact == nil || !indexFailure.Artifact.IndexingFailed || !indexFailure.Successful() {
		t.Fatalf("index failure projection = %#v", indexFailure)
	}
	assertProjectionOmits(t, indexFailure, indexSecret)
	if summary := indexFailure.SummaryText(); !strings.Contains(summary, "saved; indexing incomplete") {
		t.Fatalf("index failure summary = %q", summary)
	}
}

func TestProjectFileCheapSaveReceiptDropsArtifactOnApplicationFailure(t *testing.T) {
	got := ProjectReceipt(ProjectToolCall("fcheap__fcheap_save", nil), RawReceipt{
		Structured: json.RawMessage(fileCheapSaveDocument("")),
		ToolError:  true,
	})
	if got.Transport != TransportSucceeded || got.Domain != DomainFailed || got.Artifact != nil || got.Successful() {
		t.Fatalf("application-level save failure retained an artifact: %#v", got)
	}

	warning := ProjectReceipt(ProjectToolCall("fcheap__fcheap_save", nil), RawReceipt{
		Structured: json.RawMessage(fileCheapSaveDocument(`,"secrets_warning":"review","secrets":[{"file":"private","rule":"credential"}]`)),
	})
	if warning.Domain != DomainAttention || warning.Artifact == nil || !warning.Artifact.SecretsWarning {
		t.Fatalf("attention save lost its listable artifact: %#v", warning)
	}
}

func TestProjectFileCheapReceiptIsOperationSpecificAndFailClosed(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		document  string
	}{
		{name: "save manifest under list", operation: "fcheap_list", document: fileCheapSaveDocument("")},
		{name: "legacy invented save status", operation: "fcheap_save", document: `{"status":"saved","verified":true}`},
		{name: "missing manifest", operation: "fcheap_save", document: `{}`},
		{name: "future schema", operation: "fcheap_save", document: fileCheapManifestDocument("2.0", fileCheapTestStashID, fileCheapTestHash, "2026-07-13T12:34:56Z", `2`, `1234`)},
		{name: "unsafe id", operation: "fcheap_save", document: fileCheapManifestDocument("1.0", "../../secret", fileCheapTestHash, "2026-07-13T12:34:56Z", `2`, `1234`)},
		{name: "invalid hash", operation: "fcheap_save", document: fileCheapManifestDocument("1.0", fileCheapTestStashID, strings.ToUpper(fileCheapTestHash), "2026-07-13T12:34:56Z", `2`, `1234`)},
		{name: "invalid created at", operation: "fcheap_save", document: fileCheapManifestDocument("1.0", fileCheapTestStashID, fileCheapTestHash, "not-a-time", `2`, `1234`)},
		{name: "negative file count", operation: "fcheap_save", document: fileCheapManifestDocument("1.0", fileCheapTestStashID, fileCheapTestHash, "2026-07-13T12:34:56Z", `-1`, `1234`)},
		{name: "missing total size", operation: "fcheap_save", document: fileCheapManifestDocument("1.0", fileCheapTestStashID, fileCheapTestHash, "2026-07-13T12:34:56Z", `2`, `null`)},
		{name: "warning without findings", operation: "fcheap_save", document: fileCheapSaveDocument(`,"secrets_warning":"review"`)},
		{name: "index success and error conflict", operation: "fcheap_save", document: fileCheapSaveDocument(`,"indexed":{},"index_error":"failed"`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("fcheap__"+test.operation, nil), RawReceipt{
				Structured: json.RawMessage(test.document),
			})
			if got.Transport != TransportSucceeded || got.Domain != DomainUnknown || got.Evidence != EvidenceNone || got.Artifact != nil || got.Successful() {
				t.Fatalf("malformed projection = %#v", got)
			}
		})
	}
}

func TestProjectFileCheapRestoreReceiptUsesOnlyRestoreContract(t *testing.T) {
	verified := ProjectReceipt(ProjectToolCall("fcheap__fcheap_restore", nil), RawReceipt{
		Structured: json.RawMessage(`{"stash_id":"` + fileCheapTestStashID + `","target":"/tmp/restore","file_count":2,"verified":true,"mismatches":[],"status":"restored"}`),
	})
	if verified.Domain != DomainSucceeded || verified.Evidence != EvidenceVerified || !verified.Successful() || verified.Artifact != nil {
		t.Fatalf("verified restore projection = %#v", verified)
	}

	inconsistent := ProjectReceipt(ProjectToolCall("fcheap__fcheap_restore", nil), RawReceipt{
		Structured: json.RawMessage(`{"stash_id":"` + fileCheapTestStashID + `","target":"/tmp/restore","file_count":2,"verified":false,"mismatches":[],"status":"restored"}`),
	})
	if inconsistent.Domain != DomainUnknown || inconsistent.Evidence != EvidenceNone || inconsistent.Successful() {
		t.Fatalf("inconsistent restore projection = %#v", inconsistent)
	}
}

func TestFileCheapArtifactProjectionRoundTripsAndReDerivesURI(t *testing.T) {
	original := ProjectReceipt(ProjectToolCall("fcheap__fcheap_save", nil), RawReceipt{
		Structured: json.RawMessage(fileCheapSaveDocument(`,"index_error":"temporary failure"`)),
	})
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored ToolProjection
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	restored = restored.Normalize()
	if !reflect.DeepEqual(original, restored) {
		t.Fatalf("round trip mismatch\noriginal: %#v\nrestored: %#v", original, restored)
	}

	restored.Artifact.URI = "https://attacker.invalid/SECRET_URI"
	restored = restored.Normalize()
	if restored.Artifact == nil || restored.Artifact.URI != fileCheapStashURI(fileCheapTestStashID) {
		t.Fatalf("artifact URI was not re-derived: %#v", restored.Artifact)
	}
	assertProjectionOmits(t, restored, "SECRET_URI")

	restored.Artifact.ID = "../escape"
	if invalid := restored.Normalize(); invalid.Artifact != nil {
		t.Fatalf("unsafe restored artifact survived normalization: %#v", invalid.Artifact)
	}
}

func assertFileCheapArtifact(t *testing.T, artifact *ArtifactDigest) {
	t.Helper()
	if artifact == nil {
		t.Fatal("missing file.cheap artifact digest")
	}
	if artifact.Kind != ArtifactDigestFileCheapStash || artifact.ID != fileCheapTestStashID ||
		artifact.URI != fileCheapStashURI(fileCheapTestStashID) || artifact.SchemaVersion != "1.0" ||
		artifact.ContentSHA256 != fileCheapTestHash || artifact.FileCount != 2 || artifact.TotalSize != 1234 ||
		artifact.CreatedAt != "2026-07-13T12:34:56Z" {
		t.Fatalf("artifact digest = %#v", artifact)
	}
}

func fileCheapSaveDocument(extra string) string {
	return fileCheapManifestDocument("1.0", fileCheapTestStashID, fileCheapTestHash, "2026-07-13T12:34:56Z", `2`, `1234`, extra)
}

func fileCheapManifestDocument(schema, id, hash, createdAt, fileCount, totalSize string, extra ...string) string {
	topLevel := ""
	if len(extra) > 0 {
		topLevel = extra[0]
	}
	return `{"manifest":{` +
		`"schema_version":"` + schema + `",` +
		`"id":"` + id + `",` +
		`"created_at":"` + createdAt + `",` +
		`"source_path":"/private/source/SECRET_SOURCE",` +
		`"file_count":` + fileCount + `,` +
		`"total_size":` + totalSize + `,` +
		`"content_hash":"` + hash + `",` +
		`"files":[{"path":"SECRET_FILE_PATH","size":1234,"hash":"SECRET_FILE_HASH"}],` +
		`"tags":["SECRET_TAG"],"custom":{"token":"SECRET_CUSTOM"}` +
		`}` + topLevel + `}`
}
