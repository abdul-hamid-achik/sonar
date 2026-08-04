package ecosystem

import (
	"encoding/json"
	"strings"
)

// projectVecgrepReceipt recognizes vecgrep's readiness contract — the only
// structured payload its MCP surface attaches, and only on vecgrep_search,
// vecgrep_ensure, and vecgrep_status. The envelope carries no schema marker,
// so recognition requires the complete field shape; anything less stays
// unknown. Search hits themselves remain candidate evidence: retrieval never
// proves behavior.
func projectVecgrepReceipt(operation string, receipt RawReceipt) (DomainState, EvidenceState, bool) {
	document, ok := receiptDocument(receipt)
	if !ok || !strings.HasPrefix(operation, "vecgrep_") {
		return "", EvidenceNone, false
	}
	switch operation {
	case "vecgrep_search", "vecgrep_ensure", "vecgrep_status":
	default:
		return "", EvidenceNone, false
	}
	var output struct {
		State           string  `json:"state"`
		Indexed         *bool   `json:"indexed"`
		Fresh           *bool   `json:"fresh"`
		Chunks          *int64  `json:"chunks"`
		ProfileMatches  *bool   `json:"profile_matches"`
		Action          *string `json:"action"`
		Reason          *string `json:"reason"`
		StoredProfileID *string `json:"stored_profile_id"`
		ActiveProfileID *string `json:"active_profile_id"`
		Error           any     `json:"error"`
	}
	if json.Unmarshal(document, &output) != nil {
		return "", EvidenceNone, false
	}
	if output.Error != nil {
		return DomainFailed, EvidenceNone, true
	}
	if output.Indexed == nil || output.Fresh == nil || output.Chunks == nil ||
		output.ProfileMatches == nil || *output.Chunks < 0 {
		return "", EvidenceNone, false
	}
	switch output.State {
	case "empty", "profile_mismatch":
		// BlocksSearch states: the index cannot answer until the agent runs
		// the advertised repair action (vecgrep_index / force reindex).
		return DomainBlocked, EvidenceNone, true
	case "unknown":
		return DomainAttention, EvidenceNone, true
	case "stale":
		// A stale index still answers, but its hits describe an older tree.
		return DomainAttention, EvidenceStale, true
	case "ready":
		if *output.Indexed && *output.Fresh && *output.ProfileMatches {
			return DomainSucceeded, EvidenceCandidate, true
		}
		// A ready claim that contradicts its own facts is not a contract we
		// recognize.
		return "", EvidenceNone, false
	default:
		return "", EvidenceNone, false
	}
}
