package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenPath(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func TestOpenAndMigrate(t *testing.T) {
	s := testStore(t)
	// Verify the store is functional by counting sessions.
	ctx := context.Background()
	count, err := s.CountSessions(ctx)
	if err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 sessions, got %d", count)
	}
}

func TestOpenPathUsesPrivateDatabasePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	store, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode = %04o, want 0600", got)
	}
}

func TestSessionCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create.
	sess, err := s.CreateSession(ctx, CreateSessionParams{
		Title: "Test Session",
		Model: "qwen3.5:4b",
		Mode:  "BUILD",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess.Title != "Test Session" {
		t.Fatalf("expected title 'Test Session', got %q", sess.Title)
	}

	// Read.
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Model != "qwen3.5:4b" {
		t.Fatalf("expected model 'qwen3.5:4b', got %q", got.Model)
	}

	// Create message.
	msg, err := s.CreateSessionMessage(ctx, CreateSessionMessageParams{
		SessionID: sess.ID,
		Role:      "user",
		Content:   "Hello world",
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if msg.Content != "Hello world" {
		t.Fatalf("expected content 'Hello world', got %q", msg.Content)
	}

	// List messages.
	msgs, err := s.GetSessionMessages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	// Delete.
	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	count, err := s.CountSessions(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 after delete, got %d", count)
	}
}

func TestToolPermissions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Upsert.
	perm, err := s.UpsertToolPermission(ctx, UpsertToolPermissionParams{
		ToolName: "bash",
		Policy:   "allow",
	})
	if err != nil {
		t.Fatalf("upsert permission: %v", err)
	}
	if perm.Policy != "allow" {
		t.Fatalf("expected policy 'allow', got %q", perm.Policy)
	}

	// Update via upsert.
	perm2, err := s.UpsertToolPermission(ctx, UpsertToolPermissionParams{
		ToolName: "bash",
		Policy:   "deny",
	})
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if perm2.Policy != "deny" {
		t.Fatalf("expected policy 'deny', got %q", perm2.Policy)
	}

	// List.
	perms, err := s.ListToolPermissions(ctx)
	if err != nil {
		t.Fatalf("list permissions: %v", err)
	}
	if len(perms) != 1 {
		t.Fatalf("expected 1 permission, got %d", len(perms))
	}

	// Reset.
	if err := s.ResetToolPermissions(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	perms, err = s.ListToolPermissions(ctx)
	if err != nil {
		t.Fatalf("list after reset: %v", err)
	}
	if len(perms) != 0 {
		t.Fatalf("expected 0 after reset, got %d", len(perms))
	}
}

func TestTokenStats(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, CreateSessionParams{
		Title: "Stats Test",
		Model: "qwen3.5:4b",
		Mode:  "ASK",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Record usage.
	_, err = s.RecordTokenUsage(ctx, RecordTokenUsageParams{
		SessionID:    sess.ID,
		Turn:         1,
		EvalCount:    100,
		PromptTokens: 500,
		Model:        "qwen3.5:4b",
	})
	if err != nil {
		t.Fatalf("record usage: %v", err)
	}

	_, err = s.RecordTokenUsage(ctx, RecordTokenUsageParams{
		SessionID:    sess.ID,
		Turn:         2,
		EvalCount:    200,
		PromptTokens: 600,
		Model:        "qwen3.5:4b",
	})
	if err != nil {
		t.Fatalf("record usage 2: %v", err)
	}

	// Get totals.
	totals, err := s.GetSessionTotalTokens(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get totals: %v", err)
	}
	if totals.TotalEval != 300 {
		t.Fatalf("expected total_eval 300, got %v", totals.TotalEval)
	}
	if totals.TotalPrompt != 1100 {
		t.Fatalf("expected total_prompt 1100, got %v", totals.TotalPrompt)
	}
	if totals.TurnCount != 2 {
		t.Fatalf("expected turn_count 2, got %v", totals.TurnCount)
	}
}

func TestFileChanges(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, CreateSessionParams{
		Title: "Changes Test",
		Model: "qwen3.5:4b",
		Mode:  "BUILD",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	_, err = s.RecordFileChange(ctx, RecordFileChangeParams{
		SessionID: sess.ID,
		FilePath:  "main.go",
		ToolName:  "write_file",
		Added:     10,
		Removed:   3,
	})
	if err != nil {
		t.Fatalf("record change: %v", err)
	}

	_, err = s.RecordFileChange(ctx, RecordFileChangeParams{
		SessionID: sess.ID,
		FilePath:  "main.go",
		ToolName:  "write_file",
		Added:     5,
		Removed:   2,
	})
	if err != nil {
		t.Fatalf("record change 2: %v", err)
	}

	summary, err := s.GetSessionFileChangeSummary(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	if len(summary) != 1 {
		t.Fatalf("expected 1 file, got %d", len(summary))
	}
	if summary[0].TotalAdded != 15 {
		t.Fatalf("expected 15 added, got %d", summary[0].TotalAdded)
	}
}

func TestDoubleOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	s1, err := OpenPath(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	t.Cleanup(func() {
		if err := s1.Close(); err != nil {
			t.Errorf("close first store: %v", err)
		}
	})

	// Running migrations again should be idempotent (IF NOT EXISTS).
	s2, err := OpenPath(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() {
		if err := s2.Close(); err != nil {
			t.Errorf("close second store: %v", err)
		}
	})
}

func TestMigrationLedgerRecordsExactEmbeddedChecksums(t *testing.T) {
	store := testStore(t)
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		want[entry.Name()] = fmt.Sprintf("%x", sha256.Sum256(data))
	}

	rows, err := store.db.Query(`SELECT name, checksum FROM schema_migrations ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	for rows.Next() {
		var name, checksum string
		if err := rows.Scan(&name, &checksum); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		got[name] = checksum
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("migration ledger count = %d, want %d: %#v", len(got), len(want), got)
	}
	for name, checksum := range want {
		if got[name] != checksum {
			t.Fatalf("migration %s checksum = %q, want %q", name, got[name], checksum)
		}
	}

	// A second run reads the ledger and leaves the exact recorded set intact.
	if err := runMigrations(store.db); err != nil {
		t.Fatalf("repeat migrations: %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(want) {
		t.Fatalf("repeat migration ledger count = %d, want %d", count, len(want))
	}
}

func TestMigrationLedgerRejectsRewrittenMigration(t *testing.T) {
	store := testStore(t)
	if _, err := store.db.Exec(`UPDATE schema_migrations SET checksum = ? WHERE name = ?`, strings.Repeat("0", 64), "001_init.sql"); err != nil {
		t.Fatal(err)
	}
	err := runMigrations(store.db)
	if err == nil || !strings.Contains(err.Error(), "001_init.sql checksum mismatch") {
		t.Fatalf("rewritten migration error = %v", err)
	}
}

func TestOpenPathRetiresLegacyBroadToolAllows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-permissions.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE tool_permissions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			tool_name TEXT NOT NULL UNIQUE,
			policy TEXT NOT NULL DEFAULT 'ask',
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
		INSERT INTO tool_permissions (tool_name, policy) VALUES
			('bash', 'allow'),
			('server__mutate', 'allow'),
			('remove', 'deny');
	`); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rows, err := store.db.Query(`SELECT tool_name, policy FROM tool_permissions ORDER BY tool_name`)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	for rows.Next() {
		var toolName, policy string
		if err := rows.Scan(&toolName, &policy); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		got[toolName] = policy
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"bash":           "ask",
		"server__mutate": "ask",
		"remove":         "deny",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated permissions = %#v, want %#v", got, want)
	}
	var applied int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = '008_retire_broad_tool_allows.sql'`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("retirement migration ledger rows = %d, want 1", applied)
	}
}

func TestFailedMigrationRollsBackSchemaAndLedgerTogether(t *testing.T) {
	store := testStore(t)
	err := applyMigration(store.db, "999_broken.sql", []byte(`
		CREATE TABLE migration_rollback_probe (id INTEGER PRIMARY KEY);
		INSERT INTO table_that_does_not_exist (id) VALUES (1);
	`))
	if err == nil || !strings.Contains(err.Error(), "exec migration 999_broken.sql") {
		t.Fatalf("broken migration error = %v", err)
	}

	var schemaCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'migration_rollback_probe'`).Scan(&schemaCount); err != nil {
		t.Fatal(err)
	}
	if schemaCount != 0 {
		t.Fatal("failed migration left its schema change committed")
	}
	var ledgerCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = '999_broken.sql'`).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 0 {
		t.Fatal("failed migration left a ledger receipt")
	}
}

func TestOpenPathMigratesLegacySessionWorkspaceIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT 'BUILD',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
		INSERT INTO sessions (title, model, mode) VALUES ('legacy', 'qwen', 'BUILD');
	`)
	if err != nil {
		_ = legacy.Close()
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	ctx := context.Background()
	for openNumber := 1; openNumber <= 3; openNumber++ {
		store, err := OpenPath(path)
		if err != nil {
			t.Fatalf("open migrated database #%d: %v", openNumber, err)
		}

		var workspaceColumns int
		err = store.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'workspace_id'`,
		).Scan(&workspaceColumns)
		if err != nil {
			_ = store.Close()
			t.Fatalf("inspect workspace column #%d: %v", openNumber, err)
		}
		if workspaceColumns != 1 {
			_ = store.Close()
			t.Fatalf("workspace column count after open #%d = %d, want 1", openNumber, workspaceColumns)
		}

		var workspaceIndexes int
		err = store.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_sessions_workspace_updated'`,
		).Scan(&workspaceIndexes)
		if err != nil {
			_ = store.Close()
			t.Fatalf("inspect workspace index #%d: %v", openNumber, err)
		}
		if workspaceIndexes != 1 {
			_ = store.Close()
			t.Fatalf("workspace index count after open #%d = %d, want 1", openNumber, workspaceIndexes)
		}

		legacySession, err := store.GetSession(ctx, 1)
		if err != nil {
			_ = store.Close()
			t.Fatalf("read legacy session #%d: %v", openNumber, err)
		}
		if legacySession.WorkspaceID != "" {
			_ = store.Close()
			t.Fatalf("legacy session workspace after open #%d = %q, want empty", openNumber, legacySession.WorkspaceID)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close migrated database #%d: %v", openNumber, err)
		}
	}
}

func TestOpenPathMigratesLegacyWorkspaceConcurrently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-concurrent.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL DEFAULT 'BUILD',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
	`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	const openers = 8
	start := make(chan struct{})
	errs := make(chan error, openers)
	var wg sync.WaitGroup
	for range openers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store, openErr := OpenPath(path)
			if openErr == nil {
				openErr = store.Close()
			}
			errs <- openErr
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent legacy open: %v", err)
		}
	}

	store, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var columns int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'workspace_id'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 1 {
		t.Fatalf("workspace columns = %d, want 1", columns)
	}
}

func TestOpenPathMigratesLegacyCheckpointWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-checkpoints.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE checkpoints (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id INTEGER NOT NULL DEFAULT 0,
			label TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT 'manual',
			messages TEXT NOT NULL DEFAULT '[]',
			msg_count INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
		INSERT INTO checkpoints (label) VALUES ('legacy');
	`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	for openNumber := 1; openNumber <= 2; openNumber++ {
		store, err := OpenPath(path)
		if err != nil {
			t.Fatalf("open #%d: %v", openNumber, err)
		}
		checkpoint, err := store.GetCheckpoint(context.Background(), 1)
		if err != nil {
			_ = store.Close()
			t.Fatalf("read legacy checkpoint #%d: %v", openNumber, err)
		}
		if checkpoint.WorkspaceID != "" {
			_ = store.Close()
			t.Fatalf("legacy workspace = %q", checkpoint.WorkspaceID)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListSessionsFiltersWorkspace(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	create := func(title, workspaceID string) int64 {
		t.Helper()
		session, err := store.CreateSession(ctx, CreateSessionParams{
			Title:       title,
			Model:       "qwen3.5:2b",
			Mode:        "BUILD",
			WorkspaceID: workspaceID,
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		return session.ID
	}

	a1 := create("a-one", "/workspace/a")
	a2 := create("a-two", "/workspace/a")
	b1 := create("b-one", "/workspace/b")

	sessions, err := store.ListSessions(ctx, ListSessionsParams{WorkspaceID: "/workspace/a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("workspace A sessions = %#v, want two", sessions)
	}
	wantIDs := map[int64]bool{a1: true, a2: true}
	for _, session := range sessions {
		if session.WorkspaceID != "/workspace/a" || !wantIDs[session.ID] || session.ID == b1 {
			t.Fatalf("cross-workspace session leaked into result: %#v", session)
		}
	}
}

func TestOpenDefault(t *testing.T) {
	// Ensure Open() doesn't panic. It writes to ~/.config/sonar/
	// which should be fine in CI/dev.
	if os.Getenv("CI") != "" {
		t.Skip("skip in CI to avoid side effects")
	}
	s, err := Open()
	if err != nil {
		t.Fatalf("open default: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close default store: %v", err)
	}
}
