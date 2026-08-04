package ecosystem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProjectContinuationActionsCortexPlan(t *testing.T) {
	receipt := RawReceipt{Structured: json.RawMessage(`{
		"ok":true,
		"taskId":"task_la2",
		"phase":"planned",
		"summary":"accepted",
		"actions":[{
			"tool":"cortex_begin_change",
			"command":"rm -rf /",
			"reason":"human display text is not authority",
			"arguments":{"workspace":"/workspace","ttl":"15m0s","taskId":"task_la2"},
			"inputs":["actor"]
		}],
		"rawAvailable":false
	}`)}
	projection := ProjectReceipt(ProjectToolCall("cortex__cortex_plan", nil), receipt)
	actions := ProjectContinuationActions(projection, receipt)
	if len(actions) != 1 {
		t.Fatalf("continuation actions = %#v, want one", actions)
	}
	action := actions[0]
	if action.Source != "cortex" || action.SourceOperation != "cortex_plan" || action.Tool != "cortex_begin_change" ||
		action.TaskID != "task_la2" || action.WorkspaceRef != "/workspace" || action.SourceRevision != 0 ||
		!reflect.DeepEqual(action.Inputs, []string{"actor"}) || len(action.BlockedBy) != 0 {
		t.Fatalf("continuation action = %#v", action)
	}
	wantArguments := map[string]any{"workspace": "/workspace", "ttl": "15m0s", "taskId": "task_la2"}
	if got := action.ArgumentValues(); !reflect.DeepEqual(got, wantArguments) {
		t.Fatalf("arguments = %#v, want %#v", got, wantArguments)
	}
	encoded, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "rm -rf") || strings.Contains(string(encoded), "human display text") {
		t.Fatalf("untrusted command/reason survived bounded projection: %s", encoded)
	}

	values := action.ArgumentValues()
	values["workspace"] = "/mutated"
	if got := action.ArgumentValues()["workspace"]; got != "/workspace" {
		t.Fatalf("ArgumentValues leaked mutable state: %#v", got)
	}
}

func TestProjectContinuationActionsRemainOutsidePersistentProjection(t *testing.T) {
	receipt := RawReceipt{Structured: json.RawMessage(`{
		"ok":true,
		"taskId":"task_la2",
		"phase":"planned",
		"summary":"accepted",
		"actions":[{
			"tool":"cortex_begin_change",
			"command":"RAW_ACTION_COMMAND_MARKER",
			"reason":"RAW_ACTION_REASON_MARKER",
			"arguments":{"taskId":"task_la2","workspace":"/workspace","ttl":"RAW_ACTION_ARGUMENT_MARKER"},
			"inputs":["actor"]
		}],
		"rawAvailable":false
	}`)}
	projection := ProjectReceipt(ProjectToolCall("cortex__cortex_plan", nil), receipt)
	actions := ProjectContinuationActions(projection, receipt)
	if len(actions) != 1 || actions[0].ArgumentValues()["ttl"] != "RAW_ACTION_ARGUMENT_MARKER" {
		t.Fatalf("bounded transient action = %#v, want parsed marker", actions)
	}

	persisted, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	durable := string(persisted) + "\n" + SafeReceiptText(projection)
	for _, forbidden := range []string{
		"cortex_begin_change",
		"RAW_ACTION_COMMAND_MARKER",
		"RAW_ACTION_REASON_MARKER",
		"RAW_ACTION_ARGUMENT_MARKER",
	} {
		if strings.Contains(durable, forbidden) {
			t.Fatalf("raw action field %q escaped into persistent projection: %s", forbidden, durable)
		}
	}
}

