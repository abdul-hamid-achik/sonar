package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustLoadAll(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.LoadAll(); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
}

func mustActivate(t *testing.T, m *Manager, name string) {
	t.Helper()
	if err := m.Activate(name); err != nil {
		t.Fatalf("activate %s: %v", name, err)
	}
}

func TestManager_LoadAll(t *testing.T) {
	dir := t.TempDir()

	// Create valid skill files.
	mustWriteFile(t, filepath.Join(dir, "greeting.md"), "---\nname: greeting\ndescription: Say hello\n---\nHello!")
	mustWriteFile(t, filepath.Join(dir, "farewell.md"), "---\nname: farewell\ndescription: Say bye\n---\nGoodbye!")

	// Create a non-.md file (should be skipped).
	mustWriteFile(t, filepath.Join(dir, "notes.txt"), "not a skill")

	// Create a subdirectory (should be skipped).
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := NewManager(dir)
	if err := m.LoadAll(); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	skills := m.All()
	if len(skills) != 2 {
		t.Fatalf("loaded %d skills, want 2", len(skills))
	}

	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
	}
	if !names["greeting"] {
		t.Error("missing 'greeting' skill")
	}
	if !names["farewell"] {
		t.Error("missing 'farewell' skill")
	}
}

func TestManager_LoadAll_NoFrontmatter(t *testing.T) {
	dir := t.TempDir()

	// File without frontmatter uses filename as name.
	mustWriteFile(t, filepath.Join(dir, "plain.md"), "Just content, no frontmatter")

	m := NewManager(dir)
	if err := m.LoadAll(); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	skills := m.All()
	if len(skills) != 1 {
		t.Fatalf("loaded %d skills, want 1", len(skills))
	}
	if skills[0].Name != "plain" {
		t.Errorf("Name = %q, want 'plain'", skills[0].Name)
	}
}

func TestManagerLoadsFlatAndAgentSkillsDirectoryForms(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "zeta.md"), "---\nname: zeta\ndescription: Last skill\n---\nZeta body")
	directorySkill := filepath.Join(dir, "alpha")
	if err := os.MkdirAll(directorySkill, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(directorySkill, "SKILL.md"), "---\ndescription: First skill\n---\nAlpha body")

	m := NewManager(dir)
	mustLoadAll(t, m)

	if got := m.Names(); len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("deterministic names = %#v, want [alpha zeta]", got)
	}
	catalog := m.Catalog()
	if len(catalog) != 2 || catalog[0] != (CatalogEntry{Name: "alpha", Description: "First skill"}) || catalog[1] != (CatalogEntry{Name: "zeta", Description: "Last skill"}) {
		t.Fatalf("catalog = %#v", catalog)
	}
	if body, ok := m.Load("alpha"); !ok || body != "Alpha body" {
		t.Fatalf("Load(alpha) = %q, %v", body, ok)
	}
	if body, ok := m.Load("alpha "); ok || body != "" {
		t.Fatalf("non-exact Load = %q, %v", body, ok)
	}
	if body, ok := m.Load("missing"); ok || body != "" {
		t.Fatalf("missing Load = %q, %v", body, ok)
	}
	if content := m.ActiveContent(); content != "" {
		t.Fatalf("on-demand load changed activation: %q", content)
	}

	// Catalog snapshots are detached from Manager state.
	catalog[0].Description = "mutated"
	if current := m.Catalog(); current[0].Description != "First skill" {
		t.Fatalf("catalog mutation changed manager: %#v", current)
	}
}

