package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func TestClaimLegacyFileForWorkspaceIsOneTimeAndBackedUp(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "memories.json")
	legacy := NewStore(legacyPath)
	if _, err := legacy.Save("legacy fact", []string{"legacy"}); err != nil {
		t.Fatal(err)
	}
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	targetA := filepath.Join(dir, "memory", "workspace-a.json")
	targetB := filepath.Join(dir, "memory", "workspace-b.json")

	result, err := ClaimLegacyFileForWorkspace(legacyPath, targetA, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Claimed || result.AlreadyClaimed {
		t.Fatalf("first claim result = %#v", result)
	}
	for _, path := range []string{result.BackupPath, result.MarkerPath, targetA} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("mode of %s = %04o, want 0600", path, got)
		}
	}
	if NewStore(result.BackupPath).Count() != 1 {
		t.Fatal("legacy backup does not preserve the original memory")
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("live global legacy file survived claim: %v", err)
	}
	claimed := NewStore(targetA)
	if claimed.Count() != 1 {
		t.Fatalf("claimed memories = %d, want 1", claimed.Count())
	}
	if _, err := claimed.Save("workspace-only", nil); err != nil {
		t.Fatal(err)
	}

	result, err = ClaimLegacyFileForWorkspace(legacyPath, targetA, workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	if result.Claimed || !result.AlreadyClaimed {
		t.Fatalf("repeat claim result = %#v", result)
	}
	if NewStore(targetA).Count() != 2 {
		t.Fatal("idempotent claim overwrote the evolved workspace store")
	}

	if _, err := ClaimLegacyFileForWorkspace(legacyPath, targetB, workspaceB); !errors.Is(err, ErrLegacyMemoryClaimedByAnotherWorkspace) {
		t.Fatalf("second-workspace claim error = %v", err)
	}
	if _, err := os.Stat(targetB); !os.IsNotExist(err) {
		t.Fatalf("rejected workspace target exists: %v", err)
	}
}

