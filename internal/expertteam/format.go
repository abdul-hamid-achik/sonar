package expertteam

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type boundedCollector struct {
	limit     int
	builder   strings.Builder
	truncated bool
}

func newBoundedCollector(limit int) *boundedCollector { return &boundedCollector{limit: limit} }

func (collector *boundedCollector) Append(value string) {
	if collector == nil || value == "" || collector.truncated {
		return
	}
	value = strings.ToValidUTF8(value, "�")
	remaining := collector.limit - collector.builder.Len()
	if remaining <= 0 {
		collector.truncated = true
		return
	}
	if len(value) <= remaining {
		collector.builder.WriteString(value)
		return
	}
	cut := remaining
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	collector.builder.WriteString(value[:cut])
	collector.truncated = true
}

func (collector *boundedCollector) String() string {
	if collector == nil {
		return ""
	}
	value := collector.builder.String()
	if collector.truncated {
		marker := "\n… (expert report truncated by host)"
		payloadLimit := max(0, collector.limit-len(marker))
		value = strings.TrimSpace(boundUTF8Bytes(value, payloadLimit)) + marker
	}
	return value
}

// Format returns bounded tool-result text. Expert prose is explicitly labeled
// advisory and never presented as host evidence or successful tool execution.
func (result Result) Format() string {
	limit := result.ResultLimit
	if limit <= 0 {
		limit = DefaultMaxResultBytes
	}
	collector := newBoundedCollector(limit)
	completed := 0
	for _, expert := range result.Experts {
		if expert.Status == ExpertCompleted {
			completed++
		}
	}
	mode := "parallel"
	if result.Parallelism <= 1 {
		mode = "serial"
	}
	collector.Append("Expert consultation receipt (advisory; not verified evidence)\n")
	collector.Append(fmt.Sprintf("experts: total=%d · completed=%d · failed=%d\n", len(result.Experts), completed, len(result.Experts)-completed))
	collector.Append(fmt.Sprintf("strategy: %s · execution: %s · parallelism: %d\n", result.Strategy, mode, max(1, result.Parallelism)))
	collector.Append(fmt.Sprintf("resource policy: shared=%d · distinct-model=%d · bottleneck=%s\n",
		result.Plan.MaxConcurrentInference, result.Plan.MaxConcurrentDistinctModels, result.Plan.Bottleneck))
	for _, warning := range result.Warnings {
		collector.Append("resource warning: " + oneLine(warning) + "\n")
	}
	// Emit the complete bounded status projection before any advisory prose.
	// A large early report can therefore never hide later expert outcomes when
	// the overall tool result reaches its byte limit.
	for _, expert := range result.Experts {
		collector.Append(fmt.Sprintf("\n[%s · %s · %s · score %d]\n", oneLine(expert.Name), oneLine(expert.Model), expert.Status, expert.Score))
		collector.Append("selection: " + oneLine(expert.Reason) + "\n")
		if boundary := externalExecutionBoundary(expert.Location); boundary != "" {
			collector.Append(boundary + "\n")
		}
		if expert.ChargedEvalTokens > 0 {
			usage := fmt.Sprintf("usage: %d eval tokens", expert.ChargedEvalTokens)
			if expert.UsageEstimated {
				usage += " · conservative reservation"
			}
			collector.Append(usage + "\n")
		}
		if expert.Status != ExpertCompleted {
			collector.Append("error: " + oneLine(expert.ErrorCode) + "\n")
		}
	}
	for _, expert := range result.Experts {
		if expert.Status != ExpertCompleted {
			continue
		}
		collector.Append("\nreport: " + oneLine(expert.Name) + "\n")
		collector.Append(expert.Report + "\n")
	}
	return strings.TrimSpace(collector.String())
}

func externalExecutionBoundary(location llm.OllamaModelLocation) string {
	switch location {
	case llm.OllamaModelLocationCloud:
		return "execution boundary: CLOUD (model inference leaves this machine)"
	case llm.OllamaModelLocationRemote:
		return "execution boundary: REMOTE (model inference runs on a configured remote host)"
	default:
		return ""
	}
}

// ChargedUsage validates and aggregates every expert receipt. Custom runtime
// implementations cannot smuggle negative or overflowing usage into a parent
// Goal budget.
func (result Result) ChargedUsage() (Usage, error) {
	usage := Usage{}
	maxInt := int(^uint(0) >> 1)
	for _, expert := range result.Experts {
		if (expert.Status != ExpertCompleted && expert.Status != ExpertFailed) ||
			expert.EvalTokens < 0 || expert.ChargedEvalTokens < expert.EvalTokens || expert.PromptEvalTokens < 0 ||
			(expert.Status == ExpertCompleted && expert.ChargedEvalTokens == 0) ||
			expert.ChargedEvalTokens > maxInt-usage.EvalTokens ||
			expert.PromptEvalTokens > maxInt-usage.PromptEvalTokens {
			return Usage{}, errors.New("invalid expert usage receipt")
		}
		usage.EvalTokens += expert.ChargedEvalTokens
		usage.PromptEvalTokens += expert.PromptEvalTokens
	}
	return usage, nil
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(strings.ToValidUTF8(value, "�")), " ")
}

func boundUTF8Bytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
