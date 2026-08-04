package memory

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const maxMemoryStoreBytes int64 = 64 << 20

var memoryStoreFileReader = safeio.NewReader()
var memoryStoreReadTimeout = 5 * time.Second
var memoryStoreLockTimeout = 5 * time.Second
var memoryStoreWriteLimit = maxMemoryStoreBytes

// Memory represents a single persisted memory entry.
type Memory struct {
	ID        int       `json:"id"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used"`
}

// Store manages persistent memories backed by a JSON file.
type Store struct {
	mu       sync.Mutex
	path     string
	memories []Memory
	nextID   int
	loadErr  error
}

// DefaultPathForWorkspace returns a private, project-scoped memory path.
func DefaultPathForWorkspace(workspace string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	if workspace == "" {
		return filepath.Join(home, ".config", "sonar", "memories.json")
	}
	canonical, err := filepath.Abs(workspace)
	if err == nil {
		if resolved, resolveErr := filepath.EvalSymlinks(canonical); resolveErr == nil {
			canonical = resolved
		}
	}
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(home, ".config", "sonar", "memory", fmt.Sprintf("%x.json", sum[:8]))
}

// NewStore creates a new Store. If path is empty, uses the default
// ~/.config/sonar/memories.json location.
func NewStore(path string) *Store {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		path = filepath.Join(home, ".config", "sonar", "memories.json")
	}

	s := &Store{path: path}
	s.load()
	return s
}

// Save persists a new memory with the given content and tags.
func (s *Store) Save(content string, tags []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return 0, fmt.Errorf("refusing to overwrite unreadable memory store: %w", s.loadErr)
	}

	var id int
	err := s.mutateLocked(func() bool {
		s.nextID++
		now := time.Now()
		mem := Memory{
			ID:        s.nextID,
			Content:   content,
			Tags:      append([]string(nil), tags...),
			CreatedAt: now,
			LastUsed:  now,
		}
		s.memories = append(s.memories, mem)
		id = mem.ID
		return true
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Recall searches memories by keyword/tag matching and returns up to maxResults.
//
// The error is not decoration. This result reaches the model as an answer, and
// returning an empty slice for a lock timeout, a persist failure, or an
// unreadable store told it "there is no such memory" — an authoritative
// absence built from a failure. Every mutating method in this file already
// reports that condition; the read paths swallowed it.
func (s *Store) Recall(query string, maxResults int) ([]Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		// Fail closed here too: the store deliberately refuses to answer from a
		// partially-read file, and that refusal must reach the caller.
		return nil, fmt.Errorf("memory store unavailable: %w", s.loadErr)
	}

	if maxResults <= 0 {
		maxResults = 5
	}

	queryLower := strings.ToLower(query)
	words := strings.Fields(queryLower)
	var out []Memory
	if err := s.mutateLocked(func() bool {
		type scored struct {
			mem   Memory
			score int
		}

		var results []scored
		for i := range s.memories {
			mem := s.memories[i]
			score := 0
			contentLower := strings.ToLower(mem.Content)

			for _, word := range words {
				if strings.Contains(contentLower, word) {
					score += 2
				}
			}
			for _, tag := range mem.Tags {
				tagLower := strings.ToLower(tag)
				for _, word := range words {
					if strings.Contains(tagLower, word) {
						score += 3
					}
				}
			}
			if score > 0 {
				results = append(results, scored{mem: mem, score: score})
			}
		}

		sort.Slice(results, func(i, j int) bool {
			if results[i].score != results[j].score {
				return results[i].score > results[j].score
			}
			return results[i].mem.LastUsed.After(results[j].mem.LastUsed)
		})
		if len(results) > maxResults {
			results = results[:maxResults]
		}

		now := time.Now()
		out = make([]Memory, len(results))
		for i, result := range results {
			out[i] = result.mem
			out[i].Tags = append([]string(nil), result.mem.Tags...)
			for j := range s.memories {
				if s.memories[j].ID == result.mem.ID {
					s.memories[j].LastUsed = now
					break
				}
			}
		}
		return len(results) > 0
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// Recent returns the N most recently used memories. Like Recall, it reports an
// unusable store rather than presenting it as an empty one.
func (s *Store) Recent(n int) ([]Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if n <= 0 {
		return nil, nil
	}
	if !s.refreshLocked() {
		if s.loadErr != nil {
			return nil, fmt.Errorf("memory store unavailable: %w", s.loadErr)
		}
		return nil, errors.New("memory store unavailable")
	}
	if len(s.memories) == 0 {
		return nil, nil
	}

	// Sort by LastUsed descending.
	sorted := make([]Memory, len(s.memories))
	for i := range s.memories {
		sorted[i] = cloneMemory(s.memories[i])
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].LastUsed.After(sorted[j].LastUsed)
	})

	if n > len(sorted) {
		n = len(sorted)
	}
	return sorted[:n], nil
}

// Count returns the total number of stored memories.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.refreshLocked() {
		return 0
	}
	return len(s.memories)
}

