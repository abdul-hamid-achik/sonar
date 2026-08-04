// Package expertteam runs bounded, read-only application-level expert
// consultations. It is intentionally not a token-level model MoE: sonar
// selects ordinary agent profiles, asks them independently without tools, and
// returns their advisory reports to the parent agent for synthesis.
package expertteam

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/expertselector"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/resource"
)

const (
	DefaultMaxEvalTokens  = 768
	DefaultExpertTimeout  = 90 * time.Second
	DefaultMaxReportBytes = 12 * 1024
	DefaultMaxResultBytes = 40 * 1024
	expertCleanupTimeout  = 5 * time.Second

	maxObjectiveBytes    = 32 * 1024
	maxSystemPromptBytes = 16 * 1024
	maxExpertNames       = expertselector.MaxSelectedExperts
	maxExpertNameBytes   = 128
	maxModelNameBytes    = 256
)

var (
	ErrUnavailable      = errors.New("expert consultation is unavailable")
	ErrInvalidRequest   = errors.New("invalid expert consultation request")
	ErrAllExpertsFailed = errors.New("all selected experts failed")
	errEvalBudget       = errors.New("expert evaluation budget exceeded")
)

// ModelRunner is the narrow provider surface required by the runtime. The
// concrete ModelManager retains local-only/cloud-consent and model-admission
// authority; the expert runtime never bypasses it.
type ModelRunner interface {
	ChatStreamForModel(context.Context, string, llm.ChatOptions, func(llm.StreamChunk) error) error
	CurrentModel() string
	EffectiveContext(string) (int, bool)
	PrepareExpertModels(context.Context, []string) (llm.ExpertModelSnapshot, error)
	ReleaseExpertModels(context.Context, llm.ExpertModelSnapshot) error
}

// Profile is one advisory expert. Skills and MCP scopes are intentionally not
// represented because an expert consultation has no tool execution authority.
type Profile struct {
	Name         string
	Description  string
	UseCases     []string
	Model        string
	SystemPrompt string
}

// Options configures host-owned limits. Zero values select conservative
// defaults. ModelWeights is a fallback for custom runners that cannot provide
// live model weights; production verified-local snapshots fail closed instead.
type Options struct {
	Profiles          []Profile
	Probe             resource.Probe
	ResourceOverrides resource.Overrides
	ModelWeights      map[string]int64
	DefaultNumCtx     int
	MaxEvalTokens     int
	ExpertTimeout     time.Duration
	MaxReportBytes    int
	MaxResultBytes    int
}

// Request describes one parent-owned consultation. ExpertNames are exact
// profile names; an empty list uses the strategy's deterministic selector.
type Request struct {
	Strategy    expertselector.Strategy
	Objective   string
	ExpertNames []string
	// Model is an optional exact default model for every selected expert. It is
	// routing intent only: the ModelRunner still owns inventory, cloud consent,
	// context, residency, and concurrency admission.
	Model string
	// ModelOverrides apply exact per-profile model choices. They are normalized
	// and bounded host-owned values, never an arbitrary downstream map.
	ModelOverrides []ModelOverride
	// MaxTotalEvalTokens is a host-owned cap shared by every selected expert.
	// Zero retains the runtime's ordinary per-expert caps. This field is never
	// accepted from model tool arguments.
	MaxTotalEvalTokens int
}

// ModelOverride assigns one exact model to one exact selected expert.
type ModelOverride struct {
	Expert string
	Model  string
}

type ExpertStatus string

const (
	ExpertCompleted ExpertStatus = "completed"
	ExpertFailed    ExpertStatus = "failed"
)

// ProgressPhase is a host-authored lifecycle state for one bounded expert
// consultation. It deliberately excludes provider text and private reasoning.
type ProgressPhase string

const (
	ProgressPlanned   ProgressPhase = "planned"
	ProgressStarted   ProgressPhase = "started"
	ProgressCompleted ProgressPhase = "completed"
	ProgressFailed    ProgressPhase = "failed"
)

// ProgressEvent is the complete persistence-safe progress projection. The
// Agent adds tool-call correlation when forwarding it; the runtime never sees
// or stores a call ID. No objective, report, prompt, path, raw provider error,
// or reasoning content is represented here.
type ProgressEvent struct {
	Sequence    uint64
	Phase       ProgressPhase
	Strategy    expertselector.Strategy
	Total       int
	Parallelism int
	Running     int
	Queued      int
	Completed   int
	Failed      int
	ExpertIndex int
	Expert      string
	Model       string
	Location    llm.OllamaModelLocation
	Status      ExpertStatus
	ErrorCode   string
	EvalTokens  int
}

// Observer receives monotonic, bounded progress events synchronously from the
// consultation scheduler. Callers should return promptly.
type Observer func(ProgressEvent)

