package expertteam

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/expertselector"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/resource"
)

type fakeModelRunner struct {
	mu                sync.Mutex
	current           string
	active            int
	maxActive         int
	started           chan string
	release           <-chan struct{}
	fail              map[string]error
	options           []llm.ChatOptions
	models            []string
	responseFor       map[string]string
	modelState        llm.ExpertModelSnapshot
	prepareErr        error
	releaseErr        error
	failAfterCallback map[string]error
	reasoningFor      map[string]string
	reasoningOnly     map[string]string
	prepared          int
	preparedModels    []string
	released          int
	useLimit          bool
	omitDone          bool
	overreport        bool
	afterDone         bool
	zeroDone          bool
}

func (runner *fakeModelRunner) CurrentModel() string { return runner.current }

func (runner *fakeModelRunner) EffectiveContext(string) (int, bool) { return 8192, true }

func (runner *fakeModelRunner) PrepareExpertModels(_ context.Context, selected []string) (llm.ExpertModelSnapshot, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.prepared++
	runner.preparedModels = append([]string(nil), selected...)
	if runner.prepareErr != nil {
		return llm.ExpertModelSnapshot{}, runner.prepareErr
	}
	snapshot := runner.modelState
	if len(snapshot.Models) == 0 {
		snapshot.Models = make([]llm.ExpertModelResource, 0, len(selected))
		for _, model := range selected {
			snapshot.Models = append(snapshot.Models, llm.ExpertModelResource{Name: model, Selected: true})
		}
	}
	return snapshot, nil
}

func (runner *fakeModelRunner) ReleaseExpertModels(context.Context, llm.ExpertModelSnapshot) error {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.released++
	return runner.releaseErr
}

func (runner *fakeModelRunner) ChatStreamForModel(ctx context.Context, model string, options llm.ChatOptions, callback func(llm.StreamChunk) error) error {
	runner.mu.Lock()
	runner.active++
	if runner.active > runner.maxActive {
		runner.maxActive = runner.active
	}
	runner.options = append(runner.options, options)
	runner.models = append(runner.models, model)
	runner.mu.Unlock()
	defer func() {
		runner.mu.Lock()
		runner.active--
		runner.mu.Unlock()
	}()
	if runner.started != nil {
		select {
		case runner.started <- model:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if runner.release != nil {
		select {
		case <-runner.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := runner.fail[model]; err != nil {
		return err
	}
	response := "independent report for " + model
	if configured := runner.responseFor[model]; configured != "" {
		response = configured
	}
	evalTokens := 12
	if runner.useLimit {
		evalTokens = options.MaxEvalTokens
	}
	if runner.overreport {
		evalTokens = options.MaxEvalTokens + 1
	}
	if runner.zeroDone {
		evalTokens = 0
	}
	if runner.afterDone {
		if err := callback(llm.StreamChunk{Text: response, Done: true, EvalCount: evalTokens, PromptEvalCount: 24}); err != nil {
			return err
		}
		return callback(llm.StreamChunk{Text: "late uncharged text"})
	}
	if reasoning := runner.reasoningFor[model]; reasoning != "" {
		if err := callback(llm.StreamChunk{Reasoning: reasoning}); err != nil {
			return err
		}
	}
	if reasoning := runner.reasoningOnly[model]; reasoning != "" {
		if err := callback(llm.StreamChunk{Reasoning: reasoning}); err != nil {
			return err
		}
		return callback(llm.StreamChunk{Done: true, EvalCount: evalTokens, PromptEvalCount: 24})
	}
	if err := callback(llm.StreamChunk{Text: response, Done: !runner.omitDone, EvalCount: evalTokens, PromptEvalCount: 24}); err != nil {
		return err
	}
	return runner.failAfterCallback[model]
}

func (runner *fakeModelRunner) snapshot() (maxActive int, options []llm.ChatOptions, models []string) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.maxActive, append([]llm.ChatOptions(nil), runner.options...), append([]string(nil), runner.models...)
}

func highCapacityProbe() resource.Probe {
	return resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
		return resource.HostSnapshot{LogicalCPU: 10, TotalRAMBytes: 32 << 30, AvailableRAMBytes: 24 << 30}, nil
	})
}

func TestValidateRequestRejectsInventedProfilesWithoutProviderOrResourceWork(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b"}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	err = runtime.ValidateRequest(context.Background(), Request{
		Strategy: expertselector.StrategySwarm, Objective: "Compare game engines.",
		ExpertNames: []string{"Game Engine Architect", "Networking Engineer"},
	})
	if !errors.Is(err, ErrInvalidRequest) || !errors.Is(err, expertselector.ErrUnknownExplicitProfile) {
		t.Fatalf("validation error = %v, want invalid request + unknown exact profile", err)
	}
	if runner.prepared != 0 || len(runner.models) != 0 {
		t.Fatalf("pure validation touched provider state: prepared=%d models=%v", runner.prepared, runner.models)
	}
	if err := runtime.ValidateRequest(context.Background(), Request{
		Strategy: expertselector.StrategySwarm, Objective: "Compare architecture, risks, and verification.",
	}); err != nil {
		t.Fatalf("automatic catalog selection failed validation: %v", err)
	}
	if runner.prepared != 0 || len(runner.models) != 0 {
		t.Fatalf("valid pure validation touched provider state: prepared=%d models=%v", runner.prepared, runner.models)
	}
}

