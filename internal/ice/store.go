package ice

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

// timeNow is a variable for testing.
var timeNow = time.Now

const minSimilarityThreshold = 0.3

const maxICEStoreBytes int64 = 256 << 20

var iceStoreFileReader = safeio.NewReader()
var iceStoreReadTimeout = 5 * time.Second
var iceStoreLockTimeout = 5 * time.Second
var iceStoreWriteLimit = maxICEStoreBytes

// Store is a flat-file vector store for conversation history.
// It holds all entries in memory and persists to a JSON file.
type Store struct {
	mu         sync.Mutex
	path       string
	entries    []ConversationEntry
	nextID     int
	dirty      bool
	loadErr    error
	embedModel string // stamps new entries; filters search to same-model vectors
}

// NewStore loads an existing store from path or creates an empty one.
func NewStore(path string) *Store {
	s := &Store{path: path}
	s.load()
	return s
}

// SetEmbedModel records the active embedding model name. New entries are
// stamped with it, and SearchScoped quarantines entries from other models
// (cross-model cosine similarity is meaningless).
func (s *Store) SetEmbedModel(model string) {
	s.mu.Lock()
	s.embedModel = model
	s.mu.Unlock()
}

// Add appends a new conversation entry and returns its ID.
func (s *Store) Add(sessionID, role, content string, embedding []float32, turnIndex int) (int, error) {
	return s.AddScoped("", sessionID, role, content, embedding, turnIndex)
}

// maxEntriesPerProject is the soft cap for ICE entries per workspace.
// AddScoped prunes the oldest entries when this limit is exceeded.
const maxEntriesPerProject = 5000

// AddScoped records an entry under a canonical workspace identity.
func (s *Store) AddScoped(projectID, sessionID, role, content string, embedding []float32, turnIndex int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.loadErr != nil {
		return 0, fmt.Errorf("refusing to mutate unreadable ICE store: %w", s.loadErr)
	}

	var id int
	err := s.mutateLocked(func() (bool, error) {
		s.nextID++
		entry := ConversationEntry{
			ID:         s.nextID,
			ProjectID:  projectID,
			SessionID:  sessionID,
			Role:       role,
			Content:    content,
			Embedding:  append([]float32(nil), embedding...),
			EmbedModel: s.embedModel,
			TurnIndex:  turnIndex,
			CreatedAt:  timeNow(),
		}
		s.entries = append(s.entries, entry)
		id = entry.ID
		return true, nil
	})
	if err != nil {
		return 0, err
	}
	// Best-effort prune to keep the store bounded per workspace.
	if projectID != "" && s.countScopedLocked(projectID) > maxEntriesPerProject {
		_, _ = s.pruneScopedLocked(projectID, maxEntriesPerProject)
	}
	return id, nil
}

// PruneScoped evicts the oldest entries for projectID until at most maxEntries
// remain. Entries without embeddings are evicted first (useless for vector
// search), then by CreatedAt ascending. Returns the number of entries removed.
func (s *Store) PruneScoped(projectID string, maxEntries int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return 0, fmt.Errorf("refusing to mutate unreadable ICE store: %w", s.loadErr)
	}
	return s.pruneScopedLocked(projectID, maxEntries)
}

func (s *Store) pruneScopedLocked(projectID string, maxEntries int) (int, error) {
	if projectID == "" || maxEntries < 0 {
		return 0, nil
	}
	var removed int
	err := s.mutateLocked(func() (bool, error) {
		var scoped []int
		for i := range s.entries {
			if s.entries[i].ProjectID == projectID {
				scoped = append(scoped, i)
			}
		}
		if len(scoped) <= maxEntries {
			return false, nil
		}
		sort.Slice(scoped, func(a, b int) bool {
			ea, eb := s.entries[scoped[a]], s.entries[scoped[b]]
			aHasEmb := len(ea.Embedding) > 0
			bHasEmb := len(eb.Embedding) > 0
			if aHasEmb != bHasEmb {
				return !aHasEmb
			}
			return ea.CreatedAt.Before(eb.CreatedAt)
		})
		toRemove := len(scoped) - maxEntries
		evict := make(map[int]bool, toRemove)
		for i := 0; i < toRemove; i++ {
			evict[scoped[i]] = true
		}
		kept := make([]ConversationEntry, 0, len(s.entries)-toRemove)
		for i := range s.entries {
			if !evict[i] {
				kept = append(kept, s.entries[i])
			}
		}
		removed = len(s.entries) - len(kept)
		s.entries = kept
		return removed > 0, nil
	})
	return removed, err
}

