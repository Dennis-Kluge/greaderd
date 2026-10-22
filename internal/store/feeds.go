package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Feed is a subscription to an RSS, Atom or JSON feed.
type Feed struct {
	ID int64
	// URL is the address of the feed document. It is unique per store.
	URL string
	// Title is set by the first successful fetch unless the user set one.
	Title   string
	SiteURL string
	// Folder is the name of the folder the feed belongs to.
	Folder string

	// Conditional GET state and the time of the last fetch attempt
	// (zero if the feed has never been fetched).
	ETag          string
	LastModified  string
	LastFetchedAt time.Time

	// Statistics, only set by Feeds.
	Unread        int
	NewestCrawled time.Time // zero if the feed has no entries
}

// FetchResult is the outcome of a successful fetch, written by SaveFetch.
type FetchResult struct {
	FetchedAt    time.Time
	ETag         string
	LastModified string
	Title        string
	SiteURL      string
	// Entries in the order they should receive IDs, usually oldest first.
	Entries []Entry
}

const feedColumns = `f.id, f.url, f.title, f.site_url, COALESCE(fo.name, ''),
	f.etag, f.last_modified, f.last_fetched_at`

const feedSource = ` FROM feeds f LEFT JOIN folders fo ON fo.id = f.folder_id`

func scanFeed(sc scanner, extra ...any) (Feed, error) {
	var f Feed
	var fetchedAt int64
	dest := append([]any{&f.ID, &f.URL, &f.Title, &f.SiteURL, &f.Folder, &f.ETag, &f.LastModified, &fetchedAt}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return Feed{}, err
	}
	f.LastFetchedAt = fromUnix(fetchedAt)
	return f, nil
}

// Feeds returns all feeds ordered by title, with unread counts and the crawl
// time of their newest entry.
func (s *Store) Feeds(ctx context.Context) ([]Feed, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+feedColumns+`,
		       (SELECT COUNT(*) FROM entries e WHERE e.feed_id = f.id AND e.is_read = 0),
		       (SELECT COALESCE(MAX(e.crawled_usec), 0) FROM entries e WHERE e.feed_id = f.id)`+
		feedSource+`
		ORDER BY f.title COLLATE NOCASE`)
	if err != nil {
		return nil, fmt.Errorf("list feeds: %w", err)
	}
	feeds, err := collect(rows, func(sc scanner) (Feed, error) {
		var newest int64
		var unread int
		f, err := scanFeed(sc, &unread, &newest)
		f.Unread, f.NewestCrawled = unread, fromUnixMicro(newest)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("list feeds: %w", err)
	}
	return feeds, nil
}

// FeedByID returns the feed with the given ID or ErrNotFound.
func (s *Store) FeedByID(ctx context.Context, id int64) (Feed, error) {
	f, err := s.feedWhere(ctx, `f.id = ?`, id)
	if err != nil {
		return Feed{}, fmt.Errorf("get feed %d: %w", id, err)
	}
	return f, nil
}

// FeedByURL returns the feed with the given feed URL or ErrNotFound.
func (s *Store) FeedByURL(ctx context.Context, url string) (Feed, error) {
	f, err := s.feedWhere(ctx, `f.url = ?`, url)
	if err != nil {
		return Feed{}, fmt.Errorf("get feed %q: %w", url, err)
	}
	return f, nil
}

func (s *Store) feedWhere(ctx context.Context, cond string, arg any) (Feed, error) {
	f, err := scanFeed(s.db.QueryRowContext(ctx, `SELECT `+feedColumns+feedSource+` WHERE `+cond, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Feed{}, ErrNotFound
	}
	return f, err
}

// AddFeed subscribes to the feed at url in the default folder. If the feed is
// already subscribed, it returns the existing feed and created is false.
func (s *Store) AddFeed(ctx context.Context, url string) (feed Feed, created bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO feeds (url, folder_id) VALUES (?, ?)`, url, DefaultFolderID)
	if err != nil {
		return Feed{}, false, fmt.Errorf("add feed %q: %w", url, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Feed{}, false, fmt.Errorf("add feed %q: %w", url, err)
	}
	feed, err = s.FeedByURL(ctx, url)
	return feed, n > 0, err
}

// DeleteFeed unsubscribes from a feed and deletes its entries.
func (s *Store) DeleteFeed(ctx context.Context, id int64) error {
	if err := execOne(ctx, s.db, `DELETE FROM feeds WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete feed %d: %w", id, err)
	}
	return nil
}

