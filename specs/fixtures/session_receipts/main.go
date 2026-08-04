// Command session-receipts seeds the deterministic SQLite state used by the
// saved-session Glyphrun contract without depending on a system sqlite3 CLI.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	_ "modernc.org/sqlite"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "seed session receipts: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) (resultErr error) {
	if len(args) != 2 {
		return errors.New("usage: session-receipts DATABASE PROJECT_ROOT")
	}
	databasePath, projectRoot := args[0], args[1]
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		return err
	}
	// Open through the application store so every migration (including
	// public_id) is applied and recorded in schema_migrations.
	store, err := db.OpenPath(databasePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}

	database, err := sql.Open("sqlite", databasePath+"?_foreign_keys=ON")
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, database.Close())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	contents, readErr := os.ReadFile("specs/fixtures/session_receipts.sql")
	if readErr != nil {
		return fmt.Errorf("read session_receipts.sql: %w", readErr)
	}
	statement := strings.ReplaceAll(string(contents), "__PROJECT_ROOT__", projectRoot)
	if _, execErr := database.ExecContext(ctx, statement); execErr != nil {
		return fmt.Errorf("apply session_receipts.sql: %w", execErr)
	}
	return nil
}
