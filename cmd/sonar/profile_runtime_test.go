package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/safeio"
	"github.com/abdul-hamid-achik/sonar/internal/skill"
)

func TestBuildBaseLoadedContextPrefersAgentsMD(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("current instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("legacy instructions"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := buildBaseLoadedContextAt(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "current instructions") {
		t.Fatalf("context missing AGENTS.md: %q", got)
	}
	if strings.Contains(got, "legacy instructions") {
		t.Fatalf("context should prefer AGENTS.md over AGENT.md: %q", got)
	}
}

func TestNewRuntimeSkillManagerUsesOnlySelectedAgentsRoot(t *testing.T) {
	home := t.TempDir()
	selectedRoot := filepath.Join(home, "custom-agents")
	selectedSkill := filepath.Join(selectedRoot, "skills", "review")
	legacySkill := filepath.Join(home, ".config", "sonar", "skills", "review")
	for _, dir := range []string{selectedSkill, legacySkill} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(selectedSkill, "SKILL.md"), []byte("---\nname: review\n---\nselected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacySkill, "SKILL.md"), []byte("---\nname: review\n---\nlegacy"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := newRuntimeSkillManager(&config.AgentsDir{Path: selectedRoot}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.LoadAll(); err != nil {
		t.Fatal(err)
	}
	all := manager.All()
	if len(all) != 1 || all[0].Content != "selected" || all[0].Path != filepath.Join(selectedSkill, "SKILL.md") {
		t.Fatalf("selected root skills = %#v", all)
	}
}

func TestNewRuntimeSkillManagerRejectsMissingLoadedAgents(t *testing.T) {
	for _, agentsDir := range []*config.AgentsDir{nil, {}} {
		if _, err := newRuntimeSkillManager(agentsDir, true); err == nil {
			t.Fatalf("newRuntimeSkillManager(%#v, true) succeeded, want error", agentsDir)
		}
	}
}

