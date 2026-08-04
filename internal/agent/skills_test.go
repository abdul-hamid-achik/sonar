package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/skill"
)

type fakeSkillLoader struct {
	catalog []skill.CatalogEntry
	bodies  map[string]string
	calls   []string
}

func (f *fakeSkillLoader) Catalog() []skill.CatalogEntry {
	return append([]skill.CatalogEntry(nil), f.catalog...)
}

func (f *fakeSkillLoader) Load(name string) (string, bool) {
	f.calls = append(f.calls, name)
	body, ok := f.bodies[name]
	return body, ok
}

func TestSkillCatalogPromptContainsOnlyBoundedMetadata(t *testing.T) {
	loader := &fakeSkillLoader{
		catalog: []skill.CatalogEntry{
			{Name: "zeta", Description: "Last metadata"},
			{Name: "alpha", Description: "First metadata"},
		},
		bodies: map[string]string{
			"alpha": "PRIVATE BODY /Users/example/.agents/skills/alpha/SKILL.md",
			"zeta":  "ANOTHER PRIVATE BODY",
		},
	}
	ag := New(nil, nil, 0)
	ag.SetSkillLoader(loader)

	catalog := ag.skillCatalogPrompt()
	if !strings.Contains(catalog, "call `load_skill` with its exact name before acting") || !strings.Contains(catalog, "do not infer activation from keywords") {
		t.Fatalf("catalog guidance missing: %q", catalog)
	}
	alpha := strings.Index(catalog, `"alpha": "First metadata"`)
	zeta := strings.Index(catalog, `"zeta": "Last metadata"`)
	if alpha < 0 || zeta < 0 || alpha >= zeta {
		t.Fatalf("catalog order/content = %q", catalog)
	}

	prompt := buildSystemPrompt(context.Background(), systemPromptOptions{
		SkillCatalog: catalog,
		ModelName:    "test-model",
	})
	for _, forbidden := range []string{"PRIVATE BODY", "ANOTHER PRIVATE BODY", "/Users/example", "SKILL.md"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("model prompt exposed %q: %q", forbidden, prompt)
		}
	}
	if !strings.Contains(prompt, `"alpha": "First metadata"`) {
		t.Fatalf("model prompt omitted catalog: %q", prompt)
	}
}

func TestSkillCatalogPromptIsBounded(t *testing.T) {
	entries := make([]skill.CatalogEntry, 0, maxModelSkillCatalogEntries+10)
	for i := 0; i < maxModelSkillCatalogEntries+10; i++ {
		entries = append(entries, skill.CatalogEntry{
			Name:        fmt.Sprintf("skill-%03d", i),
			Description: strings.Repeat("d", maxModelSkillDescriptionBytes*2),
		})
	}
	ag := New(nil, nil, 0)
	ag.SetSkillLoader(&fakeSkillLoader{catalog: entries})
	prompt := ag.skillCatalogPrompt()
	if got := strings.Count(prompt, "\n- \""); got > maxModelSkillCatalogEntries {
		t.Fatalf("catalog entries = %d, limit %d", got, maxModelSkillCatalogEntries)
	}
	if len(prompt) > maxModelSkillCatalogBytes+1024 {
		t.Fatalf("catalog prompt length = %d", len(prompt))
	}
}

func TestSkillCatalogPromptRejectsUnicodeFormatName(t *testing.T) {
	ag := New(nil, nil, 0)
	ag.SetSkillLoader(&fakeSkillLoader{catalog: []skill.CatalogEntry{
		{Name: "safe", Description: "Visible"},
		{Name: "unsafe\u202e", Description: "Hidden"},
	}})

	prompt := ag.skillCatalogPrompt()
	if !strings.Contains(prompt, `"safe": "Visible"`) {
		t.Fatalf("catalog omitted safe skill: %q", prompt)
	}
	if strings.Contains(prompt, "unsafe") || strings.Contains(prompt, "Hidden") {
		t.Fatalf("catalog exposed Unicode-format skill name: %q", prompt)
	}
}

