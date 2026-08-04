package ecosystem

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestHitspecSearchProjectsCandidateEvidenceAndTransientResults(t *testing.T) {
	projection := ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "hitspec", "tool": "hitspec_search_web",
	})
	document := json.RawMessage(`{
		"kind":"discovery","query":"private current-events query",
		"results":[
			{"title":"Primary docs","url":"https://docs.sonar.dev/guide","domain":"docs.sonar.dev","snippet":"Untrusted candidate summary","published_at":"2026-07-14T00:00:00Z","citation_id":"source-01"},
			{"title":"Release notes","url":"https://sonar.dev/releases","domain":"sonar.dev","snippet":"Second candidate","citation_id":"source-02"}
		],"truncated":true
	}`)

	got := ProjectReceipt(projection, RawReceipt{Text: string(document)})
	if got.Specialist != "hitspec" || got.Role != RoleDiscovery || got.Transport != TransportSucceeded ||
		got.Domain != DomainSucceeded || !got.DomainTyped || got.Evidence != EvidenceCandidate || !got.Successful() {
		t.Fatalf("projection = %#v", got)
	}
	if got.Digest == nil || got.Digest.Kind != DigestHitspecSearch || got.Digest.Count != 2 ||
		!got.Digest.Truncated || strings.Join(got.Digest.Items, ",") != "docs.sonar.dev,sonar.dev" {
		t.Fatalf("digest = %#v", got.Digest)
	}
	durable := SafeReceiptText(got)
	for _, forbidden := range []string{"private current-events query", "Primary docs", "/guide", "Untrusted candidate summary"} {
		if strings.Contains(durable, forbidden) {
			t.Fatalf("durable receipt retained %q: %s", forbidden, durable)
		}
	}
	for _, expected := range []string{"domain=succeeded", "evidence=candidate", "2 candidate sources", "docs.sonar.dev", "more results omitted"} {
		if !strings.Contains(durable, expected) {
			t.Fatalf("durable receipt missing %q: %s", expected, durable)
		}
	}
	transient, ok := TransientModelContent(got, RawReceipt{Text: string(document)})
	if !ok {
		t.Fatal("validated search result was not available transiently")
	}
	for _, expected := range []string{"transient", "not saved", "candidate sources", "https://docs.sonar.dev/guide", "Untrusted candidate summary"} {
		if !strings.Contains(transient, expected) {
			t.Fatalf("transient search result missing %q: %s", expected, transient)
		}
	}
	if strings.Contains(transient, "private current-events query") {
		t.Fatalf("transient search result unnecessarily repeated the private query: %s", transient)
	}
	wrongContext := got
	wrongContext.Operation = "hitspec_fetch"
	wrongContext = wrongContext.Normalize()
	if wrongContext.Digest != nil || wrongContext.SummaryText() != "" {
		t.Fatalf("Hitspec search digest survived a mismatched restored context: %#v", wrongContext)
	}
}

