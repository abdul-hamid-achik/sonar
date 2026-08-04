package goaladvisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

type fakeContinuationInterpreter struct {
	call   llm.ToolCall
	result *mcp.ToolResult
}

func (f *fakeContinuationInterpreter) InterpretContinuationResult(call llm.ToolCall, result *mcp.ToolResult) *agent.ContinuationContext {
	f.call, f.result = call, result
	return nil
}

type fakeRegistry struct {
	routes  map[string]string
	name    string
	calls   []string
	args    map[string]any
	history []map[string]any
	result  *mcp.ToolResult
	results map[string]*mcp.ToolResult
	err     error
}

func (f *fakeRegistry) ResolveToolName(remote string) (string, bool) {
	name, ok := f.routes[remote]
	return name, ok
}

func (f *fakeRegistry) CallTool(ctx context.Context, name string, args map[string]any) (*mcp.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.name = name
	f.calls = append(f.calls, name)
	f.args = args
	f.history = append(f.history, cloneTestArgs(args))
	if f.results != nil {
		return f.results[name], f.err
	}
	return f.result, f.err
}

func cloneTestArgs(args map[string]any) map[string]any {
	payload, _ := json.Marshal(args)
	var cloned map[string]any
	_ = json.Unmarshal(payload, &cloned)
	return cloned
}

func validPendingDecisionFixture() map[string]any {
	return map[string]any{
		"id":          "dec_1",
		"question":    "Which migration?",
		"requester":   "agent-a",
		"requestedAt": "2026-07-13T08:30:00-04:00",
		"status":      "pending",
		"sensitive":   true,
		"options": []any{
			map[string]any{"id": "safe", "label": "Two-step", "consequence": "Slower, reversible rollout"},
			map[string]any{"id": "fast", "label": "One-step", "consequence": "Faster, harder rollback"},
		},
	}
}

