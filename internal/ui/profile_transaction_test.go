package ui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/skill"
)

func transactionalProfileFixture(t *testing.T, modelManager *llm.ModelManager) (*Model, *agent.Agent, *skill.Manager) {
	t.Helper()
	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "installed.md"), []byte("---\nname: installed\n---\ninstalled content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	skillMgr := skill.NewManager(skillDir)
	if err := skillMgr.LoadAll(); err != nil {
		t.Fatal(err)
	}
	ag := agent.New(modelManager, nil, 4096)
	m := New(ag, command.NewRegistry(), skillMgr, nil, modelManager, nil, nil)
	m.model = "qwen3.5:2b"
	m.manualLoadedContext = "manual context"
	m.SetAgentProfileSource(&config.AgentsDir{Agents: map[string]config.AgentProfile{
		"restricted": {
			Name:         "restricted",
			Model:        "qwen3.5:2b",
			Skills:       []string{"installed"},
			MCPServers:   []string{"private-server"},
			SystemPrompt: "restricted prompt",
		},
		"missing-skill": {
			Name:         "missing-skill",
			Model:        "qwen3.5:4b",
			Skills:       []string{"not-installed"},
			MCPServers:   []string{"other-server"},
			SystemPrompt: "other prompt",
		},
	}}, "base context", "")
	if err := m.applyAgentProfile("restricted"); err != nil {
		t.Fatalf("activate restricted fixture: %v", err)
	}
	return m, ag, skillMgr
}

func TestRuntimeProfileMissingSkillPreservesPriorState(t *testing.T) {
	modelManager := llm.NewModelManager("http://localhost:11434", 4096)
	if err := modelManager.SetCurrentModel("qwen3.5:2b"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(modelManager.Close)
	m, ag, skillMgr := transactionalProfileFixture(t, modelManager)

	beforeProfile := m.agentProfile
	beforeProfileSkills := append([]string(nil), m.profileSkills...)
	beforeContext := ag.LoadedContext()
	beforeSkillContent := ag.SkillContent()
	beforeActiveSkills := skillMgr.ActiveContent()
	beforeScope, beforeRestricted := ag.MCPServerScope()
	beforeModel := m.model
	beforeManagerModel := modelManager.CurrentModel()
	beforePinned := m.modelPinned

	if err := m.applyAgentProfile("missing-skill"); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("missing-skill switch error = %v", err)
	}
	afterScope, afterRestricted := ag.MCPServerScope()
	if m.agentProfile != beforeProfile || !reflect.DeepEqual(m.profileSkills, beforeProfileSkills) {
		t.Fatalf("profile changed from %q/%v to %q/%v", beforeProfile, beforeProfileSkills, m.agentProfile, m.profileSkills)
	}
	if ag.LoadedContext() != beforeContext || ag.SkillContent() != beforeSkillContent || skillMgr.ActiveContent() != beforeActiveSkills {
		t.Fatalf("profile content changed: context=%q skills=%q active=%q", ag.LoadedContext(), ag.SkillContent(), skillMgr.ActiveContent())
	}
	if beforeRestricted != afterRestricted || !reflect.DeepEqual(beforeScope, afterScope) {
		t.Fatalf("MCP scope changed from (%v,%v) to (%v,%v)", beforeScope, beforeRestricted, afterScope, afterRestricted)
	}
	if m.model != beforeModel || modelManager.CurrentModel() != beforeManagerModel || m.modelPinned != beforePinned {
		t.Fatalf("model state changed: ui=%q manager=%q pinned=%v", m.model, modelManager.CurrentModel(), m.modelPinned)
	}
}

func TestRestoreUnavailableModelPreservesRestrictedProfileAndTranscript(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"models":[{"name":"qwen3.5:2b","model":"qwen3.5:2b","size":123}]}`)
	}))
	defer server.Close()

	modelManager := llm.NewModelManager(server.URL, 4096)
	modelManager.ConfigureLocalInventory(true, []llm.LocalModel{{Name: "qwen3.5:2b", Size: 123}}, true)
	if err := modelManager.SetCurrentModel("qwen3.5:2b"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(modelManager.Close)
	m, ag, skillMgr := transactionalProfileFixture(t, modelManager)
	m.mode = ModeAsk
	m.entries = []ChatEntry{{Kind: "user", Content: "keep this transcript"}}
	ag.ReplaceMessages([]llm.Message{{Role: "user", Content: "keep this transcript"}})

	beforeProfile := m.agentProfile
	beforeProfileSkills := append([]string(nil), m.profileSkills...)
	beforeContext := ag.LoadedContext()
	beforeSkillContent := ag.SkillContent()
	beforeActiveSkills := skillMgr.ActiveContent()
	beforeScope, beforeRestricted := ag.MCPServerScope()
	beforeModel := m.model
	beforeManagerModel := modelManager.CurrentModel()
	beforePinned := m.modelPinned
	beforeMode := m.mode
	beforeEntries := append([]ChatEntry(nil), m.entries...)
	beforeMessages := ag.Messages()

	err := m.restoreSessionState(persistedSessionState{
		Version:      1,
		Mode:         ModeBuild,
		Model:        "qwen3.5:4b",
		ModelPinned:  false,
		AgentProfile: "",
		Entries:      []persistedChatEntry{{Kind: "assistant", Content: "must not replace"}},
		Messages:     []llm.Message{{Role: "assistant", Content: "must not replace"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not installed with local Ollama weights") {
		t.Fatalf("unavailable saved model error = %v", err)
	}
	afterScope, afterRestricted := ag.MCPServerScope()
	if m.agentProfile != beforeProfile || !reflect.DeepEqual(m.profileSkills, beforeProfileSkills) {
		t.Fatalf("profile changed from %q/%v to %q/%v", beforeProfile, beforeProfileSkills, m.agentProfile, m.profileSkills)
	}
	if ag.LoadedContext() != beforeContext || ag.SkillContent() != beforeSkillContent || skillMgr.ActiveContent() != beforeActiveSkills {
		t.Fatalf("profile content changed: context=%q skills=%q active=%q", ag.LoadedContext(), ag.SkillContent(), skillMgr.ActiveContent())
	}
	if beforeRestricted != afterRestricted || !reflect.DeepEqual(beforeScope, afterScope) {
		t.Fatalf("MCP scope changed from (%v,%v) to (%v,%v)", beforeScope, beforeRestricted, afterScope, afterRestricted)
	}
	if m.model != beforeModel || modelManager.CurrentModel() != beforeManagerModel || m.modelPinned != beforePinned || m.mode != beforeMode {
		t.Fatalf("model/mode changed: ui=%q manager=%q pinned=%v mode=%v", m.model, modelManager.CurrentModel(), m.modelPinned, m.mode)
	}
	if !reflect.DeepEqual(m.entries, beforeEntries) || !reflect.DeepEqual(ag.Messages(), beforeMessages) {
		t.Fatalf("transcript changed: entries=%#v messages=%#v", m.entries, ag.Messages())
	}
}