// ExpertReceipt is a bounded advisory result. ErrorCode is host-authored and
// never contains raw provider errors, prompts, paths, or tool output.
type ExpertReceipt struct {
	Name  string
	Model string
	// Location is populated only from a verified live model inventory. Unknown
	// never implies local execution.
	Location         llm.OllamaModelLocation
	Reason           string
	Score            int
	Status           ExpertStatus
	ErrorCode        string
	Report           string
	EvalTokens       int
	PromptEvalTokens int
	// ChargedEvalTokens is either the trusted terminal usage or the conservative
	// reservation charged when a dispatched stream has no trustworthy terminal
	// receipt. UsageEstimated distinguishes those cases for inspection.
	ChargedEvalTokens int
	UsageEstimated    bool
}

// Usage is the aggregate provider usage charged to the parent turn.
type Usage struct {
	EvalTokens       int
	PromptEvalTokens int
}

// Result keeps the resource decision inspectable without claiming that any
// report is verified evidence.
type Result struct {
	Strategy    expertselector.Strategy
	Parallelism int
	Plan        resource.ConcurrencyPlan
	Warnings    []string
	Experts     []ExpertReceipt
	ResultLimit int
}

// Runtime owns immutable configuration and serializes top-level consultations.
// One Agent runs one turn at a time, so this gate mainly protects embedders
// from accidentally starting overlapping teams against independently computed
// memory snapshots. Experts inside one consultation still run concurrently up
// to the resource plan.
type Runtime struct {
	runner ModelRunner
	opts   Options
	gate   chan struct{}
}

func New(runner ModelRunner, options Options) (*Runtime, error) {
	if runner == nil {
		return nil, fmt.Errorf("%w: model runner is required", ErrUnavailable)
	}
	if options.Probe == nil {
		options.Probe = resource.SystemProbe{}
	}
	if options.DefaultNumCtx <= 0 {
		return nil, fmt.Errorf("%w: a positive default context is required", ErrUnavailable)
	}
	if options.MaxEvalTokens == 0 {
		options.MaxEvalTokens = DefaultMaxEvalTokens
	}
	if options.ExpertTimeout == 0 {
		options.ExpertTimeout = DefaultExpertTimeout
	}
	if options.MaxReportBytes == 0 {
		options.MaxReportBytes = DefaultMaxReportBytes
	}
	if options.MaxResultBytes == 0 {
		options.MaxResultBytes = DefaultMaxResultBytes
	}
	if options.MaxEvalTokens < 1 || options.ExpertTimeout < time.Second ||
		options.MaxReportBytes < 256 || options.MaxResultBytes < 1024 {
		return nil, fmt.Errorf("%w: expert limits are invalid", ErrUnavailable)
	}
	if err := options.ResourceOverrides.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	profiles, err := cloneAndValidateProfiles(options.Profiles)
	if err != nil {
		return nil, err
	}
	options.Profiles = profiles
	options.ModelWeights = cloneModelWeights(options.ModelWeights)
	return &Runtime{runner: runner, opts: options, gate: make(chan struct{}, 1)}, nil
}

// ProfileCount reports the immutable selectable catalog size, including
// built-ins not shadowed by configured profiles. It exposes no prompts, model
// names, or consultation content.
func (r *Runtime) ProfileCount() int {
	if r == nil {
		return 0
	}
	return len(mergeProfiles(r.opts.Profiles, builtinProfiles()))
}

// ValidateRequest performs the complete pure catalog/selector validation used
// by Consult without reserving models, probing host resources, or entering the
// provider. Hosts can use it at their tool preflight boundary so an invented
// exact profile name does not consume a consultation dispatch.
func (r *Runtime) ValidateRequest(ctx context.Context, request Request) error {
	if r == nil || r.runner == nil {
		return ErrUnavailable
	}
	_, _, _, err := r.selectRequest(ctx, request)
	return err
}

// Consult selects experts, takes one current host resource snapshot, and runs
// the selected reports with common cancellation. It returns partial receipts
// when at least one expert completes.
func (r *Runtime) Consult(ctx context.Context, request Request) (Result, error) {
	return r.ConsultWithProgress(ctx, request, nil)
}

