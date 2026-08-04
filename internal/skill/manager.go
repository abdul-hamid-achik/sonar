package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const (
	maxSkillFileBytes        int64 = MaxSkillBodyBytes
	maxSkillCatalogEntries         = 64
	maxSkillNameBytes              = 128
	maxSkillDescriptionBytes       = 512
	maxSkillCatalogBytes           = 32 * 1024
)

var skillFileReader = safeio.NewReader()

// Manager handles skill discovery, loading, and activation.
type Manager struct {
	mu     sync.RWMutex
	skills []*Skill
	dirs   []string
}

// NewManager creates a skill manager for an explicit search directory. An
// empty directory disables discovery; the application owns selection of the
// shared agents root so auto-load and configured overrides remain authoritative.
func NewManager(dir string) *Manager {
	dirs := []string{}
	if dir != "" {
		dirs = append(dirs, dir)
	}
	return &Manager{dirs: dirs}
}

// AddSearchPath adds a directory to search for skills.
func (m *Manager) AddSearchPath(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.dirs {
		if d == dir {
			return
		}
	}
	m.dirs = append(m.dirs, dir)
}

// Names returns all skill names.
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.skills))
	for _, s := range m.skills {
		names = append(names, s.Name)
	}
	return names
}

// LoadAll discovers and loads all .md skill files from the skills directories.
func (m *Manager) LoadAll() error {
	m.mu.RLock()
	dirs := append([]string(nil), m.dirs...)
	m.mu.RUnlock()

	discovered := make([]*Skill, 0)
	byName := make(map[string]string)
	for _, dir := range dirs {
		sk, err := loadSkillsFromDir(dir)
		if err != nil {
			return err
		}
		for _, candidate := range sk {
			if previous, duplicate := byName[candidate.Name]; duplicate {
				return fmt.Errorf("duplicate skill name %q: %s and %s", candidate.Name, previous, candidate.Path)
			}
			byName[candidate.Name] = candidate.Path
			discovered = append(discovered, candidate)
		}
	}
	sort.Slice(discovered, func(i, j int) bool {
		if discovered[i].Name == discovered[j].Name {
			return discovered[i].Path < discovered[j].Path
		}
		return discovered[i].Name < discovered[j].Name
	})

	m.mu.Lock()
	active := make(map[string]bool, len(m.skills))
	for _, existing := range m.skills {
		active[existing.Name] = existing.Active
	}
	for _, candidate := range discovered {
		candidate.Active = active[candidate.Name]
	}
	m.skills = discovered
	m.mu.Unlock()
	return nil
}

func loadSkillsFromDir(dir string) ([]*Skill, error) {
	if dir == "" {
		return nil, nil
	}

	dirInfo, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect skills dir: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("inspect skills dir: %w: %s", safeio.ErrSymlink, dir)
	}
	if !dirInfo.IsDir() {
		return nil, fmt.Errorf("skills path is not a directory: %s", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read skills dir: %w", err)
	}

	loaded := make([]*Skill, 0)
	for _, entry := range entries {
		// Dotfiles and hidden package-manager links are not Agent Skills. Skip
		// them before metadata resolution so an unrelated hidden symlink cannot
		// disable the entire canonical skills directory.
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect skill entry %s: %w", filepath.Join(dir, entry.Name()), err)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("inspect skill entry: %w: %s", safeio.ErrSymlink, filepath.Join(dir, entry.Name()))
		}

		var candidatePaths []string
		var fallbackName string
		switch {
		case entryInfo.IsDir():
			candidatePaths = []string{
				filepath.Join(dir, entry.Name(), "SKILL.md"),
				filepath.Join(dir, entry.Name(), "skill.md"),
			}
			fallbackName = entry.Name()
		case strings.HasSuffix(entry.Name(), ".md"):
			candidatePaths = []string{filepath.Join(dir, entry.Name())}
			fallbackName = strings.TrimSuffix(entry.Name(), ".md")
		default:
			continue
		}

		var data []byte
		path := ""
		for _, candidatePath := range candidatePaths {
			data, err = skillFileReader.ReadRegularFileNoFollow(candidatePath, maxSkillFileBytes, safeio.StartupReadTimeout)
			if err == nil {
				path = candidatePath
				break
			}
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read skill %s: %w", candidatePath, err)
		}
		if path == "" {
			continue
		}

		if !utf8.Valid(data) {
			return nil, fmt.Errorf("parse skill %s: content is not valid UTF-8", path)
		}
		skill, err := parseFrontmatter(string(data))
		if err != nil {
			return nil, fmt.Errorf("parse skill %s: %w", path, err)
		}

		skill.Path = path
		if skill.nameDeclared && skill.Name == "" {
			return nil, fmt.Errorf("parse skill %s: invalid skill name: declared name is blank", path)
		}
		if skill.Name == "" {
			skill.Name = fallbackName
		}
		if err := validateSkillName(skill.Name); err != nil {
			return nil, fmt.Errorf("parse skill %s: invalid skill name: %w", path, err)
		}
		loaded = append(loaded, skill)
	}

	return loaded, nil
}

