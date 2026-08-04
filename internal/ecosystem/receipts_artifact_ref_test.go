package ecosystem

import (
	"encoding/json"
	"testing"
)

// The fixtures below are byte-for-byte copies of file.cheap's published
// contract corpus (contracts/artifact-ref/v1/valid/), so parser drift against
// the canonical schema fails here first.
const (
	artifactRefLocalFixture = `{
  "$schema": "urn:filecheap.dev:artifact-ref:v1",
  "version": 1,
  "provider": "fcheap-local",
  "uri": "fcheap://stash/report_20260723_184500.123456789_0123456789abcdef01234567",
  "artifact_id": "report_20260723_184500.123456789_0123456789abcdef01234567",
  "kind": "filecheap.stash"
}`
	artifactRefCloudFixture = `{
  "$schema": "urn:filecheap.dev:artifact-ref:v1",
  "version": 1,
  "provider": "fcheap-cloud",
  "uri": "fcheap://cloud/vaults/vlt_01/artifacts/art_01",
  "artifact_id": "art_01",
  "kind": "cairntrace.run",
  "producer": {
    "tool": "cairntrace",
    "version": "1.8.0",
    "native_schema": "urn:cairntrace.dev:run:v1",
    "native_id": "run_01",
    "entrypoint": "run.json"
  },
  "web_url": "https://artifacts.example/artifacts/art_01"
}`
	artifactRefLinkFixture = `{
  "$schema": "urn:filecheap.dev:artifact-ref:v1",
  "version": 1,
  "provider": "link",
  "uri": "https://artifacts.example.com/reports/report-01",
  "kind": "chalupa.report",
  "producer": {
    "tool": "chalupa",
    "version": "0.9.0",
    "native_schema": "urn:chalupa.dev:report:v1",
    "native_id": "report_01"
  }
}`
)

func TestFileCheapArtifactRefProjectsPortableDigest(t *testing.T) {
	local := ProjectReceipt(ProjectToolCall("fcheap__fcheap_artifact_ref", nil), RawReceipt{
		Structured: json.RawMessage(artifactRefLocalFixture),
	})
	if local.Domain != DomainSucceeded || !local.DomainTyped || local.Evidence != EvidenceSupported {
		t.Fatalf("local ref projection = %+v", local)
	}
	if local.Artifact == nil || local.Artifact.Kind != ArtifactDigestPortableRef ||
		local.Artifact.Provider != "fcheap-local" || local.Artifact.RefKind != "filecheap.stash" ||
		local.Artifact.URI != "fcheap://stash/report_20260723_184500.123456789_0123456789abcdef01234567" {
		t.Fatalf("local ref digest = %+v", local.Artifact)
	}

	cloud := ProjectReceipt(ProjectToolCall("fcheap__fcheap_artifact_ref", nil), RawReceipt{
		Structured: json.RawMessage(artifactRefCloudFixture),
	})
	if cloud.Domain != DomainSucceeded || cloud.Evidence != EvidenceNone {
		t.Fatalf("cloud ref projection = %+v", cloud)
	}
	if cloud.Artifact == nil || cloud.Artifact.URI != "" || cloud.Artifact.ID != "art_01" ||
		cloud.Artifact.RefKind != "cairntrace.run" {
		t.Fatalf("cloud ref digest must drop the wire URI: %+v", cloud.Artifact)
	}

	link := ProjectReceipt(ProjectToolCall("fcheap__fcheap_artifact_ref", nil), RawReceipt{
		Structured: json.RawMessage(artifactRefLinkFixture),
	})
	if link.Domain != DomainSucceeded || link.Evidence != EvidenceNone {
		t.Fatalf("link ref projection = %+v", link)
	}
	if link.Artifact == nil || link.Artifact.ID != "" || link.Artifact.URI != "" ||
		link.Artifact.RefKind != "chalupa.report" {
		t.Fatalf("link ref digest = %+v", link.Artifact)
	}
}

func TestArtifactRefRejectsForeignAndMalformedEnvelopes(t *testing.T) {
	for name, document := range map[string]string{
		"wrong schema":       `{"$schema":"urn:other:v1","version":1,"provider":"fcheap-local","uri":"fcheap://stash/x","artifact_id":"x","kind":"a.b"}`,
		"wrong version":      `{"$schema":"urn:filecheap.dev:artifact-ref:v1","version":2,"provider":"fcheap-local","uri":"fcheap://stash/x","artifact_id":"x","kind":"a.b"}`,
		"uri identity split": `{"$schema":"urn:filecheap.dev:artifact-ref:v1","version":1,"provider":"fcheap-local","uri":"fcheap://stash/other","artifact_id":"x","kind":"a.b"}`,
		"invalid kind":       `{"$schema":"urn:filecheap.dev:artifact-ref:v1","version":1,"provider":"fcheap-local","uri":"fcheap://stash/x","artifact_id":"x","kind":"Bad..Kind"}`,
		"unknown provider":   `{"$schema":"urn:filecheap.dev:artifact-ref:v1","version":1,"provider":"s3","uri":"s3://bucket/key","kind":"a.b"}`,
		"link with id":       `{"$schema":"urn:filecheap.dev:artifact-ref:v1","version":1,"provider":"link","uri":"https://a.example/x","artifact_id":"x","kind":"a.b"}`,
	} {
		projection := ProjectReceipt(ProjectToolCall("fcheap__fcheap_artifact_ref", nil), RawReceipt{
			Structured: json.RawMessage(document),
		})
		if projection.Domain != DomainUnknown || projection.Artifact != nil {
			t.Fatalf("%s: expected fail-closed unknown, got %+v", name, projection)
		}
	}
}

