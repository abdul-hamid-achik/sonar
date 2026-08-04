package agent

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/skill"
)

const (
	maxModelSkillCatalogEntries   = 64
	maxModelSkillNameBytes        = 128
	maxModelSkillDescriptionBytes = 512
	maxModelSkillCatalogBytes     = 32 * 1024
	maxLoadedSkillBodyBytes       = skill.MaxSkillBodyBytes
)

// SkillLoader is the progressive-disclosure boundary used by Agent. Catalog
// exposes metadata only for skills not already active; Load returns an
// already-discovered body by exact name.
type SkillLoader interface {
	Catalog() []skill.CatalogEntry
	Load(name string) (body string, ok bool)
}

// SetSkillLoader configures model-selected, on-demand skill loading. It does
// not change manual or profile activation and may be cleared with nil.
func (a *Agent) SetSkillLoader(loader SkillLoader) {
	a.mu.Lock()
	a.skillLoader = loader
	a.mu.Unlock()
}

func (a *Agent) skillLoaderSnapshot() SkillLoader {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.skillLoader
}

func (a *Agent) hasSkillLoader() bool {
	return a.skillLoaderSnapshot() != nil
}

func (a *Agent) skillCatalogPrompt() string {
	loader := a.skillLoaderSnapshot()
	if loader == nil {
		return ""
	}
	entries := loader.Catalog()
	if len(entries) > maxModelSkillCatalogEntries*4 {
		entries = entries[:maxModelSkillCatalogEntries*4]
	}
	entries = append([]skill.CatalogEntry(nil), entries...)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name == entries[j].Name {
			return entries[i].Description < entries[j].Description
		}
		return entries[i].Name < entries[j].Name
	})

	var catalog strings.Builder
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if len(seen) >= maxModelSkillCatalogEntries {
			break
		}
		name := entry.Name
		if !validModelSkillName(name) {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}

		description := singleLineSkillDescription(entry.Description)
		line := fmt.Sprintf("- %s: %s\n", strconv.Quote(name), strconv.Quote(description))
		if catalog.Len()+len(line) > maxModelSkillCatalogBytes {
			break
		}
		catalog.WriteString(line)
	}
	if catalog.Len() == 0 {
		return ""
	}

	return "\n## Available Skills (load on demand)\n" +
		"This catalog contains only skill names and descriptions; listing a skill does not activate or include its body. " +
		"When a skill's metadata clearly matches the user's task, call `load_skill` with its exact name before acting. " +
		"A skill already supplied under Active Skills need not be loaded again. Model-selected `load_skill` is the automatic loading boundary; " +
		"do not infer activation from keywords, and do not change manual or profile activation.\n" +
		catalog.String()
}

func validModelSkillName(name string) bool {
	if name == "" || name != strings.TrimSpace(name) || !utf8.ValidString(name) || len(name) > maxModelSkillNameBytes {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func singleLineSkillDescription(description string) string {
	description = truncateUTF8Bytes(description, maxModelSkillDescriptionBytes)
	description = strings.Join(strings.Fields(description), " ")
	if description == "" {
		return "No description provided."
	}
	return description
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (a *Agent) handleLoadSkill(args map[string]any) (string, bool) {
	name, ok := args["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return "error: name is required", true
	}
	if len(args) != 1 || !validModelSkillName(name) {
		return "error: name must be one exact catalog name", true
	}
	loader := a.skillLoaderSnapshot()
	if loader == nil {
		return "error: skill loading is unavailable", true
	}
	body, found := loader.Load(name)
	if !found {
		return "error: skill not found", true
	}
	if len(body) > maxLoadedSkillBodyBytes {
		return "error: skill body exceeds the loading limit", true
	}
	if !utf8.ValidString(body) {
		return "error: skill body is not valid UTF-8", true
	}
	return body, false
}
