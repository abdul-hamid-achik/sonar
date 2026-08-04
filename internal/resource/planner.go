package resource

import (
	"errors"
	"math"
)

// Plan is pure and applies conservative CPU, RAM, topology, and override caps.
// Hardware and memory-estimation overrides must already have been normalized by
// BuildProfile; Plan reads only the Max* cap fields from overrides.
func Plan(profile ResourceProfile, overrides Overrides) (ConcurrencyPlan, error) {
	if err := overrides.Validate(); err != nil {
		return ConcurrencyPlan{}, err
	}
	if profile.LogicalCPU <= 0 || profile.EffectiveNumCtx <= 0 {
		return ConcurrencyPlan{}, errors.New("resource profile is incomplete")
	}
	if profile.ReservedLogicalCPU < 0 || profile.ThreadsPerInference <= 0 ||
		profile.PlanningRAMBytes < 0 || profile.SharedInferenceBytes <= 0 || profile.DistinctInferenceBytes <= 0 {
		return ConcurrencyPlan{}, errors.New("resource profile contains invalid planning values")
	}
	if profile.DistinctInferenceBytes < profile.SharedInferenceBytes {
		return ConcurrencyPlan{}, errors.New("distinct-model inference cannot cost less than shared-model inference")
	}
	if profile.MemoryConfidence != MemoryAvailable && profile.MemoryConfidence != MemoryTotalOnly && profile.MemoryConfidence != MemoryUnknown {
		return ConcurrencyPlan{}, errors.New("resource profile has invalid memory confidence")
	}

	usableCPU := profile.LogicalCPU - profile.ReservedLogicalCPU
	cpuLimit := 0
	if usableCPU > 0 && usableCPU >= profile.ThreadsPerInference {
		cpuLimit = usableCPU / profile.ThreadsPerInference
	}
	sharedMemoryLimit := 1
	distinctMemoryLimit := 1
	if profile.MemoryConfidence != MemoryUnknown {
		sharedMemoryLimit = memorySlots(profile.PlanningRAMBytes, profile.SharedInferenceBytes)
		distinctMemoryLimit = memorySlots(profile.PlanningRAMBytes, profile.DistinctInferenceBytes)
	}

	rawShared := min(cpuLimit, sharedMemoryLimit)
	sharedLimit := min(rawShared, DefaultMaxConcurrentInference)
	bottleneck := limitBottleneck(profile.MemoryConfidence, cpuLimit, sharedMemoryLimit)
	if sharedLimit < rawShared {
		bottleneck = BottleneckPolicy
	}
	if overrides.MaxConcurrentInference > 0 && overrides.MaxConcurrentInference < sharedLimit {
		sharedLimit = overrides.MaxConcurrentInference
		bottleneck = BottleneckOverride
	}
	sharedLimit = max(0, sharedLimit)

	rawDistinct := min(cpuLimit, distinctMemoryLimit)
	distinctLimit := min(rawDistinct, DefaultMaxConcurrentDistinctModels)
	distinctBottleneck := limitBottleneck(profile.MemoryConfidence, cpuLimit, distinctMemoryLimit)
	if distinctLimit < rawDistinct {
		distinctBottleneck = BottleneckPolicy
	}
	if overrides.MaxConcurrentInference > 0 {
		if overrides.MaxConcurrentInference < distinctLimit {
			distinctLimit = overrides.MaxConcurrentInference
			distinctBottleneck = BottleneckOverride
		}
	}
	if overrides.MaxConcurrentDistinctModels > 0 && overrides.MaxConcurrentDistinctModels < distinctLimit {
		distinctLimit = overrides.MaxConcurrentDistinctModels
		distinctBottleneck = BottleneckOverride
	}
	distinctLimit = max(0, distinctLimit)

	// Fan-out describes how much work a topology may select, not how much it may
	// execute simultaneously. Keeping these independent lets a one-slot host run
	// a full team/swarm/MoE safely by scheduling its experts one at a time.
	teamExperts := DefaultMaxTeamExperts
	swarmWorkers := DefaultMaxSwarmWorkers
	moeExperts := DefaultMaxMoEExperts
	concurrentTeams := min(sharedLimit, DefaultMaxConcurrentTeams)

	teamExperts = applyCap(teamExperts, overrides.MaxTeamExperts)
	swarmWorkers = applyCap(swarmWorkers, overrides.MaxSwarmWorkers)
	moeExperts = applyCap(moeExperts, overrides.MaxMoEExperts)
	concurrentTeams = applyCap(concurrentTeams, overrides.MaxConcurrentTeams)

	return ConcurrencyPlan{
		Admitted:                    sharedLimit > 0,
		MaxConcurrentInference:      sharedLimit,
		MaxConcurrentDistinctModels: distinctLimit,
		MaxConcurrentTeams:          concurrentTeams,
		MaxTeamExperts:              teamExperts,
		MaxSwarmWorkers:             swarmWorkers,
		MaxMoEExperts:               moeExperts,
		CPULimit:                    cpuLimit,
		SharedMemoryLimit:           sharedMemoryLimit,
		DistinctMemoryLimit:         distinctMemoryLimit,
		Bottleneck:                  bottleneck,
		DistinctBottleneck:          distinctBottleneck,
		SerialOnly:                  sharedLimit == 1 && distinctLimit <= 1,
	}, nil
}

func memorySlots(budget, perInference int64) int {
	if budget <= 0 || perInference <= 0 {
		return 0
	}
	slots := budget / perInference
	if slots <= 0 {
		return 0
	}
	if slots > int64(math.MaxInt) {
		return math.MaxInt
	}
	return int(slots)
}

func limitBottleneck(confidence MemoryConfidence, cpuLimit, memoryLimit int) Bottleneck {
	if confidence == MemoryUnknown {
		return BottleneckUnknownMemory
	}
	switch {
	case cpuLimit == memoryLimit:
		return BottleneckBalanced
	case cpuLimit < memoryLimit:
		return BottleneckCPU
	case memoryLimit < cpuLimit:
		return BottleneckMemory
	default:
		return BottleneckBalanced
	}
}

func applyCap(value, cap int) int {
	if cap > 0 && cap < value {
		return cap
	}
	return value
}