// SetFeedTitle sets a user-chosen title, which later fetches keep.
func (s *Store) SetFeedTitle(ctx context.Context, id int64, title string) error {
	if err := execOne(ctx, s.db, `UPDATE feeds SET title = ? WHERE id = ?`, title, id); err != nil {
		return fmt.Errorf("set title of feed %d: %w", id, err)
	}
	return nil
}

// SetFeedFolder moves a feed into the folder called folder, creating it if
// needed. An empty name moves the feed to the default folder.
func (s *Store) SetFeedFolder(ctx context.Context, id int64, folder string) error {
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		folderID := DefaultFolderID
		if folder != "" {
			var err error
			if folderID, err = ensureFolder(ctx, tx, folder); err != nil {
				return err
			}
		}
		return execOne(ctx, tx, `UPDATE feeds SET folder_id = ? WHERE id = ?`, folderID, id)
	})
	if err != nil {
		return fmt.Errorf("move feed %d to folder %q: %w", id, folder, err)
	}
	return nil
}

// DueFeeds returns the feeds last fetched before the given time, least
// recently fetched first.
func (s *Store) DueFeeds(ctx context.Context, before time.Time) ([]Feed, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+feedColumns+feedSource+`
		WHERE f.last_fetched_at < ? ORDER BY f.last_fetched_at`, toUnix(before))
	if err != nil {
		return nil, fmt.Errorf("list due feeds: %w", err)
	}
	feeds, err := collect(rows, func(sc scanner) (Feed, error) { return scanFeed(sc) })
	if err != nil {
		return nil, fmt.Errorf("list due feeds: %w", err)
	}
	return feeds, nil
}

// SaveFetch records a successful fetch and inserts its entries. Entries whose
// GUID the feed already has are left untouched, so read and starred states
// survive. It returns the number of new entries.
func (s *Store) SaveFetch(ctx context.Context, feedID int64, r FetchResult) (int, error) {
	inserted := 0
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		// Keep a user-set title; only fill it in when empty.
		err := execOne(ctx, tx, `
			UPDATE feeds SET etag = ?, last_modified = ?, last_fetched_at = ?, last_error = '',
			       title = CASE WHEN title = '' THEN ? ELSE title END,
			       site_url = CASE WHEN ? <> '' THEN ? ELSE site_url END
			WHERE id = ?`,
			r.ETag, r.LastModified, toUnix(r.FetchedAt), r.Title, r.SiteURL, r.SiteURL, feedID)
		if err != nil {
			return err
		}

		stmt, err := tx.PrepareContext(ctx, `
			INSERT OR IGNORE INTO entries (feed_id, guid, title, url, author, content, published, crawled_usec)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for _, e := range r.Entries {
			res, err := stmt.ExecContext(ctx, feedID, e.GUID, e.Title, e.URL, e.Author, e.Content,
				toUnix(e.Published), toUnixMicro(e.Crawled))
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			inserted += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("save fetch of feed %d: %w", feedID, err)
	}
	return inserted, nil
}

// MarkFetched records a fetch attempt that produced no new content, either
// because the feed was not modified (fetchErr is nil) or because it failed.
func (s *Store) MarkFetched(ctx context.Context, feedID int64, at time.Time, fetchErr error) error {
	msg := ""
	if fetchErr != nil {
		msg = fetchErr.Error()
	}
	err := execOne(ctx, s.db, `UPDATE feeds SET last_fetched_at = ?, last_error = ? WHERE id = ?`,
		toUnix(at), msg, feedID)
	if err != nil {
		return fmt.Errorf("mark feed %d fetched: %w", feedID, err)
	}
	return nil
}
