package ecosystem

import (
	"encoding/json"
	"strings"
)

func projectMonitorReceipt(operation string, receipt RawReceipt) (DomainState, EvidenceState, *ArtifactDigest, bool) {
	document, ok := receiptDocument(receipt)
	if !ok || !strings.HasPrefix(operation, "monitor_") {
		return "", EvidenceNone, nil, false
	}
	var output struct {
		SchemaVersion    *int            `json:"schema_version"`
		Kind             string          `json:"kind"`
		Summary          string          `json:"summary"`
		Hostname         string          `json:"hostname"`
		CPU              json.RawMessage `json:"cpu"`
		Processes        json.RawMessage `json:"processes"`
		Total            *int            `json:"total"`
		Truncated        *bool           `json:"truncated"`
		Reason           string          `json:"reason"`
		Healthy          *bool           `json:"healthy"`
		Samples          *int            `json:"samples"`
		Diagnoses        json.RawMessage `json:"diagnoses"`
		Error            any             `json:"error"`
		Refused          bool            `json:"refused"`
		Limitation       string          `json:"limitation"`
		Outcome          string          `json:"outcome"`
		Captured         *bool           `json:"captured"`
		Recording        *bool           `json:"recording"`
		ArtifactVerified *bool           `json:"artifact_verified"`
		Artifact         struct {
			Verified *bool `json:"verified"`
		} `json:"artifact"`
		Verdict string `json:"verdict"`
		// Issue intelligence (monitor_issues / monitor_issue).
		Issues               json.RawMessage `json:"issues"`
		Issue                json.RawMessage `json:"issue"`
		Occurrences          json.RawMessage `json:"occurrences"`
		OccurrencesTruncated *bool           `json:"occurrences_truncated"`
		// ArtifactRef is the ArtifactRefV1 pointer a completed incident
		// investigation emits after stashing its evidence bundle.
		ArtifactRef json.RawMessage `json:"artifact_ref"`
	}
	if json.Unmarshal(document, &output) != nil {
		return "", EvidenceNone, nil, false
	}
	if output.Refused {
		return DomainBlocked, EvidenceNone, nil, true
	}
	if output.Error != nil {
		return DomainFailed, EvidenceNone, nil, true
	}
	if output.Limitation != "" {
		return DomainAttention, EvidenceNone, nil, true
	}
	switch operation {
	case "monitor_snapshot":
		compact := output.SchemaVersion != nil && *output.SchemaVersion == 1 && output.Kind == "monitor.compact_snapshot"
		full := output.Summary != "" && (output.Hostname != "" || jsonKind(output.CPU, '{'))
		if !compact && !full {
			return "", EvidenceNone, nil, false
		}
		return DomainSucceeded, EvidenceSupported, nil, true
	case "monitor_processes":
		if !jsonKind(output.Processes, '[') || output.Total == nil || output.Truncated == nil ||
			(output.Reason != "top_cpu" && output.Reason != "top_rss" && output.Reason != "filtered") {
			return "", EvidenceNone, nil, false
		}
		return DomainSucceeded, EvidenceSupported, nil, true
	case "monitor_doctor":
		var tools map[string]struct {
			Available *bool `json:"available"`
		}
		if json.Unmarshal(document, &tools) != nil {
			return "", EvidenceNone, nil, false
		}
		recognized, unavailable := false, false
		for _, name := range []string{"codemap", "fcheap", "vecgrep", "tinyvault", "vidtrace", "glyphrun", "cairntrace", "veclite", "tmux"} {
			if status, ok := tools[name]; ok && status.Available != nil {
				recognized = true
				unavailable = unavailable || !*status.Available
			}
		}
		if !recognized {
			return "", EvidenceNone, nil, false
		}
		if unavailable {
			return DomainAttention, EvidenceSupported, nil, true
		}
		return DomainSucceeded, EvidenceSupported, nil, true
	case "monitor_analyze":
		if output.Healthy == nil || output.Samples == nil || *output.Samples < 0 || !jsonKind(output.Diagnoses, '[') {
			return "", EvidenceNone, nil, false
		}
		if !*output.Healthy {
			return DomainAttention, EvidenceSupported, nil, true
		}
		return DomainSucceeded, EvidenceSupported, nil, true
	case "monitor_issues":
		if !jsonKind(output.Issues, '[') || output.Total == nil || *output.Total < 0 || output.Truncated == nil {
			return "", EvidenceNone, nil, false
		}
		anyOpen, ok := monitorIssuesContainOpen(output.Issues)
		if !ok {
			return "", EvidenceNone, nil, false
		}
		if anyOpen {
			return DomainAttention, EvidenceSupported, nil, true
		}
		return DomainSucceeded, EvidenceSupported, nil, true
	case "monitor_issue":
		if !jsonKind(output.Issue, '{') || !jsonKind(output.Occurrences, '[') || output.OccurrencesTruncated == nil {
			return "", EvidenceNone, nil, false
		}
		status, ok := monitorIssueStatus(output.Issue)
		if !ok {
			return "", EvidenceNone, nil, false
		}
		if status == "open" {
			return DomainAttention, EvidenceSupported, nil, true
		}
		return DomainSucceeded, EvidenceSupported, nil, true
	case "monitor_kill":
		switch output.Outcome {
		case "terminated":
			return DomainSucceeded, EvidenceVerified, nil, true
		case "still_running":
			return DomainFailed, EvidenceContradicted, nil, true
		case "unknown":
			return DomainUnknown, EvidenceNone, nil, true
		default:
			return "", EvidenceNone, nil, false
		}
	case "monitor_profile_capture":
		if output.Captured == nil || output.Artifact.Verified == nil {
			return "", EvidenceNone, nil, false
		}
		if *output.Captured && *output.Artifact.Verified {
			return DomainSucceeded, EvidenceVerified, nil, true
		}
		return DomainAttention, EvidenceNone, nil, true
	case "monitor_investigate":
		// A completed investigation may carry the ArtifactRefV1 pointer of its
		// stashed evidence bundle. Monitor emits fcheap-local refs only; a
		// present-but-invalid ref invalidates the whole receipt fail-closed.
		var artifact *ArtifactDigest
		if rawJSONPresent(output.ArtifactRef) {
			digest, ok := artifactDigestFromRef(output.ArtifactRef)
			if !ok || digest.Provider != artifactRefProviderLocal {
				return "", EvidenceNone, nil, false
			}
			artifact = digest
		}
		switch output.Verdict {
		case "complete":
			return DomainSucceeded, EvidenceSupported, artifact, true
		case "partial":
			return DomainAttention, EvidenceSupported, artifact, true
		default:
			return "", EvidenceNone, nil, false
		}
	case "monitor_record":
		if output.Recording == nil || output.ArtifactVerified == nil {
			return "", EvidenceNone, nil, false
		}
		if *output.Recording && *output.ArtifactVerified {
			return DomainSucceeded, EvidenceVerified, nil, true
		}
		return DomainAttention, EvidenceNone, nil, true
	default:
		return "", EvidenceNone, nil, false
	}
}

// monitorIssuesContainOpen decodes only each issue's status. Any element that
// does not carry a valid status invalidates the receipt: the projection never
// guesses severity from a shape it does not fully recognize.
func monitorIssuesContainOpen(issues json.RawMessage) (anyOpen, ok bool) {
	var entries []struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(issues, &entries) != nil {
		return false, false
	}
	for _, entry := range entries {
		switch entry.Status {
		case "open":
			anyOpen = true
		case "resolved", "ignored":
		default:
			return false, false
		}
	}
	return anyOpen, true
}

func monitorIssueStatus(issue json.RawMessage) (string, bool) {
	var entry struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if json.Unmarshal(issue, &entry) != nil || strings.TrimSpace(entry.ID) == "" {
		return "", false
	}
	switch entry.Status {
	case "open", "resolved", "ignored":
		return entry.Status, true
	default:
		return "", false
	}
}
