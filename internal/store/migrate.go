package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order; PRAGMA user_version records how many have
// run. The first migration is idempotent because databases created before
// versioning already contain its tables at user_version 0.
var migrations = []string{
	`
	CREATE TABLE IF NOT EXISTS folders (
		id   INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE
	);

	CREATE TABLE IF NOT EXISTS feeds (
		id              INTEGER PRIMARY KEY,
		url             TEXT NOT NULL UNIQUE,
		title           TEXT NOT NULL DEFAULT '',
		site_url        TEXT NOT NULL DEFAULT '',
		folder_id       INTEGER REFERENCES folders(id) ON DELETE SET NULL,
		etag            TEXT NOT NULL DEFAULT '',
		last_modified   TEXT NOT NULL DEFAULT '',
		last_fetched_at INTEGER NOT NULL DEFAULT 0, -- unix seconds
		last_error      TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS entries (
		id           INTEGER PRIMARY KEY,
		feed_id      INTEGER NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
		guid         TEXT NOT NULL,
		title        TEXT NOT NULL DEFAULT '',
		url          TEXT NOT NULL DEFAULT '',
		author       TEXT NOT NULL DEFAULT '',
		content      TEXT NOT NULL DEFAULT '',
		published    INTEGER NOT NULL, -- unix seconds
		crawled_usec INTEGER NOT NULL, -- unix microseconds
		is_read      INTEGER NOT NULL DEFAULT 0,
		is_starred   INTEGER NOT NULL DEFAULT 0,
		UNIQUE (feed_id, guid)
	);

	CREATE INDEX IF NOT EXISTS entries_unread  ON entries (is_read, id);
	CREATE INDEX IF NOT EXISTS entries_starred ON entries (is_starred, id);
	CREATE INDEX IF NOT EXISTS entries_crawled ON entries (crawled_usec);

	INSERT OR IGNORE INTO folders (id, name) VALUES (1, 'Uncategorized');
	UPDATE feeds SET folder_id = 1 WHERE folder_id IS NULL;
	`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		err := withTx(ctx, db, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			// PRAGMA does not accept placeholders.
			_, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
	}
	return nil
}
