package resource

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Assess obtains one host snapshot, builds a normalized profile, and plans
// conservative concurrency. The probe is invoked exactly once.
func Assess(ctx context.Context, probe Probe, input Input) (Assessment, error) {
	if ctx == nil {
		return Assessment{}, errors.New("resource probe context is required")
	}
	if probe == nil {
		return Assessment{}, errors.New("resource probe is required")
	}
	snapshot, err := probe.Snapshot(ctx)
	if err != nil {
		return Assessment{}, fmt.Errorf("probe host resources: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Assessment{}, err
	}
	profile, err := BuildProfile(snapshot, input)
	if err != nil {
		return Assessment{}, err
	}
	plan, err := Plan(profile, input.Overrides)
	if err != nil {
		return Assessment{}, err
	}
	return Assessment{Profile: profile, Plan: plan}, nil
}

// BuildProfile is pure: it normalizes a supplied host snapshot and model set
// without probing the OS or mutating caller-owned slices.
func BuildProfile(snapshot HostSnapshot, input Input) (ResourceProfile, error) {
	if err := input.Overrides.Validate(); err != nil {
		return ResourceProfile{}, err
	}
	if snapshot.LogicalCPU < 0 || snapshot.TotalRAMBytes < 0 || snapshot.AvailableRAMBytes < 0 {
		return ResourceProfile{}, errors.New("host resource values cannot be negative")
	}

	logicalCPU := snapshot.LogicalCPU
	if input.Overrides.LogicalCPU > 0 {
		logicalCPU = input.Overrides.LogicalCPU
	}
	if logicalCPU <= 0 {
		return ResourceProfile{}, errors.New("logical CPU count is unavailable")
	}

	totalRAM := snapshot.TotalRAMBytes
	availableRAM := snapshot.AvailableRAMBytes
	availableKnown := snapshot.AvailableRAMKnown || availableRAM > 0
	if input.Overrides.TotalRAMBytes > 0 {
		totalRAM = input.Overrides.TotalRAMBytes
		// A probed available value is not comparable with a simulated/configured
		// total. Require a matching explicit available override or use total-only.
		if input.Overrides.AvailableRAMBytes == 0 {
			availableRAM = 0
			availableKnown = false
		}
	}
	if input.Overrides.AvailableRAMBytes > 0 {
		availableRAM = input.Overrides.AvailableRAMBytes
		availableKnown = true
	}
	if totalRAM > 0 && availableRAM > totalRAM {
		return ResourceProfile{}, fmt.Errorf("available RAM %d exceeds total RAM %d", availableRAM, totalRAM)
	}

	resident, err := normalizeModels(input.ActiveModels, input.NumCtx)
	if err != nil {
		return ResourceProfile{}, err
	}
	selected, err := normalizeModels(input.SelectedModels, input.NumCtx)
	if err != nil {
		return ResourceProfile{}, err
	}
	residentByName := make(map[string]struct{}, len(resident.models))
	for _, model := range resident.models {
		residentByName[modelKey(model.Name)] = struct{}{}
	}
	selectedBytes := int64(0)
	for _, model := range selected.models {
		if _, alreadyResident := residentByName[modelKey(model.Name)]; alreadyResident {
			continue
		}
		selectedBytes, err = checkedAdd(selectedBytes, modelFootprint(model))
		if err != nil {
			return ResourceProfile{}, fmt.Errorf("sum selected model footprints: %w", err)
		}
	}
	largestWeight := maxInt64(resident.largest, selected.largest)
	effectiveNumCtx := max(resident.numCtx, selected.numCtx)
	if effectiveNumCtx <= 0 {
		return ResourceProfile{}, errors.New("num_ctx is required when no model reports it")
	}
	weightAssumed := resident.assumed || selected.assumed
	if largestWeight == 0 {
		// Callers that do not provide SelectedModels retain the legacy ability to
		// plan an unknown future model. Expert execution always supplies its full
		// selected set and therefore reserves one fallback per unknown model.
		largestWeight = DefaultUnknownModelWeightBytes
		weightAssumed = true
	}

	kvBytesPerToken := input.Overrides.KVBytesPerToken
	if kvBytesPerToken == 0 {
		kvBytesPerToken = DefaultKVBytesPerToken
	}
	kvBytes, err := checkedMultiply(int64(effectiveNumCtx), kvBytesPerToken)
	if err != nil {
		return ResourceProfile{}, fmt.Errorf("estimate KV cache: %w", err)
	}
	overhead := input.Overrides.PerInferenceOverheadBytes
	if overhead == 0 {
		overhead = DefaultPerInferenceOverheadBytes
	}
	sharedBytes, err := checkedAdd(kvBytes, overhead)
	if err != nil {
		return ResourceProfile{}, fmt.Errorf("estimate shared-model inference: %w", err)
	}
	distinctBytes := sharedBytes
	if len(selected.models) == 0 {
		distinctBytes, err = checkedAdd(sharedBytes, largestWeight)
		if err != nil {
			return ResourceProfile{}, fmt.Errorf("estimate distinct-model inference: %w", err)
		}
	}

	warnings := make([]string, 0, 4)
	reservedCPU := input.Overrides.ReservedLogicalCPU
	if reservedCPU == 0 {
		reservedCPU = DefaultReservedLogicalCPU
	}
	if reservedCPU >= logicalCPU {
		if input.Overrides.ReservedLogicalCPU > 0 {
			return ResourceProfile{}, fmt.Errorf(
				"reserved logical CPU %d leaves no inference capacity on a %d-CPU host",
				reservedCPU, logicalCPU,
			)
		}
		reservedCPU = max(0, logicalCPU-1)
		warnings = append(warnings, "default CPU reserve was reduced to retain one inference CPU on this host")
	}
	usableCPU := logicalCPU - reservedCPU
	threads := input.Overrides.ThreadsPerInference
	if threads == 0 {
		threads = DefaultThreadsPerInference
	}
	if threads > usableCPU {
		if input.Overrides.ThreadsPerInference > 0 {
			return ResourceProfile{}, fmt.Errorf(
				"threads per inference %d exceeds the %d CPU(s) left after reserve",
				threads, usableCPU,
			)
		}
		threads = usableCPU
		warnings = append(warnings, "default inference threads were reduced to fit the CPU capacity left after reserve")
	}
	reservedRAM := input.Overrides.ReservedRAMBytes
	if reservedRAM == 0 {
		reservedRAM = defaultRAMReserve(totalRAM)
	}

	confidence := MemoryUnknown
	planningRAM := int64(0)
	switch {
	case availableKnown:
		confidence = MemoryAvailable
		planningRAM = subtractFloor(availableRAM, reservedRAM)
		planningRAM = subtractFloor(planningRAM, selectedBytes)
	case totalRAM > 0:
		confidence = MemoryTotalOnly
		planningRAM = subtractFloor(totalRAM, reservedRAM)
		planningRAM = subtractFloor(planningRAM, resident.bytes)
		planningRAM = subtractFloor(planningRAM, selectedBytes)
		// Without live available-memory telemetry, never allocate more than a
		// quarter of physical RAM to new concurrency.
		planningRAM = minInt64(planningRAM, totalRAM/4)
		warnings = append(warnings, "available RAM is unavailable; using a capped total-RAM estimate")
	default:
		warnings = append(warnings, "RAM telemetry is unavailable; concurrency is restricted to serial inference")
	}
	if planningRAM == 0 && confidence != MemoryUnknown {
		warnings = append(warnings, "planning RAM does not exceed the configured safety reserve")
	}
	if weightAssumed {
		warnings = append(warnings, "one or more model weights are unknown; using the configured fallback estimate per model")
	}

	return ResourceProfile{
		LogicalCPU: logicalCPU, TotalRAMBytes: totalRAM, AvailableRAMBytes: availableRAM,
		MemoryConfidence: confidence, ActiveModels: resident.models, SelectedModels: selected.models,
		ActiveModelBytes: resident.bytes, SelectedModelBytes: selectedBytes,
		LargestModelWeightBytes: largestWeight, ModelWeightAssumed: weightAssumed,
		EffectiveNumCtx: effectiveNumCtx, ReservedRAMBytes: reservedRAM,
		ReservedLogicalCPU: reservedCPU, ThreadsPerInference: threads,
		KVBytesPerToken: kvBytesPerToken, KVBytesPerInference: kvBytes,
		PerInferenceOverheadBytes: overhead, SharedInferenceBytes: sharedBytes,
		DistinctInferenceBytes: distinctBytes, PlanningRAMBytes: planningRAM,
		Warnings: warnings,
	}, nil
}

// Validate rejects ambiguous or unsafe override values. Max fields are caps and
// therefore may exceed built-in defaults harmlessly; Plan still applies the
// smaller policy limit.
func (o Overrides) Validate() error {
	for _, field := range []struct {
		name  string
		value int
	}{
		{"logical_cpu", o.LogicalCPU},
		{"reserved_logical_cpu", o.ReservedLogicalCPU},
		{"threads_per_inference", o.ThreadsPerInference},
		{"max_concurrent_inference", o.MaxConcurrentInference},
		{"max_concurrent_distinct_models", o.MaxConcurrentDistinctModels},
		{"max_concurrent_teams", o.MaxConcurrentTeams},
		{"max_team_experts", o.MaxTeamExperts},
		{"max_swarm_workers", o.MaxSwarmWorkers},
		{"max_moe_experts", o.MaxMoEExperts},
	} {
		if field.value < 0 {
			return fmt.Errorf("resource override %s cannot be negative", field.name)
		}
	}
	for _, field := range []struct {
		name  string
		value int64
	}{
		{"total_ram_bytes", o.TotalRAMBytes},
		{"available_ram_bytes", o.AvailableRAMBytes},
		{"reserved_ram_bytes", o.ReservedRAMBytes},
		{"kv_bytes_per_token", o.KVBytesPerToken},
		{"per_inference_overhead_bytes", o.PerInferenceOverheadBytes},
	} {
		if field.value < 0 {
			return fmt.Errorf("resource override %s cannot be negative", field.name)
		}
	}
	if o.TotalRAMBytes > 0 && o.AvailableRAMBytes > o.TotalRAMBytes {
		return fmt.Errorf("resource override available_ram_bytes exceeds total_ram_bytes")
	}
	if o.ReservedRAMBytes > 0 && o.ReservedRAMBytes < minimumRAMReserveBytes {
		return fmt.Errorf("resource override reserved_ram_bytes must be at least %d", minimumRAMReserveBytes)
	}
	if o.KVBytesPerToken > 0 && o.KVBytesPerToken < minimumKVBytesPerToken {
		return fmt.Errorf("resource override kv_bytes_per_token must be at least %d", minimumKVBytesPerToken)
	}
	if o.PerInferenceOverheadBytes > 0 && o.PerInferenceOverheadBytes < minimumPerInferenceOverheadBytes {
		return fmt.Errorf("resource override per_inference_overhead_bytes must be at least %d", minimumPerInferenceOverheadBytes)
	}
	return nil
}

type normalizedModels struct {
	models  []ActiveModel
	bytes   int64
	largest int64
	numCtx  int
	assumed bool
}

func normalizeModels(input []ActiveModel, defaultNumCtx int) (normalizedModels, error) {
	if defaultNumCtx < 0 {
		return normalizedModels{}, errors.New("num_ctx cannot be negative")
	}
	byName := make(map[string]ActiveModel, len(input))
	for _, candidate := range input {
		candidate.Name = strings.TrimSpace(candidate.Name)
		if candidate.Name == "" {
			return normalizedModels{}, errors.New("model name is required")
		}
		if candidate.WeightBytes < 0 || candidate.ResidentBytes < 0 || candidate.NumCtx < 0 {
			return normalizedModels{}, fmt.Errorf("model %q has a negative resource value", candidate.Name)
		}
		key := modelKey(candidate.Name)
		if existing, ok := byName[key]; ok {
			existing.WeightBytes = maxInt64(existing.WeightBytes, candidate.WeightBytes)
			existing.ResidentBytes = maxInt64(existing.ResidentBytes, candidate.ResidentBytes)
			existing.NumCtx = max(existing.NumCtx, candidate.NumCtx)
			byName[key] = existing
			continue
		}
		byName[key] = candidate
	}

	models := make([]ActiveModel, 0, len(byName))
	for _, model := range byName {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool {
		return strings.ToLower(models[i].Name) < strings.ToLower(models[j].Name)
	})

	result := normalizedModels{models: models, numCtx: defaultNumCtx}
	for index := range result.models {
		model := &result.models[index]
		footprint := modelFootprint(*model)
		if footprint == 0 {
			model.WeightBytes = DefaultUnknownModelWeightBytes
			footprint = DefaultUnknownModelWeightBytes
			result.assumed = true
		}
		var err error
		result.bytes, err = checkedAdd(result.bytes, footprint)
		if err != nil {
			return normalizedModels{}, fmt.Errorf("sum model footprints: %w", err)
		}
		result.largest = maxInt64(result.largest, footprint)
		result.numCtx = max(result.numCtx, model.NumCtx)
	}
	return result, nil
}

func modelFootprint(model ActiveModel) int64 {
	return maxInt64(model.WeightBytes, model.ResidentBytes)
}

func modelKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func defaultRAMReserve(total int64) int64 {
	reserve := DefaultMinimumRAMReserveBytes
	if fraction := total / 5; fraction > reserve {
		reserve = fraction
	}
	return reserve
}

func checkedAdd(left, right int64) (int64, error) {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return 0, errors.New("resource byte estimate overflows int64")
	}
	return left + right, nil
}

func checkedMultiply(left, right int64) (int64, error) {
	if left < 0 || right < 0 || (left != 0 && right > math.MaxInt64/left) {
		return 0, errors.New("resource byte estimate overflows int64")
	}
	return left * right, nil
}

func subtractFloor(value, amount int64) int64 {
	if amount >= value {
		return 0
	}
	return value - amount
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