func validateSkillName(name string) error {
	switch {
	case name == "":
		return errors.New("name is blank")
	case name != strings.TrimSpace(name):
		return errors.New("name has leading or trailing whitespace")
	case !utf8.ValidString(name):
		return errors.New("name is not valid UTF-8")
	case len(name) > maxSkillNameBytes:
		return fmt.Errorf("name exceeds %d bytes", maxSkillNameBytes)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("name contains disallowed Unicode character %U", r)
		}
	}
	return nil
}

// All returns all discovered skills.
func (m *Manager) All() []*Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*Skill, len(m.skills))
	for index, item := range m.skills {
		copy := *item
		result[index] = &copy
	}
	return result
}

// Catalog returns a deterministic metadata-only snapshot of inactive skills
// for progressive disclosure. Bodies and paths never cross this boundary.
func (m *Manager) Catalog() []CatalogEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]CatalogEntry, 0, min(len(m.skills), maxSkillCatalogEntries))
	totalBytes := 0
	for _, item := range m.skills {
		if len(result) >= maxSkillCatalogEntries {
			break
		}
		if item.Active {
			continue
		}
		name := item.Name
		if validateSkillName(name) != nil {
			continue
		}
		description := truncateUTF8(item.Description, maxSkillDescriptionBytes)
		description = strings.Join(strings.Fields(description), " ")
		entryBytes := len(name) + len(description)
		if totalBytes+entryBytes > maxSkillCatalogBytes {
			break
		}
		totalBytes += entryBytes
		result = append(result, CatalogEntry{Name: name, Description: description})
	}
	return result
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// Load returns the already-discovered body for an exact skill name. It never
// rereads the filesystem and deliberately does not change activation state.
func (m *Manager) Load(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, item := range m.skills {
		if item.Name == name {
			return item.Content, true
		}
	}
	return "", false
}

// Has reports whether a skill name is available without changing activation.
func (m *Manager) Has(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, skill := range m.skills {
		if skill.Name == name {
			return true
		}
	}
	return false
}

// Activate enables a skill by name.
func (m *Manager) Activate(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.skills {
		if s.Name == name {
			s.Active = true
			return nil
		}
	}
	return fmt.Errorf("skill not found: %s", name)
}

// Deactivate disables a skill by name.
func (m *Manager) Deactivate(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.skills {
		if s.Name == name {
			s.Active = false
			return nil
		}
	}
	return fmt.Errorf("skill not found: %s", name)
}

// UpdateActive atomically validates and then applies a profile skill change.
// Skills outside remove/add are preserved, so manually activated skills are
// not disturbed when a profile changes.
func (m *Manager) UpdateActive(remove, add []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	available := make(map[string]struct{}, len(m.skills))
	for _, item := range m.skills {
		available[item.Name] = struct{}{}
	}
	for _, name := range append(append([]string(nil), remove...), add...) {
		if _, ok := available[name]; !ok {
			return fmt.Errorf("skill not found: %s", name)
		}
	}
	removeSet := make(map[string]struct{}, len(remove))
	addSet := make(map[string]struct{}, len(add))
	for _, name := range remove {
		removeSet[name] = struct{}{}
	}
	for _, name := range add {
		addSet[name] = struct{}{}
	}
	for _, s := range m.skills {
		if _, removeSkill := removeSet[s.Name]; removeSkill {
			s.Active = false
		}
		if _, addSkill := addSet[s.Name]; addSkill {
			s.Active = true
		}
	}
	return nil
}

// ActiveContent returns the combined markdown content of all active skills.
func (m *Manager) ActiveContent() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var parts []string
	for _, s := range m.skills {
		if s.Active && s.Content != "" {
			parts = append(parts, fmt.Sprintf("### %s\n%s", s.Name, s.Content))
		}
	}
	return strings.Join(parts, "\n\n")
}
