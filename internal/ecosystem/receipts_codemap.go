package ecosystem

import (
	"encoding/json"
	"strings"
)

func projectCodemapReceipt(operation string, receipt RawReceipt) (DomainState, EvidenceState, bool) {
	document, ok := receiptDocument(receipt)
	if !ok || !strings.HasPrefix(operation, "codemap_") {
		return "", EvidenceNone, false
	}
	switch operation {
	case "codemap_annotate":
		return projectCodemapAnnotateReceipt(document)
	case "codemap_doctor":
		return projectCodemapDoctorReceipt(document)
	}
	var output struct {
		SchemaVersion *int  `json:"schema_version"`
		Registered    *bool `json:"registered"`
		Indexed       *bool `json:"indexed"`
		Stale         *struct {
			Changed int `json:"changed"`
			New     int `json:"new"`
			Deleted int `json:"deleted"`
		} `json:"stale"`
		FileStale     *bool           `json:"file_stale"`
		PartialErrors json.RawMessage `json:"partial_errors"`
		Confidence    *string         `json:"confidence"`
		CallGraph     *string         `json:"call_graph"`
		Error         any             `json:"error"`
		// Batch impact (ImpactBatchReport): item failures stay inside results.
		Requested *int            `json:"requested"`
		Processed *int            `json:"processed"`
		Truncated *bool           `json:"truncated"`
		Results   json.RawMessage `json:"results"`
	}
	if json.Unmarshal(document, &output) != nil {
		return "", EvidenceNone, false
	}
	if output.Error != nil {
		return DomainFailed, EvidenceNone, true
	}
	batch := jsonKind(output.Results, '[') && output.Requested != nil && output.Processed != nil &&
		*output.Requested >= 0 && *output.Processed >= 0
	recognized := batch
	if operation == "codemap_status" {
		recognized = output.Registered != nil || output.Indexed != nil
	} else {
		if output.SchemaVersion != nil {
			if *output.SchemaVersion != 1 {
				return "", EvidenceNone, false
			}
			recognized = true
		}
		recognized = recognized || output.FileStale != nil || output.Confidence != nil || output.CallGraph != nil || rawJSONPresent(output.PartialErrors)
	}
	if !recognized {
		return "", EvidenceNone, false
	}
	if output.Registered != nil && !*output.Registered || output.Indexed != nil && !*output.Indexed {
		return DomainBlocked, EvidenceNone, true
	}
	if output.FileStale != nil && *output.FileStale || output.Stale != nil && output.Stale.Changed+output.Stale.New+output.Stale.Deleted > 0 {
		return DomainAttention, EvidenceStale, true
	}
	if rawJSONArrayLen(output.PartialErrors) > 0 {
		return DomainAttention, EvidenceSupported, true
	}
	if batch {
		itemFailures, ok := codemapBatchItemFailures(output.Results)
		if !ok {
			return "", EvidenceNone, false
		}
		if itemFailures || output.Truncated != nil && *output.Truncated {
			// Partial coverage: some positions resolved, some did not (or the
			// batch hit its disclosure cap). The resolved subset stays useful.
			return DomainAttention, EvidenceSupported, true
		}
		return DomainSucceeded, EvidenceSupported, true
	}
	evidence := EvidenceSupported
	if output.Confidence != nil {
		switch *output.Confidence {
		case "candidate", "mixed":
			evidence = EvidenceCandidate
		case "none":
			evidence = EvidenceNone
		case "confirmed", "high", "medium", "low", "resolved":
		default:
			return "", EvidenceNone, false
		}
	}
	return DomainSucceeded, evidence, true
}

// projectCodemapAnnotateReceipt recognizes the idempotent annotation contract:
// action is the closed idempotency outcome, matched reports whether the
// target resolved to an indexed node.
func projectCodemapAnnotateReceipt(document json.RawMessage) (DomainState, EvidenceState, bool) {
	var output struct {
		Action  string `json:"action"`
		Target  string `json:"target"`
		Matched *bool  `json:"matched"`
		Error   any    `json:"error"`
	}
	if json.Unmarshal(document, &output) != nil {
		return "", EvidenceNone, false
	}
	if output.Error != nil {
		return DomainFailed, EvidenceNone, true
	}
	switch output.Action {
	case "created", "updated", "unchanged":
	default:
		return "", EvidenceNone, false
	}
	if strings.TrimSpace(output.Target) == "" {
		return "", EvidenceNone, false
	}
	if output.Matched != nil && !*output.Matched {
		return DomainAttention, EvidenceNone, true
	}
	return DomainSucceeded, EvidenceSupported, true
}

// projectCodemapDoctorReceipt reads the doctor checklist. Any failed check is
// attention — the report's agent_fix steps are advisory prose and stay
// outside the projection.
func projectCodemapDoctorReceipt(document json.RawMessage) (DomainState, EvidenceState, bool) {
	var output struct {
		Checks []struct {
			Name string `json:"name"`
			OK   *bool  `json:"ok"`
		} `json:"checks"`
		Error any `json:"error"`
	}
	if json.Unmarshal(document, &output) != nil {
		return "", EvidenceNone, false
	}
	if output.Error != nil {
		return DomainFailed, EvidenceNone, true
	}
	if len(output.Checks) == 0 {
		return "", EvidenceNone, false
	}
	failing := false
	for _, check := range output.Checks {
		if strings.TrimSpace(check.Name) == "" || check.OK == nil {
			return "", EvidenceNone, false
		}
		failing = failing || !*check.OK
	}
	if failing {
		return DomainAttention, EvidenceSupported, true
	}
	return DomainSucceeded, EvidenceSupported, true
}

// codemapBatchItemFailures scans batch results for item-level errors without
// retaining any item content.
func codemapBatchItemFailures(results json.RawMessage) (failures, ok bool) {
	var entries []struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(results, &entries) != nil {
		return false, false
	}
	for _, entry := range entries {
		if rawJSONPresent(entry.Error) {
			return true, true
		}
	}
	return false, true
}