func TestManagerUsesOnlyExplicitSharedAgentsSkillsRoot(t *testing.T) {
	home := t.TempDir()

	sharedDir := filepath.Join(home, ".agents", "skills", "review")
	legacyDir := filepath.Join(home, ".config", "sonar", "skills", "review")
	for _, dir := range []string{sharedDir, legacyDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create skill dir %s: %v", dir, err)
		}
	}
	mustWriteFile(t, filepath.Join(sharedDir, "SKILL.md"), "---\nname: review\ndescription: Canonical\n---\nshared body")
	mustWriteFile(t, filepath.Join(legacyDir, "SKILL.md"), "---\nname: review\ndescription: Legacy duplicate\n---\nlegacy body")

	m := NewManager(filepath.Join(home, ".agents", "skills"))
	mustLoadAll(t, m)

	all := m.All()
	if len(all) != 1 {
		t.Fatalf("default skills = %#v, want one shared skill", all)
	}
	if all[0].Path != filepath.Join(sharedDir, "SKILL.md") || all[0].Content != "shared body" {
		t.Fatalf("default skill = %#v, want canonical .agents skill", all[0])
	}
}

func TestManagerDirectorySkillAcceptsLegacyLowercaseFilename(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "lowercase")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(skillDir, "skill.md"), "---\nname: lowercase\ndescription: Compatible\n---\nlegacy lowercase body")

	m := NewManager(dir)
	mustLoadAll(t, m)
	all := m.All()
	if len(all) != 1 {
		t.Fatalf("lowercase directory skills = %d, want one", len(all))
	}
	if all[0].Name != "lowercase" || all[0].Content != "legacy lowercase body" {
		t.Fatalf("lowercase directory skill: name=%q content=%q path=%q", all[0].Name, all[0].Content, all[0].Path)
	}
}

func TestManagerEmptyRootDisablesImplicitDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, root := range []string{
		filepath.Join(home, ".agents", "skills", "shared"),
		filepath.Join(home, ".config", "sonar", "skills", "legacy"),
	} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(root, "SKILL.md"), "---\ndescription: must not load\n---\nbody")
	}

	m := NewManager("")
	mustLoadAll(t, m)
	if all := m.All(); len(all) != 0 {
		t.Fatalf("empty root discovered implicit skills: %#v", all)
	}
}

func TestManagerCatalogBoundsDoNotLimitFlatSkillDiscovery(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxSkillCatalogEntries+1; i++ {
		name := fmt.Sprintf("skill-%03d", i)
		mustWriteFile(t, filepath.Join(dir, name+".md"), fmt.Sprintf("---\nname: %s\ndescription: %s\n---\nbody-%03d", name, strings.Repeat("d", maxSkillDescriptionBytes+100), i))
	}
	m := NewManager(dir)
	mustLoadAll(t, m)
	if got := len(m.All()); got != maxSkillCatalogEntries+1 {
		t.Fatalf("discovered skills = %d", got)
	}
	catalog := m.Catalog()
	if len(catalog) == 0 || len(catalog) > maxSkillCatalogEntries {
		t.Fatalf("catalog entries = %d", len(catalog))
	}
	for _, entry := range catalog {
		if len(entry.Name) > maxSkillNameBytes || len(entry.Description) > maxSkillDescriptionBytes {
			t.Fatalf("unbounded catalog entry = %#v", entry)
		}
	}
	if body, ok := m.Load(fmt.Sprintf("skill-%03d", maxSkillCatalogEntries)); !ok || body != fmt.Sprintf("body-%03d", maxSkillCatalogEntries) {
		t.Fatalf("exact load outside catalog bound = %q, %v", body, ok)
	}
}

func TestManagerCatalogOmitsActiveSkills(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "active.md"), "---\nname: active\ndescription: Already active\n---\nActive body")
	mustWriteFile(t, filepath.Join(dir, "available.md"), "---\nname: available\ndescription: Load on demand\n---\nAvailable body")
	m := NewManager(dir)
	mustLoadAll(t, m)
	mustActivate(t, m, "active")

	catalog := m.Catalog()
	if len(catalog) != 1 || catalog[0].Name != "available" {
		t.Fatalf("catalog with active skill = %#v", catalog)
	}
	if active := m.ActiveContent(); !strings.Contains(active, "Active body") {
		t.Fatalf("active content = %q", active)
	}
	if err := m.Deactivate("active"); err != nil {
		t.Fatal(err)
	}
	catalog = m.Catalog()
	if len(catalog) != 2 || catalog[0].Name != "active" || catalog[1].Name != "available" {
		t.Fatalf("catalog after deactivation = %#v", catalog)
	}
}

