package ecosystem

import (
	"encoding/json"
	"strings"
)

func projectVerifierReceipt(specialist, operation string, receipt RawReceipt) (DomainState, EvidenceState, bool) {
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", EvidenceNone, false
	}
	if specialist == "glyphrun" || specialist == "glyph" {
		return projectGlyphReceipt(operation, document)
	}
	return projectCairnReceipt(operation, document)
}

func projectGlyphReceipt(operation string, document json.RawMessage) (DomainState, EvidenceState, bool) {
	if operation != "glyph_run" && operation != "glyphrun_run" && !strings.HasSuffix(operation, "_glyph_run") {
		return "", EvidenceNone, false
	}
	var envelope struct {
		SchemaVersion int             `json:"schemaVersion"`
		RunID         string          `json:"runId"`
		SpecName      string          `json:"specName"`
		Status        string          `json:"status"`
		StartedAt     string          `json:"startedAt"`
		EndedAt       string          `json:"endedAt"`
		DurationMS    *int64          `json:"durationMs"`
		Target        json.RawMessage `json:"target"`
		Terminal      json.RawMessage `json:"terminal"`
		Outcomes      json.RawMessage `json:"outcomes"`
		Artifacts     json.RawMessage `json:"artifacts"`
		RunDir        string          `json:"runDir"`
		ExitCode      *int            `json:"exitCode"`
	}
	if json.Unmarshal(document, &envelope) != nil || envelope.SchemaVersion != 1 ||
		envelope.RunID == "" || envelope.SpecName == "" || envelope.StartedAt == "" || envelope.EndedAt == "" ||
		envelope.DurationMS == nil || *envelope.DurationMS < 0 || envelope.RunDir == "" || envelope.ExitCode == nil ||
		!jsonKind(envelope.Target, '{') || !jsonKind(envelope.Terminal, '{') ||
		!jsonKind(envelope.Outcomes, '[') || !jsonKind(envelope.Artifacts, '{') {
		return "", EvidenceNone, false
	}
	if envelope.Status == "passed" && (*envelope.ExitCode != 0 || !allOutcomeStatuses(envelope.Outcomes, "passed")) {
		return DomainUnknown, EvidenceNone, true
	}
	return projectRunStatuses([]string{envelope.Status})
}

func projectCairnReceipt(operation string, document json.RawMessage) (DomainState, EvidenceState, bool) {
	if operation != "cairn_run" && operation != "cairntrace_run" && !strings.HasSuffix(operation, "_cairn_run") {
		return "", EvidenceNone, false
	}
	var envelope struct {
		Schema          string            `json:"$schema"`
		Version         string            `json:"version"`
		Status          string            `json:"status"`
		Reason          string            `json:"reason"`
		RunID           string            `json:"runId"`
		RunDir          string            `json:"runDir"`
		Spec            json.RawMessage   `json:"spec"`
		Environment     string            `json:"environment"`
		Backend         string            `json:"backend"`
		ColdStart       *bool             `json:"coldStart"`
		StartedAt       string            `json:"startedAt"`
		EndedAt         string            `json:"endedAt"`
		DurationMS      *int64            `json:"durationMs"`
		Outcomes        json.RawMessage   `json:"outcomes"`
		Steps           json.RawMessage   `json:"steps"`
		Artifacts       json.RawMessage   `json:"artifacts"`
		ExitCode        *int              `json:"exitCode"`
		Parallel        *int              `json:"parallel"`
		TotalDurationMS *int64            `json:"totalDurationMs"`
		Summary         json.RawMessage   `json:"summary"`
		Results         []json.RawMessage `json:"results"`
	}
	if json.Unmarshal(document, &envelope) != nil {
		return "", EvidenceNone, false
	}
	if envelope.Schema == "urn:cairntrace.dev:run:v1" && envelope.Version == "1" {
		if !validCairnRun(envelope.RunID, envelope.RunDir, envelope.Spec, envelope.Environment, envelope.Backend,
			envelope.ColdStart, envelope.StartedAt, envelope.EndedAt, envelope.DurationMS,
			envelope.Outcomes, envelope.Steps, envelope.Artifacts, envelope.ExitCode) {
			return "", EvidenceNone, false
		}
		if envelope.Status == "passed" && (*envelope.ExitCode != 0 || !allOutcomeStatuses(envelope.Outcomes, "passed", "skipped")) {
			return DomainUnknown, EvidenceNone, true
		}
		return projectRunStatuses([]string{envelope.Status})
	}
	if envelope.Schema == "urn:cairntrace.dev:run-batch:v1" && envelope.Version == "1" && len(envelope.Results) > 0 &&
		envelope.Parallel != nil && *envelope.Parallel > 0 && envelope.TotalDurationMS != nil && *envelope.TotalDurationMS >= 0 &&
		jsonKind(envelope.Summary, '{') && envelope.ExitCode != nil {
		statuses := make([]string, 0, len(envelope.Results))
		for _, raw := range envelope.Results {
			domain, _, recognized := projectCairnReceipt(operation, raw)
			if !recognized {
				return "", EvidenceNone, false
			}
			switch domain {
			case DomainSucceeded:
				statuses = append(statuses, "passed")
			case DomainFailed:
				statuses = append(statuses, "failed")
			default:
				return DomainAttention, EvidenceNone, true
			}
		}
		if *envelope.ExitCode == 0 {
			for _, status := range statuses {
				if status != "passed" {
					return DomainUnknown, EvidenceNone, true
				}
			}
		}
		return projectRunStatuses(statuses)
	}
	if envelope.Status == "skipped" && envelope.Reason == "not_in_blast_radius" {
		return DomainAttention, EvidenceNone, true
	}
	return "", EvidenceNone, false
}

func projectRunStatuses(statuses []string) (DomainState, EvidenceState, bool) {
	if len(statuses) == 0 {
		return "", EvidenceNone, false
	}
	for _, status := range statuses {
		switch status {
		case "failed":
			return DomainFailed, EvidenceContradicted, true
		case "errored":
			return DomainFailed, EvidenceNone, true
		case "skipped":
			return DomainAttention, EvidenceNone, true
		case "passed":
		default:
			return "", EvidenceNone, false
		}
	}
	return DomainSucceeded, EvidenceVerified, true
}

func allOutcomeStatuses(raw json.RawMessage, allowed ...string) bool {
	if !jsonKind(raw, '[') {
		return false
	}
	var outcomes []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &outcomes) != nil {
		return false
	}
	set := make(map[string]struct{}, len(allowed))
	for _, value := range allowed {
		set[value] = struct{}{}
	}
	for _, outcome := range outcomes {
		if outcome.ID == "" {
			return false
		}
		if _, ok := set[outcome.Status]; !ok {
			return false
		}
	}
	return true
}

func validCairnRun(runID, runDir string, spec json.RawMessage, environment, backend string, coldStart *bool,
	startedAt, endedAt string, durationMS *int64, outcomes, steps, artifacts json.RawMessage, exitCode *int,
) bool {
	if runID == "" || runDir == "" || environment == "" || backend == "" || coldStart == nil ||
		startedAt == "" || endedAt == "" || durationMS == nil || *durationMS < 0 || exitCode == nil ||
		!jsonKind(spec, '{') || !jsonKind(outcomes, '[') || !jsonKind(steps, '[') || !jsonKind(artifacts, '{') {
		return false
	}
	var ref struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	return json.Unmarshal(spec, &ref) == nil && ref.Name != "" && ref.Path != ""
}