func TestConsultTeamRunsBoundedExpertsInParallelWithoutTools(t *testing.T) {
	release := make(chan struct{})
	runner := &fakeModelRunner{current: "qwen:2b", started: make(chan string, 3), release: release}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, consultErr := runtime.Consult(context.Background(), Request{
			Strategy:  expertselector.StrategyTeam,
			Objective: "Review the integration design and its failure modes.",
		})
		done <- outcome{result: result, err: consultErr}
	}()
	for range 3 {
		select {
		case <-runner.started:
		case <-time.After(time.Second):
			t.Fatal("team did not start three experts concurrently")
		}
	}
	close(release)
	completed := <-done
	if completed.err != nil {
		t.Fatal(completed.err)
	}
	if completed.result.Parallelism != 3 || len(completed.result.Experts) != 3 {
		t.Fatalf("result = %#v", completed.result)
	}
	if completed.result.Plan.MaxConcurrentTeams != 1 {
		t.Fatalf("effective concurrent teams = %d, want serialized runtime value 1", completed.result.Plan.MaxConcurrentTeams)
	}
	maxActive, options, _ := runner.snapshot()
	if maxActive != 3 {
		t.Fatalf("max active inference = %d, want 3", maxActive)
	}
	for _, option := range options {
		if len(option.Tools) != 0 || option.MaxEvalTokens != DefaultMaxEvalTokens || !option.DisableReasoning || option.NumThread != resource.DefaultThreadsPerInference || option.ExpectedContext != 8192 {
			t.Fatalf("unsafe/unbounded expert options = %#v", option)
		}
		if !strings.Contains(option.System, "no tools") || !strings.Contains(option.System, "not verified") {
			t.Fatalf("expert contract missing from system prompt: %q", option.System)
		}
	}
}