func TestLoadSkillToolExposureByConfigurationAndMode(t *testing.T) {
	withoutLoader := New(nil, nil, 0)
	if hasToolDef(withoutLoader.toolsBuiltinToolDefs(), "load_skill") {
		t.Fatal("load_skill exposed without a configured loader")
	}

	withLoader := New(nil, nil, 0)
	withLoader.SetSkillLoader(&fakeSkillLoader{})
	if !hasToolDef(withLoader.toolsBuiltinToolDefs(), "load_skill") {
		t.Fatal("load_skill absent with configured loader")
	}

	policies := []struct {
		name   string
		policy ToolPolicy
	}{
		{name: "ask", policy: AskToolPolicy()},
		{name: "plan", policy: PlanToolPolicy()},
		{name: "build", policy: BuildToolPolicy()},
	}
	for _, tt := range policies {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.policy.AllowsBuiltin("load_skill") {
				t.Fatal("policy blocks load_skill")
			}
			withoutLoader.SetModeContext("", tt.policy)
			withLoader.SetModeContext("", tt.policy)
			if got, want := withLoader.ToolCount(), withoutLoader.ToolCount()+1; got != want {
				t.Fatalf("configured ToolCount = %d, want %d", got, want)
			}
		})
	}
	withLoader.SetSkillLoader(nil)
	if hasToolDef(withLoader.toolsBuiltinToolDefs(), "load_skill") {
		t.Fatal("load_skill remained exposed after clearing the loader")
	}
}

func TestLoadSkillExecutionContractAndExactHandler(t *testing.T) {
	ag := New(nil, nil, 0)
	kind, effect := ag.executionKind("load_skill")
	if kind != executionpkg.KindBuiltin || effect != executionpkg.EffectReadOnly {
		t.Fatalf("execution kind = %s/%s", kind, effect)
	}
	if builtinToolRequiresApproval("load_skill") {
		t.Fatal("load_skill requires approval")
	}
	if err := ag.preflightToolCall(kind, llm.ToolCall{Name: "load_skill", Arguments: map[string]any{"name": "alpha"}}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("preflight without loader = %v", err)
	}

	loader := &fakeSkillLoader{
		catalog: []skill.CatalogEntry{{Name: "alpha", Description: "Alpha metadata"}},
		bodies:  map[string]string{"alpha": "Alpha body"},
	}
	ag.SetSkillLoader(loader)
	ag.SetSkillContent("Manually active body")
	for _, call := range []llm.ToolCall{
		{Name: "load_skill", Arguments: nil},
		{Name: "load_skill", Arguments: map[string]any{"name": 7}},
		{Name: "load_skill", Arguments: map[string]any{"name": "alpha "}},
		{Name: "load_skill", Arguments: map[string]any{"name": "alpha", "path": "/private/skill.md"}},
	} {
		if err := ag.preflightToolCall(kind, call); err == nil {
			t.Fatalf("preflight accepted %#v", call.Arguments)
		}
	}
	exact := llm.ToolCall{Name: "load_skill", Arguments: map[string]any{"name": "alpha"}}
	if err := ag.preflightToolCall(kind, exact); err != nil {
		t.Fatalf("exact preflight: %v", err)
	}
	result, isErr := ag.handleBuiltinToolWithCancellation(context.Background(), exact, false)
	if isErr || result != "Alpha body" {
		t.Fatalf("exact handler = %q, error=%v", result, isErr)
	}
	if len(loader.calls) != 1 || loader.calls[0] != "alpha" {
		t.Fatalf("loader calls = %#v", loader.calls)
	}
	if got := ag.SkillContent(); got != "Manually active body" {
		t.Fatalf("on-demand load changed active skill content: %q", got)
	}

	missing := llm.ToolCall{Name: "load_skill", Arguments: map[string]any{"name": "missing"}}
	result, isErr = ag.handleToolsTool(context.Background(), missing)
	if !isErr || result != "error: skill not found" {
		t.Fatalf("missing handler = %q, error=%v", result, isErr)
	}
	if got := loader.calls[len(loader.calls)-1]; got != "missing" {
		t.Fatalf("missing callback name = %q", got)
	}
}

func TestLoadSkillHandlerRejectsUnboundedOrInvalidCustomBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "invalid UTF-8", body: string([]byte{0xff}), want: "not valid UTF-8"},
		{name: "oversized", body: strings.Repeat("x", maxLoadedSkillBodyBytes+1), want: "exceeds the loading limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ag := New(nil, nil, 0)
			ag.SetSkillLoader(&fakeSkillLoader{bodies: map[string]string{"unsafe": tt.body}})
			result, isErr := ag.handleLoadSkill(map[string]any{"name": "unsafe"})
			if !isErr || !strings.Contains(result, tt.want) {
				t.Fatalf("handler = %q, error=%v", result, isErr)
			}
		})
	}
}

func hasToolDef(defs []llm.ToolDef, name string) bool {
	for _, def := range defs {
		if def.Name == name {
			return true
		}
	}
	return false
}
