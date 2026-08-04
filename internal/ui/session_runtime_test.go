package ui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/skill"
)

func sessionRuntimeFixture(t *testing.T) (*Model, *agent.Agent) {
	t.Helper()
	skillDir := t.TempDir()
	for name, content := range map[string]string{
		"manual-a":  "manual skill A",
		"manual-b":  "manual skill B",
		"profile-a": "profile skill A",
		"profile-b": "profile skill B",
	} {
		body := "---\nname: " + name + "\n---\n" + content + "\n"
		if err := os.WriteFile(filepath.Join(skillDir, name+".md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	skillMgr := skill.NewManager(skillDir)
	if err := skillMgr.LoadAll(); err != nil {
		t.Fatal(err)
	}
	ag := agent.New(nil, nil, 0)
	m := New(ag, command.NewRegistry(), skillMgr, nil, nil, nil, nil)
	m.SetAgentProfileSource(&config.AgentsDir{Agents: map[string]config.AgentProfile{
		"alpha": {
			Name:         "alpha",
			Skills:       []string{"profile-a"},
			MCPServers:   []string{"scope-a"},
			SystemPrompt: "alpha profile prompt",
		},
		"beta": {
			Name:         "beta",
			Skills:       []string{"profile-b"},
			MCPServers:   []string{"scope-b"},
			SystemPrompt: "beta profile prompt",
		},
	}}, "base project context", "")
	return m, ag
}

func configureSessionRuntimeA(t *testing.T, m *Model) {
	t.Helper()
	if err := m.applyAgentProfile("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := m.setManualSkill("manual-a", true); err != nil {
		t.Fatal(err)
	}
	// Record an overlapping manual contribution. Removing the profile must not
	// deactivate this skill until its manual contribution is also removed.
	if err := m.setManualSkill("profile-a", true); err != nil {
		t.Fatal(err)
	}
	m.loadedFile = "context-a.md"
	m.manualLoadedContext = "manual context A"
	m.syncLoadedContext()
}

func configureSessionRuntimeB(t *testing.T, m *Model) {
	t.Helper()
	if err := m.applyAgentProfile("beta"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manual-a", "profile-a"} {
		if err := m.setManualSkill(name, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.setManualSkill("manual-b", true); err != nil {
		t.Fatal(err)
	}
	m.loadedFile = "context-b.md"
	m.manualLoadedContext = "manual context B"
	m.syncLoadedContext()
}

func assertSessionRuntimeA(t *testing.T, m *Model, ag *agent.Agent) {
	t.Helper()
	if m.loadedFile != "context-a.md" || m.manualLoadedContext != "manual context A" {
		t.Fatalf("manual context not restored: file=%q context=%q", m.loadedFile, m.manualLoadedContext)
	}
	if !reflect.DeepEqual(m.manualSkills, []string{"manual-a", "profile-a"}) {
		t.Fatalf("manual skills = %#v", m.manualSkills)
	}
	if !reflect.DeepEqual(m.profileSkills, []string{"profile-a"}) || m.agentProfile != "alpha" {
		t.Fatalf("profile runtime = %q/%#v", m.agentProfile, m.profileSkills)
	}
	context := ag.LoadedContext()
	for _, want := range []string{"base project context", "manual context A", "alpha profile prompt"} {
		if !strings.Contains(context, want) {
			t.Fatalf("loaded context %q missing %q", context, want)
		}
	}
	for _, leaked := range []string{"manual context B", "beta profile prompt"} {
		if strings.Contains(context, leaked) {
			t.Fatalf("loaded context leaked %q: %q", leaked, context)
		}
	}
	skills := ag.SkillContent()
	for _, want := range []string{"manual skill A", "profile skill A"} {
		if !strings.Contains(skills, want) {
			t.Fatalf("skill content %q missing %q", skills, want)
		}
	}
	for _, leaked := range []string{"manual skill B", "profile skill B"} {
		if strings.Contains(skills, leaked) {
			t.Fatalf("skill content leaked %q: %q", leaked, skills)
		}
	}
	scope, restricted := ag.MCPServerScope()
	if !restricted || !reflect.DeepEqual(scope, []string{"scope-a"}) {
		t.Fatalf("scope = (%v, %v), want restricted scope-a", scope, restricted)
	}
}

func TestSessionRuntimeSwitchAndCrossProcessRoundTrip(t *testing.T) {
	source, sourceAgent := sessionRuntimeFixture(t)
	configureSessionRuntimeA(t, source)
	rawA, err := encodeSessionState(source)
	if err != nil {
		t.Fatal(err)
	}
	stateA, err := decodeSessionState(rawA)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stateA.ManualSkills, []string{"manual-a", "profile-a"}) || stateA.LoadedFile != "context-a.md" || stateA.ManualLoadedContext != "manual context A" {
		t.Fatalf("encoded manual runtime is incomplete: %#v", stateA)
	}

	// Load A after B in one process. No B prompt contribution may survive.
	configureSessionRuntimeB(t, source)
	if err := source.restoreSessionState(stateA); err != nil {
		t.Fatal(err)
	}
	assertSessionRuntimeA(t, source, sourceAgent)

	// Persist through SQLite, then load into a freshly constructed model to
	// simulate a second process reading A from disk.
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close session store: %v", err)
		}
	})
	workspaceID, err := canonicalWorkspaceID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := store.CreateSession(ctx, db.CreateSessionParams{
		Title: "session A", Mode: "BUILD", WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(ctx, session.ID, rawA); err != nil {
		t.Fatal(err)
	}
	_, decodedA, _, err := loadPersistedSession(ctx, store, session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}

	target, targetAgent := sessionRuntimeFixture(t)
	configureSessionRuntimeB(t, target)
	if err := target.restoreSessionState(decodedA); err != nil {
		t.Fatal(err)
	}
	assertSessionRuntimeA(t, target, targetAgent)
}

func TestLegacySessionClearsUnownedManualRuntime(t *testing.T) {
	m, ag := sessionRuntimeFixture(t)
	configureSessionRuntimeB(t, m)
	legacy, err := decodeSessionState(`{"version":1,"messages":[],"entries":[],"mode":2,"agent_profile":"alpha"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.restoreSessionState(legacy); err != nil {
		t.Fatal(err)
	}
	if m.loadedFile != "" || m.manualLoadedContext != "" || len(m.manualSkills) != 0 {
		t.Fatalf("legacy restore retained manual runtime: file=%q context=%q skills=%v", m.loadedFile, m.manualLoadedContext, m.manualSkills)
	}
	if ag.LoadedContext() != "base project context\n\nalpha profile prompt" {
		t.Fatalf("legacy context = %q", ag.LoadedContext())
	}
	skills := ag.SkillContent()
	if !strings.Contains(skills, "profile skill A") || strings.Contains(skills, "manual skill B") || strings.Contains(skills, "profile skill B") {
		t.Fatalf("legacy skill ownership leaked: %q", skills)
	}
	scope, restricted := ag.MCPServerScope()
	if !restricted || !reflect.DeepEqual(scope, []string{"scope-a"}) {
		t.Fatalf("legacy scope = (%v, %v)", scope, restricted)
	}
}

func TestRestoreMissingManualSkillPreservesRuntimeAndAuthority(t *testing.T) {
	m, ag := sessionRuntimeFixture(t)
	configureSessionRuntimeB(t, m)
	beforeContext := ag.LoadedContext()
	beforeSkills := ag.SkillContent()
	beforeManual := append([]string(nil), m.manualSkills...)
	beforeProfile := append([]string(nil), m.profileSkills...)
	beforeScope, beforeRestricted := ag.MCPServerScope()

	err := m.restoreSessionState(persistedSessionState{
		Version:             1,
		Mode:                ModeBuild,
		AgentProfile:        "alpha",
		LoadedFile:          "must-not-commit.md",
		ManualLoadedContext: "must not commit",
		ManualSkills:        []string{"missing-skill"},
	})
	if err == nil || !strings.Contains(err.Error(), "missing-skill") {
		t.Fatalf("missing skill restore error = %v", err)
	}
	afterScope, afterRestricted := ag.MCPServerScope()
	if beforeRestricted != afterRestricted || !reflect.DeepEqual(beforeScope, afterScope) {
		t.Fatalf("authority changed from (%v,%v) to (%v,%v)", beforeScope, beforeRestricted, afterScope, afterRestricted)
	}
	if ag.LoadedContext() != beforeContext || ag.SkillContent() != beforeSkills || !reflect.DeepEqual(m.manualSkills, beforeManual) || !reflect.DeepEqual(m.profileSkills, beforeProfile) {
		t.Fatalf("runtime changed on failed restore: context=%q skills=%q manual=%v profile=%v", ag.LoadedContext(), ag.SkillContent(), m.manualSkills, m.profileSkills)
	}
	if m.loadedFile != "context-b.md" || m.manualLoadedContext != "manual context B" || m.agentProfile != "beta" {
		t.Fatalf("session identity changed on failed restore: file=%q context=%q profile=%q", m.loadedFile, m.manualLoadedContext, m.agentProfile)
	}
}
