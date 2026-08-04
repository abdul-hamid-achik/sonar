// Command assert-empty-recovery verifies that a CLI recovery inspection did
// not create durable recovery authority. It avoids a system sqlite3 dependency.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "assert empty recovery: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) (resultErr error) {
	if len(args) != 1 {
		return errors.New("usage: assert-empty-recovery DATABASE")
	}
	database, err := sql.Open("sqlite", "file:"+args[0]+"?mode=ro&_foreign_keys=ON")
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, database.Close())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	err = database.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM reconciliation_groups) +
			(SELECT COUNT(*) FROM reconciliation_group_resolutions) +
			(SELECT COUNT(*) FROM control_items) +
			(SELECT COUNT(*) FROM control_resolutions)
	`).Scan(&count)
	if err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("found %d durable recovery rows, want 0", count)
	}
	return nil
}