// ConsultWithProgress is Consult plus an optional host-owned progress sink.
// Consult remains the stable compatibility surface for existing embedders.
func (r *Runtime) ConsultWithProgress(ctx context.Context, request Request, observer Observer) (Result, error) {
	if r == nil || r.runner == nil {
		return Result{}, ErrUnavailable
	}
	selections, byName, overrides, err := r.selectRequest(ctx, request)
	if err != nil {
		return Result{}, err
	}
	select {
	case r.gate <- struct{}{}:
		defer func() { <-r.gate }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	currentModel := strings.TrimSpace(r.runner.CurrentModel())
	if currentModel == "" {
		return Result{}, fmt.Errorf("%w: no current model", ErrUnavailable)
	}
	selected := make([]selectedExpert, 0, len(selections))
	for _, selection := range selections {
		profile, ok := byName[profileKey(selection.Profile.Name)]
		if !ok {
			return Result{}, fmt.Errorf("%w: selected profile disappeared", ErrUnavailable)
		}
		model := selectedModel(request, profile, overrides, currentModel)
		selected = append(selected, selectedExpert{
			profile: profile, model: model, location: llm.OllamaModelLocationUnknown,
			reason: selection.Reason, score: selection.Score,
		})
	}
	modelSnapshot, err := r.runner.PrepareExpertModels(ctx, selectedModelNames(selected))
	if err != nil {
		return Result{}, fmt.Errorf("%w: live model snapshot failed: %v", ErrUnavailable, err)
	}
	leaseReleased := false
	releaseLease := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), expertCleanupTimeout)
		defer cancel()
		return r.runner.ReleaseExpertModels(cleanupCtx, modelSnapshot)
	}
	defer func() {
		if !leaseReleased {
			_ = releaseLease()
		}
	}()
	selected = applyVerifiedLocations(modelSnapshot, selected)
	hostSnapshot, err := r.opts.Probe.Snapshot(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("%w: resource assessment failed: %v", ErrUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	selected, assessment, fanoutReduced, err := r.admitSelected(ctx, hostSnapshot, modelSnapshot, request.Strategy, selected)
	if err != nil {
		return Result{}, fmt.Errorf("%w: resource assessment failed: %v", ErrUnavailable, err)
	}
	if len(selected) == 0 {
		return Result{}, fmt.Errorf("%w: host resource policy denied expert inference", ErrUnavailable)
	}
	// Runtime.gate serializes complete consultations, so the inspectable plan
	// must report the effective topology rather than the planner's generic host
	// capacity for independently scheduled teams.
	assessment.Plan.MaxConcurrentTeams = 1
	parallelism := assessment.Plan.MaxConcurrentInference
	distinctParallelism := assessment.Plan.MaxConcurrentDistinctModels
	if !assessment.Plan.Admitted || parallelism < 1 || distinctParallelism < 1 {
		return Result{}, fmt.Errorf("%w: host resource policy denied expert inference", ErrUnavailable)
	}
	parallelism = max(1, min(parallelism, len(selected)))
	distinctParallelism = max(1, min(distinctParallelism, distinctModelCount(selected)))
	evalLimits := allocateEvalLimits(len(selected), request.MaxTotalEvalTokens, r.opts.MaxEvalTokens)
	progress := newProgressEmitter(observer, request.Strategy, selected, parallelism)
	progress.planned()

	receipts, completed, err := r.runSelected(ctx, request.Objective, selected, evalLimits, parallelism, distinctParallelism, assessment.Profile.ThreadsPerInference, progress)
	warnings := append([]string(nil), assessment.Profile.Warnings...)
	if fanoutReduced {
		warnings = append(warnings, "expert fan-out was reduced to fit the current host resource budget")
	}
	result := Result{
		Strategy: request.Strategy, Parallelism: parallelism, Plan: assessment.Plan,
		Warnings: warnings,
		Experts:  receipts, ResultLimit: r.opts.MaxResultBytes,
	}
	cleanupErr := releaseLease()
	leaseReleased = true
	if cleanupErr != nil {
		result.Warnings = append(result.Warnings, "one or more temporary expert models could not be unloaded")
	}
	if err != nil {
		return result, err
	}
	if completed == 0 {
		return result, ErrAllExpertsFailed
	}
	return result, nil
}

func (r *Runtime) selectRequest(ctx context.Context, request Request) ([]expertselector.Selection, map[string]Profile, map[string]string, error) {
	if ctx == nil {
		return nil, nil, nil, fmt.Errorf("%w: context is required", ErrInvalidRequest)
	}
	if err := validateRequest(request); err != nil {
		return nil, nil, nil, err
	}
	profiles := mergeProfiles(r.opts.Profiles, builtinProfiles())
	selectionProfiles := make([]expertselector.Profile, 0, len(profiles))
	byName := make(map[string]Profile, len(profiles))
	for _, profile := range profiles {
		selectionProfiles = append(selectionProfiles, expertselector.Profile{
			Name: profile.Name, Description: profile.Description,
			UseCases: append([]string(nil), profile.UseCases...), Model: profile.Model,
		})
		byName[profileKey(profile.Name)] = profile
	}

	explicit := append([]string(nil), request.ExpertNames...)
	if request.Strategy == expertselector.StrategyMoE && len(explicit) == 0 {
		// In MoE, explicit names are used only as a no-match fallback. Keeping a
		// built-in generalist here makes arbitrary tasks degrade usefully without
		// displacing profiles that do have positive bounded lexical matches.
		explicit = []string{"generalist"}
	}
	selections, err := expertselector.Select(ctx, expertselector.Request{
		Strategy: request.Strategy,
		Prompt:   request.Objective,
		Profiles: selectionProfiles,
		Options: expertselector.Options{
			ExplicitNames: explicit,
			MaxExperts:    logicalFanout(request.Strategy, r.opts.ResourceOverrides),
		},
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if request.MaxTotalEvalTokens > 0 && len(selections) > request.MaxTotalEvalTokens {
		// Every dispatched expert needs at least one evaluation token. Prefer the
		// selector's deterministic ordering when the remaining parent budget cannot
		// admit the full logical fanout. Model overrides are validated against this
		// final selected set, so a dropped expert cannot retain a hidden assignment.
		selections = selections[:request.MaxTotalEvalTokens]
	}

	overrides, err := validatedModelOverrides(request, byName, selections)
	if err != nil {
		return nil, nil, nil, err
	}
	return selections, byName, overrides, nil
}

func selectedModel(request Request, profile Profile, overrides map[string]string, current string) string {
	if model := overrides[profileKey(profile.Name)]; model != "" {
		return model
	}
	if request.Model != "" {
		return request.Model
	}
	if profile.Model != "" {
		return profile.Model
	}
	return current
}

func validatedModelOverrides(request Request, profiles map[string]Profile, selections []expertselector.Selection) (map[string]string, error) {
	if len(request.ModelOverrides) == 0 {
		return nil, nil
	}
	selected := make(map[string]struct{}, len(selections))
	for _, selection := range selections {
		selected[profileKey(selection.Profile.Name)] = struct{}{}
	}
	overrides := make(map[string]string, len(request.ModelOverrides))
	for _, override := range request.ModelOverrides {
		key := profileKey(override.Expert)
		if _, exists := profiles[key]; !exists {
			return nil, fmt.Errorf("%w: model override names an unknown expert", ErrInvalidRequest)
		}
		if _, exists := selected[key]; !exists {
			return nil, fmt.Errorf("%w: model override names an unselected expert", ErrInvalidRequest)
		}
		overrides[key] = override.Model
	}
	return overrides, nil
}

type selectedExpert struct {
	profile  Profile
	model    string
	location llm.OllamaModelLocation
	reason   string
	score    int
}

// admitSelected greedily preserves selector order while skipping candidates
// whose complete local model set cannot be reserved on the one host snapshot.
// Every accepted step is reassessed with all accepted model weights, so a
// sequential fan-out cannot accumulate models that were never budgeted.
func (r *Runtime) admitSelected(ctx context.Context, host resource.HostSnapshot, snapshot llm.ExpertModelSnapshot, strategy expertselector.Strategy, selected []selectedExpert) ([]selectedExpert, resource.Assessment, bool, error) {
	admitted := make([]selectedExpert, 0, len(selected))
	var assessment resource.Assessment
	var firstAssessmentErr error
	for _, candidate := range selected {
		if err := ctx.Err(); err != nil {
			return nil, resource.Assessment{}, false, err
		}
		proposed := make([]selectedExpert, len(admitted)+1)
		copy(proposed, admitted)
		proposed[len(admitted)] = candidate
		current, err := r.assessSelected(host, snapshot, proposed)
		if err != nil {
			if firstAssessmentErr == nil {
				firstAssessmentErr = err
			}
			continue
		}
		if !current.Plan.Admitted || current.Plan.MaxConcurrentInference < 1 ||
			current.Plan.MaxConcurrentDistinctModels < 1 || len(proposed) > planFanout(strategy, current.Plan) {
			continue
		}
		admitted = proposed
		assessment = current
	}
	if len(admitted) == 0 && firstAssessmentErr != nil {
		return nil, resource.Assessment{}, false, firstAssessmentErr
	}
	return admitted, assessment, len(admitted) != len(selected), nil
}

func (r *Runtime) assessSelected(host resource.HostSnapshot, snapshot llm.ExpertModelSnapshot, selected []selectedExpert) (resource.Assessment, error) {
	if allSelectedExternal(selected) {
		return r.assessExternalSelected(host)
	}
	residentModels, selectedModels, err := r.resourceModels(snapshot, selected)
	if err != nil {
		return resource.Assessment{}, err
	}
	input := resource.Input{
		ActiveModels: residentModels, SelectedModels: selectedModels,
		NumCtx: r.opts.DefaultNumCtx, Overrides: r.opts.ResourceOverrides,
	}
	profile, err := resource.BuildProfile(host, input)
	if err != nil {
		return resource.Assessment{}, err
	}
	plan, err := resource.Plan(profile, input.Overrides)
	if err != nil {
		return resource.Assessment{}, err
	}
	return resource.Assessment{Profile: profile, Plan: plan}, nil
}

// assessExternalSelected deliberately excludes local RAM facts. A verified
// cloud/remote-only set does not load local model weights, but remains serial
// because remote provider capacity is not inferred from host telemetry.
func (r *Runtime) assessExternalSelected(host resource.HostSnapshot) (resource.Assessment, error) {
	overrides := r.opts.ResourceOverrides
	overrides.TotalRAMBytes = 0
	overrides.AvailableRAMBytes = 0
	overrides.ReservedRAMBytes = 0
	overrides.KVBytesPerToken = 0
	overrides.PerInferenceOverheadBytes = 0
	// A positive synthetic context satisfies the generic pure planner without
	// importing any local KV-cache estimate into an external-only decision.
	input := resource.Input{NumCtx: 1, Overrides: overrides}
	profile, err := resource.BuildProfile(resource.HostSnapshot{LogicalCPU: host.LogicalCPU}, input)
	if err != nil {
		return resource.Assessment{}, err
	}
	profile.Warnings = []string{"external expert inference does not consume local model weights; concurrency is restricted to serial inference"}
	plan, err := resource.Plan(profile, overrides)
	if err != nil {
		return resource.Assessment{}, err
	}
	return resource.Assessment{Profile: profile, Plan: plan}, nil
}

type expertOutcome struct {
	index   int
	receipt ExpertReceipt
}

type progressEmitter struct {
	observer    Observer
	sequence    uint64
	strategy    expertselector.Strategy
	selected    []selectedExpert
	parallelism int
	running     int
	queued      int
	completed   int
	failed      int
}

func newProgressEmitter(observer Observer, strategy expertselector.Strategy, selected []selectedExpert, parallelism int) *progressEmitter {
	return &progressEmitter{
		observer: observer, strategy: strategy, selected: append([]selectedExpert(nil), selected...),
		parallelism: parallelism, queued: len(selected),
	}
}

func (emitter *progressEmitter) planned() {
	if emitter == nil {
		return
	}
	emitter.emit(ProgressEvent{Phase: ProgressPlanned, ExpertIndex: -1})
}

func (emitter *progressEmitter) started(index int) {
	if emitter == nil {
		return
	}
	emitter.queued = max(0, emitter.queued-1)
	emitter.running++
	emitter.emit(emitter.expertEvent(index, ProgressStarted, ExpertReceipt{}))
}

func (emitter *progressEmitter) settled(index int, receipt ExpertReceipt) {
	if emitter == nil {
		return
	}
	emitter.running = max(0, emitter.running-1)
	phase := ProgressFailed
	if receipt.Status == ExpertCompleted {
		emitter.completed++
		phase = ProgressCompleted
	} else {
		emitter.failed++
	}
	emitter.emit(emitter.expertEvent(index, phase, receipt))
}

func (emitter *progressEmitter) failedBeforeStart(index int, receipt ExpertReceipt) {
	if emitter == nil {
		return
	}
	emitter.queued = max(0, emitter.queued-1)
	emitter.failed++
	emitter.emit(emitter.expertEvent(index, ProgressFailed, receipt))
}

func (emitter *progressEmitter) expertEvent(index int, phase ProgressPhase, receipt ExpertReceipt) ProgressEvent {
	event := ProgressEvent{Phase: phase, ExpertIndex: index}
	if index >= 0 && index < len(emitter.selected) {
		selected := emitter.selected[index]
		event.Expert = selected.profile.Name
		event.Model = selected.model
		event.Location = selected.location
	}
	event.Status = receipt.Status
	event.ErrorCode = receipt.ErrorCode
	event.EvalTokens = receipt.ChargedEvalTokens
	return event
}

func (emitter *progressEmitter) emit(event ProgressEvent) {
	if emitter == nil || emitter.observer == nil {
		return
	}
	emitter.sequence++
	event.Sequence = emitter.sequence
	event.Strategy = emitter.strategy
	event.Total = len(emitter.selected)
	event.Parallelism = emitter.parallelism
	event.Running = emitter.running
	event.Queued = emitter.queued
	event.Completed = emitter.completed
	event.Failed = emitter.failed
	emitter.observer(event)
}

func (r *Runtime) runSelected(ctx context.Context, objective string, selected []selectedExpert, evalLimits []int, parallelism, distinctParallelism, numThread int, progress *progressEmitter) ([]ExpertReceipt, int, error) {
	receipts := make([]ExpertReceipt, len(selected))
	parallelism = max(1, min(parallelism, len(selected)))
	distinctParallelism = max(1, min(distinctParallelism, distinctModelCount(selected)))
	outcomes := make(chan expertOutcome, len(selected))
	pending := make([]int, len(selected))
	for index := range pending {
		pending[index] = index
	}
	active := 0
	activeModels := make(map[string]int, distinctParallelism)
	cancelPending := func(err error) {
		for _, index := range pending {
			receipts[index] = failedReceipt(selected[index], errorCode(err))
			progress.failedBeforeStart(index, receipts[index])
		}
		pending = nil
	}
	for len(pending) > 0 || active > 0 {
		if err := ctx.Err(); err != nil && len(pending) > 0 {
			cancelPending(err)
		}
		for ctx.Err() == nil && active < parallelism {
			position := nextSchedulable(selected, pending, activeModels, distinctParallelism)
			if position < 0 {
				break
			}
			index := pending[position]
			copy(pending[position:], pending[position+1:])
			pending = pending[:len(pending)-1]
			modelKey := canonicalModelKey(selected[index].model)
			active++
			activeModels[modelKey]++
			progress.started(index)
			go func() {
				outcomes <- expertOutcome{
					index:   index,
					receipt: r.runExpert(ctx, objective, selected[index], evalLimits[index], numThread),
				}
			}()
		}
		if active == 0 {
			// The caller validates positive limits, so this is defensive only.
			if len(pending) > 0 {
				cancelPending(ErrUnavailable)
			}
			break
		}
		var outcome expertOutcome
		if ctx.Err() != nil {
			outcome = <-outcomes
		} else {
			select {
			case outcome = <-outcomes:
			case <-ctx.Done():
				continue
			}
		}
		receipts[outcome.index] = outcome.receipt
		active--
		progress.settled(outcome.index, outcome.receipt)
		modelKey := canonicalModelKey(selected[outcome.index].model)
		activeModels[modelKey]--
		if activeModels[modelKey] == 0 {
			delete(activeModels, modelKey)
		}
	}
	completed := 0
	for _, receipt := range receipts {
		if receipt.Status == ExpertCompleted {
			completed++
		}
	}
	if err := ctx.Err(); err != nil {
		return receipts, completed, err
	}
	return receipts, completed, nil
}

func nextSchedulable(selected []selectedExpert, pending []int, activeModels map[string]int, distinctParallelism int) int {
	for position, index := range pending {
		modelKey := canonicalModelKey(selected[index].model)
		if activeModels[modelKey] > 0 || len(activeModels) < distinctParallelism {
			return position
		}
	}
	return -1
}

func (r *Runtime) runExpert(parent context.Context, objective string, selected selectedExpert, maxEvalTokens, numThread int) ExpertReceipt {
	ctx, cancel := context.WithTimeout(parent, r.opts.ExpertTimeout)
	defer cancel()
	collector := newBoundedCollector(r.opts.MaxReportBytes)
	receipt := ExpertReceipt{
		Name: selected.profile.Name, Model: selected.model, Location: selected.location,
		Reason: selected.reason, Score: selected.score, Status: ExpertCompleted,
	}
	doneSeen := false
	callbackSeen := false
	expected, _ := r.runner.EffectiveContext(selected.model)
	err := r.runner.ChatStreamForModel(ctx, selected.model, llm.ChatOptions{
		System:   expertSystemPrompt(selected.profile),
		Messages: []llm.Message{{Role: "user", Content: objective}},
		Tools:    nil, MaxEvalTokens: maxEvalTokens, DisableReasoning: true,
		NumThread: numThread, ExpectedContext: expected,
	}, func(chunk llm.StreamChunk) error {
		callbackSeen = true
		if err := ctx.Err(); err != nil {
			return err
		}
		if doneSeen {
			return errors.New("expert streamed data after its terminal usage receipt")
		}
		collector.Append(chunk.Text)
		if len(chunk.ToolCalls) > 0 {
			return errors.New("expert attempted an unavailable tool")
		}
		if chunk.Done {
			doneSeen = true
			if chunk.EvalCount < 0 || chunk.PromptEvalCount < 0 {
				return errors.New("expert returned an invalid terminal usage receipt")
			}
			receipt.EvalTokens = chunk.EvalCount
			receipt.PromptEvalTokens = chunk.PromptEvalCount
			if chunk.EvalCount > maxEvalTokens {
				return errEvalBudget
			}
		}
		return nil
	})
	if err != nil {
		failed := failedReceipt(selected, errorCode(err))
		failed.EvalTokens = receipt.EvalTokens
		failed.PromptEvalTokens = receipt.PromptEvalTokens
		inferenceNotStarted := !callbackSeen && (errors.Is(err, llm.ErrInferenceNotStarted) || errors.Is(err, llm.ErrNoModelSelected))
		if !inferenceNotStarted {
			failed.ChargedEvalTokens = max(maxEvalTokens, receipt.EvalTokens)
			failed.UsageEstimated = !errors.Is(err, errEvalBudget)
		}
		return failed
	}
	if !doneSeen {
		failed := failedReceipt(selected, "missing_usage_receipt")
		failed.ChargedEvalTokens = maxEvalTokens
		failed.UsageEstimated = true
		return failed
	}
	receipt.ChargedEvalTokens = receipt.EvalTokens
	if receipt.ChargedEvalTokens == 0 {
		receipt.ChargedEvalTokens = maxEvalTokens
		receipt.UsageEstimated = true
	}
	receipt.Report = strings.TrimSpace(collector.String())
	if receipt.Report == "" {
		// Provider-native reasoning is never a report and is intentionally not
		// collected. Even when a provider ignores DisableReasoning, fail with a
		// host-owned code instead of exposing or persisting private reasoning.
		failed := failedReceipt(selected, "no_visible_report")
		failed.EvalTokens = receipt.EvalTokens
		failed.PromptEvalTokens = receipt.PromptEvalTokens
		failed.ChargedEvalTokens = receipt.ChargedEvalTokens
		failed.UsageEstimated = receipt.UsageEstimated
		return failed
	}
	return receipt
}

func failedReceipt(selected selectedExpert, code string) ExpertReceipt {
	return ExpertReceipt{
		Name: selected.profile.Name, Model: selected.model, Location: selected.location,
		Reason: selected.reason, Score: selected.score,
		Status: ExpertFailed, ErrorCode: code,
	}
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed_out"
	case errors.Is(err, llm.ErrNoModelSelected):
		return "model_unavailable"
	case errors.Is(err, errEvalBudget):
		return "budget_exceeded"
	default:
		return "inference_failed"
	}
}

func selectedModelNames(selected []selectedExpert) []string {
	models := make([]string, 0, len(selected))
	seen := make(map[string]struct{}, len(selected))
	for _, expert := range selected {
		key := canonicalModelKey(expert.model)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, expert.model)
	}
	return models
}

func applyVerifiedLocations(snapshot llm.ExpertModelSnapshot, selected []selectedExpert) []selectedExpert {
	if !snapshot.InventoryVerified {
		return selected
	}
	locations := make(map[string]llm.OllamaModelLocation, len(snapshot.Models))
	conflicts := make(map[string]struct{})
	for _, model := range snapshot.Models {
		if !model.Selected || !knownModelLocation(model.Location) {
			continue
		}
		key := canonicalModelKey(model.Name)
		if existing, ok := locations[key]; ok && existing != model.Location {
			delete(locations, key)
			conflicts[key] = struct{}{}
			continue
		}
		if _, conflicted := conflicts[key]; !conflicted {
			locations[key] = model.Location
		}
	}
	result := append([]selectedExpert(nil), selected...)
	for index := range result {
		location, ok := locations[canonicalModelKey(result[index].model)]
		if !ok {
			location = llm.OllamaModelLocationUnknown
		}
		result[index].location = location
	}
	return result
}

func knownModelLocation(location llm.OllamaModelLocation) bool {
	return location == llm.OllamaModelLocationLocal ||
		location == llm.OllamaModelLocationCloud ||
		location == llm.OllamaModelLocationRemote
}

func allSelectedExternal(selected []selectedExpert) bool {
	if len(selected) == 0 {
		return false
	}
	for _, expert := range selected {
		if expert.location != llm.OllamaModelLocationCloud && expert.location != llm.OllamaModelLocationRemote {
			return false
		}
	}
	return true
}

func (r *Runtime) resourceModels(snapshot llm.ExpertModelSnapshot, selected []selectedExpert) ([]resource.ActiveModel, []resource.ActiveModel, error) {
	live := make(map[string]llm.ExpertModelResource, len(snapshot.Models))
	resident := make(map[string]resource.ActiveModel, len(snapshot.Models))
	for _, model := range snapshot.Models {
		key := canonicalModelKey(model.Name)
		live[key] = model
		if !model.Resident || !usesLocalInference(model.Location) {
			continue
		}
		numCtx := max(r.opts.DefaultNumCtx, model.ContextLength)
		resident[key] = resource.ActiveModel{
			Name: key, WeightBytes: model.WeightBytes,
			ResidentBytes: model.ResidentBytes, NumCtx: numCtx,
		}
	}

	planned := make(map[string]resource.ActiveModel, len(selected))
	for _, expert := range selected {
		key := canonicalModelKey(expert.model)
		if _, exists := planned[key]; exists {
			continue
		}
		model, found := live[key]
		if found && !usesLocalInference(model.Location) {
			continue
		}
		weight := model.WeightBytes
		if weight <= 0 {
			weight = r.opts.ModelWeights[key]
		}
		if snapshot.InventoryVerified && weight <= 0 {
			return nil, nil, fmt.Errorf("%w: live local weight is unavailable for selected model %q", ErrUnavailable, expert.model)
		}
		numCtx := r.opts.DefaultNumCtx
		if effective, ok := r.runner.EffectiveContext(expert.model); ok && effective > numCtx {
			numCtx = effective
		}
		if model.ContextLength > numCtx {
			numCtx = model.ContextLength
		}
		planned[key] = resource.ActiveModel{Name: key, WeightBytes: weight, NumCtx: numCtx}
	}

	residentResult := make([]resource.ActiveModel, 0, len(resident))
	for _, model := range resident {
		residentResult = append(residentResult, model)
	}
	plannedResult := make([]resource.ActiveModel, 0, len(planned))
	for _, model := range planned {
		plannedResult = append(plannedResult, model)
	}
	return residentResult, plannedResult, nil
}

func usesLocalInference(location llm.OllamaModelLocation) bool {
	return location != llm.OllamaModelLocationCloud && location != llm.OllamaModelLocationRemote
}

func validateRequest(request Request) error {
	if request.Strategy != expertselector.StrategyTeam && request.Strategy != expertselector.StrategySwarm && request.Strategy != expertselector.StrategyMoE {
		return fmt.Errorf("%w: unsupported strategy", ErrInvalidRequest)
	}
	if !boundedText(request.Objective, maxObjectiveBytes, false) {
		return fmt.Errorf("%w: objective is empty, invalid, or too large", ErrInvalidRequest)
	}
	if len(request.ExpertNames) > maxExpertNames {
		return fmt.Errorf("%w: too many expert names", ErrInvalidRequest)
	}
	if !boundedSingleLine(request.Model, maxModelNameBytes, true) {
		return fmt.Errorf("%w: default model name is invalid", ErrInvalidRequest)
	}
	if len(request.ModelOverrides) > maxExpertNames {
		return fmt.Errorf("%w: too many model overrides", ErrInvalidRequest)
	}
	if request.MaxTotalEvalTokens < 0 {
		return fmt.Errorf("%w: total evaluation budget must not be negative", ErrInvalidRequest)
	}
	experts := make(map[string]struct{}, len(request.ExpertNames))
	for _, name := range request.ExpertNames {
		if !boundedSingleLine(name, maxExpertNameBytes, false) {
			return fmt.Errorf("%w: expert name is invalid", ErrInvalidRequest)
		}
		key := profileKey(name)
		if _, duplicate := experts[key]; duplicate {
			return fmt.Errorf("%w: duplicate expert name", ErrInvalidRequest)
		}
		experts[key] = struct{}{}
	}
	overrides := make(map[string]struct{}, len(request.ModelOverrides))
	for _, override := range request.ModelOverrides {
		if !boundedSingleLine(override.Expert, maxExpertNameBytes, false) ||
			!boundedSingleLine(override.Model, maxModelNameBytes, false) {
			return fmt.Errorf("%w: model override is invalid", ErrInvalidRequest)
		}
		key := profileKey(override.Expert)
		if _, duplicate := overrides[key]; duplicate {
			return fmt.Errorf("%w: duplicate model override", ErrInvalidRequest)
		}
		overrides[key] = struct{}{}
	}
	return nil
}

func allocateEvalLimits(count, total, perExpert int) []int {
	if count <= 0 {
		return nil
	}
	limits := make([]int, count)
	if total <= 0 {
		for index := range limits {
			limits[index] = perExpert
		}
		return limits
	}
	base := total / count
	remainder := total % count
	for index := range limits {
		limit := base
		if index < remainder {
			limit++
		}
		limits[index] = min(perExpert, limit)
	}
	return limits
}

func cloneAndValidateProfiles(values []Profile) ([]Profile, error) {
	result := make([]Profile, 0, len(values))
	for _, profile := range values {
		if !boundedSingleLine(profile.Name, 128, false) || strings.ContainsAny(profile.Name, "/\\") ||
			!boundedText(profile.Description, 8*1024, true) ||
			!boundedSingleLine(profile.Model, 256, true) ||
			!boundedText(profile.SystemPrompt, maxSystemPromptBytes, true) || len(profile.UseCases) > 64 {
			return nil, fmt.Errorf("%w: profile metadata is invalid or too large", ErrUnavailable)
		}
		cloned := profile
		cloned.UseCases = append([]string(nil), profile.UseCases...)
		for _, useCase := range cloned.UseCases {
			if !boundedText(useCase, 512, false) {
				return nil, fmt.Errorf("%w: profile use case is invalid or too large", ErrUnavailable)
			}
		}
		result = append(result, cloned)
	}
	return result, nil
}

func boundedSingleLine(value string, limit int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || len(value) > limit || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return false
	}
	if value == "" {
		return allowEmpty
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func boundedText(value string, limit int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || len(value) > limit || (!allowEmpty && strings.TrimSpace(value) == "") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return false
		}
	}
	return true
}

