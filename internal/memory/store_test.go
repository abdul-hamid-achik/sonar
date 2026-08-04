package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func mustSave(t *testing.T, s *Store, content string, tags []string) int {
	t.Helper()
	id, err := s.Save(content, tags)
	if err != nil {
		t.Fatalf("save %q: %v", content, err)
	}
	return id
}

func TestStore_Save_And_Count(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")

	s := NewStore(path)
	if s.Count() != 0 {
		t.Fatalf("new store Count = %d, want 0", s.Count())
	}

	id1, err := s.Save("first memory", []string{"tag1"})
	if err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if id1 != 1 {
		t.Errorf("first Save id = %d, want 1", id1)
	}
	if s.Count() != 1 {
		t.Errorf("Count after first Save = %d, want 1", s.Count())
	}

	id2, err := s.Save("second memory", []string{"tag2"})
	if err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if id2 != 2 {
		t.Errorf("second Save id = %d, want 2", id2)
	}
	if s.Count() != 2 {
		t.Errorf("Count after second Save = %d, want 2", s.Count())
	}
}

func TestStore_Recall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")
	s := NewStore(path)

	mustSave(t, s, "the user prefers Go language", []string{"preference", "golang"})
	mustSave(t, s, "project uses PostgreSQL database", []string{"tech", "database"})
	mustSave(t, s, "user name is Alice", []string{"name"})

	t.Run("content match", func(t *testing.T) {
		results, _ := s.Recall("Go", 10)
		if len(results) == 0 {
			t.Fatal("expected results for 'Go' query")
		}
		found := false
		for _, r := range results {
			if r.Content == "the user prefers Go language" {
				found = true
			}
		}
		if !found {
			t.Error("expected to find 'the user prefers Go language'")
		}
	})

	t.Run("tag match", func(t *testing.T) {
		results, _ := s.Recall("golang", 10)
		if len(results) == 0 {
			t.Fatal("expected results for 'golang' tag query")
		}
		if results[0].Content != "the user prefers Go language" {
			t.Errorf("top result = %q, want 'the user prefers Go language'", results[0].Content)
		}
	})

	t.Run("combined scoring", func(t *testing.T) {
		// "database" matches both content and tag for PostgreSQL entry.
		results, _ := s.Recall("database", 10)
		if len(results) == 0 {
			t.Fatal("expected results for 'database' query")
		}
		if results[0].Content != "project uses PostgreSQL database" {
			t.Errorf("top result = %q, want 'project uses PostgreSQL database'",
				results[0].Content)
		}
	})

	t.Run("maxResults limit", func(t *testing.T) {
		results, _ := s.Recall("user", 1)
		if len(results) > 1 {
			t.Errorf("maxResults=1 but got %d results", len(results))
		}
	})

	t.Run("default maxResults 5 when 0", func(t *testing.T) {
		// With 3 memories, should return all 3 (default limit is 5).
		results, _ := s.Recall("user", 0)
		if len(results) > 5 {
			t.Errorf("default maxResults should be 5, got %d results", len(results))
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		results, _ := s.Recall("ALICE", 10)
		if len(results) == 0 {
			t.Fatal("expected case-insensitive match for 'ALICE'")
		}
		if results[0].Content != "user name is Alice" {
			t.Errorf("result = %q, want 'user name is Alice'", results[0].Content)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		results, _ := s.Recall("xyzzyzxyz", 10)
		if len(results) != 0 {
			t.Errorf("expected no results for nonsense query, got %d", len(results))
		}
	})
}

func TestStore_Recall_TieBreakByRecency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")
	s := NewStore(path)

	// Save two memories with the same scoring potential.
	mustSave(t, s, "alpha topic info", []string{"info"})
	// Small delay so LastUsed differs.
	time.Sleep(10 * time.Millisecond)
	mustSave(t, s, "beta topic info", []string{"info"})

	results, _ := s.Recall("info", 10)
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}
	// Both match tag "info" equally (+3), so more recent (beta) should come first.
	if results[0].Content != "beta topic info" {
		t.Errorf("expected more recent 'beta topic info' first, got %q", results[0].Content)
	}
}

