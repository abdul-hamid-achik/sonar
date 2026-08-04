package db

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store wraps the sqlc Queries with the underlying database connection.
type Store struct {
	*Queries
	db                 *sql.DB
	dbPath             string
	executionLeaseRoot string
}

// Open creates or opens the SQLite database at the default location
// (~/.config/sonar/sonar.db) and runs migrations.
func Open() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}

	dir := filepath.Join(home, ".config", "sonar")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure config dir: %w", err)
	}

	return OpenPath(filepath.Join(dir, "sonar.db"))
}

// OpenPath creates or opens the SQLite database at the given path and runs migrations.
func OpenPath(path string) (*Store, error) {
	dbPath := path
	executionLeaseRoot := ""
	if path != ":memory:" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create db: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close db bootstrap file: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("secure db: %w", err)
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve db path: %w", err)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve db symlinks: %w", err)
		}
		dbPath = filepath.Clean(resolved)
		executionLeaseRoot = dbPath + ".leases"
	}

	conn, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Keep migration PRAGMAs and DDL on one connection. modernc SQLite does
	// not honor the mattn-style _busy_timeout DSN option on every version, so
	// set it explicitly before concurrent first-open migration attempts.
	conn.SetMaxOpenConns(1)
	if err := configureSQLiteConnection(conn, path); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("configure sqlite connection: %w", err)
	}

	// Run migrations.
	if err := runMigrations(conn); err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			return nil, fmt.Errorf("migrations: %w (close db: %v)", err, closeErr)
		}
		return nil, fmt.Errorf("migrations: %w", err)
	}
	if path != ":memory:" {
		for _, sqlitePath := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.Chmod(sqlitePath, 0o600); err != nil && !os.IsNotExist(err) {
				_ = conn.Close()
				return nil, fmt.Errorf("secure sqlite file %s: %w", filepath.Base(sqlitePath), err)
			}
		}
	}

	return &Store{
		Queries:            New(conn),
		db:                 conn,
		dbPath:             dbPath,
		executionLeaseRoot: executionLeaseRoot,
	}, nil
}

func configureSQLiteConnection(conn *sql.DB, path string) error {
	if _, err := conn.Exec(`PRAGMA busy_timeout = 5000; PRAGMA foreign_keys = ON;`); err != nil {
		return err
	}

	// WAL mode is persistent database state and two first-open processes can race
	// to set it. Retry only SQLITE_BUSY; all other errors fail closed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var journalMode string
		err := conn.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode)
		if err == nil {
			if path != ":memory:" && !strings.EqualFold(journalMode, "wal") {
				return fmt.Errorf("journal mode is %q, want WAL", journalMode)
			}
			break
		}
		if !sqliteBusy(err) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := conn.Exec(`PRAGMA synchronous = FULL;`); err != nil {
		return err
	}
	return nil
}

func sqliteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 5
}

func sqliteConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 19
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for direct access.
func (s *Store) DB() *sql.DB {
	return s.db
}

func runMigrations(conn *sql.DB) error {
	return runMigrationFS(conn, migrations, "migrations")
}

// runMigrationFS applies each embedded migration exactly once and records the
// source checksum in the database. Earlier releases replayed every file on
// every startup, which made all future migrations depend on perfect SQL-level
// idempotence and could not detect a rewritten migration. Existing databases
// without the ledger are upgraded by replaying the current idempotent baseline
// once, then recording it transactionally.
func runMigrationFS(conn *sql.DB, source fs.ReadDirFS, directory string) error {
	if conn == nil {
		return errors.New("migration database is nil")
	}
	if _, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			checksum   TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			CHECK (length(trim(name)) > 0 AND length(CAST(name AS BLOB)) <= 255),
			CHECK (length(checksum) = 64 AND checksum NOT GLOB '*[^0-9a-f]*')
		)
	`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	entries, err := source.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, err := fs.ReadFile(source, directory+"/"+entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		if err := applyMigration(conn, entry.Name(), data); err != nil {
			return err
		}
	}
	if err := ensureSessionWorkspaceColumn(conn); err != nil {
		return err
	}
	if err := ensureCheckpointWorkspaceColumn(conn); err != nil {
		return err
	}
	if err := ensureSessionStateRevisionColumn(conn); err != nil {
		return err
	}
	return ensureSessionPublicIDs(conn)
}

func applyMigration(conn *sql.DB, name string, data []byte) error {
	checksum := fmt.Sprintf("%x", sha256.Sum256(data))
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	var recorded string
	err = tx.QueryRow(`SELECT checksum FROM schema_migrations WHERE name = ?`, name).Scan(&recorded)
	switch {
	case err == nil:
		if recorded != checksum {
			return fmt.Errorf("migration %s checksum mismatch: database=%s source=%s", name, recorded, checksum)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit checked migration %s: %w", name, err)
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read migration %s ledger: %w", name, err)
	}

	if _, err := tx.Exec(string(data)); err != nil {
		return fmt.Errorf("exec migration %s: %w", name, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (name, checksum) VALUES (?, ?)`, name, checksum); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	return nil
}

