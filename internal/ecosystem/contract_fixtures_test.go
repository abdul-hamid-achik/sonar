package ecosystem

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	contractFixtureManifestVersion  = 1
	maxContractFixtureDocumentBytes = 64 * 1024
	maxPersistedFixtureProjection   = 4 * 1024
)

type contractFixtureManifest struct {
	SchemaVersion    int                   `json:"schema_version"`
	ReviewedRevision string                `json:"reviewed_revision"`
	Cases            []contractFixtureCase `json:"cases"`
}

type contractFixtureSource struct {
	Product   string `json:"product"`
	Version   string `json:"version"`
	Reference string `json:"reference"`
}

type contractFixtureCase struct {
	ID             string                     `json:"id"`
	Source         contractFixtureSource      `json:"source"`
	Corpus         string                     `json:"corpus,omitempty"`
	Document       string                     `json:"document"`
	Tool           string                     `json:"tool"`
	Arguments      map[string]any             `json:"arguments,omitempty"`
	Delivery       string                     `json:"delivery"`
	ToolError      bool                       `json:"tool_error,omitempty"`
	MustFailClosed bool                       `json:"must_fail_closed,omitempty"`
	Forbidden      []string                   `json:"forbidden_persistence_markers,omitempty"`
	Expected       contractFixtureExpectation `json:"expected"`
}

type contractFixtureExpectation struct {
	Projection       ToolProjection `json:"projection"`
	SafeReceiptText  string         `json:"safe_receipt_text"`
	TransientContent bool           `json:"transient_content"`
}

func TestExactContractFixtures(t *testing.T) {
	root := filepath.Join("testdata", "contracts")
	manifest := loadContractFixtureManifest(t, filepath.Join(root, "index.json"))
	if manifest.SchemaVersion != contractFixtureManifestVersion {
		t.Fatalf("fixture manifest schema_version = %d, want %d", manifest.SchemaVersion, contractFixtureManifestVersion)
	}
	if len(manifest.ReviewedRevision) != 40 || canonicalIdentifier(manifest.ReviewedRevision) != manifest.ReviewedRevision {
		t.Fatalf("fixture manifest reviewed_revision is not an exact commit identity: %q", manifest.ReviewedRevision)
	}
	if len(manifest.Cases) == 0 {
		t.Fatal("fixture manifest has no contract cases")
	}

	seen := make(map[string]struct{}, len(manifest.Cases))
	for _, fixture := range manifest.Cases {
		if _, duplicate := seen[fixture.ID]; duplicate {
			t.Fatalf("duplicate fixture id %q", fixture.ID)
		}
		seen[fixture.ID] = struct{}{}
		fixture := fixture
		t.Run(fixture.ID, func(t *testing.T) {
			t.Parallel()
			runExactContractFixture(t, root, fixture)
		})
	}
}

func loadContractFixtureManifest(t *testing.T, path string) contractFixtureManifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read contract fixture manifest: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest contractFixtureManifest
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode contract fixture manifest: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("contract fixture manifest has trailing JSON values: %v", err)
	}
	return manifest
}

