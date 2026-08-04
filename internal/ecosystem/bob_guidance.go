package ecosystem

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const maxBobGuidanceDocumentBytes = 72 << 10

func isBobGuidanceOperation(operation string) bool {
	switch operation {
	case "bob_context", "bob_path", "bob_playbook":
		return true
	default:
		return false
	}
}

func projectBobGuidanceReceipt(operation string, document json.RawMessage, output bobMCPReceipt) (DomainState, *ReceiptDigest, bool) {
	if len(document) > maxBobGuidanceDocumentBytes || !validBobAuthority(output.Authority, "", false) {
		return "", nil, false
	}
	if output.Error != nil {
		if output.OK || strings.TrimSpace(output.Error.Code) == "" || strings.TrimSpace(output.Error.Code) != output.Error.Code ||
			strings.TrimSpace(output.Error.Message) == "" || bobGuidancePayloadPresent(document) ||
			!validBobGuidanceErrorCode(operation, output.Operation, output.Error.Code) {
			return "", nil, false
		}
		if operation != "bob_playbook" && jsonObjectHasKey(document, "operation") {
			return "", nil, false
		}
		return classifyBobErrorCode(output.Error.Code), &ReceiptDigest{Kind: DigestBobFailure, Target: output.Error.Code}, true
	}
	if !output.OK {
		return "", nil, false
	}

	switch operation {
	case "bob_context":
		domain, ok := validBobContextResult(document, output)
		if !ok {
			return "", nil, false
		}
		digest, ok := bobContextDigest(output.Context)
		return domain, digest, ok
	case "bob_path":
		if validBobPathResult(document, output) {
			domain, digest, ok := bobPathDigest(output.Path)
			return domain, digest, ok
		}
	case "bob_playbook":
		domain, ok := validBobPlaybookResult(document, output)
		if !ok {
			return "", nil, false
		}
		digest, ok := bobPlaybookDigest(output)
		return domain, digest, ok
	}
	return "", nil, false
}

// validBobGuidanceErrorCode is the exact Bob v0.4.0 guidance error vocabulary.
// Operation-specific fallbacks are deliberately not interchangeable: accepting
// arbitrary canonical-looking codes would turn future or spoofed schemas into
// durable host state.
func validBobGuidanceErrorCode(operation, playbookOperation, code string) bool {
	common := oneOf(code, "input_invalid", "workspace_invalid", "workspace_unauthorized", "manifest_invalid")
	switch operation {
	case "bob_context":
		return playbookOperation == "" && (common || code == "context_failed")
	case "bob_path":
		return playbookOperation == "" && (common || code == "path_failed")
	case "bob_playbook":
		if code == "input_invalid" {
			// Bob echoes the rejected operation verbatim for invalid-input
			// failures, including an empty or future value. It is never persisted.
			// In v0.4.0, missing input names exist only in Error.Message and share
			// this code with unknown playbooks/values. We therefore map the exact
			// code coarsely to DomainBlocked and never parse prose into Digest.Required.
			// LA-2 can expose named missing inputs only after Bob adds a typed field
			// or distinct closed code.
			return len(playbookOperation) <= maxBobGuidanceDocumentBytes && utf8.ValidString(playbookOperation) &&
				!strings.ContainsRune(playbookOperation, '\x00')
		}
		return oneOf(playbookOperation, "list", "show", "plan") && (common || code == "playbook_failed")
	default:
		return false
	}
}

func bobGuidancePayloadPresent(document json.RawMessage) bool {
	for _, key := range []string{"context", "path", "list", "show", "plan"} {
		if jsonObjectHasValue(document, key) {
			return true
		}
	}
	return false
}

