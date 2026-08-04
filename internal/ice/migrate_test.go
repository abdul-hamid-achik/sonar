package ice

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func TestClaimLegacyEntriesIsScopedPersistentAndOneTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	store := NewStore(path)
	if _, err := store.Add("legacy-one", "user", "one", []float32{1, 0}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("legacy-two", "assistant", "two", []float32{1, 0}, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddScoped("workspace:other", "other", "user", "other", []float32{1, 0}, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	result, err := store.ClaimLegacyEntries("workspace:current")
	if err != nil {
		t.Fatal(err)
	}
	if result.Claimed != 2 || result.AlreadyClaimed {
		t.Fatalf("first claim = %#v", result)
	}
	info, err := os.Stat(result.MarkerPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("marker mode = %04o, want 0600", got)
	}

	reloaded := NewStore(path)
	if err := reloaded.Err(); err != nil {
		t.Fatal(err)
	}
	claimed := 0
	other := 0
	for _, entry := range reloaded.entries {
		switch entry.ProjectID {
		case "workspace:current":
			claimed++
		case "workspace:other":
			other++
		case "":
			t.Fatal("legacy entry remained unscoped")
		}
	}
	if claimed != 2 || other != 1 {
		t.Fatalf("claimed=%d other=%d entries=%#v", claimed, other, reloaded.entries)
	}

	result, err = reloaded.ClaimLegacyEntries("workspace:current")
	if err != nil {
		t.Fatal(err)
	}
	if result.Claimed != 0 || !result.AlreadyClaimed {
		t.Fatalf("repeat claim = %#v", result)
	}
	if _, err := reloaded.ClaimLegacyEntries("workspace:attacker"); !errors.Is(err, ErrLegacyICEClaimedByAnotherProject) {
		t.Fatalf("second project claim error = %v", err)
	}
}

func TestClaimLegacyEntriesRequiresIdentityAndDoesNothingWithoutLegacyData(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "conversations.json"))
	if _, err := store.ClaimLegacyEntries(""); err == nil {
		t.Fatal("empty project identity was accepted")
	}
	if _, err := store.AddScoped("workspace:current", "s", "user", "scoped", []float32{1}, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	result, err := store.ClaimLegacyEntries("workspace:current")
	if err != nil {
		t.Fatal(err)
	}
	if result.Claimed != 0 || result.AlreadyClaimed {
		t.Fatalf("no-op claim = %#v", result)
	}
	if _, err := os.Stat(result.MarkerPath); !os.IsNotExist(err) {
		t.Fatalf("no-op claim created marker: %v", err)
	}
}

func TestPreviewedLegacyICEClaimIsExplicitAndCountBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	store := NewStore(path)
	if _, err := store.Add("legacy", "user", "one", []float32{1}, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	preview, err := store.PreviewLegacyEntries("workspace:current")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Count != 1 || preview.AlreadyClaimed {
		t.Fatalf("preview = %#v", preview)
	}
	if _, err := os.Stat(preview.MarkerPath); !os.IsNotExist(err) {
		t.Fatalf("preview created marker: %v", err)
	}
	if _, err := store.Add("late", "assistant", "two", []float32{1}, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.claimLegacyEntries("workspace:current", preview.Count); !errors.Is(err, ErrLegacyICEPreviewStale) {
		t.Fatalf("stale preview claim error = %v", err)
	}
	if _, err := os.Stat(preview.MarkerPath); !os.IsNotExist(err) {
		t.Fatalf("stale preview created marker: %v", err)
	}

	preview, err = store.PreviewLegacyEntries("workspace:current")
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.claimLegacyEntries("workspace:current", preview.Count)
	if err != nil {
		t.Fatal(err)
	}
	if result.Claimed != 2 {
		t.Fatalf("claim = %#v", result)
	}
}

func TestNewEngineRequiresWorkspaceIdentity(t *testing.T) {
	engine, err := NewEngine(nil, nil, EngineConfig{StorePath: filepath.Join(t.TempDir(), "conversations.json")})
	if err == nil || engine != nil {
		t.Fatalf("unscoped engine = %#v, err=%v", engine, err)
	}
}

func TestConcurrentLegacyICEClaimsSerializeOnStoreLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	seed := NewStore(path)
	if _, err := seed.Add("legacy", "user", "one owner only", []float32{1}, 0); err != nil {
		t.Fatal(err)
	}
	stores := []*Store{NewStore(path), NewStore(path)}
	projects := []string{"project-a", "project-b"}
	results := make([]LegacyEntryClaimResult, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range stores {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = stores[i].ClaimLegacyEntries(projects[i])
		}()
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner >= 0 {
				t.Fatalf("both projects claimed legacy entries: %#v", results)
			}
			winner = i
			continue
		}
		if !errors.Is(err, ErrLegacyICEClaimedByAnotherProject) {
			t.Fatalf("losing claim error = %v", err)
		}
	}
	if winner < 0 || results[winner].Claimed != 1 {
		t.Fatalf("winner=%d results=%#v errors=%v", winner, results, errs)
	}
	reloaded := NewStore(path)
	if len(reloaded.entries) != 1 || reloaded.entries[0].ProjectID != projects[winner] {
		t.Fatalf("durable claimed entries = %#v", reloaded.entries)
	}
}

func TestPreviewRecoversPendingClaimAfterStoreCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	store := NewStore(path)
	if _, err := store.Add("legacy", "user", "committed before crash", []float32{1}, 0); err != nil {
		t.Fatal(err)
	}
	markerPath := path + ".workspace-claim.json"
	marker := legacyICEClaimMarker{Version: legacyICEClaimVersion, ProjectID: "project", Status: "pending"}
	markerData, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := installICEClaimMarker(markerPath, markerData); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	store.entries[0].ProjectID = "project"
	store.dirty = true
	if err := store.persist(); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.mu.Unlock()

	preview, err := store.PreviewLegacyEntries("project")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Count != 0 || !preview.AlreadyClaimed {
		t.Fatalf("recovered preview = %#v", preview)
	}
	completed, exists, err := readICEClaimMarker(markerPath)
	if err != nil || !exists || completed.Status != "complete" {
		t.Fatalf("recovered marker = %#v exists=%v err=%v", completed, exists, err)
	}
}

func TestLegacyICEMarkerReadsAreBoundedAndNoFollow(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conversations.json")
		store := NewStore(path)
		victim := filepath.Join(t.TempDir(), "marker.json")
		victimData := []byte(`{"version":1,"project_id":"project","status":"pending"}`)
		if err := os.WriteFile(victim, victimData, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, path+".workspace-claim.json"); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := store.PreviewLegacyEntries("project"); !errors.Is(err, safeio.ErrSymlink) {
			t.Fatalf("symlink marker error = %v", err)
		}
		data, err := os.ReadFile(victim)
		if err != nil || string(data) != string(victimData) {
			t.Fatalf("marker victim changed: data=%q err=%v", data, err)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conversations.json")
		store := NewStore(path)
		markerPath := path + ".workspace-claim.json"
		if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(markerPath, maxLegacyICEClaimMarkerBytes+1); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PreviewLegacyEntries("project"); !errors.Is(err, safeio.ErrTooLarge) {
			t.Fatalf("oversized marker error = %v", err)
		}
	})
}
