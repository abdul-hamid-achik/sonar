package ice

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func mustAdd(t *testing.T, s *Store, sessionID, role, content string, embedding []float32, turnIndex int) int {
	t.Helper()
	id, err := s.Add(sessionID, role, content, embedding, turnIndex)
	if err != nil {
		t.Fatalf("add %q: %v", content, err)
	}
	return id
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float32
		tol  float32
	}{
		{
			name: "identical vectors",
			a:    []float32{1, 2, 3},
			b:    []float32{1, 2, 3},
			want: 1.0,
			tol:  1e-6,
		},
		{
			name: "orthogonal vectors",
			a:    []float32{1, 0},
			b:    []float32{0, 1},
			want: 0.0,
			tol:  1e-6,
		},
		{
			name: "opposite vectors",
			a:    []float32{1, 0},
			b:    []float32{-1, 0},
			want: -1.0,
			tol:  1e-6,
		},
		{
			name: "different lengths returns 0",
			a:    []float32{1, 0},
			b:    []float32{1, 0, 0},
			want: 0,
			tol:  0,
		},
		{
			name: "zero vector returns 0",
			a:    []float32{0, 0},
			b:    []float32{1, 1},
			want: 0,
			tol:  0,
		},
		{
			name: "known value with tolerance",
			a:    []float32{1, 1},
			b:    []float32{1, 0},
			// 1/(sqrt(2)*1) ≈ 0.7071
			want: float32(1.0 / math.Sqrt(2)),
			tol:  1e-4,
		},
		{
			name: "empty vectors",
			a:    []float32{},
			b:    []float32{},
			want: 0,
			tol:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			diff := got - tt.want
			if diff < 0 {
				diff = -diff
			}
			if diff > tt.tol {
				t.Errorf("cosineSimilarity(%v, %v) = %f, want %f (±%f)",
					tt.a, tt.b, got, tt.want, tt.tol)
			}
		})
	}
}

func TestStore_Add_And_Count(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")

	s := NewStore(path)

	if s.Count() != 0 {
		t.Fatalf("new store Count = %d, want 0", s.Count())
	}

	id1, err := s.Add("sess1", "user", "hello", []float32{1, 0, 0}, 0)
	if err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if id1 != 1 {
		t.Errorf("first Add returned id=%d, want 1", id1)
	}
	if s.Count() != 1 {
		t.Errorf("Count after first Add = %d, want 1", s.Count())
	}

	id2, err := s.Add("sess1", "assistant", "world", []float32{0, 1, 0}, 1)
	if err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if id2 != 2 {
		t.Errorf("second Add returned id=%d, want 2", id2)
	}
	if s.Count() != 2 {
		t.Errorf("Count after second Add = %d, want 2", s.Count())
	}
}