func TestMonitorIssueIntelligenceProjection(t *testing.T) {
	openList := ProjectReceipt(ProjectToolCall("monitor__monitor_issues", nil), RawReceipt{
		Structured: json.RawMessage(`{"issues":[{"id":"iss_1","status":"open"},{"id":"iss_2","status":"resolved"}],"total":2,"truncated":false}`),
	})
	if openList.Domain != DomainAttention || !openList.DomainTyped || openList.Evidence != EvidenceSupported {
		t.Fatalf("open issues projection = %+v", openList)
	}

	settledList := ProjectReceipt(ProjectToolCall("monitor__monitor_issues", nil), RawReceipt{
		Structured: json.RawMessage(`{"issues":[{"id":"iss_2","status":"resolved"}],"total":1,"truncated":false}`),
	})
	if settledList.Domain != DomainSucceeded {
		t.Fatalf("settled issues projection = %+v", settledList)
	}

	unknownStatus := ProjectReceipt(ProjectToolCall("monitor__monitor_issues", nil), RawReceipt{
		Structured: json.RawMessage(`{"issues":[{"id":"iss_3","status":"snoozed"}],"total":1,"truncated":false}`),
	})
	if unknownStatus.Domain != DomainUnknown {
		t.Fatalf("unrecognized status must fail closed, got %+v", unknownStatus)
	}

	openIssue := ProjectReceipt(ProjectToolCall("monitor__monitor_issue", nil), RawReceipt{
		Structured: json.RawMessage(`{"issue":{"id":"iss_1","status":"open"},"occurrences":[{"pid":42}],"occurrences_truncated":false}`),
	})
	if openIssue.Domain != DomainAttention || !openIssue.DomainTyped {
		t.Fatalf("open issue projection = %+v", openIssue)
	}
}

func TestMonitorInvestigateCapturesArtifactRef(t *testing.T) {
	complete := ProjectReceipt(ProjectToolCall("monitor__monitor_investigate", nil), RawReceipt{
		Structured: json.RawMessage(`{"investigated":true,"verdict":"complete","steps":[],"artifact_ref":{
			"$schema":"urn:filecheap.dev:artifact-ref:v1","version":1,"provider":"fcheap-local",
			"uri":"fcheap://stash/incident_001","artifact_id":"incident_001","kind":"monitor.incident"}}`),
	})
	if complete.Domain != DomainSucceeded || !complete.DomainTyped {
		t.Fatalf("investigate projection = %+v", complete)
	}
	if complete.Artifact == nil || complete.Artifact.Kind != ArtifactDigestPortableRef ||
		complete.Artifact.RefKind != "monitor.incident" || complete.Artifact.URI != "fcheap://stash/incident_001" {
		t.Fatalf("investigate artifact digest = %+v", complete.Artifact)
	}

	// Monitor only emits local refs; a cloud ref inside an investigation is a
	// contract violation and invalidates the receipt entirely.
	foreign := ProjectReceipt(ProjectToolCall("monitor__monitor_investigate", nil), RawReceipt{
		Structured: json.RawMessage(`{"verdict":"complete","artifact_ref":{
			"$schema":"urn:filecheap.dev:artifact-ref:v1","version":1,"provider":"fcheap-cloud",
			"uri":"fcheap://cloud/vaults/v/artifacts/a","artifact_id":"a","kind":"monitor.incident"}}`),
	})
	if foreign.Domain != DomainUnknown || foreign.Artifact != nil {
		t.Fatalf("foreign investigate ref must fail closed, got %+v", foreign)
	}

	// The pre-existing no-artifact shape stays recognized.
	partial := ProjectReceipt(ProjectToolCall("monitor__monitor_investigate", nil), RawReceipt{
		Structured: json.RawMessage(`{"verdict":"partial"}`),
	})
	if partial.Domain != DomainAttention || partial.Artifact != nil {
		t.Fatalf("partial investigate projection = %+v", partial)
	}
}