func TestManager_LoadAll_NonexistentDir(t *testing.T) {
	m := NewManager("/nonexistent/path/that/does/not/exist")
	if err := m.LoadAll(); err != nil {
		t.Fatalf("LoadAll on nonexistent dir should not error, got: %v", err)
	}
	if len(m.All()) != 0 {
		t.Errorf("expected 0 skills from nonexistent dir, got %d", len(m.All()))
	}
}

func TestManagerLoadAllRejectsOversizedSkill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oversized.md")
	if err := os.WriteFile(path, make([]byte, maxSkillFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir)
	if err := m.LoadAll(); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("oversized skill error = %v", err)
	}
}

func TestManagerLoadAllRejectsSymlinkedSkill(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	m := NewManager(dir)
	if err := m.LoadAll(); !errors.Is(err, safeio.ErrSymlink) {
		t.Fatalf("symlinked skill error = %v", err)
	}
}

func TestManagerDirectorySkillsFailClosed(t *testing.T) {
	t.Run("hidden symlink is not a skill candidate", func(t *testing.T) {
		dir := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, ".package-manager")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		validDir := filepath.Join(dir, "valid")
		if err := os.MkdirAll(validDir, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(validDir, "SKILL.md"), "---\nname: valid\n---\nbody")

		m := NewManager(dir)
		mustLoadAll(t, m)
		if names := m.Names(); len(names) != 1 || names[0] != "valid" {
			t.Fatalf("skills with hidden package link = %#v", names)
		}
	})

	t.Run("symlinked SKILL.md", func(t *testing.T) {
		dir := t.TempDir()
		skillDir := filepath.Join(dir, "linked")
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.md")
		mustWriteFile(t, outside, "outside secret")
		if err := os.Symlink(outside, filepath.Join(skillDir, "SKILL.md")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if err := NewManager(dir).LoadAll(); !errors.Is(err, safeio.ErrSymlink) {
			t.Fatalf("symlinked directory skill error = %v", err)
		}
	})

	t.Run("symlinked root", func(t *testing.T) {
		realRoot := t.TempDir()
		linkParent := t.TempDir()
		link := filepath.Join(linkParent, "skills")
		if err := os.Symlink(realRoot, link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if err := NewManager(link).LoadAll(); !errors.Is(err, safeio.ErrSymlink) {
			t.Fatalf("symlinked root error = %v", err)
		}
	})

	t.Run("oversized SKILL.md", func(t *testing.T) {
		dir := t.TempDir()
		skillDir := filepath.Join(dir, "large")
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), make([]byte, maxSkillFileBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewManager(dir).LoadAll(); !errors.Is(err, safeio.ErrTooLarge) {
			t.Fatalf("oversized directory skill error = %v", err)
		}
	})
}

func TestManager_Activate_Deactivate(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "test.md"), "---\nname: test\n---\nTest content")

	m := NewManager(dir)
	mustLoadAll(t, m)

	t.Run("activate found", func(t *testing.T) {
		err := m.Activate("test")
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		skill := m.All()[0]
		if !skill.Active {
			t.Error("skill should be active after Activate")
		}
	})

	t.Run("activate not found", func(t *testing.T) {
		err := m.Activate("nonexistent")
		if err == nil {
			t.Error("expected error for nonexistent skill")
		}
	})

	t.Run("deactivate found", func(t *testing.T) {
		err := m.Deactivate("test")
		if err != nil {
			t.Fatalf("Deactivate: %v", err)
		}
		skill := m.All()[0]
		if skill.Active {
			t.Error("skill should be inactive after Deactivate")
		}
	})

	t.Run("deactivate not found", func(t *testing.T) {
		err := m.Deactivate("nonexistent")
		if err == nil {
			t.Error("expected error for nonexistent skill")
		}
	})
}

