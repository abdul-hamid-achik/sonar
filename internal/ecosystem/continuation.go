package ecosystem

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxContinuationDocumentBytes  = 72 << 10
	maxContinuationActions        = 16
	maxContinuationArguments      = 16
	maxContinuationArgumentsBytes = 16 << 10
	maxContinuationCollection     = 32
	maxContinuationValueDepth     = 4
	maxContinuationValueNodes     = 128
	maxContinuationValueBytes     = 8 << 10
	maxContinuationStringBytes    = 4 << 10
	maxContinuationTextBytes      = 1024
	maxContinuationFieldBytes     = 96
)

// BoundedValueKind identifies the closed JSON value forms accepted at the
// continuation parser boundary. Null is intentionally absent: a trusted action
// must distinguish a missing input from a concrete value.
type BoundedValueKind string

const (
	BoundedString BoundedValueKind = "string"
	BoundedBool   BoundedValueKind = "bool"
	BoundedNumber BoundedValueKind = "number"
	BoundedArray  BoundedValueKind = "array"
	BoundedObject BoundedValueKind = "object"
)

// BoundedField is one deterministic field in a bounded object value. Fields
// are sorted by Name, which makes fingerprints independent of JSON map order.
type BoundedField struct {
	Name  string
	Value BoundedValue
}

// BoundedValue is a host-owned, deeply bounded copy of one trusted action
// argument. It never retains json.RawMessage or an arbitrary downstream map.
type BoundedValue struct {
	Kind   BoundedValueKind
	Text   string
	Bool   bool
	Number string
	Items  []BoundedValue
	Fields []BoundedField
}

// Any returns a fresh ordinary Go value suitable for a later registry/schema
// validation pass. Mutating the returned slices or maps cannot mutate the
// bounded continuation candidate.
func (v BoundedValue) Any() any {
	switch v.Kind {
	case BoundedString:
		return v.Text
	case BoundedBool:
		return v.Bool
	case BoundedNumber:
		// jsonschema-go validates the ordinary values produced by encoding/json;
		// json.Number is string-backed and can otherwise be mistaken for a JSON
		// string. The parser has already proved the lexical number is finite.
		if integer, err := strconv.ParseInt(v.Number, 10, 64); err == nil {
			return integer
		}
		if unsigned, err := strconv.ParseUint(v.Number, 10, 64); err == nil {
			return unsigned
		}
		if number, err := strconv.ParseFloat(v.Number, 64); err == nil {
			return number
		}
		return nil
	case BoundedArray:
		values := make([]any, len(v.Items))
		for index := range v.Items {
			values[index] = v.Items[index].Any()
		}
		return values
	case BoundedObject:
		values := make(map[string]any, len(v.Fields))
		for _, field := range v.Fields {
			values[field.Name] = field.Value.Any()
		}
		return values
	default:
		return nil
	}
}

// ContinuationAction is a transient, host-owned candidate derived from an
// exact trusted companion contract. It is not an authority grant: the agent
// must still resolve Tool in its current registry, validate ArgumentValues
// against that tool's current input schema, and apply host approval policy.
// Human command/reason prose is deliberately not retained here.
type ContinuationAction struct {
	Source          string
	SourceOperation string
	Tool            string
	Arguments       map[string]BoundedValue
	Inputs          []string
	BlockedBy       []string
	ReasonCode      string
	WorkspaceRef    string
	TaskID          string
	SourceRevision  uint64
	ContextDigest   string
}

// ArgumentValues returns a fresh ordinary argument map for a later exact tool
// schema validation pass.
func (a ContinuationAction) ArgumentValues() map[string]any {
	values := make(map[string]any, len(a.Arguments))
	for name, value := range a.Arguments {
		values[name] = value.Any()
	}
	return values
}

// ProjectContinuationActions extracts bounded continuation candidates from a
// receipt only after the receipt has produced the exact trusted semantic
// projection supplied by the caller. Unknown routes, schemas, action tools,
// argument fields, or Bob semantic tuples fail closed to no candidates.
func ProjectContinuationActions(projection ToolProjection, receipt RawReceipt) []ContinuationAction {
	projection, ok := exactContinuationProjection(projection, receipt)
	if !ok {
		return nil
	}
	document, ok := receiptDocument(receipt)
	if !ok || len(document) > maxContinuationDocumentBytes {
		return nil
	}
	switch projection.Specialist {
	case "cortex":
		return projectCortexContinuationActions(projection, document)
	case "bob":
		return projectBobContinuationActions(projection, document)
	default:
		return nil
	}
}

