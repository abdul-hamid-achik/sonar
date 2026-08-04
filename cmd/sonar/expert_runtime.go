package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/resource"
)

func newRuntimeExpertConsultant(cfg *config.Config, runner expertteam.ModelRunner, agentsDir *config.AgentsDir, inventory []llm.OllamaModel) (*expertteam.Runtime, error) {
	return buildRuntimeExpertConsultant(cfg, runner, agentsDir, inventory, resource.SystemProbe{})
}

func buildRuntimeExpertConsultant(cfg *config.Config, runner expertteam.ModelRunner, agentsDir *config.AgentsDir, inventory []llm.OllamaModel, probe resource.Probe) (*expertteam.Runtime, error) {
	if cfg == nil || !cfg.Experts.Enabled {
		return nil, nil
	}
	if runner == nil {
		return nil, fmt.Errorf("model runner is unavailable")
	}
	timeout, err := time.ParseDuration(cfg.Experts.Timeout)
	if err != nil {
		return nil, fmt.Errorf("parse expert timeout: %w", err)
	}
	profiles := make([]expertteam.Profile, 0)
	if agentsDir != nil {
		configured := agentsDir.ListAgents()
		sort.Slice(configured, func(left, right int) bool { return configured[left].Name < configured[right].Name })
		profiles = make([]expertteam.Profile, 0, len(configured))
		for _, profile := range configured {
			profiles = append(profiles, expertteam.Profile{
				Name: profile.Name, Description: profile.Description,
				UseCases: append([]string(nil), profile.UseCases...),
				Model:    profile.Model, SystemPrompt: profile.SystemPrompt,
			})
		}
	}
	weights := make(map[string]int64, len(inventory))
	for _, model := range inventory {
		if model.Location == llm.OllamaModelLocationLocal && model.SizeBytes > 0 {
			weights[model.Name] = model.SizeBytes
		}
	}
	overrides := resource.Overrides{
		MaxConcurrentInference:      cfg.Experts.MaxConcurrentInference,
		MaxConcurrentDistinctModels: cfg.Experts.MaxConcurrentDistinctModels,
		MaxTeamExperts:              cfg.Experts.MaxTeamExperts,
		MaxSwarmWorkers:             cfg.Experts.MaxSwarmWorkers,
		MaxMoEExperts:               cfg.Experts.MaxMoEExperts,
	}
	return expertteam.New(runner, expertteam.Options{
		Profiles: profiles, Probe: probe, ResourceOverrides: overrides,
		ModelWeights: weights, DefaultNumCtx: cfg.Ollama.NumCtx,
		MaxEvalTokens: cfg.Experts.MaxEvalTokens, ExpertTimeout: timeout,
	})
}

// Compile-time check that the concrete manager preserves privacy/model
// admission when used by expert consultations.
var _ expertteam.ModelRunner = (*llm.ModelManager)(nil)