func (s *Store) countScopedLocked(projectID string) int {
	count := 0
	for i := range s.entries {
		if s.entries[i].ProjectID == projectID {
			count++
		}
	}
	return count
}

// Search returns the top-K entries most similar to queryEmbedding.
// Entries from excludeSession are skipped. Results are sorted by score descending.
func (s *Store) Search(queryEmbedding []float32, excludeSession string, topK int) []ScoredEntry {
	return s.SearchScoped(queryEmbedding, "", excludeSession, topK)
}

// SearchScoped returns candidates only from the same canonical workspace.
func (s *Store) SearchScoped(queryEmbedding []float32, projectID, excludeSession string, topK int) []ScoredEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	if topK <= 0 || len(queryEmbedding) == 0 || !s.refreshLocked() || len(s.entries) == 0 {
		return nil
	}

	var scored []ScoredEntry
	for _, e := range s.entries {
		if projectID != "" && e.ProjectID != projectID {
			continue
		}
		if e.SessionID == excludeSession {
			continue
		}
		if len(e.Embedding) == 0 {
			continue
		}
		// Quarantine entries from a different embedding model: cross-model
		// cosine similarity is meaningless. Legacy entries (EmbedModel == "")
		// are excluded when a model is configured.
		if s.embedModel != "" && e.EmbedModel != s.embedModel {
			continue
		}
		sim := cosineSimilarity(queryEmbedding, e.Embedding)
		if sim >= minSimilarityThreshold {
			scored = append(scored, ScoredEntry{Entry: cloneConversationEntry(e), Score: sim})
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored
}

// Flush persists any pending changes to disk.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.loadErr != nil {
		return fmt.Errorf("refusing to overwrite unreadable ICE store: %w", s.loadErr)
	}
	if !s.dirty {
		return nil
	}
	pending, _, _ := s.snapshot()
	return s.mutateLocked(func() (bool, error) {
		changed := false
		for _, candidate := range pending {
			matched := false
			for _, durable := range s.entries {
				if candidate.ID == durable.ID && conversationEntriesEqual(candidate, durable) {
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			for _, durable := range s.entries {
				if candidate.ID == durable.ID {
					s.nextID++
					candidate.ID = s.nextID
					break
				}
			}
			if candidate.ID > s.nextID {
				s.nextID = candidate.ID
			}
			candidate.Embedding = append([]float32(nil), candidate.Embedding...)
			s.entries = append(s.entries, candidate)
			changed = true
		}
		return changed, nil
	})
}

func conversationEntriesEqual(left, right ConversationEntry) bool {
	if left.ID != right.ID || left.ProjectID != right.ProjectID || left.SessionID != right.SessionID || left.Role != right.Role || left.Content != right.Content || left.EmbedModel != right.EmbedModel || left.TurnIndex != right.TurnIndex || !left.CreatedAt.Equal(right.CreatedAt) || len(left.Embedding) != len(right.Embedding) {
		return false
	}
	for i := range left.Embedding {
		if left.Embedding[i] != right.Embedding[i] {
			return false
		}
	}
	return true
}

// Err reports a load-time corruption/parse error, if any.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

// Count returns the total number of stored entries.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.refreshLocked() {
		return 0
	}
	return len(s.entries)
}

// CountScoped returns only entries owned by projectID. Provenance-free legacy
// entries remain quarantined and are excluded.
func (s *Store) CountScoped(projectID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID == "" {
		return 0
	}
	if !s.refreshLocked() {
		return 0
	}
	count := 0
	for i := range s.entries {
		if s.entries[i].ProjectID == projectID {
			count++
		}
	}
	return count
}

func (s *Store) maxTurnIndexScoped(projectID, sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID == "" || sessionID == "" || !s.refreshLocked() {
		return 0
	}
	maxTurnIndex := 0
	for i := range s.entries {
		entry := s.entries[i]
		if entry.ProjectID == projectID && entry.SessionID == sessionID && entry.TurnIndex > maxTurnIndex {
			maxTurnIndex = entry.TurnIndex
		}
	}
	return maxTurnIndex
}