// ensureSessionWorkspaceColumn upgrades databases created before workspace
// scoping. Migration files are intentionally idempotent CREATE statements, so
// this PRAGMA-gated ALTER is kept in Go for safe repeated startup.
func ensureSessionWorkspaceColumn(conn *sql.DB) error {
	found, err := sessionWorkspaceColumnExists(conn)
	if err != nil {
		return err
	}
	if !found {
		if _, err := conn.Exec(`ALTER TABLE sessions ADD COLUMN workspace_id TEXT NOT NULL DEFAULT ''`); err != nil {
			// Another sonar process may have won the first-open race after
			// our PRAGMA check. Re-inspect and accept only the desired end state.
			addedByPeer, inspectErr := sessionWorkspaceColumnExists(conn)
			if inspectErr != nil {
				return fmt.Errorf("add sessions workspace identity: %v (re-inspect: %w)", err, inspectErr)
			}
			if !addedByPeer {
				return fmt.Errorf("add sessions workspace identity: %w", err)
			}
		}
	}
	if _, err := conn.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_workspace_updated ON sessions(workspace_id, updated_at DESC)`); err != nil {
		return fmt.Errorf("index sessions workspace identity: %w", err)
	}
	return nil
}

func sessionWorkspaceColumnExists(conn *sql.DB) (bool, error) {
	rows, err := conn.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		return false, fmt.Errorf("inspect sessions schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("scan sessions schema: %w", err)
		}
		if name == "workspace_id" {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close sessions schema rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read sessions schema: %w", err)
	}
	return found, nil
}

func ensureCheckpointWorkspaceColumn(conn *sql.DB) error {
	found, err := tableColumnExists(conn, "checkpoints", "workspace_id")
	if err != nil {
		return err
	}
	if !found {
		if _, err := conn.Exec(`ALTER TABLE checkpoints ADD COLUMN workspace_id TEXT NOT NULL DEFAULT ''`); err != nil {
			addedByPeer, inspectErr := tableColumnExists(conn, "checkpoints", "workspace_id")
			if inspectErr != nil {
				return fmt.Errorf("add checkpoints workspace identity: %v (re-inspect: %w)", err, inspectErr)
			}
			if !addedByPeer {
				return fmt.Errorf("add checkpoints workspace identity: %w", err)
			}
		}
	}
	if _, err := conn.Exec(`CREATE INDEX IF NOT EXISTS idx_checkpoints_workspace_session ON checkpoints(workspace_id, session_id, id DESC)`); err != nil {
		return fmt.Errorf("index checkpoints workspace identity: %w", err)
	}
	return nil
}

// ensureSessionStateRevisionColumn adds the compare-and-swap generation used
// by compound reconciliation transactions. Legacy rows begin at revision zero;
// the next successful write advances them to one.
func ensureSessionStateRevisionColumn(conn *sql.DB) error {
	found, err := tableColumnExists(conn, "session_state", "revision")
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := conn.Exec(`ALTER TABLE session_state ADD COLUMN revision INTEGER NOT NULL DEFAULT 0`); err != nil {
		addedByPeer, inspectErr := tableColumnExists(conn, "session_state", "revision")
		if inspectErr != nil {
			return fmt.Errorf("add session state revision: %v (re-inspect: %w)", err, inspectErr)
		}
		if !addedByPeer {
			return fmt.Errorf("add session state revision: %w", err)
		}
	}
	return nil
}

func tableColumnExists(conn *sql.DB, table, column string) (bool, error) {
	rows, err := conn.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		return false, fmt.Errorf("inspect %s schema: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("scan %s schema: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close %s schema rows: %w", table, err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read %s schema: %w", table, err)
	}
	return found, nil
}
