package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/command"
)

func TestCompleter_Complete(t *testing.T) {
	reg := command.NewRegistry()
	command.RegisterBuiltins(reg)
	c := NewCompleter(reg, []string{"model-a"}, []string{"skill-a", "skill-b"}, []string{"agent-x"}, nil)

	t.Run("slash_dispatches_to_commands", func(t *testing.T) {
		results := c.Complete("/h")
		if len(results) == 0 {
			t.Error("expected command completions for /h")
		}
		for _, r := range results {
			if r.Category != "command" {
				t.Errorf("expected category 'command', got %q", r.Category)
			}
		}
	})

	t.Run("at_dispatches_to_agents", func(t *testing.T) {
		results := c.Complete("@agent")
		found := false
		for _, r := range results {
			if r.Category == "agent" {
				found = true
			}
		}
		if !found {
			t.Error("expected agent completions for @agent")
		}
	})

	t.Run("hash_dispatches_to_skills", func(t *testing.T) {
		results := c.Complete("#skill")
		if len(results) == 0 {
			t.Error("expected skill completions for #skill")
		}
		for _, r := range results {
			if r.Category != "skill" {
				t.Errorf("expected category 'skill', got %q", r.Category)
			}
		}
	})

	t.Run("plain_returns_nothing", func(t *testing.T) {
		results := c.Complete("hello")
		if len(results) != 0 {
			t.Errorf("expected no completions for plain text, got %d", len(results))
		}
	})
}

func TestCompleterGoalActionsUseRegistryMetadata(t *testing.T) {
	registry := command.NewRegistry()
	command.RegisterBuiltins(registry)
	completer := NewCompleter(registry, nil, nil, nil, nil)

	all := completer.Complete("/goal ")
	if len(all) != 6 {
		t.Fatalf("goal action completions = %d, want 6: %#v", len(all), all)
	}
	if all[0].Label != "/goal new" || all[len(all)-1].Label != "/goal drop" {
		t.Fatalf("goal actions lost registry order: %#v", all)
	}
	resume := completer.Complete("/goal re")
	if len(resume) != 1 || resume[0].Label != "/goal resume" || !strings.Contains(strings.ToLower(resume[0].Description), "resume") {
		t.Fatalf("resume completion = %#v", resume)
	}
	alias := completer.Complete("/g stat")
	if len(alias) != 1 || alias[0].Label != "/goal show" {
		t.Fatalf("goal alias completion = %#v", alias)
	}
}

func TestSearchFilesIsBoundedToWorkspaceWithoutMCP(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"src/alpha_handler.go", "nested/alpha_test.go", "node_modules/alpha.js"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := &Completer{workDir: root}
	results := c.SearchFiles(context.Background(), "alpha")
	if len(results) != 2 {
		t.Fatalf("SearchFiles returned %d results, want 2: %#v", len(results), results)
	}
	for _, result := range results {
		if strings.Contains(result.Insert, "node_modules") {
			t.Fatalf("heavy directory escaped completion filter: %#v", result)
		}
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := c.SearchFiles(cancelled, "alpha"); len(got) != 0 {
		t.Fatalf("cancelled search returned results: %#v", got)
	}
}

func TestSearchResultCompletionAcceptsOnlyWorkspaceFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Completer{workDir: root}
	completion, ok := c.searchResultCompletion("src/main.go")
	if !ok || completion.Insert != "@src/main.go " {
		t.Fatalf("valid completion = %#v, %v", completion, ok)
	}
	for _, candidate := range []string{"Here are the results:", "```go", "../outside.go", "missing.go"} {
		if completion, ok := c.searchResultCompletion(candidate); ok {
			t.Fatalf("fake completion accepted for %q: %#v", candidate, completion)
		}
	}
}

func TestCompleteCommand(t *testing.T) {
	reg := command.NewRegistry()
	command.RegisterBuiltins(reg)
	c := NewCompleter(reg, nil, nil, nil, nil)

	t.Run("prefix_matching", func(t *testing.T) {
		results := c.Complete("/hel")
		found := false
		for _, r := range results {
			if r.Insert == "/help " {
				found = true
			}
		}
		if !found {
			t.Error("expected /help completion for prefix /hel")
		}
	})

	t.Run("alias_matching", func(t *testing.T) {
		results := c.Complete("/h")
		if len(results) != 1 || results[0].Label != "/help" || results[0].Insert != "/help " {
			t.Fatalf("alias should resolve to one canonical /help row: %#v", results)
		}
		if results[0].Description == "" {
			t.Fatal("canonical command row lost its useful description")
		}
	})

	t.Run("canonical_label_with_description", func(t *testing.T) {
		results := c.Complete("/model")
		for _, r := range results {
			if r.Insert == "/model " {
				if r.Label != "/model" || !strings.Contains(r.Description, "models") {
					t.Fatalf("model completion is not canonical and descriptive: %#v", r)
				}
			}
		}
	})

	t.Run("no_matches", func(t *testing.T) {
		results := c.Complete("/zzzzz")
		if len(results) != 0 {
			t.Errorf("expected no completions for /zzzzz, got %d", len(results))
		}
	})
}

func TestCompleteSkill(t *testing.T) {
	reg := command.NewRegistry()
	c := NewCompleter(reg, nil, []string{"coding", "writing", "debugging"}, nil, nil)

	t.Run("prefix_matching", func(t *testing.T) {
		results := c.Complete("#cod")
		if len(results) != 1 {
			t.Fatalf("expected 1 match for #cod, got %d", len(results))
		}
		if results[0].Label != "#coding" {
			t.Errorf("expected '#coding', got %q", results[0].Label)
		}
		if results[0].Category != "skill" {
			t.Errorf("expected category 'skill', got %q", results[0].Category)
		}
	})

	t.Run("all_match_empty_prefix", func(t *testing.T) {
		results := c.Complete("#")
		if len(results) != 3 {
			t.Errorf("expected 3 matches for #, got %d", len(results))
		}
	})

	t.Run("no_matches", func(t *testing.T) {
		results := c.Complete("#zzz")
		if len(results) != 0 {
			t.Errorf("expected no matches for #zzz, got %d", len(results))
		}
	})
}

func TestCompleterUpdateModels(t *testing.T) {
	reg := command.NewRegistry()
	c := NewCompleter(reg, []string{"old-model"}, nil, nil, nil)

	c.UpdateModels([]string{"new-model-a", "new-model-b"})

	if len(c.models) != 2 {
		t.Errorf("expected 2 models, got %d", len(c.models))
	}
	if c.models[0] != "new-model-a" {
		t.Errorf("expected 'new-model-a', got %q", c.models[0])
	}
}

func TestCompleterUpdateAgents(t *testing.T) {
	reg := command.NewRegistry()
	c := NewCompleter(reg, nil, nil, []string{"old-agent"}, nil)

	c.UpdateAgents([]string{"new-agent"})

	if len(c.agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(c.agents))
	}
	if c.agents[0] != "new-agent" {
		t.Errorf("expected 'new-agent', got %q", c.agents[0])
	}
}