func TestHitspecSearchRejectsMalformedOrNonPublicDiscoveryContracts(t *testing.T) {
	tests := map[string]string{
		"provider metadata": `{"kind":"discovery","query":"q","results":[],"truncated":false,"provider":"tavily"}`,
		"null results":      `{"kind":"discovery","query":"q","results":null,"truncated":false}`,
		"null title":        `{"kind":"discovery","query":"q","results":[{"title":null,"url":"https://sonar.dev/","domain":"sonar.dev","snippet":"x","citation_id":"source-01"}],"truncated":false}`,
		"null snippet":      `{"kind":"discovery","query":"q","results":[{"title":"x","url":"https://sonar.dev/","domain":"sonar.dev","snippet":null,"citation_id":"source-01"}],"truncated":false}`,
		"null published at": `{"kind":"discovery","query":"q","results":[{"title":"x","url":"https://sonar.dev/","domain":"sonar.dev","snippet":"x","published_at":null,"citation_id":"source-01"}],"truncated":false}`,
		"wrong citation":    `{"kind":"discovery","query":"q","results":[{"title":"x","url":"https://sonar.dev/","domain":"sonar.dev","snippet":"x","citation_id":"source-02"}],"truncated":false}`,
		"forged domain":     `{"kind":"discovery","query":"q","results":[{"title":"x","url":"https://sonar.dev/","domain":"forged.invalid","snippet":"x","citation_id":"source-01"}],"truncated":false}`,
		"private candidate": `{"kind":"discovery","query":"q","results":[{"title":"x","url":"http://127.0.0.1/","domain":"127.0.0.1","snippet":"x","citation_id":"source-01"}],"truncated":false}`,
		"localhost":         `{"kind":"discovery","query":"q","results":[{"title":"x","url":"http://localhost/","domain":"localhost","snippet":"x","citation_id":"source-01"}],"truncated":false}`,
		"noncanonical URL":  `{"kind":"discovery","query":"q","results":[{"title":"x","url":"https://Example.com:443/path?utm_source=test&id=1#fragment","domain":"example.com","snippet":"x","citation_id":"source-01"}],"truncated":false}`,
		"duplicate URL":     `{"kind":"discovery","query":"q","results":[{"title":"x","url":"https://sonar.dev/","domain":"sonar.dev","snippet":"x","citation_id":"source-01"},{"title":"y","url":"https://sonar.dev/","domain":"sonar.dev","snippet":"y","citation_id":"source-02"}],"truncated":false}`,
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("hitspec__hitspec_search_web", nil), RawReceipt{Text: document})
			if got.Domain != DomainUnknown || got.DomainTyped || got.Evidence != EvidenceNone || got.Digest != nil || got.Successful() {
				t.Fatalf("malformed discovery projection = %#v", got)
			}
			if transient, ok := TransientModelContent(got, RawReceipt{Text: document}); ok || transient != "" {
				t.Fatalf("malformed discovery escaped transiently: %q, %v", transient, ok)
			}
		})
	}
}

func TestHitspecSearchAcceptsUpstreamCanonicalExampleAndPublicIP(t *testing.T) {
	document := `{"kind":"discovery","query":"q","results":[{"title":"Example","url":"https://example.com/path?id=1","domain":"example.com","snippet":"candidate","citation_id":"source-01"},{"title":"Public resolver","url":"https://8.8.8.8/","domain":"8.8.8.8","snippet":"candidate","citation_id":"source-02"}],"truncated":false}`
	got := ProjectReceipt(ProjectToolCall("hitspec__hitspec_search_web", nil), RawReceipt{Text: document})
	if got.Domain != DomainSucceeded || got.Evidence != EvidenceCandidate || got.Digest == nil || got.Digest.Count != 2 {
		t.Fatalf("canonical upstream discovery projection = %#v", got)
	}
}

func TestHitspecMCPHubCleanPublicAliasesRetainTypedContracts(t *testing.T) {
	search := ProjectReceipt(ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "hitspec", "tool": "search_web",
	}), RawReceipt{Text: `{"kind":"discovery","query":"q","results":[],"truncated":false}`})
	if search.Operation != "search_web" || search.Domain != DomainSucceeded || !search.DomainTyped ||
		search.Evidence != EvidenceCandidate || search.Digest == nil {
		t.Fatalf("clean search alias projection = %#v", search)
	}

	capture := ProjectReceipt(ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "hitspec", "tool": "capture_webpage",
	}), RawReceipt{Structured: json.RawMessage(`{
		"url":"https://example.com","final_url":"https://example.com","title":"Example",
		"http_status":200,"content_type":"text/html","markdown_bytes":12,
		"stash":{"id":"stash-clean","status":"saved","file_count":1,"total_size":12,
			"indexed":false,"index_requested":false}
	}`)})
	if capture.Operation != "capture_webpage" || capture.Role != RoleArtifact ||
		capture.Domain != DomainSucceeded || !capture.DomainTyped || capture.Artifact == nil ||
		capture.Artifact.ID != "stash-clean" {
		t.Fatalf("clean capture alias projection = %#v", capture)
	}
	if restored := capture.Normalize(); restored.Artifact == nil || restored.Artifact.ID != "stash-clean" {
		t.Fatalf("clean capture artifact did not survive normalization: %#v", restored)
	}
}

