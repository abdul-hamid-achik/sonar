// Package resource builds conservative, side-effect-free resource profiles and
// concurrency plans for local expert runtimes.
package resource

import "context"

const (
	DefaultKVBytesPerToken             int64 = 32 << 10
	DefaultPerInferenceOverheadBytes   int64 = 256 << 20
	DefaultUnknownModelWeightBytes     int64 = 2 << 30
	DefaultMinimumRAMReserveBytes      int64 = 2 << 30
	DefaultReservedLogicalCPU                = 2
	DefaultThreadsPerInference               = 2
	DefaultMaxConcurrentInference            = 8
	DefaultMaxConcurrentDistinctModels       = 4
	// The production expert runtime serializes top-level consultations. Keep the
	// reported topology limit aligned with that effective execution contract.
	DefaultMaxConcurrentTeams = 1
	DefaultMaxTeamExperts     = 3
	DefaultMaxSwarmWorkers    = 7
	DefaultMaxMoEExperts      = 4
)

const (
	minimumKVBytesPerToken           int64 = 4 << 10
	minimumPerInferenceOverheadBytes int64 = 64 << 20
	minimumRAMReserveBytes           int64 = 512 << 20
)

// MemoryConfidence identifies which host measurement backs PlanningRAMBytes.
type MemoryConfidence string

const (
	MemoryAvailable MemoryConfidence = "available"
	MemoryTotalOnly MemoryConfidence = "total_only"
	MemoryUnknown   MemoryConfidence = "unknown"
)

// Bottleneck identifies the constraint that determined generic shared-model
// inference concurrency.
type Bottleneck string

const (
	BottleneckCPU           Bottleneck = "cpu"
	BottleneckMemory        Bottleneck = "memory"
	BottleneckBalanced      Bottleneck = "balanced"
	BottleneckPolicy        Bottleneck = "policy"
	BottleneckOverride      Bottleneck = "override"
	BottleneckUnknownMemory Bottleneck = "unknown_memory"
)

// HostSnapshot is a point-in-time host capacity observation. A zero total means
// that measurement was unavailable. A zero available value is unavailable
// unless AvailableRAMKnown records an observed exhausted budget.
type HostSnapshot struct {
	LogicalCPU        int   `json:"logical_cpu" yaml:"logical_cpu"`
	TotalRAMBytes     int64 `json:"total_ram_bytes,omitempty" yaml:"total_ram_bytes,omitempty"`
	AvailableRAMBytes int64 `json:"available_ram_bytes,omitempty" yaml:"available_ram_bytes,omitempty"`
	// AvailableRAMKnown distinguishes a measured zero-byte budget from missing
	// telemetry. Positive AvailableRAMBytes remains implicitly known for custom
	// probes written before this field was introduced.
	AvailableRAMKnown bool `json:"available_ram_known,omitempty" yaml:"available_ram_known,omitempty"`
}

// Probe supplies host capacity without coupling the pure planner to one OS.
type Probe interface {
	Snapshot(context.Context) (HostSnapshot, error)
}

// ProbeFunc adapts a function into a Probe for deterministic tests or custom
// embedders.
type ProbeFunc func(context.Context) (HostSnapshot, error)

func (f ProbeFunc) Snapshot(ctx context.Context) (HostSnapshot, error) {
	if f == nil {
		return HostSnapshot{}, errNilProbeFunc
	}
	return f(ctx)
}

// ActiveModel describes one local model relevant to a resource decision.
// ResidentBytes should use measured runtime residency when available. A
// selected, non-resident model belongs in Input.SelectedModels so its weights
// are reserved before any inference slot is admitted.
type ActiveModel struct {
	Name          string `json:"name" yaml:"name"`
	WeightBytes   int64  `json:"weight_bytes,omitempty" yaml:"weight_bytes,omitempty"`
	ResidentBytes int64  `json:"resident_bytes,omitempty" yaml:"resident_bytes,omitempty"`
	NumCtx        int    `json:"num_ctx,omitempty" yaml:"num_ctx,omitempty"`
}

// Overrides contains explicit configuration inputs and safety caps. Zero means
// automatic. Max fields can only tighten the planner's built-in hard limits;
// they never force concurrency above measured capacity.
type Overrides struct {
	LogicalCPU        int   `json:"logical_cpu,omitempty" yaml:"logical_cpu,omitempty"`
	TotalRAMBytes     int64 `json:"total_ram_bytes,omitempty" yaml:"total_ram_bytes,omitempty"`
	AvailableRAMBytes int64 `json:"available_ram_bytes,omitempty" yaml:"available_ram_bytes,omitempty"`

	ReservedRAMBytes            int64 `json:"reserved_ram_bytes,omitempty" yaml:"reserved_ram_bytes,omitempty"`
	ReservedLogicalCPU          int   `json:"reserved_logical_cpu,omitempty" yaml:"reserved_logical_cpu,omitempty"`
	ThreadsPerInference         int   `json:"threads_per_inference,omitempty" yaml:"threads_per_inference,omitempty"`
	KVBytesPerToken             int64 `json:"kv_bytes_per_token,omitempty" yaml:"kv_bytes_per_token,omitempty"`
	PerInferenceOverheadBytes   int64 `json:"per_inference_overhead_bytes,omitempty" yaml:"per_inference_overhead_bytes,omitempty"`
	MaxConcurrentInference      int   `json:"max_concurrent_inference,omitempty" yaml:"max_concurrent_inference,omitempty"`
	MaxConcurrentDistinctModels int   `json:"max_concurrent_distinct_models,omitempty" yaml:"max_concurrent_distinct_models,omitempty"`
	MaxConcurrentTeams          int   `json:"max_concurrent_teams,omitempty" yaml:"max_concurrent_teams,omitempty"`
	MaxTeamExperts              int   `json:"max_team_experts,omitempty" yaml:"max_team_experts,omitempty"`
	MaxSwarmWorkers             int   `json:"max_swarm_workers,omitempty" yaml:"max_swarm_workers,omitempty"`
	MaxMoEExperts               int   `json:"max_moe_experts,omitempty" yaml:"max_moe_experts,omitempty"`
}