func TestManager_ActiveContent(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "alpha.md"), "---\nname: alpha\n---\nAlpha content")
	mustWriteFile(t, filepath.Join(dir, "beta.md"), "---\nname: beta\n---\nBeta content")

	m := NewManager(dir)
	mustLoadAll(t, m)

	t.Run("none active returns empty", func(t *testing.T) {
		content := m.ActiveContent()
		if content != "" {
			t.Errorf("expected empty content, got %q", content)
		}
	})

	t.Run("one active returns its content", func(t *testing.T) {
		mustActivate(t, m, "alpha")
		content := m.ActiveContent()
		if content == "" {
			t.Fatal("expected non-empty content")
		}
		if !contains(content, "Alpha content") {
			t.Errorf("content missing 'Alpha content': %q", content)
		}
		if contains(content, "Beta content") {
			t.Errorf("content should not contain inactive 'Beta content': %q", content)
		}
		if err := m.Deactivate("alpha"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("multiple active returns combined", func(t *testing.T) {
		mustActivate(t, m, "alpha")
		mustActivate(t, m, "beta")
		content := m.ActiveContent()
		if !contains(content, "Alpha content") || !contains(content, "Beta content") {
			t.Errorf("combined content missing expected parts: %q", content)
		}
	})
}

func TestManagerReloadIsIdempotentAndPreservesActivation(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "stable.md"), "---\nname: stable\ndescription: Stable skill\n---\nStable body")
	m := NewManager(dir)
	mustLoadAll(t, m)
	mustActivate(t, m, "stable")
	mustLoadAll(t, m)

	all := m.All()
	if len(all) != 1 || !all[0].Active || all[0].Content != "Stable body" {
		t.Fatalf("reloaded skills = %#v", all)
	}
	// Callers receive snapshots, not pointers into shared manager state.
	all[0].Active = false
	all[0].Content = "mutated"
	if current := m.All(); len(current) != 1 || !current[0].Active || current[0].Content != "Stable body" {
		t.Fatalf("external mutation changed manager state: %#v", current)
	}
}

func TestManagerRejectsAmbiguousAndMalformedSkills(t *testing.T) {
	t.Run("duplicate name", func(t *testing.T) {
		first, second := t.TempDir(), t.TempDir()
		mustWriteFile(t, filepath.Join(first, "first.md"), "---\nname: duplicate\n---\nfirst")
		mustWriteFile(t, filepath.Join(second, "second.md"), "---\nname: duplicate\n---\nsecond")
		m := NewManager(first)
		m.AddSearchPath(second)
		err := m.LoadAll()
		if err == nil || !strings.Contains(err.Error(), "duplicate skill name \"duplicate\"") ||
			!strings.Contains(err.Error(), "first.md") || !strings.Contains(err.Error(), "second.md") {
			t.Fatalf("duplicate skill error = %v", err)
		}
	})

	t.Run("duplicate flat and directory name", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteFile(t, filepath.Join(dir, "duplicate.md"), "---\nname: duplicate\n---\nflat")
		directorySkill := filepath.Join(dir, "nested")
		if err := os.MkdirAll(directorySkill, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(directorySkill, "SKILL.md"), "---\nname: duplicate\n---\ndirectory")
		err := NewManager(dir).LoadAll()
		if err == nil || !strings.Contains(err.Error(), "duplicate skill name \"duplicate\"") || !strings.Contains(err.Error(), "duplicate.md") || !strings.Contains(err.Error(), "SKILL.md") {
			t.Fatalf("flat/directory duplicate error = %v", err)
		}
		if second := NewManager(dir).LoadAll(); second == nil || second.Error() != err.Error() {
			t.Fatalf("duplicate rejection was not deterministic: first=%v second=%v", err, second)
		}
	})

	t.Run("invalid frontmatter", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteFile(t, filepath.Join(dir, "invalid.md"), "---\n: :\n---\nbody")
		err := NewManager(dir).LoadAll()
		if err == nil || !strings.Contains(err.Error(), "parse skill") {
			t.Fatalf("invalid skill error = %v", err)
		}
	})

	t.Run("unsupported frontmatter field", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteFile(t, filepath.Join(dir, "unknown.md"), "---\nname: valid\ndescripton: typo\n---\nbody")
		err := NewManager(dir).LoadAll()
		if err == nil || !strings.Contains(err.Error(), "field descripton not found") {
			t.Fatalf("unknown frontmatter field error = %v", err)
		}
	})

	t.Run("known Agent Skills extension fields", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteFile(t, filepath.Join(dir, "extended.md"), `---
name: extended
description: Extended metadata
license: MIT
compatibility: sonar
metadata:
  version: "1"
allowed-tools: Bash(go test:*)
triggers:
  - go
disable-model-invocation: false
user-invocable: true
argument-hint: "[path]"
---
body`)
		m := NewManager(dir)
		if err := m.LoadAll(); err != nil {
			t.Fatalf("known extension fields: %v", err)
		}
		if got := m.Names(); len(got) != 1 || got[0] != "extended" {
			t.Fatalf("extended skill names = %#v", got)
		}
	})
}