func TestHitspecOversizeCaptureIsTypedOnlyForExactPreEffectError(t *testing.T) {
	for _, operation := range []string{"capture_webpage", "hitspec_capture_webpage"} {
		got := ProjectReceipt(ProjectToolCall("hitspec__"+operation, nil), RawReceipt{
			Text: "response body exceeds 1048576-byte limit", ToolError: true,
		})
		if got.Domain != DomainFailed || !got.DomainTyped || got.Evidence != EvidenceNone || got.Artifact != nil {
			t.Fatalf("typed pre-effect failure for %q = %#v", operation, got)
		}
	}
	for name, receipt := range map[string]RawReceipt{
		"not a tool error": {Text: "response body exceeds 1048576-byte limit"},
		"transport error":  {Text: "response body exceeds 1048576-byte limit", ToolError: true, TransportError: true},
		"wording drift":    {Text: "response body exceeds one-megabyte limit", ToolError: true},
		"invalid limit":    {Text: "response body exceeds 0-byte limit", ToolError: true},
	} {
		t.Run(name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("hitspec__capture_webpage", nil), receipt)
			if got.DomainTyped || got.Artifact != nil {
				t.Fatalf("ambiguous failure became typed: %#v", got)
			}
		})
	}
}

func TestHitspecCaptureProjectsBoundedDurableArtifact(t *testing.T) {
	projection := ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "hitspec", "tool": "hitspec_capture_webpage",
	})
	structured := json.RawMessage(`{
		"url":"https://example.com/docs",
		"final_url":"https://docs.example.com/guide",
		"title":"private title","http_status":200,"content_type":"text/html","markdown_bytes":321,
		"stash":{"id":"stash-123","name":"private capture name","status":"saved","created_at":"2026-07-13T12:00:00Z",
			"expires_at":"2026-07-15T12:00:00Z","tags":["web","markdown"],
			"content_hash":"provider-specific-hash","file_count":1,"total_size":321,
			"indexed":true,"index_requested":true,"failed":null}
	}`)

	got := ProjectReceipt(projection, RawReceipt{Structured: structured})
	if got.Specialist != "hitspec" || got.Role != RoleArtifact || got.Domain != DomainSucceeded || got.Evidence != EvidenceSupported {
		t.Fatalf("projection = %#v", got)
	}
	if got.Artifact == nil || got.Artifact.Kind != ArtifactDigestHitspecCapture || got.Artifact.ID != "stash-123" ||
		got.Artifact.URI != "fcheap://stash/stash-123" || got.Artifact.SchemaVersion != hitspecCaptureSchema ||
		got.Artifact.FileCount != 1 || got.Artifact.TotalSize != 321 || got.Artifact.ContentSHA256 != "" {
		t.Fatalf("artifact = %#v", got.Artifact)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"example.com", "token", "private title", "private capture name", "provider-specific-hash"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("projection retained %q: %s", forbidden, encoded)
		}
	}
	if summary := SafeReceiptText(got); !strings.Contains(summary, "fcheap://stash/stash-123") || !strings.Contains(summary, "321 bytes") {
		t.Fatalf("safe receipt = %q", summary)
	}
}

