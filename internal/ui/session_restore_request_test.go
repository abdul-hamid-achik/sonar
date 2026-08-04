package ui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/db"
)

func createRestorableSession(t *testing.T, store *db.Store, workspace, title, content string) db.Session {
	t.Helper()
	session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
		Title: title, WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	state := persistedSessionState{
		Version: currentPersistedSessionVersion,
		Mode:    ModeNormal,
		Entries: []persistedChatEntry{{Kind: "user", Content: content}},
	}
	raw, err := marshalPersistedSessionState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSessionState(context.Background(), session.ID, raw); err != nil {
		t.Fatal(err)
	}
	return session
}

func newSessionRestoreTestModel(t *testing.T, store *db.Store, workspace string) *Model {
	t.Helper()
	m := newTestModel(t)
	m.SetSessionStore(store)
	m.agent.SetWorkDir(workspace)
	m.entries = []ChatEntry{{Kind: "user", Content: "current"}}
	return m
}

func TestSessionRestoreRequestUsesDatabaseTitleAndCanonicalState(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := canonicalSessionTestWorkspace(t)
	session := createRestorableSession(t, store, workspace, "database title", "restored")
	m := newSessionRestoreTestModel(t, store, workspace)
	selector, err := SessionIDResumeSelector(session.ID)
	if err != nil {
		t.Fatal(err)
	}

	receipt := awaitCommandMessage[SessionLoadedMsg](t, commandMessages(m.requestSessionRestore(selector)), 2*time.Second)
	if receipt.Err != nil {
		t.Fatalf("restore receipt: %v", receipt.Err)
	}
	if receipt.Title != "database title" || receipt.SessionID != session.ID || receipt.StateRecord.SessionID != session.ID || receipt.ExecutionLease == nil {
		t.Fatalf("restore receipt = %#v", receipt)
	}
	updated, cmd := m.Update(receipt)
	m = updated.(*Model)
	if cmd != nil {
		_ = cmd // recovery projection is allowed; it never starts provider work.
	}
	if m.sessionID != session.ID || len(m.entries) < 2 || m.entries[0].Content != "restored" || !strings.Contains(m.entries[len(m.entries)-1].Content, "database title") {
		t.Fatalf("restored model session=%d entries=%#v", m.sessionID, m.entries)
	}
	if err := m.ReleaseExecutionSessionLease(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRestoreRequestSanitizesPersistedTitleBeforeChat(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "restore-title.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := canonicalSessionTestWorkspace(t)
	session := createRestorableSession(t, store, workspace, "safe\x1b]8;;https://example.invalid\x07link\x1b]8;;\x07\n\u202eevil", "restored")
	m := newSessionRestoreTestModel(t, store, workspace)
	selector, err := SessionIDResumeSelector(session.ID)
	if err != nil {
		t.Fatal(err)
	}

	receipt := awaitCommandMessage[SessionLoadedMsg](t, commandMessages(m.requestSessionRestore(selector)), 2*time.Second)
	if receipt.Err != nil {
		t.Fatalf("restore receipt: %v", receipt.Err)
	}
	if got, want := receipt.Title, "safelink evil"; got != want {
		t.Fatalf("restore title = %q, want %q", got, want)
	}
	updated, _ := m.Update(receipt)
	m = updated.(*Model)
	if got, want := m.entries[len(m.entries)-1].Content, "Restored session "+sessionDisplayLabel(session.PublicID, "safelink evil", 72); got != want {
		t.Fatalf("restore entry = %q, want %q", got, want)
	}
	if m.activeSessionTitle != "safelink evil" {
		t.Fatalf("active session title = %q", m.activeSessionTitle)
	}
	if err := m.ReleaseExecutionSessionLease(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRestoreRequestRejectsInvalidCrossWorkspaceAndBusyLease(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "guards.db")
	first, err := db.OpenPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := db.OpenPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	workspace := canonicalSessionTestWorkspace(t)
	foreignWorkspace := canonicalSessionTestWorkspace(t)
	local := createRestorableSession(t, first, workspace, "local", "local")
	foreign := createRestorableSession(t, first, foreignWorkspace, "foreign", "foreign")

	t.Run("invalid selector", func(t *testing.T) {
		m := newSessionRestoreTestModel(t, second, workspace)
		receipt := awaitCommandMessage[SessionLoadedMsg](t, commandMessages(m.requestSessionRestore(SessionResumeSelector{})), 2*time.Second)
		if receipt.Err == nil || !strings.Contains(receipt.Err.Error(), "invalid session resume selector") {
			t.Fatalf("invalid selector error = %v", receipt.Err)
		}
	})

	t.Run("cross workspace", func(t *testing.T) {
		m := newSessionRestoreTestModel(t, second, workspace)
		selector, _ := SessionIDResumeSelector(foreign.ID)
		receipt := awaitCommandMessage[SessionLoadedMsg](t, commandMessages(m.requestSessionRestore(selector)), 2*time.Second)
		if receipt.Err == nil || !strings.Contains(strings.ToLower(receipt.Err.Error()), "workspace") {
			t.Fatalf("cross-workspace error = %v", receipt.Err)
		}
	})

	t.Run("busy lease", func(t *testing.T) {
		lease, err := first.AcquireExecutionSessionLease(context.Background(), local.ID, workspace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lease.Close() }()
		m := newSessionRestoreTestModel(t, second, workspace)
		selector, _ := SessionIDResumeSelector(local.ID)
		receipt := awaitCommandMessage[SessionLoadedMsg](t, commandMessages(m.requestSessionRestore(selector)), 2*time.Second)
		if !errors.Is(receipt.Err, db.ErrExecutionSessionBusy) {
			t.Fatalf("busy lease error = %v", receipt.Err)
		}
	})
}

func TestSessionRestoreRequestCancellationAndStaleGenerationCannotReplaceState(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "generation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := canonicalSessionTestWorkspace(t)
	first := createRestorableSession(t, store, workspace, "first", "first restored")
	second := createRestorableSession(t, store, workspace, "second", "second restored")
	m := newSessionRestoreTestModel(t, store, workspace)
	firstSelector, _ := SessionIDResumeSelector(first.ID)
	secondSelector, _ := SessionIDResumeSelector(second.ID)

	firstCmd := m.requestSessionRestore(firstSelector)
	_ = m.requestSessionRestore(secondSelector)
	stale := awaitCommandMessage[SessionLoadedMsg](t, commandMessages(firstCmd), 2*time.Second)
	updated, _ := m.Update(stale)
	m = updated.(*Model)
	if m.sessionID != 0 || m.entries[0].Content != "current" || !m.sessionLoading {
		t.Fatalf("stale restore changed state: session=%d loading=%v entries=%#v", m.sessionID, m.sessionLoading, m.entries)
	}
	m.cancelSessionLoad()

	cancelledCmd := m.requestSessionRestore(secondSelector)
	m.cancelSessionLoad()
	cancelled := awaitCommandMessage[SessionLoadedMsg](t, commandMessages(cancelledCmd), 2*time.Second)
	updated, _ = m.Update(cancelled)
	m = updated.(*Model)
	if m.sessionID != 0 || m.entries[0].Content != "current" || m.sessionLoading {
		t.Fatalf("cancelled restore changed state: session=%d loading=%v entries=%#v", m.sessionID, m.sessionLoading, m.entries)
	}
}

func TestStartupSessionResumeWaitsForInitCompleteAndDoesNotAutoRun(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "startup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := canonicalSessionTestWorkspace(t)
	session := createRestorableSession(t, store, workspace, "startup database title", "startup restored")
	selector, err := SessionIDResumeSelector(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	client := &goalCountingClient{}
	m := newGoalRuntimeTestModel(t, client)
	m.agent.SetWorkDir(workspace)
	m.SetSessionStore(store)
	m.initializing = true
	if err := m.SetStartupSessionResume(selector); err != nil {
		t.Fatal(err)
	}
	if m.sessionLoading || m.sessionID != 0 {
		t.Fatalf("startup restore began before InitComplete: loading=%v session=%d", m.sessionLoading, m.sessionID)
	}

	updated, cmd := m.Update(InitCompleteMsg{Model: client.Model(), NumCtx: 4096})
	m = updated.(*Model)
	if cmd == nil || m.initializing || !m.sessionLoading || m.sessionID != 0 {
		t.Fatalf("InitComplete startup state: cmd=%v initializing=%v loading=%v session=%d", cmd != nil, m.initializing, m.sessionLoading, m.sessionID)
	}
	if calls := client.calls.Load(); calls != 0 {
		t.Fatalf("startup selector dispatched provider work before restore: %d", calls)
	}
	receipt := awaitCommandMessage[SessionLoadedMsg](t, commandMessages(cmd), 2*time.Second)
	updated, followup := m.Update(receipt)
	m = updated.(*Model)
	if receipt.Err != nil || m.sessionID != session.ID || m.state != StateIdle || m.sessionLoading {
		t.Fatalf("startup restore receipt=%v session=%d state=%v loading=%v", receipt.Err, m.sessionID, m.state, m.sessionLoading)
	}
	if calls := client.calls.Load(); calls != 0 {
		t.Fatalf("startup restore auto-dispatched provider work: %d", calls)
	}
	if followup != nil {
		// A paused-goal recovery projection may be returned for goal sessions;
		// this fixture has no goal and must not schedule any work.
		t.Fatal("clean startup restore scheduled unexpected follow-up work")
	}
	if len(m.entries) < 2 || m.entries[0].Content != "startup restored" || !strings.Contains(m.entries[len(m.entries)-1].Content, "startup database title") {
		t.Fatalf("startup entries = %#v", m.entries)
	}
	if err := m.ReleaseExecutionSessionLease(); err != nil {
		t.Fatal(err)
	}
}
