package ui

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestGoalSessionStateRoundTripAndLegacyClear(t *testing.T) {
	source := newGoalRuntimeTestModel(t, &goalCountingClient{})
	source.sessionID = 42
	source.goalRuntime = newUIGoalRuntime(t, source.sessionID, goal.BudgetLimits{
		MaxContinuationTurns: 4,
		MaxEvalTokens:        2_000,
	})
	recordUIGoalTurn(t, source.goalRuntime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: "turn_saved", EvalTokens: 31, Productive: true, Summary: "saved progress",
	})

	raw, err := encodeSessionState(source)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeSessionState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if state.Goal == nil || state.Goal.SessionID != source.sessionID {
		t.Fatalf("encoded goal = %#v", state.Goal)
	}

	target := newGoalRuntimeTestModel(t, &goalCountingClient{})
	target.goalRuntime = newUIGoalRuntime(t, 99, goal.BudgetLimits{})
	if err := target.restoreSessionState(state); err != nil {
		t.Fatal(err)
	}
	restored := snapshotUIGoal(t, target.goalRuntime)
	if restored.ID != state.Goal.ID || restored.SessionID != 42 || restored.Usage.EvalTokens != 31 {
		t.Fatalf("restored goal = %#v", restored)
	}

	legacy, err := decodeSessionState(`{"version":1,"messages":[],"entries":[],"mode":2}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.restoreSessionState(legacy); err != nil {
		t.Fatal(err)
	}
	if target.goalRuntime != nil {
		t.Fatal("legacy session retained a goal owned by the previous session")
	}
}

func TestMalformedGoalRestorePreservesCurrentRuntimeTransactionally(t *testing.T) {
	m := newGoalRuntimeTestModel(t, &goalCountingClient{})
	m.mode = ModeAsk
	m.entries = []ChatEntry{{Kind: "user", Content: "current transcript"}}
	m.loadedFile = "current.md"
	m.manualLoadedContext = "current context"
	m.agent.ReplaceMessages([]llm.Message{{Role: "user", Content: "current prompt"}})
	m.goalRuntime = newUIGoalRuntime(t, 42, goal.BudgetLimits{MaxContinuationTurns: 3})

	beforeGoal := snapshotUIGoal(t, m.goalRuntime)
	beforeEntries := append([]ChatEntry(nil), m.entries...)
	beforeMessages := m.agent.Messages()
	malformed := beforeGoal
	malformed.Version = goal.SnapshotVersion + 1

	err := m.restoreSessionState(persistedSessionState{
		Version:             1,
		Mode:                ModeBuild,
		Messages:            []llm.Message{{Role: "user", Content: "must not replace"}},
		Entries:             []persistedChatEntry{{Kind: "user", Content: "must not replace"}},
		LoadedFile:          "must-not-commit.md",
		ManualLoadedContext: "must not commit",
		Goal:                &malformed,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported goal snapshot version") {
		t.Fatalf("malformed goal restore error = %v", err)
	}

	afterGoal := snapshotUIGoal(t, m.goalRuntime)
	if !reflect.DeepEqual(afterGoal, beforeGoal) {
		t.Fatalf("malformed restore changed goal:\nbefore=%#v\nafter=%#v", beforeGoal, afterGoal)
	}
	if m.mode != ModeAsk || !reflect.DeepEqual(m.entries, beforeEntries) || !reflect.DeepEqual(m.agent.Messages(), beforeMessages) {
		t.Fatalf("malformed goal restore changed runtime: mode=%v entries=%#v messages=%#v", m.mode, m.entries, m.agent.Messages())
	}
	if m.loadedFile != "current.md" || m.manualLoadedContext != "current context" {
		t.Fatalf("malformed goal restore changed context: file=%q context=%q", m.loadedFile, m.manualLoadedContext)
	}
}

func TestLoadPersistedSessionRejectsGoalOwnedByDifferentSession(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "goal-session-mismatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "goal mismatch", Mode: "BUILD", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongRuntime := newUIGoalRuntime(t, session.ID+1, goal.BudgetLimits{})
	wrongSnapshot := snapshotUIGoal(t, wrongRuntime)
	raw, err := marshalPersistedSessionState(persistedSessionState{
		Version: 1, Mode: ModeBuild, Goal: &wrongSnapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), session.ID, raw); err != nil {
		t.Fatal(err)
	}

	_, _, _, err = loadPersistedSession(context.Background(), store, session.ID, workspace)
	if err == nil || !strings.Contains(err.Error(), "contains goal state for session") {
		t.Fatalf("cross-session goal load error = %v", err)
	}
}

func TestRestoredPendingTurnAdmissionsBlockWithoutProviderDispatch(t *testing.T) {
	for _, test := range []struct {
		name              string
		kind              goal.TurnAdmissionKind
		priorTurn         bool
		wantContinuations int64
	}{
		{name: "initial", kind: goal.AdmissionInitial},
		{name: "manual", kind: goal.AdmissionManual, priorTurn: true},
		{name: "automatic", kind: goal.AdmissionAutomatic, priorTurn: true, wantContinuations: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &goalCountingClient{}
			m := newGoalRuntimeTestModel(t, client)
			store, err := db.OpenPath(filepath.Join(t.TempDir(), "goal-pending-restore.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			workspace, err := canonicalWorkspaceID(m.agent.WorkDir())
			if err != nil {
				t.Fatal(err)
			}
			m.agent.SetWorkDir(workspace)
			session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
				Title: "pending goal", Mode: "BUILD", WorkspaceID: workspace,
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime := newUIGoalRuntime(t, session.ID, goal.BudgetLimits{MaxContinuationTurns: 3})
			if test.priorTurn {
				recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
					TurnID: "turn_initial", Productive: true, Summary: "verified progress",
				})
			}
			if _, err := runtime.BeginTurn(context.Background(), "turn_orphaned", test.kind); err != nil {
				t.Fatal(err)
			}
			pending := snapshotUIGoal(t, runtime)
			state := persistedSessionState{Version: 1, Mode: ModeBuild, Goal: &pending}
			raw, err := marshalPersistedSessionState(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveSessionState(context.Background(), session.ID, raw); err != nil {
				t.Fatal(err)
			}
			stateRecord, err := store.GetSessionStateRecord(context.Background(), session.ID)
			if err != nil {
				t.Fatal(err)
			}

			m.SetSessionStore(store)
			m.sessionLoading = true
			m.sessionLoadToken = 7
			updated, cmd := m.Update(SessionLoadedMsg{
				LoadToken: 7, SessionID: session.ID, State: state, StateRecord: stateRecord, Title: "pending goal",
			})
			m = updated.(*Model)
			if cmd != nil {
				t.Fatalf("restoring an orphaned %s turn returned a provider command", test.kind)
			}
			if got := client.calls.Load(); got != 0 {
				t.Fatalf("restored orphaned %s turn dispatched %d provider calls", test.kind, got)
			}

			recovered := snapshotUIGoal(t, m.goalRuntime)
			if recovered.State != goal.StateBlocked || recovered.PendingContinuation != nil || recovered.Blocker == nil {
				t.Fatalf("restored orphaned %s turn = %#v", test.kind, recovered)
			}
			if recovered.LastPendingRecovery == nil || recovered.LastPendingRecovery.Recovery.Kind != goal.PendingOutcomeUnknown || recovered.LastPendingRecovery.Permit.Kind != test.kind {
				t.Fatalf("restored %s turn lacked bound outcome-unknown recovery: %#v", test.kind, recovered.LastPendingRecovery)
			}
			if recovered.Usage.ContinuationTurns != test.wantContinuations {
				t.Fatalf("restored %s continuation usage = %#v", test.kind, recovered.Usage)
			}

			savedRaw, err := store.GetSessionState(context.Background(), session.ID)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := decodeSessionState(savedRaw)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Goal == nil || saved.Goal.State != goal.StateBlocked || saved.Goal.PendingContinuation != nil {
				t.Fatalf("recovered %s goal was not durably saved: %#v", test.kind, saved.Goal)
			}
			if len(m.entries) == 0 || !strings.Contains(m.entries[0].Content, "Goal recovery blocked") {
				t.Fatalf("restored goal did not explain recovery block: %#v", m.entries)
			}
		})
	}
}

func TestRestoredActiveGoalPausesDurablyWithoutProviderDispatch(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "goal-active-restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := m.agent.WorkDir()
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "active goal", Mode: "BUILD", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newUIGoalRuntime(t, session.ID, goal.BudgetLimits{MaxContinuationTurns: 3})
	active := snapshotUIGoal(t, runtime)
	state := persistedSessionState{Version: 1, Mode: ModeBuild, Goal: &active}
	raw, err := marshalPersistedSessionState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), session.ID, raw); err != nil {
		t.Fatal(err)
	}
	stateRecord, err := store.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}

	m.SetSessionStore(store)
	m.sessionLoading = true
	m.sessionLoadToken = 9
	updated, cmd := m.Update(SessionLoadedMsg{
		LoadToken: 9, SessionID: session.ID, State: state, StateRecord: stateRecord, Title: "active goal",
	})
	m = updated.(*Model)
	if cmd != nil {
		t.Fatal("restoring an active goal returned a command that could dispatch work")
	}
	if got := client.calls.Load(); got != 0 {
		t.Fatalf("restored active goal dispatched %d provider calls", got)
	}
	recovered := snapshotUIGoal(t, m.goalRuntime)
	if recovered.State != goal.StatePaused || !strings.Contains(recovered.StateReason, "resume explicitly") {
		t.Fatalf("restored active goal = %#v", recovered)
	}

	savedRaw, err := store.GetSessionState(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := decodeSessionState(savedRaw)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Goal == nil || saved.Goal.State != goal.StatePaused {
		t.Fatalf("restored active pause was not durably saved: %#v", saved.Goal)
	}
}

func TestSettledTurnSaveFailureStopsAndRestoresPendingAsOutcomeUnknown(t *testing.T) {
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	databasePath := filepath.Join(t.TempDir(), "goal-settlement-failure.db")
	store, err := db.OpenPath(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := canonicalWorkspaceID(m.agent.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	m.agent.SetWorkDir(workspace)
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: "settlement failure", Mode: "BUILD", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newUIGoalRuntime(t, session.ID, goal.BudgetLimits{MaxContinuationTurns: 3})
	recordUIGoalTurn(t, runtime, goal.AdmissionInitial, goal.TurnReport{
		TurnID: "turn_initial", Productive: true, Summary: "verified progress",
	})
	if _, err := runtime.BeginContinuation(context.Background(), "turn_settled"); err != nil {
		t.Fatal(err)
	}
	m.SetSessionStore(store)
	m.sessionID = session.ID
	if err := m.initializeSessionStateRevision(0); err != nil {
		t.Fatal(err)
	}
	m.goalRuntime = runtime
	if err := m.persistGoalSession(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	m.goalTurnID = "turn_settled"
	m.goalTurnToolCalls = 1
	m.goalTurnSuccesses = 1
	m.turnEvalTotal = 13
	m.state = StateStreaming
	updated, _ := m.Update(AgentDoneMsg{TurnID: "turn_settled"})
	m = updated.(*Model)
	if !m.goalPersistenceDirty || m.goalNeedsEvaluation || m.goalOperationRunning || client.calls.Load() != 0 {
		t.Fatalf("failed settlement save left work armed: dirty=%v evaluate=%v operation=%v calls=%d", m.goalPersistenceDirty, m.goalNeedsEvaluation, m.goalOperationRunning, client.calls.Load())
	}
	if snapshot := snapshotUIGoal(t, runtime); snapshot.State != goal.StatePaused || snapshot.PendingContinuation != nil {
		t.Fatalf("in-memory settlement did not stop safely: %#v", snapshot)
	}

	reopened, err := db.OpenPath(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	stateRecord, err := reopened.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := decodeSessionState(stateRecord.StateJSON)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Goal == nil || saved.Goal.PendingContinuation == nil || saved.Goal.PendingContinuation.TurnID != "turn_settled" {
		t.Fatalf("crash snapshot did not retain admitted permit: %#v", saved.Goal)
	}

	restarted := newGoalRuntimeTestModel(t, client)
	restarted.agent.SetWorkDir(workspace)
	restarted.SetSessionStore(reopened)
	restarted.sessionID = session.ID
	if err := restarted.initializeSessionStateRevision(stateRecord.Revision); err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreSessionState(saved); err != nil {
		t.Fatal(err)
	}
	if err := restarted.recoverRestoredGoal(); err != nil {
		t.Fatal(err)
	}
	recovered := snapshotUIGoal(t, restarted.goalRuntime)
	if recovered.State != goal.StateBlocked || recovered.PendingContinuation != nil || recovered.LastPendingRecovery == nil {
		t.Fatalf("restart did not recover pending permit as outcome-unknown: %#v", recovered)
	}
	if recovered.LastPendingRecovery.Recovery.Kind != goal.PendingOutcomeUnknown || client.calls.Load() != 0 {
		t.Fatalf("restart recovery kind=%q calls=%d", recovered.LastPendingRecovery.Recovery.Kind, client.calls.Load())
	}
}