func validBobContextResult(document json.RawMessage, output bobMCPReceipt) (DomainState, bool) {
	if !jsonObjectHasValue(document, "context") || jsonObjectHasKey(document, "path") ||
		jsonObjectHasKey(document, "operation") || jsonObjectHasKey(document, "list") ||
		jsonObjectHasKey(document, "show") || jsonObjectHasKey(document, "plan") {
		return "", false
	}
	var value struct {
		SchemaVersion   int             `json:"schema_version"`
		Profile         string          `json:"profile"`
		Workspace       string          `json:"workspace"`
		ContractDigest  string          `json:"contract_digest"`
		ContextDigest   string          `json:"context_digest"`
		Recipe          json.RawMessage `json:"recipe"`
		Product         json.RawMessage `json:"product"`
		Repository      json.RawMessage `json:"repository"`
		Capabilities    json.RawMessage `json:"capabilities"`
		EntryPoints     json.RawMessage `json:"entry_points"`
		ExtensionPoints json.RawMessage `json:"extension_points"`
		Invariants      json.RawMessage `json:"invariants"`
		Playbooks       json.RawMessage `json:"playbooks"`
		Artifacts       json.RawMessage `json:"artifacts"`
		Notices         json.RawMessage `json:"notices"`
		Actions         json.RawMessage `json:"actions"`
		Truncation      json.RawMessage `json:"truncation"`
	}
	if json.Unmarshal(output.Context, &value) != nil || value.SchemaVersion != 1 ||
		!oneOf(value.Profile, "compact", "standard", "full") || !validBobWorkspace(value.Workspace) ||
		!validPrefixedSHA256(value.ContractDigest) || !validPrefixedSHA256(value.ContextDigest) {
		return "", false
	}
	if !validBobAuthority(output.Authority, value.Workspace, true) || !validBobRecipeRefOnly(value.Recipe) ||
		!validBobContextProduct(value.Product) || !validBobContextCollection(value.Capabilities, validBobContextCapability, 256, false) ||
		!validBobContextCollection(value.EntryPoints, validBobEntryPoint, 256, true) ||
		!validBobContextCollection(value.ExtensionPoints, validBobExtensionPoint, 256, true) ||
		!validBobContextCollection(value.Invariants, validBobInvariant, 256, false) ||
		!validBobContextCollection(value.Playbooks, validBobPlaybookSummary, 256, false) ||
		!validBobContextCollection(value.Notices, validBobNotice, 256, true) ||
		!validBobContextCollection(value.Actions, func(raw json.RawMessage) bool { return validBobGuidanceAction(raw, value.Workspace) }, 256, true) ||
		!validBobGuidanceTruncation(value.Truncation, value.Profile, bobContextProfileLimit(value.Profile)) {
		return "", false
	}
	if value.Profile == "full" {
		if !validBobContextCollection(value.Artifacts, validBobArtifact, 1024, true) {
			return "", false
		}
	} else if jsonObjectHasKey(output.Context, "artifacts") {
		return "", false
	}
	domain, ok := validBobContextRepository(value.Repository)
	return domain, ok
}

func validBobPathResult(document json.RawMessage, output bobMCPReceipt) bool {
	if !jsonObjectHasValue(document, "path") || jsonObjectHasKey(document, "context") ||
		jsonObjectHasKey(document, "operation") || jsonObjectHasKey(document, "list") ||
		jsonObjectHasKey(document, "show") || jsonObjectHasKey(document, "plan") {
		return false
	}
	var value struct {
		SchemaVersion    int             `json:"schema_version"`
		Workspace        string          `json:"workspace"`
		Path             string          `json:"path"`
		Exists           *bool           `json:"exists"`
		Classification   string          `json:"classification"`
		State            string          `json:"state"`
		HumanEditEffect  string          `json:"human_edit_effect"`
		Ownership        json.RawMessage `json:"ownership"`
		PlanAction       json.RawMessage `json:"plan_action"`
		Artifact         json.RawMessage `json:"artifact"`
		ExtensionPoints  json.RawMessage `json:"extension_points"`
		RelatedPlaybooks json.RawMessage `json:"related_playbooks"`
		Notices          json.RawMessage `json:"notices"`
		Actions          json.RawMessage `json:"actions"`
		Truncation       json.RawMessage `json:"truncation"`
	}
	if json.Unmarshal(output.Path, &value) != nil || value.SchemaVersion != 1 || !validBobWorkspace(value.Workspace) ||
		!validBobActionPath(value.Path) || value.Exists == nil || !validBobPathState(value.Classification, value.State, value.HumanEditEffect, *value.Exists) ||
		!validBobAuthority(output.Authority, value.Workspace, true) || !validBobPathOwnership(value.Ownership) ||
		!validBobIDArray(value.ExtensionPoints, 128, true) || !validBobIDArray(value.RelatedPlaybooks, 128, true) ||
		!validBobContextCollection(value.Notices, validBobNotice, 128, true) ||
		!validBobContextCollection(value.Actions, func(raw json.RawMessage) bool { return validBobGuidanceAction(raw, value.Workspace) }, 128, true) ||
		!validBobGuidanceTruncation(value.Truncation, "path", 8<<10) {
		return false
	}
	if rawJSONPresent(value.PlanAction) && !validBobPathPlanAction(value.PlanAction) {
		return false
	}
	if rawJSONPresent(value.Artifact) && !validBobArtifact(value.Artifact) {
		return false
	}
	return true
}

