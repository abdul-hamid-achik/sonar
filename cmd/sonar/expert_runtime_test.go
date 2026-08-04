package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/expertselector"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/resource"
)

type expertRuntimeRunner struct {
	mu      sync.Mutex
	options []llm.ChatOptions
	models  []string
}

func (*expertRuntimeRunner) CurrentModel() string                { return "current:2b" }
func (*expertRuntimeRunner) EffectiveContext(string) (int, bool) { return 8192, true }
func (*expertRuntimeRunner) PrepareExpertModels(_ context.Context, selected []string) (llm.ExpertModelSnapshot, error) {
	models := make([]llm.ExpertModelResource, 0, len(selected))
	for _, model := range selected {
		models = append(models, llm.ExpertModelResource{Name: model, Selected: true})
	}
	return llm.ExpertModelSnapshot{Models: models}, nil
}
func (*expertRuntimeRunner) ReleaseExpertModels(context.Context, llm.ExpertModelSnapshot) error {
	return nil
}
func (runner *expertRuntimeRunner) ChatStreamForModel(_ context.Context, model string, options llm.ChatOptions, callback func(llm.StreamChunk) error) error {
	runner.mu.Lock()
	runner.models = append(runner.models, model)
	runner.options = append(runner.options, options)
	runner.mu.Unlock()
	return callback(llm.StreamChunk{Text: "bounded report", Done: true})
}

func TestBuildRuntimeExpertConsultantUsesProfilesAndCaps(t *testing.T) {
	cfg := &config.Config{
		Ollama: config.OllamaConfig{NumCtx: 8192},
		Experts: config.ExpertsConfig{
			Enabled: true, MaxConcurrentInference: 1, MaxTeamExperts: 1,
			MaxEvalTokens: 333, Timeout: "5s",
		},
	}
	agents := &config.AgentsDir{Agents: map[string]config.AgentProfile{
		"media": {
			Name: "media", Model: "vision:4b", Description: "video specialist",
			UseCases: []string{"mp4 video timeline"}, SystemPrompt: "Focus on visual failure sequences.",
		},
	}}
	runner := &expertRuntimeRunner{}
	runtime, err := buildRuntimeExpertConsultant(cfg, runner, agents, []llm.OllamaModel{{
		Name: "vision:4b", Location: llm.OllamaModelLocationLocal, SizeBytes: 3 << 30,
	}}, resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
		return resource.HostSnapshot{LogicalCPU: 8, TotalRAMBytes: 16 << 30, AvailableRAMBytes: 10 << 30}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), expertteam.Request{
		Strategy: expertselector.StrategyTeam, Objective: "Inspect the video.", ExpertNames: []string{"media"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Parallelism != 1 || len(result.Experts) != 1 || result.Experts[0].Model != "vision:4b" {
		t.Fatalf("result = %#v", result)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.options) != 1 || runner.options[0].MaxEvalTokens != 333 || len(runner.options[0].Tools) != 0 ||
		!strings.Contains(runner.options[0].System, "Focus on visual failure sequences") {
		t.Fatalf("runtime options = %#v", runner.options)
	}
}

func TestBuildRuntimeExpertConsultantHonorsBoundedRequestModelOverride(t *testing.T) {
	cfg := &config.Config{
		Ollama:  config.OllamaConfig{NumCtx: 8192},
		Experts: config.ExpertsConfig{Enabled: true, MaxConcurrentInference: 1, MaxTeamExperts: 1, Timeout: "5s"},
	}
	agents := &config.AgentsDir{Agents: map[string]config.AgentProfile{
		"critic": {Name: "critic", Model: "profile:2b", Description: "configured critic"},
	}}
	runner := &expertRuntimeRunner{}
	runtime, err := buildRuntimeExpertConsultant(cfg, runner, agents, []llm.OllamaModel{
		{Name: "profile:2b", Location: llm.OllamaModelLocationLocal, SizeBytes: 2 << 30},
		{Name: "override:1b", Location: llm.OllamaModelLocationLocal, SizeBytes: 1 << 30},
	}, resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
		return resource.HostSnapshot{LogicalCPU: 8, TotalRAMBytes: 16 << 30, AvailableRAMBytes: 10 << 30}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), expertteam.Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review with an exact model.",
		ExpertNames: []string{"critic"}, Model: "request:2b",
		ModelOverrides: []expertteam.ModelOverride{{Expert: "critic", Model: "override:1b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experts) != 1 || result.Experts[0].Model != "override:1b" {
		t.Fatalf("override result = %#v", result)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.models) != 1 || runner.models[0] != "override:1b" || len(runner.options) != 1 || !runner.options[0].DisableReasoning {
		t.Fatalf("override dispatch models=%v options=%#v", runner.models, runner.options)
	}
}

func TestBuildRuntimeExpertConsultantCanBeDisabled(t *testing.T) {
	runtime, err := buildRuntimeExpertConsultant(&config.Config{}, &expertRuntimeRunner{}, nil, nil, resource.SystemProbe{})
	if err != nil || runtime != nil {
		t.Fatalf("disabled runtime = %#v, %v", runtime, err)
	}
}