func TestStore_Recent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")
	s := NewStore(path)

	mustSave(t, s, "old memory", nil)
	time.Sleep(10 * time.Millisecond)
	mustSave(t, s, "new memory", nil)

	t.Run("ordering by LastUsed", func(t *testing.T) {
		recent, _ := s.Recent(2)
		if len(recent) != 2 {
			t.Fatalf("Recent(2) returned %d, want 2", len(recent))
		}
		if recent[0].Content != "new memory" {
			t.Errorf("first recent = %q, want 'new memory'", recent[0].Content)
		}
		if recent[1].Content != "old memory" {
			t.Errorf("second recent = %q, want 'old memory'", recent[1].Content)
		}
	})

	t.Run("limit exceeds count returns all", func(t *testing.T) {
		recent, _ := s.Recent(100)
		if len(recent) != 2 {
			t.Errorf("Recent(100) returned %d, want 2", len(recent))
		}
	})

	t.Run("empty store", func(t *testing.T) {
		emptyPath := filepath.Join(dir, "empty.json")
		empty := NewStore(emptyPath)
		recent, _ := empty.Recent(5)
		if recent != nil {
			t.Errorf("empty Recent should return nil, got %v", recent)
		}
	})

	t.Run("non-positive limit", func(t *testing.T) {
		if recent, _ := s.Recent(-1); recent != nil {
			t.Fatalf("Recent(-1) = %#v, want nil", recent)
		}
	})
}

func TestStorePrivateAndCorruptionSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "memories.json")
	store := NewStore(path)
	if _, err := store.Save("secret project fact", nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("memory mode = %04o, want 0600", got)
	}

	corruptPath := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(corruptPath, []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	corrupt := NewStore(corruptPath)
	if corrupt.Err() == nil {
		t.Fatal("corrupt memory store did not fail closed")
	}
	if got, _ := corrupt.Recall("anything", 5); got != nil {
		t.Fatalf("Recall on corrupt store = %#v, want nil", got)
	}
	mutations := []struct {
		name string
		run  func() error
	}{
		{name: "save", run: func() error { _, err := corrupt.Save("overwrite", nil); return err }},
		{name: "delete", run: func() error { _, err := corrupt.Delete(1); return err }},
		{name: "delete by tag", run: func() error { _, err := corrupt.DeleteByTag("tag"); return err }},
		{name: "update", run: func() error { _, err := corrupt.Update(1, "overwrite", nil); return err }},
		{name: "prune", run: func() error { _, err := corrupt.Prune(time.Hour); return err }},
		{name: "persist", run: corrupt.persist},
	}
	for _, mutation := range mutations {
		t.Run("corrupt "+mutation.name, func(t *testing.T) {
			if err := mutation.run(); err == nil {
				t.Fatalf("corrupt memory store accepted %s", mutation.name)
			}
		})
	}
	data, err := os.ReadFile(corruptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "not-json" {
		t.Fatalf("corrupt memory store was overwritten: %q", data)
	}
}

func TestStoreReadFailureFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	store := NewStore(path)
	if store.Err() == nil {
		t.Fatal("directory-backed memory store did not report a read error")
	}
	if got, _ := store.Recall("anything", 5); got != nil {
		t.Fatalf("Recall on unreadable store = %#v, want nil", got)
	}
	mutations := []struct {
		name string
		run  func() error
	}{
		{name: "save", run: func() error { _, err := store.Save("overwrite", nil); return err }},
		{name: "delete", run: func() error { _, err := store.Delete(1); return err }},
		{name: "delete by tag", run: func() error { _, err := store.DeleteByTag("tag"); return err }},
		{name: "update", run: func() error { _, err := store.Update(1, "overwrite", nil); return err }},
		{name: "prune", run: func() error { _, err := store.Prune(time.Hour); return err }},
		{name: "persist", run: store.persist},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			if err := mutation.run(); err == nil {
				t.Fatalf("unreadable memory store accepted %s", mutation.name)
			}
		})
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("failed read path was replaced by a regular file")
	}
}

func TestStoreLoadRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxMemoryStoreBytes+1); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if !errors.Is(store.Err(), safeio.ErrTooLarge) {
		t.Fatalf("oversized store error = %v", store.Err())
	}
	if _, err := store.Save("must not overwrite", nil); err == nil {
		t.Fatal("oversized store accepted mutation")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != maxMemoryStoreBytes+1 {
		t.Fatalf("oversized source changed: info=%v err=%v", info, err)
	}
}

func TestStoreLoadRejectsSymlinkWithoutTouchingVictim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	victim := filepath.Join(t.TempDir(), "victim.json")
	victimData := []byte(`[{"id":99,"content":"outside secret"}]`)
	if err := os.WriteFile(victim, victimData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store := NewStore(path)
	if !errors.Is(store.Err(), safeio.ErrSymlink) {
		t.Fatalf("symlink store error = %v", store.Err())
	}
	if _, err := store.Save("must not overwrite", nil); err == nil {
		t.Fatal("symlink store accepted mutation")
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != string(victimData) {
		t.Fatalf("victim content changed: data=%q err=%v", data, err)
	}
	info, err := os.Stat(victim)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("victim mode changed: info=%v err=%v", info, err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("store symlink changed: info=%v err=%v", info, err)
	}
}

func TestStoreLoadSecuresVerifiedDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if err := store.Err(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("loaded store mode = %v, err=%v", info, err)
	}
}

func TestDefaultPathForWorkspaceIsScoped(t *testing.T) {
	first := DefaultPathForWorkspace(t.TempDir())
	second := DefaultPathForWorkspace(t.TempDir())
	if first == second {
		t.Fatalf("different workspaces share memory path %q", first)
	}
	if filepath.Dir(first) != filepath.Dir(second) {
		t.Fatalf("workspace stores should share a managed parent: %q, %q", first, second)
	}
}

func TestStore_Persistence_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")

	s1 := NewStore(path)
	mustSave(t, s1, "persistent memory", []string{"test"})
	mustSave(t, s1, "another memory", []string{"test2"})

	// Create new store from same path.
	s2 := NewStore(path)
	if s2.Count() != 2 {
		t.Errorf("reloaded Count = %d, want 2", s2.Count())
	}

	// Verify data is intact.
	recent, _ := s2.Recent(2)
	contents := map[string]bool{}
	for _, m := range recent {
		contents[m.Content] = true
	}
	if !contents["persistent memory"] {
		t.Error("missing 'persistent memory' after reload")
	}
	if !contents["another memory"] {
		t.Error("missing 'another memory' after reload")
	}

	// Verify IDs continue.
	id, err := s2.Save("third", nil)
	if err != nil {
		t.Fatalf("Save after reload: %v", err)
	}
	if id != 3 {
		t.Errorf("continued id = %d, want 3", id)
	}
}

func TestStoreConcurrentInstancesMergeWritesWithUniqueIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	stores := []*Store{NewStore(path), NewStore(path)}
	const writesPerStore = 20

	start := make(chan struct{})
	ids := make(chan int, len(stores)*writesPerStore)
	errs := make(chan error, len(stores)*writesPerStore)
	var wg sync.WaitGroup
	for storeIndex, store := range stores {
		storeIndex, store := storeIndex, store
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for writeIndex := 0; writeIndex < writesPerStore; writeIndex++ {
				id, err := store.Save(fmt.Sprintf("store-%d-memory-%d", storeIndex, writeIndex), nil)
				if err != nil {
					errs <- err
					return
				}
				ids <- id
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatalf("concurrent Save: %v", err)
	}

	want := len(stores) * writesPerStore
	gotIDs := make(map[int]bool, want)
	for id := range ids {
		if gotIDs[id] {
			t.Fatalf("duplicate ID %d returned by concurrent stores", id)
		}
		gotIDs[id] = true
	}
	if len(gotIDs) != want {
		t.Fatalf("unique IDs = %d, want %d", len(gotIDs), want)
	}
	reloaded := NewStore(path)
	if err := reloaded.Err(); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Count(); got != want {
		t.Fatalf("durable memories = %d, want %d", got, want)
	}
	for id := 1; id <= want; id++ {
		if !gotIDs[id] {
			t.Fatalf("ID sequence is missing %d", id)
		}
	}
}

