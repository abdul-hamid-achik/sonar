package ui

import (
	"context"
	"fmt"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

// SessionResumeSelector identifies one exact saved session or the newest
// session in the canonical current workspace. Its fields are closed so callers
// must pass through the validated constructors below.
type SessionResumeSelector struct {
	sessionID int64
	publicID  string
	latest    bool
}

// ParseSessionResumeSelector parses the value accepted by --resume.
func ParseSessionResumeSelector(value string) (SessionResumeSelector, error) {
	if value == "" {
		return SessionResumeSelector{}, fmt.Errorf("resume selector is required (use a 7-char hex handle like a1b2c3d or latest)")
	}
	if value == "latest" {
		return SessionResumeSelector{latest: true}, nil
	}
	publicID, err := sessionref.Parse(value)
	if err != nil {
		return SessionResumeSelector{}, fmt.Errorf("invalid resume selector %q (use a 7-char hex handle like a1b2c3d or latest)", value)
	}
	return SessionResumeSelector{publicID: publicID}, nil
}

// SessionIDResumeSelector constructs the exact-ID form used by the interactive
// picker after its database-backed list selection.
func SessionIDResumeSelector(id int64) (SessionResumeSelector, error) {
	if id <= 0 {
		return SessionResumeSelector{}, fmt.Errorf("invalid session id %d", id)
	}
	return SessionResumeSelector{sessionID: id}, nil
}

func (s SessionResumeSelector) valid() bool {
	if s.latest {
		return s.sessionID == 0 && s.publicID == ""
	}
	return (s.sessionID > 0 && s.publicID == "") || (s.sessionID == 0 && s.publicID != "")
}

func (s SessionResumeSelector) resolve(ctx context.Context, store *db.Store, workspaceID string) (int64, error) {
	if !s.valid() {
		return 0, fmt.Errorf("invalid session resume selector")
	}
	if s.latest {
		sessions, err := listPersistedSessions(ctx, store, workspaceID, 1)
		if err != nil {
			return 0, err
		}
		if len(sessions) == 0 {
			return 0, fmt.Errorf("no saved sessions in this workspace")
		}
		if sessions[0].ID <= 0 {
			return 0, fmt.Errorf("latest saved session has an invalid id")
		}
		return sessions[0].ID, nil
	}
	if s.publicID != "" {
		if store == nil {
			return 0, fmt.Errorf("session persistence is unavailable")
		}
		session, err := store.GetSessionByPublicID(ctx, s.publicID)
		if err != nil {
			return 0, fmt.Errorf("session %s: %w", s.publicID, err)
		}
		if workspaceID == "" || session.WorkspaceID != workspaceID {
			return 0, fmt.Errorf("session %s belongs to a different workspace", s.publicID)
		}
		return session.ID, nil
	}
	return s.sessionID, nil
}
