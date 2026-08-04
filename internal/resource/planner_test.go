package resource

import "testing"

func TestPlanKeepsFanoutUsableWhenInferenceMustBeSerial(t *testing.T) {
	t.Parallel()

	profile, err := BuildProfile(HostSnapshot{LogicalCPU: 32}, Input{
		ActiveModels: []ActiveModel{{Name: "model", WeightBytes: 4 * gib}},
		NumCtx:       8_192,
	})
	if err != nil {
		t.Fatalf("BuildProfile() error = %v", err)
	}
	plan, err := Plan(profile, Overrides{})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	if plan.MaxConcurrentInference != 1 || plan.MaxConcurrentDistinctModels != 1 {
		t.Fatalf("simultaneous limits = shared %d, distinct %d; want 1, 1", plan.MaxConcurrentInference, plan.MaxConcurrentDistinctModels)
	}
	if !plan.SerialOnly {
		t.Error("SerialOnly = false, want true")
	}
	if plan.MaxConcurrentTeams != 1 {
		t.Errorf("MaxConcurrentTeams = %d, want 1", plan.MaxConcurrentTeams)
	}
	if plan.MaxTeamExperts != 3 || plan.MaxSwarmWorkers != 7 || plan.MaxMoEExperts != 4 {
		t.Errorf("serial plan disabled logical fan-out: %+v", plan)
	}
	if plan.Bottleneck != BottleneckUnknownMemory || plan.DistinctBottleneck != BottleneckUnknownMemory {
		t.Errorf("unknown-memory bottlenecks not retained: %+v", plan)
	}
}