func TestStore_Search(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	s := NewStore(path)

	// Add entries with known embeddings.
	mustAdd(t, s, "sess1", "user", "entry A", []float32{1, 0, 0}, 0)
	mustAdd(t, s, "sess1", "user", "entry B", []float32{0, 1, 0}, 1)
	mustAdd(t, s, "sess2", "user", "entry C", []float32{0.9, 0.1, 0}, 0)
	mustAdd(t, s, "sess2", "user", "entry D", []float32{0, 0, 1}, 1) // orthogonal to query

	t.Run("similarity filtering and sorting", func(t *testing.T) {
		// Query similar to entries A and C, exclude no session.
		results := s.Search([]float32{1, 0, 0}, "", 10)
		if len(results) == 0 {
			t.Fatal("expected results, got 0")
		}
		// Entry A should be highest (identical to query).
		if results[0].Entry.Content != "entry A" {
			t.Errorf("top result = %q, want 'entry A'", results[0].Entry.Content)
		}
	})

	t.Run("session exclusion", func(t *testing.T) {
		results := s.Search([]float32{1, 0, 0}, "sess1", 10)
		for _, r := range results {
			if r.Entry.SessionID == "sess1" {
				t.Errorf("excluded session sess1 should not appear in results")
			}
		}
	})

	t.Run("min threshold 0.3", func(t *testing.T) {
		// Entry D: [0,0,1] is orthogonal to [1,0,0] → similarity 0.
		results := s.Search([]float32{1, 0, 0}, "", 10)
		for _, r := range results {
			if r.Score < minSimilarityThreshold {
				t.Errorf("result %q has score %f below threshold %f",
					r.Entry.Content, r.Score, minSimilarityThreshold)
			}
		}
	})

	t.Run("topK limit", func(t *testing.T) {
		results := s.Search([]float32{1, 0, 0}, "", 1)
		if len(results) > 1 {
			t.Errorf("topK=1 but got %d results", len(results))
		}
	})

	t.Run("workspace scope", func(t *testing.T) {
		scoped := NewStore(filepath.Join(dir, "scoped.json"))
		if _, err := scoped.AddScoped("project-a", "a", "user", "A", []float32{1, 0}, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := scoped.AddScoped("project-b", "b", "user", "B", []float32{1, 0}, 0); err != nil {
			t.Fatal(err)
		}
		results := scoped.SearchScoped([]float32{1, 0}, "project-a", "", 10)
		if len(results) != 1 || results[0].Entry.Content != "A" {
			t.Fatalf("cross-project retrieval leaked: %#v", results)
		}
	})

	t.Run("empty store returns nil", func(t *testing.T) {
		emptyPath := filepath.Join(dir, "empty.json")
		empty := NewStore(emptyPath)
		results := empty.Search([]float32{1, 0}, "", 5)
		if results != nil {
			t.Errorf("empty store search should return nil, got %v", results)
		}
	})

	t.Run("empty query embedding returns nil", func(t *testing.T) {
		results := s.Search([]float32{}, "", 5)
		if results != nil {
			t.Errorf("empty query should return nil, got %v", results)
		}
	})
}

func TestStoreSearchResultsDoNotAliasEmbeddings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	store := NewStore(path)
	if _, err := store.AddScoped("project", "session", "user", "immutable result", []float32{1, 0}, 0); err != nil {
		t.Fatal(err)
	}
	results := store.SearchScoped([]float32{1, 0}, "project", "", 1)
	if len(results) != 1 || len(results[0].Entry.Embedding) != 2 {
		t.Fatalf("search result = %#v", results)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			results[0].Entry.Embedding[0] = -1
		}
	}()
	for i := 0; i < 1000; i++ {
		current := store.SearchScoped([]float32{1, 0}, "project", "", 1)
		if len(current) != 1 || current[0].Entry.Embedding[0] != 1 {
			t.Fatalf("returned embedding mutated store state: %#v", current)
		}
	}
	wg.Wait()
}

func TestStore_Flush_Persistence(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "store.json")

		s1 := NewStore(path)
		mustAdd(t, s1, "sess1", "user", "hello", []float32{1, 0}, 0)
		mustAdd(t, s1, "sess1", "assistant", "world", []float32{0, 1}, 1)

		if err := s1.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		// Reload from same path.
		s2 := NewStore(path)
		if s2.Count() != 2 {
			t.Errorf("reloaded store Count = %d, want 2", s2.Count())
		}
	})

	t.Run("corrupt JSON fails closed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "store.json")

		// Write corrupt JSON.
		if err := os.WriteFile(path, []byte("not valid json{{{"), 0o644); err != nil {
			t.Fatal(err)
		}

		s := NewStore(path)
		if s.Count() != 0 {
			t.Errorf("corrupt store Count = %d, want 0", s.Count())
		}
		if s.Err() == nil {
			t.Fatal("corrupt store did not report its load error")
		}
		if got := s.Search([]float32{1}, "", 5); got != nil {
			t.Fatalf("Search on corrupt store = %#v, want nil", got)
		}
		if _, err := s.Add("new", "user", "must not overwrite", []float32{1}, 0); err == nil {
			t.Fatal("corrupt store accepted a new entry")
		}
		if _, err := s.AddScoped("project", "new", "user", "must not overwrite", []float32{1}, 0); err == nil {
			t.Fatal("corrupt store accepted a new scoped entry")
		}
		if s.Count() != 0 {
			t.Fatalf("rejected mutations changed corrupt store count to %d", s.Count())
		}
		if err := s.Flush(); err == nil {
			t.Fatal("corrupt store was overwritten")
		}
		if err := s.persist(); err == nil {
			t.Fatal("corrupt store bypassed persistence guard")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "not valid json{{{" {
			t.Fatalf("corrupt source changed: %q, err=%v", data, err)
		}
	})

	t.Run("private permissions", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "private")
		path := filepath.Join(dir, "store.json")
		s := NewStore(path)
		mustAdd(t, s, "s", "user", "private", []float32{1}, 0)
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("store mode = %04o, want 0600", got)
		}
	})

	t.Run("nextID restoration", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "store.json")

		s1 := NewStore(path)
		mustAdd(t, s1, "sess1", "user", "first", []float32{1}, 0)
		mustAdd(t, s1, "sess1", "user", "second", []float32{1}, 1)
		if err := s1.Flush(); err != nil {
			t.Fatal(err)
		}

		s2 := NewStore(path)
		id := mustAdd(t, s2, "sess1", "user", "third", []float32{1}, 2)
		if id != 3 {
			t.Errorf("continued id = %d, want 3", id)
		}
	})
}