func logicalFanout(strategy expertselector.Strategy, overrides resource.Overrides) int {
	value := resource.DefaultMaxTeamExperts
	switch strategy {
	case expertselector.StrategySwarm:
		value = resource.DefaultMaxSwarmWorkers
		if overrides.MaxSwarmWorkers > 0 {
			value = min(value, overrides.MaxSwarmWorkers)
		}
	case expertselector.StrategyMoE:
		value = resource.DefaultMaxMoEExperts
		if overrides.MaxMoEExperts > 0 {
			value = min(value, overrides.MaxMoEExperts)
		}
	default:
		if overrides.MaxTeamExperts > 0 {
			value = min(value, overrides.MaxTeamExperts)
		}
	}
	return max(1, value)
}

func planFanout(strategy expertselector.Strategy, plan resource.ConcurrencyPlan) int {
	switch strategy {
	case expertselector.StrategySwarm:
		return plan.MaxSwarmWorkers
	case expertselector.StrategyMoE:
		return plan.MaxMoEExperts
	default:
		return plan.MaxTeamExperts
	}
}

func distinctModelCount(selected []selectedExpert) int {
	values := make(map[string]struct{}, len(selected))
	for _, expert := range selected {
		values[canonicalModelKey(expert.model)] = struct{}{}
	}
	return len(values)
}

func cloneModelWeights(values map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(values))
	for name, size := range values {
		if size > 0 {
			result[canonicalModelKey(name)] = size
		}
	}
	return result
}

func canonicalModelKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return value
	}
	last := value
	if slash := strings.LastIndexByte(value, '/'); slash >= 0 {
		last = value[slash+1:]
	}
	if !strings.ContainsAny(last, ":@") {
		value += ":latest"
	}
	return value
}
