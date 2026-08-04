package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/expertselector"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/resource"
)

type fakeExpertConsultant struct {
	request  expertteam.Request
	result   expertteam.Result
	err      error
	calls    int
	profiles int
	events   []expertteam.ProgressEvent
	validate func(expertteam.Request) error
}

func (consultant *fakeExpertConsultant) ProfileCount() int { return consultant.profiles }

func (consultant *fakeExpertConsultant) ValidateRequest(_ context.Context, request expertteam.Request) error {
	if consultant.validate != nil {
		return consultant.validate(request)
	}
	return nil
}

func (consultant *fakeExpertConsultant) Consult(_ context.Context, request expertteam.Request) (expertteam.Result, error) {
	consultant.calls++
	consultant.request = request
	return consultant.result, consultant.err
}

func (consultant *fakeExpertConsultant) ConsultWithProgress(_ context.Context, request expertteam.Request, observer expertteam.Observer) (expertteam.Result, error) {
	consultant.calls++
	consultant.request = request
	for _, event := range consultant.events {
		if observer != nil {
			observer(event)
		}
	}
	return consultant.result, consultant.err
}

func TestConsultExpertsToolIsAvailableOnlyWithHostRuntime(t *testing.T) {
	ag := New(nil, nil, 8192)
	if hasToolDef(ag.toolsBuiltinToolDefs(), "consult_experts") {
		t.Fatal("consult_experts was exposed without a runtime")
	}
	ag.SetExpertConsultant(&fakeExpertConsultant{})
	if !hasToolDef(ag.toolsBuiltinToolDefs(), "consult_experts") {
		t.Fatal("consult_experts was not exposed after runtime installation")
	}
	if builtinToolRequiresApproval("consult_experts") {
		t.Fatal("read-only expert consultation unexpectedly requires effect approval")
	}
	kind, effect := ag.executionKind("consult_experts")
	if kind != execution.KindBuiltin || effect != execution.EffectReadOnly {
		t.Fatalf("execution contract = %s/%s", kind, effect)
	}
	if !AskToolPolicy().AllowsBuiltin("consult_experts") || !PlanToolPolicy().AllowsBuiltin("consult_experts") {
		t.Fatal("read-only modes do not admit expert consultation")
	}
}

func TestExpertConsultationProfileCountUsesOptionalRuntimeSurface(t *testing.T) {
	ag := New(nil, nil, 8192)
	if got := ag.ExpertConsultationProfileCount(); got != 0 {
		t.Fatalf("profile count without runtime=%d", got)
	}
	ag.SetExpertConsultant(&fakeExpertConsultant{profiles: 7})
	if got := ag.ExpertConsultationProfileCount(); got != 7 {
		t.Fatalf("profile count=%d, want 7", got)
	}
}

func TestConsultExpertsPreflightRejectsInvalidShapeBeforeRuntime(t *testing.T) {
	ag := New(nil, nil, 8192)
	ag.SetExpertConsultant(&fakeExpertConsultant{})
	tooManyOverrides := make([]any, expertselector.MaxSelectedExperts+1)
	for index := range tooManyOverrides {
		tooManyOverrides[index] = map[string]any{"expert": fmt.Sprintf("expert-%d", index), "model": "qwen"}
	}
	tests := []llm.ToolCall{
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team"}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "invalid", "objective": "review"}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "extra": true}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "max_concurrent_inference": 8}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "experts": nil}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "experts": []any{"ok", 3}}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": strings.Repeat("x", maxExpertObjectiveBytes+1)}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "experts": []any{"bad\nname"}}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "experts": []any{"critic", "CRITIC"}}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "model": ""}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "model": " qwen"}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "model": strings.Repeat("m", 257)}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "model_overrides": nil}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "model_overrides": map[string]any{"critic": "qwen"}}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "model_overrides": []any{map[string]any{"expert": "critic"}}}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "model_overrides": []any{map[string]any{"expert": "critic", "model": "qwen", "extra": true}}}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "model_overrides": []any{
			map[string]any{"expert": "critic", "model": "qwen"}, map[string]any{"expert": "CRITIC", "model": "flash"},
		}}},
		{Name: "consult_experts", Arguments: map[string]any{"strategy": "team", "objective": "review", "model_overrides": tooManyOverrides}},
	}
	for index, call := range tests {
		if err := ag.preflightToolCall(execution.KindBuiltin, call); err == nil {
			t.Errorf("case %d passed preflight", index)
		}
	}
}

