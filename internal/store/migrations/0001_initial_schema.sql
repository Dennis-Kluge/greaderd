-- Initial schema.
--
-- The statements are idempotent because databases created before migrations
-- were versioned already contain these tables at user_version 0.

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

-- Every feed lives in a folder; see DefaultFolderID.
INSERT OR IGNORE INTO folders (id, name) VALUES (1, 'Uncategorized');
UPDATE feeds SET folder_id = 1 WHERE folder_id IS NULL;