func TestHitspecCaptureKeepsStorageAndIndexingOutcomesDistinct(t *testing.T) {
	projection := ProjectToolCall("hitspec__hitspec_capture_webpage", nil)
	structured := json.RawMessage(`{
		"url":"https://example.com","final_url":"https://example.com","title":"Example",
		"http_status":200,"content_type":"text/html","markdown_bytes":12,
		"stash":{"id":"stash-partial","status":"saved_with_failures","file_count":1,"total_size":12,
			"indexed":false,"index_requested":true,
			"failed":[{"id":"index","stage":"index","error":"SECRET_FAILURE_PROSE"}]}
	}`)

	got := ProjectReceipt(projection, RawReceipt{Structured: structured})
	if got.Domain != DomainAttention || got.Evidence != EvidenceSupported || got.Artifact == nil || !got.Artifact.IndexingFailed {
		t.Fatalf("projection = %#v", got)
	}
	if strings.Contains(SafeReceiptText(got), "SECRET_FAILURE_PROSE") || !strings.Contains(SafeReceiptText(got), "indexing incomplete") {
		t.Fatalf("safe receipt = %q", SafeReceiptText(got))
	}
}

func TestHitspecCapturePreservesExplicitFailedStatusWithoutArtifactID(t *testing.T) {
	got := ProjectReceipt(ProjectToolCall("hitspec__hitspec_capture_webpage", nil), RawReceipt{Structured: json.RawMessage(`{
		"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar",
		"http_status":200,"content_type":"text/html","markdown_bytes":12,
		"stash":{"id":"","status":"failed","file_count":0,"total_size":0,"indexed":false,"index_requested":true,
			"failed":[{"id":"save","stage":"storage","error":"sink rejected capture"}]}
	}`)})
	if got.Domain != DomainFailed || !got.DomainTyped || got.Evidence != EvidenceNone || got.Artifact != nil || got.Successful() {
		t.Fatalf("explicit failed capture projection = %#v", got)
	}
}