func pendingAdvicePayload(t *testing.T, decision any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"ok": true, "pendingDecision": decision})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func pendingStatusPayload(t *testing.T) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"ok":              true,
		"taskId":          "task_1",
		"revision":        8,
		"phase":           "needs_human_decision",
		"pendingDecision": validPendingDecisionFixture(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func exactUTF8Bytes(limit int) string {
	return strings.Repeat("界", limit/len("界")) + strings.Repeat("x", limit%len("界"))
}

func TestCortexOpenUsesLazyGatewayAndStableIdentity(t *testing.T) {
	registry := &fakeRegistry{
		routes: map[string]string{gatewayTool: "mcphub__mcphub_call_tool"},
		result: &mcp.ToolResult{Content: `{"ok":true,"taskId":"task_1","phase":"investigating","summary":"opened"}`},
	}
	advisor := NewCortex(registry, "/work/repo", "sonar:test")
	advice, err := advisor.Open(context.Background(), OpenRequest{
		GoalID: "goal_1", Objective: "Ship safely",
		AcceptanceCriteria: []goal.AcceptanceCriterion{{ID: "ac_1", Description: "Tests pass"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if advice.TaskID != "task_1" || registry.name != "mcphub__mcphub_call_tool" {
		t.Fatalf("advice/call = %+v %q", advice, registry.name)
	}
	if registry.args["server"] != "cortex" || registry.args["tool"] != openTool {
		t.Fatalf("gateway route = %#v", registry.args)
	}
	downstream, ok := registry.args["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("downstream args = %#v", registry.args["arguments"])
	}
	if downstream["idempotencyKey"] != "goal_1" || downstream["workspace"] != "/work/repo" {
		t.Fatalf("identity args = %#v", downstream)
	}
	if text, _ := downstream["goal"].(string); text != "Ship safely" {
		t.Fatalf("goal text = %q", text)
	}
	criteriaJSON, err := json.Marshal(downstream["acceptanceCriteria"])
	if err != nil {
		t.Fatal(err)
	}
	if string(criteriaJSON) != `[{"id":"ac_1","statement":"Tests pass"}]` {
		t.Fatalf("acceptance criteria = %s", criteriaJSON)
	}
}

func TestCortexOpenSendsImmutableCriteriaAndRetriesIdentically(t *testing.T) {
	registry := &fakeRegistry{
		routes: map[string]string{openTool: "cortex__cortex_open_task"},
		result: &mcp.ToolResult{
			Content:    `{"ok":false,"summary":"stale display text"}`,
			Structured: json.RawMessage(`{"ok":true,"taskId":"task_1","phase":"investigating"}`),
		},
	}
	request := OpenRequest{
		GoalID:    "goal_1",
		Objective: "Ship safely",
		AcceptanceCriteria: []goal.AcceptanceCriterion{
			{ID: "tests", Description: "All tests pass"},
			{ID: "docs", Description: "Public docs match behavior"},
		},
	}
	advisor := NewCortex(registry, "/work/repo", "")
	for attempt := 0; attempt < 2; attempt++ {
		advice, err := advisor.Open(context.Background(), request)
		if err != nil || advice.TaskID != "task_1" {
			t.Fatalf("attempt %d: advice=%+v err=%v", attempt+1, advice, err)
		}
	}
	if len(registry.history) != 2 || !reflect.DeepEqual(registry.history[0], registry.history[1]) {
		t.Fatalf("idempotent retry changed arguments: %#v", registry.history)
	}
	if registry.history[0]["idempotencyKey"] != "goal_1" {
		t.Fatalf("idempotency key = %#v", registry.history[0]["idempotencyKey"])
	}
}

func TestCortexOpenRejectsInvalidImmutableCriteriaBeforeDispatch(t *testing.T) {
	tests := []struct {
		name     string
		criteria []goal.AcceptanceCriterion
	}{
		{name: "missing", criteria: nil},
		{name: "empty id", criteria: []goal.AcceptanceCriterion{{Description: "Tests pass"}}},
		{name: "empty statement", criteria: []goal.AcceptanceCriterion{{ID: "tests"}}},
		{name: "duplicate id", criteria: []goal.AcceptanceCriterion{{ID: "tests", Description: "One"}, {ID: "tests", Description: "Two"}}},
		{name: "oversized statement", criteria: []goal.AcceptanceCriterion{{ID: "tests", Description: strings.Repeat("x", goal.MaxCriterionBytes+1)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := &fakeRegistry{routes: map[string]string{openTool: "open"}}
			_, err := NewCortex(registry, "/work", "").Open(context.Background(), OpenRequest{
				GoalID: "goal_1", Objective: "Ship", AcceptanceCriteria: test.criteria,
			})
			if !errors.Is(err, ErrRejected) || len(registry.calls) != 0 {
				t.Fatalf("err=%v calls=%#v", err, registry.calls)
			}
		})
	}
}

func TestCortexStatusPrefersDirectToolAndDropsUnvalidatedActions(t *testing.T) {
	interpreter := &fakeContinuationInterpreter{}
	registry := &fakeRegistry{
		routes: map[string]string{statusTool: "cortex__cortex_status", gatewayTool: "mcphub__mcphub_call_tool"},
		result: &mcp.ToolResult{Content: `{"ok":true,"taskId":"task_1","revision":7,"phase":"verifying","verificationOutcome":"verified","actions":[{"tool":"cortex_remember","reason":"preserve"}]}` + "\nstructured: {}"},
	}
	advice, err := NewCortex(registry, "/work/repo", "", interpreter).Status(context.Background(), "task_1")
	if err != nil {
		t.Fatal(err)
	}
	if registry.name != "cortex__cortex_status" || advice.Revision != 7 || advice.VerificationOutcome != "verified" {
		t.Fatalf("direct status = name %q advice %+v", registry.name, advice)
	}
	if interpreter.call.Name != "cortex__cortex_status" || interpreter.result != registry.result ||
		interpreter.call.Arguments["taskId"] != "task_1" {
		t.Fatalf("exact continuation boundary call = %#v result=%p", interpreter.call, interpreter.result)
	}
}

func TestCortexStatusCollectsCriterionBoundHandoffEvidence(t *testing.T) {
	registry := &fakeRegistry{
		routes: map[string]string{
			statusTool:  "cortex__cortex_status",
			handoffTool: "cortex__cortex_handoff",
		},
		results: map[string]*mcp.ToolResult{
			"cortex__cortex_status": {Content: `{"ok":true,"taskId":"task_1","revision":7,"phase":"complete","verificationOutcome":"verified"}`},
			"cortex__cortex_handoff": {Content: `{
				"taskId":"task_1",
				"revision":7,
				"phase":"complete",
				"verification":{"outcome":"verified"},
				"receipts":[
					{"id":"vr_run","batchId":"batch_1","purpose":"verifier_run","status":"passed","binding":"bound","revision":"commit_1","dirtyDigest":"sha256:dirty_1","evidence":["ev_1"]},
					{"id":"vr_claim","batchId":"batch_1","claimId":"criterion_1","claim":"The durable receipt is verified","purpose":"named_claim","status":"passed","binding":"bound","revision":"commit_1","dirtyDigest":"sha256:dirty_1"}
				]
			}`},
		},
	}
	advisor := NewCortex(registry, "/work/repo", "")
	advisor.revision = func(context.Context, string) (WorkspaceRevision, error) {
		return WorkspaceRevision{Commit: "commit_1", DirtyDigest: "sha256:dirty_1"}, nil
	}
	advice, err := advisor.Status(context.Background(), "task_1")
	if err != nil {
		t.Fatal(err)
	}
	proof := advice.CriterionEvidence["criterion_1"]
	refs := proof.Evidence
	if proof.Claim != "The durable receipt is verified" || proof.Revision != "commit_1" || proof.DirtyDigest != "sha256:dirty_1" {
		t.Fatalf("criterion proof = %#v", proof)
	}
	for _, want := range []string{
		"case://task_1/verification/vr_claim",
		"case://task_1/verification/vr_run",
		"ev_1",
	} {
		if !containsString(refs, want) {
			t.Fatalf("criterion evidence %#v missing %q", refs, want)
		}
	}
	if !reflect.DeepEqual(registry.calls, []string{"cortex__cortex_status", "cortex__cortex_handoff"}) {
		t.Fatalf("calls = %#v", registry.calls)
	}
}

func TestCortexStatusDoesNotTrustMismatchedHandoff(t *testing.T) {
	registry := &fakeRegistry{
		routes: map[string]string{statusTool: "status", handoffTool: "handoff"},
		results: map[string]*mcp.ToolResult{
			"status":  {Content: `{"ok":true,"taskId":"task_1","revision":7,"phase":"complete","verificationOutcome":"verified"}`},
			"handoff": {Content: `{"taskId":"task_1","revision":6,"phase":"complete","verification":{"outcome":"verified"}}`},
		},
	}
	advice, err := NewCortex(registry, "/work/repo", "").Status(context.Background(), "task_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(advice.CriterionEvidence) != 0 || len(advice.Warnings) == 0 {
		t.Fatalf("mismatched handoff was trusted: %+v", advice)
	}
}

func TestCortexStatusRejectsReceiptsAfterWorkspaceMutation(t *testing.T) {
	registry := &fakeRegistry{
		routes: map[string]string{statusTool: "status", handoffTool: "handoff"},
		results: map[string]*mcp.ToolResult{
			"status": {Content: `{"ok":true,"taskId":"task_1","revision":7,"phase":"complete","verificationOutcome":"verified"}`},
			"handoff": {Content: `{
				"taskId":"task_1","revision":7,"phase":"complete","verification":{"outcome":"verified"},
				"receipts":[
					{"id":"run_old","batchId":"batch_1","purpose":"verifier_run","status":"passed","binding":"bound","revision":"commit_old","dirtyDigest":"sha256:dirty_old","evidence":["ev_old"]},
					{"id":"claim_old","batchId":"batch_1","claimId":"criterion_1","claim":"Tests pass","purpose":"named_claim","status":"passed","binding":"bound","revision":"commit_old","dirtyDigest":"sha256:dirty_old"}
				]
			}`},
		},
	}
	advisor := NewCortex(registry, "/work/repo", "")
	advisor.revision = func(context.Context, string) (WorkspaceRevision, error) {
		return WorkspaceRevision{Commit: "commit_old", DirtyDigest: "sha256:dirty_after_write"}, nil
	}
	advice, err := advisor.Status(context.Background(), "task_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(advice.CriterionEvidence) != 0 {
		t.Fatalf("stale receipts survived workspace mutation: %#v", advice.CriterionEvidence)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestCortexRejectsErrorEnvelopeWithoutLosingContext(t *testing.T) {
	registry := &fakeRegistry{
		routes: map[string]string{statusTool: "cortex__cortex_status"},
		result: &mcp.ToolResult{Content: `{"ok":false,"taskId":"task_1","phase":"blocked","summary":"decision required"}`, IsError: true},
	}
	advice, err := NewCortex(registry, "", "").Status(context.Background(), "task_1")
	if !errors.Is(err, ErrRejected) || advice.TaskID != "task_1" || advice.Phase != "blocked" {
		t.Fatalf("error advice = %+v err=%v", advice, err)
	}
}

func TestCortexUnavailableWithoutDirectOrGatewayTool(t *testing.T) {
	_, err := NewCortex(&fakeRegistry{routes: map[string]string{}}, "/work", "").Status(context.Background(), "task_1")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestCortexStatusFailsClosedOnIdentityPhaseAndDegradedDrift(t *testing.T) {
	decisionInWrongPhase, err := json.Marshal(map[string]any{
		"ok":              true,
		"taskId":          "task_1",
		"phase":           "investigating",
		"pendingDecision": validPendingDecisionFixture(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		content string
	}{
		{name: "task mismatch", content: `{"ok":true,"taskId":"task_other","phase":"investigating"}`},
		{name: "padded task", content: `{"ok":true,"taskId":" task_1 ","phase":"investigating"}`},
		{name: "oversized task", content: `{"ok":true,"taskId":"` + strings.Repeat("t", goal.MaxCorrelationIDBytes+1) + `","phase":"investigating"}`},
		{name: "missing phase", content: `{"ok":true,"taskId":"task_1"}`},
		{name: "unknown phase", content: `{"ok":true,"taskId":"task_1","phase":"teleporting"}`},
		{name: "degraded", content: `{"ok":true,"taskId":"task_1","phase":"investigating","degraded":true}`},
		{name: "decision phase without typed decision", content: `{"ok":true,"taskId":"task_1","phase":"needs_human_decision"}`},
		{name: "typed decision outside decision phase", content: string(decisionInWrongPhase)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := &fakeRegistry{
				routes: map[string]string{statusTool: "status"},
				result: &mcp.ToolResult{Content: test.content},
			}
			_, err := NewCortex(registry, "/work", "").Status(context.Background(), "task_1")
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCortexAnswerDecisionUsesExactDirectAndGatewayArguments(t *testing.T) {
	request := AnswerDecisionRequest{
		TaskID: "task_1", DecisionID: "decision_1", OptionID: "option_safe", Responder: "human@example.test",
	}
	wantArguments := map[string]any{
		"taskId":     "task_1",
		"workspace":  "/work/repo",
		"decisionId": "decision_1",
		"answer":     "option_safe",
		"responder":  "human@example.test",
	}
	for _, test := range []struct {
		name       string
		routes     map[string]string
		wantTool   string
		gateway    bool
		structured bool
	}{
		{name: "direct", routes: map[string]string{answerDecisionTool: "cortex__cortex_answer_decision"}, wantTool: "cortex__cortex_answer_decision"},
		{name: "gateway", routes: map[string]string{gatewayTool: "mcphub__mcphub_call_tool"}, wantTool: "mcphub__mcphub_call_tool", gateway: true, structured: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := &mcp.ToolResult{Content: `{"ok":true,"taskId":"task_1","revision":9,"phase":"investigating"}`}
			if test.structured {
				result.Content = `{"ok":false,"summary":"stale gateway display"}`
				result.Structured = json.RawMessage(`{"ok":true,"taskId":"task_1","revision":9,"phase":"investigating"}`)
			}
			registry := &fakeRegistry{
				routes: test.routes,
				result: result,
			}
			advice, err := NewCortex(registry, "/work/repo", "").AnswerDecision(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if advice.TaskID != request.TaskID || registry.name != test.wantTool {
				t.Fatalf("answer advice/tool = %+v %q", advice, registry.name)
			}
			if !test.gateway {
				if !reflect.DeepEqual(registry.args, wantArguments) {
					t.Fatalf("direct answer args = %#v, want %#v", registry.args, wantArguments)
				}
				if _, exists := registry.args["resume"]; exists {
					t.Fatalf("direct answer requested resume: %#v", registry.args)
				}
				return
			}
			if registry.args["server"] != "cortex" || registry.args["tool"] != answerDecisionTool || len(registry.args) != 3 {
				t.Fatalf("gateway answer route = %#v", registry.args)
			}
			downstream, ok := registry.args["arguments"].(map[string]any)
			if !ok || !reflect.DeepEqual(downstream, wantArguments) {
				t.Fatalf("gateway answer args = %#v, want %#v", registry.args["arguments"], wantArguments)
			}
			if _, exists := downstream["resume"]; exists {
				t.Fatalf("gateway answer requested resume: %#v", downstream)
			}
		})
	}
}

func TestCortexAnswerDecisionRejectsInvalidExactRequestBeforeDispatch(t *testing.T) {
	valid := AnswerDecisionRequest{TaskID: "task_1", DecisionID: "decision_1", OptionID: "safe", Responder: "human"}
	for _, test := range []struct {
		name   string
		mutate func(*AnswerDecisionRequest)
	}{
		{name: "blank task", mutate: func(request *AnswerDecisionRequest) { request.TaskID = "" }},
		{name: "padded task", mutate: func(request *AnswerDecisionRequest) { request.TaskID = " task_1" }},
		{name: "blank decision", mutate: func(request *AnswerDecisionRequest) { request.DecisionID = " " }},
		{name: "padded option", mutate: func(request *AnswerDecisionRequest) { request.OptionID = "safe " }},
		{name: "blank responder", mutate: func(request *AnswerDecisionRequest) { request.Responder = "" }},
		{name: "unsafe responder", mutate: func(request *AnswerDecisionRequest) { request.Responder = "human\nadmin" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			test.mutate(&request)
			registry := &fakeRegistry{routes: map[string]string{answerDecisionTool: "answer"}}
			_, err := NewCortex(registry, "/work", "").AnswerDecision(context.Background(), request)
			if !errors.Is(err, ErrRejected) || len(registry.calls) != 0 {
				t.Fatalf("answer error=%v calls=%#v", err, registry.calls)
			}
		})
	}
}

func TestCortexAnswerDecisionFailsClosedOnAmbiguousResult(t *testing.T) {
	pendingWrongPhase, err := json.Marshal(map[string]any{
		"ok":              true,
		"taskId":          "task_1",
		"phase":           "investigating",
		"pendingDecision": validPendingDecisionFixture(),
	})
	if err != nil {
		t.Fatal(err)
	}
	pendingDecision, err := json.Marshal(map[string]any{
		"ok":              true,
		"taskId":          "task_1",
		"phase":           "needs_human_decision",
		"pendingDecision": validPendingDecisionFixture(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "task mismatch", content: `{"ok":true,"taskId":"task_other","phase":"investigating"}`},
		{name: "padded response task", content: `{"ok":true,"taskId":" task_1 ","phase":"investigating"}`},
		{name: "missing phase", content: `{"ok":true,"taskId":"task_1"}`},
		{name: "unknown phase", content: `{"ok":true,"taskId":"task_1","phase":"working"}`},
		{name: "degraded", content: `{"ok":true,"taskId":"task_1","phase":"investigating","degraded":true}`},
		{name: "decision phase still pending without typed request", content: `{"ok":true,"taskId":"task_1","phase":"needs_human_decision"}`},
		{name: "typed decision still pending", content: string(pendingDecision)},
		{name: "typed decision outside decision phase", content: string(pendingWrongPhase)},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := &fakeRegistry{
				routes: map[string]string{answerDecisionTool: "answer"},
				result: &mcp.ToolResult{Content: test.content},
			}
			_, err := NewCortex(registry, "/work", "").AnswerDecision(context.Background(), AnswerDecisionRequest{
				TaskID: "task_1", DecisionID: "decision_1", OptionID: "safe", Responder: "human",
			})
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("ambiguous answer error = %v", err)
			}
		})
	}
}

func TestCortexAnswerDecisionPreservesTransportErrorsAndCancellation(t *testing.T) {
	request := AnswerDecisionRequest{TaskID: "task_1", DecisionID: "decision_1", OptionID: "safe", Responder: "human"}
	transportErr := errors.New("transport unavailable")
	registry := &fakeRegistry{routes: map[string]string{answerDecisionTool: "answer"}, err: transportErr}
	_, err := NewCortex(registry, "/work", "").AnswerDecision(context.Background(), request)
	if !errors.Is(err, transportErr) {
		t.Fatalf("transport error = %v", err)
	}
	for _, result := range []*mcp.ToolResult{
		{Content: `{"ok":false,"taskId":"task_1","phase":"investigating","summary":"answer rejected"}`},
		{Content: `{"ok":true,"taskId":"task_1","phase":"investigating"}`, IsError: true},
	} {
		registry = &fakeRegistry{routes: map[string]string{answerDecisionTool: "answer"}, result: result}
		_, err = NewCortex(registry, "/work", "").AnswerDecision(context.Background(), request)
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("tool error result = %#v, err=%v", result, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	registry = &fakeRegistry{routes: map[string]string{answerDecisionTool: "answer"}}
	_, err = NewCortex(registry, "/work", "").AnswerDecision(ctx, request)
	if !errors.Is(err, context.Canceled) || len(registry.calls) != 0 {
		t.Fatalf("cancellation error=%v calls=%#v", err, registry.calls)
	}
}

func TestCortexStatusProjectsPendingDecisionDirectAndGateway(t *testing.T) {
	for _, test := range []struct {
		name          string
		routes        map[string]string
		wantTool      string
		useStructured bool
	}{
		{name: "direct", routes: map[string]string{statusTool: "cortex__cortex_status"}, wantTool: "cortex__cortex_status"},
		{name: "gateway", routes: map[string]string{gatewayTool: "mcphub__mcphub_call_tool"}, wantTool: "mcphub__mcphub_call_tool", useStructured: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := pendingStatusPayload(t)
			result := &mcp.ToolResult{Content: payload}
			if test.useStructured {
				result.Content = `{"ok":false,"summary":"stale display text"}`
				result.Structured = json.RawMessage(payload)
			}
			registry := &fakeRegistry{routes: test.routes, result: result}
			advice, err := NewCortex(registry, "/work/repo", "").Status(context.Background(), "task_1")
			if err != nil {
				t.Fatal(err)
			}
			if registry.name != test.wantTool {
				t.Fatalf("status tool = %q, want %q", registry.name, test.wantTool)
			}
			if test.name == "gateway" && (registry.args["server"] != "cortex" || registry.args["tool"] != statusTool) {
				t.Fatalf("gateway route = %#v", registry.args)
			}
			decision := advice.Decision
			if !advice.PendingDecision || decision == nil {
				t.Fatalf("pending decision was collapsed or omitted: %+v", advice)
			}
			wantTime := time.Date(2026, 7, 13, 12, 30, 0, 0, time.UTC)
			if decision.ID != "dec_1" || decision.Question != "Which migration?" || decision.Requester != "agent-a" ||
				decision.Status != DecisionStatusPending || !decision.Sensitive || !decision.RequestedAt.Equal(wantTime) || decision.RequestedAt.Location() != time.UTC {
				t.Fatalf("decision = %#v", decision)
			}
			wantOptions := []DecisionOption{
				{ID: "safe", Label: "Two-step", Consequence: "Slower, reversible rollout"},
				{ID: "fast", Label: "One-step", Consequence: "Faster, harder rollback"},
			}
			if !reflect.DeepEqual(decision.Options, wantOptions) {
				t.Fatalf("options = %#v, want %#v", decision.Options, wantOptions)
			}
		})
	}
}

func TestParseAdvicePendingDecisionAbsentAndNullRemainFalse(t *testing.T) {
	for _, payload := range []string{`{"ok":true}`, `{"ok":true,"pendingDecision":null}`} {
		advice, err := parseAdvice(payload)
		if err != nil || advice.PendingDecision || advice.Decision != nil {
			t.Fatalf("advice = %+v err=%v for %s", advice, err, payload)
		}
	}
}

func TestParseAdviceRejectsMalformedPendingDecision(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "empty object", mutate: func(decision map[string]any) {
			for key := range decision {
				delete(decision, key)
			}
		}},
		{name: "non-pending status", mutate: func(decision map[string]any) { decision["status"] = "answered" }},
		{name: "missing status", mutate: func(decision map[string]any) { delete(decision, "status") }},
		{name: "one option", mutate: func(decision map[string]any) {
			decision["options"] = decision["options"].([]any)[:1]
		}},
		{name: "too many options", mutate: func(decision map[string]any) {
			options := make([]any, maxPendingDecisionOptions+1)
			for index := range options {
				options[index] = map[string]any{"id": fmt.Sprintf("option-%d", index), "label": "Choice", "consequence": "Trade-off"}
			}
			decision["options"] = options
		}},
		{name: "duplicate option id", mutate: func(decision map[string]any) {
			options := decision["options"].([]any)
			options[1].(map[string]any)["id"] = " safe "
		}},
		{name: "missing option consequence", mutate: func(decision map[string]any) {
			delete(decision["options"].([]any)[0].(map[string]any), "consequence")
		}},
		{name: "invalid timestamp", mutate: func(decision map[string]any) { decision["requestedAt"] = "yesterday" }},
		{name: "answer field", mutate: func(decision map[string]any) { decision["answer"] = "safe" }},
		{name: "responder field", mutate: func(decision map[string]any) { decision["responder"] = "human" }},
		{name: "answered-at field", mutate: func(decision map[string]any) { decision["answeredAt"] = nil }},
		{name: "evidence field", mutate: func(decision map[string]any) { decision["evidenceId"] = "ev_secret" }},
		{name: "unknown field", mutate: func(decision map[string]any) { decision["futureAnswer"] = "secret" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := validPendingDecisionFixture()
			test.mutate(decision)
			if _, err := parseAdvice(pendingAdvicePayload(t, decision)); err == nil {
				t.Fatalf("malformed pending decision was accepted: %#v", decision)
			}
		})
	}
}

func TestParseAdviceRejectsPendingDecisionControlsAndOversizeText(t *testing.T) {
	controls := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "question newline", mutate: func(decision map[string]any) { decision["question"] = "Choose\nnow" }},
		{name: "requester tab", mutate: func(decision map[string]any) { decision["requester"] = "agent\troot" }},
		{name: "option nul", mutate: func(decision map[string]any) {
			decision["options"].([]any)[0].(map[string]any)["id"] = "safe\x00hidden"
		}},
		{name: "bidi label", mutate: func(decision map[string]any) {
			decision["options"].([]any)[0].(map[string]any)["label"] = "safe\u202ehidden"
		}},
		{name: "line separator consequence", mutate: func(decision map[string]any) {
			decision["options"].([]any)[0].(map[string]any)["consequence"] = "one\u2028two"
		}},
	}
	for _, test := range controls {
		t.Run(test.name, func(t *testing.T) {
			decision := validPendingDecisionFixture()
			test.mutate(decision)
			if _, err := parseAdvice(pendingAdvicePayload(t, decision)); err == nil {
				t.Fatalf("unsafe control was accepted: %#v", decision)
			}
		})
	}

	oversize := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "decision id", mutate: func(decision map[string]any) { decision["id"] = strings.Repeat("i", maxDecisionStableIDBytes+1) }},
		{name: "question", mutate: func(decision map[string]any) { decision["question"] = strings.Repeat("q", maxDecisionQuestionBytes+1) }},
		{name: "requester", mutate: func(decision map[string]any) {
			decision["requester"] = strings.Repeat("r", maxDecisionRequesterBytes+1)
		}},
		{name: "option id", mutate: func(decision map[string]any) {
			decision["options"].([]any)[0].(map[string]any)["id"] = strings.Repeat("i", maxDecisionStableIDBytes+1)
		}},
		{name: "option label", mutate: func(decision map[string]any) {
			decision["options"].([]any)[0].(map[string]any)["label"] = strings.Repeat("l", maxDecisionOptionLabelBytes+1)
		}},
		{name: "option consequence", mutate: func(decision map[string]any) {
			decision["options"].([]any)[0].(map[string]any)["consequence"] = strings.Repeat("c", maxDecisionOptionConsequenceBytes+1)
		}},
	}
	for _, test := range oversize {
		t.Run(test.name, func(t *testing.T) {
			decision := validPendingDecisionFixture()
			test.mutate(decision)
			if _, err := parseAdvice(pendingAdvicePayload(t, decision)); err == nil {
				t.Fatalf("oversized decision text was accepted: %s", test.name)
			}
		})
	}

	decision := validPendingDecisionFixture()
	decision["id"] = strings.Repeat("i", maxDecisionStableIDBytes)
	decision["question"] = exactUTF8Bytes(maxDecisionQuestionBytes)
	decision["requester"] = strings.Repeat("r", maxDecisionRequesterBytes)
	option := decision["options"].([]any)[0].(map[string]any)
	option["id"] = strings.Repeat("o", maxDecisionStableIDBytes)
	option["label"] = exactUTF8Bytes(maxDecisionOptionLabelBytes)
	option["consequence"] = exactUTF8Bytes(maxDecisionOptionConsequenceBytes)
	advice, err := parseAdvice(pendingAdvicePayload(t, decision))
	if err != nil || advice.Decision == nil || !utf8.ValidString(advice.Decision.Question) || len(advice.Decision.Question) != maxDecisionQuestionBytes {
		t.Fatalf("valid maximum decision was not preserved: decision=%#v err=%v", advice.Decision, err)
	}

	payload := pendingAdvicePayload(t, validPendingDecisionFixture())
	payload = strings.Replace(payload, "Which migration?", string([]byte{0xff}), 1)
	if _, err := parseAdvice(payload); err == nil {
		t.Fatal("invalid UTF-8 advice was accepted")
	}
}

func TestPendingDecisionJSONRoundTripOmitsAnswerAndEvidence(t *testing.T) {
	advice, err := parseAdvice(pendingAdvicePayload(t, validPendingDecisionFixture()))
	if err != nil || advice.Decision == nil {
		t.Fatalf("parse pending decision: %#v, %v", advice.Decision, err)
	}
	payload, err := json.Marshal(advice.Decision)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"answer", "responder", "answeredAt", "evidenceId"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("pending projection retained %q: %s", forbidden, payload)
		}
	}
	var restored PendingDecision
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*advice.Decision, restored) {
		t.Fatalf("decision round trip mismatch\noriginal: %#v\nrestored: %#v", *advice.Decision, restored)
	}
}

func TestParseAdvicePreservesBoundedVerificationContextWithoutActions(t *testing.T) {
	advice, err := parseAdvice(`{
		"ok":true,
		"taskId":"task_1",
		"revision":9,
		"phase":"verifying",
		"verificationOutcome":"partial",
		"verificationDone":["go test ./..."],
		"missingVerification":["glyph run"],
		"staleVerification":["old vet receipt"],
		"degraded":true,
		"actions":[{"tool":"cortex_verify","arguments":{"taskId":"task_1"},"blockedBy":["approval"]}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if !advice.Degraded || advice.Revision != 9 || advice.VerificationOutcome != "partial" {
		t.Fatalf("advice = %+v", advice)
	}
	if !reflect.DeepEqual(advice.VerificationDone, []string{"go test ./..."}) ||
		!reflect.DeepEqual(advice.MissingVerification, []string{"glyph run"}) ||
		!reflect.DeepEqual(advice.StaleVerification, []string{"old vet receipt"}) {
		t.Fatalf("verification context = %+v", advice)
	}
}

func TestParseAdviceDoesNotRetainDownstreamActionPayload(t *testing.T) {
	const secret = "must-not-enter-goal-prompt"
	advice, err := parseAdvice(`{
		"ok":true,
		"taskId":"task_1",
		"revision":9,
		"phase":"investigating",
		"actions":[{
			"tool":"cortex_begin_change",
			"command":"sh -c '` + secret + `'",
			"reason":"` + secret + `",
			"arguments":{"workspace":"/work/repo","payload":"` + secret + `"},
			"inputs":["` + secret + `"],
			"blockedBy":["` + secret + `"]
		}]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	persistable, err := json.Marshal(advice)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persistable), secret) {
		t.Fatalf("downstream action payload survived advice projection: %s", persistable)
	}
}

func TestParseAdviceRejectsNegativeRevisionAndBoundsLists(t *testing.T) {
	if _, err := parseAdvice(`{"ok":true,"revision":-1}`); err == nil {
		t.Fatal("negative revision was accepted")
	}

	warnings := make([]string, maxAdviceItems+4)
	for index := range warnings {
		warnings[index] = strings.Repeat("界", maxAdviceTextBytes)
	}
	payload, err := json.Marshal(map[string]any{"ok": true, "warnings": warnings})
	if err != nil {
		t.Fatal(err)
	}
	advice, err := parseAdvice(string(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(advice.Warnings) != maxAdviceItems {
		t.Fatalf("warnings = %d, want %d", len(advice.Warnings), maxAdviceItems)
	}
	for _, warning := range advice.Warnings {
		if len(warning) > maxAdviceTextBytes || !utf8.ValidString(warning) {
			t.Fatalf("warning was not UTF-8 bounded: bytes=%d valid=%v", len(warning), utf8.ValidString(warning))
		}
	}
}
