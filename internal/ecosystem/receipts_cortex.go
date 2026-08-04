package ecosystem

import (
	"encoding/json"
)

// cortexEnvelopeOperations lists the cortex operations whose results carry the
// shared lifecycle envelope (cortex_status embeds it in its StatusReport).
// List/read projections with their own shapes stay outside this catalog and
// therefore remain DomainUnknown until they gain exact parsers.
var cortexEnvelopeOperations = map[string]bool{
	"cortex_open_task":        true,
	"cortex_start_task":       true,
	"cortex_investigate":      true,
	"cortex_plan":             true,
	"cortex_begin_change":     true,
	"cortex_verify":           true,
	"cortex_remember":         true,
	"cortex_resolve":          true,
	"cortex_note":             true,
	"cortex_request_decision": true,
	"cortex_answer_decision":  true,
	"cortex_abort_task":       true,
	"cortex_status":           true,
}

// projectCortexReceipt recognizes the shared cortex lifecycle envelope for the
// catalogued operations. ok=true with a task identity is coordination success
// — deliberately never verification evidence, so the evidence state stays
// untouched. Structurally unrecognized documents report false and stay on the
// fail-closed unknown path.
func projectCortexReceipt(operation string, receipt RawReceipt) (DomainState, *ReceiptDigest, bool) {
	if operation == "cortex_handoff" {
		return projectCortexHandoffReceipt(receipt)
	}
	if !cortexEnvelopeOperations[operation] {
		return "", nil, false
	}
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", nil, false
	}
	var envelope struct {
		OK     *bool   `json:"ok"`
		TaskID *string `json:"taskId"`
		Phase  *string `json:"phase"`
	}
	if json.Unmarshal(document, &envelope) != nil || envelope.OK == nil || envelope.TaskID == nil {
		return "", nil, false
	}
	taskID := canonicalIdentifier(*envelope.TaskID)
	if !*envelope.OK {
		return DomainFailed, &ReceiptDigest{Kind: DigestCortexFailure, Target: taskID}, true
	}
	if taskID == "" {
		return "", nil, false
	}
	digest := &ReceiptDigest{Kind: DigestCortexReceipt, Target: taskID}
	if envelope.Phase != nil {
		if phase := canonicalIdentifier(*envelope.Phase); phase != "" {
			digest.Items = []string{phase}
		}
	}
	return DomainSucceeded, digest, true
}

// projectCortexHandoffReceipt recognizes the handoff export, which
// deliberately differs from the shared lifecycle envelope: it is keyed on
// schemaVersion and carries the exported case's identity, revision, and
// phase. A handoff is a coordination artifact — proof closure, receipts, and
// hypotheses stay inside the short-lived parser boundary and never become
// evidence by export alone.
func projectCortexHandoffReceipt(receipt RawReceipt) (DomainState, *ReceiptDigest, bool) {
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", nil, false
	}
	var envelope struct {
		SchemaVersion *int   `json:"schemaVersion"`
		TaskID        string `json:"taskId"`
		Revision      *int64 `json:"revision"`
		Phase         string `json:"phase"`
	}
	if json.Unmarshal(document, &envelope) != nil ||
		envelope.SchemaVersion == nil || *envelope.SchemaVersion != 1 {
		return "", nil, false
	}
	taskID := canonicalIdentifier(envelope.TaskID)
	phase := canonicalIdentifier(envelope.Phase)
	if taskID == "" || taskID != envelope.TaskID || phase == "" ||
		envelope.Revision == nil || *envelope.Revision < 0 {
		return "", nil, false
	}
	return DomainSucceeded, &ReceiptDigest{Kind: DigestCortexReceipt, Target: taskID, Items: []string{phase}}, true
}

// projectCortexFailureReceipt recognizes only the shared, explicit rejection
// envelope. It intentionally does not promote ok=true to success: coordination
// success and verified evidence are different claims and need operation-specific
// parsers. Error prose and summaries remain outside the persisted projection.
func projectCortexFailureReceipt(receipt RawReceipt) (string, bool) {
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", false
	}
	var envelope struct {
		OK     *bool  `json:"ok"`
		TaskID string `json:"taskId"`
	}
	if json.Unmarshal(document, &envelope) != nil || envelope.OK == nil || *envelope.OK {
		return "", false
	}
	return canonicalIdentifier(envelope.TaskID), true
}