// Input contains model/runtime facts used to construct a ResourceProfile.
// ActiveModels are already resident and therefore already reflected by a live
// AvailableRAMBytes measurement. SelectedModels are the complete set a planned
// operation may load; selected weights not already resident are reserved once
// up front, including when available-memory telemetry exists.
type Input struct {
	ActiveModels   []ActiveModel `json:"active_models,omitempty" yaml:"active_models,omitempty"`
	SelectedModels []ActiveModel `json:"selected_models,omitempty" yaml:"selected_models,omitempty"`
	NumCtx         int           `json:"num_ctx" yaml:"num_ctx"`
	Overrides      Overrides     `json:"overrides,omitempty" yaml:"overrides,omitempty"`
}

// ResourceProfile is the normalized, inspectable input to Plan. Selected model
// weights are reserved before per-inference KV/worker overhead is divided into
// slots, so sequential fan-out cannot accumulate unbudgeted resident weights.
type ResourceProfile struct {
	LogicalCPU        int              `json:"logical_cpu"`
	TotalRAMBytes     int64            `json:"total_ram_bytes,omitempty"`
	AvailableRAMBytes int64            `json:"available_ram_bytes,omitempty"`
	MemoryConfidence  MemoryConfidence `json:"memory_confidence"`

	ActiveModels            []ActiveModel `json:"active_models,omitempty"`
	SelectedModels          []ActiveModel `json:"selected_models,omitempty"`
	ActiveModelBytes        int64         `json:"active_model_bytes,omitempty"`
	SelectedModelBytes      int64         `json:"selected_model_bytes,omitempty"`
	LargestModelWeightBytes int64         `json:"largest_model_weight_bytes"`
	ModelWeightAssumed      bool          `json:"model_weight_assumed"`
	EffectiveNumCtx         int           `json:"effective_num_ctx"`

	ReservedRAMBytes          int64    `json:"reserved_ram_bytes"`
	ReservedLogicalCPU        int      `json:"reserved_logical_cpu"`
	ThreadsPerInference       int      `json:"threads_per_inference"`
	KVBytesPerToken           int64    `json:"kv_bytes_per_token"`
	KVBytesPerInference       int64    `json:"kv_bytes_per_inference"`
	PerInferenceOverheadBytes int64    `json:"per_inference_overhead_bytes"`
	SharedInferenceBytes      int64    `json:"shared_inference_bytes"`
	DistinctInferenceBytes    int64    `json:"distinct_inference_bytes"`
	PlanningRAMBytes          int64    `json:"planning_ram_bytes"`
	Warnings                  []string `json:"warnings,omitempty"`
}

// ConcurrencyPlan limits future topologies without launching any work.
//
// MaxConcurrentInference and MaxConcurrentDistinctModels limit simultaneous
// generations. MaxTeamExperts, MaxSwarmWorkers, and MaxMoEExperts instead cap
// total logical fan-out: a runtime may schedule that many experts sequentially
// even when SerialOnly is true. The parent coordinator is not charged as a
// concurrent generation while it waits for an expert tool to complete.
// MaxConcurrentTeams reflects the production runtime's serialized top-level
// consultation gate and therefore never exceeds one.
type ConcurrencyPlan struct {
	Admitted                    bool       `json:"admitted"`
	MaxConcurrentInference      int        `json:"max_concurrent_inference"`
	MaxConcurrentDistinctModels int        `json:"max_concurrent_distinct_models"`
	MaxConcurrentTeams          int        `json:"max_concurrent_teams"`
	MaxTeamExperts              int        `json:"max_team_experts"`
	MaxSwarmWorkers             int        `json:"max_swarm_workers"`
	MaxMoEExperts               int        `json:"max_moe_experts"`
	CPULimit                    int        `json:"cpu_limit"`
	SharedMemoryLimit           int        `json:"shared_memory_limit"`
	DistinctMemoryLimit         int        `json:"distinct_memory_limit"`
	Bottleneck                  Bottleneck `json:"bottleneck"`
	DistinctBottleneck          Bottleneck `json:"distinct_bottleneck"`
	SerialOnly                  bool       `json:"serial_only"`
}

// Assessment keeps the normalized profile and plan together for callers that
// want one probe-to-plan operation.
type Assessment struct {
	Profile ResourceProfile `json:"profile"`
	Plan    ConcurrencyPlan `json:"plan"`
}
