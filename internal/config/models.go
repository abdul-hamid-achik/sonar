package config

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type ModelFamily string

const (
	FamilyQwen3   ModelFamily = "qwen3"
	FamilyQwen35  ModelFamily = "qwen3.5"
	FamilyGemma   ModelFamily = "gemma"
	FamilyLlama   ModelFamily = "llama"
	FamilyMistral ModelFamily = "mistral"
	FamilyPhi     ModelFamily = "phi"
)

type ModelCapability int

const (
	CapabilitySimple ModelCapability = iota
	CapabilityMedium
	CapabilityComplex
	CapabilityAdvanced
)

func (c ModelCapability) String() string {
	switch c {
	case CapabilitySimple:
		return "simple"
	case CapabilityMedium:
		return "medium"
	case CapabilityComplex:
		return "complex"
	case CapabilityAdvanced:
		return "advanced"
	default:
		return strconv.Itoa(int(c))
	}
}

// UnmarshalYAML accepts the human-readable capability names used in the
// shipped example config while remaining compatible with older numeric files.
func (c *ModelCapability) UnmarshalYAML(node *yaml.Node) error {
	value := strings.ToLower(strings.TrimSpace(node.Value))
	switch value {
	case "simple":
		*c = CapabilitySimple
		return nil
	case "medium":
		*c = CapabilityMedium
		return nil
	case "complex":
		*c = CapabilityComplex
		return nil
	case "advanced":
		*c = CapabilityAdvanced
		return nil
	}

	n, err := strconv.Atoi(value)
	if err != nil || n < int(CapabilitySimple) || n > int(CapabilityAdvanced) {
		return fmt.Errorf("invalid model capability %q (want simple, medium, complex, advanced, or 0..3)", node.Value)
	}
	*c = ModelCapability(n)
	return nil
}

func (c ModelCapability) MarshalYAML() (any, error) {
	return c.String(), nil
}

type Model struct {
	Name        string          `yaml:"name"`
	Family      ModelFamily     `yaml:"family"`
	DisplayName string          `yaml:"display_name"`
	Size        string          `yaml:"size"`
	Parameters  string          `yaml:"parameters"`
	ContextSize int             `yaml:"context_size"`
	Capability  ModelCapability `yaml:"capability"`
	Speed       float64         `yaml:"speed"` // 1.0 = baseline
	UseCases    []string        `yaml:"use_cases"`
	Description string          `yaml:"description"`
	Default     bool            `yaml:"default,omitempty"`
	Exclusive   bool            `yaml:"exclusive,omitempty"` // manual profile; never auto-loaded
}

type ModelConfig struct {
	Models        []Model  `yaml:"models"`
	DefaultModel  string   `yaml:"default_model"`
	FallbackChain []string `yaml:"fallback_chain"`
	AutoSelect    bool     `yaml:"auto_select"`
	EmbedModel    string   `yaml:"embed_model,omitempty"`
}

