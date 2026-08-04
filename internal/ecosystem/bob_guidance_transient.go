package ecosystem

import (
	"encoding/json"
	"reflect"
)

const (
	maxTransientBobGuidanceBytes   = 8 * 1024
	maxTransientBobCapabilities    = 32
	maxTransientBobExtensionPoints = 16
	maxTransientBobPlaybooks       = 16
	maxTransientBobInputs          = 16
	maxTransientBobSteps           = 24
)

type bobTransientRecipe struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type bobTransientContext struct {
	Contract        string                        `json:"contract"`
	Recipe          bobTransientRecipe            `json:"recipe"`
	Repository      bobTransientRepository        `json:"repository"`
	Capabilities    []bobTransientCapability      `json:"capabilities,omitempty"`
	ExtensionPoints []string                      `json:"extension_points,omitempty"`
	Playbooks       []bobTransientPlaybookSummary `json:"playbooks,omitempty"`
	Truncated       bool                          `json:"truncated"`
}

type bobTransientRepository struct {
	State          string `json:"state"`
	ConflictCount  int64  `json:"conflict_count"`
	ManagedFiles   int64  `json:"managed_files"`
	ContractDigest string `json:"contract_digest"`
	ContextDigest  string `json:"context_digest"`
	PlanDigest     string `json:"plan_digest"`
}

type bobTransientCapability struct {
	ID              string `json:"id"`
	Selection       string `json:"selection"`
	Materialization string `json:"materialization"`
	Availability    string `json:"availability"`
}

type bobTransientPlaybookSummary struct {
	ID             string   `json:"id"`
	Applicable     bool     `json:"applicable"`
	Available      bool     `json:"available"`
	BlockedBy      []string `json:"blocked_by,omitempty"`
	RequiredInputs []string `json:"required_inputs,omitempty"`
	Scope          string   `json:"scope"`
	Risk           string   `json:"risk"`
}

type bobTransientSourcePlaybookSummary struct {
	ID             string   `json:"id"`
	Applicable     bool     `json:"applicable"`
	Available      bool     `json:"available"`
	BlockedBy      []string `json:"blocked_by"`
	RequiredInputs []string `json:"required_inputs"`
	Scope          string   `json:"scope_class"`
	Risk           string   `json:"risk"`
}

type bobTransientPath struct {
	Contract         string   `json:"contract"`
	Exists           bool     `json:"exists"`
	Classification   string   `json:"classification"`
	State            string   `json:"state"`
	HumanEditEffect  string   `json:"human_edit_effect"`
	ExtensionPoints  []string `json:"extension_points,omitempty"`
	RelatedPlaybooks []string `json:"related_playbooks,omitempty"`
	SuggestedAction  string   `json:"suggested_action,omitempty"`
	Truncated        bool     `json:"truncated"`
}

type bobTransientPlaybook struct {
	Contract  string                        `json:"contract"`
	Operation string                        `json:"operation"`
	Recipe    bobTransientRecipe            `json:"recipe"`
	Items     []bobTransientPlaybookSummary `json:"items,omitempty"`
	Playbook  *bobTransientPlaybookDetail   `json:"playbook,omitempty"`
	Truncated bool                          `json:"truncated"`
}

type bobTransientPlaybookDetail struct {
	ID         string                      `json:"id"`
	Applicable bool                        `json:"applicable"`
	Available  bool                        `json:"available"`
	BlockedBy  []string                    `json:"blocked_by,omitempty"`
	Scope      string                      `json:"scope"`
	Risk       string                      `json:"risk"`
	Inputs     []bobTransientPlaybookInput `json:"inputs,omitempty"`
	Steps      []bobTransientPlaybookStep  `json:"steps,omitempty"`
}

type bobTransientPlaybookInput struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Validation string `json:"validation"`
	Required   bool   `json:"required"`
}

type bobTransientPlaybookStep struct {
	ID                        string   `json:"id"`
	Kind                      string   `json:"kind"`
	Effect                    string   `json:"effect"`
	DependsOn                 []string `json:"depends_on,omitempty"`
	BlockedBy                 []string `json:"blocked_by,omitempty"`
	RequiresExplicitAuthority bool     `json:"requires_explicit_authority"`
}