func TestProjectContinuationActionsCortexStatusCarriesRevisionAndBlocker(t *testing.T) {
	receipt := RawReceipt{Structured: json.RawMessage(`{
		"ok":true,
		"taskId":"task_la2",
		"phase":"needs_human_decision",
		"summary":"waiting",
		"revision":7,
		"workspace":{"root":"/workspace","repository":"repo","branch":"main","commitBefore":"abc"},
		"actions":[{
			"tool":"cortex_answer_decision",
			"arguments":{"taskId":"task_la2","workspace":"/workspace","decisionId":"decision_1"},
			"inputs":["answer","responder"],
			"blockedBy":["pending human decision"]
		}],
		"rawAvailable":false
	}`)}
	projection := ProjectReceipt(ProjectToolCall("cortex__cortex_status", nil), receipt)
	actions := ProjectContinuationActions(projection, receipt)
	if len(actions) != 1 {
		t.Fatalf("continuation actions = %#v, want one", actions)
	}
	action := actions[0]
	if action.SourceRevision != 7 || action.TaskID != "task_la2" || action.WorkspaceRef != "/workspace" ||
		!reflect.DeepEqual(action.BlockedBy, []string{"pending human decision"}) ||
		!reflect.DeepEqual(action.Inputs, []string{"answer", "responder"}) {
		t.Fatalf("status continuation = %#v", action)
	}
}

func TestProjectContinuationActionsCortexNestedBoundedArguments(t *testing.T) {
	receipt := RawReceipt{Structured: json.RawMessage(`{
		"ok":true,
		"taskId":"task_la2",
		"phase":"investigating",
		"summary":"recovery",
		"revision":3,
		"workspace":{"root":"/workspace","repository":"repo"},
		"actions":[{
			"tool":"cortex_request_decision",
			"arguments":{
				"taskId":"task_la2",
				"workspace":"/workspace",
				"question":"Choose a path",
				"requester":"agent-a",
				"options":[
					{"id":"safe","label":"Safe","consequence":"Slower"},
					{"id":"fast","label":"Fast","consequence":"Riskier"}
				]
			}
		}],
		"rawAvailable":false
	}`)}
	projection := ProjectReceipt(ProjectToolCall("cortex__cortex_status", nil), receipt)
	actions := ProjectContinuationActions(projection, receipt)
	if len(actions) != 1 {
		t.Fatalf("continuation actions = %#v, want one", actions)
	}
	options := actions[0].Arguments["options"]
	if options.Kind != BoundedArray || len(options.Items) != 2 || options.Items[0].Kind != BoundedObject {
		t.Fatalf("bounded options = %#v", options)
	}
	want := []any{
		map[string]any{"id": "safe", "label": "Safe", "consequence": "Slower"},
		map[string]any{"id": "fast", "label": "Fast", "consequence": "Riskier"},
	}
	if got := actions[0].ArgumentValues()["options"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("ordinary options = %#v, want %#v", got, want)
	}
}

func TestProjectContinuationActionsCortexRouteParity(t *testing.T) {
	receipt := RawReceipt{Structured: cortexPlanContinuationFixture()}
	direct := ProjectReceipt(ProjectToolCall("cortex__cortex_plan", nil), receipt)
	lazy := ProjectReceipt(ProjectToolCall("mcphub__cortex__cortex_plan", nil), receipt)
	restoredGateway := lazy
	restoredGateway.Route.Gateway = "trustedhub"
	directActions := ProjectContinuationActions(direct, receipt)
	lazyActions := ProjectContinuationActions(lazy, receipt)
	restoredActions := ProjectContinuationActions(restoredGateway, receipt)
	if len(directActions) != 1 || !reflect.DeepEqual(directActions, lazyActions) || !reflect.DeepEqual(directActions, restoredActions) {
		t.Fatalf("direct/lazy/restored actions differ: direct=%#v lazy=%#v restored=%#v", directActions, lazyActions, restoredActions)
	}
}

func TestProjectContinuationActionsCortexFailureActionsHaveNoContractAuthority(t *testing.T) {
	receipt := RawReceipt{Structured: json.RawMessage(`{
		"ok":false,
		"taskId":"task_la2",
		"error":"rejected",
		"actions":[{
			"tool":"cortex_investigate",
			"arguments":{"taskId":"task_la2","workspace":"/workspace"},
			"inputs":["question"]
		}]
	}`)}
	projection := ProjectReceipt(ProjectToolCall("cortex__cortex_plan", nil), receipt)
	if projection.Domain != DomainFailed || !projection.DomainTyped || projection.Digest == nil || projection.Digest.Kind != DigestCortexFailure {
		t.Fatalf("failure projection = %#v", projection)
	}
	if got := ProjectContinuationActions(projection, receipt); len(got) != 0 {
		t.Fatalf("unversioned hypothetical failure actions gained authority: %#v", got)
	}
}