func validBobPlaybookResult(document json.RawMessage, output bobMCPReceipt) (DomainState, bool) {
	if !oneOf(output.Operation, "list", "show", "plan") || jsonObjectHasKey(document, "context") || jsonObjectHasKey(document, "path") {
		return "", false
	}
	payloads := map[string]json.RawMessage{"list": output.List, "show": output.Show, "plan": output.Plan}
	for name, raw := range payloads {
		if (name == output.Operation) != rawJSONPresent(raw) {
			return "", false
		}
	}
	raw := payloads[output.Operation]
	var common struct {
		SchemaVersion int             `json:"schema_version"`
		Workspace     string          `json:"workspace"`
		Recipe        json.RawMessage `json:"recipe"`
		Playbooks     json.RawMessage `json:"playbooks"`
		Playbook      json.RawMessage `json:"playbook"`
		Observations  json.RawMessage `json:"observations"`
		Values        json.RawMessage `json:"values"`
		Truncation    json.RawMessage `json:"truncation"`
	}
	if json.Unmarshal(raw, &common) != nil || common.SchemaVersion != 1 || !validBobWorkspace(common.Workspace) ||
		!validBobAuthority(output.Authority, common.Workspace, true) || !validBobRecipeRefOnly(common.Recipe) {
		return "", false
	}
	switch output.Operation {
	case "list":
		if jsonObjectHasKey(raw, "playbook") || jsonObjectHasKey(raw, "observations") || jsonObjectHasKey(raw, "values") ||
			!validBobContextCollection(common.Playbooks, validBobPlaybookSummary, 256, true) ||
			!validBobGuidanceTruncation(common.Truncation, "list", 8<<10) {
			return "", false
		}
		return DomainSucceeded, true
	case "show", "plan":
		if jsonObjectHasKey(raw, "playbooks") || !validBobStringValueArray(common.Observations, validBobObservation, 128, true) ||
			!validBobGuidanceTruncation(common.Truncation, output.Operation, 24<<10) {
			return "", false
		}
		available, blocked, ok := validBobPlaybookDefinition(common.Playbook)
		if !ok {
			return "", false
		}
		if output.Operation == "plan" {
			if !validBobPlanValues(common.Playbook, common.Values) {
				return "", false
			}
		} else if jsonObjectHasKey(raw, "values") {
			return "", false
		}
		if !available || blocked {
			return DomainBlocked, true
		}
		return DomainSucceeded, true
	}
	return "", false
}

