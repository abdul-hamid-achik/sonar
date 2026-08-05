package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/catalog"
)

// fakeCatalogProviders builds n distinct providers that satisfy the catalog's
// sanity floor when served as a Catwalk response.
func fakeCatalogProviders(n int) []catalog.Provider {
	out := make([]catalog.Provider, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("fake-%d", i)
		out = append(out, catalog.Provider{
			ID:     catalog.ProviderID(id),
			Name:   id,
			Models: []catalog.Model{{ID: id + "-model"}},
		})
	}
	return out
}

// fakeCatwalkServer serves a synthetic catalog on the exact URL shape the
// catalog client requests: base + "/v2/providers", where the base is the
// endpoint passed via --url.
func fakeCatwalkServer(t *testing.T, providers []catalog.Provider) *httptest.Server {
	t.Helper()
	payload, err := json.Marshal(providers)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/v2/providers" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestProvidersHelpAndInvalidArgumentsHaveNoSideEffects(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	var stdout, stderr bytes.Buffer
	writeRootUsage(&stdout, "sonar")
	if !strings.Contains(stdout.String(), "providers  Refresh the embedded provider catalog snapshot") {
		t.Fatalf("root help omitted providers:\n%s", stdout.String())
	}

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "bare", args: nil},
		{name: "help", args: []string{"help"}},
		{name: "short help", args: []string{"-h"}},
		{name: "long help", args: []string{"--help"}},
		{name: "refresh help", args: []string{"refresh", "--help"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			if code := handleProvidersCommandIO(test.args, &stdout, &stderr); code != 0 {
				t.Fatalf("providers %q exit = %d, want 0; stderr=%s", test.args, code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("providers %q wrote to stderr: %q", test.args, stderr.String())
			}
			for _, expected := range []string{
				"sonar providers refresh [--url URL] [--dry-run]",
				"--url URL",
				"--dry-run",
				"embedded at build time",
				"atomic",
			} {
				if !strings.Contains(stdout.String(), expected) {
					t.Fatalf("providers help omitted %q:\n%s", expected, stdout.String())
				}
			}
			assertHelpLinesAtMost(t, stdout.String(), 100)
		})
	}

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "unknown command", args: []string{"frobnicate"}},
		{name: "help with extra argument", args: []string{"help", "extra"}},
		{name: "flag help with extra argument", args: []string{"--help", "extra"}},
		{name: "refresh unexpected positional", args: []string{"refresh", "extra"}},
		{name: "refresh unknown flag", args: []string{"refresh", "--force"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			if code := handleProvidersCommandIO(test.args, &stdout, &stderr); code != 2 {
				t.Fatalf("providers %q exit = %d, want 2", test.args, code)
			}
			if stderr.Len() == 0 {
				t.Fatalf("providers %q wrote nothing to stderr", test.args)
			}
		})
	}

	assertPathDoesNotExist(t, filepath.Join(workDir, "internal", "catalog", "providers.json"))
}

func TestProvidersRefreshFetchesAndWritesSnapshot(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	server := fakeCatwalkServer(t, fakeCatalogProviders(25))

	var stdout, stderr bytes.Buffer
	if code := handleProvidersRefresh([]string{"--url", server.URL + "/v2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("refresh exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	for _, expected := range []string{
		"catalog updated: 25 providers, 25 models",
		"wrote internal/catalog/providers.json",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("refresh output omitted %q:\n%s", expected, stdout.String())
		}
	}

	snapshot, err := os.ReadFile(filepath.Join(workDir, "internal", "catalog", "providers.json"))
	if err != nil {
		t.Fatalf("snapshot was not written: %v", err)
	}
	var parsed []catalog.Provider
	if err := json.Unmarshal(snapshot, &parsed); err != nil {
		t.Fatalf("written snapshot does not parse: %v", err)
	}
	if len(parsed) != 25 {
		t.Errorf("snapshot has %d providers, want 25", len(parsed))
	}
}

func TestProvidersRefreshDryRunWritesNothing(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	server := fakeCatwalkServer(t, fakeCatalogProviders(25))

	var stdout, stderr bytes.Buffer
	if code := handleProvidersRefresh([]string{"--dry-run", "--url", server.URL + "/v2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry-run exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	for _, expected := range []string{
		"dry run: catalog updated: 25 providers, 25 models",
		"would rewrite internal/catalog/providers.json",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("dry-run output omitted %q:\n%s", expected, stdout.String())
		}
	}
	assertPathDoesNotExist(t, filepath.Join(workDir, "internal", "catalog", "providers.json"))
}

func TestProvidersRefreshRefusesTruncatedFetch(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	server := fakeCatwalkServer(t, fakeCatalogProviders(3))

	var stdout, stderr bytes.Buffer
	if code := handleProvidersRefresh([]string{"--url", server.URL + "/v2"}, &stdout, &stderr); code != 1 {
		t.Fatalf("refresh exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "truncated") {
		t.Fatalf("stderr did not explain the truncated fetch: %s", stderr.String())
	}
	assertPathDoesNotExist(t, filepath.Join(workDir, "internal", "catalog", "providers.json"))
}
