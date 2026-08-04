package ui

import (
	"fmt"
	"math"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// BuildOllamaModelDescriptors projects Ollama's authoritative inventory into
// presentation state. Static configuration may rank models, but never invents
// availability or capabilities here.
func BuildOllamaModelDescriptors(inventory []llm.OllamaModel, running []llm.OllamaRunningModel, current string, localOnly bool) []OllamaModelDescriptor {
	runningByName := make(map[string]llm.OllamaRunningModel, len(running))
	for _, model := range running {
		runningByName[config.CanonicalModelName(model.Model.Name)] = model
	}
	descriptors := make([]OllamaModelDescriptor, 0, len(inventory))
	for _, model := range inventory {
		if len(model.Capabilities) > 0 && !hasOllamaCapability(model.Capabilities, "completion") {
			continue
		}
		descriptor := OllamaModelDescriptor{
			Name: model.Name, DisplayName: model.Name,
			ParameterSize: model.ParameterSize,
			Quantization:  model.Quantization, ContextLength: boundedContextLength(model.ContextLength),
			Capabilities: append([]string(nil), model.Capabilities...),
			Current:      config.CanonicalModelName(model.Name) == config.CanonicalModelName(current),
			Selectable:   true, Fit: true, AutoRoutable: true,
		}
		switch model.Location {
		case llm.OllamaModelLocationLocal:
			descriptor.Source = OllamaModelLocal
			descriptor.SizeBytes = model.SizeBytes
			if err := config.CheckLocalModelSizeSafe(model.Name, model.SizeBytes); err != nil {
				descriptor.Fit = false
				descriptor.Reason = "outside local memory profile"
			}
		case llm.OllamaModelLocationCloud:
			descriptor.Source = OllamaModelCloud
			// Cloud is an explicit user choice, never an automatic routing
			// candidate. Privacy policy controls whether that choice needs an
			// exact conversation grant; it does not grant router authority.
			descriptor.AutoRoutable = false
			if localOnly {
				descriptor.RequiresConsent = true
				descriptor.Reason = "conversation confirmation required"
			}
			if descriptor.ContextLength <= 0 {
				descriptor.Selectable = false
				descriptor.AutoRoutable = false
				descriptor.Reason = "context maximum unavailable; refresh details"
			}
		case llm.OllamaModelLocationRemote:
			descriptor.Source = OllamaModelRemote
			descriptor.Selectable = false
			descriptor.AutoRoutable = false
			descriptor.Reason = "remote host is not Ollama Cloud"
		default:
			descriptor.Source = OllamaModelRemote
			descriptor.Selectable = false
			descriptor.Fit = false
			descriptor.AutoRoutable = false
			descriptor.Reason = "execution location unknown"
		}
		if len(model.Capabilities) == 0 {
			descriptor.Selectable = false
			descriptor.AutoRoutable = false
			if descriptor.Reason == "" {
				descriptor.Reason = "capabilities unknown; inspect or refresh"
			}
		} else if !hasOllamaCapability(model.Capabilities, "tools") {
			descriptor.Selectable = false
			descriptor.AutoRoutable = false
			if descriptor.Reason == "" {
				descriptor.Reason = "tool calling unavailable"
			}
		}
		if resident, ok := runningByName[config.CanonicalModelName(model.Name)]; ok {
			descriptor.Running = true
			if resident.ContextLength > 0 {
				descriptor.AllocatedContext = boundedContextLength(resident.ContextLength)
			}
			descriptor.SizeVRAM = resident.SizeVRAM
		}
		descriptors = append(descriptors, descriptor)
	}
	return descriptors
}

func hasOllamaCapability(capabilities []string, wanted string) bool {
	for _, capability := range capabilities {
		if strings.EqualFold(strings.TrimSpace(capability), wanted) {
			return true
		}
	}
	return false
}

func boundedContextLength(value int64) int {
	if value <= 0 {
		return 0
	}
	if value > math.MaxInt {
		return math.MaxInt
	}
	return int(value)
}

func ollamaInventorySummary(models []OllamaModelDescriptor) string {
	local, cloud, remote, unavailable := 0, 0, 0, 0
	for _, model := range models {
		switch {
		case !model.Selectable || !model.Fit:
			unavailable++
		case model.Source == OllamaModelLocal:
			local++
		case model.Source == OllamaModelCloud:
			cloud++
		default:
			remote++
		}
	}
	parts := []string{fmt.Sprintf("%d local", local), fmt.Sprintf("%d cloud", cloud)}
	if remote > 0 {
		parts = append(parts, fmt.Sprintf("%d remote", remote))
	}
	parts = append(parts, fmt.Sprintf("%d unavailable", unavailable))
	return strings.Join(parts, " · ")
}