func validBobContextRepository(raw json.RawMessage) (DomainState, bool) {
	var value struct {
		State             string `json:"state"`
		Clean             *bool  `json:"clean"`
		LockChanged       *bool  `json:"lock_changed"`
		ConflictCount     *int   `json:"conflict_count"`
		ManagedFiles      *int   `json:"managed_files"`
		PlanDigestVersion int    `json:"plan_digest_version"`
		PlanDigest        string `json:"plan_digest"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Clean == nil || value.LockChanged == nil || value.ConflictCount == nil ||
		value.ManagedFiles == nil || *value.ConflictCount < 0 || *value.ManagedFiles < 0 || value.PlanDigestVersion != 1 ||
		!validPrefixedSHA256(value.PlanDigest) {
		return "", false
	}
	switch value.State {
	case "clean":
		return DomainSucceeded, *value.Clean && !*value.LockChanged && *value.ConflictCount == 0
	case "drifted":
		return DomainDrift, !*value.Clean && *value.ConflictCount == 0
	case "conflicted":
		return DomainConflict, !*value.Clean && *value.ConflictCount > 0
	default:
		return "", false
	}
}

func bobContextDigest(raw json.RawMessage) (*ReceiptDigest, bool) {
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
		Capabilities    []json.RawMessage `json:"capabilities"`
		ExtensionPoints []struct {
			ID string `json:"id"`
		} `json:"extension_points"`
		Playbooks []json.RawMessage `json:"playbooks"`
		Actions   []struct {
			ID string `json:"id"`
		} `json:"actions"`
		Truncation struct {
			Truncated bool `json:"truncated"`
		} `json:"truncation"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	recipeID, recipeVersion, ok := bobDigestRecipe(value.Recipe)
	if !ok {
		return nil, false
	}
	extensionIDs := make([]string, 0, len(value.ExtensionPoints))
	for _, point := range value.ExtensionPoints {
		extensionIDs = append(extensionIDs, point.ID)
	}
	firstAction := ""
	if len(value.Actions) > 0 {
		firstAction = value.Actions[0].ID
	}
	return &ReceiptDigest{
		Kind:           DigestBobContext,
		RecipeID:       recipeID,
		RecipeVersion:  recipeVersion,
		State:          value.Repository.State,
		ContractDigest: value.ContractDigest,
		ContextDigest:  value.ContextDigest,
		PlanDigest:     value.Repository.PlanDigest,
		ConflictCount:  value.Repository.ConflictCount,
		ManagedFiles:   value.Repository.ManagedFiles,
		Capabilities:   int64(len(value.Capabilities)),
		ExtensionCount: int64(len(value.ExtensionPoints)),
		PlaybookCount:  int64(len(value.Playbooks)),
		Items:          extensionIDs,
		FirstAction:    firstAction,
		Truncated:      value.Truncation.Truncated || len(extensionIDs) > maxProjectionDigestItems,
	}, true
}

func bobPathDigest(raw json.RawMessage) (DomainState, *ReceiptDigest, bool) {
	var value struct {
		Exists           *bool    `json:"exists"`
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
	if json.Unmarshal(raw, &value) != nil || value.Exists == nil {
		return "", nil, false
	}
	domain := DomainSucceeded
	if oneOf(value.HumanEditEffect, "will_conflict", "reserved_for_bob", "requires_manifest_change", "unsafe") {
		domain = DomainAttention
	}
	items := append(append([]string(nil), value.ExtensionPoints...), value.RelatedPlaybooks...)
	firstAction := ""
	if len(value.Actions) > 0 {
		firstAction = value.Actions[0].ID
	}
	return domain, &ReceiptDigest{
		Kind:           DigestBobPath,
		Classification: value.Classification,
		State:          value.State,
		Effect:         value.HumanEditEffect,
		Items:          items,
		FirstAction:    firstAction,
		ExtensionCount: int64(len(value.ExtensionPoints)),
		PlaybookCount:  int64(len(value.RelatedPlaybooks)),
		Exists:         *value.Exists,
		Truncated:      value.Truncation.Truncated || len(items) > maxProjectionDigestItems,
	}, true
}

func bobPlaybookDigest(output bobMCPReceipt) (*ReceiptDigest, bool) {
	payloads := map[string]json.RawMessage{"list": output.List, "show": output.Show, "plan": output.Plan}
	raw := payloads[output.Operation]
	var common struct {
		Recipe    json.RawMessage `json:"recipe"`
		Playbooks []struct {
			ID string `json:"id"`
		} `json:"playbooks"`
		Playbook struct {
			ID         string   `json:"id"`
			ScopeClass string   `json:"scope_class"`
			Risk       string   `json:"risk"`
			Applicable *bool    `json:"applicable"`
			Available  *bool    `json:"available"`
			BlockedBy  []string `json:"blocked_by"`
			Inputs     []struct {
				Name     string `json:"name"`
				Required *bool  `json:"required"`
			} `json:"inputs"`
			Steps []struct {
				ID     string `json:"id"`
				Effect string `json:"effect"`
			} `json:"steps"`
		} `json:"playbook"`
		Values     map[string]string `json:"values"`
		Truncation struct {
			Truncated bool `json:"truncated"`
		} `json:"truncation"`
	}
	if json.Unmarshal(raw, &common) != nil {
		return nil, false
	}
	recipeID, recipeVersion, ok := bobDigestRecipe(common.Recipe)
	if !ok {
		return nil, false
	}
	digest := &ReceiptDigest{
		Kind:          DigestBobPlaybook,
		RecipeID:      recipeID,
		RecipeVersion: recipeVersion,
		State:         output.Operation,
		Truncated:     common.Truncation.Truncated,
	}
	if output.Operation == "list" {
		digest.Count = int64(len(common.Playbooks))
		digest.PlaybookCount = digest.Count
		for _, playbook := range common.Playbooks {
			digest.Items = append(digest.Items, playbook.ID)
		}
		digest.Truncated = digest.Truncated || len(common.Playbooks) > maxProjectionDigestItems
		return digest, true
	}
	if common.Playbook.Applicable == nil || common.Playbook.Available == nil {
		return nil, false
	}
	digest.Target = common.Playbook.ID
	digest.Scope = common.Playbook.ScopeClass
	digest.Risk = common.Playbook.Risk
	digest.Available = *common.Playbook.Available
	digest.Blocked = len(common.Playbook.BlockedBy) > 0
	switch {
	case digest.Blocked:
		digest.State = "blocked"
	case !*common.Playbook.Applicable || !*common.Playbook.Available:
		digest.State = "unavailable"
	default:
		digest.State = "ready"
	}
	for _, input := range common.Playbook.Inputs {
		if input.Required == nil || !*input.Required {
			continue
		}
		if output.Operation == "plan" {
			if _, supplied := common.Values[input.Name]; supplied {
				continue
			}
		}
		digest.Required = append(digest.Required, input.Name)
	}
	digest.Truncated = digest.Truncated || len(digest.Required) > maxProjectionDigestItems
	digest.Count = int64(len(common.Playbook.Steps))
	if len(common.Playbook.Steps) > 0 {
		digest.FirstAction = common.Playbook.Steps[0].ID
		digest.Effect = common.Playbook.Steps[0].Effect
	}
	return digest, true
}

func bobDigestRecipe(raw json.RawMessage) (string, int64, bool) {
	var value struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	if json.Unmarshal(raw, &value) != nil || value.ID == "" || value.Version <= 0 {
		return "", 0, false
	}
	return value.ID, value.Version, true
}

func validBobContextProduct(raw json.RawMessage) bool {
	var value struct {
		Name       string `json:"name"`
		Module     string `json:"module"`
		Runtime    string `json:"runtime"`
		Kind       string `json:"kind"`
		Visibility string `json:"visibility"`
	}
	return json.Unmarshal(raw, &value) == nil && validBoundedText(value.Name, 256, true) &&
		validBoundedText(value.Module, 512, false) && validBoundedText(value.Runtime, 64, false) &&
		validBoundedText(value.Kind, 64, false) && validBoundedText(value.Visibility, 64, false)
}

func validBobContextCapability(raw json.RawMessage) bool {
	var value struct {
		ID, Selection, Materialization, Availability, Verification string
	}
	return json.Unmarshal(raw, &value) == nil && validCanonicalBobID(value.ID) &&
		oneOf(value.Selection, "required", "enabled", "disabled") &&
		oneOf(value.Materialization, "in_sync", "missing", "drifted", "conflicted", "unknown", "not_applicable") &&
		oneOf(value.Availability, "available", "unavailable", "not_checked", "not_applicable") && value.Verification == "not_assessed"
}

func validBobEntryPoint(raw json.RawMessage) bool {
	var value struct{ ID, Path, Ownership string }
	return json.Unmarshal(raw, &value) == nil && validCanonicalBobID(value.ID) && validBobActionPath(value.Path) &&
		oneOf(value.Ownership, "bob_whole_file", "human")
}

func validBobExtensionPoint(raw json.RawMessage) bool {
	var value struct {
		ID, Ownership  string
		CreatePatterns json.RawMessage `json:"create_patterns"`
	}
	return json.Unmarshal(raw, &value) == nil && validCanonicalBobID(value.ID) && value.Ownership == "human" &&
		validBobStringArray(value.CreatePatterns, 128, 4096, true)
}

func validBobInvariant(raw json.RawMessage) bool {
	var value struct{ ID, Statement string }
	return json.Unmarshal(raw, &value) == nil && validCanonicalBobID(value.ID) && validBoundedText(value.Statement, 4096, true)
}

func validBobPlaybookSummary(raw json.RawMessage) bool {
	var value struct {
		ID             string          `json:"id"`
		ScopeClass     string          `json:"scope_class"`
		Risk           string          `json:"risk"`
		Applicable     *bool           `json:"applicable"`
		Available      *bool           `json:"available"`
		BlockedBy      json.RawMessage `json:"blocked_by"`
		RequiredInputs json.RawMessage `json:"required_inputs"`
	}
	if json.Unmarshal(raw, &value) != nil || !validCanonicalBobID(value.ID) || value.Applicable == nil || value.Available == nil ||
		!oneOf(value.ScopeClass, "metadata_only", "small", "single_file", "multi_surface", "repository_wide") ||
		!oneOf(value.Risk, "low", "medium", "high") || !validBobIDArray(value.BlockedBy, 128, true) ||
		!validBobIDArray(value.RequiredInputs, 32, true) {
		return false
	}
	return *value.Available != (lenRawJSONArray(value.BlockedBy) > 0)
}

func validBobArtifact(raw json.RawMessage) bool {
	var value struct {
		ID            string          `json:"id"`
		Path          string          `json:"path"`
		Ownership     string          `json:"ownership"`
		Roles         json.RawMessage `json:"roles"`
		CapabilityIDs json.RawMessage `json:"capability_ids"`
	}
	return json.Unmarshal(raw, &value) == nil && validCanonicalBobID(value.ID) &&
		(value.Path == "" || validBobActionPath(value.Path)) &&
		(value.Ownership == "" || oneOf(value.Ownership, "bob_whole_file", "human")) &&
		validBobStringArray(value.Roles, 128, 256, true) && validBobStringArray(value.CapabilityIDs, 256, 256, true)
}

func validBobNotice(raw json.RawMessage) bool {
	var value struct {
		ID, Severity, Code, Message string
		Paths                       json.RawMessage
	}
	return json.Unmarshal(raw, &value) == nil && validCanonicalBobID(value.ID) && oneOf(value.Severity, "info", "warning", "error") &&
		validCanonicalBobID(value.Code) && validBoundedText(value.Message, 4096, true) && validOptionalBobStringArray(value.Paths, 128, 4096)
}

func validBobGuidanceAction(raw json.RawMessage, workspace string) bool {
	var value struct {
		ID                        string          `json:"id"`
		Kind                      string          `json:"kind"`
		Effect                    string          `json:"effect"`
		CWD                       string          `json:"cwd"`
		ReasonCode                string          `json:"reason_code"`
		Argv                      json.RawMessage `json:"argv"`
		BlockedBy                 json.RawMessage `json:"blocked_by"`
		RequiresExplicitAuthority *bool           `json:"requires_explicit_authority"`
	}
	if json.Unmarshal(raw, &value) != nil || !validCanonicalBobID(value.ID) || !validCanonicalBobID(value.Kind) ||
		!oneOf(value.Effect, "read_only", "subprocess_probe", "repository_mutation") || value.CWD != workspace ||
		!validCanonicalBobID(value.ReasonCode) || value.RequiresExplicitAuthority == nil ||
		!validBobStringArray(value.Argv, 64, 4096, true) || !validBobIDArray(value.BlockedBy, 64, true) {
		return false
	}
	return value.Effect != "repository_mutation" || *value.RequiresExplicitAuthority
}

func validBobPathOwnership(raw json.RawMessage) bool {
	var value struct {
		Recipe        json.RawMessage `json:"recipe"`
		LockedSHA256  string          `json:"locked_sha256"`
		CurrentSHA256 string          `json:"current_sha256"`
	}
	if json.Unmarshal(raw, &value) != nil || !validBobRecipeRefOnly(value.Recipe) {
		return false
	}
	return (value.LockedSHA256 == "" || validLowerHexDigest(value.LockedSHA256)) &&
		(value.CurrentSHA256 == "" || validLowerHexDigest(value.CurrentSHA256))
}

func validBobPathPlanAction(raw json.RawMessage) bool {
	var value struct{ Kind, Code string }
	return json.Unmarshal(raw, &value) == nil && validBobActionKindCode(value.Kind, value.Code)
}

func validBobPathState(classification, state, effect string, exists bool) bool {
	switch classification {
	case "managed":
		switch state {
		case "managed_in_sync", "managed_modified":
			return exists && effect == "will_conflict"
		case "managed_missing":
			return !exists && effect == "will_conflict"
		case "retired_owned":
			return effect == "reserved_for_bob"
		case "symlink", "special_file":
			return exists && effect == "will_conflict"
		}
	case "reserved":
		return state == "reserved" && oneOf(effect, "requires_manifest_change", "reserved_for_bob", "unsafe")
	case "unmanaged":
		return (state == "unmanaged_present" && exists && effect == "outside_bob_ownership") ||
			((state == "symlink" || state == "special_file") && exists && effect == "unsafe")
	case "missing":
		return state == "unmanaged_missing" && !exists && effect == "outside_bob_ownership"
	case "extension_point":
		return state == "extension_point" && effect == "outside_bob_ownership"
	}
	return false
}

func validBobPlaybookDefinition(raw json.RawMessage) (bool, bool, bool) {
	var value struct {
		ID                string          `json:"id"`
		Title             string          `json:"title"`
		Purpose           string          `json:"purpose"`
		ScopeClass        string          `json:"scope_class"`
		Risk              string          `json:"risk"`
		Applicable        *bool           `json:"applicable"`
		Available         *bool           `json:"available"`
		BlockedBy         json.RawMessage `json:"blocked_by"`
		Inputs            json.RawMessage `json:"inputs"`
		Preconditions     json.RawMessage `json:"preconditions"`
		Boundary          json.RawMessage `json:"boundary"`
		Steps             json.RawMessage `json:"steps"`
		VerificationHints json.RawMessage `json:"verification_hints"`
		FailureModes      json.RawMessage `json:"failure_modes"`
		CapabilityIDs     json.RawMessage `json:"capability_ids"`
		ExtensionPointIDs json.RawMessage `json:"extension_point_ids"`
	}
	if json.Unmarshal(raw, &value) != nil || !validCanonicalBobID(value.ID) || !validBoundedText(value.Title, 512, true) ||
		!validBoundedText(value.Purpose, 4096, true) || value.Applicable == nil || value.Available == nil ||
		!oneOf(value.ScopeClass, "metadata_only", "small", "single_file", "multi_surface", "repository_wide") || !oneOf(value.Risk, "low", "medium", "high") ||
		!validBobIDArray(value.BlockedBy, 128, true) || !validBobPlaybookInputs(value.Inputs) ||
		!validBobStringArray(value.Preconditions, 128, 4096, true) || !validBobPlaybookBoundary(value.Boundary) ||
		!validBobStringValueArray(value.Steps, validBobPlaybookStep, 128, true) ||
		!validBobStringArray(value.VerificationHints, 128, 4096, true) || !validBobStringArray(value.FailureModes, 128, 4096, true) ||
		!validBobIDArray(value.CapabilityIDs, 256, true) || !validBobIDArray(value.ExtensionPointIDs, 256, true) {
		return false, false, false
	}
	if !validBobPlaybookStepGraph(value.Steps) {
		return false, false, false
	}
	blocked := lenRawJSONArray(value.BlockedBy) > 0
	if *value.Available == blocked {
		return false, false, false
	}
	return *value.Available, blocked, true
}

func validBobPlaybookInput(raw json.RawMessage) bool {
	var value struct {
		Name       string          `json:"name"`
		Type       string          `json:"type"`
		Validation string          `json:"validation"`
		Required   *bool           `json:"required"`
		Enum       json.RawMessage `json:"enum"`
		Forbidden  json.RawMessage `json:"forbidden"`
	}
	if json.Unmarshal(raw, &value) != nil || !validCanonicalBobID(value.Name) || value.Required == nil ||
		!validOptionalBobStringArray(value.Enum, 128, 256) || !validOptionalBobStringArray(value.Forbidden, 128, 256) {
		return false
	}
	switch value.Type {
	case "identifier":
		return value.Validation == "lowercase-kebab" && !rawJSONPresent(value.Enum)
	case "repository_path":
		return value.Validation == "safe-relative-path" && !rawJSONPresent(value.Enum)
	case "enum":
		return value.Validation == "closed-enum" && validBobStringArray(value.Enum, 128, 256, false)
	default:
		return false
	}
}

func validBobPlaybookInputs(raw json.RawMessage) bool {
	if !validBobStringValueArray(raw, validBobPlaybookInput, 32, true) {
		return false
	}
	var inputs []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &inputs) != nil {
		return false
	}
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if _, duplicate := seen[input.Name]; duplicate {
			return false
		}
		seen[input.Name] = struct{}{}
	}
	return true
}