func TestClaimLegacyFileForWorkspaceRefusesAmbiguousOrCorruptSource(t *testing.T) {
	dir := t.TempDir()
	workspace := t.TempDir()
	legacyPath := filepath.Join(dir, "memories.json")
	targetPath := filepath.Join(dir, "scoped.json")
	if err := os.WriteFile(legacyPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimLegacyFileForWorkspace(legacyPath, targetPath, workspace); err == nil {
		t.Fatal("corrupt legacy memory was migrated")
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("corrupt migration created target: %v", err)
	}

	if err := os.WriteFile(legacyPath, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte(`[{"id":"existing","content":"scoped fact"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimLegacyFileForWorkspace(legacyPath, targetPath, workspace); err == nil {
		t.Fatal("pre-existing non-empty target without marker was overwritten")
	}
}

func TestClaimLegacyFileForWorkspaceAdoptsAndBacksUpEmptyScopedTarget(t *testing.T) {
	dir := t.TempDir()
	workspace := t.TempDir()
	legacyPath := filepath.Join(dir, "memories.json")
	targetPath := filepath.Join(dir, "memory", "scoped.json")
	legacy := NewStore(legacyPath)
	if _, err := legacy.Save("legacy fact", []string{"legacy"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		t.Fatal(err)
	}
	emptyTarget := []byte("null")
	if err := os.WriteFile(targetPath, emptyTarget, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ClaimLegacyFileForWorkspace(legacyPath, targetPath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Claimed {
		t.Fatalf("claim result = %#v", result)
	}
	backedUpTarget, err := os.ReadFile(result.TargetBackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backedUpTarget) != string(emptyTarget) {
		t.Fatalf("empty target backup = %q, want %q", backedUpTarget, emptyTarget)
	}
	if NewStore(targetPath).Count() != 1 {
		t.Fatal("legacy memory was not installed into the empty scoped store")
	}
	if info, err := os.Stat(result.TargetBackupPath); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("target backup mode = %04o, want 0600", got)
	}
}

func TestPreviewedDefaultLegacyClaimIsExplicitAndByteBound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := t.TempDir()
	legacyPath := DefaultPathForWorkspace("")
	legacy := NewStore(legacyPath)
	if _, err := legacy.Save("first", nil); err != nil {
		t.Fatal(err)
	}

	preview, err := PreviewDefaultLegacyForWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Count != 1 || preview.SourceSHA256 == "" || preview.AlreadyClaimed {
		t.Fatalf("preview = %#v", preview)
	}
	for _, path := range []string{preview.BackupPath, preview.MarkerPath, preview.ScopedPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("read-only preview created %s: %v", path, err)
		}
	}

	if _, err := legacy.Save("changed after preview", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimPreviewedDefaultLegacyForWorkspace(workspace, preview); !errors.Is(err, ErrLegacyMemoryPreviewStale) {
		t.Fatalf("stale preview claim error = %v", err)
	}
	if _, err := os.Stat(preview.MarkerPath); !os.IsNotExist(err) {
		t.Fatalf("stale confirmation created marker: %v", err)
	}

	preview, err = PreviewDefaultLegacyForWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ClaimPreviewedDefaultLegacyForWorkspace(workspace, preview)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Claimed || NewStore(preview.ScopedPath).Count() != 2 {
		t.Fatalf("explicit result=%#v scoped_count=%d", result, NewStore(preview.ScopedPath).Count())
	}
}

func TestConcurrentLegacyClaimsSerializeOnTheSourceStore(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "memories.json")
	legacy := NewStore(legacyPath)
	if _, err := legacy.Save("one owner only", nil); err != nil {
		t.Fatal(err)
	}
	workspaces := []string{t.TempDir(), t.TempDir()}
	targets := []string{filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")}
	errs := make([]error, 2)
	results := make([]LegacyClaimResult, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workspaces {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = ClaimLegacyFileForWorkspace(legacyPath, targets[i], workspaces[i])
		}()
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner >= 0 {
				t.Fatalf("both workspaces claimed the same source: results=%#v", results)
			}
			winner = i
			continue
		}
		if !errors.Is(err, ErrLegacyMemoryClaimedByAnotherWorkspace) {
			t.Fatalf("losing claim error = %v", err)
		}
	}
	if winner < 0 || !results[winner].Claimed || NewStore(targets[winner]).Count() != 1 {
		t.Fatalf("winning claim = %d, results=%#v errors=%v", winner, results, errs)
	}
}

func TestLegacyClaimAndStoreWriterNeverLoseMemory(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "memories.json")
	targetPath := filepath.Join(dir, "scoped.json")
	legacy := NewStore(legacyPath)
	if _, err := legacy.Save("before claim", nil); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var claimErr, saveErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, claimErr = ClaimLegacyFileForWorkspace(legacyPath, targetPath, t.TempDir())
	}()
	go func() {
		defer wg.Done()
		<-start
		_, saveErr = legacy.Save("concurrent writer", nil)
	}()
	close(start)
	wg.Wait()
	if claimErr != nil || saveErr != nil {
		t.Fatalf("claim error=%v save error=%v", claimErr, saveErr)
	}

	total := NewStore(targetPath).Count()
	if leftover := NewStore(legacyPath); leftover.Err() == nil {
		total += leftover.Count()
	}
	if total != 2 {
		t.Fatalf("claim/write race preserved %d memories, want 2", total)
	}
}

func TestLegacySourceSwapIsQuarantinedInsteadOfDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	original := []byte(`[{"id":1,"content":"original"}]`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(original)

	replacement := []byte(`[{"id":2,"content":"new writer data"}]`)
	tmp := filepath.Join(filepath.Dir(path), "replacement.tmp")
	if err := os.WriteFile(tmp, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyMemorySourceIfUnchanged(path, hex.EncodeToString(digest[:]), originalInfo); err == nil {
		t.Fatal("replaced source was treated as the verified original")
	}
	quarantinePath := path + ".workspace-claim.consumed"
	data, err := os.ReadFile(quarantinePath)
	if err != nil || string(data) != string(replacement) {
		t.Fatalf("replacement was not preserved: data=%q err=%v", data, err)
	}
}

func TestCompletedClaimRecoversJournaledSourceQuarantine(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "memories.json")
	targetPath := filepath.Join(dir, "scoped.json")
	workspace := t.TempDir()
	legacy := NewStore(legacyPath)
	if _, err := legacy.Save("recover after crash", nil); err != nil {
		t.Fatal(err)
	}
	preview, err := previewLegacyFileForWorkspace(legacyPath, targetPath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ClaimLegacyFileForWorkspace(legacyPath, targetPath, workspace)
	if err != nil {
		t.Fatal(err)
	}

	// Recreate the exact post-rename/pre-verification crash state. The complete
	// marker is durable and the claimed bytes are stranded in the deterministic
	// consumed path.
	backupData, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	quarantinePath := legacyPath + ".workspace-claim.consumed"
	if err := os.WriteFile(quarantinePath, backupData, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = ClaimLegacyFileForWorkspace(legacyPath, targetPath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyClaimed || result.Claimed || preview.SourceSHA256 == "" {
		t.Fatalf("recovered result=%#v preview=%#v", result, preview)
	}
	if _, err := os.Stat(quarantinePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified consumed source was not finalized: %v", err)
	}
}

func TestLegacyMigrationReadsAreBoundedAndNoFollow(t *testing.T) {
	t.Run("oversized source", func(t *testing.T) {
		dir := t.TempDir()
		legacyPath := filepath.Join(dir, "memories.json")
		if err := os.WriteFile(legacyPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(legacyPath, maxMemoryStoreBytes+1); err != nil {
			t.Fatal(err)
		}
		_, err := previewLegacyFileForWorkspace(legacyPath, filepath.Join(dir, "target.json"), t.TempDir())
		if !errors.Is(err, safeio.ErrTooLarge) {
			t.Fatalf("oversized source error = %v", err)
		}
	})

	t.Run("symlink marker", func(t *testing.T) {
		dir := t.TempDir()
		legacyPath := filepath.Join(dir, "memories.json")
		if err := os.WriteFile(legacyPath, []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(t.TempDir(), "marker.json")
		victimData := []byte(`{"version":1,"status":"pending"}`)
		if err := os.WriteFile(victim, victimData, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, legacyPath+".workspace-claim.json"); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		_, err := previewLegacyFileForWorkspace(legacyPath, filepath.Join(dir, "target.json"), t.TempDir())
		if !errors.Is(err, safeio.ErrSymlink) {
			t.Fatalf("symlink marker error = %v", err)
		}
		data, readErr := os.ReadFile(victim)
		if readErr != nil || string(data) != string(victimData) {
			t.Fatalf("marker victim changed: data=%q err=%v", data, readErr)
		}
	})

	t.Run("oversized marker", func(t *testing.T) {
		dir := t.TempDir()
		legacyPath := filepath.Join(dir, "memories.json")
		if err := os.WriteFile(legacyPath, []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
		markerPath := legacyPath + ".workspace-claim.json"
		if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(markerPath, maxLegacyMemoryMarkerBytes+1); err != nil {
			t.Fatal(err)
		}
		_, err := previewLegacyFileForWorkspace(legacyPath, filepath.Join(dir, "target.json"), t.TempDir())
		if !errors.Is(err, safeio.ErrTooLarge) {
			t.Fatalf("oversized marker error = %v", err)
		}
	})
}