// Delete removes a memory by ID. Returns true if found and deleted.
func (s *Store) Delete(id int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return false, fmt.Errorf("memory store unavailable: %w", s.loadErr)
	}

	deleted := false
	err := s.mutateLocked(func() bool {
		for i, mem := range s.memories {
			if mem.ID == id {
				s.memories = append(s.memories[:i], s.memories[i+1:]...)
				deleted = true
				return true
			}
		}
		return false
	})
	return deleted, err
}

// DeleteByTag removes all memories containing a specific tag.
func (s *Store) DeleteByTag(tag string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return 0, fmt.Errorf("memory store unavailable: %w", s.loadErr)
	}

	tagLower := strings.ToLower(tag)
	deleted := 0
	err := s.mutateLocked(func() bool {
		remaining := make([]Memory, 0, len(s.memories))
		for _, mem := range s.memories {
			found := false
			for _, memoryTag := range mem.Tags {
				if strings.ToLower(memoryTag) == tagLower {
					found = true
					break
				}
			}
			if found {
				deleted++
			} else {
				remaining = append(remaining, mem)
			}
		}
		s.memories = remaining
		return deleted > 0
	})
	return deleted, err
}

// Update modifies an existing memory's content and/or tags.
// If content is empty, the existing content is preserved.
// If tags is nil, the existing tags are preserved.
func (s *Store) Update(id int, content string, tags []string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return false, fmt.Errorf("memory store unavailable: %w", s.loadErr)
	}

	updated := false
	err := s.mutateLocked(func() bool {
		for i, mem := range s.memories {
			if mem.ID == id {
				if content != "" {
					s.memories[i].Content = content
				}
				if tags != nil {
					s.memories[i].Tags = append([]string(nil), tags...)
				}
				s.memories[i].LastUsed = time.Now()
				updated = true
				return true
			}
		}
		return false
	})
	return updated, err
}

// Prune removes memories older than the specified duration.
func (s *Store) Prune(olderThan time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return 0, fmt.Errorf("memory store unavailable: %w", s.loadErr)
	}

	cutoff := time.Now().Add(-olderThan)
	deleted := 0
	err := s.mutateLocked(func() bool {
		remaining := make([]Memory, 0, len(s.memories))
		for _, mem := range s.memories {
			if mem.CreatedAt.Before(cutoff) {
				deleted++
			} else {
				remaining = append(remaining, mem)
			}
		}
		s.memories = remaining
		return deleted > 0
	})
	return deleted, err
}

// Get retrieves a single memory by ID.
func (s *Store) Get(id int) (Memory, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.refreshLocked() {
		return Memory{}, false
	}

	for _, mem := range s.memories {
		if mem.ID == id {
			return cloneMemory(mem), true
		}
	}
	return Memory{}, false
}

// Err reports a load-time permissions or corruption error.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