func transientBobGuidanceContent(projection ToolProjection, receipt RawReceipt) (string, bool) {
	projection = projection.Normalize()
	if projection.Specialist != "bob" || !isBobGuidanceOperation(projection.Operation) ||
		projection.Role != RoleBuild || projection.Transport != TransportSucceeded ||
		!projection.DomainTyped || projection.Evidence != EvidenceNone || projection.Digest == nil {
		return "", false
	}
	domain, digest, ok := projectBobReceipt(projection.Operation, receipt)
	if !ok || digest == nil || domain != projection.Domain {
		return "", false
	}
	normalized := normalizeReceiptDigest(*digest)
	if !reflect.DeepEqual(projection.Digest, &normalized) || digest.Kind == DigestBobFailure {
		return "", false
	}
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", false
	}
	var output bobMCPReceipt
	if json.Unmarshal(document, &output) != nil {
		return "", false
	}
	var value any
	switch projection.Operation {
	case "bob_context":
		value, ok = buildTransientBobContext(output.Context)
	case "bob_path":
		value, ok = buildTransientBobPath(output.Path)
	case "bob_playbook":
		value, ok = buildTransientBobPlaybook(output)
	}
	if !ok {
		return "", false
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 {
		return "", false
	}
	transient := "Bob guidance (validated transient content; not saved)\n" + string(encoded)
	if len(transient) > maxTransientBobGuidanceBytes {
		return "", false
	}
	return transient, true
}

// BobContextWorkspace returns the exact transient workspace bound by a
// validated bob_context receipt. The absolute path is intentionally excluded
// from ToolProjection and every durable digest; callers may use this value only
// to bind an in-memory cache admission to the workspace that requested it.
func BobContextWorkspace(projection ToolProjection, receipt RawReceipt) (string, bool) {
	projection = projection.Normalize()
	if projection.Specialist != "bob" || projection.Operation != "bob_context" ||
		projection.Role != RoleBuild || projection.Transport != TransportSucceeded ||
		!projection.DomainTyped || projection.Evidence != EvidenceNone || projection.Digest == nil ||
		projection.Digest.Kind != DigestBobContext {
		return "", false
	}
	domain, digest, ok := projectBobReceipt("bob_context", receipt)
	if !ok || digest == nil || domain != projection.Domain {
		return "", false
	}
	normalized := normalizeReceiptDigest(*digest)
	if !reflect.DeepEqual(projection.Digest, &normalized) {
		return "", false
	}
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", false
	}
	var output bobMCPReceipt
	if json.Unmarshal(document, &output) != nil {
		return "", false
	}
	var context struct {
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal(output.Context, &context) != nil || !validBobWorkspace(context.Workspace) {
		return "", false
	}
	return context.Workspace, true
}

func buildTransientBobContext(raw json.RawMessage) (bobTransientContext, bool) {
	var value struct {
		ContractDigest string          `json:"contract_digest"`
		ContextDigest  string          `json:"context_digest"`
		Recipe         json.RawMessage `json:"recipe"`
		Repository     struct {
			State         string `json:"state"`
			ConflictCount int64  `json:"conflict_count"`
			ManagedFiles  int64  `json:"managed_files"`
			PlanDigest    string `json:"plan_digest"`
		} `json:"repository"`
		Capabilities []struct {
			ID              string `json:"id"`
			Selection       string `json:"selection"`
			Materialization string `json:"materialization"`
			Availability    string `json:"availability"`
		} `json:"capabilities"`
		ExtensionPoints []struct {
			ID string `json:"id"`
		} `json:"extension_points"`
		Playbooks  []bobTransientSourcePlaybookSummary `json:"playbooks"`
		Truncation struct {
			Truncated bool `json:"truncated"`
		} `json:"truncation"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return bobTransientContext{}, false
	}
	recipeID, recipeVersion, ok := bobDigestRecipe(value.Recipe)
	if !ok {
		return bobTransientContext{}, false
	}
	result := bobTransientContext{
		Contract: "bob_context.v1",
		Recipe:   bobTransientRecipe{ID: recipeID, Version: recipeVersion},
		Repository: bobTransientRepository{
			State: value.Repository.State, ConflictCount: value.Repository.ConflictCount,
			ManagedFiles: value.Repository.ManagedFiles, ContractDigest: value.ContractDigest,
			ContextDigest: value.ContextDigest, PlanDigest: value.Repository.PlanDigest,
		},
		Truncated: value.Truncation.Truncated,
	}
	for index, capability := range value.Capabilities {
		if index == maxTransientBobCapabilities {
			result.Truncated = true
			break
		}
		result.Capabilities = append(result.Capabilities, bobTransientCapability{
			ID: capability.ID, Selection: capability.Selection,
			Materialization: capability.Materialization, Availability: capability.Availability,
		})
	}
	for index, point := range value.ExtensionPoints {
		if index == maxTransientBobExtensionPoints {
			result.Truncated = true
			break
		}
		result.ExtensionPoints = append(result.ExtensionPoints, point.ID)
	}
	for index, playbook := range value.Playbooks {
		if index == maxTransientBobPlaybooks {
			result.Truncated = true
			break
		}
		var summaryTruncated bool
		result.Playbooks = append(result.Playbooks, boundTransientPlaybookSummary(playbook, &summaryTruncated))
		result.Truncated = result.Truncated || summaryTruncated
	}
	return result, true
}

func buildTransientBobPath(raw json.RawMessage) (bobTransientPath, bool) {
	var value struct {
		Exists           bool     `json:"exists"`
		Classification   string   `json:"classification"`
		State            string   `json:"state"`
		HumanEditEffect  string   `json:"human_edit_effect"`
		ExtensionPoints  []string `json:"extension_points"`
		RelatedPlaybooks []string `json:"related_playbooks"`
		Actions          []struct {
			ID string `json:"id"`
		} `json:"actions"`
		Truncation struct {
			Truncated bool `json:"truncated"`
		} `json:"truncation"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return bobTransientPath{}, false
	}
	result := bobTransientPath{
		Contract: "bob_path.v1", Exists: value.Exists, Classification: value.Classification,
		State: value.State, HumanEditEffect: value.HumanEditEffect, Truncated: value.Truncation.Truncated,
	}
	result.ExtensionPoints, result.Truncated = boundedTransientIDs(value.ExtensionPoints, maxTransientBobExtensionPoints, result.Truncated)
	result.RelatedPlaybooks, result.Truncated = boundedTransientIDs(value.RelatedPlaybooks, maxTransientBobPlaybooks, result.Truncated)
	if len(value.Actions) > 0 {
		result.SuggestedAction = value.Actions[0].ID
	}
	return result, true
}