func TestStoreRecallReloadsBeforePersistingAlongsideAnotherWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	recaller := NewStore(path)
	writer := NewStore(path)
	seed := NewStore(path)
	if _, err := seed.Save("alpha shared fact", nil); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var recalled []Memory
	var saveErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		recalled, _ = recaller.Recall("alpha", 5)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, saveErr = writer.Save("concurrent second fact", nil)
	}()
	close(start)
	wg.Wait()
	if saveErr != nil {
		t.Fatal(saveErr)
	}
	if len(recalled) != 1 || recalled[0].Content != "alpha shared fact" {
		t.Fatalf("stale recaller did not reload seed: %#v", recalled)
	}
	if got := NewStore(path).Count(); got != 2 {
		t.Fatalf("Recall stale-overwrote concurrent Save: durable count = %d, want 2", got)
	}
}

func TestStoreReadAPIsSeeWritesFromAnotherLongLivedInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	writer := NewStore(path)
	reader := NewStore(path)
	id, err := writer.Save("visible across stores", []string{"shared"})
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.Count(); got != 1 {
		t.Fatalf("stale Count = %d, want 1", got)
	}
	recent, _ := reader.Recent(1)
	if len(recent) != 1 || recent[0].ID != id {
		t.Fatalf("stale Recent = %#v", recent)
	}
	got, found := reader.Get(id)
	if !found || got.Content != "visible across stores" {
		t.Fatalf("stale Get = %#v, found=%v", got, found)
	}
}

func TestStoreDoesNotChangeExistingParentDirectoryMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "workspace-docs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := NewStore(filepath.Join(dir, "memories.json"))
	if _, err := store.Save("private file in shared directory", nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("existing parent mode = %04o, want 0755", got)
	}
}

func TestStoreRejectsMutationThatWouldExceedReloadLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	store := NewStore(path)
	if _, err := store.Save("durable", nil); err != nil {
		t.Fatal(err)
	}
	oldLimit := memoryStoreWriteLimit
	memoryStoreWriteLimit = 512
	t.Cleanup(func() { memoryStoreWriteLimit = oldLimit })
	if _, err := store.Save(strings.Repeat("x", 1024), nil); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("crossing mutation error = %v", err)
	}
	if got := NewStore(path).Count(); got != 1 {
		t.Fatalf("oversized mutation changed durable count to %d", got)
	}
}

func TestStore_Delete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")
	s := NewStore(path)

	id := mustSave(t, s, "to be deleted", []string{"temp"})
	if s.Count() != 1 {
		t.Fatalf("expected 1 memory, got %d", s.Count())
	}

	deleted, err := s.Delete(id)
	if err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if !deleted {
		t.Error("Delete returned false for existing memory")
	}
	if s.Count() != 0 {
		t.Errorf("Count after delete = %d, want 0", s.Count())
	}

	// Try deleting non-existent.
	deleted, err = s.Delete(999)
	if err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if deleted {
		t.Error("Delete should return false for non-existent memory")
	}
}

