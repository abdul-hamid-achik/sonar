package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func snapshotOf(t *testing.T, providers []Provider) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.json")
	encoded, err := json.MarshalIndent(providers, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func syntheticProviders(n int) []Provider {
	out := make([]Provider, 0, n)
	for i := 0; i < n; i++ {
		id := ProviderID(string(rune('a'+i%26)) + string(rune('a'+i/26)))
		out = append(out, Provider{
			ID:     id,
			Name:   string(id),
			Models: []Model{{ID: string(id) + "-model", ContextWindow: 1000}},
		})
	}
	return out
}

// A truncated or empty fetch parses fine as JSON. Writing it would replace a
// working catalog with a broken one, so the floor exists to make a bad fetch
// cost nothing.
func TestRefuseToWriteAnImplausiblySmallCatalog(t *testing.T) {
	path := snapshotOf(t, syntheticProviders(30))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := writeSnapshot(syntheticProviders(3), path); err == nil {
		t.Fatal("a 3-provider catalog was accepted")
	} else if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error does not explain the refusal: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the existing snapshot was modified by a refused write")
	}
}

func TestWriteSnapshotReportsAddedAndRemovedProviders(t *testing.T) {
	old := syntheticProviders(25)
	path := snapshotOf(t, old)

	updated := append([]Provider(nil), old[:24]...) // drop one
	updated = append(updated, Provider{ID: "brand-new", Name: "Brand New",
		Models: []Model{{ID: "brand-new-1"}}})

	summary, err := writeSnapshot(updated, path)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !summary.Changed {
		t.Error("a differing catalog reported no change")
	}
	if len(summary.AddedProviders) != 1 || summary.AddedProviders[0] != "brand-new" {
		t.Errorf("added = %v, want [brand-new]", summary.AddedProviders)
	}
	if len(summary.RemovedProviders) != 1 || summary.RemovedProviders[0] != string(old[24].ID) {
		t.Errorf("removed = %v, want [%s]", summary.RemovedProviders, old[24].ID)
	}
	if summary.Providers != 25 {
		t.Errorf("providers = %d, want 25", summary.Providers)
	}

	// The written bytes must be loadable, or the next build embeds a broken file.
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip []Provider
	if err := json.Unmarshal(written, &roundTrip); err != nil {
		t.Fatalf("written snapshot does not parse: %v", err)
	}
	if len(roundTrip) != 25 {
		t.Errorf("round-tripped %d providers, want 25", len(roundTrip))
	}
}

// An unchanged catalog must not rewrite the file — a no-op refresh should not
// show up as a diff in version control.
func TestUnchangedCatalogLeavesTheFileAlone(t *testing.T) {
	providers := syntheticProviders(25)
	path := snapshotOf(t, providers)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	summary, err := writeSnapshot(providers, path)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if summary.Changed {
		t.Error("an identical catalog reported a change")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Error("an unchanged catalog rewrote the file")
	}
	if !strings.Contains(summary.String(), "unchanged") {
		t.Errorf("summary = %q, want it to say unchanged", summary.String())
	}
}

func TestWriteSnapshotCreatesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	summary, err := writeSnapshot(syntheticProviders(25), path)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !summary.Changed {
		t.Error("writing a new file reported no change")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file was not created: %v", err)
	}
}

// catwalk.New() falls back to localhost:8080, so a refresh that forgot to pass
// a URL would silently talk to nothing. The default must be the real service.
func TestDefaultCatwalkURLIsThePublicService(t *testing.T) {
	if !strings.HasPrefix(DefaultCatwalkURL, "https://") {
		t.Errorf("default URL %q is not https", DefaultCatwalkURL)
	}
	if strings.Contains(DefaultCatwalkURL, "localhost") {
		t.Errorf("default URL %q points at localhost", DefaultCatwalkURL)
	}
}