func TestReceiptHasContinuationActionsEvenWhenActionFailsClosed(t *testing.T) {
	receipt := RawReceipt{Structured: json.RawMessage(`{
		"ok":true,"taskId":"task_la2","phase":"planned",
		"actions":[{"tool":"shell","command":"dangerous prose","arguments":{}}]
	}`)}
	projection := ProjectReceipt(ProjectToolCall("cortex__cortex_plan", nil), receipt)
	if actions := ProjectContinuationActions(projection, receipt); len(actions) != 0 {
		t.Fatalf("invalid action unexpectedly validated: %#v", actions)
	}
	if !ReceiptHasContinuationActions(projection, receipt) {
		t.Fatal("invalid action surface was not detected for raw-model suppression")
	}
}

func TestProjectContinuationActionsCortexFailsClosed(t *testing.T) {
	base := cortexPlanContinuationFixture()
	tests := map[string]func(t *testing.T) (ToolProjection, RawReceipt){
		"untyped projection": func(t *testing.T) (ToolProjection, RawReceipt) {
			receipt := RawReceipt{Structured: base}
			projection := ProjectReceipt(ProjectToolCall("cortex__cortex_plan", nil), receipt)
			projection.DomainTyped = false
			return projection, receipt
		},
		"route mismatch": func(t *testing.T) (ToolProjection, RawReceipt) {
			receipt := RawReceipt{Structured: base}
			projection := ProjectReceipt(ProjectToolCall("cortex__cortex_plan", nil), receipt)
			projection.Route.Server = "lookalike"
			return projection, receipt
		},
		"future schema": func(t *testing.T) (ToolProjection, RawReceipt) {
			return cortexMutation(t, base, func(document map[string]any) { document["schemaVersion"] = 2 })
		},
		"snake case schema": func(t *testing.T) (ToolProjection, RawReceipt) {
			return cortexMutation(t, base, func(document map[string]any) { document["schema_version"] = 1 })
		},
		"unknown action field": func(t *testing.T) (ToolProjection, RawReceipt) {
			return cortexMutation(t, base, func(document map[string]any) { firstCortexAction(document)["future"] = true })
		},
		"unknown tool": func(t *testing.T) (ToolProjection, RawReceipt) {
			return cortexMutation(t, base, func(document map[string]any) { firstCortexAction(document)["tool"] = "shell" })
		},
		"task mismatch": func(t *testing.T) (ToolProjection, RawReceipt) {
			return cortexMutation(t, base, func(document map[string]any) {
				firstCortexAction(document)["arguments"].(map[string]any)["taskId"] = "task_other"
			})
		},
		"unknown argument": func(t *testing.T) (ToolProjection, RawReceipt) {
			return cortexMutation(t, base, func(document map[string]any) {
				firstCortexAction(document)["arguments"].(map[string]any)["payload"] = "arbitrary"
			})
		},
		"missing input": func(t *testing.T) (ToolProjection, RawReceipt) {
			return cortexMutation(t, base, func(document map[string]any) { delete(firstCortexAction(document), "inputs") })
		},
		"duplicate input": func(t *testing.T) (ToolProjection, RawReceipt) {
			return cortexMutation(t, base, func(document map[string]any) {
				firstCortexAction(document)["inputs"] = []any{"actor", "actor"}
			})
		},
		"unexpected blocker": func(t *testing.T) (ToolProjection, RawReceipt) {
			return cortexMutation(t, base, func(document map[string]any) {
				firstCortexAction(document)["blockedBy"] = []any{"prose cannot change policy"}
			})
		},
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			projection, receipt := build(t)
			if got := ProjectContinuationActions(projection, receipt); len(got) != 0 {
				t.Fatalf("malformed continuation produced candidates: %#v", got)
			}
		})
	}

	t.Run("duplicate JSON action key", func(t *testing.T) {
		receipt := RawReceipt{Structured: json.RawMessage(`{
			"ok":true,"taskId":"task_la2","phase":"planned","actions":[],
			"actions":[{"tool":"cortex_begin_change","arguments":{"taskId":"task_la2","workspace":"/workspace","ttl":"15m0s"},"inputs":["actor"]}]
		}`)}
		projection := ProjectReceipt(ProjectToolCall("cortex__cortex_plan", nil), receipt)
		if !projection.DomainTyped {
			t.Fatal("baseline parser unexpectedly rejected duplicate-key adversarial envelope")
		}
		if got := ProjectContinuationActions(projection, receipt); len(got) != 0 {
			t.Fatalf("duplicate-key envelope produced candidates: %#v", got)
		}
	})
}

