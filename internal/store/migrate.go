package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
)

// migrationFiles holds the schema migrations; see AGENTS.md for how to add one.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationDir = "migrations"

// migrationName matches "NNNN_description.sql"; NNNN is the schema version
// the migration produces.
var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// migration is one schema change.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the migrations in dir. Versions must start at 1 and
// have no gaps, so the order in which they run is unambiguous.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir) // sorted by name
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	migrations := make([]migration, 0, len(entries))
	for _, e := range entries {
		match := migrationName.FindStringSubmatch(e.Name())
		if match == nil || e.IsDir() {
			return nil, fmt.Errorf("migration %q: name must match NNNN_description.sql", e.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", e.Name(), err)
		}
		if want := len(migrations) + 1; version != want {
			return nil, fmt.Errorf("migration %q: version %d, want %d", e.Name(), version, want)
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", e.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: e.Name(), sql: string(body)})
	}
	return migrations, nil
}

// migrate applies the migrations newer than the database's schema version,
// which PRAGMA user_version stores.
//
// Each migration runs in its own transaction, so a failing one leaves the
// database at the previous version. Foreign keys are disabled meanwhile, as
// SQLite's procedure for rebuilding tables requires, and checked before each
// commit instead.
func migrate(ctx context.Context, db *sql.DB, migrations []migration) (err error) {
	// PRAGMAs apply per connection, so pin one.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get connection: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()

	var current int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this build supports (%d); upgrade greaderd",
			current, len(migrations))
	}
	if current == len(migrations) {
		return nil
	}

	// foreign_keys cannot change inside a transaction.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	defer func() {
		if _, enableErr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`); enableErr != nil {
			err = errors.Join(err, fmt.Errorf("enable foreign keys: %w", enableErr))
		}
	}()

	for _, m := range migrations[current:] {
		if err := withTx(ctx, conn, func(tx *sql.Tx) error { return applyMigration(ctx, tx, m) }); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, tx *sql.Tx, m migration) error {
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return err
	}
	// PRAGMA does not accept placeholders.
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, m.version))
	return err
}

// checkForeignKeys fails if any row references a missing parent.
func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var fkID int
		if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return err
		}
		return fmt.Errorf("foreign key violation: row %d of %s references a missing row of %s",
			rowID.Int64, table, parent)
	}
	return rows.Err()
}
