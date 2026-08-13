package main

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

func shouldPinStartupModel(modelOverride string, autoSelect bool) bool {
	return modelOverride != "" || !autoSelect
}

func shouldRestoreManualModelPreference(modelOverride string, profileModelPinned bool) bool {
	return strings.TrimSpace(modelOverride) == "" && !profileModelPinned
}

// restoreManualModelPreference accepts a saved selection only when the current
// Ollama inventory proves it is local and manually selectable. In particular,
// a remembered Ollama Cloud name never carries its prior session consent into
// a new process.
func restoreManualModelPreference(
	preferred string,
	manuallySelectable []string,
	localSelectable []string,
	inventoryKnown bool,
) (string, bool, string) {
	preferred = strings.TrimSpace(preferred)
	if preferred == "" {
		return "", false, ""
	}
	if !inventoryKnown {
		return "", false, fmt.Sprintf("saved model %q was not restored because Ollama inventory is unavailable", preferred)
	}
	if model, ok := matchingModel(localSelectable, preferred); ok {
		return model, true, ""
	}
	if _, ok := matchingModel(manuallySelectable, preferred); ok {
		return "", false, fmt.Sprintf("saved model %q needs a fresh explicit selection because it is not a verified local model", preferred)
	}
	return "", false, fmt.Sprintf("saved model %q is no longer available; using automatic or configured selection", preferred)
}

// restoreManualRemoteModelPreference accepts a saved selection only when the
// active remote provider's catalog (or configured list) still offers that id.
// Unlike the Ollama path, there is no inventory consent to refresh — the
// catalog is the authority. An id absent from SelectableRemoteModels is not
// forced in: passing preferred as the "current" argument would always admit it.
func restoreManualRemoteModelPreference(preferred, providerType string) (string, bool, string) {
	preferred = strings.TrimSpace(preferred)
	if preferred == "" {
		return "", false, ""
	}
	offered := config.SelectableRemoteModels(providerType, "")
	if model, ok := matchingModel(offered, preferred); ok {
		return model, true, ""
	}
	providerType = strings.TrimSpace(providerType)
	if providerType == "" {
		providerType = "remote"
	}
	return "", false, fmt.Sprintf(
		"saved model %q is not offered by provider %s; using configured selection",
		preferred, providerType,
	)
}

func matchingModel(models []string, wanted string) (string, bool) {
	wanted = config.CanonicalModelName(wanted)
	for _, model := range models {
		if config.CanonicalModelName(model) == wanted {
			return model, true
		}
	}
	return "", false
}

// resolveStartupModel keeps the manual selection inventory separate from the
// local-only automatic routing inventory. When inventoryKnown is false, it
// preserves the configured selection so the TUI can still start and report an
// offline Ollama diagnostic. The name-based local memory guard remains active
// in that fallback because no verified byte size is available yet; it
// deliberately does not infer execution location.
func resolveStartupModel(
	modelName string,
	modelPinned bool,
	localOnly bool,
	modelConfig *config.ModelConfig,
	manuallySelectable []string,
	autoRoutable []string,
	inventoryKnown bool,
	router availabilityAwareRouter,
) (string, []string, error) {
	if !inventoryKnown {
		if err := config.CheckLocalModelNameMemorySafe(modelName); err != nil {
			return "", nil, err
		}
	}
	if localOnly {
		check := config.CheckLocalModelNameMemorySafe
		if !inventoryKnown {
			check = config.CheckModelMemorySafe
		}
		if err := check(modelName); err != nil {
			return "", nil, err
		}
	}

	var modelList []string
	if !inventoryKnown {
		router.SetAvailableModels([]string{})
		return modelName, modelList, nil
	}

	modelList = uniqueModelNames(manuallySelectable)
	autoModels := uniqueModelNames(autoRoutable)
	router.SetAvailableModels(autoModels)
	if !modelPinned {
		modelName = router.ResolveAvailableModel(modelName)
		if !containsModel(autoModels, modelName) {
			modelName = ""
		}
		if modelName == "" && len(autoModels) > 0 {
			modelName = autoModels[0]
		}
	}
	if modelName == "" {
		return "", modelList, fmt.Errorf("no compatible local chat model is installed; pull a configured model such as %q", modelConfig.DefaultModel)
	}
	if localOnly {
		if err := config.CheckModelAvailableLocally(modelName, manuallySelectable); err != nil {
			return "", modelList, err
		}
	} else if !containsModel(manuallySelectable, modelName) {
		return "", modelList, fmt.Errorf("model %q is not available from the configured Ollama host", modelName)
	}
	return modelName, modelList, nil
}

func uniqueModelNames(models []string) []string {
	result := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		canonical := config.CanonicalModelName(model)
		if canonical == "" {
			continue
		}
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		result = append(result, model)
	}
	return result
}

func containsModel(models []string, wanted string) bool {
	wanted = config.CanonicalModelName(wanted)
	for _, model := range models {
		if config.CanonicalModelName(model) == wanted {
			return true
		}
	}
	return false
}

func selectHeadlessModel(current, prompt string, pinned bool, router config.ModelRouter, mode config.ModeContext) string {
	if pinned || router == nil {
		return current
	}
	return router.SelectModelForMode(prompt, mode)
}