func TestProjectContinuationActionsCortexRejectsWorkspaceMismatch(t *testing.T) {
	receipt := RawReceipt{Structured: json.RawMessage(`{
		"ok":true,"taskId":"task_la2","phase":"verifying","revision":2,
		"workspace":{"root":"/workspace","repository":"repo"},
		"actions":[{"tool":"cortex_remember","arguments":{"taskId":"task_la2","workspace":"/other"},"inputs":["outcome"]}]
	}`)}
	projection := ProjectReceipt(ProjectToolCall("cortex__cortex_status", nil), receipt)
	if !projection.DomainTyped {
		t.Fatal("baseline parser unexpectedly rejected workspace-mismatch adversarial envelope")
	}
	if got := ProjectContinuationActions(projection, receipt); len(got) != 0 {
		t.Fatalf("workspace mismatch produced candidates: %#v", got)
	}
}

func TestProjectContinuationActionsBobV040Fixtures(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		operation  string
		wantTool   string
		wantReason string
		wantDigest bool
	}{
		{name: "context drift", file: "context-drift-v1.json", operation: "bob_context", wantTool: "bob_plan", wantReason: "repository_drift", wantDigest: true},
		{name: "context conflict", file: "context-conflict-v1.json", operation: "bob_context", wantTool: "bob_plan", wantReason: "ownership_conflict", wantDigest: true},
		{name: "path extension", file: "path-extension-v1.json", operation: "bob_path", wantTool: "bob_playbook", wantReason: "related_playbook"},
		{name: "path managed", file: "path-managed-v1.json", operation: "bob_path", wantTool: "bob_playbook", wantReason: "related_playbook"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			receipt := RawReceipt{Structured: bobV040ContinuationFixture(t, test.file)}
			projection := ProjectReceipt(ProjectToolCall("bob__"+test.operation, nil), receipt)
			actions := ProjectContinuationActions(projection, receipt)
			if len(actions) != 1 {
				t.Fatalf("continuation actions = %#v, want one", actions)
			}
			action := actions[0]
			if action.Source != "bob" || action.SourceOperation != test.operation || action.Tool != test.wantTool ||
				action.ReasonCode != test.wantReason || action.WorkspaceRef != "/workspace" || action.TaskID != "" ||
				action.SourceRevision != 0 || len(action.Inputs) != 0 || len(action.BlockedBy) != 0 {
				t.Fatalf("Bob continuation = %#v", action)
			}
			if test.wantDigest != (action.ContextDigest != "") {
				t.Fatalf("context digest = %q, want present %t", action.ContextDigest, test.wantDigest)
			}
			if got := action.ArgumentValues()["workspace"]; got != "/workspace" {
				t.Fatalf("workspace argument = %#v", got)
			}
		})
	}
}

func TestProjectContinuationActionsBobNoActionContracts(t *testing.T) {
	for _, test := range []struct {
		file      string
		operation string
	}{
		{file: "context-clean-v1.json", operation: "bob_context"},
		{file: "playbook-ready-v1.json", operation: "bob_playbook"},
		{file: "playbook-missing-input-v1.json", operation: "bob_playbook"},
	} {
		t.Run(test.file, func(t *testing.T) {
			receipt := RawReceipt{Structured: bobV040ContinuationFixture(t, test.file)}
			projection := ProjectReceipt(ProjectToolCall("bob__"+test.operation, nil), receipt)
			if got := ProjectContinuationActions(projection, receipt); len(got) != 0 {
				t.Fatalf("non-action Bob contract produced candidates: %#v", got)
			}
		})
	}
}

