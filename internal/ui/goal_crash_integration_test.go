//go:build darwin || linux

package ui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/controlplane"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

const (
	goalCrashHelperEnv      = "SONAR_GOAL_CRASH_HELPER"
	goalCrashDBEnv          = "SONAR_GOAL_CRASH_DB"
	goalCrashSessionEnv     = "SONAR_GOAL_CRASH_SESSION"
	goalCrashWorkspaceEnv   = "SONAR_GOAL_CRASH_WORKSPACE"
	goalCrashTurnEnv        = "SONAR_GOAL_CRASH_TURN"
	goalCrashLeaseMarkerEnv = "SONAR_GOAL_CRASH_LEASE_MARKER"
	goalCrashProviderEnv    = "SONAR_GOAL_CRASH_PROVIDER_MARKER"
	goalCrashExitCode       = 87
)

type goalCrashLedger struct {
	store *db.Store
}

func (l *goalCrashLedger) AppendExecutionEvent(ctx context.Context, event executionpkg.Event) (executionpkg.Event, bool, error) {
	stored, inserted, err := l.store.AppendExecutionEvent(ctx, event)
	if err == nil && event.Type == executionpkg.EventStarted {
		os.Exit(goalCrashExitCode)
	}
	return stored, inserted, err
}

func (l *goalCrashLedger) ListExecutionRecoveryHazards(ctx context.Context, sessionID int64, workspaceID string, cursor int64, limit int) ([]executionpkg.State, error) {
	return l.store.ListExecutionRecoveryHazards(ctx, sessionID, workspaceID, cursor, limit)
}

type goalCrashClient struct {
	providerMarker string
}

func (c *goalCrashClient) ChatStream(_ context.Context, _ llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if err := os.WriteFile(c.providerMarker, []byte("one provider request\n"), 0o600); err != nil {
		return err
	}
	return emit(llm.StreamChunk{
		Done: true, EvalCount: 3, PromptEvalCount: 5,
		ToolCalls: []llm.ToolCall{{
			ID: "crash-write", Name: "write",
			Arguments: map[string]any{"path": "must-not-exist.txt", "content": "crashed before backend dispatch"},
		}},
	})
}