func DefaultModels() []Model {
	return []Model{
		{
			Name:        "qwen3.5:0.8b",
			Family:      FamilyQwen35,
			DisplayName: "Qwen 3.5 0.8B",
			Size:        "0.8B",
			Parameters:  "0.8 billion",
			ContextSize: 262144,
			Capability:  CapabilitySimple,
			Speed:       4.0,
			UseCases:    []string{"quick_answers", "simple_tools", "single_file_edits"},
			Description: "Fast, lightweight model for simple tasks and quick answers",
			Default:     false,
		},
		{
			Name:        "qwen3.5:2b",
			Family:      FamilyQwen35,
			DisplayName: "Qwen 3.5 2B",
			Size:        "2B",
			Parameters:  "2 billion",
			ContextSize: 262144,
			Capability:  CapabilityMedium,
			Speed:       2.5,
			UseCases:    []string{"code_completion", "simple_refactoring", "explanations"},
			Description: "Balanced model for medium complexity tasks",
			Default:     true,
		},
		{
			Name:        "qwen3.5:4b",
			Family:      FamilyQwen35,
			DisplayName: "Qwen 3.5 4B",
			Size:        "4B",
			Parameters:  "4 billion",
			ContextSize: 262144,
			Capability:  CapabilityComplex,
			Speed:       1.5,
			UseCases:    []string{"multi_step_reasoning", "code_review", "debugging", "refactoring"},
			Description: "Capable model for complex reasoning and code analysis",
			Default:     false,
		},
		// Ollama advertises native tools for this profile, but the current
		// behavioral harness has not verified a valid native tool call. Keep it
		// visible for explicit selection and tool-free expert use without
		// placing it on an automatic agent path.
		{
			Name:        "phi4-mini:latest",
			Family:      FamilyPhi,
			DisplayName: "Phi-4 Mini",
			Size:        "2.5GB",
			Parameters:  "3.8 billion",
			ContextSize: 16384,
			Capability:  CapabilityMedium,
			Speed:       1.8,
			UseCases:    []string{"alternate_reasoning", "code_review", "explicit_profile"},
			Description: "Alternative compact reasoning profile; manual-only pending behavioral tool verification",
			Default:     false,
			Exclusive:   true,
		},
		// These tiers fit only as exclusive profiles on a 16GB machine. They are
		// visible and manually selectable, but the auto-router never loads them.
		{
			Name:        "qwen3.5:9b",
			Family:      FamilyQwen35,
			DisplayName: "Qwen 3.5 9B (exclusive)",
			Size:        "6.6GB",
			Parameters:  "9 billion",
			ContextSize: 262144,
			Capability:  CapabilityAdvanced,
			Speed:       0.8,
			UseCases:    []string{"architecture", "deep_review", "explicit_profile"},
			Description: "High-capability manual profile; unloads the previous chat model",
			Default:     false,
			Exclusive:   true,
		},
		{
			Name:        "ornith:latest",
			Family:      FamilyQwen35,
			DisplayName: "Ornith 9B (exclusive)",
			Size:        "5.6GB Q4",
			Parameters:  "9 billion",
			ContextSize: 262144,
			Capability:  CapabilityAdvanced,
			Speed:       0.8,
			UseCases:    []string{"advanced_coding", "multi_file_edits", "architecture", "deep_review", "explicit_profile"},
			Description: "Qwen 3.5-derived advanced coding model; manual exclusive profile with runtime context clamped by Ollama num_ctx",
			Default:     false,
			Exclusive:   true,
		},
		{
			Name:        "gemma4:e2b",
			Family:      FamilyGemma,
			DisplayName: "Gemma 4 E2B",
			Size:        "7.2GB",
			Parameters:  "2B effective",
			ContextSize: 131072,
			Capability:  CapabilityMedium,
			Speed:       2.2,
			UseCases:    []string{"alternate_reasoning", "explicit_profile"},
			Description: "Native tool calling; manual exclusive profile on 16GB",
			Default:     false,
			Exclusive:   true,
		},
		{
			Name:        "gemma4:e4b",
			Family:      FamilyGemma,
			DisplayName: "Gemma 4 E4B",
			Size:        "9.6GB",
			Parameters:  "4B effective",
			ContextSize: 131072,
			Capability:  CapabilityComplex,
			Speed:       1.3,
			UseCases:    []string{"unavailable_16gb"},
			Description: "Stronger Gemma, but ~9.6GB — unsafe on 16GB.",
			Default:     false,
			Exclusive:   true,
		},
	}
}

func DefaultModelConfig() ModelConfig {
	models := DefaultModels()
	return ModelConfig{
		Models:        models,
		DefaultModel:  "qwen3.5:2b",
		FallbackChain: []string{"qwen3.5:2b", "qwen3.5:0.8b", "qwen3.5:4b"},
		AutoSelect:    true,
		EmbedModel:    "nomic-embed-text",
	}
}

func (m *Model) IsSimpleTask() bool {
	return m.Capability <= CapabilityMedium
}

func (m *Model) IsComplexTask() bool {
	return m.Capability >= CapabilityComplex
}

func (mc *ModelConfig) GetModel(name string) (*Model, error) {
	for _, m := range mc.Models {
		if m.Name == name {
			return &m, nil
		}
	}
	return nil, fmt.Errorf("model not found: %s", name)
}

func (mc *ModelConfig) GetDefaultModel() *Model {
	for _, m := range mc.Models {
		if m.Default {
			return &m
		}
	}
	if len(mc.Models) > 0 {
		return &mc.Models[len(mc.Models)-1]
	}
	return nil
}

// CanonicalModelName mirrors Ollama's implicit :latest tag. It is used only
// for identity comparisons; callers may keep sending the user's configured
// spelling to Ollama.
func CanonicalModelName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	lastSegment := name
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		lastSegment = name[slash+1:]
	}
	if strings.Contains(lastSegment, ":") || strings.Contains(lastSegment, "@") {
		return name
	}
	return name + ":latest"
}

func (mc *ModelConfig) SelectModelForTask(taskComplexity string) string {
	if !mc.AutoSelect {
		return mc.DefaultModel
	}
	if len(mc.Models) == 0 {
		return ""
	}

	switch taskComplexity {
	case "simple":
		return mc.firstSafe(CapabilitySimple, mc.Models[0].Name)
	case "medium":
		return mc.firstSafe(CapabilityMedium, mc.DefaultModel)
	case "complex":
		return mc.firstSafe(CapabilityComplex, mc.DefaultModel)
	case "advanced":
		// There is no safe local tier above 4B on 16GB (larger models OOM), so
		// advanced tasks use the most capable SAFE model — the complex tier.
		return mc.firstSafe(CapabilityComplex, mc.DefaultModel)
	}

	return mc.autoSafeFallback(mc.DefaultModel)
}