func TestProjectContinuationActionsBobSemanticTupleFailsClosed(t *testing.T) {
	tests := map[string]struct {
		file      string
		operation string
		mutate    func(map[string]any)
	}{
		"context reason mismatch": {
			file: "context-drift-v1.json", operation: "bob_context",
			mutate: func(document map[string]any) {
				bobFixtureAction(document, "context")["reason_code"] = "looks_safe"
			},
		},
		"context argv mismatch": {
			file: "context-drift-v1.json", operation: "bob_context",
			mutate: func(document map[string]any) {
				bobFixtureAction(document, "context")["argv"] = []any{"bob", "apply", "/workspace", "--json"}
			},
		},
		"context downstream effect": {
			file: "context-drift-v1.json", operation: "bob_context",
			mutate: func(document map[string]any) {
				bobFixtureAction(document, "context")["effect"] = "subprocess_probe"
			},
		},
		"context extra field": {
			file: "context-drift-v1.json", operation: "bob_context",
			mutate: func(document map[string]any) {
				bobFixtureAction(document, "context")["future"] = "value"
			},
		},
		"path id mismatch": {
			file: "path-extension-v1.json", operation: "bob_path",
			mutate: func(document map[string]any) {
				bobFixtureAction(document, "path")["id"] = "show_playbook:other"
			},
		},
		"path blocker": {
			file: "path-extension-v1.json", operation: "bob_path",
			mutate: func(document map[string]any) {
				bobFixtureAction(document, "path")["blocked_by"] = []any{"wait"}
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			document := decodeContinuationFixtureObject(t, bobV040ContinuationFixture(t, test.file))
			test.mutate(document)
			raw, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			receipt := RawReceipt{Structured: raw}
			projection := ProjectReceipt(ProjectToolCall("bob__"+test.operation, nil), receipt)
			if !projection.DomainTyped {
				t.Fatal("baseline Bob projection unexpectedly rejected generic-but-semantic adversarial action")
			}
			if got := ProjectContinuationActions(projection, receipt); len(got) != 0 {
				t.Fatalf("semantic mismatch produced candidates: %#v", got)
			}
		})
	}
}

func TestBoundedValueRejectsUnboundedAndAmbiguousJSON(t *testing.T) {
	valid, ok := decodeContinuationArguments(json.RawMessage(`{"enabled":false,"count":2,"nested":{"items":["a","b"]}}`))
	if !ok {
		t.Fatal("valid bounded arguments rejected")
	}
	if got := valid["count"].Any(); got != int64(2) {
		t.Fatalf("number = %#v", got)
	}
	for name, raw := range map[string]json.RawMessage{
		"null":            json.RawMessage(`{"value":null}`),
		"duplicate field": json.RawMessage(`{"value":"a","value":"b"}`),
		"control":         json.RawMessage(`{"value":"\u001b[31m"}`),
		"oversized":       json.RawMessage(fmt.Sprintf(`{"value":%q}`, strings.Repeat("x", maxContinuationStringBytes+1))),
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := decodeContinuationArguments(raw); ok {
				t.Fatalf("unsafe bounded arguments accepted: %s", raw)
			}
		})
	}
}

func cortexPlanContinuationFixture() json.RawMessage {
	return json.RawMessage(`{
		"ok":true,
		"taskId":"task_la2",
		"phase":"planned",
		"summary":"accepted",
		"actions":[{
			"tool":"cortex_begin_change",
			"arguments":{"taskId":"task_la2","workspace":"/workspace","ttl":"15m0s"},
			"inputs":["actor"]
		}],
		"rawAvailable":false
	}`)
}

func cortexMutation(t *testing.T, raw json.RawMessage, mutate func(map[string]any)) (ToolProjection, RawReceipt) {
	t.Helper()
	document := decodeContinuationFixtureObject(t, raw)
	mutate(document)
	mutated, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	receipt := RawReceipt{Structured: mutated}
	return ProjectReceipt(ProjectToolCall("cortex__cortex_plan", nil), receipt), receipt
}

func firstCortexAction(document map[string]any) map[string]any {
	return document["actions"].([]any)[0].(map[string]any)
}

func bobV040ContinuationFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "bob_v040", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodeContinuationFixtureObject(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func bobFixtureAction(document map[string]any, payload string) map[string]any {
	return document[payload].(map[string]any)["actions"].([]any)[0].(map[string]any)
}
