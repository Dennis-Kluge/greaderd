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

	// NextFetchAt is when the feed is due; zero means now.
	NextFetchAt time.Time
	// ErrorCount is the number of consecutive failed fetches, and LastError
	// the message of the latest one.
	ErrorCount int
	LastError  string

	// Statistics, only set by Feeds.
	Unread        int
	NewestCrawled time.Time // zero if the feed has no entries
}

const feedColumns = `f.id, f.url, f.title, f.site_url, COALESCE(fo.name, ''),
	f.etag, f.last_modified, f.last_fetched_at, f.next_fetch_at, f.error_count, f.last_error`

const feedSource = ` FROM feeds f LEFT JOIN folders fo ON fo.id = f.folder_id`

func scanFeed(sc scanner, extra ...any) (Feed, error) {
	var f Feed
	var fetchedAt, nextFetchAt int64
	dest := append([]any{
		&f.ID, &f.URL, &f.Title, &f.SiteURL, &f.Folder,
		&f.ETag, &f.LastModified, &fetchedAt, &nextFetchAt, &f.ErrorCount, &f.LastError,
	}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return Feed{}, err
	}
	f.LastFetchedAt = fromUnix(fetchedAt)
	f.NextFetchAt = fromUnix(nextFetchAt)
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