func TestConsultModelRoutingPrecedence(t *testing.T) {
	runner := &fakeModelRunner{current: "parent:2b"}
	runtime, err := New(runner, Options{
		Probe: highCapacityProbe(), DefaultNumCtx: 8192,
		Profiles: []Profile{
			{Name: "critic", Model: "profile-critic:2b", Description: "configured critic"},
			{Name: "architect", Model: "profile-architect:2b", Description: "configured architect"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review model routing.",
		ExpertNames: []string{"critic", "architect", "explorer"}, Model: "request:2b",
		ModelOverrides: []ModelOverride{{Expert: "critic", Model: "override:2b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	models := make(map[string]string, len(result.Experts))
	for _, receipt := range result.Experts {
		models[receipt.Name] = receipt.Model
	}
	if models["critic"] != "override:2b" || models["architect"] != "request:2b" || models["explorer"] != "request:2b" {
		t.Fatalf("request routing = %#v", models)
	}

	result, err = runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review fallback routing.",
		ExpertNames: []string{"critic", "generalist"},
	})
	if err != nil {
		t.Fatal(err)
	}
	models = make(map[string]string, len(result.Experts))
	for _, receipt := range result.Experts {
		models[receipt.Name] = receipt.Model
	}
	if models["critic"] != "profile-critic:2b" || models["generalist"] != "parent:2b" {
		t.Fatalf("fallback routing = %#v", models)
	}
}

func TestConsultRejectsInvalidModelRoutingBeforeProviderAdmission(t *testing.T) {
	tests := []struct {
		name    string
		request Request
	}{
		{name: "duplicate experts", request: Request{Strategy: expertselector.StrategyTeam, Objective: "review", ExpertNames: []string{"critic", "CRITIC"}}},
		{name: "invalid default", request: Request{Strategy: expertselector.StrategyTeam, Objective: "review", Model: " bad"}},
		{name: "duplicate overrides", request: Request{Strategy: expertselector.StrategyTeam, Objective: "review", ExpertNames: []string{"critic"}, ModelOverrides: []ModelOverride{{Expert: "critic", Model: "one"}, {Expert: "CRITIC", Model: "two"}}}},
		{name: "unknown override", request: Request{Strategy: expertselector.StrategyTeam, Objective: "review", ExpertNames: []string{"critic"}, ModelOverrides: []ModelOverride{{Expert: "unknown", Model: "one"}}}},
		{name: "unselected override", request: Request{Strategy: expertselector.StrategyTeam, Objective: "review", ExpertNames: []string{"critic"}, ModelOverrides: []ModelOverride{{Expert: "architect", Model: "one"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeModelRunner{current: "parent:2b"}
			runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runtime.Consult(context.Background(), test.request)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error=%v, want ErrInvalidRequest", err)
			}
			runner.mu.Lock()
			prepared := runner.prepared
			runner.mu.Unlock()
			if prepared != 0 {
				t.Fatalf("invalid request prepared %d model snapshots", prepared)
			}
		})
	}
}

func TestConsultUnavailableOverrideFailsClosedBeforeDispatch(t *testing.T) {
	runner := &fakeModelRunner{
		current: "parent:2b",
		modelState: llm.ExpertModelSnapshot{InventoryVerified: true, Models: []llm.ExpertModelResource{
			{Name: "missing:2b", Selected: true, Location: llm.OllamaModelLocationUnknown},
		}},
	}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review with an unavailable model.",
		ExpertNames: []string{"critic"}, Model: "missing:2b",
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error=%v, want ErrUnavailable", err)
	}
	_, _, models := runner.snapshot()
	if len(models) != 0 {
		t.Fatalf("unavailable model dispatched: %v", models)
	}
}

func TestConsultReasoningNeverBecomesExpertReport(t *testing.T) {
	t.Run("reasoning only fails with typed code", func(t *testing.T) {
		runner := &fakeModelRunner{
			current: "reasoner:2b", useLimit: true,
			reasoningOnly: map[string]string{"reasoner:2b": "private chain of thought must not leak"},
		}
		runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Consult(context.Background(), Request{
			Strategy: expertselector.StrategyTeam, Objective: "Return a visible review.",
		})
		if !errors.Is(err, ErrAllExpertsFailed) {
			t.Fatalf("error=%v, want ErrAllExpertsFailed", err)
		}
		for _, receipt := range result.Experts {
			if receipt.Status != ExpertFailed || receipt.ErrorCode != "no_visible_report" || receipt.Report != "" {
				t.Fatalf("reasoning-only receipt = %#v", receipt)
			}
		}
		if formatted := result.Format(); strings.Contains(formatted, "private chain") || strings.Contains(formatted, "thought") {
			t.Fatalf("private reasoning leaked into result: %q", formatted)
		}
		_, options, _ := runner.snapshot()
		for _, option := range options {
			if !option.DisableReasoning {
				t.Fatalf("expert request did not disable reasoning: %#v", option)
			}
		}
	})

	t.Run("visible text succeeds while reasoning stays transient", func(t *testing.T) {
		runner := &fakeModelRunner{
			current: "reasoner:2b", reasoningFor: map[string]string{"reasoner:2b": "private analysis"},
			responseFor: map[string]string{"reasoner:2b": "visible bounded finding"},
		}
		runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Consult(context.Background(), Request{
			Strategy: expertselector.StrategyTeam, Objective: "Return a visible review.",
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, receipt := range result.Experts {
			if receipt.Report != "visible bounded finding" || strings.Contains(receipt.Report, "private") {
				t.Fatalf("mixed receipt = %#v", receipt)
			}
		}
	})
}

func TestConsultProgressIsMonotonicBoundedAndSchedulerOwned(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b"}
	runtime, err := New(runner, Options{
		Probe: highCapacityProbe(), DefaultNumCtx: 8192,
		ResourceOverrides: resource.Overrides{MaxConcurrentInference: 1, MaxConcurrentDistinctModels: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []ProgressEvent
	result, err := runtime.ConsultWithProgress(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "sensitive objective must not enter progress",
	}, func(event ProgressEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1+2*len(result.Experts) {
		t.Fatalf("events=%#v", events)
	}
	if events[0].Phase != ProgressPlanned || events[0].ExpertIndex != -1 || events[0].Queued != len(result.Experts) || events[0].Parallelism != 1 {
		t.Fatalf("planned event=%#v", events[0])
	}
	for index, event := range events {
		if event.Sequence != uint64(index+1) || event.Total != len(result.Experts) || event.Strategy != expertselector.StrategyTeam {
			t.Fatalf("event[%d]=%#v", index, event)
		}
		if strings.Contains(event.Expert, "sensitive") || strings.Contains(event.Model, "sensitive") || strings.Contains(event.ErrorCode, "sensitive") {
			t.Fatalf("objective leaked into event=%#v", event)
		}
	}
	for index := 0; index < len(result.Experts); index++ {
		started := events[1+2*index]
		completed := events[2+2*index]
		if started.Phase != ProgressStarted || started.Running != 1 || completed.Phase != ProgressCompleted || completed.Running != 0 ||
			started.ExpertIndex != completed.ExpertIndex || completed.Completed != index+1 {
			t.Fatalf("expert event pair start=%#v complete=%#v", started, completed)
		}
	}
}

func TestConsultProgressReportsTypedFailuresWithoutProviderText(t *testing.T) {
	runner := &fakeModelRunner{
		current: "qwen:2b",
		fail:    map[string]error{"qwen:2b": errors.New("provider failed with password=do-not-leak")},
	}
	runtime, err := New(runner, Options{
		Probe: highCapacityProbe(), DefaultNumCtx: 8192,
		ResourceOverrides: resource.Overrides{MaxConcurrentInference: 1, MaxConcurrentDistinctModels: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	var events []ProgressEvent
	result, err := runtime.ConsultWithProgress(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "secret objective must remain transient",
	}, func(event ProgressEvent) {
		events = append(events, event)
	})
	if !errors.Is(err, ErrAllExpertsFailed) {
		t.Fatalf("error=%v, want ErrAllExpertsFailed", err)
	}
	if len(events) != 1+2*len(result.Experts) {
		t.Fatalf("events=%#v", events)
	}
	last := events[len(events)-1]
	if last.Phase != ProgressFailed || last.Status != ExpertFailed || last.ErrorCode != "inference_failed" ||
		last.Failed != len(result.Experts) || last.Completed != 0 || last.Running != 0 || last.Queued != 0 {
		t.Fatalf("last failure event=%#v", last)
	}
	for _, event := range events {
		formatted := fmt.Sprintf("%#v", event)
		if strings.Contains(formatted, "password") || strings.Contains(formatted, "do-not-leak") || strings.Contains(formatted, "secret objective") {
			t.Fatalf("sensitive provider content leaked into event: %s", formatted)
		}
	}
}

func TestProfileCountIncludesConfiguredProfilesAndDeduplicatedBuiltins(t *testing.T) {
	runtime, err := New(&fakeModelRunner{current: "qwen:2b"}, Options{
		Probe: highCapacityProbe(), DefaultNumCtx: 8192,
		Profiles: []Profile{
			{Name: "critic", Description: "configured override"},
			{Name: "domain-specialist", Description: "custom profile"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := runtime.ProfileCount(); got != len(builtinProfiles())+1 {
		t.Fatalf("profile count=%d, want %d", got, len(builtinProfiles())+1)
	}
}

func TestConsultSharesParentEvaluationBudgetAcrossExperts(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b", useLimit: true}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review a bounded change.", MaxTotalEvalTokens: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experts) != 3 {
		t.Fatalf("experts=%d, want 3", len(result.Experts))
	}
	usage, err := result.ChargedUsage()
	if err != nil {
		t.Fatal(err)
	}
	if usage.EvalTokens != 5 {
		t.Fatalf("charged usage=%d, want shared cap 5", usage.EvalTokens)
	}
	_, options, _ := runner.snapshot()
	limits := make([]int, 0, len(options))
	total := 0
	for _, option := range options {
		limits = append(limits, option.MaxEvalTokens)
		total += option.MaxEvalTokens
	}
	if total != 5 || len(limits) != 3 {
		t.Fatalf("expert limits=%v total=%d", limits, total)
	}
}

func TestConsultReducesFanoutWhenBudgetCannotGiveEveryExpertOneToken(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b", useLimit: true}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review a nearly exhausted goal.", MaxTotalEvalTokens: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experts) != 2 || result.Parallelism != 2 {
		t.Fatalf("budget-reduced result: experts=%d parallelism=%d", len(result.Experts), result.Parallelism)
	}
	usage, err := result.ChargedUsage()
	if err != nil || usage.EvalTokens != 2 {
		t.Fatalf("usage=%#v error=%v", usage, err)
	}
	_, options, _ := runner.snapshot()
	for _, option := range options {
		if option.MaxEvalTokens != 1 {
			t.Fatalf("budget-reduced expert limit=%d, want 1", option.MaxEvalTokens)
		}
	}
}

func TestChargedUsageRejectsInvalidCustomRuntimeReceipt(t *testing.T) {
	for name, receipt := range map[string]ExpertReceipt{
		"negative raw":          {EvalTokens: -1},
		"negative charged":      {ChargedEvalTokens: -1},
		"charge below actual":   {EvalTokens: 2, ChargedEvalTokens: 1},
		"completed zero charge": {Status: ExpertCompleted, Report: "impossible", ChargedEvalTokens: 0},
		"unknown status":        {Status: "invented", EvalTokens: 1, ChargedEvalTokens: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (Result{Experts: []ExpertReceipt{receipt}}).ChargedUsage(); err == nil {
				t.Fatalf("invalid custom usage receipt was accepted: %#v", receipt)
			}
		})
	}
}

func TestConsultConvertsZeroTokenTerminalReceiptToConservativelyChargedSuccess(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b", zeroDone: true}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyMoE, Objective: "Reject an inconsistent terminal receipt.", MaxTotalEvalTokens: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	usage, usageErr := result.ChargedUsage()
	if usageErr != nil || usage.EvalTokens != 5 {
		t.Fatalf("zero-terminal usage=%#v error=%v", usage, usageErr)
	}
	if len(result.Experts) != 1 || result.Experts[0].Status != ExpertCompleted || !result.Experts[0].UsageEstimated {
		t.Fatalf("zero-terminal receipt=%#v", result.Experts)
	}
}

func TestConsultCancellationDoesNotStartQueuedExperts(t *testing.T) {
	release := make(chan struct{})
	runner := &fakeModelRunner{current: "qwen:2b", started: make(chan string, 3), release: release}
	runtime, err := New(runner, Options{
		DefaultNumCtx: 8192,
		Probe: resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
			return resource.HostSnapshot{LogicalCPU: 1}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result Result
	var consultErr error
	go func() {
		result, consultErr = runtime.Consult(ctx, Request{Strategy: expertselector.StrategyTeam, Objective: "Cancel a serial team."})
		close(done)
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("first expert did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled consultation did not join")
	}
	if !errors.Is(consultErr, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", consultErr)
	}
	_, _, models := runner.snapshot()
	if len(models) != 1 {
		t.Fatalf("queued experts started after cancellation: models=%v", models)
	}
	usage, err := result.ChargedUsage()
	if err != nil || usage.EvalTokens != DefaultMaxEvalTokens {
		t.Fatalf("cancelled usage=%#v error=%v", usage, err)
	}
}

func TestConsultRejectsChunksAfterTerminalReceiptAndChargesReservation(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b", useLimit: true, afterDone: true}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyMoE, Objective: "Reject post-terminal text.", MaxTotalEvalTokens: 5,
	})
	if !errors.Is(err, ErrAllExpertsFailed) {
		t.Fatalf("error=%v, want all experts failed", err)
	}
	usage, usageErr := result.ChargedUsage()
	if usageErr != nil || usage.EvalTokens != 5 {
		t.Fatalf("post-terminal usage=%#v error=%v", usage, usageErr)
	}
	if len(result.Experts) != 1 || !result.Experts[0].UsageEstimated || strings.Contains(result.Experts[0].Report, "late") {
		t.Fatalf("post-terminal receipt=%#v", result.Experts)
	}
}

func TestConsultChargesReservationsWhenExpertUsageIsUnknown(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b", useLimit: true, omitDone: true}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review missing receipts.", MaxTotalEvalTokens: 6,
	})
	if !errors.Is(err, ErrAllExpertsFailed) {
		t.Fatalf("error=%v, want all experts failed", err)
	}
	usage, usageErr := result.ChargedUsage()
	if usageErr != nil {
		t.Fatal(usageErr)
	}
	if usage.EvalTokens != 6 {
		t.Fatalf("charged usage=%d, want reserved 6", usage.EvalTokens)
	}
	for _, receipt := range result.Experts {
		if receipt.ErrorCode != "missing_usage_receipt" || !receipt.UsageEstimated {
			t.Fatalf("missing receipt was not conservatively marked: %#v", receipt)
		}
	}
}

func TestConsultDoesNotChargeNoModelPreflightBeforeCallback(t *testing.T) {
	runner := &fakeModelRunner{
		current: "qwen:2b",
		fail:    map[string]error{"qwen:2b": llm.ErrNoModelSelected},
	}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyMoE, Objective: "Reject before entering the provider stream.", MaxTotalEvalTokens: 5,
	})
	if !errors.Is(err, ErrAllExpertsFailed) {
		t.Fatalf("error=%v, want all experts failed", err)
	}
	usage, usageErr := result.ChargedUsage()
	if usageErr != nil || usage.EvalTokens != 0 {
		t.Fatalf("preflight usage=%#v error=%v", usage, usageErr)
	}
	if len(result.Experts) != 1 || result.Experts[0].UsageEstimated {
		t.Fatalf("preflight receipt=%#v", result.Experts)
	}
}

func TestConsultDoesNotChargeWrappedInferenceNotStartedPreflight(t *testing.T) {
	runner := &fakeModelRunner{
		current: "qwen:2b",
		fail: map[string]error{
			"qwen:2b": fmt.Errorf("bounded manager preflight: %w", llm.ErrInferenceNotStarted),
		},
	}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyMoE, Objective: "Reject before provider inference starts.", MaxTotalEvalTokens: 5,
	})
	if !errors.Is(err, ErrAllExpertsFailed) {
		t.Fatalf("error=%v, want all experts failed", err)
	}
	usage, usageErr := result.ChargedUsage()
	if usageErr != nil || usage.EvalTokens != 0 {
		t.Fatalf("wrapped preflight usage=%#v error=%v", usage, usageErr)
	}
	if len(result.Experts) != 1 || result.Experts[0].UsageEstimated {
		t.Fatalf("wrapped preflight receipt=%#v", result.Experts)
	}
}

func TestConsultChargesGenericNoCallbackProviderFailure(t *testing.T) {
	runner := &fakeModelRunner{
		current: "qwen:2b",
		fail:    map[string]error{"qwen:2b": errors.New("provider failed before callback")},
	}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyMoE, Objective: "Conservatively charge an ambiguous provider failure.", MaxTotalEvalTokens: 5,
	})
	if !errors.Is(err, ErrAllExpertsFailed) {
		t.Fatalf("error=%v, want all experts failed", err)
	}
	usage, usageErr := result.ChargedUsage()
	if usageErr != nil || usage.EvalTokens != 5 {
		t.Fatalf("generic no-callback usage=%#v error=%v", usage, usageErr)
	}
	if len(result.Experts) != 1 || !result.Experts[0].UsageEstimated {
		t.Fatalf("generic no-callback receipt=%#v", result.Experts)
	}
}

func TestConsultChargesReservationWhenNoModelErrorFollowsCallback(t *testing.T) {
	runner := &fakeModelRunner{
		current:           "qwen:2b",
		omitDone:          true,
		responseFor:       map[string]string{"qwen:2b": "   "},
		failAfterCallback: map[string]error{"qwen:2b": llm.ErrNoModelSelected},
	}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyMoE, Objective: "Enter the provider stream before losing the model.", MaxTotalEvalTokens: 5,
	})
	if !errors.Is(err, ErrAllExpertsFailed) {
		t.Fatalf("error=%v, want all experts failed", err)
	}
	usage, usageErr := result.ChargedUsage()
	if usageErr != nil || usage.EvalTokens != 5 {
		t.Fatalf("post-callback usage=%#v error=%v", usage, usageErr)
	}
	if len(result.Experts) != 1 || !result.Experts[0].UsageEstimated || result.Experts[0].ErrorCode != "model_unavailable" {
		t.Fatalf("post-callback receipt=%#v", result.Experts)
	}
}

func TestConsultSurfacesProviderBudgetOverreport(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b", overreport: true}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review provider limits.", MaxTotalEvalTokens: 3,
	})
	if !errors.Is(err, ErrAllExpertsFailed) {
		t.Fatalf("error=%v, want all experts failed", err)
	}
	usage, usageErr := result.ChargedUsage()
	if usageErr != nil {
		t.Fatal(usageErr)
	}
	if usage.EvalTokens <= 3 {
		t.Fatalf("provider overreport was hidden: usage=%d", usage.EvalTokens)
	}
	for _, receipt := range result.Experts {
		if receipt.ErrorCode != "budget_exceeded" || receipt.UsageEstimated {
			t.Fatalf("overreport receipt=%#v", receipt)
		}
	}
}

func TestSerialResourcePlanPreservesLogicalTeamFanout(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b"}
	runtime, err := New(runner, Options{
		DefaultNumCtx: 8192,
		Probe: resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
			return resource.HostSnapshot{LogicalCPU: 1}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review a bounded change.",
	})
	if err != nil {
		t.Fatal(err)
	}
	maxActive, _, _ := runner.snapshot()
	if !result.Plan.SerialOnly || result.Parallelism != 1 || maxActive != 1 {
		t.Fatalf("serial plan/result = plan %#v parallelism %d maxActive %d", result.Plan, result.Parallelism, maxActive)
	}
	if len(result.Experts) != resource.DefaultMaxTeamExperts {
		t.Fatalf("serial plan lost fanout: experts=%d", len(result.Experts))
	}
}

func TestConsultBudgetsEveryLiveResidentModelOnTotalOnlyHost(t *testing.T) {
	runner := &fakeModelRunner{
		current: "qwen:2b",
		modelState: llm.ExpertModelSnapshot{InventoryVerified: true, Models: []llm.ExpertModelResource{
			{Name: "qwen:2b", WeightBytes: 8 << 30, ResidentBytes: 8 << 30, Resident: true, Current: true, Selected: true, Location: llm.OllamaModelLocationLocal},
			{Name: "old-expert:4b", WeightBytes: 4 << 30, ResidentBytes: 4 << 30, Resident: true, Active: true, ExpertOnly: true, Location: llm.OllamaModelLocationLocal},
		}},
	}
	runtime, err := New(runner, Options{
		DefaultNumCtx: 8192,
		Probe: resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
			return resource.HostSnapshot{LogicalCPU: 10, TotalRAMBytes: 16 << 30}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review the change.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Parallelism != 1 || result.Plan.MaxConcurrentInference != 1 {
		t.Fatalf("resident-model plan = %+v, parallelism=%d; want serial", result.Plan, result.Parallelism)
	}
	runner.mu.Lock()
	prepared, released := runner.prepared, runner.released
	runner.mu.Unlock()
	if prepared != 1 || released != 1 {
		t.Fatalf("model lease lifecycle = prepare %d release %d", prepared, released)
	}
}

func TestConsultDeniesKnownRAMBeforeLoadingSelectedModelAndReleasesLease(t *testing.T) {
	runner := &fakeModelRunner{
		current: "current:2b",
		modelState: llm.ExpertModelSnapshot{InventoryVerified: true, Models: []llm.ExpertModelResource{
			{Name: "large:8b", WeightBytes: 8 << 30, Selected: true, Location: llm.OllamaModelLocationLocal},
		}},
	}
	runtime, err := New(runner, Options{
		DefaultNumCtx: 8192,
		Profiles:      []Profile{{Name: "large", Model: "large:8b", Description: "large local reviewer"}},
		Probe: resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
			return resource.HostSnapshot{LogicalCPU: 8, TotalRAMBytes: 16 << 30, AvailableRAMBytes: 10 << 30}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review without overcommitting memory.", ExpertNames: []string{"large"},
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("known-insufficient consultation error = %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.models) != 0 || runner.released != 1 {
		t.Fatalf("denied consultation dispatched=%v released=%d", runner.models, runner.released)
	}
}

func TestConsultAdaptivelyKeepsBestSelectorOrderSubsetUsingOneHostProbe(t *testing.T) {
	probeCalls := 0
	runner := &fakeModelRunner{
		current: "current:2b",
		modelState: llm.ExpertModelSnapshot{InventoryVerified: true, Models: []llm.ExpertModelResource{
			{Name: "first:4b", WeightBytes: 4 << 30, Selected: true, Location: llm.OllamaModelLocationLocal},
			{Name: "second:4b", WeightBytes: 4 << 30, Selected: true, Location: llm.OllamaModelLocationLocal},
			{Name: "third:2b", WeightBytes: 2 << 30, Selected: true, Location: llm.OllamaModelLocationLocal},
		}},
	}
	runtime, err := New(runner, Options{
		DefaultNumCtx: 8192,
		Profiles: []Profile{
			{Name: "first", Model: "first:4b", Description: "first ordered reviewer"},
			{Name: "second", Model: "second:4b", Description: "second ordered reviewer"},
			{Name: "third", Model: "third:2b", Description: "third ordered reviewer"},
		},
		Probe: resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
			probeCalls++
			return resource.HostSnapshot{LogicalCPU: 10, TotalRAMBytes: 16 << 30, AvailableRAMBytes: 10 << 30}, nil
		}),
		ResourceOverrides: resource.Overrides{ReservedRAMBytes: 2 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review within the live host budget.",
		ExpertNames: []string{"first", "second", "third"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if probeCalls != 1 {
		t.Fatalf("host probe calls=%d, want exactly 1", probeCalls)
	}
	if len(result.Experts) != 2 || result.Experts[0].Name != "first" || result.Experts[1].Name != "third" {
		t.Fatalf("adaptive selector-order subset=%#v", result.Experts)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "fan-out was reduced") {
		t.Fatalf("adaptive warnings=%q", result.Warnings)
	}
	if result.Plan.MaxConcurrentTeams != 1 {
		t.Fatalf("effective concurrent teams=%d, want 1", result.Plan.MaxConcurrentTeams)
	}
	runner.mu.Lock()
	prepared, released := runner.prepared, runner.released
	runner.mu.Unlock()
	if prepared != 1 || released != 1 {
		t.Fatalf("adaptive lease lifecycle=prepare %d release %d", prepared, released)
	}
}

func TestConsultOn16GBHostDoesNotAdmitOrnithAndGemmaTogether(t *testing.T) {
	const gib = int64(1 << 30)
	runner := &fakeModelRunner{
		current: "phi4-mini:latest",
		modelState: llm.ExpertModelSnapshot{InventoryVerified: true, Models: []llm.ExpertModelResource{
			{
				Name: "phi4-mini:latest", WeightBytes: 5 * gib / 2, ResidentBytes: 5 * gib / 2,
				Resident: true, Current: true, Location: llm.OllamaModelLocationLocal,
			},
			{
				Name: "ornith:latest", WeightBytes: 56 * gib / 10,
				Selected: true, Location: llm.OllamaModelLocationLocal,
			},
			{
				Name: "gemma4:e2b", WeightBytes: 72 * gib / 10,
				Selected: true, Location: llm.OllamaModelLocationLocal,
			},
		}},
	}
	runtime, err := New(runner, Options{
		DefaultNumCtx: 16_384,
		Profiles: []Profile{
			{Name: "ornith-reviewer", Model: "ornith:latest", Description: "first local reviewer"},
			{Name: "gemma-reviewer", Model: "gemma4:e2b", Description: "second local reviewer"},
		},
		Probe: resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
			// Darwin deliberately uses total-only planning because it has no
			// stable MemAvailable equivalent.
			return resource.HostSnapshot{LogicalCPU: 10, TotalRAMBytes: 16 * gib}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := runtime.Consult(context.Background(), Request{
		Strategy:  expertselector.StrategyTeam,
		Objective: "Review within the 16 GB local residency budget.",
		ExpertNames: []string{
			"ornith-reviewer",
			"gemma-reviewer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experts) != 1 || result.Experts[0].Model != "ornith:latest" {
		t.Fatalf("16 GB admitted experts = %#v, want only the first fitting model", result.Experts)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "fan-out was reduced") {
		t.Fatalf("16 GB warnings = %q, want an explicit fan-out reduction", result.Warnings)
	}
	runner.mu.Lock()
	dispatched := append([]string(nil), runner.models...)
	maxActive := runner.maxActive
	runner.mu.Unlock()
	if len(dispatched) != 1 || dispatched[0] != "ornith:latest" || maxActive != 1 {
		t.Fatalf("16 GB dispatch = %v max_active=%d, want one Ornith inference", dispatched, maxActive)
	}
}

func TestConsultPreservesVerifiedExternalLocationWithoutLocalRAMDenial(t *testing.T) {
	for _, test := range []struct {
		name     string
		location llm.OllamaModelLocation
		boundary string
	}{
		{name: "cloud", location: llm.OllamaModelLocationCloud, boundary: "execution boundary: CLOUD"},
		{name: "remote", location: llm.OllamaModelLocationRemote, boundary: "execution boundary: REMOTE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeModelRunner{
				current: "current:2b",
				modelState: llm.ExpertModelSnapshot{InventoryVerified: true, Models: []llm.ExpertModelResource{
					{Name: "external:70b", WeightBytes: 64 << 30, Selected: true, Location: test.location},
				}},
			}
			runtime, err := New(runner, Options{
				DefaultNumCtx: 8192,
				Profiles:      []Profile{{Name: "external", Model: "external:70b", Description: "external model reviewer"}},
				Probe: resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
					return resource.HostSnapshot{LogicalCPU: 8, TotalRAMBytes: 8 << 30, AvailableRAMBytes: 1 << 30}, nil
				}),
				ResourceOverrides: resource.Overrides{
					ReservedRAMBytes: 2 << 30, KVBytesPerToken: 1 << 60,
					PerInferenceOverheadBytes: 1 << 62,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runtime.Consult(context.Background(), Request{
				Strategy: expertselector.StrategyTeam, Objective: "Review through a verified external model.",
				ExpertNames: []string{"external"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Experts) != 1 || result.Experts[0].Location != test.location {
				t.Fatalf("external receipt=%#v", result.Experts)
			}
			if !result.Plan.SerialOnly || result.Parallelism != 1 {
				t.Fatalf("external plan=%#v parallelism=%d, want conservative serial admission", result.Plan, result.Parallelism)
			}
			formatted := result.Format()
			if !strings.Contains(formatted, test.boundary) || !strings.Contains(formatted, "does not consume local model weights") {
				t.Fatalf("external formatted receipt:\n%s", formatted)
			}
		})
	}
}

func TestConsultDoesNotClaimAnUnverifiedExternalBoundary(t *testing.T) {
	runner := &fakeModelRunner{
		current: "current:2b",
		modelState: llm.ExpertModelSnapshot{Models: []llm.ExpertModelResource{
			{Name: "unverified:7b", Selected: true, Location: llm.OllamaModelLocationCloud},
		}},
	}
	runtime, err := New(runner, Options{
		DefaultNumCtx: 8192, Probe: highCapacityProbe(),
		Profiles: []Profile{{Name: "unverified", Model: "unverified:7b", Description: "unverified location reviewer"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review without inventing a location boundary.",
		ExpertNames: []string{"unverified"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experts) != 1 || result.Experts[0].Location != llm.OllamaModelLocationUnknown {
		t.Fatalf("unverified receipt=%#v", result.Experts)
	}
	if strings.Contains(result.Format(), "execution boundary:") {
		t.Fatalf("unverified location was presented as a verified boundary:\n%s", result.Format())
	}
}

func TestConsultMixedLocationsStillReserveEveryAcceptedLocalModel(t *testing.T) {
	runner := &fakeModelRunner{
		current: "current:2b",
		modelState: llm.ExpertModelSnapshot{InventoryVerified: true, Models: []llm.ExpertModelResource{
			{Name: "cloud:70b", WeightBytes: 64 << 30, Selected: true, Location: llm.OllamaModelLocationCloud},
			{Name: "local-one:4b", WeightBytes: 4 << 30, Selected: true, Location: llm.OllamaModelLocationLocal},
			{Name: "local-two:4b", WeightBytes: 4 << 30, Selected: true, Location: llm.OllamaModelLocationLocal},
		}},
	}
	runtime, err := New(runner, Options{
		DefaultNumCtx: 8192,
		Profiles: []Profile{
			{Name: "cloud", Model: "cloud:70b", Description: "cloud reviewer"},
			{Name: "local-one", Model: "local-one:4b", Description: "first local reviewer"},
			{Name: "local-two", Model: "local-two:4b", Description: "second local reviewer"},
		},
		Probe: resource.ProbeFunc(func(context.Context) (resource.HostSnapshot, error) {
			return resource.HostSnapshot{LogicalCPU: 10, TotalRAMBytes: 16 << 30, AvailableRAMBytes: 10 << 30}, nil
		}),
		ResourceOverrides: resource.Overrides{ReservedRAMBytes: 2 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review with mixed execution locations.",
		ExpertNames: []string{"cloud", "local-one", "local-two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experts) != 2 || result.Experts[0].Name != "cloud" || result.Experts[1].Name != "local-one" {
		t.Fatalf("mixed admitted experts=%#v", result.Experts)
	}
	if result.Experts[0].Location != llm.OllamaModelLocationCloud || result.Experts[1].Location != llm.OllamaModelLocationLocal {
		t.Fatalf("mixed locations=%#v", result.Experts)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "fan-out was reduced") {
		t.Fatalf("mixed warnings=%q", result.Warnings)
	}
}

func TestConsultSurfacesBoundedCleanupWarningWithoutDiscardingReports(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b", releaseErr: errors.New("provider path and secret must not leak")}
	runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyMoE, Objective: "Review cleanup handling.",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(result.Warnings, "\n")
	if !strings.Contains(joined, "could not be unloaded") || strings.Contains(joined, "secret") {
		t.Fatalf("cleanup warning = %q", joined)
	}
}

func TestMoERoutesVideoObjectiveToConfiguredSpecialist(t *testing.T) {
	runner := &fakeModelRunner{current: "qwen:2b"}
	runtime, err := New(runner, Options{
		DefaultNumCtx: 8192, Probe: highCapacityProbe(),
		Profiles: []Profile{
			{Name: "vidtrace", Model: "vision:4b", Description: "Video frame and timeline specialist", UseCases: []string{"inspect mp4 video media and motion"}},
			{Name: "database", Model: "sql:2b", Description: "Relational query specialist", UseCases: []string{"postgres database schema"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy:  expertselector.StrategyMoE,
		Objective: "Inspect the MP4 video timeline and explain the visible failure.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experts) != 1 || result.Experts[0].Name != "vidtrace" || result.Experts[0].Model != "vision:4b" {
		t.Fatalf("MoE selection = %#v", result.Experts)
	}
	if strings.Contains(result.Experts[0].Reason, "MP4") || strings.Contains(result.Experts[0].Reason, "timeline") {
		t.Fatalf("selection reason leaked objective material: %q", result.Experts[0].Reason)
	}
}

func TestDistinctModelsRespectDistinctConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	runner := &fakeModelRunner{current: "current:2b", started: make(chan string, 3), release: release}
	profiles := []Profile{
		{Name: "one", Model: "one:2b", Description: "first perspective"},
		{Name: "two", Model: "two:2b", Description: "second perspective"},
		{Name: "three", Model: "three:2b", Description: "third perspective"},
	}
	runtime, err := New(runner, Options{
		Profiles: profiles, Probe: highCapacityProbe(), DefaultNumCtx: 8192,
		ResourceOverrides: resource.Overrides{MaxConcurrentDistinctModels: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, consultErr := runtime.Consult(context.Background(), Request{
			Strategy: expertselector.StrategyTeam, Objective: "Compare three approaches.",
			ExpertNames: []string{"one", "two", "three"},
		})
		done <- consultErr
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("first expert did not start")
	}
	select {
	case model := <-runner.started:
		t.Fatalf("distinct cap started a second model before release: %s", model)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	maxActive, _, _ := runner.snapshot()
	if maxActive != 1 {
		t.Fatalf("max active distinct models = %d, want 1", maxActive)
	}
}

func TestDistinctCapAllowsSameModelInferenceToCoexist(t *testing.T) {
	release := make(chan struct{})
	runner := &fakeModelRunner{current: "current:2b", started: make(chan string, 3), release: release}
	profiles := []Profile{
		{Name: "one-a", Model: "shared:2b", Description: "first shared-model perspective"},
		{Name: "two-a", Model: "shared:2b", Description: "second shared-model perspective"},
		{Name: "three-b", Model: "other:2b", Description: "distinct-model perspective"},
	}
	runtime, err := New(runner, Options{
		Profiles: profiles, Probe: highCapacityProbe(), DefaultNumCtx: 8192,
		ResourceOverrides: resource.Overrides{MaxConcurrentInference: 3, MaxConcurrentDistinctModels: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, consultErr := runtime.Consult(context.Background(), Request{
			Strategy: expertselector.StrategyTeam, Objective: "Compare shared and distinct model scheduling.",
			ExpertNames: []string{"one-a", "two-a", "three-b"},
		})
		done <- consultErr
	}()
	for range 2 {
		select {
		case model := <-runner.started:
			if model != "shared:2b" {
				t.Fatalf("started model=%q, want shared model", model)
			}
		case <-time.After(time.Second):
			t.Fatal("two same-model experts did not start concurrently")
		}
	}
	select {
	case model := <-runner.started:
		t.Fatalf("distinct model started before shared model released: %s", model)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	maxActive, _, _ := runner.snapshot()
	if maxActive != 2 {
		t.Fatalf("max active inference=%d, want two same-model calls", maxActive)
	}
}

func TestConsultReturnsBoundedPartialReceiptsWithoutRawProviderErrors(t *testing.T) {
	runner := &fakeModelRunner{
		current: "current:2b",
		fail: map[string]error{
			"broken:2b": errors.New("provider failed with password=do-not-leak"),
		},
		responseFor: map[string]string{"ok:2b": strings.Repeat("界", 5000)},
	}
	runtime, err := New(runner, Options{
		Probe: highCapacityProbe(), DefaultNumCtx: 8192,
		MaxReportBytes: 1024, MaxResultBytes: 1500,
		Profiles: []Profile{
			{Name: "broken", Model: "broken:2b", Description: "failure review"},
			{Name: "ok", Model: "ok:2b", Description: "successful review"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Consult(context.Background(), Request{
		Strategy: expertselector.StrategyTeam, Objective: "Review this failure.",
		ExpertNames: []string{"broken", "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	formatted := result.Format()
	if len(formatted) > 1500 {
		t.Fatalf("formatted result is %d bytes, want <= 1500", len(formatted))
	}
	if strings.Contains(formatted, "do-not-leak") || !strings.Contains(formatted, "experts: total=2 · completed=1 · failed=1") ||
		!strings.Contains(formatted, "inference_failed") ||
		!strings.Contains(formatted, "[broken · broken:2b · failed") ||
		!strings.Contains(formatted, "[ok · ok:2b · completed") ||
		!strings.Contains(formatted, "truncated by host") {
		t.Fatalf("unsafe or incomplete partial receipt:\n%s", formatted)
	}
}

func TestConsultCancellationAndAllFailed(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		release := make(chan struct{})
		runner := &fakeModelRunner{current: "qwen:2b", started: make(chan string, 3), release: release}
		runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, consultErr := runtime.Consult(ctx, Request{Strategy: expertselector.StrategyTeam, Objective: "Review cancellation."})
			done <- consultErr
		}()
		<-runner.started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %v", err)
		}
	})

	t.Run("all failed", func(t *testing.T) {
		runner := &fakeModelRunner{current: "qwen:2b", fail: map[string]error{"qwen:2b": errors.New("offline")}}
		runtime, err := New(runner, Options{Probe: highCapacityProbe(), DefaultNumCtx: 8192})
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Consult(context.Background(), Request{Strategy: expertselector.StrategyTeam, Objective: "Review failure."})
		if !errors.Is(err, ErrAllExpertsFailed) || len(result.Experts) != 3 {
			t.Fatalf("all-failed result=%#v err=%v", result, err)
		}
	})
}
