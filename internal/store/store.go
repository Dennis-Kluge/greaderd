// Package store persists folders, feeds and entries in SQLite.
//
// It is the only package that contains SQL. Every feed belongs to exactly one
// folder: the folder with DefaultFolderID always exists, because NetNewsWire's
// FreshRSS account type does not allow feeds outside of folders.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

var (
	// ErrNotFound is returned when a folder or feed does not exist.
	ErrNotFound = errors.New("store: not found")

	// ErrDefaultFolder is returned when trying to delete the default folder.
	ErrDefaultFolder = errors.New("store: the default folder cannot be deleted")
)

// DefaultFolderID identifies the folder that holds feeds without an explicit
// folder. It is created by the first migration and cannot be deleted.
const DefaultFolderID int64 = 1

// Store persists folders, feeds and entries in a SQLite database.
// It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens the database at path, creating it if necessary, and applies
// pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// A single connection serialises writers and avoids SQLITE_BUSY; a single
	// user never needs more.
	db.SetMaxOpenConns(1)
	migrations, err := loadMigrations(migrationFiles, migrationDir)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if err := migrate(ctx, db, migrations); err != nil {
		return nil, errors.Join(fmt.Errorf("migrate database: %w", err), db.Close())
	}
	return &Store{db: db}, nil
}

// dsn returns the data source name for the database file at path. The pragmas
// apply to every new connection.
func dsn(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
}

// Ping checks that the database can be reached.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// querier is implemented by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scanner is implemented by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// txBeginner is implemented by *sql.DB and *sql.Conn.
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// withTx runs fn in a transaction and commits it if fn succeeds.
func withTx(ctx context.Context, db txBeginner, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		// fn's error is the one worth reporting; a failed rollback leaves
		// nothing committed either way.
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// collect scans every row with scan.
func collect[T any](rows *sql.Rows, scan func(scanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// execOne runs a statement that must affect at least one row and returns
// ErrNotFound if it affects none.
func execOne(ctx context.Context, q querier, query string, args ...any) error {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// inClause returns placeholders and arguments for "IN (...)".
func inClause(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}
