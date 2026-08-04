package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
	"github.com/abdul-hamid-achik/sonar/internal/ui"
)

func openGoalCommandTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "sonar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createGoalSession(t *testing.T, store *db.Store, workspace, objective string) (db.Session, goal.Snapshot) {
	t.Helper()
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: objective, Model: "test", Mode: "AUTO", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := goal.New(goal.Spec{
		ID:        "goal_" + strings.ReplaceAll(objective, " ", "_"),
		SessionID: session.ID,
		Objective: objective,
		AcceptanceCriteria: []goal.AcceptanceCriterion{
			{ID: "criterion_1", Description: "the observer reports durable state"},
		},
		Budget: goal.BudgetLimits{MaxContinuationTurns: 3, MaxEvalTokens: 1_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"version": 1, "goal": snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), session.ID, string(payload)); err != nil {
		t.Fatal(err)
	}
	return session, snapshot
}

type goalRecoveryFixture struct {
	Session       db.Session
	Record        db.SessionStateRecord
	Snapshot      goal.Snapshot
	TurnID        string
	Group         db.ReconciliationGroup
	ExpectedGroup string
}

func createGoalRecoveryFixture(t *testing.T, store *db.Store, workspace string, withMember, ensureGroup bool) goalRecoveryFixture {
	t.Helper()
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "recover abandoned turn", Model: "test", Mode: "AUTO", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := goal.New(goal.Spec{
		ID: fmt.Sprintf("goal_recover_%d", session.ID), SessionID: session.ID,
		Objective:          "Recover the abandoned turn without redispatch",
		AcceptanceCriteria: []goal.AcceptanceCriterion{{ID: "safe", Description: "No unknown effect is retried"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	turnID := fmt.Sprintf("turn_recover_%d", session.ID)
	if _, err := runtime.BeginTurn(context.Background(), turnID, goal.AdmissionInitial); err != nil {
		t.Fatal(err)
	}
	if withMember {
		argumentsSHA, err := execution.HashCanonicalArguments(map[string]any{"path": "recovery.txt"})
		if err != nil {
			t.Fatal(err)
		}
		identity := execution.Identity{
			SessionID: session.ID, WorkspaceID: workspace, RunID: "run_cli_recovery", TurnID: turnID,
			ExecutionID: fmt.Sprintf("exec_recover_%d", session.ID), IdempotencyKey: fmt.Sprintf("idem_recover_%d", session.ID),
			ProviderCallID: "provider_cli_recovery", CanonicalCallID: "call_cli_recovery",
			ToolName: "write", Iteration: 1, Ordinal: 1, Kind: execution.KindBuiltin, EffectClass: execution.Effectful,
		}
		base := execution.Event{
			Identity: identity, Type: execution.EventRequested, Approval: execution.ApprovalNotApplicable,
			ArgumentsSHA256: argumentsSHA, OccurredAt: time.Date(2026, time.July, 12, 17, 0, 0, 0, time.UTC),
		}
		for _, event := range []execution.Event{
			base,
			func() execution.Event {
				value := base
				value.Type = execution.EventApproved
				value.Approval = execution.ApprovalEmbedding
				value.OccurredAt = value.OccurredAt.Add(time.Second)
				return value
			}(),
			func() execution.Event {
				value := base
				value.Type = execution.EventStarted
				value.OccurredAt = value.OccurredAt.Add(2 * time.Second)
				return value
			}(),
			func() execution.Event {
				value := base
				value.Type = execution.EventOutcomeUnknown
				value.Detail = "provider transport closed after dispatch"
				value.OccurredAt = value.OccurredAt.Add(3 * time.Second)
				return value
			}(),
		} {
			if _, _, err := store.AppendExecutionEvent(context.Background(), event); err != nil {
				t.Fatal(err)
			}
		}
		if err := runtime.RecordTurn(context.Background(), goal.TurnReport{
			TurnID: turnID, Summary: "effect outcome is unknown", OutcomeUnknown: true, OutcomeRef: identity.ExecutionID,
		}); err != nil {
			t.Fatal(err)
		}
	} else if err := runtime.RecoverPendingContinuation(context.Background(), goal.PendingRecovery{
		TurnID: turnID, Kind: goal.PendingOutcomeUnknown,
		Reason:   "provider response was lost before a tool lifecycle appeared",
		Evidence: "the admitted turn has no settled provider receipt", OutcomeRef: turnID,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"version": 2, "execution_cursor": 0, "goal": snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), session.ID, string(payload)); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	identitySHA := controlplane.HashText(fmt.Sprintf("reconciliation-group\x00%d\x00%s\x00%s", session.ID, snapshot.ID, turnID))
	fixture := goalRecoveryFixture{
		Session: session, Record: record, Snapshot: snapshot, TurnID: turnID,
		ExpectedGroup: "recongrp_" + identitySHA[:32],
	}
	if ensureGroup {
		lease, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, workspace)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Group, _, err = store.EnsureReconciliationGroup(context.Background(), lease, db.EnsureReconciliationGroupRequest{
			SessionID: session.ID, WorkspaceID: workspace, ExpectedSessionRevision: record.Revision,
		})
		closeErr := lease.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("ensure recovery group error=%v close=%v", err, closeErr)
		}
	}
	return fixture
}

func goalRecoveryApplyArgs(sessionPublicID, itemID, observation, summary string) []string {
	return []string{
		sessionref.Format(sessionPublicID), "--apply", "--item", itemID,
		"--observation", observation, "--source", string(reconciliation.SourceOperatorObservation),
		"--reference", "operator-check:cli", "--summary", summary,
		"--observed-at", "2026-07-12T17:30:00Z", "--json",
	}
}

func TestListGoalSummariesFiltersAndValidatesDurableSessions(t *testing.T) {
	store := openGoalCommandTestStore(t)
	workspace := "/workspace/a"
	first, firstGoal := createGoalSession(t, store, workspace, "Polish the durable goal observer")
	createGoalSession(t, store, "/workspace/b", "Other workspace")
	withoutGoal, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "chat", Model: "test", Mode: "NORMAL", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), withoutGoal.ID, `{"version":1}`); err != nil {
		t.Fatal(err)
	}
	corrupt, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "corrupt", Model: "test", Mode: "AUTO", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), corrupt.ID, `{`); err != nil {
		t.Fatal(err)
	}

	summaries, warnings, err := listGoalSummaries(context.Background(), store, workspace, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].SessionID != first.ID || summaries[0].GoalID != firstGoal.ID {
		t.Fatalf("summaries = %#v", summaries)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "session ") {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestGetGoalSummaryEnforcesWorkspaceAndSessionBinding(t *testing.T) {
	store := openGoalCommandTestStore(t)
	session, expected := createGoalSession(t, store, "/workspace/a", "Inspect exact scope")
	summary, err := getGoalSummary(context.Background(), store, "/workspace/a", session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.GoalID != expected.ID || summary.State != goal.StateActive || summary.Snapshot.SessionID != session.ID {
		t.Fatalf("summary = %#v", summary)
	}
	if _, err := getGoalSummary(context.Background(), store, "/workspace/b", session.ID); err == nil || !strings.Contains(err.Error(), "different workspace") {
		t.Fatalf("cross-workspace error = %v", err)
	}

	forged := expected
	forged.SessionID++
	payload, _ := json.Marshal(map[string]any{"version": 1, "goal": forged})
	if err := store.SaveSessionState(context.Background(), session.ID, string(payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := getGoalSummary(context.Background(), store, "/workspace/a", session.ID); err == nil || !strings.Contains(err.Error(), "belongs to session") {
		t.Fatalf("forged session error = %v", err)
	}
}

func TestGoalListAndShowRenderingAndJSON(t *testing.T) {
	store := openGoalCommandTestStore(t)
	workspace := "/workspace/a"
	session, _ := createGoalSession(t, store, workspace, "A very useful Unicode 目标 goal\nwith a compact second line")

	var stdout, stderr bytes.Buffer
	if code := handleGoalList(store, workspace, []string{"--json", "--limit", "10"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d stderr=%q", code, stderr.String())
	}
	var decoded []goalSummary
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || len(decoded) != 1 || decoded[0].SessionID != session.ID {
		t.Fatalf("list JSON=%q decoded=%#v err=%v", stdout.String(), decoded, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := handleGoalList(store, workspace, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("text list code=%d stderr=%q", code, stderr.String())
	}
	for _, want := range []string{"SESSION", sessionref.Format(session.PublicID), "STATE", "A very useful Unicode 目标 goal with a compact second line"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("text list %q lacks %q", stdout.String(), want)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := handleGoalShow(store, workspace, []string{"--json", sessionref.Format(session.PublicID)}, &stdout, &stderr); code != 0 {
		t.Fatalf("show code=%d stderr=%q", code, stderr.String())
	}
	var snapshot goal.Snapshot
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil || snapshot.SessionID != session.ID {
		t.Fatalf("show JSON=%q snapshot=%#v err=%v", stdout.String(), snapshot, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := handleGoalShow(store, workspace, []string{sessionref.Format(session.PublicID)}, &stdout, &stderr); code != 0 {
		t.Fatalf("text show code=%d stderr=%q", code, stderr.String())
	}
	if want := "Session: " + sessionref.Format(session.PublicID); !strings.Contains(stdout.String(), want) {
		t.Fatalf("text show %q lacks %q", stdout.String(), want)
	}
}

func TestGoalOpenPersistsValidatedHeadlessRuntime(t *testing.T) {
	store := openGoalCommandTestStore(t)
	workspace := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := handleGoalOpen(store, workspace, []string{
		"--objective", "Inspect the release",
		"--criterion", "the release report is complete",
		"--max-continuation-turns", "4",
		"--max-eval-tokens", "1200",
		"--json",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("goal open code=%d stderr=%s", code, stderr.String())
	}
	var result goalOpenResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode goal open result: %v (%s)", err, stdout.String())
	}
	if result.SessionID <= 0 || result.Workspace != workspace || result.Goal.SessionID != result.SessionID || result.Goal.State != goal.StateActive {
		t.Fatalf("goal open result = %#v", result)
	}
	if result.Goal.Budget.MaxContinuationTurns != 4 || result.Goal.Budget.MaxEvalTokens != 1200 {
		t.Fatalf("goal open budget = %#v", result.Goal.Budget)
	}
	if _, err := getGoalSummary(context.Background(), store, workspace, result.SessionID); err != nil {
		t.Fatalf("persisted goal cannot be read: %v", err)
	}
	_, restored, state, record, err := loadHeadlessGoalState(context.Background(), store, workspace, result.SessionID)
	if err != nil || restored == nil || state.Goal == nil || record.Revision != 1 {
		t.Fatalf("headless goal load runtime=%v state=%#v revision=%d err=%v", restored, state, record.Revision, err)
	}
}

func TestLoadHeadlessGoalStateRestoresContextPromptFloorFromSQLite(t *testing.T) {
	store := openGoalCommandTestStore(t)
	workspace := t.TempDir()
	session, snapshot := createGoalSession(t, store, workspace, "Resume the bounded goal")
	floor := agent.ContextPromptFloor{
		Tokens:        2_950,
		HostTokens:    725,
		MessageTokens: 2_025,
		Model:         "test",
	}
	messages := []llm.Message{
		{Role: "user", Content: "continue the goal"},
		{Role: "assistant", Content: "the bounded step is complete"},
	}
	raw, err := ui.EncodeHeadlessGoalSessionStateWithContextFloor(
		messages, floor.Model, "", true, 9, snapshot, floor,
	)
	if err != nil {
		t.Fatalf("encode headless goal session: %v", err)
	}
	before, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	written, err := store.SaveSessionStateCAS(context.Background(), session.ID, before.Revision, raw)
	if err != nil {
		t.Fatalf("save headless goal session: %v", err)
	}

	loadedSession, restored, state, record, err := loadHeadlessGoalState(context.Background(), store, workspace, session.ID)
	if err != nil {
		t.Fatalf("load headless goal session: %v", err)
	}
	if loadedSession.ID != session.ID || restored == nil || state.Goal == nil || state.Goal.SessionID != session.ID {
		t.Fatalf("loaded headless goal session=%#v runtime=%v state=%#v", loadedSession, restored, state)
	}
	if record.Revision != written.Revision || state.ExecutionCursor != 9 {
		t.Fatalf("loaded revision/cursor = %d/%d, want %d/9", record.Revision, state.ExecutionCursor, written.Revision)
	}
	if state.ContextPromptFloor != floor {
		t.Fatalf("loaded context prompt floor = %#v, want %#v", state.ContextPromptFloor, floor)
	}
	if len(state.Messages) != len(messages) || state.Messages[1].Content != messages[1].Content {
		t.Fatalf("loaded messages = %#v", state.Messages)
	}
}

func TestLoadHeadlessGoalStateRejectsInvalidContextPromptFloor(t *testing.T) {
	tests := []struct {
		name  string
		model string
		floor agent.ContextPromptFloor
	}{
		{
			name:  "mismatched model",
			model: "test",
			floor: agent.ContextPromptFloor{Tokens: 2_950, HostTokens: 725, MessageTokens: 2_025, Model: "other-model"},
		},
		{
			name:  "partial numeric projection",
			model: "test",
			floor: agent.ContextPromptFloor{HostTokens: 725, Model: "test"},
		},
		{
			name:  "oversized numeric projection",
			model: "test",
			floor: agent.ContextPromptFloor{Tokens: agent.MaxContextPromptFloorTokens + 1, HostTokens: 1, MessageTokens: 1, Model: "test"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openGoalCommandTestStore(t)
			workspace := t.TempDir()
			session, snapshot := createGoalSession(t, store, workspace, "Reject "+test.name)
			payload, err := json.Marshal(headlessGoalState{
				Version:            2,
				Model:              test.model,
				Messages:           []llm.Message{{Role: "user", Content: "continue"}},
				ContextPromptFloor: test.floor,
				Goal:               &snapshot,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveSessionState(context.Background(), session.ID, string(payload)); err != nil {
				t.Fatal(err)
			}
			if _, _, _, _, err := loadHeadlessGoalState(context.Background(), store, workspace, session.ID); err == nil {
				t.Fatalf("load accepted invalid context prompt floor %#v", test.floor)
			}
		})
	}
}

func TestGoalOpenPrintsUsableShortHandleGuidance(t *testing.T) {
	store := openGoalCommandTestStore(t)
	workspace := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := handleGoalOpen(store, workspace, []string{
		"--objective", "\n  Inspect the release\nwith supporting details",
		"--criterion", "the release report is complete",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("goal open code=%d stderr=%q", code, stderr.String())
	}
	sessions, err := store.ListSessions(context.Background(), db.ListSessionsParams{WorkspaceID: workspace, Limit: 10})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("list sessions = %#v, error=%v", sessions, err)
	}
	handle := sessionref.Format(sessions[0].PublicID)
	if sessions[0].Title != "Inspect the release" {
		t.Fatalf("session title = %q", sessions[0].Title)
	}
	for _, want := range []string{"session " + handle, "goal show --json " + handle} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("goal open output %q lacks %q", stdout.String(), want)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := handleGoalShow(store, workspace, []string{"--json", handle}, &stdout, &stderr); code != 0 {
		t.Fatalf("printed guidance is not executable: code=%d stderr=%q", code, stderr.String())
	}
	var snapshot goal.Snapshot
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil || snapshot.SessionID != sessions[0].ID {
		t.Fatalf("guided show JSON=%q snapshot=%#v err=%v", stdout.String(), snapshot, err)
	}
}

func TestParseGoalRunArgsAcceptsFlagsAfterSessionID(t *testing.T) {
	for _, sessionReference := range []string{"a1b2c3d", "A1B2C3D"} {
		t.Run(sessionReference, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			options, code := parseGoalRunArgs([]string{
				sessionReference, "--prompt", " continue safely ", "--skip-approvals", "--model=qwen3", "--agent", "reviewer",
			}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("parse code=%d stderr=%q", code, stderr.String())
			}
			if options.SessionPublicID != "a1b2c3d" || options.Prompt != "continue safely" || !options.SkipApprovals || options.Model != "qwen3" || options.AgentProfile != "reviewer" {
				t.Fatalf("goal run options = %#v", options)
			}
		})
	}
}

func TestParseGoalRunArgsRejectsMissingPrompt(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, code := parseGoalRunArgs([]string{"a1b2c3d"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--prompt is required") {
		t.Fatalf("parse code=%d stderr=%q", code, stderr.String())
	}
}

func TestGoalRunExecutesAndSettlesDurableTurn(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Chdir(workspace)

	var chatCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(writer, `{"models":[{"name":"qwen3.5:2b","size":42,"details":{"family":"qwen3"}}]}`)
		case "/api/show":
			_, _ = fmt.Fprint(writer, `{"capabilities":["completion","tools"],"details":{"family":"qwen3"},"model_info":{"qwen3.context_length":32768}}`)
		case "/api/chat":
			if chatCalls.Add(1) == 1 {
				_, _ = fmt.Fprintln(writer, `{"message":{"role":"assistant","content":"durable turn complete"},"done":true,"eval_count":7,"prompt_eval_count":11}`)
			} else {
				_, _ = fmt.Fprintln(writer, `{"error":"known provider failure","done":true}`)
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	t.Setenv("OLLAMA_HOST", server.URL)
	t.Setenv("SONAR_MODEL", "qwen3.5:2b")
	t.Setenv("SONAR_LOCAL_ONLY", "true")
	// This case exercises durable goal settlement, not provider selection, so
	// it pins the stub-backed provider explicitly. Riding the default would
	// dispatch the turn at the real DeepSeek API.
	t.Setenv("SONAR_PROVIDER", "ollama")

	store, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	var openOut, openErr bytes.Buffer
	if code := handleGoalOpen(store, currentWorkspace(), []string{
		"--objective", "Finish the durable task", "--criterion", "the turn is recorded",
		"--max-continuation-turns", "2", "--max-eval-tokens", "100", "--json",
	}, &openOut, &openErr); code != 0 {
		t.Fatalf("goal open code=%d stderr=%q", code, openErr.String())
	}
	var opened goalOpenResult
	if err := json.Unmarshal(openOut.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	openedSession, err := store.GetSession(context.Background(), opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var usageOut, usageErr bytes.Buffer
	if code := handleGoalRun([]string{openedSession.PublicID, "--prompt", "perform one verified turn"}, &usageOut, &usageErr); code != 0 {
		t.Fatalf("goal run code=%d usage stderr=%q", code, usageErr.String())
	}

	store, err = db.Open()
	if err != nil {
		t.Fatal(err)
	}
	_, restored, state, record, err := loadHeadlessGoalState(context.Background(), store, currentWorkspace(), opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := restored.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Revision != 3 || snapshot.PendingContinuation != nil || snapshot.LastTurn == nil {
		t.Fatalf("settled state revision=%d snapshot=%#v", record.Revision, snapshot)
	}
	if snapshot.LastTurn.Summary != "assistant yielded without a concrete tool receipt" || snapshot.Usage.EvalTokens != 7 || snapshot.LastTurn.Productive {
		t.Fatalf("turn receipt=%#v usage=%#v", snapshot.LastTurn, snapshot.Usage)
	}
	if len(state.Messages) < 2 || state.Messages[len(state.Messages)-1].Content != "durable turn complete" {
		t.Fatalf("persisted messages=%#v", state.Messages)
	}
	// Headless turns persist provider accounting like the TUI does.
	stats, err := store.GetSessionTokenStats(context.Background(), opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].EvalCount != 7 || stats[0].PromptTokens != 11 || stats[0].Turn != 1 {
		t.Fatalf("headless token stats=%#v", stats)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if code := handleGoalRun([]string{openedSession.PublicID, "--prompt", "try a known failing turn"}, &usageOut, &usageErr); code != 1 {
		t.Fatalf("known-failure goal run code=%d, want 1", code)
	}
	store, err = db.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	_, restored, _, record, err = loadHeadlessGoalState(context.Background(), store, currentWorkspace(), opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = restored.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Revision != 5 || snapshot.PendingContinuation != nil || snapshot.State != goal.StateExhausted || snapshot.LastPendingRecovery != nil {
		t.Fatalf("known failure left unsafe state revision=%d snapshot=%#v", record.Revision, snapshot)
	}
	// The headless turn now runs under the goal's hard eval-token limit, so a
	// provider stream that dies without a trustworthy terminal usage receipt
	// charges its reservation fail-closed and exhausts the remaining budget —
	// the same accounting the TUI applies to capped goal turns.
	if snapshot.Usage.EvalTokens != snapshot.Budget.MaxEvalTokens {
		t.Fatalf("fail-closed reservation should consume the remaining budget, usage=%#v budget=%#v", snapshot.Usage, snapshot.Budget)
	}
	if snapshot.LastTurn == nil || snapshot.LastTurn.OutcomeUnknown || !strings.Contains(snapshot.LastTurn.Summary, "budget exhausted") {
		t.Fatalf("known failure receipt=%#v", snapshot.LastTurn)
	}

	// An exhausted goal must stop at the supervisor gate: no admission, no
	// provider dispatch, and no durable state churn.
	dispatchedBefore := chatCalls.Load()
	var deniedOut, deniedErr bytes.Buffer
	if code := handleGoalRun([]string{openedSession.PublicID, "--prompt", "one more turn"}, &deniedOut, &deniedErr); code != 1 {
		t.Fatalf("exhausted goal run code=%d, want 1", code)
	}
	if chatCalls.Load() != dispatchedBefore {
		t.Fatalf("exhausted goal still dispatched the provider: %d -> %d calls", dispatchedBefore, chatCalls.Load())
	}
	_, restored, _, record, err = loadHeadlessGoalState(context.Background(), store, currentWorkspace(), opened.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = restored.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Revision != 5 || snapshot.State != goal.StateExhausted {
		t.Fatalf("denied dispatch mutated durable state revision=%d state=%s", record.Revision, snapshot.State)
	}
}

func TestGoalCommandArgumentFailures(t *testing.T) {
	store := openGoalCommandTestStore(t)
	var stdout, stderr bytes.Buffer
	if code := handleGoalList(store, "/workspace", []string{"--limit", "0"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid list limit code=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := handleGoalShow(store, "/workspace", []string{"not-a-session"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid show ID code=%d stderr=%q", code, stderr.String())
	}
	if got := compactGoalObjective(strings.Repeat("界", 80), 8); len([]rune(got)) != 8 || !strings.HasSuffix(got, "…") {
		t.Fatalf("compact Unicode objective = %q", got)
	}
	if got := terminalSafeGoalText("safe\x1b[31m\nnext"); strings.ContainsRune(got, '\x1b') || got != "safe[31m next" {
		t.Fatalf("terminal-safe text = %q", got)
	}
	if got := terminalSafeGoalText("safe\u202ereversed"); got != "safereversed" {
		t.Fatalf("terminal bidi-safe text = %q", got)
	}
}

func TestGoalPendingListsOnlyUnresolvedControlItems(t *testing.T) {
	store := openGoalCommandTestStore(t)
	workspace := "/workspace/a"
	session, snapshot := createGoalSession(t, store, workspace, "Resolve durable decisions")
	lease, err := store.AcquireExecutionSessionLease(context.Background(), session.ID, workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	const privatePayload = "private-control-envelope-detail"
	payload, digest, err := controlplane.MarshalDocument(map[string]any{"internal_context": privatePayload})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.AppendControlItem(context.Background(), lease, controlplane.Item{
		ItemID: "ctrl_pending", IdempotencyKey: "ctrlidem_pending",
		Kind: controlplane.KindCortexDecision,
		Identity: controlplane.Identity{
			SessionID: session.ID, WorkspaceID: workspace, GoalID: snapshot.ID,
		},
		Summary: "Choose a migration strategy", PayloadJSON: payload, PayloadSHA256: digest,
	})
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := handleGoalPending(store, workspace, []string{"--json", sessionref.Format(session.PublicID)}, &stdout, &stderr); code != 0 {
		t.Fatalf("pending code=%d stderr=%q", code, stderr.String())
	}
	var states []pendingControlSummary
	if err := json.Unmarshal(stdout.Bytes(), &states); err != nil || len(states) != 1 || states[0].SessionID != session.ID || states[0].ItemID != "ctrl_pending" {
		t.Fatalf("pending JSON=%q states=%#v err=%v", stdout.String(), states, err)
	}
	if strings.Contains(stdout.String(), privatePayload) || strings.Contains(strings.ToLower(stdout.String()), "payload") {
		t.Fatalf("pending JSON disclosed private payload envelope: %q", stdout.String())
	}

	evidence, evidenceDigest, err := controlplane.MarshalDocument(map[string]any{"answer": "forward-only"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.ResolveControlItem(context.Background(), lease, controlplane.Resolution{
		ResolutionID: "ctrlres_pending", IdempotencyKey: "ctrlidem_resolution_pending",
		ItemID: "ctrl_pending", SessionID: session.ID, WorkspaceID: workspace,
		Outcome: controlplane.OutcomeAnswered, EvidenceJSON: evidence, EvidenceSHA256: evidenceDigest,
		ResolvedBy: "test", Detail: "decision recorded",
	})
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := handleGoalPending(store, workspace, []string{sessionref.Format(session.PublicID)}, &stdout, &stderr); code != 0 {
		t.Fatalf("resolved pending code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No pending") {
		t.Fatalf("resolved pending output = %q", stdout.String())
	}
}

func TestGoalRecoverDryRunIsReadOnlyRedactedAndJSONStable(t *testing.T) {
	store := openGoalCommandTestStore(t)
	workspace := "/workspace/recover-dry-run"
	fixture := createGoalRecoveryFixture(t, store, workspace, true, true)
	before, err := store.GetSessionStateRecord(context.Background(), fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := handleGoalRecover(store, workspace, []string{sessionref.Format(fixture.Session.PublicID), "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry-run code=%d stderr=%q", code, stderr.String())
	}
	var view goalRecoveryDryRun
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatalf("dry-run JSON=%q error=%v", stdout.String(), err)
	}
	if !view.DryRun || view.SessionID != fixture.Session.ID || view.SessionRevision != before.Revision || view.GroupItemID != fixture.Group.GroupItemID ||
		len(view.Members) != 1 || len(view.UnresolvedExecutionItems) != 1 || view.Parent.Ready || view.Parent.Resolved {
		t.Fatalf("dry-run projection = %#v", view)
	}
	if !strings.Contains(view.NoResumeWarning, "never resumes") ||
		strings.Contains(stdout.String(), "payload_json") || strings.Contains(stdout.String(), "evidence_json") {
		t.Fatalf("dry-run leaked authority envelope or warning: %q", stdout.String())
	}
	after, err := store.GetSessionStateRecord(context.Background(), fixture.Session.ID)
	if err != nil || after.Revision != before.Revision || after.StateJSON != before.StateJSON {
		t.Fatalf("dry-run mutated session: before=%#v after=%#v error=%v", before, after, err)
	}
	group, err := store.GetReconciliationGroup(context.Background(), fixture.Session.ID, workspace, fixture.Group.GroupItemID)
	if err != nil || group.ParentResolution != nil || group.Members[0].Resolved {
		t.Fatalf("dry-run mutated group = %#v error=%v", group, err)
	}
}

func TestGoalRecoverDryRunNeverEnsuresMissingGroup(t *testing.T) {
	store := openGoalCommandTestStore(t)
	workspace := "/workspace/recover-no-ensure"
	fixture := createGoalRecoveryFixture(t, store, workspace, false, false)
	var stdout, stderr bytes.Buffer
	if code := handleGoalRecover(store, workspace, []string{sessionref.Format(fixture.Session.PublicID)}, &stdout, &stderr); code != 1 {
		t.Fatalf("missing-group dry-run code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run never creates") {
		t.Fatalf("missing-group dry-run error = %q", stderr.String())
	}
	if _, err := store.InspectReconciliationGroup(context.Background(), fixture.Session.ID, workspace); !errors.Is(err, db.ErrReconciliationGroupNotFound) {
		t.Fatalf("dry-run created a group: %v", err)
	}
	after, err := store.GetSessionStateRecord(context.Background(), fixture.Session.ID)
	if err != nil || after.Revision != fixture.Record.Revision || after.StateJSON != fixture.Record.StateJSON {
		t.Fatalf("missing-group dry-run mutated session: %#v error=%v", after, err)
	}
}

func TestGoalRecoverApplyZeroToolEnsuresPausesAndExactlyReplays(t *testing.T) {
	store := openGoalCommandTestStore(t)
	workspace := "/workspace/recover-zero-apply"
	fixture := createGoalRecoveryFixture(t, store, workspace, false, false)
	args := goalRecoveryApplyArgs(
		fixture.Session.PublicID, fixture.ExpectedGroup,
		string(reconciliation.TurnAbandonedAfterInspection), "Inspected the abandoned provider turn and found no execution lifecycle.",
	)
	var stdout, stderr bytes.Buffer
	if code := handleGoalRecover(store, workspace, args, &stdout, &stderr); code != 0 {
		t.Fatalf("zero-tool apply code=%d stderr=%q", code, stderr.String())
	}
	var result goalRecoveryApplyResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("zero-tool JSON=%q error=%v", stdout.String(), err)
	}
	if !result.Applied || !result.Inserted || !result.GoalCleared || result.GoalState != goal.StatePaused ||
		result.GroupItemID != fixture.ExpectedGroup || result.ParentPending || result.RemainingExecutions != 0 {
		t.Fatalf("zero-tool result = %#v", result)
	}
	if !strings.Contains(result.NoResumeWarning, "never resumes") {
		t.Fatalf("zero-tool warning = %q", result.NoResumeWarning)
	}

	stdout.Reset()
	stderr.Reset()
	if code := handleGoalRecover(store, workspace, args, &stdout, &stderr); code != 0 {
		t.Fatalf("exact replay code=%d stderr=%q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Inserted || !result.GoalCleared || result.GoalState != goal.StatePaused {
		t.Fatalf("exact replay result=%#v JSON=%q error=%v", result, stdout.String(), err)
	}

	for _, mutate := range []func([]string) []string{
		func(values []string) []string {
			return replaceGoalRecoverFlag(values, "--summary", "different evidence summary")
		},
		func(values []string) []string {
			return replaceGoalRecoverFlag(values, "--observed-at", "2026-07-12T17:30:01Z")
		},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := handleGoalRecover(store, workspace, mutate(append([]string(nil), args...)), &stdout, &stderr); code != 1 {
			t.Fatalf("conflicting replay code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "conflict") && !strings.Contains(stderr.String(), "differs") {
			t.Fatalf("conflicting replay error = %q", stderr.String())
		}
	}
}

func TestGoalRecoverMemberThenParentGating(t *testing.T) {
	store := openGoalCommandTestStore(t)
	workspace := "/workspace/recover-gating"
	fixture := createGoalRecoveryFixture(t, store, workspace, true, true)
	member := fixture.Group.Members[0]
	parentArgs := goalRecoveryApplyArgs(
		fixture.Session.PublicID, fixture.Group.GroupItemID,
		string(reconciliation.TurnAbandonedAfterInspection), "Inspected the abandoned turn and every execution member.",
	)
	var stdout, stderr bytes.Buffer
	if code := handleGoalRecover(store, workspace, parentArgs, &stdout, &stderr); code != 1 {
		t.Fatalf("early parent code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unresolved execution") {
		t.Fatalf("early parent error = %q", stderr.String())
	}

	memberArgs := goalRecoveryApplyArgs(
		fixture.Session.PublicID, member.ControlItemID,
		string(reconciliation.DispositionEffectNotApplied), "Verified that the external effect was not applied.",
	)
	stdout.Reset()
	stderr.Reset()
	if code := handleGoalRecover(store, workspace, memberArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("member apply code=%d stderr=%q", code, stderr.String())
	}
	var memberResult goalRecoveryApplyResult
	if err := json.Unmarshal(stdout.Bytes(), &memberResult); err != nil || memberResult.GoalCleared || !memberResult.ParentPending || memberResult.RemainingExecutions != 0 {
		t.Fatalf("member result=%#v JSON=%q error=%v", memberResult, stdout.String(), err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := handleGoalRecover(store, workspace, []string{"--json", sessionref.Format(fixture.Session.PublicID)}, &stdout, &stderr); code != 0 {
		t.Fatalf("post-member dry-run code=%d stderr=%q", code, stderr.String())
	}
	var view goalRecoveryDryRun
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil || len(view.UnresolvedExecutionItems) != 0 || !view.Parent.Ready || !view.Members[0].Resolved {
		t.Fatalf("post-member dry-run=%#v JSON=%q error=%v", view, stdout.String(), err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := handleGoalRecover(store, workspace, parentArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("final parent code=%d stderr=%q", code, stderr.String())
	}
	var final goalRecoveryApplyResult
	if err := json.Unmarshal(stdout.Bytes(), &final); err != nil || !final.GoalCleared || final.GoalState != goal.StatePaused || final.ParentPending {
		t.Fatalf("final parent=%#v JSON=%q error=%v", final, stdout.String(), err)
	}
}

func TestGoalRecoverApplyRequiresLeaseAndRejectsInvalidOrStaleFlags(t *testing.T) {
	t.Run("busy lease", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "recover-busy.db")
		first, err := db.OpenPath(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = first.Close() }()
		second, err := db.OpenPath(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = second.Close() }()
		workspace := "/workspace/recover-busy"
		fixture := createGoalRecoveryFixture(t, first, workspace, false, true)
		lease, err := first.AcquireExecutionSessionLease(context.Background(), fixture.Session.ID, workspace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lease.Close() }()
		args := goalRecoveryApplyArgs(fixture.Session.PublicID, fixture.Group.GroupItemID, string(reconciliation.TurnAbandonedAfterInspection), "Inspected busy recovery turn.")
		var stdout, stderr bytes.Buffer
		if code := handleGoalRecover(second, workspace, args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "busy") {
			t.Fatalf("busy lease code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("invalid flags never ensure", func(t *testing.T) {
		store := openGoalCommandTestStore(t)
		workspace := "/workspace/recover-invalid"
		fixture := createGoalRecoveryFixture(t, store, workspace, false, false)
		var stdout, stderr bytes.Buffer
		if code := handleGoalRecover(store, workspace, []string{sessionref.Format(fixture.Session.PublicID), "--apply", "--item", fixture.ExpectedGroup}, &stdout, &stderr); code != 2 {
			t.Fatalf("missing evidence code=%d stderr=%q", code, stderr.String())
		}
		if _, err := store.InspectReconciliationGroup(context.Background(), fixture.Session.ID, workspace); !errors.Is(err, db.ErrReconciliationGroupNotFound) {
			t.Fatalf("invalid flags ensured group: %v", err)
		}
		stdout.Reset()
		stderr.Reset()
		if code := handleGoalRecover(store, workspace, []string{sessionref.Format(fixture.Session.PublicID), "--force"}, &stdout, &stderr); code != 2 {
			t.Fatalf("force flag code=%d stderr=%q", code, stderr.String())
		}
		invalidTime := goalRecoveryApplyArgs(fixture.Session.PublicID, fixture.ExpectedGroup, string(reconciliation.TurnAbandonedAfterInspection), "Inspected invalid timestamp turn.")
		invalidTime = replaceGoalRecoverFlag(invalidTime, "--observed-at", "yesterday")
		stdout.Reset()
		stderr.Reset()
		if code := handleGoalRecover(store, workspace, invalidTime, &stdout, &stderr); code != 2 {
			t.Fatalf("invalid time code=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("stale loaded revision", func(t *testing.T) {
		store := openGoalCommandTestStore(t)
		workspace := "/workspace/recover-stale"
		fixture := createGoalRecoveryFixture(t, store, workspace, false, true)
		wrapped := &staleGoalRecoveryStore{Store: store}
		args := goalRecoveryApplyArgs(fixture.Session.PublicID, fixture.Group.GroupItemID, string(reconciliation.TurnAbandonedAfterInspection), "Inspected stale recovery turn.")
		var stdout, stderr bytes.Buffer
		if code := handleGoalRecover(wrapped, workspace, args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "revision") {
			t.Fatalf("stale revision code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		group, err := store.GetReconciliationGroup(context.Background(), fixture.Session.ID, workspace, fixture.Group.GroupItemID)
		if err != nil || group.ParentResolution != nil {
			t.Fatalf("stale apply mutated parent = %#v error=%v", group.ParentResolution, err)
		}
	})
}

type staleGoalRecoveryStore struct {
	*db.Store
	advanced bool
}

func (s *staleGoalRecoveryStore) GetSessionStateRecord(ctx context.Context, sessionID int64) (db.SessionStateRecord, error) {
	record, err := s.Store.GetSessionStateRecord(ctx, sessionID)
	if err != nil || s.advanced {
		return record, err
	}
	s.advanced = true
	if _, err := s.SaveSessionStateCAS(ctx, sessionID, record.Revision, record.StateJSON); err != nil {
		return db.SessionStateRecord{}, err
	}
	return record, nil
}

func replaceGoalRecoverFlag(values []string, name, replacement string) []string {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == name {
			values[index+1] = replacement
			return values
		}
	}
	return values
}

func TestProjectPendingControlItemsRejectsResolvedAndCrossScopeRows(t *testing.T) {
	payload, digest, err := controlplane.MarshalDocument(map[string]any{"safe": true})
	if err != nil {
		t.Fatal(err)
	}
	item := controlplane.Item{
		ItemID: "ctrl_projection", IdempotencyKey: "ctrlidem_projection",
		Kind:     controlplane.KindDeferredApproval,
		Identity: controlplane.Identity{SessionID: 7, WorkspaceID: "/workspace"},
		Summary:  "Approve the bounded operation", PayloadJSON: payload, PayloadSHA256: digest,
	}
	if _, err := projectPendingControlItems([]controlplane.State{{Item: item}}, 8, "/workspace"); err == nil {
		t.Fatal("cross-session pending row unexpectedly projected")
	}
	if _, err := projectPendingControlItems([]controlplane.State{{Item: item, Resolution: &controlplane.Resolution{}}}, 7, "/workspace"); err == nil {
		t.Fatal("resolved pending row unexpectedly projected")
	}
}

func TestDecodeGoalSummaryRefreshesElapsedWallBudget(t *testing.T) {
	store := openGoalCommandTestStore(t)
	session, snapshot := createGoalSession(t, store, "/workspace", "Expired goal")
	snapshot.Budget.MaxWallTime = time.Nanosecond
	snapshot.CreatedAt = time.Now().Add(-time.Hour).UTC()
	snapshot.UpdatedAt = snapshot.CreatedAt
	payload, _ := json.Marshal(map[string]any{"version": 1, "goal": snapshot})
	if err := store.SaveSessionState(context.Background(), session.ID, string(payload)); err != nil {
		t.Fatal(err)
	}
	summary, err := getGoalSummary(context.Background(), store, "/workspace", session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.State != goal.StateExhausted {
		t.Fatalf("elapsed state = %s, want exhausted", summary.State)
	}
}
