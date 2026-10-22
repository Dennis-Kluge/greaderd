package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
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
	last_fetched_at INTEGER NOT NULL DEFAULT 0,
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

-- Like FreshRSS, every feed lives in a folder; NetNewsWire's FreshRSS
-- account refuses feeds at the root.
INSERT OR IGNORE INTO folders (id, name) VALUES (1, 'Uncategorized');
UPDATE feeds SET folder_id = 1 WHERE folder_id IS NULL;
`

const defaultFolderID = 1

var (
	errNotFound          = errors.New("not found")
	errDefaultFolderLock = errors.New("the default folder cannot be deleted")
)

type Store struct {
	db *sql.DB
}

type Folder struct {
	ID     int64
	Name   string
	Unread int
}

type Feed struct {
	ID            int64
	URL           string
	Title         string
	SiteURL       string
	Folder        string // empty when the feed has no folder
	ETag          string
	LastModified  string
	LastFetchedAt int64
	Unread        int
	NewestUsec    int64
}

type Entry struct {
	ID          int64
	FeedID      int64
	GUID        string
	Title       string
	URL         string
	Author      string
	Content     string
	Published   int64
	CrawledUsec int64
	Read        bool
	Starred     bool

	// Joined from feeds/folders when reading.
	FeedTitle   string
	FeedSiteURL string
	Folder      string
}

// StreamQuery selects entries for the stream endpoints.
type StreamQuery struct {
	FeedID         int64 // 0 = any feed, -1 = match nothing
	Folder         string
	OnlyRead       bool
	OnlyStarred    bool
	ExcludeRead    bool
	ExcludeStarred bool
	SinceUsec      int64 // crawled_usec >= SinceUsec
	UntilUsec      int64 // crawled_usec < UntilUsec
	Oldest         bool  // oldest first instead of newest first
	Continuation   int64 // id of the last item of the previous page
	Limit          int
}

func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection keeps SQLite free of SQLITE_BUSY; plenty for a single user.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- folders ---

func (s *Store) Folders(ctx context.Context) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT fo.id, fo.name,
		       (SELECT COUNT(*) FROM entries e JOIN feeds f ON f.id = e.feed_id
		        WHERE f.folder_id = fo.id AND e.is_read = 0)
		FROM folders fo ORDER BY fo.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.Name, &f.Unread); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) EnsureFolder(ctx context.Context, name string) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO folders (name) VALUES (?)`, name); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM folders WHERE name = ?`, name).Scan(&id)
	return id, err
}

// RenameFolder renames a folder; if the destination already exists the two are merged.
func (s *Store) RenameFolder(ctx context.Context, from, to string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var fromID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM folders WHERE name = ?`, from).Scan(&fromID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	var toID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM folders WHERE name = ?`, to).Scan(&toID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `UPDATE folders SET name = ? WHERE id = ?`, to, fromID); err != nil {
			return err
		}
	case err != nil:
		return err
	case fromID == defaultFolderID:
		// Merge into the default folder so it survives.
		if _, err := tx.ExecContext(ctx, `UPDATE feeds SET folder_id = ? WHERE folder_id = ?`, fromID, toID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE id = ?`, toID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE folders SET name = ? WHERE id = ?`, to, fromID); err != nil {
			return err
		}
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE feeds SET folder_id = ? WHERE folder_id = ?`, toID, fromID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE id = ?`, fromID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteFolder removes a folder; its feeds move to the default folder.
func (s *Store) DeleteFolder(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM folders WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if id == defaultFolderID {
		return errDefaultFolderLock
	}
	if _, err := tx.ExecContext(ctx, `UPDATE feeds SET folder_id = ? WHERE folder_id = ?`, defaultFolderID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// --- feeds ---

const feedColumns = `
	f.id, f.url, f.title, f.site_url, COALESCE(fo.name, ''), f.etag, f.last_modified, f.last_fetched_at`

func scanFeed(sc interface{ Scan(...any) error }, extra ...any) (Feed, error) {
	var f Feed
	dest := append([]any{&f.ID, &f.URL, &f.Title, &f.SiteURL, &f.Folder, &f.ETag, &f.LastModified, &f.LastFetchedAt}, extra...)
	return f, sc.Scan(dest...)
}

func (s *Store) Feeds(ctx context.Context) ([]Feed, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+feedColumns+`,
		       (SELECT COUNT(*) FROM entries e WHERE e.feed_id = f.id AND e.is_read = 0),
		       (SELECT COALESCE(MAX(crawled_usec), 0) FROM entries e WHERE e.feed_id = f.id)
		FROM feeds f LEFT JOIN folders fo ON fo.id = f.folder_id
		ORDER BY f.title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feed
	for rows.Next() {
		var unread int
		var newest int64
		f, err := scanFeed(rows, &unread, &newest)
		if err != nil {
			return nil, err
		}
		f.Unread, f.NewestUsec = unread, newest
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) FeedByID(ctx context.Context, id int64) (Feed, error) {
	return s.feedWhere(ctx, `f.id = ?`, id)
}

func (s *Store) FeedByURL(ctx context.Context, url string) (Feed, error) {
	return s.feedWhere(ctx, `f.url = ?`, url)
}

func (s *Store) feedWhere(ctx context.Context, cond string, arg any) (Feed, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+feedColumns+`
		FROM feeds f LEFT JOIN folders fo ON fo.id = f.folder_id WHERE `+cond, arg)
	f, err := scanFeed(row)
	if errors.Is(err, sql.ErrNoRows) {
		return f, errNotFound
	}
	return f, err
}

