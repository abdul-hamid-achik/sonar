package ui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/db"
)

func canonicalSessionTestWorkspace(t *testing.T) string {
	t.Helper()
	workspace, err := canonicalWorkspaceID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func TestParseSessionResumeSelector(t *testing.T) {
	for _, value := range []string{"", "S", "S0", "0", "-1", "+1", "01", "S01", " 1", "1 ", "1.5", "LATEST", "latest ", "42", "S42", "s42", "a1b2c3", "a1b2c3de", strings.Repeat("9", 40)} {
		t.Run("invalid_"+strings.ReplaceAll(value, " ", "_"), func(t *testing.T) {
			if _, err := ParseSessionResumeSelector(value); err == nil {
				t.Fatalf("ParseSessionResumeSelector(%q) succeeded", value)
			}
		})
	}

	exact, err := ParseSessionResumeSelector("a1b2c3d")
	if err != nil || !exact.valid() || exact.latest || exact.sessionID != 0 || exact.publicID != "a1b2c3d" {
		t.Fatalf("exact selector = %#v, error=%v", exact, err)
	}
	alias, err := ParseSessionResumeSelector("A1B2C3D")
	if err != nil || !alias.valid() || alias.latest || alias.publicID != "a1b2c3d" {
		t.Fatalf("alias selector = %#v, error=%v", alias, err)
	}
	latest, err := ParseSessionResumeSelector("latest")
	if err != nil || !latest.valid() || !latest.latest || latest.sessionID != 0 || latest.publicID != "" {
		t.Fatalf("latest selector = %#v, error=%v", latest, err)
	}
	if _, err := SessionIDResumeSelector(0); err == nil {
		t.Fatal("nonpositive picker session id succeeded")
	}
}

func TestLatestSessionResumeSelectorUsesCanonicalWorkspaceNewest(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "latest.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := canonicalSessionTestWorkspace(t)
	otherWorkspace := canonicalSessionTestWorkspace(t)

	older, err := store.CreateSession(context.Background(), db.CreateSessionParams{Title: "older", WorkspaceID: workspace})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := store.CreateSession(context.Background(), db.CreateSessionParams{Title: "newer", WorkspaceID: workspace})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := store.CreateSession(context.Background(), db.CreateSessionParams{Title: "foreign newest", WorkspaceID: otherWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	// SQLite timestamps have millisecond precision. Ties are common during fast
	// startup/test inserts, so the higher durable ID must win deterministically.
	if _, err := store.DB().ExecContext(context.Background(), `UPDATE sessions SET updated_at = ? WHERE id IN (?, ?)`, "2026-02-01T00:00:00.000Z", older.ID, newer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(context.Background(), `UPDATE sessions SET updated_at = ? WHERE id = ?`, "2026-03-01T00:00:00.000Z", foreign.ID); err != nil {
		t.Fatal(err)
	}

	selector, err := ParseSessionResumeSelector("latest")
	if err != nil {
		t.Fatal(err)
	}
	id, err := selector.resolve(context.Background(), store, workspace)
	if err != nil || id != newer.ID {
		t.Fatalf("latest resolved id=%d, error=%v; want %d", id, err, newer.ID)
	}
	listed, err := listPersistedSessions(context.Background(), store, workspace, 10)
	if err != nil || len(listed) != 2 || listed[0].ID != newer.ID || listed[1].ID != older.ID {
		t.Fatalf("deterministic picker order = %#v, error=%v", listed, err)
	}
	if listed[0].PublicID == "" || listed[1].PublicID == "" {
		t.Fatalf("list items missing public ids: %#v", listed)
	}
	if _, err := selector.resolve(context.Background(), store, canonicalSessionTestWorkspace(t)); err == nil || !strings.Contains(err.Error(), "no saved sessions") {
		t.Fatalf("empty workspace latest error = %v", err)
	}

	// Public-id selector resolves and enforces workspace.
	byPublic, err := ParseSessionResumeSelector(newer.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := byPublic.resolve(context.Background(), store, workspace)
	if err != nil || resolved != newer.ID {
		t.Fatalf("public resolve id=%d err=%v want %d", resolved, err, newer.ID)
	}
	if _, err := byPublic.resolve(context.Background(), store, otherWorkspace); err == nil {
		t.Fatal("foreign workspace public resolve succeeded")
	}
}