func TestStore_Update(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")
	s := NewStore(path)

	id := mustSave(t, s, "original content", []string{"original"})

	updated, err := s.Update(id, "updated content", []string{"updated"})
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if !updated {
		t.Error("Update returned false for existing memory")
	}

	// Verify update.
	mem, found := s.Get(id)
	if !found {
		t.Fatal("memory not found after update")
	}
	if mem.Content != "updated content" {
		t.Errorf("Content = %q, want 'updated content'", mem.Content)
	}
	if len(mem.Tags) != 1 || mem.Tags[0] != "updated" {
		t.Errorf("Tags = %v, want ['updated']", mem.Tags)
	}

	// Try updating non-existent.
	updated, err = s.Update(999, "test", nil)
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if updated {
		t.Error("Update should return false for non-existent memory")
	}
}

func TestStore_DeleteByTag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")
	s := NewStore(path)

	mustSave(t, s, "keep this 1", []string{"keep"})
	mustSave(t, s, "delete this", []string{"temp"})
	mustSave(t, s, "keep this 2", []string{"keep"})
	mustSave(t, s, "delete this too", []string{"temp"})
	mustSave(t, s, "also keep", []string{"permanent"})

	deleted, err := s.DeleteByTag("temp")
	if err != nil {
		t.Fatalf("DeleteByTag returned error: %v", err)
	}
	if deleted != 2 {
		t.Errorf("DeleteByTag deleted = %d, want 2", deleted)
	}
	if s.Count() != 3 {
		t.Errorf("Count after delete = %d, want 3", s.Count())
	}

	// Verify only temp memories are gone.
	results, _ := s.Recall("keep", 10)
	if len(results) != 3 {
		t.Errorf("Recall returned %d, want 3", len(results))
	}
}

func TestStore_Get(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")
	s := NewStore(path)

	id := mustSave(t, s, "test memory", []string{"tag"})

	mem, found := s.Get(id)
	if !found {
		t.Fatal("Get returned false for existing memory")
	}
	if mem.Content != "test memory" {
		t.Errorf("Content = %q, want 'test memory'", mem.Content)
	}
	if len(mem.Tags) != 1 || mem.Tags[0] != "tag" {
		t.Errorf("Tags = %v, want ['tag']", mem.Tags)
	}

	// Try getting non-existent.
	_, found = s.Get(999)
	if found {
		t.Error("Get should return false for non-existent memory")
	}
}

func TestStoreReadResultsDoNotAliasNestedTags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.json")
	store := NewStore(path)
	id := mustSave(t, store, "immutable result", []string{"original"})

	recent, _ := store.Recent(1)
	got, found := store.Get(id)
	if len(recent) != 1 || !found {
		t.Fatal("saved memory unavailable")
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			recent[0].Tags[0] = "mutated-recent"
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			got.Tags[0] = "mutated-get"
		}
	}()
	for i := 0; i < 1000; i++ {
		current, ok := store.Get(id)
		if !ok || len(current.Tags) != 1 || current.Tags[0] != "original" {
			t.Fatalf("returned tags mutated store state: %#v", current)
		}
	}
	wg.Wait()
}

func TestStore_UpdatePartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.json")
	s := NewStore(path)

	id := mustSave(t, s, "original content", []string{"original", "tags"})

	// Update only content, keep tags.
	updated, err := s.Update(id, "new content", nil)
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if !updated {
		t.Error("Update returned false")
	}

	mem, _ := s.Get(id)
	if mem.Content != "new content" {
		t.Errorf("Content = %q, want 'new content'", mem.Content)
	}
	// Tags should remain unchanged when nil is passed.
	if len(mem.Tags) != 2 {
		t.Errorf("Tags = %v, want 2 tags", mem.Tags)
	}

	// Update only tags, keep content.
	updated, err = s.Update(id, "", []string{"only", "tags"})
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if !updated {
		t.Error("Update returned false")
	}

	mem, _ = s.Get(id)
	if mem.Content != "new content" {
		t.Errorf("Content changed unexpectedly to %q", mem.Content)
	}
	if len(mem.Tags) != 2 || mem.Tags[0] != "only" || mem.Tags[1] != "tags" {
		t.Errorf("Tags = %v, want ['only', 'tags']", mem.Tags)
	}
}