// AddFeed subscribes to url. It returns the existing feed when already subscribed.
func (s *Store) AddFeed(ctx context.Context, url string) (Feed, bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO feeds (url, folder_id) VALUES (?, ?)`, url, defaultFolderID)
	if err != nil {
		return Feed{}, false, err
	}
	n, _ := res.RowsAffected()
	f, err := s.FeedByURL(ctx, url)
	return f, n > 0, err
}

func (s *Store) DeleteFeed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM feeds WHERE id = ?`, id)
	return err
}

func (s *Store) SetFeedTitle(ctx context.Context, id int64, title string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE feeds SET title = ? WHERE id = ?`, title, id)
	return err
}

// SetFeedFolder moves a feed into folder; an empty name moves it to the default folder.
func (s *Store) SetFeedFolder(ctx context.Context, id int64, folder string) error {
	if folder == "" {
		_, err := s.db.ExecContext(ctx, `UPDATE feeds SET folder_id = ? WHERE id = ?`, defaultFolderID, id)
		return err
	}
	folderID, err := s.EnsureFolder(ctx, folder)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE feeds SET folder_id = ? WHERE id = ?`, folderID, id)
	return err
}

// DueFeeds returns feeds not fetched since the given unix time.
func (s *Store) DueFeeds(ctx context.Context, before int64) ([]Feed, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+feedColumns+`
		FROM feeds f LEFT JOIN folders fo ON fo.id = f.folder_id
		WHERE f.last_fetched_at < ? ORDER BY f.last_fetched_at`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feed
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FetchResult is what a successful feed refresh writes back.
type FetchResult struct {
	ETag         string
	LastModified string
	Title        string
	SiteURL      string
	Entries      []Entry
}

// SaveFetch stores a refresh and inserts new entries; existing GUIDs are left untouched.
func (s *Store) SaveFetch(ctx context.Context, feedID int64, r FetchResult) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Keep a user-set title; only fill it in when empty.
	_, err = tx.ExecContext(ctx, `
		UPDATE feeds SET etag = ?, last_modified = ?, last_fetched_at = ?, last_error = '',
		       title = CASE WHEN title = '' THEN ? ELSE title END,
		       site_url = CASE WHEN ? <> '' THEN ? ELSE site_url END
		WHERE id = ?`,
		r.ETag, r.LastModified, time.Now().Unix(), r.Title, r.SiteURL, r.SiteURL, feedID)
	if err != nil {
		return 0, err
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO entries (feed_id, guid, title, url, author, content, published, crawled_usec)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	inserted := 0
	for _, e := range r.Entries {
		res, err := stmt.ExecContext(ctx, feedID, e.GUID, e.Title, e.URL, e.Author, e.Content, e.Published, e.CrawledUsec)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	return inserted, tx.Commit()
}

// MarkFetched records a refresh without new content (304 or error).
func (s *Store) MarkFetched(ctx context.Context, feedID int64, fetchErr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE feeds SET last_fetched_at = ?, last_error = ? WHERE id = ?`,
		time.Now().Unix(), fetchErr, feedID)
	return err
}

// --- entries ---

func (q StreamQuery) where() (string, []any) {
	conds := []string{"1 = 1"}
	var args []any
	add := func(cond string, a ...any) {
		conds = append(conds, cond)
		args = append(args, a...)
	}
	if q.FeedID != 0 {
		add("e.feed_id = ?", q.FeedID)
	}
	if q.Folder != "" {
		add("f.folder_id = (SELECT id FROM folders WHERE name = ?)", q.Folder)
	}
	if q.OnlyRead {
		add("e.is_read = 1")
	}
	if q.OnlyStarred {
		add("e.is_starred = 1")
	}
	if q.ExcludeRead {
		add("e.is_read = 0")
	}
	if q.ExcludeStarred {
		add("e.is_starred = 0")
	}
	if q.SinceUsec > 0 {
		add("e.crawled_usec >= ?", q.SinceUsec)
	}
	if q.UntilUsec > 0 {
		add("e.crawled_usec < ?", q.UntilUsec)
	}
	if q.Continuation > 0 {
		if q.Oldest {
			add("e.id > ?", q.Continuation)
		} else {
			add("e.id < ?", q.Continuation)
		}
	}
	return strings.Join(conds, " AND "), args
}

// ItemIDs returns matching entry ids and crawl times, plus a continuation id (0 = no more pages).
func (s *Store) ItemIDs(ctx context.Context, q StreamQuery) ([]Entry, int64, error) {
	where, args := q.where()
	order := "DESC"
	if q.Oldest {
		order = "ASC"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.feed_id, e.crawled_usec
		FROM entries e JOIN feeds f ON f.id = e.feed_id
		WHERE `+where+` ORDER BY e.id `+order+` LIMIT ?`,
		append(args, q.Limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.FeedID, &e.CrawledUsec); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var continuation int64
	if len(out) > q.Limit {
		out = out[:q.Limit]
		continuation = out[len(out)-1].ID
	}
	return out, continuation, nil
}

func (s *Store) EntriesByIDs(ctx context.Context, ids []int64) ([]Entry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.feed_id, e.guid, e.title, e.url, e.author, e.content, e.published, e.crawled_usec,
		       e.is_read, e.is_starred, f.title, f.site_url, COALESCE(fo.name, '')
		FROM entries e JOIN feeds f ON f.id = e.feed_id LEFT JOIN folders fo ON fo.id = f.folder_id
		WHERE e.id IN (`+placeholders+`) ORDER BY e.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.FeedID, &e.GUID, &e.Title, &e.URL, &e.Author, &e.Content, &e.Published,
			&e.CrawledUsec, &e.Read, &e.Starred, &e.FeedTitle, &e.FeedSiteURL, &e.Folder); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetFlag sets is_read or is_starred on the given entries.
func (s *Store) SetFlag(ctx context.Context, ids []int64, column string, value bool) error {
	if len(ids) == 0 {
		return nil
	}
	if column != "is_read" && column != "is_starred" {
		return fmt.Errorf("unknown flag %q", column)
	}
	placeholders, args := inClause(ids)
	_, err := s.db.ExecContext(ctx, `UPDATE entries SET `+column+` = ? WHERE id IN (`+placeholders+`)`,
		append([]any{value}, args...)...)
	return err
}

// MarkAllRead marks every entry of the stream as read.
func (s *Store) MarkAllRead(ctx context.Context, q StreamQuery) error {
	where, args := q.where()
	_, err := s.db.ExecContext(ctx, `
		UPDATE entries SET is_read = 1 WHERE id IN (
			SELECT e.id FROM entries e JOIN feeds f ON f.id = e.feed_id WHERE `+where+`)`, args...)
	return err
}

func inClause(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}
