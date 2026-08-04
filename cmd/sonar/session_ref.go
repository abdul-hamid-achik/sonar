package main

import (
	"context"
	"fmt"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

type sessionRefStore interface {
	ResolveSessionRef(context.Context, string) (db.Session, error)
	SessionHandle(context.Context, int64) (string, error)
}

// resolveSessionArg turns a user-facing 7-char hex handle into the durable
// session row via the store. Bare integers and S-prefixed sequential ids are
// intentionally rejected by sessionref.Parse inside ResolveSessionRef.
func resolveSessionArg(ctx context.Context, store sessionRefStore, ref string) (db.Session, error) {
	if store == nil {
		return db.Session{}, fmt.Errorf("session store is unavailable")
	}
	return store.ResolveSessionRef(ctx, ref)
}

func sessionDisplayHandle(session db.Session) string {
	if handle := sessionref.Format(session.PublicID); handle != "" {
		return handle
	}
	return ""
}

func sessionHandleOrLookup(ctx context.Context, store sessionRefStore, sessionID int64, known string) string {
	if handle := sessionref.Format(known); handle != "" {
		return handle
	}
	if store == nil || sessionID <= 0 {
		return ""
	}
	handle, err := store.SessionHandle(ctx, sessionID)
	if err != nil {
		return ""
	}
	return handle
}