// ReceiptHasContinuationActions reports only the presence of a top-level
// Cortex actions surface on an otherwise host-routed receipt. It intentionally
// does not require the actions to validate: callers use it to suppress raw
// command/reason prose even when exact continuation parsing fails closed.
func ReceiptHasContinuationActions(projection ToolProjection, receipt RawReceipt) bool {
	if projection.Specialist != "cortex" || projection.Role != RoleCoordination ||
		projection.Route.Server != "cortex" || projection.Route.Tool != projection.Operation {
		return false
	}
	document, ok := receiptDocument(receipt)
	if !ok || len(document) > maxContinuationDocumentBytes {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(document)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	for decoder.More() {
		nameToken, nameErr := decoder.Token()
		name, nameOK := nameToken.(string)
		if nameErr != nil || !nameOK {
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
		if name == "actions" {
			return true
		}
	}
	return false
}

func exactContinuationProjection(projection ToolProjection, receipt RawReceipt) (ToolProjection, bool) {
	if receipt.TransportError || receipt.ToolError || len(receipt.ErrorMeta) > 0 ||
		!reflect.DeepEqual(projection, projection.Normalize()) ||
		projection.Transport != TransportSucceeded || !projection.DomainTyped || projection.Evidence != EvidenceNone ||
		projection.Route.Server != projection.Specialist || projection.Route.Tool != projection.Operation ||
		(projection.Route.Lazy && projection.Route.Gateway == "") {
		return ToolProjection{}, false
	}
	switch projection.Specialist {
	case "cortex":
		if projection.Role != RoleCoordination || projection.Domain != DomainSucceeded ||
			projection.Digest == nil || projection.Digest.Kind != DigestCortexReceipt {
			return ToolProjection{}, false
		}
	case "bob":
		if projection.Role != RoleBuild || projection.Digest == nil ||
			(projection.Operation != "bob_context" && projection.Operation != "bob_path" && projection.Operation != "bob_playbook") {
			return ToolProjection{}, false
		}
	default:
		return ToolProjection{}, false
	}

	base := ProjectToolCall(projection.Specialist+"__"+projection.Operation, nil)
	base.Route = projection.Route
	reparsed := ProjectReceipt(base, receipt).Normalize()
	if !reflect.DeepEqual(reparsed, projection) {
		return ToolProjection{}, false
	}
	return projection, true
}

func projectCortexContinuationActions(projection ToolProjection, document json.RawMessage) []ContinuationAction {
	object, ok := strictJSONObject(document)
	if !ok || !exactJSONBool(object["ok"], true) {
		return nil
	}
	if _, exists := object["schema_version"]; exists {
		return nil
	}
	if _, exists := object["error"]; exists {
		return nil
	}
	if raw, exists := object["schemaVersion"]; exists {
		version, valid := exactJSONUint64(raw)
		if !valid || version != 1 {
			return nil
		}
	}
	taskID, ok := exactJSONString(object["taskId"], maxContinuationFieldBytes, true)
	if !ok || taskID != canonicalIdentifier(taskID) || projection.Digest == nil || projection.Digest.Target != taskID {
		return nil
	}

	revision := uint64(0)
	if raw, exists := object["revision"]; exists {
		var valid bool
		revision, valid = exactJSONUint64(raw)
		if !valid {
			return nil
		}
	} else if projection.Operation == "cortex_status" {
		return nil
	}

	sourceWorkspace := ""
	if raw, exists := object["workspace"]; exists {
		workspace, valid := exactCortexWorkspace(raw)
		if !valid {
			return nil
		}
		sourceWorkspace = workspace
	} else if projection.Operation == "cortex_status" {
		return nil
	}

	actionsRaw, exists := object["actions"]
	if !exists {
		return nil
	}
	actions, ok := strictJSONArray(actionsRaw, maxContinuationActions)
	if !ok {
		return nil
	}
	result := make([]ContinuationAction, 0, len(actions))
	workspace := sourceWorkspace
	for _, raw := range actions {
		action, valid := parseCortexContinuationAction(raw, projection.Operation, taskID, revision)
		if !valid || (workspace != "" && action.WorkspaceRef != workspace) {
			return nil
		}
		if workspace == "" {
			workspace = action.WorkspaceRef
		}
		result = append(result, action)
	}
	return result
}

func exactCortexWorkspace(raw json.RawMessage) (string, bool) {
	object, ok := strictJSONObject(raw)
	if !ok || !objectKeysAllowed(object,
		[]string{"root", "repository", "branch", "commitBefore", "baseRef"}, []string{"root"}) {
		return "", false
	}
	root, ok := exactJSONString(object["root"], maxContinuationStringBytes, true)
	if !ok || !validContinuationText(root, maxContinuationStringBytes, true) {
		return "", false
	}
	for _, name := range []string{"repository", "branch", "commitBefore", "baseRef"} {
		if value, exists := object[name]; exists {
			if _, valid := exactJSONString(value, maxContinuationStringBytes, false); !valid {
				return "", false
			}
		}
	}
	return root, true
}

func parseCortexContinuationAction(raw json.RawMessage, sourceOperation, taskID string, revision uint64) (ContinuationAction, bool) {
	object, ok := strictJSONObject(raw)
	if !ok || !objectKeysAllowed(object,
		[]string{"tool", "command", "reason", "arguments", "inputs", "blockedBy"},
		[]string{"tool", "arguments"}) {
		return ContinuationAction{}, false
	}
	tool, ok := exactJSONString(object["tool"], maxContinuationFieldBytes, true)
	if !ok || tool != canonicalIdentifier(tool) || !knownCortexContinuationTool(tool) {
		return ContinuationAction{}, false
	}
	for _, name := range []string{"command", "reason"} {
		if value, exists := object[name]; exists {
			maximum := maxContinuationTextBytes
			if name == "command" {
				maximum = maxContinuationStringBytes
			}
			text, valid := exactJSONString(value, maximum, false)
			if !valid || !validContinuationText(text, maximum, false) {
				return ContinuationAction{}, false
			}
		}
	}
	arguments, ok := decodeContinuationArguments(object["arguments"])
	if !ok {
		return ContinuationAction{}, false
	}
	inputs, ok := optionalContinuationStrings(object, "inputs", maxContinuationCollection, true)
	if !ok {
		return ContinuationAction{}, false
	}
	blockedBy, ok := optionalContinuationStrings(object, "blockedBy", maxContinuationCollection, false)
	if !ok || !validCortexContinuationArguments(tool, taskID, arguments, inputs, blockedBy) {
		return ContinuationAction{}, false
	}
	workspace, ok := boundedArgumentString(arguments, "workspace")
	if !ok || !validContinuationText(workspace, maxContinuationStringBytes, true) {
		return ContinuationAction{}, false
	}
	return ContinuationAction{
		Source: "cortex", SourceOperation: sourceOperation, Tool: tool,
		Arguments: arguments, Inputs: inputs, BlockedBy: blockedBy,
		WorkspaceRef: workspace, TaskID: taskID, SourceRevision: revision,
	}, true
}

func knownCortexContinuationTool(tool string) bool {
	switch tool {
	case "cortex_open_task", "cortex_investigate", "cortex_plan", "cortex_begin_change",
		"cortex_verify", "cortex_remember", "cortex_request_decision", "cortex_answer_decision",
		"bob_context", "bob_path", "bob_playbook":
		return true
	default:
		return false
	}
}

func validCortexContinuationArguments(tool, taskID string, arguments map[string]BoundedValue, inputs, blockedBy []string) bool {
	stringKind := BoundedString
	boolKind := BoundedBool
	arrayKind := BoundedArray
	var allowed map[string]BoundedValueKind
	var required []string
	switch tool {
	case "cortex_open_task":
		allowed = map[string]BoundedValueKind{"goal": stringKind, "workspace": stringKind, "mode": stringKind, "risk": stringKind,
			"surfaces": arrayKind, "actor": stringKind, "parentTaskId": stringKind, "idempotencyKey": stringKind}
		required = []string{"goal", "workspace", "mode", "risk", "surfaces"}
	case "cortex_investigate", "cortex_plan", "cortex_verify", "cortex_remember":
		allowed = map[string]BoundedValueKind{"taskId": stringKind, "workspace": stringKind, "actor": stringKind}
		if tool != "cortex_verify" {
			delete(allowed, "actor")
		}
		required = []string{"taskId", "workspace"}
	case "cortex_begin_change":
		allowed = map[string]BoundedValueKind{"taskId": stringKind, "workspace": stringKind, "ttl": stringKind, "actor": stringKind}
		required = []string{"taskId", "workspace", "ttl"}
	case "cortex_request_decision":
		allowed = map[string]BoundedValueKind{"taskId": stringKind, "workspace": stringKind, "question": stringKind,
			"options": arrayKind, "requester": stringKind}
		required = []string{"taskId", "workspace", "question", "options", "requester"}
	case "cortex_answer_decision":
		allowed = map[string]BoundedValueKind{"taskId": stringKind, "workspace": stringKind, "decisionId": stringKind,
			"resume": boolKind}
		required = []string{"taskId", "workspace"}
	case "bob_context":
		allowed = map[string]BoundedValueKind{"workspace": stringKind, "profile": stringKind}
		required = []string{"workspace", "profile"}
	case "bob_path":
		allowed = map[string]BoundedValueKind{"workspace": stringKind, "path": stringKind}
		required = []string{"workspace", "path"}
	case "bob_playbook":
		allowed = map[string]BoundedValueKind{"workspace": stringKind, "id": stringKind, "operation": stringKind}
		required = []string{"workspace", "id", "operation"}
	default:
		return false
	}
	if !boundedArgumentsMatch(arguments, allowed, required) {
		return false
	}
	if value, exists := arguments["taskId"]; exists {
		if value.Kind != BoundedString || value.Text != taskID {
			return false
		}
	} else if strings.HasPrefix(tool, "cortex_") && tool != "cortex_open_task" {
		return false
	}
	if !validCortexInputContract(tool, arguments, inputs) {
		return false
	}
	if len(blockedBy) > 0 && tool != "cortex_answer_decision" && tool != "bob_context" {
		return false
	}

	switch tool {
	case "cortex_open_task":
		mode, _ := boundedArgumentString(arguments, "mode")
		risk, _ := boundedArgumentString(arguments, "risk")
		return oneOf(mode, "change", "investigate", "review") && oneOf(risk, "low", "medium", "high") &&
			validBoundedStringEnumArray(arguments["surfaces"], []string{"code", "browser", "terminal", "artifact", "secret"}, true)
	case "cortex_request_decision":
		return validDecisionOptions(arguments["options"])
	case "cortex_answer_decision":
		resume, hasResume := arguments["resume"]
		_, hasDecision := arguments["decisionId"]
		if hasResume {
			return resume.Kind == BoundedBool && resume.Bool && !hasDecision && len(inputs) == 0 && len(blockedBy) == 0
		}
		return sameStrings(blockedBy, []string{"pending human decision"})
	case "bob_context":
		profile, _ := boundedArgumentString(arguments, "profile")
		return profile == "compact"
	case "bob_path":
		path, _ := boundedArgumentString(arguments, "path")
		return validBobActionPath(path)
	case "bob_playbook":
		id, _ := boundedArgumentString(arguments, "id")
		operation, _ := boundedArgumentString(arguments, "operation")
		return validCanonicalBobID(id) && operation == "show"
	default:
		return true
	}
}

func validCortexInputContract(tool string, arguments map[string]BoundedValue, inputs []string) bool {
	want := []string(nil)
	switch tool {
	case "cortex_investigate":
		want = []string{"question"}
	case "cortex_plan":
		if !sameStrings(inputs, []string{"hypotheses", "uncertainty"}) &&
			!sameStrings(inputs, []string{"hypotheses", "uncertainty", "files"}) {
			return false
		}
		return true
	case "cortex_begin_change":
		if _, exists := arguments["actor"]; !exists {
			want = []string{"actor"}
		}
	case "cortex_remember":
		want = []string{"outcome"}
	case "cortex_answer_decision":
		if resume, exists := arguments["resume"]; exists && resume.Bool {
			want = nil
		} else if _, exists := arguments["decisionId"]; exists {
			want = []string{"answer", "responder"}
		} else {
			want = []string{"decisionId", "answer", "responder"}
		}
	}
	return sameStrings(inputs, want)
}

func validDecisionOptions(value BoundedValue) bool {
	if value.Kind != BoundedArray || len(value.Items) < 2 || len(value.Items) > maxContinuationCollection {
		return false
	}
	seen := make(map[string]struct{}, len(value.Items))
	for _, option := range value.Items {
		if option.Kind != BoundedObject || len(option.Fields) != 3 {
			return false
		}
		fields := boundedFieldsMap(option.Fields)
		id, idOK := boundedArgumentString(fields, "id")
		label, labelOK := boundedArgumentString(fields, "label")
		consequence, consequenceOK := boundedArgumentString(fields, "consequence")
		if !idOK || !labelOK || !consequenceOK || !validContinuationText(id, 256, true) ||
			!validContinuationText(label, maxContinuationStringBytes, true) ||
			!validContinuationText(consequence, maxContinuationStringBytes, true) {
			return false
		}
		if _, duplicate := seen[id]; duplicate {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

func projectBobContinuationActions(projection ToolProjection, document json.RawMessage) []ContinuationAction {
	switch projection.Operation {
	case "bob_context":
		return projectBobContextContinuationActions(projection, document)
	case "bob_path":
		return projectBobPathContinuationActions(projection, document)
	case "bob_playbook":
		// Bob v0.4 playbook steps describe a workflow; they are not typed host
		// continuation actions and must never be promoted here.
		return nil
	default:
		return nil
	}
}

func projectBobContextContinuationActions(projection ToolProjection, document json.RawMessage) []ContinuationAction {
	outer, ok := strictJSONObject(document)
	if !ok || !objectKeysAllowed(outer, []string{"schema_version", "ok", "authority", "context", "error"},
		[]string{"schema_version", "ok", "authority", "context"}) || !exactJSONBool(outer["ok"], true) ||
		!exactJSONVersionOne(outer["schema_version"]) || !exactBobAuthorityObject(outer["authority"]) {
		return nil
	}
	if _, exists := outer["error"]; exists {
		return nil
	}
	context, ok := strictJSONObject(outer["context"])
	if !ok || !objectKeysAllowed(context,
		[]string{"schema_version", "profile", "workspace", "contract_digest", "context_digest", "recipe", "product", "repository",
			"capabilities", "entry_points", "extension_points", "invariants", "playbooks", "artifacts", "notices", "actions", "truncation"},
		[]string{"schema_version", "workspace", "context_digest", "repository", "actions"}) ||
		!exactJSONVersionOne(context["schema_version"]) {
		return nil
	}
	workspace, ok := exactJSONString(context["workspace"], maxContinuationStringBytes, true)
	if !ok || !validBobWorkspace(workspace) || !bobAuthoritySelects(outer["authority"], workspace) {
		return nil
	}
	contextDigest, ok := exactJSONString(context["context_digest"], maxContinuationFieldBytes, true)
	if !ok || !validPrefixedSHA256(contextDigest) || projection.Digest == nil ||
		projection.Digest.Kind != DigestBobContext || projection.Digest.ContextDigest != contextDigest {
		return nil
	}
	repository, ok := strictJSONObject(context["repository"])
	if !ok || !objectKeysAllowed(repository,
		[]string{"state", "clean", "lock_changed", "conflict_count", "managed_files", "plan_digest_version", "plan_digest"},
		[]string{"state"}) {
		return nil
	}
	state, ok := exactJSONString(repository["state"], maxContinuationFieldBytes, true)
	if !ok || state != projection.Digest.State {
		return nil
	}
	wantReason := ""
	switch state {
	case "clean":
		if projection.Domain != DomainSucceeded {
			return nil
		}
	case "drifted":
		if projection.Domain != DomainDrift {
			return nil
		}
		wantReason = "repository_drift"
	case "conflicted":
		if projection.Domain != DomainConflict {
			return nil
		}
		wantReason = "ownership_conflict"
	default:
		return nil
	}
	actions, ok := strictJSONArray(context["actions"], maxContinuationActions)
	if !ok || (wantReason == "" && len(actions) != 0) || (wantReason != "" && len(actions) != 1) {
		return nil
	}
	if len(actions) == 0 {
		return nil
	}
	if projection.Digest.FirstAction != "review_plan" {
		return nil
	}
	if !exactBobGuidanceAction(actions[0], bobExactAction{
		ID: "review_plan", ReasonCode: wantReason, Workspace: workspace,
		Argv: []string{"bob", "plan", workspace, "--json"},
	}) {
		return nil
	}
	return []ContinuationAction{{
		Source: "bob", SourceOperation: "bob_context", Tool: "bob_plan",
		Arguments:  map[string]BoundedValue{"workspace": boundedStringValue(workspace)},
		ReasonCode: wantReason, WorkspaceRef: workspace, ContextDigest: contextDigest,
	}}
}

func projectBobPathContinuationActions(projection ToolProjection, document json.RawMessage) []ContinuationAction {
	outer, ok := strictJSONObject(document)
	if !ok || !objectKeysAllowed(outer, []string{"schema_version", "ok", "authority", "path", "error"},
		[]string{"schema_version", "ok", "authority", "path"}) || !exactJSONBool(outer["ok"], true) ||
		!exactJSONVersionOne(outer["schema_version"]) || !exactBobAuthorityObject(outer["authority"]) {
		return nil
	}
	if _, exists := outer["error"]; exists {
		return nil
	}
	path, ok := strictJSONObject(outer["path"])
	if !ok || !objectKeysAllowed(path,
		[]string{"schema_version", "workspace", "path", "exists", "classification", "state", "human_edit_effect", "ownership",
			"plan_action", "artifact", "extension_points", "related_playbooks", "notices", "actions", "truncation"},
		[]string{"schema_version", "workspace", "path", "classification", "state", "human_edit_effect", "related_playbooks", "actions"}) ||
		!exactJSONVersionOne(path["schema_version"]) {
		return nil
	}
	workspace, ok := exactJSONString(path["workspace"], maxContinuationStringBytes, true)
	if !ok || !validBobWorkspace(workspace) || !bobAuthoritySelects(outer["authority"], workspace) {
		return nil
	}
	pathValue, pathOK := exactJSONString(path["path"], maxContinuationStringBytes, true)
	classification, classificationOK := exactJSONString(path["classification"], maxContinuationFieldBytes, true)
	state, stateOK := exactJSONString(path["state"], maxContinuationFieldBytes, true)
	effect, effectOK := exactJSONString(path["human_edit_effect"], maxContinuationFieldBytes, true)
	if !pathOK || !classificationOK || !stateOK || !effectOK || !validBobActionPath(pathValue) || projection.Digest == nil ||
		projection.Digest.Kind != DigestBobPath || projection.Digest.Classification != classification ||
		projection.Digest.State != state || projection.Digest.Effect != effect {
		return nil
	}
	playbooks, ok := exactCanonicalStringArray(path["related_playbooks"], maxContinuationActions)
	if !ok {
		return nil
	}
	wantFirstAction := ""
	if len(playbooks) > 0 {
		wantFirstAction = "show_playbook:" + playbooks[0]
	}
	if projection.Digest.FirstAction != wantFirstAction {
		return nil
	}
	actions, ok := strictJSONArray(path["actions"], maxContinuationActions)
	if !ok || len(actions) != len(playbooks) {
		return nil
	}
	result := make([]ContinuationAction, 0, len(actions))
	for index, id := range playbooks {
		actionID := "show_playbook:" + id
		if !exactBobGuidanceAction(actions[index], bobExactAction{
			ID: actionID, ReasonCode: "related_playbook", Workspace: workspace,
			Argv: []string{"bob", "playbook", "show", id, workspace, "--json"},
		}) {
			return nil
		}
		result = append(result, ContinuationAction{
			Source: "bob", SourceOperation: "bob_path", Tool: "bob_playbook",
			Arguments: map[string]BoundedValue{
				"workspace": boundedStringValue(workspace),
				"id":        boundedStringValue(id),
				"operation": boundedStringValue("show"),
			},
			ReasonCode: "related_playbook", WorkspaceRef: workspace,
		})
	}
	return result
}

type bobExactAction struct {
	ID         string
	ReasonCode string
	Workspace  string
	Argv       []string
}

func exactBobGuidanceAction(raw json.RawMessage, want bobExactAction) bool {
	object, ok := strictJSONObject(raw)
	if !ok || !objectKeysAllowed(object,
		[]string{"id", "kind", "effect", "cwd", "argv", "reason_code", "requires_explicit_authority", "blocked_by"},
		[]string{"id", "kind", "effect", "cwd", "argv", "reason_code", "requires_explicit_authority", "blocked_by"}) {
		return false
	}
	id, idOK := exactJSONString(object["id"], 256, true)
	kind, kindOK := exactJSONString(object["kind"], maxContinuationFieldBytes, true)
	effect, effectOK := exactJSONString(object["effect"], maxContinuationFieldBytes, true)
	cwd, cwdOK := exactJSONString(object["cwd"], maxContinuationStringBytes, true)
	reason, reasonOK := exactJSONString(object["reason_code"], maxContinuationFieldBytes, true)
	argv, argvOK := exactStringArray(object["argv"], 64, maxContinuationStringBytes, false)
	blocked, blockedOK := exactCanonicalStringArray(object["blocked_by"], 64)
	requiresAuthority, authorityOK := exactJSONBoolValue(object["requires_explicit_authority"])
	return idOK && kindOK && effectOK && cwdOK && reasonOK && argvOK && blockedOK && authorityOK &&
		id == want.ID && kind == "command" && effect == "read_only" && cwd == want.Workspace && reason == want.ReasonCode &&
		!requiresAuthority && len(blocked) == 0 && sameStrings(argv, want.Argv)
}

func exactBobAuthorityObject(raw json.RawMessage) bool {
	object, ok := strictJSONObject(raw)
	return ok && objectKeysAllowed(object,
		[]string{"mode", "default_workspace", "selected_workspace", "allowed_workspace_count"},
		[]string{"mode", "default_workspace", "selected_workspace", "allowed_workspace_count"})
}

func bobAuthoritySelects(raw json.RawMessage, workspace string) bool {
	object, ok := strictJSONObject(raw)
	if !ok {
		return false
	}
	selected, ok := exactJSONString(object["selected_workspace"], maxContinuationStringBytes, true)
	return ok && selected == workspace
}

func exactJSONVersionOne(raw json.RawMessage) bool {
	version, ok := exactJSONUint64(raw)
	return ok && version == 1
}

func boundedStringValue(value string) BoundedValue {
	return BoundedValue{Kind: BoundedString, Text: value}
}

func decodeContinuationArguments(raw json.RawMessage) (map[string]BoundedValue, bool) {
	if len(raw) == 0 || len(raw) > maxContinuationArgumentsBytes {
		return nil, false
	}
	object, ok := strictJSONObject(raw)
	if !ok || len(object) == 0 || len(object) > maxContinuationArguments {
		return nil, false
	}
	budget := boundedValueBudget{}
	arguments := make(map[string]BoundedValue, len(object))
	for name, value := range object {
		if !validContinuationFieldName(name) {
			return nil, false
		}
		bounded, valid := decodeBoundedValue(value, 0, &budget)
		if !valid {
			return nil, false
		}
		arguments[name] = bounded
	}
	return arguments, true
}

type boundedValueBudget struct {
	nodes int
	bytes int
}

func decodeBoundedValue(raw json.RawMessage, depth int, budget *boundedValueBudget) (BoundedValue, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || depth > maxContinuationValueDepth || budget == nil {
		return BoundedValue{}, false
	}
	budget.nodes++
	if budget.nodes > maxContinuationValueNodes {
		return BoundedValue{}, false
	}
	switch raw[0] {
	case '"':
		value, ok := exactJSONString(raw, maxContinuationStringBytes, false)
		if !ok || !validContinuationText(value, maxContinuationStringBytes, false) {
			return BoundedValue{}, false
		}
		budget.bytes += len(value)
		if budget.bytes > maxContinuationValueBytes {
			return BoundedValue{}, false
		}
		return BoundedValue{Kind: BoundedString, Text: value}, true
	case 't', 'f':
		value, ok := exactJSONBoolValue(raw)
		return BoundedValue{Kind: BoundedBool, Bool: value}, ok
	case '[':
		values, ok := strictJSONArray(raw, maxContinuationCollection)
		if !ok {
			return BoundedValue{}, false
		}
		items := make([]BoundedValue, 0, len(values))
		for _, value := range values {
			bounded, valid := decodeBoundedValue(value, depth+1, budget)
			if !valid {
				return BoundedValue{}, false
			}
			items = append(items, bounded)
		}
		return BoundedValue{Kind: BoundedArray, Items: items}, true
	case '{':
		object, ok := strictJSONObject(raw)
		if !ok || len(object) > maxContinuationCollection {
			return BoundedValue{}, false
		}
		names := make([]string, 0, len(object))
		for name := range object {
			if !validContinuationFieldName(name) {
				return BoundedValue{}, false
			}
			names = append(names, name)
		}
		sort.Strings(names)
		fields := make([]BoundedField, 0, len(names))
		for _, name := range names {
			bounded, valid := decodeBoundedValue(object[name], depth+1, budget)
			if !valid {
				return BoundedValue{}, false
			}
			fields = append(fields, BoundedField{Name: name, Value: bounded})
		}
		return BoundedValue{Kind: BoundedObject, Fields: fields}, true
	default:
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var number json.Number
		if err := decoder.Decode(&number); err != nil || !decoderAtEOF(decoder) || len(number.String()) > 64 {
			return BoundedValue{}, false
		}
		if _, err := number.Float64(); err != nil {
			return BoundedValue{}, false
		}
		return BoundedValue{Kind: BoundedNumber, Number: number.String()}, true
	}
}

func strictJSONObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	result := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, false
		}
		if _, duplicate := result[name]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		result[name] = append(json.RawMessage(nil), value...)
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || !decoderAtEOF(decoder) {
		return nil, false
	}
	return result, true
}

func strictJSONArray(raw json.RawMessage, maximum int) ([]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, false
	}
	result := make([]json.RawMessage, 0)
	for decoder.More() {
		if len(result) >= maximum {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		result = append(result, append(json.RawMessage(nil), value...))
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim(']') || !decoderAtEOF(decoder) {
		return nil, false
	}
	return result, true
}

func decoderAtEOF(decoder *json.Decoder) bool {
	var extra json.RawMessage
	return decoder.Decode(&extra) == io.EOF
}

func objectKeysAllowed(object map[string]json.RawMessage, allowed, required []string) bool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for name := range object {
		if _, ok := allowedSet[name]; !ok {
			return false
		}
	}
	for _, name := range required {
		if _, ok := object[name]; !ok {
			return false
		}
	}
	return true
}

func exactJSONString(raw json.RawMessage, maximum int, required bool) (string, bool) {
	if len(raw) == 0 || len(raw) > maximum+16 {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !validContinuationText(value, maximum, required) {
		return "", false
	}
	return value, true
}

func exactJSONBool(raw json.RawMessage, want bool) bool {
	value, ok := exactJSONBoolValue(raw)
	return ok && value == want
}

func exactJSONBoolValue(raw json.RawMessage) (bool, bool) {
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return value, bytes.Equal(bytes.TrimSpace(raw), []byte("true")) || bytes.Equal(bytes.TrimSpace(raw), []byte("false"))
}

func exactJSONUint64(raw json.RawMessage) (uint64, bool) {
	var value uint64
	if json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	return value, true
}

func optionalContinuationStrings(object map[string]json.RawMessage, name string, maximum int, identifiers bool) ([]string, bool) {
	raw, exists := object[name]
	if !exists {
		return nil, true
	}
	if identifiers {
		return exactFieldNameArray(raw, maximum)
	}
	return exactStringArray(raw, maximum, maxContinuationTextBytes, true)
}

func exactFieldNameArray(raw json.RawMessage, maximum int) ([]string, bool) {
	values, ok := exactStringArray(raw, maximum, maxContinuationFieldBytes, true)
	if !ok {
		return nil, false
	}
	for _, value := range values {
		if !validContinuationFieldName(value) {
			return nil, false
		}
	}
	return values, true
}

func exactCanonicalStringArray(raw json.RawMessage, maximum int) ([]string, bool) {
	values, ok := exactStringArray(raw, maximum, 256, true)
	if !ok {
		return nil, false
	}
	for _, value := range values {
		if !validCanonicalBobID(value) {
			return nil, false
		}
	}
	return values, true
}

func exactStringArray(raw json.RawMessage, maximum, maximumBytes int, unique bool) ([]string, bool) {
	items, ok := strictJSONArray(raw, maximum)
	if !ok {
		return nil, false
	}
	result := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		value, valid := exactJSONString(item, maximumBytes, true)
		if !valid {
			return nil, false
		}
		if unique {
			if _, duplicate := seen[value]; duplicate {
				return nil, false
			}
			seen[value] = struct{}{}
		}
		result = append(result, value)
	}
	return result, true
}

func boundedArgumentsMatch(arguments map[string]BoundedValue, allowed map[string]BoundedValueKind, required []string) bool {
	for name, value := range arguments {
		kind, ok := allowed[name]
		if !ok || value.Kind != kind {
			return false
		}
	}
	for _, name := range required {
		if _, ok := arguments[name]; !ok {
			return false
		}
	}
	return true
}

func boundedArgumentString(arguments map[string]BoundedValue, name string) (string, bool) {
	value, ok := arguments[name]
	return value.Text, ok && value.Kind == BoundedString
}

func boundedFieldsMap(fields []BoundedField) map[string]BoundedValue {
	result := make(map[string]BoundedValue, len(fields))
	for _, field := range fields {
		result[field.Name] = field.Value
	}
	return result
}

func validBoundedStringEnumArray(value BoundedValue, allowed []string, required bool) bool {
	if value.Kind != BoundedArray || (required && len(value.Items) == 0) {
		return false
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, item := range allowed {
		allowedSet[item] = struct{}{}
	}
	seen := make(map[string]struct{}, len(value.Items))
	for _, item := range value.Items {
		if item.Kind != BoundedString {
			return false
		}
		if _, ok := allowedSet[item.Text]; !ok {
			return false
		}
		if _, duplicate := seen[item.Text]; duplicate {
			return false
		}
		seen[item.Text] = struct{}{}
	}
	return true
}

func validContinuationText(value string, maximum int, required bool) bool {
	if len(value) > maximum || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') ||
		(required && strings.TrimSpace(value) == "") || (!required && value != "" && strings.TrimSpace(value) == "") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validContinuationFieldName(value string) bool {
	if value == "" || len(value) > maxContinuationFieldBytes {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(index > 0 && character >= '0' && character <= '9') || (index > 0 && character == '_') {
			continue
		}
		return false
	}
	return true
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