func (*goalCrashClient) Ping() error   { return nil }
func (*goalCrashClient) Model() string { return "goal-crash-test" }
func (*goalCrashClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

type goalCrashOutput struct{}

func (*goalCrashOutput) StreamText(string)                                          {}
func (*goalCrashOutput) StreamReasoning(string)                                     {}
func (*goalCrashOutput) StreamDone(int, int)                                        {}
func (*goalCrashOutput) ToolCallStart(string, string, map[string]any)               {}
func (*goalCrashOutput) ToolCallResult(string, string, string, bool, time.Duration) {}
func (*goalCrashOutput) SystemMessage(string)                                       {}
func (*goalCrashOutput) Error(string)                                               {}

func TestGoalExecutionCrashHelperProcess(t *testing.T) {
	if os.Getenv(goalCrashHelperEnv) != "1" {
		return
	}
	sessionID, err := strconv.ParseInt(os.Getenv(goalCrashSessionEnv), 10, 64)
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	store, err := db.OpenPath(os.Getenv(goalCrashDBEnv))
	if err != nil {
		t.Fatalf("open crash database: %v", err)
	}
	workspace := os.Getenv(goalCrashWorkspaceEnv)
	lease, err := store.AcquireExecutionSessionLease(context.Background(), sessionID, workspace)
	if err != nil {
		t.Fatalf("acquire crash lease: %v", err)
	}
	// Keep both resources live until os.Exit proves kernel/process cleanup.
	_ = lease
	if err := os.WriteFile(os.Getenv(goalCrashLeaseMarkerEnv), []byte("lease acquired\n"), 0o600); err != nil {
		t.Fatalf("write lease marker: %v", err)
	}

	client := &goalCrashClient{providerMarker: os.Getenv(goalCrashProviderEnv)}
	ag := agent.New(client, nil, 4096)
	ag.SetWorkDir(workspace)
	ag.SetModeContext("test", agent.BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.SetExecutionLedger(&goalCrashLedger{store: store})
	ag.SetExecutionSessionID(sessionID, "")
	ag.SetExecutionSnapshotCursor(0)
	ag.RequireExecutionLedger(true)
	ag.AddUserMessage("write once under the admitted goal turn")
	if err := ag.RunTurn(context.Background(), &goalCrashOutput{}, os.Getenv(goalCrashTurnEnv)); err != nil {
		t.Fatalf("agent returned before crash boundary: %v", err)
	}
	t.Fatal("agent completed without crashing after the durable dispatch marker")
}

func TestGoalAndExecutionLedgerRecoverTogetherAfterSubprocessCrash(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	configDir := filepath.Join(home, ".config", "sonar")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := canonicalWorkspaceID(workspace)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(configDir, "sonar.db")
	store, err := db.OpenPath(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "goal crash recovery", Model: "goal-crash-test", Mode: "BUILD", WorkspaceID: workspaceID,
	})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	runtime := newUIGoalRuntime(t, session.ID, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: "turn_before_crash", Productive: true, Summary: "prior progress was verified",
	})
	const admittedTurn = "turn_goal_crash"
	if _, err := runtime.BeginContinuation(context.Background(), admittedTurn); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	pending := snapshotUIGoal(t, runtime)
	raw, err := marshalPersistedSessionState(persistedSessionState{
		Version: 1, Mode: ModeBuild, Model: "goal-crash-test", ExecutionCursor: 0, Goal: &pending,
	})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), session.ID, raw); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	leaseMarker := filepath.Join(root, "lease-acquired")
	providerMarker := filepath.Join(root, "provider-called")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGoalExecutionCrashHelperProcess$", "-test.count=1")
	command.Env = append(os.Environ(),
		goalCrashHelperEnv+"=1",
		goalCrashDBEnv+"="+databasePath,
		goalCrashSessionEnv+"="+strconv.FormatInt(session.ID, 10),
		goalCrashWorkspaceEnv+"="+workspaceID,
		goalCrashTurnEnv+"="+admittedTurn,
		goalCrashLeaseMarkerEnv+"="+leaseMarker,
		goalCrashProviderEnv+"="+providerMarker,
		"HOME="+home,
	)
	output, runErr := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("crash subprocess timed out: %v\n%s", ctx.Err(), output)
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != goalCrashExitCode {
		t.Fatalf("crash subprocess exit = %v, want %d\n%s", runErr, goalCrashExitCode, output)
	}
	if _, err := os.Stat(leaseMarker); err != nil {
		t.Fatalf("subprocess did not acquire the session lease: %v", err)
	}
	if data, err := os.ReadFile(providerMarker); err != nil || strings.Count(string(data), "provider request") != 1 {
		t.Fatalf("provider marker = %q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(workspaceID, "must-not-exist.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("effect ran after crash boundary: %v", err)
	}

	restartedStore, err := db.OpenPath(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restartedStore.Close() }()
	restartedLease, err := restartedStore.AcquireExecutionSessionLease(context.Background(), session.ID, workspaceID)
	if err != nil {
		t.Fatalf("crashed process did not release its lease: %v", err)
	}
	leaseOwnedByModel := false
	defer func() {
		if !leaseOwnedByModel {
			_ = restartedLease.Close()
		}
	}()
	_, persisted, stateRecord, err := loadPersistedSession(context.Background(), restartedStore, session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ExecutionCursor != 0 || persisted.Goal == nil || persisted.Goal.PendingContinuation == nil {
		t.Fatalf("crash snapshot lost cursor or permit: %#v", persisted)
	}
	if persisted.Goal.PendingContinuation.TurnID != admittedTurn ||
		persisted.Goal.PendingContinuation.Kind != goal.AdmissionAutomatic ||
		persisted.Goal.Usage.ContinuationTurns != 1 {
		t.Fatalf("persisted permit = %#v usage=%#v", persisted.Goal.PendingContinuation, persisted.Goal.Usage)
	}
	hazards, err := restartedStore.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(hazards) != 1 || hazards[0].Latest.Type != executionpkg.EventStarted || hazards[0].Identity.TurnID != admittedTurn {
		t.Fatalf("crash execution hazards = %#v", hazards)
	}
	warning := unresolvedExecutionWarning(hazards, true)

	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	m.agent.SetWorkDir(workspaceID)
	m.SetSessionStore(restartedStore)
	m.sessionLoading = true
	m.sessionLoadToken = 1
	updated, recoveryCommand := m.Update(SessionLoadedMsg{
		LoadToken: 1, SessionID: session.ID, State: persisted, StateRecord: stateRecord, Title: session.Title,
		RecoveryWarning: warning, ExecutionLease: restartedLease,
	})
	m = updated.(*Model)
	if recoveryCommand == nil {
		t.Fatal("restored outcome-unknown goal did not schedule reconciliation group ensure")
		return
	}
	recoveryResult := awaitCommandMessage[goalRecoveryLoadResultMsg](t, commandMessages(recoveryCommand), 3*time.Second)
	if recoveryResult.err != nil {
		t.Fatalf("ensure restored reconciliation group: %v", recoveryResult.err)
	}
	updated, _ = m.Update(recoveryResult)
	m = updated.(*Model)
	leaseOwnedByModel = true
	defer func() { _ = m.releaseExecutionSessionLease() }()
	if client.calls.Load() != 0 {
		t.Fatalf("restored crash state redispatched provider %d time(s)", client.calls.Load())
	}
	recovered := snapshotUIGoal(t, m.goalRuntime)
	if recovered.State != goal.StateBlocked || recovered.PendingContinuation != nil || recovered.LastPendingRecovery == nil {
		t.Fatalf("goal crash recovery = %#v", recovered)
	}
	if recovered.Usage.ContinuationTurns != 1 || recovered.LastPendingRecovery.Recovery.TurnID != admittedTurn {
		t.Fatalf("goal crash recovery refunded or changed permit: %#v", recovered)
	}
	if recovered.LastPendingRecovery.Recovery.Kind != goal.PendingOutcomeUnknown {
		t.Fatalf("goal crash recovery kind = %q", recovered.LastPendingRecovery.Recovery.Kind)
	}
	if cmd := m.resumeGoal(); cmd != nil {
		t.Fatal("blocked crash recovery returned a provider command")
	}
	if client.calls.Load() != 0 {
		t.Fatalf("blocked resume redispatched provider %d time(s)", client.calls.Load())
	}
	if m.executionCursor != 0 {
		t.Fatalf("unprojected crash execution advanced cursor to %d", m.executionCursor)
	}
	joinedEntries := ""
	for _, entry := range m.entries {
		joinedEntries += entry.Content + "\n"
	}
	for _, want := range []string{"Goal recovery blocked", "durable dispatch marker"} {
		if !strings.Contains(joinedEntries, want) {
			t.Fatalf("restored receipt missing %q: %s", want, joinedEntries)
		}
	}
	savedRaw, err := restartedStore.GetSessionState(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := decodeSessionState(savedRaw)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Goal == nil || saved.Goal.State != goal.StateBlocked || saved.Goal.PendingContinuation != nil || saved.ExecutionCursor != 0 {
		t.Fatalf("durable post-restart block = %#v", saved)
	}
	controlStates, err := restartedStore.ListControlStates(context.Background(), controlplane.Query{
		SessionID: session.ID, WorkspaceID: workspaceID,
		Kind: controlplane.KindExecutionReconciliation, PendingOnly: true, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(controlStates) != 1 || controlStates[0].Item.Identity.ExecutionID != hazards[0].Identity.ExecutionID || controlStates[0].Resolution != nil {
		t.Fatalf("durable execution reconciliation item = %#v", controlStates)
	}
	if _, err := os.Stat(filepath.Join(workspaceID, "must-not-exist.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery retry created crashed effect: %v", err)
	}
}