func TestPlanDeniesKnownMemoryBudgetThatCannotFitOneInference(t *testing.T) {
	t.Parallel()

	profile, err := BuildProfile(HostSnapshot{
		LogicalCPU: 8, TotalRAMBytes: 16 * gib, AvailableRAMBytes: 6 * gib,
	}, Input{
		SelectedModels: []ActiveModel{{Name: "large", WeightBytes: 4 * gib}},
		NumCtx:         8_192,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Plan(profile, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Admitted || plan.MaxConcurrentInference != 0 || plan.MaxConcurrentDistinctModels != 0 || plan.MaxConcurrentTeams != 0 {
		t.Fatalf("known-insufficient plan admitted work: %+v", plan)
	}
	if plan.SerialOnly {
		t.Fatal("denied plan was mislabeled as serial execution")
	}
}

func TestPlanCapsConcurrencyAndFanoutIndependently(t *testing.T) {
	t.Parallel()

	profile := testProfile()
	plan, err := Plan(profile, Overrides{
		MaxConcurrentInference:      2,
		MaxConcurrentDistinctModels: 1,
		MaxConcurrentTeams:          1,
		MaxTeamExperts:              2,
		MaxSwarmWorkers:             3,
		MaxMoEExperts:               2,
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	if plan.MaxConcurrentInference != 2 || plan.MaxConcurrentDistinctModels != 1 {
		t.Errorf("simultaneous limits = shared %d distinct %d, want 2 and 1", plan.MaxConcurrentInference, plan.MaxConcurrentDistinctModels)
	}
	if plan.MaxConcurrentTeams != 1 || plan.MaxTeamExperts != 2 || plan.MaxSwarmWorkers != 3 || plan.MaxMoEExperts != 2 {
		t.Errorf("fan-out caps not applied: %+v", plan)
	}
	if plan.SerialOnly {
		t.Error("SerialOnly = true with two shared-model slots")
	}
	if plan.Bottleneck != BottleneckOverride || plan.DistinctBottleneck != BottleneckOverride {
		t.Errorf("override bottlenecks not recorded: %+v", plan)
	}
}

func TestPlanCannotReportMoreThanOneConcurrentTeam(t *testing.T) {
	t.Parallel()

	plan, err := Plan(testProfile(), Overrides{MaxConcurrentTeams: 99})
	if err != nil {
		t.Fatal(err)
	}
	if plan.MaxConcurrentTeams != 1 {
		t.Fatalf("MaxConcurrentTeams = %d, want runtime-effective limit 1", plan.MaxConcurrentTeams)
	}
}

func TestPlanReportsSharedBottleneck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		profile   ResourceProfile
		overrides Overrides
		want      Bottleneck
		wantLimit int
	}{
		{name: "CPU", profile: profileWithLimits(4, 10, MemoryAvailable), want: BottleneckCPU, wantLimit: 4},
		{name: "memory", profile: profileWithLimits(8, 2, MemoryAvailable), want: BottleneckMemory, wantLimit: 2},
		{name: "balanced", profile: profileWithLimits(4, 4, MemoryAvailable), want: BottleneckBalanced, wantLimit: 4},
		{name: "policy", profile: profileWithLimits(16, 16, MemoryAvailable), want: BottleneckPolicy, wantLimit: 8},
		{name: "override", profile: profileWithLimits(8, 8, MemoryAvailable), overrides: Overrides{MaxConcurrentInference: 2}, want: BottleneckOverride, wantLimit: 2},
		{name: "unknown memory", profile: profileWithLimits(8, 1, MemoryUnknown), want: BottleneckUnknownMemory, wantLimit: 1},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan, err := Plan(test.profile, test.overrides)
			if err != nil {
				t.Fatalf("Plan() error = %v", err)
			}
			if plan.Bottleneck != test.want || plan.MaxConcurrentInference != test.wantLimit {
				t.Errorf("Plan() = bottleneck %q limit %d, want %q and %d", plan.Bottleneck, plan.MaxConcurrentInference, test.want, test.wantLimit)
			}
		})
	}
}

func TestPlanRejectsIncompleteProfile(t *testing.T) {
	t.Parallel()

	valid := testProfile()
	tests := []struct {
		name   string
		mutate func(*ResourceProfile)
	}{
		{name: "CPU", mutate: func(profile *ResourceProfile) { profile.LogicalCPU = 0 }},
		{name: "context", mutate: func(profile *ResourceProfile) { profile.EffectiveNumCtx = 0 }},
		{name: "threads", mutate: func(profile *ResourceProfile) { profile.ThreadsPerInference = 0 }},
		{name: "negative budget", mutate: func(profile *ResourceProfile) { profile.PlanningRAMBytes = -1 }},
		{name: "shared estimate", mutate: func(profile *ResourceProfile) { profile.SharedInferenceBytes = 0 }},
		{name: "distinct estimate", mutate: func(profile *ResourceProfile) { profile.DistinctInferenceBytes = 0 }},
		{name: "distinct below shared", mutate: func(profile *ResourceProfile) { profile.DistinctInferenceBytes = profile.SharedInferenceBytes - 1 }},
		{name: "memory confidence", mutate: func(profile *ResourceProfile) { profile.MemoryConfidence = "maybe" }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			profile := valid
			test.mutate(&profile)
			if _, err := Plan(profile, Overrides{}); err == nil {
				t.Fatal("Plan() error = nil, want error")
			}
		})
	}
}

func TestPlanDeniesCPUSettingsThatLeaveNoUsableSlot(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*ResourceProfile){
		"reserve consumes host": func(profile *ResourceProfile) {
			profile.LogicalCPU = 2
			profile.ReservedLogicalCPU = 2
		},
		"threads exceed usable": func(profile *ResourceProfile) {
			profile.LogicalCPU = 2
			profile.ReservedLogicalCPU = 1
			profile.ThreadsPerInference = 2
		},
	} {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			profile := testProfile()
			mutate(&profile)
			plan, err := Plan(profile, Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Admitted || plan.CPULimit != 0 || plan.MaxConcurrentInference != 0 || plan.MaxConcurrentTeams != 0 {
				t.Fatalf("unsafe CPU plan admitted work: %+v", plan)
			}
		})
	}
}

func testProfile() ResourceProfile {
	return profileWithLimits(16, 16, MemoryAvailable)
}

func profileWithLimits(cpuLimit, memoryLimit int, confidence MemoryConfidence) ResourceProfile {
	const perInference = gib
	return ResourceProfile{
		LogicalCPU:             cpuLimit,
		MemoryConfidence:       confidence,
		EffectiveNumCtx:        8_192,
		ThreadsPerInference:    1,
		SharedInferenceBytes:   perInference,
		DistinctInferenceBytes: perInference,
		PlanningRAMBytes:       int64(memoryLimit) * perInference,
	}
}
