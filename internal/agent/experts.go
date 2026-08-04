package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/expertselector"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
)

const maxExpertObjectiveBytes = 32 * 1024

func (a *Agent) hasExpertConsultant() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	available := a.expertConsultant != nil
	a.mu.RUnlock()
	return available
}

// ExpertConsultationAvailable reports whether the host installed the bounded,
// tool-free Team/Swarm/MoE runtime. It exposes availability only, never the
// consultant implementation or its model/profile configuration.
func (a *Agent) ExpertConsultationAvailable() bool {
	return a.hasExpertConsultant()
}

// ExpertConsultationProfileCount reports a bounded catalog count when the
// installed runtime exposes one. Custom embedders that do not implement this
// optional surface still appear as available without leaking configuration.
func (a *Agent) ExpertConsultationProfileCount() int {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	consultant := a.expertConsultant
	a.mu.RUnlock()
	counter, ok := consultant.(interface{ ProfileCount() int })
	if !ok {
		return 0
	}
	return max(0, counter.ProfileCount())
}

func (a *Agent) preflightConsultExperts(arguments map[string]any) error {
	if !a.hasExpertConsultant() {
		return errors.New("expert consultation is unavailable")
	}
	request, err := expertRequest(arguments)
	if err != nil {
		return err
	}
	if len(arguments) < 2 || len(arguments) > 5 {
		return errors.New("consult_experts accepts only strategy, objective, experts, model, and model_overrides")
	}
	for key := range arguments {
		if key != "strategy" && key != "objective" && key != "experts" && key != "model" && key != "model_overrides" {
			return errors.New("consult_experts contains an unknown argument")
		}
	}
	if !boundedExpertText(request.Objective, maxExpertObjectiveBytes, false) {
		return errors.New("objective is empty, invalid, or too large")
	}
	expertNamesSeen := make(map[string]struct{}, len(request.ExpertNames))
	for _, name := range request.ExpertNames {
		if !boundedExpertText(name, 128, true) || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\r\n") {
			return errors.New("experts contains an invalid profile name")
		}
		key := strings.ToLower(name)
		if _, duplicate := expertNamesSeen[key]; duplicate {
			return errors.New("experts contains a duplicate profile name")
		}
		expertNamesSeen[key] = struct{}{}
	}
	if request.Model != "" && (!boundedExpertText(request.Model, 256, true) || strings.TrimSpace(request.Model) != request.Model) {
		return errors.New("model contains an invalid exact model name")
	}
	for _, override := range request.ModelOverrides {
		if !boundedExpertText(override.Expert, 128, true) || strings.TrimSpace(override.Expert) != override.Expert ||
			!boundedExpertText(override.Model, 256, true) || strings.TrimSpace(override.Model) != override.Model {
			return errors.New("model_overrides contains an invalid expert or model name")
		}
	}
	a.mu.RLock()
	consultant := a.expertConsultant
	a.mu.RUnlock()
	if validator, ok := consultant.(ExpertRequestValidator); ok {
		if err := validator.ValidateRequest(context.Background(), request); err != nil {
			switch {
			case errors.Is(err, expertselector.ErrUnknownExplicitProfile):
				return errors.New("experts contains unavailable exact profile names; omit experts for automatic selection instead of inventing personas")
			case errors.Is(err, expertteam.ErrInvalidRequest):
				return errors.New("expert selection is invalid for the current catalog; omit experts for automatic selection or use exact host-provided profile names")
			case errors.Is(err, expertteam.ErrUnavailable):
				return errors.New("expert consultation is unavailable")
			default:
				return errors.New("expert consultation preflight could not validate the current catalog")
			}
		}
	}
	return nil
}

func (a *Agent) handleConsultExperts(ctx context.Context, arguments map[string]any) (string, bool) {
	content, isErr, _, usageErr := a.handleConsultExpertsWithBudget(ctx, arguments, 0)
	if usageErr != nil {
		return "error: expert consultation returned an invalid usage receipt", true
	}
	return content, isErr
}

func (a *Agent) handleConsultExpertsWithBudget(ctx context.Context, arguments map[string]any, maxTotalEvalTokens int) (string, bool, expertteam.Usage, error) {
	return a.handleConsultExpertsWithBudgetAndProgress(ctx, arguments, maxTotalEvalTokens, nil)
}

