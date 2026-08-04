package ui

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/goaladvisor"
)

func TestCortexDecisionControlItemSurvivesAndResolvesAppendOnly(t *testing.T) {
	const secret = "PRIVATE-CORTEX-DECISION-SENTINEL-9c01"
	workspace := t.TempDir()
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "sonar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.agent.SetWorkDir(workspace)
	workspace, err = canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetSessionStore(store)
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "durable decision", Model: "test", Mode: "AUTO", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	m.sessionID = session.ID
	m.executionLease = lease
	m.goalRuntime = newUIGoalRuntime(t, session.ID, goal.BudgetLimits{MaxContinuationTurns: 3})
	if err := m.goalRuntime.AttachCortex(context.Background(), goal.CortexCorrelation{
		TaskID: "task_decision", Revision: 5, Actor: goalActor,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	if err := m.recordCortexDecisionControlItem(snapshot, goaladvisor.Advice{
		TaskID: "task_decision", Revision: 5, Phase: "needs_human_decision", PendingDecision: true,
	}); err == nil || !strings.Contains(err.Error(), "typed pending decision") {
		t.Fatalf("missing typed decision error = %v", err)
	}
	typedDecision := &goaladvisor.PendingDecision{
		ID:          "decision_forward_only",
		Question:    secret + " question",
		Requester:   secret + " requester",
		RequestedAt: time.Date(2026, 7, 13, 8, 30, 0, 123000000, time.FixedZone("west", -4*60*60)),
		Status:      goaladvisor.DecisionStatusPending,
		Sensitive:   true,
		Options: []goaladvisor.DecisionOption{
			{ID: "two_step", Label: secret + " two-step label", Consequence: secret + " reversible consequence"},
			{ID: "one_step", Label: secret + " one-step label", Consequence: secret + " irreversible consequence"},
		},
	}
	decision := goaladvisor.Advice{
		TaskID: "task_decision", Revision: 5, Phase: "needs_human_decision",
		Summary: secret + " advice summary", PendingDecision: true, Decision: typedDecision,
	}
	if err := m.recordCortexDecisionControlItem(snapshot, decision); err != nil {
		t.Fatal(err)
	}
	replay := decision
	replay.Revision = 99
	replay.Summary = secret + " fresh status summary"
	if err := m.recordCortexDecisionControlItem(snapshot, replay); err != nil {
		t.Fatalf("fresh revision replay failed: %v", err)
	}
	pending, err := store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: session.ID, WorkspaceID: workspace, GoalID: snapshot.ID,
		PendingOnly: true, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Item.Kind != controlplane.KindCortexDecision || pending[0].Resolution != nil {
		t.Fatalf("pending decision = %#v", pending)
	}
	item := pending[0].Item
	if item.ExternalID != typedDecision.ID || item.Summary != cortexDecisionControlSummary || len(item.PayloadJSON) > controlplane.MaxPayloadBytes {
		t.Fatalf("safe decision item = %#v", item)
	}
	binding, err := parseCortexDecisionControlBinding(item.PayloadJSON)
	if err != nil {
		t.Fatal(err)
	}
	wantRequestSHA256, err := typedDecision.RequestBindingSHA256("task_decision")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Version != cortexDecisionControlBindingVersion || binding.TaskID != "task_decision" ||
		binding.DecisionID != typedDecision.ID || binding.RequestedAt != "2026-07-13T12:30:00.123Z" ||
		!reflect.DeepEqual(binding.OptionIDs, []string{"two_step", "one_step"}) || !binding.Sensitive ||
		binding.RequestSHA256 != wantRequestSHA256 {
		t.Fatalf("decision binding = %#v", binding)
	}
	var payloadFields map[string]any
	if err := json.Unmarshal([]byte(item.PayloadJSON), &payloadFields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "task_id", "decision_id", "requested_at", "option_ids", "sensitive", "request_sha256"} {
		if _, exists := payloadFields[key]; !exists {
			t.Fatalf("safe payload missing %q: %#v", key, payloadFields)
		}
	}
	if len(payloadFields) != 7 || strings.Contains(item.PayloadJSON, secret) || strings.Contains(item.Summary, secret) {
		t.Fatalf("secret or extra fields persisted: summary=%q payload=%s", item.Summary, item.PayloadJSON)
	}

	for _, test := range []struct {
		name   string
		mutate func(*goaladvisor.PendingDecision)
	}{
		{name: "question", mutate: func(candidate *goaladvisor.PendingDecision) { candidate.Question = secret + " changed question" }},
		{name: "requester", mutate: func(candidate *goaladvisor.PendingDecision) { candidate.Requester = secret + " changed requester" }},
		{name: "requested at", mutate: func(candidate *goaladvisor.PendingDecision) {
			candidate.RequestedAt = candidate.RequestedAt.Add(time.Second)
		}},
		{name: "option id", mutate: func(candidate *goaladvisor.PendingDecision) { candidate.Options[0].ID = "three_step" }},
		{name: "option label", mutate: func(candidate *goaladvisor.PendingDecision) { candidate.Options[0].Label = secret + " changed label" }},
		{name: "option consequence", mutate: func(candidate *goaladvisor.PendingDecision) {
			candidate.Options[0].Consequence = secret + " changed consequence"
		}},
		{name: "sensitive", mutate: func(candidate *goaladvisor.PendingDecision) { candidate.Sensitive = false }},
	} {
		t.Run("immutable conflict "+test.name, func(t *testing.T) {
			candidate := *typedDecision
			candidate.Options = append([]goaladvisor.DecisionOption(nil), typedDecision.Options...)
			test.mutate(&candidate)
			changed := decision
			changed.Revision++
			changed.Decision = &candidate
			conflictErr := m.recordCortexDecisionControlItem(snapshot, changed)
			if conflictErr == nil {
				t.Fatal("conflicting immutable decision replay was accepted")
			}
			if strings.Contains(conflictErr.Error(), secret) {
				t.Fatalf("secret leaked through conflict error: %v", conflictErr)
			}
		})
	}
	resolvedAdvice := goaladvisor.Advice{
		TaskID: "task_decision", Revision: 100, Phase: "investigating",
		Summary: secret + " resolved summary",
	}
	if err := m.resolveCortexDecisionControlItems(snapshot, resolvedAdvice); err != nil {
		t.Fatal(err)
	}
	if err := m.resolveCortexDecisionControlItems(snapshot, resolvedAdvice); err != nil {
		t.Fatalf("exact resolution replay failed: %v", err)
	}
	pending, err = store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: session.ID, WorkspaceID: workspace, PendingOnly: true, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("resolved decision remains pending: %#v", pending)
	}
	all, err := store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: session.ID, WorkspaceID: workspace, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Resolution == nil || all[0].Resolution.Outcome != controlplane.OutcomeDismissed {
		t.Fatalf("append-only resolved decision = %#v", all)
	}
	var resolutionEvidence map[string]any
	if err := json.Unmarshal([]byte(all[0].Resolution.EvidenceJSON), &resolutionEvidence); err != nil {
		t.Fatal(err)
	}
	if resolutionEvidence["item_id"] != all[0].Item.ItemID ||
		resolutionEvidence["decision_payload_sha256"] != all[0].Item.PayloadSHA256 ||
		resolutionEvidence["decision_id"] != typedDecision.ID ||
		resolutionEvidence["request_sha256"] != wantRequestSHA256 ||
		resolutionEvidence["task_id"] != "task_decision" {
		t.Fatalf("resolution evidence is not bound to its immutable item: %#v", resolutionEvidence)
	}
	for _, forbiddenKey := range []string{"revision", "phase", "summary", "question", "label", "consequence", "requester"} {
		if _, exists := resolutionEvidence[forbiddenKey]; exists {
			t.Fatalf("resolution evidence retained presentation field %q: %#v", forbiddenKey, resolutionEvidence)
		}
	}
	if strings.Contains(all[0].Resolution.EvidenceJSON, secret) || strings.Contains(all[0].Resolution.Detail, secret) ||
		strings.Contains(all[0].Item.PayloadJSON, secret) || strings.Contains(all[0].Item.Summary, secret) {
		t.Fatalf("secret persisted in durable decision state: %#v", all[0])
	}

	otherDecision := goaladvisor.PendingDecision{
		ID: "decision_other", Question: "Choose another path", Requester: "agent-b",
		RequestedAt: time.Date(2026, 7, 13, 13, 0, 0, 0, time.UTC),
		Status:      goaladvisor.DecisionStatusPending,
		Options: []goaladvisor.DecisionOption{
			{ID: "left", Label: "Left", Consequence: "One trade-off"},
			{ID: "right", Label: "Right", Consequence: "Another trade-off"},
		},
	}
	otherRequestSHA256, err := otherDecision.RequestBindingSHA256("task_other")
	if err != nil {
		t.Fatal(err)
	}
	payload, digest, err := controlplane.MarshalDocument(cortexDecisionControlBinding{
		Version: cortexDecisionControlBindingVersion, TaskID: "task_other", DecisionID: otherDecision.ID,
		RequestedAt: otherDecision.RequestedAt.Format(time.RFC3339Nano), OptionIDs: []string{"left", "right"},
		Sensitive: otherDecision.Sensitive, RequestSHA256: otherRequestSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.AppendControlItem(context.Background(), lease, controlplane.Item{
		ItemID: "ctrl_other_task", IdempotencyKey: "ctrlidem_other_task",
		Kind: controlplane.KindCortexDecision,
		Identity: controlplane.Identity{
			SessionID: session.ID, WorkspaceID: workspace, GoalID: snapshot.ID,
		},
		ExternalID: otherDecision.ID, Summary: cortexDecisionControlSummary,
		PayloadJSON: payload, PayloadSHA256: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.resolveCortexDecisionControlItems(snapshot, resolvedAdvice); err == nil || !strings.Contains(err.Error(), "different task binding") || strings.Contains(err.Error(), secret) {
		t.Fatalf("cross-task resolution error = %v", err)
	}
	pending, err = store.ListControlStates(context.Background(), controlplane.Query{
		SessionID: session.ID, WorkspaceID: workspace, PendingOnly: true, Limit: 10,
	})
	if err != nil || len(pending) != 1 || pending[0].Item.ItemID != "ctrl_other_task" {
		t.Fatalf("cross-task decision was not preserved: %#v, %v", pending, err)
	}
}

func TestParseCortexDecisionControlBindingFailsClosed(t *testing.T) {
	valid := cortexDecisionControlBinding{
		Version: cortexDecisionControlBindingVersion, TaskID: "task_1", DecisionID: "decision_1",
		RequestedAt: "2026-07-13T12:30:00Z", OptionIDs: []string{"left", "right"}, Sensitive: false,
		RequestSHA256: strings.Repeat("a", 64),
	}
	document, _, err := controlplane.MarshalDocument(valid)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := parseCortexDecisionControlBinding(document); err != nil || !reflect.DeepEqual(parsed, valid) {
		t.Fatalf("valid binding = %#v, %v", parsed, err)
	}
	for _, test := range []struct {
		name     string
		document string
	}{
		{name: "legacy payload", document: `{"task_id":"task_1"}`},
		{name: "unknown field", document: strings.TrimSuffix(document, "}") + `,"question":"private"}`},
		{name: "duplicate field", document: strings.TrimSuffix(document, "}") + `,"task_id":"task_1"}`},
		{name: "missing sensitive", document: `{"version":1,"task_id":"task_1","decision_id":"decision_1","requested_at":"2026-07-13T12:30:00Z","option_ids":["left","right"],"request_sha256":"` + strings.Repeat("a", 64) + `"}`},
		{name: "non-canonical timestamp", document: strings.Replace(document, "2026-07-13T12:30:00Z", "2026-07-13T08:30:00-04:00", 1)},
		{name: "duplicate option", document: strings.Replace(document, `["left","right"]`, `["left","left"]`, 1)},
		{name: "invalid hash", document: strings.Replace(document, strings.Repeat("a", 64), strings.Repeat("A", 64), 1)},
		{name: "trailing JSON", document: document + `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseCortexDecisionControlBinding(test.document); err == nil {
				t.Fatalf("unsafe binding accepted: %s", test.document)
			}
		})
	}
	const secret = "PRIVATE-BINDING-ERROR-SENTINEL-3fd8"
	secretDocument := strings.TrimSuffix(document, "}") + `,"` + secret + `":"hidden"}`
	if _, err := parseCortexDecisionControlBinding(secretDocument); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe binding error leaked private field: %v", err)
	}
}

func TestCortexDecisionControlPlaneRequiresProductionLease(t *testing.T) {
	workspace := t.TempDir()
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "sonar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.agent.SetWorkDir(workspace)
	workspace, err = canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetSessionStore(store)
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "missing lease", Model: "test", Mode: "AUTO", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.goalRuntime = newUIGoalRuntime(t, session.ID, goal.BudgetLimits{})
	snapshot := snapshotUIGoal(t, m.goalRuntime)
	if err := m.recordCortexDecisionControlItem(snapshot, goaladvisor.Advice{
		TaskID: "task_missing_lease", Revision: 1, PendingDecision: true,
	}); err == nil {
		t.Fatal("decision append without session lease was accepted")
	}
}