// firstSafe returns the first model at the given capability that is memory-safe
// to auto-load (skips Gemma/large tiers so the router never OOMs the machine),
// falling back to fallbackName if none match.
func (mc *ModelConfig) firstSafe(cap ModelCapability, fallbackName string) string {
	for _, m := range mc.Models {
		if m.Capability == cap && !m.Exclusive && CheckModelMemorySafe(m.Name) == nil {
			return m.Name
		}
	}
	return mc.autoSafeFallback(fallbackName)
}

// autoSafeFallback returns a configured, non-exclusive, memory-safe model.
// Automatic routing must never inherit a manual-exclusive default from a
// custom catalog merely because no exact capability tier was found.
func (mc *ModelConfig) autoSafeFallback(preferred string) string {
	wanted := CanonicalModelName(preferred)
	known := false
	for _, model := range mc.Models {
		if CanonicalModelName(model.Name) == wanted {
			known = true
			if !model.Exclusive && CheckModelMemorySafe(model.Name) == nil {
				return model.Name
			}
			break
		}
	}
	// A custom default does not have exclusivity metadata when it is absent
	// from the catalog. Preserve that existing extension point while retaining
	// the global cloud/size guard.
	if !known && preferred != "" && CheckModelMemorySafe(preferred) == nil {
		return preferred
	}
	for _, model := range mc.Models {
		if !model.Exclusive && CheckModelMemorySafe(model.Name) == nil {
			return model.Name
		}
	}
	return ""
}

// AvailableLocalChatModels intersects the configured chat catalog with the
// models Ollama reported as having local weights. Safe automatic tiers come
// first in catalog order; locally installed exclusive profiles follow for
// explicit selection. Unknown models, the configured embedding model, and
// intrinsically memory-blocked tiers are intentionally omitted.
func (mc *ModelConfig) AvailableLocalChatModels(discovered []string) []string {
	available := make(map[string]struct{}, len(discovered))
	for _, name := range discovered {
		available[CanonicalModelName(name)] = struct{}{}
	}

	result := make([]string, 0, len(mc.Models))
	appendTier := func(exclusive bool) {
		for _, model := range mc.Models {
			if model.Exclusive != exclusive || model.Name == mc.EmbedModel || isMemoryRiskyModel(model.Name) {
				continue
			}
			if _, ok := available[CanonicalModelName(model.Name)]; ok {
				result = append(result, model.Name)
			}
		}
	}
	appendTier(false)
	appendTier(true)
	return result
}

// CatalogChatModels returns the configured chat catalog in the same safe-first
// order used after discovery. It is suitable for an offline diagnostic UI,
// where Ollama inventory lookup was unavailable and availability is unknown.
func (mc *ModelConfig) CatalogChatModels() []string {
	discovered := make([]string, 0, len(mc.Models))
	for _, model := range mc.Models {
		discovered = append(discovered, model.Name)
	}
	return mc.AvailableLocalChatModels(discovered)
}

// CheckModelAvailableLocally verifies a model identity against a successful
// Ollama local-weight inventory, treating an omitted tag as Ollama's :latest.
// Callers must not use it when discovery failed.
func CheckModelAvailableLocally(model string, discovered []string) error {
	wanted := CanonicalModelName(model)
	for _, name := range discovered {
		if CanonicalModelName(name) == wanted {
			return nil
		}
	}
	return fmt.Errorf("model %q is not installed with local Ollama weights", model)
}

// selectAvailableModel keeps routing within models that Ollama reported as
// local. A nil set means discovery was unavailable, in which case legacy
// behavior is preserved and the normal connection error remains visible.
func selectAvailableModel(preferred string, mc *ModelConfig, available map[string]struct{}) string {
	if available == nil {
		return preferred
	}
	if _, ok := available[CanonicalModelName(preferred)]; ok {
		return preferred
	}

	seen := map[string]struct{}{CanonicalModelName(preferred): {}}
	candidates := make([]string, 0, len(mc.FallbackChain)+len(mc.Models))
	candidates = append(candidates, mc.FallbackChain...)
	for _, model := range mc.Models {
		candidates = append(candidates, model.Name)
	}
	for _, candidate := range candidates {
		canonical := CanonicalModelName(candidate)
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		if candidate != preferred && mc.isExclusive(candidate) {
			continue
		}
		if _, ok := available[canonical]; ok {
			return candidate
		}
	}
	return ""
}

// selectAvailableAutoModel applies catalog safety before availability
// fallback. Explicit resolution intentionally uses selectAvailableModel so a
// user can still pin a locally installed manual-exclusive profile.
func selectAvailableAutoModel(preferred string, mc *ModelConfig, available map[string]struct{}) string {
	return selectAvailableModel(mc.autoSafeFallback(preferred), mc, available)
}

func (mc *ModelConfig) isExclusive(name string) bool {
	for _, model := range mc.Models {
		if model.Name == name {
			return model.Exclusive
		}
	}
	return false
}
