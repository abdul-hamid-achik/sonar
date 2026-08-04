package db

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
)

func TestSessionStateRoundTrip(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session, err := store.CreateSession(ctx, CreateSessionParams{Title: "state", Model: "qwen", Mode: "BUILD"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSessionState(ctx, session.ID); !errors.Is(err, ErrSessionStateNotFound) {
		t.Fatalf("missing state error = %v", err)
	}
	if err := store.SaveSessionState(ctx, session.ID, `{"turn":1}`); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSessionState(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"turn":1}` {
		t.Fatalf("state = %q", got)
	}
	first, err := store.GetSessionStateRecord(ctx, session.ID)
	if err != nil || first.Revision != 1 {
		t.Fatalf("first state record = %#v, error=%v", first, err)
	}
	if err := store.SaveSessionState(ctx, session.ID, `{"turn":2}`); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetSessionState(ctx, session.ID)
	if err != nil || got != `{"turn":2}` {
		t.Fatalf("updated state = %q, err=%v", got, err)
	}
	second, err := store.GetSessionStateRecord(ctx, session.ID)
	if err != nil || second.Revision != 2 {
		t.Fatalf("second state record = %#v, error=%v", second, err)
	}
}

func TestSessionStateCASMissingStaleAndLegacyRevision(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session, err := store.CreateSession(ctx, CreateSessionParams{Title: "cas", Model: "qwen", Mode: "BUILD"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.SaveSessionStateCAS(ctx, session.ID, 0, `{"turn":1}`)
	if err != nil || created.Revision != 1 {
		t.Fatalf("create CAS = %#v, error=%v", created, err)
	}
	if _, err := store.SaveSessionStateCAS(ctx, session.ID, 0, `{"turn":"stale"}`); !errors.Is(err, ErrSessionStateConflict) {
		t.Fatalf("stale CAS error = %v", err)
	}
	updated, err := store.SaveSessionStateCAS(ctx, session.ID, created.Revision, `{"turn":2}`)
	if err != nil || updated.Revision != 2 {
		t.Fatalf("update CAS = %#v, error=%v", updated, err)
	}

	legacy, err := store.CreateSession(ctx, CreateSessionParams{Title: "legacy", Model: "qwen", Mode: "BUILD"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO session_state (session_id, state_json, revision) VALUES (?, '{}', 0)`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	upgraded, err := store.SaveSessionStateCAS(ctx, legacy.ID, 0, `{"version":1}`)
	if err != nil || upgraded.Revision != 1 {
		t.Fatalf("legacy revision CAS = %#v, error=%v", upgraded, err)
	}
}

func TestSessionStateCASConcurrentWritersOnlyOneWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state-cas.db")
	first, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	session, err := first.CreateSession(context.Background(), CreateSessionParams{Title: "race", Model: "qwen", Mode: "BUILD"})
	if err != nil {
		t.Fatal(err)
	}
	base, err := first.SaveSessionStateCAS(context.Background(), session.ID, 0, `{"writer":"base"}`)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for index, store := range []*Store{first, second} {
		index, store := index, store
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, writeErr := store.SaveSessionStateCAS(context.Background(), session.ID, base.Revision,
				`{"writer":`+string(rune('1'+index))+`}`)
			errs <- writeErr
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrSessionStateConflict):
			conflicts++
		default:
			t.Fatalf("concurrent CAS error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent CAS successes=%d conflicts=%d", successes, conflicts)
	}
	record, err := first.GetSessionStateRecord(context.Background(), session.ID)
	if err != nil || record.Revision != base.Revision+1 {
		t.Fatalf("concurrent CAS record = %#v, error=%v", record, err)
	}
}

func TestSessionStateRevisionCorruptionOverflowAndCommitReadback(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session, err := store.CreateSession(ctx, CreateSessionParams{Title: "bounds", Model: "qwen", Mode: "BUILD"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.SaveSessionStateCAS(ctx, session.ID, 0, `{"safe":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !committedSessionState(store, session.ID, record) {
		t.Fatal("exact ambiguous-commit readback was not recognized")
	}
	altered := record
	altered.StateJSON = `{"forged":true}`
	if committedSessionState(store, session.ID, altered) {
		t.Fatal("different ambiguous-commit payload was accepted")
	}

	if _, err := store.db.Exec(`UPDATE session_state SET revision = ? WHERE session_id = ?`, int64(math.MaxInt64), session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSessionStateCAS(ctx, session.ID, math.MaxInt64, `{"overflow":true}`); !errors.Is(err, ErrSessionStateRevisionExhausted) {
		t.Fatalf("overflow CAS error = %v", err)
	}
	if err := store.SaveSessionState(ctx, session.ID, `{"overflow":true}`); !errors.Is(err, ErrSessionStateRevisionExhausted) {
		t.Fatalf("overflow blind save error = %v", err)
	}

	if _, err := store.db.Exec(`PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE session_state SET revision = -1 WHERE session_id = ?`, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSessionStateRecord(ctx, session.ID); err == nil {
		t.Fatal("negative durable revision was accepted")
	}
}

func TestSessionStateLegacySchemaGetsIdempotentRevisionEnsure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-state.db")
	conn, err := sql.Open("sqlite", path+"?_foreign_keys=ON")
	if err != nil {
		t.Fatal(err)
	}
	initSQL, err := migrations.ReadFile("migrations/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(string(initSQL)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`
		CREATE TABLE session_state (
			session_id INTEGER PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
			state_json TEXT NOT NULL DEFAULT '{}',
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		)`); err != nil {
		t.Fatal(err)
	}
	result, err := conn.Exec(`INSERT INTO sessions (title, model, mode, workspace_id) VALUES ('legacy', 'qwen', 'BUILD', '/workspace')`)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO session_state (session_id, state_json) VALUES (?, '{"legacy":true}')`, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	record, err := store.GetSessionStateRecord(context.Background(), sessionID)
	if err != nil || record.Revision != 0 || record.StateJSON != `{"legacy":true}` {
		t.Fatalf("ensured legacy state = %#v, error=%v", record, err)
	}
	updated, err := store.SaveSessionStateCAS(context.Background(), sessionID, 0, `{"legacy":false}`)
	if err != nil || updated.Revision != 1 {
		t.Fatalf("legacy CAS after ensure = %#v, error=%v", updated, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenPath(path)
	if err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
}