func (s *Store) snapshot() ([]Memory, int) {
	memories := make([]Memory, len(s.memories))
	for i := range s.memories {
		memories[i] = cloneMemory(s.memories[i])
	}
	return memories, s.nextID
}

func cloneMemory(memory Memory) Memory {
	memory.Tags = append([]string(nil), memory.Tags...)
	return memory
}

func (s *Store) restore(memories []Memory, nextID int) {
	s.memories = memories
	s.nextID = nextID
}

// mutateLocked reloads the latest durable snapshot while holding a lock shared
// by every process, applies mutate, and atomically persists the result. s.mu
// must already be held. Reload-before-write prevents two long-lived Store
// instances from assigning duplicate IDs or overwriting each other's changes.
func (s *Store) mutateLocked(mutate func() bool) error {
	if s.loadErr != nil {
		return fmt.Errorf("refusing to overwrite unreadable memory store: %w", s.loadErr)
	}
	dir := filepath.Dir(s.path)
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("validate memory store path: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("revalidate memory store path: %w", err)
	}

	return safeio.WithExclusiveFileLock(s.path+".lock", memoryStoreLockTimeout, func() error {
		if err := s.reloadFromDisk(); err != nil {
			return fmt.Errorf("reload memory store before mutation: %w", err)
		}
		before, beforeNextID := s.snapshot()
		if !mutate() {
			return nil
		}
		if err := s.persist(); err != nil {
			s.restore(before, beforeNextID)
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
	return safeio.WithExclusiveFileLock(s.path+".lock", memoryStoreLockTimeout, s.reloadFromDisk) == nil
}

// load reads memories from the JSON file.
func (s *Store) load() {
	if err := s.reloadFromDisk(); err != nil {
		s.loadErr = err
	}
}

func (s *Store) reloadFromDisk() error {
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("validate memory store path: %w", err)
	}
	data, err := memoryStoreFileReader.ReadPrivateRegularFileNoFollow(s.path, maxMemoryStoreBytes, memoryStoreReadTimeout)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.memories = nil
			s.nextID = 0
			return nil
		}
		return fmt.Errorf("read memory store: %w", err)
	}
	var memories []Memory
	if err := json.Unmarshal(data, &memories); err != nil {
		return fmt.Errorf("parse memory store: %w", err)
	}

	s.memories = memories
	s.nextID = 0
	for _, m := range s.memories {
		if m.ID > s.nextID {
			s.nextID = m.ID
		}
	}
	return nil
}

// persist writes all memories to the JSON file.
func (s *Store) persist() error {
	if s.loadErr != nil {
		return fmt.Errorf("refusing to overwrite unreadable memory store: %w", s.loadErr)
	}

	// Ensure directory exists.
	dir := filepath.Dir(s.path)
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("validate memory publish path: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}

	data, err := json.MarshalIndent(s.memories, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal memories: %w", err)
	}
	if int64(len(data)) > memoryStoreWriteLimit {
		return fmt.Errorf("%w: serialized memory store is %d bytes (limit %d)", safeio.ErrTooLarge, len(data), memoryStoreWriteLimit)
	}

	// Atomic write: a crash mid-write must never corrupt the live file.
	// Write to a temp file in the same dir, then rename over the target.
	tmp, err := os.CreateTemp(dir, ".memories-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp memories file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Preserve the primary persistence error. Close and removal here are
		// best-effort fallbacks; the successful close is checked below.
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op after a successful rename
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temp memories file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp memories: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp memories: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp memories: %w", err)
	}
	if err := safeio.ValidatePublishPath(s.path); err != nil {
		return fmt.Errorf("revalidate memory publish path: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("commit memories: %w", err)
	}
	// The temp file was chmod'd before rename, so the committed inode is
	// already private. No fallible operation follows the atomic commit point.
	syncDir(dir)

	return nil
}

// syncDir fsyncs a directory so a rename into it survives a hard crash.
// Best-effort: not all filesystems support directory fsync.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
