package main

import (
	"context"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type ollamaModelShower interface {
	ShowOllamaModel(context.Context, string) (llm.OllamaModelInfo, error)
}

func enrichOllamaCapabilities(ctx context.Context, manager ollamaModelShower, models []llm.OllamaModel, modelConfig *config.ModelConfig) []llm.OllamaModel {
	result := append([]llm.OllamaModel(nil), models...)
	for index := range result {
		if (len(result[index].Capabilities) > 0 && result[index].ContextLength > 0) || ctx.Err() != nil {
			continue
		}
		if info, err := manager.ShowOllamaModel(ctx, result[index].Name); err == nil {
			if len(result[index].Capabilities) == 0 && len(info.Capabilities) > 0 {
				result[index].Capabilities = append([]string(nil), info.Capabilities...)
			}
			if info.NativeContext > 0 {
				result[index].ContextLength = info.NativeContext
			}
			if len(result[index].Capabilities) > 0 {
				continue
			}
		}
		// Older Ollama servers omitted capabilities from both tags and show.
		// Preserve only explicitly configured chat profiles in that case; custom
		// unknowns remain visible but fail closed until Ollama proves completion.
		if configuredChatModel(modelConfig, result[index].Name) {
			result[index].Capabilities = []string{"completion", "tools"}
		}
	}
	return result
}

func configuredChatModel(modelConfig *config.ModelConfig, name string) bool {
	if modelConfig == nil {
		return false
	}
	wanted := config.CanonicalModelName(name)
	for _, model := range modelConfig.Models {
		if config.CanonicalModelName(model.Name) == wanted && config.CanonicalModelName(model.Name) != config.CanonicalModelName(modelConfig.EmbedModel) {
			return true
		}
	}
	return false
}

// manuallySelectableOllamaChatModels returns models the current privacy policy
// permits a user to choose explicitly. It is deliberately broader than the
// automatic router inventory when local-only mode is disabled.
func manuallySelectableOllamaChatModels(models []llm.OllamaModel, localOnly bool) []string {
	result := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if !ollamaCapability(model.Capabilities, "completion") || !ollamaCapability(model.Capabilities, "tools") {
			continue
		}
		switch model.Location {
		case llm.OllamaModelLocationLocal:
			if config.CheckLocalModelSizeSafe(model.Name, model.SizeBytes) != nil {
				continue
			}
		case llm.OllamaModelLocationCloud:
			if model.ContextLength <= 0 {
				continue
			}
			if localOnly {
				continue
			}
		case llm.OllamaModelLocationRemote:
			continue
		default:
			continue
		}
		canonical := config.CanonicalModelName(model.Name)
		if canonical == "" {
			continue
		}
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		result = append(result, model.Name)
	}
	return result
}

// autoRoutableOllamaChatModels applies the strict local admission projection
// regardless of privacy mode. Ollama Cloud and configured exclusive profiles
// remain manual-only choices even when their local weights are installed.
func autoRoutableOllamaChatModels(models []llm.OllamaModel, modelConfig *config.ModelConfig) []string {
	candidates := manuallySelectableOllamaChatModels(models, true)
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if configuredExclusiveModel(modelConfig, candidate) {
			continue
		}
		result = append(result, candidate)
	}
	return result
}

func configuredExclusiveModel(modelConfig *config.ModelConfig, name string) bool {
	if modelConfig == nil {
		return false
	}
	wanted := config.CanonicalModelName(name)
	for _, model := range modelConfig.Models {
		if config.CanonicalModelName(model.Name) == wanted {
			return model.Exclusive
		}
	}
	return false
}

func ollamaCapability(capabilities []string, wanted string) bool {
	for _, capability := range capabilities {
		if strings.EqualFold(strings.TrimSpace(capability), wanted) {
			return true
		}
	}
	return false
}