func buildTransientBobPlaybook(output bobMCPReceipt) (bobTransientPlaybook, bool) {
	payloads := map[string]json.RawMessage{"list": output.List, "show": output.Show, "plan": output.Plan}
	var value struct {
		Recipe    json.RawMessage                     `json:"recipe"`
		Playbooks []bobTransientSourcePlaybookSummary `json:"playbooks"`
		Playbook  struct {
			ID         string                      `json:"id"`
			Applicable bool                        `json:"applicable"`
			Available  bool                        `json:"available"`
			BlockedBy  []string                    `json:"blocked_by"`
			ScopeClass string                      `json:"scope_class"`
			Risk       string                      `json:"risk"`
			Inputs     []bobTransientPlaybookInput `json:"inputs"`
			Steps      []bobTransientPlaybookStep  `json:"steps"`
		} `json:"playbook"`
		Truncation struct {
			Truncated bool `json:"truncated"`
		} `json:"truncation"`
	}
	if json.Unmarshal(payloads[output.Operation], &value) != nil {
		return bobTransientPlaybook{}, false
	}
	recipeID, recipeVersion, ok := bobDigestRecipe(value.Recipe)
	if !ok {
		return bobTransientPlaybook{}, false
	}
	result := bobTransientPlaybook{
		Contract: "bob_playbook.v1", Operation: output.Operation,
		Recipe:    bobTransientRecipe{ID: recipeID, Version: recipeVersion},
		Truncated: value.Truncation.Truncated,
	}
	if output.Operation == "list" {
		for index, playbook := range value.Playbooks {
			if index == maxTransientBobPlaybooks {
				result.Truncated = true
				break
			}
			var summaryTruncated bool
			result.Items = append(result.Items, boundTransientPlaybookSummary(playbook, &summaryTruncated))
			result.Truncated = result.Truncated || summaryTruncated
		}
		return result, true
	}
	detail := &bobTransientPlaybookDetail{
		ID: value.Playbook.ID, Applicable: value.Playbook.Applicable, Available: value.Playbook.Available,
		BlockedBy: append([]string(nil), value.Playbook.BlockedBy...), Scope: value.Playbook.ScopeClass, Risk: value.Playbook.Risk,
	}
	for index, input := range value.Playbook.Inputs {
		if index == maxTransientBobInputs {
			result.Truncated = true
			break
		}
		detail.Inputs = append(detail.Inputs, input)
	}
	for index, step := range value.Playbook.Steps {
		if index == maxTransientBobSteps {
			result.Truncated = true
			break
		}
		detail.Steps = append(detail.Steps, step)
	}
	result.Playbook = detail
	return result, true
}

func boundTransientPlaybookSummary(value bobTransientSourcePlaybookSummary, truncated *bool) bobTransientPlaybookSummary {
	blocked, blockedTruncated := boundedTransientIDs(value.BlockedBy, maxProjectionDigestItems, false)
	required, requiredTruncated := boundedTransientIDs(value.RequiredInputs, maxProjectionDigestItems, false)
	if truncated != nil {
		*truncated = blockedTruncated || requiredTruncated
	}
	return bobTransientPlaybookSummary{
		ID: value.ID, Applicable: value.Applicable, Available: value.Available,
		BlockedBy: blocked, RequiredInputs: required, Scope: value.Scope, Risk: value.Risk,
	}
}

func boundedTransientIDs(values []string, maximum int, truncated bool) ([]string, bool) {
	if len(values) > maximum {
		values = values[:maximum]
		truncated = true
	}
	return append([]string(nil), values...), truncated
}