func TestStoreConcurrentInstancesMergeWritesWithUniqueIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
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
				id, err := store.AddScoped("project", fmt.Sprintf("session-%d", storeIndex), "user", fmt.Sprintf("entry-%d-%d", storeIndex, writeIndex), []float32{1, 0}, writeIndex)
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
		t.Fatalf("concurrent AddScoped: %v", err)
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
		t.Fatalf("durable ICE entries = %d, want %d", got, want)
	}
	for id := 1; id <= want; id++ {
		if !gotIDs[id] {
			t.Fatalf("ID sequence is missing %d", id)
		}
	}
}

func TestStoreReadAPIsSeeWritesFromAnotherLongLivedInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	writer := NewStore(path)
	reader := NewStore(path)
	if _, err := writer.AddScoped("project", "session", "user", "visible across stores", []float32{1, 0}, 0); err != nil {
		t.Fatal(err)
	}
	if got := reader.Count(); got != 1 {
		t.Fatalf("stale Count = %d, want 1", got)
	}
	if got := reader.CountScoped("project"); got != 1 {
		t.Fatalf("stale CountScoped = %d, want 1", got)
	}
	results := reader.SearchScoped([]float32{1, 0}, "project", "", 1)
	if len(results) != 1 || results[0].Entry.Content != "visible across stores" {
		t.Fatalf("stale SearchScoped = %#v", results)
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
	store := NewStore(filepath.Join(dir, "conversations.json"))
	if _, err := store.AddScoped("project", "session", "user", "private entry", []float32{1}, 0); err != nil {
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
	path := filepath.Join(t.TempDir(), "conversations.json")
	store := NewStore(path)
	if _, err := store.AddScoped("project", "session", "user", "durable", []float32{1}, 0); err != nil {
		t.Fatal(err)
	}
	oldLimit := iceStoreWriteLimit
	iceStoreWriteLimit = 512
	t.Cleanup(func() { iceStoreWriteLimit = oldLimit })
	if _, err := store.AddScoped("project", "session", "user", strings.Repeat("x", 1024), []float32{1}, 1); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("crossing mutation error = %v", err)
	}
	if got := NewStore(path).Count(); got != 1 {
		t.Fatalf("oversized mutation changed durable count to %d", got)
	}
}

func TestStoreReadFailureFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	store := NewStore(path)
	if store.Err() == nil {
		t.Fatal("directory-backed ICE store did not report a read error")
	}
	if got := store.Search([]float32{1}, "", 5); got != nil {
		t.Fatalf("Search on unreadable store = %#v, want nil", got)
	}
	if _, err := store.Add("session", "user", "overwrite", []float32{1}, 0); err == nil {
		t.Fatal("unreadable ICE store accepted Add")
	}
	if _, err := store.AddScoped("project", "session", "user", "overwrite", []float32{1}, 0); err == nil {
		t.Fatal("unreadable ICE store accepted AddScoped")
	}
	if err := store.Flush(); err == nil {
		t.Fatal("unreadable ICE store accepted Flush")
	}
	if err := store.persist(); err == nil {
		t.Fatal("unreadable ICE store bypassed persistence guard")
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
	path := filepath.Join(t.TempDir(), "conversations.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxICEStoreBytes+1); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if !errors.Is(store.Err(), safeio.ErrTooLarge) {
		t.Fatalf("oversized store error = %v", store.Err())
	}
	if _, err := store.Add("session", "user", "must not overwrite", []float32{1}, 0); err == nil {
		t.Fatal("oversized ICE store accepted mutation")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != maxICEStoreBytes+1 {
		t.Fatalf("oversized source changed: info=%v err=%v", info, err)
	}
}

func TestStoreLoadRejectsSymlinkWithoutTouchingVictim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
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
	if _, err := store.Add("session", "user", "must not overwrite", []float32{1}, 0); err == nil {
		t.Fatal("symlink ICE store accepted mutation")
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

func TestStoreMutationRejectsParentSwappedToSymlink(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "managed")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "conversations.json")
	store := NewStore(path)
	parked := filepath.Join(root, "managed-original")
	if err := os.Rename(parent, parked); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, parent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := store.AddScoped("project", "session", "user", "must stay confined", []float32{1}, 0); !errors.Is(err, safeio.ErrSymlink) {
		t.Fatalf("swapped parent mutation error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "conversations.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside store was published: %v", err)
	}
}

func TestStoreLoadSecuresVerifiedDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
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
		t.Fatalf("loaded ICE store mode = %v, err=%v", info, err)
	}
}
