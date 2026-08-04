package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

const maxSessionPublicIDAttempts = 8

// CreateSession inserts a session and always assigns a random mini-hash public
// id. Callers may leave PublicID empty; a non-empty caller-supplied value is
// validated and used as-is.
func (s *Store) CreateSession(ctx context.Context, arg CreateSessionParams) (Session, error) {
	if s == nil || s.Queries == nil {
		return Session{}, fmt.Errorf("session store is unavailable")
	}
	if arg.PublicID != "" {
		publicID, err := sessionref.Parse(arg.PublicID)
		if err != nil {
			return Session{}, err
		}
		arg.PublicID = publicID
		return s.Queries.CreateSession(ctx, arg)
	}
	var lastErr error
	for attempt := 0; attempt < maxSessionPublicIDAttempts; attempt++ {
		publicID, err := sessionref.New()
		if err != nil {
			return Session{}, err
		}
		arg.PublicID = publicID
		session, err := s.Queries.CreateSession(ctx, arg)
		if err == nil {
			return session, nil
		}
		lastErr = err
		if !sqliteConstraint(err) {
			return Session{}, err
		}
	}
	return Session{}, fmt.Errorf("create session public id: exhausted retries: %w", lastErr)
}

// ResolveSessionRef turns a user-facing handle into the durable session row.
func (s *Store) ResolveSessionRef(ctx context.Context, ref string) (Session, error) {
	if s == nil || s.Queries == nil {
		return Session{}, fmt.Errorf("session store is unavailable")
	}
	publicID, err := sessionref.Parse(ref)
	if err != nil {
		return Session{}, err
	}
	session, err := s.GetSessionByPublicID(ctx, publicID)
	if err != nil {
		return Session{}, fmt.Errorf("session %s: %w", publicID, err)
	}
	return session, nil
}

// SessionHandle returns the user-facing handle for a session id.
func (s *Store) SessionHandle(ctx context.Context, sessionID int64) (string, error) {
	if s == nil || s.Queries == nil {
		return "", fmt.Errorf("session store is unavailable")
	}
	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if handle := sessionref.Format(session.PublicID); handle != "" {
		return handle, nil
	}
	return "", fmt.Errorf("session %d has no public id", sessionID)
}

// UpdateSessionTitle persists a host- or model-authored session title.
func (s *Store) UpdateSessionTitle(ctx context.Context, sessionID int64, title string) error {
	if s == nil || s.Queries == nil {
		return fmt.Errorf("session store is unavailable")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("session title is empty")
	}
	if sessionID <= 0 {
		return fmt.Errorf("session id is required")
	}
	return s.Queries.UpdateSessionTitle(ctx, UpdateSessionTitleParams{
		Title: title,
		ID:    sessionID,
	})
}

func ensureSessionPublicIDs(conn *sql.DB) error {
	if conn == nil {
		return fmt.Errorf("migration database is nil")
	}
	found, err := tableColumnExists(conn, "sessions", "public_id")
	if err != nil {
		return err
	}
	if !found {
		// Migration 009 should have added the column. Fail closed if it is gone.
		return fmt.Errorf("sessions.public_id column is missing")
	}
	rows, err := conn.Query(`SELECT id FROM sessions WHERE public_id = '' OR public_id IS NULL`)
	if err != nil {
		return fmt.Errorf("list sessions missing public_id: %w", err)
	}
	var missing []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan session missing public_id: %w", err)
		}
		missing = append(missing, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close sessions missing public_id: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read sessions missing public_id: %w", err)
	}
	for _, id := range missing {
		if err := assignSessionPublicID(conn, id); err != nil {
			return err
		}
	}
	if _, err := conn.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS ux_sessions_public_id
		ON sessions(public_id)
		WHERE public_id != ''
	`); err != nil {
		return fmt.Errorf("index sessions public_id: %w", err)
	}
	return nil
}

func assignSessionPublicID(conn *sql.DB, sessionID int64) error {
	var lastErr error
	for attempt := 0; attempt < maxSessionPublicIDAttempts; attempt++ {
		publicID, err := sessionref.New()
		if err != nil {
			return err
		}
		result, err := conn.Exec(
			`UPDATE sessions SET public_id = ? WHERE id = ? AND (public_id = '' OR public_id IS NULL)`,
			publicID, sessionID,
		)
		if err != nil {
			lastErr = err
			if sqliteConstraint(err) {
				continue
			}
			return fmt.Errorf("backfill session %d public_id: %w", sessionID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("backfill session %d rows affected: %w", sessionID, err)
		}
		if affected == 1 {
			return nil
		}
		// Peer already filled it, or the row disappeared.
		return nil
	}
	return fmt.Errorf("backfill session %d public_id: exhausted retries: %w", sessionID, lastErr)
}