func TestHitspecProjectionRejectsWrongOperationOrMalformedCapture(t *testing.T) {
	document := json.RawMessage(`{
		"http_status":200,"markdown_bytes":12,
		"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":true,"index_requested":true}
	}`)

	fetch := ProjectReceipt(ProjectToolCall("hitspec__hitspec_fetch", nil), RawReceipt{Structured: document})
	if fetch.Domain != DomainUnknown || fetch.Artifact != nil || fetch.Role != RoleDiscovery {
		t.Fatalf("fetch projection = %#v", fetch)
	}
	directCaptureSpoof := ProjectReceipt(ProjectToolCall("evil__hitspec_capture_webpage", nil), RawReceipt{Structured: document})
	if directCaptureSpoof.Specialist != "" || directCaptureSpoof.Domain != DomainUnknown ||
		directCaptureSpoof.DomainTyped || directCaptureSpoof.Artifact != nil {
		t.Fatalf("direct capture suffix spoof gained Hitspec attribution: %#v", directCaptureSpoof)
	}
	directSearchSpoof := ProjectReceipt(ProjectToolCall("evil__hitspec_search_web", nil), RawReceipt{Text: `{
		"kind":"discovery","query":"q","results":[],"truncated":false
	}`})
	if directSearchSpoof.Specialist != "" || directSearchSpoof.Domain != DomainUnknown ||
		directSearchSpoof.DomainTyped || directSearchSpoof.Digest != nil {
		t.Fatalf("direct search suffix spoof gained Hitspec attribution: %#v", directSearchSpoof)
	}
	validCapture := json.RawMessage(`{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`)
	for name, projection := range map[string]ToolProjection{
		"direct prefixed server": ProjectToolCall("hitspec_evil__hitspec_capture_webpage", nil),
		"direct trimmed server":  ProjectToolCall("hitspec-__hitspec_capture_webpage", nil),
		"direct filtered server": ProjectToolCall("hitspec!__hitspec_capture_webpage", nil),
		"direct filtered tool":   ProjectToolCall("hitspec__hitspec_capture_webpage!", nil),
		"embedded gateway route": ProjectToolCall("evil__mcphub__hitspec__hitspec_capture_webpage", nil),
		"gateway prefixed server": ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
			"server": "hitspec_evil", "tool": "hitspec_capture_webpage",
		}),
		"gateway trimmed server": ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
			"server": "hitspec-", "tool": "hitspec_capture_webpage",
		}),
		"gateway filtered server": ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
			"server": "hitspec!", "tool": "hitspec_capture_webpage",
		}),
		"gateway filtered tool": ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
			"server": "hitspec", "tool": "hitspec_capture_webpage!",
		}),
		"lookalike gateway call": ProjectToolCall("evil__mcphub_call_tool", map[string]any{
			"server": "hitspec", "tool": "hitspec_capture_webpage",
		}),
	} {
		t.Run(name, func(t *testing.T) {
			got := ProjectReceipt(projection, RawReceipt{Structured: validCapture})
			if got.Specialist != "" || got.Domain != DomainUnknown || got.DomainTyped || got.Evidence != EvidenceNone || got.Artifact != nil || got.Successful() {
				t.Fatalf("prefixed server gained Hitspec attribution: %#v", got)
			}
		})
	}

	validSearch := `{"kind":"discovery","query":"q","results":[{"title":"Docs","url":"https://sonar.dev/","domain":"sonar.dev","snippet":"candidate","citation_id":"source-01"}],"truncated":false}`
	for name, projection := range map[string]ToolProjection{
		"direct trimmed server":  ProjectToolCall("hitspec-__hitspec_search_web", nil),
		"direct filtered server": ProjectToolCall("hitspec!__hitspec_search_web", nil),
		"embedded gateway route": ProjectToolCall("evil__mcphub__hitspec__hitspec_search_web", nil),
		"gateway trimmed server": ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
			"server": "hitspec-", "tool": "hitspec_search_web",
		}),
		"lookalike gateway call": ProjectToolCall("evil__mcphub_call_tool", map[string]any{
			"server": "hitspec", "tool": "hitspec_search_web",
		}),
	} {
		t.Run("search "+name, func(t *testing.T) {
			got := ProjectReceipt(projection, RawReceipt{Text: validSearch})
			if got.Specialist != "" || got.Domain != DomainUnknown || got.DomainTyped || got.Evidence != EvidenceNone || got.Digest != nil || got.Successful() {
				t.Fatalf("spoofed route gained Hitspec search attribution: %#v", got)
			}
		})
	}

	malformed := ProjectReceipt(ProjectToolCall("hitspec__hitspec_capture_webpage", nil), RawReceipt{
		Structured: json.RawMessage(`{"http_status":200,"markdown_bytes":12,"stash":{"id":"../escape","status":"saved","file_count":1,"total_size":12,"indexed":true,"index_requested":true}}`),
	})
	if malformed.Domain != DomainUnknown || malformed.Artifact != nil {
		t.Fatalf("malformed projection = %#v", malformed)
	}

	contradictions := map[string]string{
		"multiple files":    `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":2,"total_size":12,"indexed":true,"index_requested":true}}`,
		"size mismatch":     `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":13,"indexed":true,"index_requested":true}}`,
		"unrequested index": `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":true,"index_requested":false}}`,
	}
	for name, raw := range contradictions {
		t.Run(name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("hitspec__hitspec_capture_webpage", nil), RawReceipt{Structured: json.RawMessage(raw)})
			if got.Domain != DomainUnknown || got.DomainTyped || got.Artifact != nil || got.Successful() {
				t.Fatalf("contradictory capture projection = %#v", got)
			}
		})
	}
}