func TestNewRuntimeSkillManagerWithAutoLoadDisabledDisablesDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	implicitSkill := filepath.Join(home, ".agents", "skills", "implicit")
	if err := os.MkdirAll(implicitSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(implicitSkill, "SKILL.md"), []byte("implicit"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := newRuntimeSkillManager(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.LoadAll(); err != nil {
		t.Fatal(err)
	}
	if all := manager.All(); len(all) != 0 {
		t.Fatalf("nil agents root discovered implicit skills: %#v", all)
	}
}

func TestBuildBaseLoadedContextFallsBackToLegacyAgentMD(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("legacy instructions"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := buildBaseLoadedContextAt(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "legacy instructions" {
		t.Fatalf("context = %q, want legacy instructions", got)
	}
}

func TestBuildBaseLoadedContextNeverLoadsOutsideSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	secret := "PRIVATE-KEY-MATERIAL-MUST-NOT-ENTER-PROMPT"
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	loaded, err := buildBaseLoadedContextAt(nil, dir)
	if !errors.Is(err, safeio.ErrSymlink) {
		t.Fatalf("outside AGENTS.md symlink error = %v", err)
	}
	if strings.Contains(loaded, secret) {
		t.Fatalf("outside secret entered model context: %q", loaded)
	}
}

func TestBuildHostConfigProjectionIsUsefulAndRedacted(t *testing.T) {
	cfg := &config.Config{
		SourcePath: "/xdg/sonar/config.yaml",
		Privacy:    config.PrivacyConfig{LocalOnly: true},
	}
	agentsDir := &config.AgentsDir{
		Path:               "/home/user/.agents",
		Agents:             map[string]config.AgentProfile{"reviewer": {Name: "reviewer"}},
		Skills:             []config.SkillDef{{Name: "go"}, {Name: "docs"}},
		GlobalInstructions: "private instructions must not be copied",
	}
	servers := []config.ServerConfig{
		{
			Name:    "mcphub",
			Command: "/opt/homebrew/bin/mcphub",
			Args:    []string{"mcp", "serve", "--agent", "SECRET_ROUTE_VALUE"},
			Env:     []string{"TOKEN=SECRET_ENV_VALUE"},
		},
		{
			Name:      "remote",
			Transport: "streamable-http",
			URL:       "https://SECRET_URL_VALUE.example/mcp?token=SECRET_QUERY",
		},
	}

	projection := buildHostConfigProjection(cfg, agentsDir, servers)
	for _, want := range []string{
		"/xdg/sonar/config.yaml",
		"/home/user/.agents",
		"profiles: 1",
		"skills: 2",
		`"mcphub" (stdio, gateway, scoped agent route)`,
		`"remote" (streamable-http)`,
		"Do not use filesystem tools",
	} {
		if !strings.Contains(projection, want) {
			t.Fatalf("projection missing %q: %s", want, projection)
		}
	}
	for _, secret := range []string{
		"SECRET_ROUTE_VALUE",
		"SECRET_ENV_VALUE",
		"SECRET_URL_VALUE",
		"SECRET_QUERY",
		"private instructions must not be copied",
	} {
		if strings.Contains(projection, secret) {
			t.Fatalf("projection leaked %q: %s", secret, projection)
		}
	}
}

func TestAppendLoadedContextKeepsProjectionSeparate(t *testing.T) {
	if got := appendLoadedContext("project instructions", "host projection"); got != "project instructions\n\nhost projection" {
		t.Fatalf("combined context = %q", got)
	}
}

func TestBuildHostConfigProjectionBoundsAndQuotesHostFields(t *testing.T) {
	servers := make([]config.ServerConfig, 0, maxHostProjectionServers+5)
	for i := 0; i < maxHostProjectionServers+5; i++ {
		servers = append(servers, config.ServerConfig{
			Name:    fmt.Sprintf("server-%02d-\n%s", i, strings.Repeat("x", maxHostProjectionNameRunes*2)),
			Command: "tool",
		})
	}
	longPath := "/" + strings.Repeat("private/", maxHostProjectionPathRunes)
	projection := buildHostConfigProjection(&config.Config{SourcePath: longPath}, &config.AgentsDir{Path: longPath}, servers)

	if strings.Contains(projection, longPath) {
		t.Fatal("projection included an unbounded host path")
	}
	if !strings.Contains(projection, "... (5 more configured endpoints)") {
		t.Fatalf("projection did not disclose bounded endpoints: %s", projection)
	}
	if strings.Contains(projection, "server-00-\n") {
		t.Fatal("server name injected a literal newline")
	}
	if !strings.Contains(projection, `server-00-\n`) {
		t.Fatalf("quoted server name missing: %s", projection)
	}
}

func TestApplyInitialAgentProfileValidationErrorsAreTransactional(t *testing.T) {
	tests := []struct {
		name      string
		agentsDir *config.AgentsDir
	}{
		{
			name:      "missing profile directory",
			agentsDir: nil,
		},
		{
			name:      "unknown profile",
			agentsDir: &config.AgentsDir{Agents: map[string]config.AgentProfile{}},
		},
		{
			name: "missing profile skill",
			agentsDir: &config.AgentsDir{Agents: map[string]config.AgentProfile{
				"requested": {
					Name:       "requested",
					Model:      "qwen3.5:4b",
					Skills:     []string{"not-installed"},
					MCPServers: []string{"new-server"},
				},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modelManager := llm.NewModelManager("http://localhost:11434", 4096)
			if err := modelManager.SetCurrentModel("qwen3.5:2b"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(modelManager.Close)

			ag := agent.New(modelManager, nil, 4096)
			ag.SetLoadedContext("existing context")
			ag.SetSkillContent("existing skill content")
			ag.SetMCPServerScope([]string{"existing-server"})
			skillMgr := skill.NewManager(t.TempDir())
			if err := skillMgr.LoadAll(); err != nil {
				t.Fatal(err)
			}

			beforeScope, beforeRestricted := ag.MCPServerScope()
			beforeModel := modelManager.CurrentModel()
			if err := applyInitialAgentProfile(ag, skillMgr, modelManager, tt.agentsDir, "replacement base", "requested"); err == nil {
				t.Fatal("invalid requested profile was accepted")
			}

			afterScope, afterRestricted := ag.MCPServerScope()
			if ag.LoadedContext() != "existing context" {
				t.Fatalf("loaded context changed to %q", ag.LoadedContext())
			}
			if ag.SkillContent() != "existing skill content" {
				t.Fatalf("skill content changed to %q", ag.SkillContent())
			}
			if modelManager.CurrentModel() != beforeModel {
				t.Fatalf("model changed from %q to %q", beforeModel, modelManager.CurrentModel())
			}
			if beforeRestricted != afterRestricted || !reflect.DeepEqual(beforeScope, afterScope) {
				t.Fatalf("MCP scope changed from (%v,%v) to (%v,%v)", beforeScope, beforeRestricted, afterScope, afterRestricted)
			}
		})
	}
}
