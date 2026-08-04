package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

var (
	ErrSessionStateNotFound          = errors.New("session state not found")
	ErrSessionStateConflict          = errors.New("session state revision conflict")
	ErrSessionStateRevisionExhausted = errors.New("session state revision exhausted")
)

// SessionStateRecord is the lossless session envelope plus its monotonic
// compare-and-swap generation.
type SessionStateRecord struct {
	SessionID int64
	StateJSON string
	Revision  int64
	UpdatedAt time.Time
}

// SaveSessionState atomically replaces the lossless state for one session and
// advances its revision. Callers coordinating more than one durable record
// should use SaveSessionStateCAS or the transaction-local helper instead.
func (s *Store) SaveSessionState(ctx context.Context, sessionID int64, stateJSON string) error {
	if sessionID <= 0 {
		return fmt.Errorf("invalid session id %d", sessionID)
	}
	stateJSON = normalizedSessionStateJSON(stateJSON)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session state save: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	record, err := upsertSessionStateTx(ctx, tx, sessionID, stateJSON)
	if err != nil {
		return err
	}
	if err := touchSessionTx(ctx, tx, sessionID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		if committedSessionState(s, sessionID, record) {
			return nil
		}
		return fmt.Errorf("commit session state: %w", err)
	}
	return nil
}

