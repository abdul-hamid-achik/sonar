package resource

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

const (
	kib int64 = 1 << 10
	mib int64 = 1 << 20
	gib int64 = 1 << 30
)

func TestAssessUsesInjectedProbeOnce(t *testing.T) {
	t.Parallel()

	calls := 0
	probe := ProbeFunc(func(context.Context) (HostSnapshot, error) {
		calls++
		return HostSnapshot{
			LogicalCPU:        16,
			TotalRAMBytes:     32 * gib,
			AvailableRAMBytes: 24 * gib,
		}, nil
	})

	assessment, err := Assess(context.Background(), probe, Input{
		ActiveModels: []ActiveModel{{Name: "qwen", WeightBytes: 2 * gib, NumCtx: 16_384}},
		NumCtx:       8_192,
	})
	if err != nil {
		t.Fatalf("Assess() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}

	profile := assessment.Profile
	if profile.EffectiveNumCtx != 16_384 {
		t.Errorf("EffectiveNumCtx = %d, want 16384", profile.EffectiveNumCtx)
	}
	if profile.MemoryConfidence != MemoryAvailable {
		t.Errorf("MemoryConfidence = %q, want %q", profile.MemoryConfidence, MemoryAvailable)
	}
	if profile.ReservedRAMBytes != 32*gib/5 {
		t.Errorf("ReservedRAMBytes = %d, want %d", profile.ReservedRAMBytes, 32*gib/5)
	}
	if profile.PlanningRAMBytes != 24*gib-32*gib/5 {
		t.Errorf("PlanningRAMBytes = %d, want %d", profile.PlanningRAMBytes, 24*gib-32*gib/5)
	}
	if profile.KVBytesPerInference != 512*mib {
		t.Errorf("KVBytesPerInference = %d, want %d", profile.KVBytesPerInference, 512*mib)
	}

	plan := assessment.Plan
	if plan.MaxConcurrentInference != 7 {
		t.Errorf("MaxConcurrentInference = %d, want 7", plan.MaxConcurrentInference)
	}
	if plan.MaxConcurrentDistinctModels != 4 {
		t.Errorf("MaxConcurrentDistinctModels = %d, want 4", plan.MaxConcurrentDistinctModels)
	}
	if plan.MaxConcurrentTeams != 1 || plan.MaxTeamExperts != 3 || plan.MaxSwarmWorkers != 7 || plan.MaxMoEExperts != 4 {
		t.Errorf("unexpected topology limits: %+v", plan)
	}
	if plan.Bottleneck != BottleneckCPU {
		t.Errorf("Bottleneck = %q, want %q", plan.Bottleneck, BottleneckCPU)
	}
	if plan.DistinctBottleneck != BottleneckPolicy {
		t.Errorf("DistinctBottleneck = %q, want %q", plan.DistinctBottleneck, BottleneckPolicy)
	}
	if plan.SerialOnly {
		t.Error("SerialOnly = true, want false")
	}
}

func TestBuildProfileAdaptsAutomaticCPUDefaultsOnSmallHosts(t *testing.T) {
	t.Parallel()

	for logicalCPU, wantReserve := range map[int]int{1: 0, 2: 1, 3: 2} {
		logicalCPU, wantReserve := logicalCPU, wantReserve
		t.Run(string(rune('0'+logicalCPU))+" CPU", func(t *testing.T) {
			t.Parallel()
			profile, err := BuildProfile(HostSnapshot{LogicalCPU: logicalCPU}, Input{NumCtx: 8_192})
			if err != nil {
				t.Fatal(err)
			}
			if profile.ReservedLogicalCPU != wantReserve || profile.ThreadsPerInference != 1 {
				t.Fatalf("small-host CPU profile = reserve %d threads %d", profile.ReservedLogicalCPU, profile.ThreadsPerInference)
			}
			plan, err := Plan(profile, Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			if !plan.Admitted || plan.CPULimit != 1 {
				t.Fatalf("small-host plan = %+v", plan)
			}
			if !containsWarning(profile.Warnings, "CPU") {
				t.Fatalf("small-host warnings = %q", profile.Warnings)
			}
		})
	}
}

func TestBuildProfileRejectsImpossibleExplicitCPUSettings(t *testing.T) {
	t.Parallel()

	for name, overrides := range map[string]Overrides{
		"reserve": {ReservedLogicalCPU: 2},
		"threads": {ReservedLogicalCPU: 1, ThreadsPerInference: 2},
	} {
		name, overrides := name, overrides
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := BuildProfile(HostSnapshot{LogicalCPU: 2}, Input{NumCtx: 8_192, Overrides: overrides}); err == nil {
				t.Fatal("BuildProfile() error = nil, want impossible CPU settings rejected")
			}
		})
	}
}

func TestBuildProfileAvailableMemoryDoesNotSubtractResidentModelsAgain(t *testing.T) {
	t.Parallel()

	profile, err := BuildProfile(HostSnapshot{
		LogicalCPU:        8,
		TotalRAMBytes:     16 * gib,
		AvailableRAMBytes: 8 * gib,
	}, Input{
		ActiveModels: []ActiveModel{{Name: "resident", ResidentBytes: 4 * gib}},
		NumCtx:       8_192,
	})
	if err != nil {
		t.Fatalf("BuildProfile() error = %v", err)
	}

	want := 8*gib - 16*gib/5
	if profile.PlanningRAMBytes != want {
		t.Errorf("PlanningRAMBytes = %d, want %d", profile.PlanningRAMBytes, want)
	}
	if profile.ActiveModelBytes != 4*gib {
		t.Errorf("ActiveModelBytes = %d, want %d", profile.ActiveModelBytes, 4*gib)
	}
}

func TestBuildProfileTreatsMeasuredZeroAvailableMemoryAsKnown(t *testing.T) {
	t.Parallel()

	profile, err := BuildProfile(HostSnapshot{
		LogicalCPU: 8, TotalRAMBytes: 2 * gib, AvailableRAMKnown: true,
	}, Input{NumCtx: 8_192})
	if err != nil {
		t.Fatal(err)
	}
	if profile.MemoryConfidence != MemoryAvailable || profile.PlanningRAMBytes != 0 {
		t.Fatalf("known-zero memory profile = %+v", profile)
	}
	plan, err := Plan(profile, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Admitted {
		t.Fatalf("known-zero memory plan admitted work: %+v", plan)
	}
}

func TestBuildProfileAcceptsContextReportedByEitherModelSet(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		resident []ActiveModel
		selected []ActiveModel
	}{
		{name: "resident", resident: []ActiveModel{{Name: "current", NumCtx: 16_384}}},
		{name: "selected", selected: []ActiveModel{{Name: "expert", NumCtx: 16_384}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			profile, err := BuildProfile(HostSnapshot{LogicalCPU: 8}, Input{
				ActiveModels:   test.resident,
				SelectedModels: test.selected,
			})
			if err != nil {
				t.Fatalf("BuildProfile() error = %v", err)
			}
			if profile.EffectiveNumCtx != 16_384 {
				t.Fatalf("EffectiveNumCtx = %d, want 16384", profile.EffectiveNumCtx)
			}
		})
	}
}

func TestBuildProfileAvailableMemoryReservesEverySelectedNonResidentWeight(t *testing.T) {
	t.Parallel()

	profile, err := BuildProfile(HostSnapshot{
		LogicalCPU: 16, TotalRAMBytes: 16 * gib, AvailableRAMBytes: 10 * gib,
	}, Input{
		ActiveModels: []ActiveModel{{Name: "resident:latest", ResidentBytes: 4 * gib}},
		SelectedModels: []ActiveModel{
			{Name: "resident:latest", WeightBytes: 4 * gib},
			{Name: "new-a:latest", WeightBytes: 3 * gib},
			{Name: "new-b:latest"},
		},
		NumCtx: 8_192,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The already-resident model is reflected in AvailableRAMBytes and is not
	// charged twice. Both non-resident selections are reserved once; the unknown
	// model receives its own fallback rather than sharing one global estimate.
	if profile.SelectedModelBytes != 5*gib {
		t.Fatalf("SelectedModelBytes = %d, want %d", profile.SelectedModelBytes, 5*gib)
	}
	wantPlanning := 10*gib - 16*gib/5 - 5*gib
	if profile.PlanningRAMBytes != wantPlanning {
		t.Fatalf("PlanningRAMBytes = %d, want %d", profile.PlanningRAMBytes, wantPlanning)
	}
	if !profile.ModelWeightAssumed {
		t.Fatal("unknown selected weight did not surface the fallback assumption")
	}
}

func TestBuildProfileTotalOnlySubtractsResidentAndSelectedWeights(t *testing.T) {
	t.Parallel()

	profile, err := BuildProfile(HostSnapshot{LogicalCPU: 8, TotalRAMBytes: 16 * gib}, Input{
		ActiveModels:   []ActiveModel{{Name: "current", ResidentBytes: 8 * gib}},
		SelectedModels: []ActiveModel{{Name: "expert", WeightBytes: 4 * gib}},
		NumCtx:         8_192,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := 16*gib - 16*gib/5 - 8*gib - 4*gib
	if profile.PlanningRAMBytes != want {
		t.Fatalf("PlanningRAMBytes = %d, want %d", profile.PlanningRAMBytes, want)
	}
}

func TestBuildProfileTotalOnlyUsesFailSafeCap(t *testing.T) {
	t.Parallel()

	profile, err := BuildProfile(HostSnapshot{
		LogicalCPU:    8,
		TotalRAMBytes: 16 * gib,
	}, Input{
		ActiveModels: []ActiveModel{{Name: "resident", ResidentBytes: 4 * gib}},
		NumCtx:       8_192,
	})
	if err != nil {
		t.Fatalf("BuildProfile() error = %v", err)
	}

	if profile.MemoryConfidence != MemoryTotalOnly {
		t.Errorf("MemoryConfidence = %q, want %q", profile.MemoryConfidence, MemoryTotalOnly)
	}
	if profile.PlanningRAMBytes != 4*gib {
		t.Errorf("PlanningRAMBytes = %d, want fail-safe cap %d", profile.PlanningRAMBytes, 4*gib)
	}
	if !containsWarning(profile.Warnings, "capped total-RAM") {
		t.Errorf("Warnings = %q, want total-only warning", profile.Warnings)
	}
}

func TestBuildProfileNormalizesModelsWithoutMutatingInput(t *testing.T) {
	t.Parallel()

	models := []ActiveModel{
		{Name: "Beta", WeightBytes: 3 * gib, NumCtx: 4_096},
		{Name: " Alpha ", WeightBytes: gib, NumCtx: 8_192},
		{Name: "alpha", ResidentBytes: 2 * gib, NumCtx: 16_384},
	}
	original := append([]ActiveModel(nil), models...)

	profile, err := BuildProfile(HostSnapshot{LogicalCPU: 8, TotalRAMBytes: 16 * gib}, Input{
		ActiveModels: models,
		NumCtx:       2_048,
	})
	if err != nil {
		t.Fatalf("BuildProfile() error = %v", err)
	}
	if !reflect.DeepEqual(models, original) {
		t.Fatalf("BuildProfile() mutated caller models: got %#v want %#v", models, original)
	}
	if len(profile.ActiveModels) != 2 {
		t.Fatalf("len(ActiveModels) = %d, want 2", len(profile.ActiveModels))
	}
	if profile.ActiveModels[0].Name != "Alpha" || profile.ActiveModels[1].Name != "Beta" {
		t.Errorf("ActiveModels order/names = %#v", profile.ActiveModels)
	}
	if profile.ActiveModels[0].WeightBytes != gib || profile.ActiveModels[0].ResidentBytes != 2*gib {
		t.Errorf("merged Alpha = %#v", profile.ActiveModels[0])
	}
	if profile.ActiveModelBytes != 5*gib {
		t.Errorf("ActiveModelBytes = %d, want %d", profile.ActiveModelBytes, 5*gib)
	}
	if profile.LargestModelWeightBytes != 3*gib {
		t.Errorf("LargestModelWeightBytes = %d, want %d", profile.LargestModelWeightBytes, 3*gib)
	}
	if profile.EffectiveNumCtx != 16_384 {
		t.Errorf("EffectiveNumCtx = %d, want 16384", profile.EffectiveNumCtx)
	}
}

func TestBuildProfileAppliesOverrides(t *testing.T) {
	t.Parallel()

	profile, err := BuildProfile(HostSnapshot{
		LogicalCPU:        32,
		TotalRAMBytes:     64 * gib,
		AvailableRAMBytes: 48 * gib,
	}, Input{
		ActiveModels: []ActiveModel{{Name: "model", WeightBytes: 2 * gib}},
		NumCtx:       16_384,
		Overrides: Overrides{
			LogicalCPU:                  12,
			TotalRAMBytes:               16 * gib,
			AvailableRAMBytes:           10 * gib,
			ReservedRAMBytes:            2 * gib,
			ReservedLogicalCPU:          2,
			ThreadsPerInference:         4,
			KVBytesPerToken:             64 * kib,
			PerInferenceOverheadBytes:   512 * mib,
			MaxConcurrentInference:      2,
			MaxConcurrentDistinctModels: 1,
		},
	})
	if err != nil {
		t.Fatalf("BuildProfile() error = %v", err)
	}
	if profile.LogicalCPU != 12 || profile.TotalRAMBytes != 16*gib || profile.AvailableRAMBytes != 10*gib {
		t.Errorf("hardware overrides not applied: %+v", profile)
	}
	if profile.PlanningRAMBytes != 8*gib {
		t.Errorf("PlanningRAMBytes = %d, want %d", profile.PlanningRAMBytes, 8*gib)
	}
	if profile.KVBytesPerInference != gib || profile.SharedInferenceBytes != gib+512*mib {
		t.Errorf("memory tuning not applied: KV=%d shared=%d", profile.KVBytesPerInference, profile.SharedInferenceBytes)
	}

	plan, err := Plan(profile, Overrides{
		MaxConcurrentInference:      2,
		MaxConcurrentDistinctModels: 1,
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.MaxConcurrentInference != 2 || plan.MaxConcurrentDistinctModels != 1 {
		t.Errorf("concurrency overrides not applied: %+v", plan)
	}
}

func TestOverridesValidateRejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		overrides Overrides
	}{
		{name: "negative CPU", overrides: Overrides{LogicalCPU: -1}},
		{name: "negative distinct limit", overrides: Overrides{MaxConcurrentDistinctModels: -1}},
		{name: "available exceeds total", overrides: Overrides{TotalRAMBytes: gib, AvailableRAMBytes: 2 * gib}},
		{name: "RAM reserve below floor", overrides: Overrides{ReservedRAMBytes: minimumRAMReserveBytes - 1}},
		{name: "KV estimate below floor", overrides: Overrides{KVBytesPerToken: minimumKVBytesPerToken - 1}},
		{name: "worker overhead below floor", overrides: Overrides{PerInferenceOverheadBytes: minimumPerInferenceOverheadBytes - 1}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.overrides.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
		})
	}
}

func TestBuildProfileRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot HostSnapshot
		input    Input
	}{
		{name: "negative host value", snapshot: HostSnapshot{LogicalCPU: -1}, input: Input{NumCtx: 1}},
		{name: "CPU unavailable", snapshot: HostSnapshot{}, input: Input{NumCtx: 1}},
		{name: "available exceeds total", snapshot: HostSnapshot{LogicalCPU: 1, TotalRAMBytes: gib, AvailableRAMBytes: 2 * gib}, input: Input{NumCtx: 1}},
		{name: "missing context", snapshot: HostSnapshot{LogicalCPU: 1}},
		{name: "unnamed model", snapshot: HostSnapshot{LogicalCPU: 1}, input: Input{NumCtx: 1, ActiveModels: []ActiveModel{{WeightBytes: gib}}}},
		{name: "negative model value", snapshot: HostSnapshot{LogicalCPU: 1}, input: Input{NumCtx: 1, ActiveModels: []ActiveModel{{Name: "model", WeightBytes: -1}}}},
		{name: "KV overflow", snapshot: HostSnapshot{LogicalCPU: 1}, input: Input{NumCtx: math.MaxInt, Overrides: Overrides{KVBytesPerToken: math.MaxInt64}}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := BuildProfile(test.snapshot, test.input); err == nil {
				t.Fatal("BuildProfile() error = nil, want error")
			}
		})
	}
}

func TestAssessRejectsProbeAndContextFailures(t *testing.T) {
	t.Parallel()

	//nolint:staticcheck // The public boundary must reject a nil context explicitly.
	if _, err := Assess(nil, ProbeFunc(func(context.Context) (HostSnapshot, error) {
		return HostSnapshot{}, nil
	}), Input{}); err == nil {
		t.Fatal("Assess(nil context) error = nil, want error")
	}
	if _, err := Assess(context.Background(), nil, Input{}); err == nil {
		t.Fatal("Assess(nil probe) error = nil, want error")
	}
	if _, err := Assess(context.Background(), ProbeFunc(nil), Input{}); !errors.Is(err, errNilProbeFunc) {
		t.Fatalf("Assess(nil ProbeFunc) error = %v, want %v", err, errNilProbeFunc)
	}

	wantErr := errors.New("probe failed")
	_, err := Assess(context.Background(), ProbeFunc(func(context.Context) (HostSnapshot, error) {
		return HostSnapshot{}, wantErr
	}), Input{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Assess(probe error) error = %v, want errors.Is(%v)", err, wantErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, err = Assess(ctx, ProbeFunc(func(context.Context) (HostSnapshot, error) {
		cancel()
		return HostSnapshot{LogicalCPU: 1}, nil
	}), Input{NumCtx: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Assess(canceled after probe) error = %v, want context.Canceled", err)
	}
}

func containsWarning(warnings []string, substring string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, substring) {
			return true
		}
	}
	return false
}