func (a *Agent) handleConsultExpertsWithBudgetAndProgress(ctx context.Context, arguments map[string]any, maxTotalEvalTokens int, observer expertteam.Observer) (string, bool, expertteam.Usage, error) {
	request, err := expertRequest(arguments)
	if err != nil {
		return "error: " + err.Error(), true, expertteam.Usage{}, nil
	}
	request.MaxTotalEvalTokens = maxTotalEvalTokens
	a.mu.RLock()
	consultant := a.expertConsultant
	a.mu.RUnlock()
	if consultant == nil {
		return "error: expert consultation is unavailable", true, expertteam.Usage{}, nil
	}
	var result expertteam.Result
	if progressConsultant, ok := consultant.(ExpertProgressConsultant); ok {
		result, err = progressConsultant.ConsultWithProgress(ctx, request, observer)
	} else {
		result, err = consultant.Consult(ctx, request)
	}
	usage, usageErr := result.ChargedUsage()
	if usageErr != nil {
		return "error: expert consultation returned an invalid usage receipt", true, expertteam.Usage{}, usageErr
	}
	formatted := result.Format()
	if err == nil {
		return formatted, false, usage, nil
	}
	code := "expert consultation failed"
	switch {
	case errors.Is(err, context.Canceled):
		code = "expert consultation cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "expert consultation timed out"
	case errors.Is(err, expertteam.ErrAllExpertsFailed):
		code = "all selected experts failed"
	case errors.Is(err, expertteam.ErrInvalidRequest):
		code = "expert consultation request was rejected"
	case errors.Is(err, expertteam.ErrUnavailable):
		code = "expert consultation is unavailable"
	}
	if formatted == "" {
		return "error: " + code, true, usage, nil
	}
	return fmt.Sprintf("error: %s\n%s", code, formatted), true, usage, nil
}

func expertRequest(arguments map[string]any) (expertteam.Request, error) {
	strategyValue, ok := arguments["strategy"].(string)
	if !ok {
		return expertteam.Request{}, errors.New("strategy must be team, swarm, or moe")
	}
	strategy := expertselector.Strategy(strings.ToLower(strings.TrimSpace(strategyValue)))
	if strategy != expertselector.StrategyTeam && strategy != expertselector.StrategySwarm && strategy != expertselector.StrategyMoE {
		return expertteam.Request{}, errors.New("strategy must be team, swarm, or moe")
	}
	objective, ok := arguments["objective"].(string)
	if !ok || strings.TrimSpace(objective) == "" {
		return expertteam.Request{}, errors.New("objective must be a non-empty string")
	}
	expertsValue, expertsPresent := arguments["experts"]
	if expertsPresent && expertsValue == nil {
		return expertteam.Request{}, errors.New("experts must be an array of profile names")
	}
	names, err := expertNames(expertsValue)
	if err != nil {
		return expertteam.Request{}, err
	}
	model, err := expertModel(arguments, "model")
	if err != nil {
		return expertteam.Request{}, err
	}
	overrides, err := expertModelOverrides(arguments)
	if err != nil {
		return expertteam.Request{}, err
	}
	return expertteam.Request{
		Strategy: strategy, Objective: objective, ExpertNames: names,
		Model: model, ModelOverrides: overrides,
	}, nil
}

func expertModel(arguments map[string]any, key string) (string, error) {
	value, exists := arguments[key]
	if !exists {
		return "", nil
	}
	model, ok := value.(string)
	if !ok || strings.TrimSpace(model) == "" {
		return "", errors.New("model must be a non-empty exact model name")
	}
	return model, nil
}

func expertModelOverrides(arguments map[string]any) ([]expertteam.ModelOverride, error) {
	value, exists := arguments["model_overrides"]
	if !exists {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, errors.New("model_overrides must be an array")
	}
	if len(values) > expertselector.MaxSelectedExperts {
		return nil, fmt.Errorf("model_overrides supports at most %d assignments", expertselector.MaxSelectedExperts)
	}
	result := make([]expertteam.ModelOverride, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		assignment, ok := value.(map[string]any)
		if !ok || len(assignment) != 2 {
			return nil, errors.New("model_overrides must contain only expert/model assignments")
		}
		for key := range assignment {
			if key != "expert" && key != "model" {
				return nil, errors.New("model_overrides contains an unknown field")
			}
		}
		expert, expertOK := assignment["expert"].(string)
		model, modelOK := assignment["model"].(string)
		if !expertOK || !modelOK || strings.TrimSpace(expert) == "" || strings.TrimSpace(model) == "" {
			return nil, errors.New("model_overrides requires non-empty expert and model strings")
		}
		key := strings.ToLower(strings.TrimSpace(expert))
		if _, duplicate := seen[key]; duplicate {
			return nil, errors.New("model_overrides contains a duplicate expert")
		}
		seen[key] = struct{}{}
		result = append(result, expertteam.ModelOverride{Expert: expert, Model: model})
	}
	return result, nil
}

func expertNames(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	var names []string
	switch values := value.(type) {
	case []string:
		names = append([]string(nil), values...)
	case []any:
		names = make([]string, 0, len(values))
		for _, value := range values {
			name, ok := value.(string)
			if !ok {
				return nil, errors.New("experts must contain only profile names")
			}
			names = append(names, name)
		}
	default:
		return nil, errors.New("experts must be an array of profile names")
	}
	if len(names) > expertselector.MaxSelectedExperts {
		return nil, fmt.Errorf("experts supports at most %d names", expertselector.MaxSelectedExperts)
	}
	return names, nil
}

func boundedExpertText(value string, limit int, singleLine bool) bool {
	if !utf8.ValidString(value) || len(value) > limit || strings.TrimSpace(value) == "" ||
		(singleLine && strings.ContainsAny(value, "\r\n")) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) && (!singleLine && (character == '\n' || character == '\r' || character == '\t')) {
			continue
		}
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