func runExactContractFixture(t *testing.T, root string, fixture contractFixtureCase) {
	t.Helper()
	validateContractFixtureMetadata(t, fixture)
	document := loadContractFixtureDocument(t, contractFixtureCorpusRoot(t, root, fixture.Corpus), fixture.Document)
	receipt := contractFixtureReceipt(t, fixture, document)

	projection := ProjectReceipt(ProjectToolCall(fixture.Tool, fixture.Arguments), receipt)
	want := fixture.Expected.Projection.Normalize()
	if !reflect.DeepEqual(projection, want) {
		gotJSON, _ := json.MarshalIndent(projection, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("projection mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}

	safeText := SafeReceiptText(projection)
	if safeText != fixture.Expected.SafeReceiptText {
		t.Fatalf("safe receipt text = %q, want %q", safeText, fixture.Expected.SafeReceiptText)
	}
	transient, available := TransientModelContent(projection, receipt)
	if available != fixture.Expected.TransientContent {
		t.Fatalf("transient content availability = %t, want %t (content %q)", available, fixture.Expected.TransientContent, transient)
	}
	if !available && transient != "" {
		t.Fatalf("unavailable transient content was non-empty: %q", transient)
	}
	if available && !strings.HasPrefix(transient, "MCPHub result page (transient; not saved)\n") &&
		!strings.HasPrefix(transient, "Bob guidance (validated transient content; not saved)\n") {
		t.Fatalf("transient content omitted boundary notice: %q", transient)
	}

	assertContractFixturePersistenceBoundary(t, projection, safeText, document, fixture.Forbidden)
	if fixture.MustFailClosed {
		if projection.Domain == DomainSucceeded || projection.Evidence != EvidenceNone {
			t.Fatalf("malformed/future contract did not fail closed: %#v", projection)
		}
	}
}

func validateContractFixtureMetadata(t *testing.T, fixture contractFixtureCase) {
	t.Helper()
	if canonicalIdentifier(fixture.ID) != fixture.ID || fixture.ID == "" {
		t.Fatalf("fixture id is not canonical: %q", fixture.ID)
	}
	if fixture.Source.Product == "" || fixture.Source.Version == "" || fixture.Source.Reference == "" {
		t.Fatalf("fixture %q is missing source/version provenance: %#v", fixture.ID, fixture.Source)
	}
	if fixture.Tool == "" || fixture.Document == "" {
		t.Fatalf("fixture %q is missing tool or document", fixture.ID)
	}
	switch fixture.Corpus {
	case "", "bob_v040":
	default:
		t.Fatalf("fixture %q selects unsupported corpus %q", fixture.ID, fixture.Corpus)
	}
	want := fixture.Expected.Projection
	if want.Specialist == "" || want.Operation == "" || want.Role == "" ||
		want.Transport == "" || want.Domain == "" || want.Route.Tool == "" {
		t.Fatalf("fixture %q does not assert the complete routing/outcome projection: %#v", fixture.ID, want)
	}
	if fixture.Expected.SafeReceiptText == "" {
		t.Fatalf("fixture %q does not assert persistence-safe receipt text", fixture.ID)
	}
}

func contractFixtureCorpusRoot(t *testing.T, defaultRoot, corpus string) string {
	t.Helper()
	root, ok := resolveContractFixtureCorpusRoot(defaultRoot, corpus)
	if !ok {
		t.Fatalf("unsupported contract fixture corpus %q", corpus)
	}
	return root
}

func resolveContractFixtureCorpusRoot(defaultRoot, corpus string) (string, bool) {
	switch corpus {
	case "":
		return defaultRoot, true
	case "bob_v040":
		return filepath.Join(filepath.Dir(defaultRoot), "bob_v040"), true
	default:
		return "", false
	}
}

func TestContractFixtureCorpusRootAllowlist(t *testing.T) {
	const root = "testdata/contracts"
	for corpus, want := range map[string]string{
		"":         root,
		"bob_v040": "testdata/bob_v040",
	} {
		got, ok := resolveContractFixtureCorpusRoot(root, corpus)
		if !ok || got != want {
			t.Fatalf("resolveContractFixtureCorpusRoot(%q) = %q, %t; want %q, true", corpus, got, ok, want)
		}
	}
	for _, corpus := range []string{"../bob_v040", "bob_v040/..", "/tmp/bob_v040", "contracts"} {
		if got, ok := resolveContractFixtureCorpusRoot(root, corpus); ok || got != "" {
			t.Fatalf("unsafe corpus %q resolved to %q", corpus, got)
		}
	}
}

func loadContractFixtureDocument(t *testing.T, root, relative string) json.RawMessage {
	t.Helper()
	clean := filepath.Clean(relative)
	if filepath.IsAbs(relative) || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.Ext(clean) != ".json" {
		t.Fatalf("unsafe fixture document path %q", relative)
	}
	path := filepath.Join(root, clean)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture document %q: %v", relative, err)
	}
	if len(raw) == 0 || len(raw) > maxContractFixtureDocumentBytes {
		t.Fatalf("fixture document %q has invalid size %d", relative, len(raw))
	}
	if !json.Valid(raw) {
		t.Fatalf("fixture document %q is not exact valid JSON", relative)
	}
	return append(json.RawMessage(nil), raw...)
}

func contractFixtureReceipt(t *testing.T, fixture contractFixtureCase, document json.RawMessage) RawReceipt {
	t.Helper()
	receipt := RawReceipt{ToolError: fixture.ToolError}
	switch fixture.Delivery {
	case "structured":
		receipt.Structured = append(json.RawMessage(nil), document...)
	case "text":
		receipt.Text = string(document)
	case "transport_failure":
		receipt.Text = string(document)
		receipt.TransportError = true
	default:
		t.Fatalf("fixture %q has unsupported delivery %q", fixture.ID, fixture.Delivery)
	}
	return receipt
}

func assertContractFixturePersistenceBoundary(t *testing.T, projection ToolProjection, safeText string, document json.RawMessage, forbidden []string) {
	t.Helper()
	persisted, err := json.Marshal(projection)
	if err != nil {
		t.Fatalf("marshal persisted projection: %v", err)
	}
	if len(persisted) > maxPersistedFixtureProjection {
		t.Fatalf("persisted projection is unbounded: %d bytes", len(persisted))
	}

	var restored ToolProjection
	if err := json.Unmarshal(persisted, &restored); err != nil {
		t.Fatalf("restore persisted projection: %v", err)
	}
	if restored = restored.Normalize(); !reflect.DeepEqual(projection, restored) {
		t.Fatalf("persisted projection changed after normalized round trip: %#v", restored)
	}

	compactDocument := bytes.NewBuffer(nil)
	if err := json.Compact(compactDocument, document); err != nil {
		t.Fatalf("compact fixture document: %v", err)
	}
	combined := string(persisted) + "\n" + safeText
	if strings.Contains(combined, compactDocument.String()) {
		t.Fatal("raw fixture body escaped into the persistent projection")
	}
	for _, marker := range forbidden {
		if marker == "" {
			t.Fatal("empty forbidden persistence marker")
		}
		if !bytes.Contains(document, []byte(marker)) {
			t.Fatalf("forbidden marker %q is absent from its fixture document", marker)
		}
		if strings.Contains(combined, marker) {
			t.Fatalf("raw fixture marker %q escaped into persistent state: %s", marker, combined)
		}
	}

	// This catches accidental raw fields even when a fixture author forgot to
	// add a dedicated secret marker: durable JSON is a bounded semantic object,
	// never a StructuredContent-shaped document.
	if bytes.Contains(persisted, []byte(`"structured"`)) || bytes.Contains(persisted, []byte(`"data"`)) ||
		bytes.Contains(persisted, []byte(`"warnings"`)) || bytes.Contains(persisted, []byte(`"next_actions"`)) {
		t.Fatalf("raw contract field escaped into persisted projection: %s", persisted)
	}
	if projection.Transport == TransportSucceeded && projection.Domain == DomainSucceeded && !projection.DomainTyped {
		t.Fatalf("successful domain outcome lacks exact parser authority: %s", fmt.Sprint(projection))
	}
}
