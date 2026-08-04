package ecosystem

import (
	"encoding/json"
	"strings"
)

func projectVidtraceReceipt(operation string, receipt RawReceipt) (DomainState, EvidenceState, bool) {
	document, ok := receiptDocument(receipt)
	if !ok || !strings.HasPrefix(operation, "vidtrace_") {
		return "", EvidenceNone, false
	}
	var output struct {
		OK           *bool `json:"ok"`
		Error        any   `json:"error"`
		ConnectError any   `json:"connect_error"`
		CodemapError any   `json:"codemap_error"`
	}
	if json.Unmarshal(document, &output) != nil {
		return "", EvidenceNone, false
	}
	if output.Error != nil || output.OK != nil && !*output.OK {
		return DomainFailed, EvidenceNone, true
	}
	if output.ConnectError != nil || output.CodemapError != nil {
		return DomainAttention, EvidenceCandidate, true
	}
	if output.OK == nil {
		return "", EvidenceNone, false
	}
	return DomainSucceeded, EvidenceSupported, true
}