func validBobPlaybookBoundary(raw json.RawMessage) bool {
	var value struct{ Create, Modify, Forbidden json.RawMessage }
	return json.Unmarshal(raw, &value) == nil && validBobStringArray(value.Create, 256, 4096, true) &&
		validBobStringArray(value.Modify, 256, 4096, true) && validBobStringArray(value.Forbidden, 256, 4096, true)
}

func validBobPlaybookStep(raw json.RawMessage) bool {
	var value struct {
		ID                        string          `json:"id"`
		Kind                      string          `json:"kind"`
		Effect                    string          `json:"effect"`
		Summary                   string          `json:"summary"`
		SuccessCondition          string          `json:"success_condition"`
		Paths                     json.RawMessage `json:"paths"`
		Argv                      json.RawMessage `json:"argv"`
		DependsOn                 json.RawMessage `json:"depends_on"`
		BlockedBy                 json.RawMessage `json:"blocked_by"`
		RequiresExplicitAuthority *bool           `json:"requires_explicit_authority"`
	}
	if json.Unmarshal(raw, &value) != nil || !validCanonicalBobID(value.ID) ||
		!oneOf(value.Kind, "inspect", "agent_edit", "manifest_edit", "command", "bob_plan", "bob_apply", "bob_check", "human_decision") ||
		!oneOf(value.Effect, "read_only", "subprocess_probe", "repository_mutation", "user_configuration_mutation") || !validBoundedText(value.Summary, 4096, true) ||
		!validBoundedText(value.SuccessCondition, 4096, true) || value.RequiresExplicitAuthority == nil ||
		!validBobStringArray(value.Paths, 256, 4096, true) || !validBobStringArray(value.Argv, 128, 4096, true) ||
		!validBobIDArray(value.DependsOn, 128, true) || !validBobIDArray(value.BlockedBy, 128, true) {
		return false
	}
	return !oneOf(value.Effect, "repository_mutation", "user_configuration_mutation") || *value.RequiresExplicitAuthority
}

