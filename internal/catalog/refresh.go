package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"charm.land/catwalk/pkg/catwalk"
)

// DefaultCatwalkURL is the public catalog service.
//
// It must be passed explicitly. catwalk.New() reads CATWALK_URL and otherwise
// falls back to localhost:8080, so a refresh built on the zero-config client
// silently talks to nothing on a machine without that variable set.
const DefaultCatwalkURL = "https://catwalk.charm.land/v2"

// minRefreshProviders guards against replacing a good snapshot with a
// truncated or empty response. The catalog carried 40 providers when this was
// written; a fetch returning a small fraction of that is a failed fetch that
// happened to parse, not a catalog that shrank.
const minRefreshProviders = 20

// RefreshSummary reports what a refresh would change. Counts come from
// comparing the fetched catalog against the one currently embedded.
type RefreshSummary struct {
	Providers        int
	Models           int
	AddedProviders   []string
	RemovedProviders []string
	// Changed is true when the fetched bytes differ from what is on disk.
	Changed bool
}

// String renders a one-line summary for a CLI.
func (s RefreshSummary) String() string {
	if !s.Changed {
		return fmt.Sprintf("catalog unchanged (%d providers, %d models)", s.Providers, s.Models)
	}
	out := fmt.Sprintf("catalog updated: %d providers, %d models", s.Providers, s.Models)
	if len(s.AddedProviders) > 0 {
		out += fmt.Sprintf(" · added %v", s.AddedProviders)
	}
	if len(s.RemovedProviders) > 0 {
		out += fmt.Sprintf(" · removed %v", s.RemovedProviders)
	}
	return out
}

// FetchProviders retrieves the live catalog. It never touches disk.
func FetchProviders(ctx context.Context, url string) ([]Provider, error) {
	if url == "" {
		url = DefaultCatwalkURL
	}
	// NewWithURL, never New: see DefaultCatwalkURL.
	providers, err := catwalk.NewWithURL(url).GetProviders(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("catalog: fetch %s: %w", url, err)
	}
	return providers, nil
}

// Refresh fetches the live catalog and rewrites the snapshot at path, which is
// normally internal/catalog/providers.json in a source checkout.
//
// The snapshot is embedded at build time, so a refresh only takes effect after
// rebuilding. That is deliberate: it keeps the running binary's catalog
// immutable and reviewable in version control rather than mutable at runtime.
//
// The write is validated and atomic. A response that fails to re-parse, or that
// carries implausibly few providers, leaves the existing snapshot untouched —
// a bad fetch must not cost the user a working catalog.
func Refresh(ctx context.Context, url, path string) (RefreshSummary, error) {
	providers, err := FetchProviders(ctx, url)
	if err != nil {
		return RefreshSummary{}, err
	}
	return writeSnapshot(providers, path)
}

func writeSnapshot(providers []Provider, path string) (RefreshSummary, error) {
	if len(providers) < minRefreshProviders {
		return RefreshSummary{}, fmt.Errorf(
			"catalog: refusing to write %d providers; fewer than the %d-provider sanity floor suggests a truncated fetch",
			len(providers), minRefreshProviders,
		)
	}
	models := 0
	fetched := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		models += len(p.Models)
		fetched[string(p.ID)] = struct{}{}
	}

	encoded, err := json.MarshalIndent(providers, "", "  ")
	if err != nil {
		return RefreshSummary{}, fmt.Errorf("catalog: encode snapshot: %w", err)
	}
	// Re-decode what we are about to persist. Marshalling then embedding a
	// document the loader cannot read would ship a broken binary.
	var roundTrip []Provider
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		return RefreshSummary{}, fmt.Errorf("catalog: snapshot does not round-trip: %w", err)
	}

	summary := RefreshSummary{Providers: len(providers), Models: models}
	previous, readErr := os.ReadFile(path) // #nosec G304 -- operator-selected snapshot path
	if readErr == nil {
		summary.Changed = string(previous) != string(encoded)
		var old []Provider
		if json.Unmarshal(previous, &old) == nil {
			had := make(map[string]struct{}, len(old))
			for _, p := range old {
				had[string(p.ID)] = struct{}{}
			}
			for id := range fetched {
				if _, ok := had[id]; !ok {
					summary.AddedProviders = append(summary.AddedProviders, id)
				}
			}
			for id := range had {
				if _, ok := fetched[id]; !ok {
					summary.RemovedProviders = append(summary.RemovedProviders, id)
				}
			}
			sort.Strings(summary.AddedProviders)
			sort.Strings(summary.RemovedProviders)
		}
	} else {
		summary.Changed = true
	}

	if !summary.Changed {
		return summary, nil
	}

	// Write through a sibling temp file so an interrupted write cannot leave a
	// half-written snapshot in place of a working one.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".providers-*.json")
	if err != nil {
		return summary, fmt.Errorf("catalog: create temp snapshot: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return summary, fmt.Errorf("catalog: write temp snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return summary, fmt.Errorf("catalog: close temp snapshot: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return summary, fmt.Errorf("catalog: chmod temp snapshot: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return summary, fmt.Errorf("catalog: replace snapshot: %w", err)
	}
	return summary, nil
}