func TestConsultExpertsPreflightRejectsInventedProfilesBeforeDispatch(t *testing.T) {
	consultant := &fakeExpertConsultant{validate: func(request expertteam.Request) error {
		if len(request.ExpertNames) > 0 {
			return errors.Join(expertteam.ErrInvalidRequest, expertselector.ErrUnknownExplicitProfile)
		}
		return nil
	}}
	ag := New(nil, nil, 8192)
	ag.SetExpertConsultant(consultant)
	invalid := llm.ToolCall{Name: "consult_experts", Arguments: map[string]any{
		"strategy": "swarm", "objective": "Compare game engines.",
		"experts": []any{"Game Engine Architect", "Networking Engineer"},
	}}
	err := ag.preflightToolCall(execution.KindBuiltin, invalid)
	if err == nil || !strings.Contains(err.Error(), "omit experts for automatic selection") {
		t.Fatalf("preflight error = %v, want actionable automatic-selection correction", err)
	}
	if consultant.calls != 0 {
		t.Fatalf("invalid catalog request dispatched %d consultations", consultant.calls)
	}
	valid := invalid
	valid.Arguments = map[string]any{"strategy": "swarm", "objective": "Compare game engines."}
	if err := ag.preflightToolCall(execution.KindBuiltin, valid); err != nil {
		t.Fatalf("automatic selection failed preflight: %v", err)
	}
}

func TestConsultExpertsReturnsAdvisoryReceiptAndExactRequest(t *testing.T) {
	consultant := &fakeExpertConsultant{result: expertteam.Result{
		Strategy: expertselector.StrategyTeam, Parallelism: 2,
		Plan: resource.ConcurrencyPlan{MaxConcurrentInference: 2, MaxConcurrentDistinctModels: 1},
		Experts: []expertteam.ExpertReceipt{{
			Name: "critic", Model: "qwen:2b", Status: expertteam.ExpertCompleted,
			Reason: "Selected explicitly.", Report: "One bounded finding.",
			EvalTokens: 1, ChargedEvalTokens: 1,
		}},
	}}
	ag := New(nil, nil, 8192)
	ag.SetExpertConsultant(consultant)
	call := llm.ToolCall{Name: "consult_experts", Arguments: map[string]any{
		"strategy": "team", "objective": "Review the integration.", "experts": []any{"critic"},
		"model": "qwen:2b", "model_overrides": []any{map[string]any{"expert": "critic", "model": "flash:cloud"}},
	}}
	if err := ag.preflightToolCall(execution.KindBuiltin, call); err != nil {
		t.Fatal(err)
	}
	content, isErr := ag.handleBuiltinToolWithCancellation(context.Background(), call, false)
	if isErr || !strings.Contains(content, "advisory; not verified evidence") || !strings.Contains(content, "One bounded finding") {
		t.Fatalf("receipt = %q, error=%v", content, isErr)
	}
	if consultant.request.Strategy != expertselector.StrategyTeam || consultant.request.Objective != "Review the integration." ||
		len(consultant.request.ExpertNames) != 1 || consultant.request.ExpertNames[0] != "critic" ||
		consultant.request.Model != "qwen:2b" || len(consultant.request.ModelOverrides) != 1 ||
		consultant.request.ModelOverrides[0] != (expertteam.ModelOverride{Expert: "critic", Model: "flash:cloud"}) {
		t.Fatalf("request = %#v", consultant.request)
	}
}

func TestConsultExpertsMapsRuntimeErrorsWithoutLeakingRawProviderDetail(t *testing.T) {
	consultant := &fakeExpertConsultant{
		result: expertteam.Result{Strategy: expertselector.StrategyTeam, Experts: []expertteam.ExpertReceipt{{
			Name: "critic", Model: "qwen:2b", Status: expertteam.ExpertFailed, ErrorCode: "inference_failed",
		}}},
		err: errors.Join(expertteam.ErrAllExpertsFailed, errors.New("password=do-not-leak")),
	}
	ag := New(nil, nil, 8192)
	ag.SetExpertConsultant(consultant)
	content, isErr := ag.handleConsultExperts(context.Background(), map[string]any{
		"strategy": "team", "objective": "Review failure.",
	})
	if !isErr || !strings.Contains(content, "all selected experts failed") || strings.Contains(content, "do-not-leak") {
		t.Fatalf("mapped error = %q, error=%v", content, isErr)
	}
}