func validBobPlaybookStepGraph(raw json.RawMessage) bool {
	var steps []struct {
		ID        string   `json:"id"`
		DependsOn []string `json:"depends_on"`
	}
	if json.Unmarshal(raw, &steps) != nil {
		return false
	}
	known := make(map[string]struct{}, len(steps))
	for _, step := range steps {
		if _, duplicate := known[step.ID]; duplicate {
			return false
		}
		known[step.ID] = struct{}{}
	}
	for _, step := range steps {
		for _, dependency := range step.DependsOn {
			if _, exists := known[dependency]; !exists {
				return false
			}
		}
	}
	return true
}

func validBobObservation(raw json.RawMessage) bool {
	var value struct{ ID, Value string }
	return json.Unmarshal(raw, &value) == nil && validCanonicalBobID(value.ID) && validBoundedText(value.Value, 4096, true)
}

func validBobValues(raw json.RawMessage) bool {
	if !jsonKind(raw, '{') {
		return false
	}
	var values map[string]string
	if json.Unmarshal(raw, &values) != nil || len(values) > 32 {
		return false
	}
	for key, value := range values {
		if !validCanonicalBobID(key) || !validBoundedText(value, 4096, false) {
			return false
		}
	}
	return true
}

func validBobPlanValues(playbook, raw json.RawMessage) bool {
	if !validBobValues(raw) {
		return false
	}
	var definition struct {
		Inputs []struct {
			Name      string   `json:"name"`
			Type      string   `json:"type"`
			Required  bool     `json:"required"`
			Enum      []string `json:"enum"`
			Forbidden []string `json:"forbidden"`
		} `json:"inputs"`
	}
	var values map[string]string
	if json.Unmarshal(playbook, &definition) != nil || json.Unmarshal(raw, &values) != nil {
		return false
	}
	inputs := make(map[string]struct {
		typeName  string
		required  bool
		enum      []string
		forbidden []string
	}, len(definition.Inputs))
	for _, input := range definition.Inputs {
		inputs[input.Name] = struct {
			typeName  string
			required  bool
			enum      []string
			forbidden []string
		}{input.Type, input.Required, input.Enum, input.Forbidden}
	}
	for name, value := range values {
		input, known := inputs[name]
		if !known || containsBobString(input.forbidden, value) {
			return false
		}
		switch input.typeName {
		case "identifier":
			if !validBobKebabValue(value) {
				return false
			}
		case "repository_path":
			if value == "." || !validBobActionPath(value) || filepath.Clean(value) != value {
				return false
			}
		case "enum":
			if !containsBobString(input.enum, value) {
				return false
			}
		default:
			return false
		}
	}
	for name, input := range inputs {
		if input.required {
			if _, supplied := values[name]; !supplied {
				return false
			}
		}
	}
	return true
}