func TestHitspecCaptureRequiresExactTypedV218Contract(t *testing.T) {
	tests := map[string]string{
		"missing page URL":      `{"final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"missing final URL":     `{"url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"missing title":         `{"url":"https://sonar.dev","final_url":"https://sonar.dev","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"missing content type":  `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"null URL":              `{"url":null,"final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"relative URL":          `{"url":"not-a-url","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"file URL":              `{"url":"file:///etc/passwd","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"private URL":           `{"url":"http://127.0.0.1/","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"unsanitized query":     `{"url":"https://sonar.dev/?token=private","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"null stash ID":         `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":null,"status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"invalid tag item":      `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false,"tags":[12]}}`,
		"invalid failure item":  `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false,"failed":[123]}}`,
		"failure field missing": `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false,"failed":[{"id":"index","stage":"index"}]}}`,
		"failure field unknown": `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false,"failed":[{"id":"index","stage":"index","error":"failed","provider":"private"}]}}`,
		"unknown top field":     `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"provider":"private","stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`,
		"unknown stash field":   `{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false,"provider":"private"}}`,
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("hitspec__hitspec_capture_webpage", nil), RawReceipt{Structured: json.RawMessage(document)})
			if got.Domain != DomainUnknown || got.DomainTyped || got.Evidence != EvidenceNone || got.Artifact != nil || got.Successful() {
				t.Fatalf("invalid capture projection = %#v", got)
			}
		})
	}
}

func TestHitspecCaptureAcceptsProducerBoundedUnicodeAndEmptyContentType(t *testing.T) {
	document := fmt.Sprintf(`{"url":"https://sonar.dev","final_url":"https://sonar.dev/docs","title":%q,"http_status":200,"content_type":"","markdown_bytes":12,"stash":{"id":"stash-unicode","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false,"failed":[{"id":"","stage":"","error":""}]}}`, strings.Repeat("界", 300))
	got := ProjectReceipt(ProjectToolCall("hitspec__hitspec_capture_webpage", nil), RawReceipt{Structured: json.RawMessage(document)})
	if got.Domain != DomainAttention || got.Evidence != EvidenceSupported || got.Artifact == nil {
		t.Fatalf("valid producer-bounded capture projection = %#v", got)
	}
}

func TestHitspecCaptureDiscardsOpaqueOptionalCreatedAt(t *testing.T) {
	document := `{"url":"https://sonar.dev","final_url":"https://sonar.dev/docs","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-custom-time","status":"saved","created_at":"custom-sink-clock","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`
	got := ProjectReceipt(ProjectToolCall("hitspec__hitspec_capture_webpage", nil), RawReceipt{Structured: json.RawMessage(document)})
	if got.Domain != DomainSucceeded || got.Evidence != EvidenceSupported || got.Artifact == nil {
		t.Fatalf("capture with opaque optional timestamp = %#v", got)
	}
	if got.Artifact.CreatedAt != "" {
		t.Fatalf("opaque optional timestamp persisted: %#v", got.Artifact)
	}
}

func TestHitspecToolErrorCannotRetainOptimisticEvidence(t *testing.T) {
	search := ProjectReceipt(ProjectToolCall("hitspec__hitspec_search_web", nil), RawReceipt{
		Text:      `{"kind":"discovery","query":"q","results":[{"title":"Docs","url":"https://sonar.dev/","domain":"sonar.dev","snippet":"candidate","citation_id":"source-01"}],"truncated":false}`,
		ToolError: true,
	})
	if search.Domain != DomainFailed || search.Evidence != EvidenceNone || search.Digest != nil || search.Successful() {
		t.Fatalf("tool-error search retained optimistic evidence: %#v", search)
	}

	capture := ProjectReceipt(ProjectToolCall("hitspec__hitspec_capture_webpage", nil), RawReceipt{
		Structured: json.RawMessage(`{"url":"https://sonar.dev","final_url":"https://sonar.dev","title":"sonar","http_status":200,"content_type":"text/html","markdown_bytes":12,"stash":{"id":"stash-1","status":"saved","file_count":1,"total_size":12,"indexed":false,"index_requested":false}}`),
		ToolError:  true,
	})
	if capture.Domain != DomainFailed || capture.Evidence != EvidenceNone || capture.Artifact != nil || capture.Successful() {
		t.Fatalf("tool-error capture retained optimistic evidence: %#v", capture)
	}
}