func TestManagerDisableModelInvocationStaysUserOnly(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "grill.md"), `---
name: grill-with-docs
description: A relentless interview
disable-model-invocation: true
---
Run a grilling session.`)
	mustWriteFile(t, filepath.Join(dir, "normal.md"), `---
name: normal
description: Ordinary skill
---
Ordinary body.`)
	m := NewManager(dir)
	if err := m.LoadAll(); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	catalog := m.Catalog()
	if len(catalog) != 1 || catalog[0].Name != "normal" {
		t.Fatalf("catalog = %#v, want only normal", catalog)
	}
	if _, ok := m.Load("grill-with-docs"); ok {
		t.Fatal("model Load must refuse disable-model-invocation skills")
	}
	if err := m.Activate("grill-with-docs"); err != nil {
		t.Fatalf("user Activate: %v", err)
	}
	if !strings.Contains(m.ActiveContent(), "grilling") {
		t.Fatalf("active content missing body: %q", m.ActiveContent())
	}
}

func TestManagerRejectsInvalidSkillNamesDuringDiscovery(t *testing.T) {
	tests := []struct {
		name     string
		declared string
	}{
		{name: "blank", declared: `""`},
		{name: "whitespace only", declared: `"   "`},
		{name: "leading whitespace", declared: `" alpha"`},
		{name: "trailing whitespace", declared: `"alpha "`},
		{name: "control", declared: `"alpha\u0007"`},
		{name: "unicode format", declared: `"alpha\u202e"`},
		{name: "oversized", declared: `"` + strings.Repeat("x", maxSkillNameBytes+1) + `"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			mustWriteFile(t, filepath.Join(dir, "invalid.md"), "---\nname: "+tt.declared+"\n---\nbody")
			err := NewManager(dir).LoadAll()
			if err == nil || !strings.Contains(err.Error(), "invalid skill name") || !strings.Contains(err.Error(), "invalid.md") {
				t.Fatalf("invalid name error = %v", err)
			}
		})
	}
}

func TestManagerConcurrentReadsAndActivation(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "alpha.md"), "---\nname: alpha\n---\nAlpha")
	mustWriteFile(t, filepath.Join(dir, "beta.md"), "---\nname: beta\n---\nBeta")
	m := NewManager(dir)
	mustLoadAll(t, m)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = m.Names()
				_ = m.All()
				_ = m.Catalog()
				_, _ = m.Load("alpha")
				_ = m.ActiveContent()
				_ = m.Has("alpha")
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if err := m.UpdateActive([]string{"beta"}, []string{"alpha"}); err != nil {
					t.Errorf("update active: %v", err)
				}
				if err := m.UpdateActive([]string{"alpha"}, []string{"beta"}); err != nil {
					t.Errorf("update active: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