func validBobKebabValue(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	previousHyphen := false
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			previousHyphen = false
		case character == '-' && !previousHyphen:
			previousHyphen = true
		default:
			return false
		}
	}
	return !previousHyphen
}

func containsBobString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validBobGuidanceTruncation(raw json.RawMessage, profile string, byteLimit int) bool {
	var value struct {
		Profile   string         `json:"profile"`
		ByteLimit int            `json:"byte_limit"`
		Truncated *bool          `json:"truncated"`
		Omitted   map[string]int `json:"omitted"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Profile != profile || value.ByteLimit != byteLimit || value.Truncated == nil || value.Omitted == nil {
		return false
	}
	for key, count := range value.Omitted {
		if !validCanonicalBobID(key) || count <= 0 || int64(count) > maxProjectionMetric {
			return false
		}
	}
	return *value.Truncated == (len(value.Omitted) > 0)
}

func validBobContextCollection(raw json.RawMessage, validate func(json.RawMessage) bool, maximum int, allowEmpty bool) bool {
	return validBobStringValueArray(raw, validate, maximum, allowEmpty)
}

func validBobStringValueArray(raw json.RawMessage, validate func(json.RawMessage) bool, maximum int, allowEmpty bool) bool {
	if !jsonKind(raw, '[') {
		return false
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || len(values) > maximum || (!allowEmpty && len(values) == 0) {
		return false
	}
	for _, value := range values {
		if !validate(value) {
			return false
		}
	}
	return true
}

func validBobStringArray(raw json.RawMessage, maximumItems, maximumBytes int, allowEmpty bool) bool {
	return validBobStringValueArray(raw, func(value json.RawMessage) bool {
		var text string
		return json.Unmarshal(value, &text) == nil && validBoundedText(text, maximumBytes, true)
	}, maximumItems, allowEmpty)
}

func validBobIDArray(raw json.RawMessage, maximumItems int, allowEmpty bool) bool {
	return validBobStringValueArray(raw, func(value json.RawMessage) bool {
		var id string
		return json.Unmarshal(value, &id) == nil && validCanonicalBobID(id)
	}, maximumItems, allowEmpty)
}

func validOptionalBobStringArray(raw json.RawMessage, maximumItems, maximumBytes int) bool {
	return !rawJSONPresent(raw) || validBobStringArray(raw, maximumItems, maximumBytes, true)
}

func lenRawJSONArray(raw json.RawMessage) int {
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return -1
	}
	return len(values)
}

func validBobRecipeRefOnly(raw json.RawMessage) bool {
	_, ok := validBobRecipeRef(raw)
	return ok
}

func validPrefixedSHA256(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validLowerHexDigest(strings.TrimPrefix(value, "sha256:"))
}

func validCanonicalBobID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
			character == '_' || character == '-' || character == '.' || character == ':' {
			continue
		}
		return false
	}
	return canonicalIdentifier(value) == value
}

func validBoundedText(value string, maximumBytes int, required bool) bool {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || len(value) > maximumBytes {
		return false
	}
	if required {
		return strings.TrimSpace(value) != ""
	}
	return value == "" || strings.TrimSpace(value) != ""
}

func bobContextProfileLimit(profile string) int {
	switch profile {
	case "compact":
		return 6144
	case "standard":
		return 24 << 10
	case "full":
		return 64 << 10
	default:
		return 0
	}
}
