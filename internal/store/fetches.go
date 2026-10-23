package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// FetchResult is the outcome of a fetch that returned content, written by
// SaveFetch.
type FetchResult struct {
	FetchedAt   time.Time
	NextFetchAt time.Time

	ETag         string
	LastModified string
	Title        string
	SiteURL      string
	// Entries in the order they should receive IDs, usually oldest first.
	Entries []Entry
}

// DueFeeds returns the feeds whose next fetch is due at now, longest
// overdue first.
func (s *Store) DueFeeds(ctx context.Context, now time.Time) ([]Feed, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+feedColumns+feedSource+`
		WHERE f.next_fetch_at <= ? ORDER BY f.next_fetch_at`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("list due feeds: %w", err)
	}
	feeds, err := collect(rows, func(sc scanner) (Feed, error) { return scanFeed(sc) })
	if err != nil {
		return nil, fmt.Errorf("list due feeds: %w", err)
	}
	return feeds, nil
}

// SaveFetch records a fetch that returned content and inserts its entries.
// Entries whose GUID the feed already has are left untouched, so read and
// starred states survive, but they are marked as seen for PruneEntries. It
// returns the number of new entries and resets the feed's error count.
func (s *Store) SaveFetch(ctx context.Context, feedID int64, r FetchResult) (int, error) {
	inserted := 0
	seen := toUnixMicro(r.FetchedAt)
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		// Keep a user-set title; only fill it in when empty.
		err := execOne(ctx, tx, `
			UPDATE feeds SET etag = ?, last_modified = ?,
			       last_fetched_at = ?, last_success_usec = ?, next_fetch_at = ?,
			       error_count = 0, last_error = '',
			       title = CASE WHEN title = '' THEN ? ELSE title END,
			       site_url = CASE WHEN ? <> '' THEN ? ELSE site_url END
			WHERE id = ?`,
			r.ETag, r.LastModified, toUnix(r.FetchedAt), seen, toUnix(r.NextFetchAt),
			r.Title, r.SiteURL, r.SiteURL, feedID)
		if err != nil {
			return err
		}

		insert, err := tx.PrepareContext(ctx, `
			INSERT OR IGNORE INTO entries
				(feed_id, guid, title, url, author, content, published, crawled_usec, last_seen_usec)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer insert.Close()
		touch, err := tx.PrepareContext(ctx, `UPDATE entries SET last_seen_usec = ? WHERE feed_id = ? AND guid = ?`)
		if err != nil {
			return err
		}
		defer touch.Close()

		for _, e := range r.Entries {
			res, err := insert.ExecContext(ctx, feedID, e.GUID, e.Title, e.URL, e.Author, e.Content,
				toUnix(e.Published), toUnixMicro(e.Crawled), seen)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n > 0 {
				inserted++
				continue
			}
			if _, err := touch.ExecContext(ctx, seen, feedID, e.GUID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("save fetch of feed %d: %w", feedID, err)
	}
	return inserted, nil
}

// MarkNotModified records a fetch answered with 304 Not Modified. The
// feed's content and its entries' last-seen times stay as they were.
func (s *Store) MarkNotModified(ctx context.Context, feedID int64, at, next time.Time) error {
	err := execOne(ctx, s.db, `
		UPDATE feeds SET last_fetched_at = ?, next_fetch_at = ?, error_count = 0, last_error = ''
		WHERE id = ?`,
		toUnix(at), toUnix(next), feedID)
	if err != nil {
		return fmt.Errorf("mark feed %d not modified: %w", feedID, err)
	}
	return nil
}

// MarkFailed records a failed fetch and increments the feed's error count.
func (s *Store) MarkFailed(ctx context.Context, feedID int64, at, next time.Time, fetchErr error) error {
	err := execOne(ctx, s.db, `
		UPDATE feeds SET last_fetched_at = ?, next_fetch_at = ?, error_count = error_count + 1, last_error = ?
		WHERE id = ?`,
		toUnix(at), toUnix(next), fetchErr.Error(), feedID)
	if err != nil {
		return fmt.Errorf("mark feed %d failed: %w", feedID, err)
	}
	return nil
}

// MoveFeed changes a feed's URL after a permanent redirect. It returns false
// and leaves the feed unchanged if another feed already has the new URL.
func (s *Store) MoveFeed(ctx context.Context, feedID int64, url string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE feeds SET url = ?
		WHERE id = ? AND NOT EXISTS (SELECT 1 FROM feeds WHERE url = ?)`,
		url, feedID, url)
	if err != nil {
		return false, fmt.Errorf("move feed %d to %q: %w", feedID, url, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("move feed %d to %q: %w", feedID, url, err)
	}
	if n > 0 {
		return true, nil
	}
	if _, err := s.FeedByID(ctx, feedID); err != nil {
		return false, fmt.Errorf("move feed %d to %q: %w", feedID, url, err)
	}
	return false, nil
}

// SetFeedIcon records the result of looking for a feed's icon. iconURL is
// stored as given, so callers pass the previous URL to keep it.
func (s *Store) SetFeedIcon(ctx context.Context, feedID int64, iconURL string, checkedAt time.Time) error {
	err := execOne(ctx, s.db, `UPDATE feeds SET icon_url = ?, icon_checked_at = ? WHERE id = ?`,
		iconURL, toUnix(checkedAt), feedID)
	if err != nil {
		return fmt.Errorf("set icon of feed %d: %w", feedID, err)
	}
	return nil
}

// PruneEntries deletes read, unstarred entries that their feed last listed
// before cutoff and that are missing from its latest content. Entries still
// in the feed are kept however old they are; otherwise the next fetch would
// add them again as unread. It returns the number of deleted entries.
func (s *Store) PruneEntries(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM entries
		WHERE is_read = 1 AND is_starred = 0 AND last_seen_usec < ?
		  AND last_seen_usec < (SELECT f.last_success_usec FROM feeds f WHERE f.id = entries.feed_id)`,
		cutoff.UnixMicro())
	if err != nil {
		return 0, fmt.Errorf("prune entries: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune entries: %w", err)
	}
	return n, nil
}