// SaveSessionStateCAS replaces a session envelope only when expectedRevision
// matches the durable generation. Revision zero also creates a missing row.
// The returned record contains the newly committed generation.
func (s *Store) SaveSessionStateCAS(ctx context.Context, sessionID, expectedRevision int64, stateJSON string) (SessionStateRecord, error) {
	if sessionID <= 0 {
		return SessionStateRecord{}, fmt.Errorf("invalid session id %d", sessionID)
	}
	if expectedRevision < 0 {
		return SessionStateRecord{}, fmt.Errorf("invalid expected session state revision %d", expectedRevision)
	}
	stateJSON = normalizedSessionStateJSON(stateJSON)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionStateRecord{}, fmt.Errorf("begin session state compare-and-swap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	record, err := saveSessionStateCASTx(ctx, tx, sessionID, expectedRevision, stateJSON)
	if err != nil {
		return SessionStateRecord{}, err
	}
	if err := touchSessionTx(ctx, tx, sessionID); err != nil {
		return SessionStateRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		if committedSessionState(s, sessionID, record) {
			return record, nil
		}
		return SessionStateRecord{}, fmt.Errorf("commit session state compare-and-swap: %w", err)
	}
	return record, nil
}

// GetSessionState returns the lossless state payload for one session.
func (s *Store) GetSessionState(ctx context.Context, sessionID int64) (string, error) {
	record, err := s.GetSessionStateRecord(ctx, sessionID)
	if err != nil {
		return "", err
	}
	return record.StateJSON, nil
}

// GetSessionStateRecord returns the lossless state and its CAS generation.
func (s *Store) GetSessionStateRecord(ctx context.Context, sessionID int64) (SessionStateRecord, error) {
	if sessionID <= 0 {
		return SessionStateRecord{}, fmt.Errorf("invalid session id %d", sessionID)
	}
	return getSessionStateRecord(ctx, s.db, sessionID)
}

type sessionStateQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getSessionStateRecord(ctx context.Context, q sessionStateQuerier, sessionID int64) (SessionStateRecord, error) {
	var record SessionStateRecord
	var updatedAt string
	err := q.QueryRowContext(ctx, `
		SELECT session_id, state_json, revision, updated_at
		  FROM session_state
		 WHERE session_id = ?`, sessionID,
	).Scan(&record.SessionID, &record.StateJSON, &record.Revision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionStateRecord{}, ErrSessionStateNotFound
	}
	if err != nil {
		return SessionStateRecord{}, fmt.Errorf("get session state: %w", err)
	}
	if record.Revision < 0 {
		return SessionStateRecord{}, fmt.Errorf("get session state: negative durable revision %d", record.Revision)
	}
	record.UpdatedAt, err = parseExecutionTime(updatedAt)
	if err != nil {
		return SessionStateRecord{}, fmt.Errorf("parse session state update time: %w", err)
	}
	return record, nil
}

func upsertSessionStateTx(ctx context.Context, tx *sql.Tx, sessionID int64, stateJSON string) (SessionStateRecord, error) {
	current, err := getSessionStateRecord(ctx, tx, sessionID)
	switch {
	case errors.Is(err, ErrSessionStateNotFound):
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_state (session_id, state_json, revision) VALUES (?, ?, 1)`,
			sessionID, stateJSON)
		if err != nil {
			return SessionStateRecord{}, fmt.Errorf("insert session state: %w", err)
		}
	case err != nil:
		return SessionStateRecord{}, err
	case current.Revision == math.MaxInt64:
		return SessionStateRecord{}, ErrSessionStateRevisionExhausted
	default:
		result, err := tx.ExecContext(ctx, `
			UPDATE session_state
			   SET state_json = ?, revision = revision + 1,
			       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			 WHERE session_id = ? AND revision = ?`, stateJSON, sessionID, current.Revision)
		if err != nil {
			return SessionStateRecord{}, fmt.Errorf("save session state: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return SessionStateRecord{}, fmt.Errorf("read saved session state count: %w", err)
		}
		if affected != 1 {
			return SessionStateRecord{}, fmt.Errorf("%w: blind update raced at revision %d", ErrSessionStateConflict, current.Revision)
		}
	}
	record, err := getSessionStateRecord(ctx, tx, sessionID)
	if err != nil {
		return SessionStateRecord{}, fmt.Errorf("read saved session state: %w", err)
	}
	return record, nil
}

// saveSessionStateCASTx is the transaction-local state primitive used by the
// later atomic reconciliation commit. It does not touch sessions.updated_at or
// commit the caller's transaction.
func saveSessionStateCASTx(ctx context.Context, tx *sql.Tx, sessionID, expectedRevision int64, stateJSON string) (SessionStateRecord, error) {
	if expectedRevision == math.MaxInt64 {
		return SessionStateRecord{}, ErrSessionStateRevisionExhausted
	}
	current, err := getSessionStateRecord(ctx, tx, sessionID)
	switch {
	case errors.Is(err, ErrSessionStateNotFound):
		if expectedRevision != 0 {
			return SessionStateRecord{}, fmt.Errorf("%w: row is missing, expected revision %d", ErrSessionStateConflict, expectedRevision)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_state (session_id, state_json, revision) VALUES (?, ?, 1)`,
			sessionID, stateJSON,
		); err != nil {
			if sqliteConstraint(err) {
				return SessionStateRecord{}, fmt.Errorf("%w: session state was concurrently created", ErrSessionStateConflict)
			}
			return SessionStateRecord{}, fmt.Errorf("insert session state compare-and-swap: %w", err)
		}
	case err != nil:
		return SessionStateRecord{}, err
	case current.Revision != expectedRevision:
		return SessionStateRecord{}, fmt.Errorf("%w: durable revision is %d, expected %d", ErrSessionStateConflict, current.Revision, expectedRevision)
	default:
		result, err := tx.ExecContext(ctx, `
			UPDATE session_state
			   SET state_json = ?, revision = revision + 1,
			       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			 WHERE session_id = ? AND revision = ?`,
			stateJSON, sessionID, expectedRevision,
		)
		if err != nil {
			return SessionStateRecord{}, fmt.Errorf("update session state compare-and-swap: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return SessionStateRecord{}, fmt.Errorf("read session state compare-and-swap count: %w", err)
		}
		if affected != 1 {
			return SessionStateRecord{}, fmt.Errorf("%w: update affected %d rows", ErrSessionStateConflict, affected)
		}
	}
	record, err := getSessionStateRecord(ctx, tx, sessionID)
	if err != nil {
		return SessionStateRecord{}, fmt.Errorf("read compared-and-swapped session state: %w", err)
	}
	if record.Revision != expectedRevision+1 || record.StateJSON != stateJSON {
		return SessionStateRecord{}, fmt.Errorf("%w: compare-and-swap postcondition failed", ErrSessionStateConflict)
	}
	return record, nil
}

func touchSessionTx(ctx context.Context, tx *sql.Tx, sessionID int64) error {
	result, err := tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("update session timestamp: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read updated session row count: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("update session timestamp affected %d rows, want 1", affected)
	}
	return nil
}

func committedSessionState(s *Store, sessionID int64, expected SessionStateRecord) bool {
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stored, err := s.GetSessionStateRecord(readCtx, sessionID)
	return err == nil && stored.Revision == expected.Revision && stored.StateJSON == expected.StateJSON
}

func normalizedSessionStateJSON(value string) string {
	if value == "" {
		return "{}"
	}
	return value
}