func (s *Store) snapshot() ([]ConversationEntry, int, bool) {
	entries := make([]ConversationEntry, len(s.entries))
	for i := range s.entries {
		entries[i] = cloneConversationEntry(s.entries[i])
	}
	return entries, s.nextID, s.dirty
}

func cloneConversationEntry(entry ConversationEntry) ConversationEntry {
	entry.Embedding = append([]float32(nil), entry.Embedding...)
	return entry
}

func (s *Store) restore(entries []ConversationEntry, nextID int, dirty bool) {
	s.entries = entries
	s.nextID = nextID
	s.dirty = dirty
}

// mutateLocked reloads the newest durable snapshot while holding a lock shared
// by every process, applies mutate, and atomically commits it. s.mu must be
// held. This makes IDs unique across multiple long-lived Store instances.
func (s *Store) mutateLocked(mutate func() (bool, error)) error {
	if s.loadErr != nil {
		return fmt.Errorf("refusing to overwrite unreadable ICE store: %w", s.loadErr)
	}
	dir := filepath.Dir(s.path)
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("validate ICE store path: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create ice store dir: %w", err)
	}
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("revalidate ICE store path: %w", err)
	}

	return safeio.WithExclusiveFileLock(s.path+".lock", iceStoreLockTimeout, func() error {
		if err := s.reloadFromDisk(); err != nil {
			return fmt.Errorf("reload ICE store before mutation: %w", err)
		}
		before, beforeNextID, beforeDirty := s.snapshot()
		changed, err := mutate()
		if err != nil {
			s.restore(before, beforeNextID, beforeDirty)
			return err
		}
		if !changed {
			return nil
		}
		s.dirty = true
		if err := s.persist(); err != nil {
			s.restore(before, beforeNextID, beforeDirty)
			return err
		}
		return nil
	})
}

func (s *Store) refreshLocked() bool {
	if s.loadErr != nil {
		return false
	}
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Dir(s.path)); errors.Is(err, os.ErrNotExist) {
		return s.reloadFromDisk() == nil
	} else if err != nil {
		return false
	}
	return safeio.WithExclusiveFileLock(s.path+".lock", iceStoreLockTimeout, s.reloadFromDisk) == nil
}

// load reads entries from the JSON file.
func (s *Store) load() {
	if err := s.reloadFromDisk(); err != nil {
		s.loadErr = err
	}
}

func (s *Store) reloadFromDisk() error {
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("validate ICE store path: %w", err)
	}
	data, err := iceStoreFileReader.ReadPrivateRegularFileNoFollow(s.path, maxICEStoreBytes, iceStoreReadTimeout)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.entries = nil
			s.nextID = 0
			s.dirty = false
			return nil
		}
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	var entries []ConversationEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}

	s.entries = entries
	s.nextID = 0
	s.dirty = false
	for _, e := range s.entries {
		if e.ID > s.nextID {
			s.nextID = e.ID
		}
	}
	return nil
}

// persist writes all entries to the JSON file.
func (s *Store) persist() error {
	if s.loadErr != nil {
		return fmt.Errorf("refusing to overwrite unreadable ICE store: %w", s.loadErr)
	}

	dir := filepath.Dir(s.path)
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("validate ICE publish path: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create ice store dir: %w", err)
	}

	data, err := json.Marshal(s.entries)
	if err != nil {
		return fmt.Errorf("marshal ice store: %w", err)
	}
	if int64(len(data)) > iceStoreWriteLimit {
		return fmt.Errorf("%w: serialized ICE store is %d bytes (limit %d)", safeio.ErrTooLarge, len(data), iceStoreWriteLimit)
	}

	// Atomic write: temp file + rename so a crash can't corrupt the live store.
	tmp, err := os.CreateTemp(dir, ".ice-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp ice store: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Preserve the primary persistence error. The successful close is
		// checked below; these are best-effort failure-path cleanup steps.
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op after a successful rename
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temp ice store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp ice store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp ice store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp ice store: %w", err)
	}
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("revalidate ICE publish path: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("commit ice store: %w", err)
	}
	// The committed inode was chmod'd through its temp-file descriptor before
	// rename; do not re-open the path and risk following a swapped symlink.
	// fsync the directory so the rename survives a hard crash (best-effort).
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}

	s.dirty = false
	return nil
}

// cosineSimilarity computes the cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return float32(dot / denom)
}
