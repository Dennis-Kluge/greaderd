package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Entry is an article of a feed.
type Entry struct {
	ID     int64
	FeedID int64
	// GUID identifies the entry within its feed across fetches.
	GUID    string
	Title   string
	URL     string
	Author  string
	Content string // HTML
	// Published is the date the feed gives for the entry.
	Published time.Time
	// Crawled is when greaderd first saw the entry.
	Crawled time.Time
	Read    bool
	Starred bool

	// Details of the entry's feed, only set by Entries.
	FeedTitle   string
	FeedSiteURL string
	Folder      string
}

// Flag filters entries by a boolean state such as read or starred.
type Flag int

const (
	// Any matches entries regardless of the state.
	Any Flag = iota
	// Only matches entries that have the state.
	Only
	// Not matches entries that do not have the state.
	Not
)

// Query selects entries. The zero value matches all entries, newest first.
type Query struct {
	// FeedID restricts the query to one feed; 0 means all feeds.
	FeedID int64
	// Folder restricts the query to the feeds in a folder; empty means all folders.
	Folder string

	// Read and Starred filter by the entry's read and starred state.
	Read    Flag
	Starred Flag

	// CrawledAfter (inclusive) and CrawledBefore (exclusive) bound the crawl
	// time; zero values leave the range open.
	CrawledAfter  time.Time
	CrawledBefore time.Time

	// OldestFirst reverses the default newest-first order.
	OldestFirst bool
	// Continuation is the ID returned by the previous call to ItemIDs.
	Continuation int64
	// Limit is the page size of ItemIDs and must be positive there.
	Limit int
}

// ItemRef identifies an entry in a stream.
type ItemRef struct {
	ID      int64
	Crawled time.Time
}

func (q Query) where() (string, []any) {
	var b whereBuilder
	if q.FeedID != 0 {
		b.add("e.feed_id = ?", q.FeedID)
	}
	if q.Folder != "" {
		b.add("f.folder_id = (SELECT id FROM folders WHERE name = ?)", q.Folder)
	}
	b.flag("e.is_read", q.Read)
	b.flag("e.is_starred", q.Starred)
	if !q.CrawledAfter.IsZero() {
		b.add("e.crawled_usec >= ?", q.CrawledAfter.UnixMicro())
	}
	if !q.CrawledBefore.IsZero() {
		b.add("e.crawled_usec < ?", q.CrawledBefore.UnixMicro())
	}
	if q.Continuation > 0 {
		if q.OldestFirst {
			b.add("e.id > ?", q.Continuation)
		} else {
			b.add("e.id < ?", q.Continuation)
		}
	}
	return b.String(), b.args
}

type whereBuilder struct {
	conds []string
	args  []any
}

func (b *whereBuilder) add(cond string, args ...any) {
	b.conds = append(b.conds, cond)
	b.args = append(b.args, args...)
}

func (b *whereBuilder) flag(column string, f Flag) {
	switch f {
	case Only:
		b.add(column + " = 1")
	case Not:
		b.add(column + " = 0")
	}
}

func (b *whereBuilder) String() string {
	if len(b.conds) == 0 {
		return "1 = 1"
	}
	return strings.Join(b.conds, " AND ")
}

// ItemIDs returns one page of entries matching q, ordered by ID. Entry IDs
// grow with crawl time, so this is also crawl order. If more entries match,
// continuation is the value to pass as Query.Continuation for the next page;
// otherwise it is 0.
func (s *Store) ItemIDs(ctx context.Context, q Query) (refs []ItemRef, continuation int64, err error) {
	if q.Limit <= 0 {
		return nil, 0, errors.New("item ids: limit must be positive")
	}
	where, args := q.where()
	order := "DESC"
	if q.OldestFirst {
		order = "ASC"
	}
	// Fetch one extra row to learn whether another page exists.
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.crawled_usec
		FROM entries e JOIN feeds f ON f.id = e.feed_id
		WHERE `+where+` ORDER BY e.id `+order+` LIMIT ?`,
		append(args, q.Limit+1)...)
	if err != nil {
		return nil, 0, fmt.Errorf("item ids: %w", err)
	}
	refs, err = collect(rows, func(sc scanner) (ItemRef, error) {
		var r ItemRef
		var crawled int64
		err := sc.Scan(&r.ID, &crawled)
		r.Crawled = fromUnixMicro(crawled)
		return r, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("item ids: %w", err)
	}
	if len(refs) > q.Limit {
		refs = refs[:q.Limit]
		continuation = refs[len(refs)-1].ID
	}
	return refs, continuation, nil
}

// Entries returns the entries with the given IDs, newest first. Unknown IDs
// are skipped.
func (s *Store) Entries(ctx context.Context, ids []int64) ([]Entry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders, args := inClause(ids)
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.feed_id, e.guid, e.title, e.url, e.author, e.content, e.published, e.crawled_usec,
		       e.is_read, e.is_starred, f.title, f.site_url, COALESCE(fo.name, '')
		FROM entries e
		JOIN feeds f ON f.id = e.feed_id
		LEFT JOIN folders fo ON fo.id = f.folder_id
		WHERE e.id IN (`+placeholders+`)
		ORDER BY e.id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("get entries: %w", err)
	}
	entries, err := collect(rows, func(sc scanner) (Entry, error) {
		var e Entry
		var published, crawled int64
		err := sc.Scan(&e.ID, &e.FeedID, &e.GUID, &e.Title, &e.URL, &e.Author, &e.Content, &published,
			&crawled, &e.Read, &e.Starred, &e.FeedTitle, &e.FeedSiteURL, &e.Folder)
		e.Published, e.Crawled = fromUnix(published), fromUnixMicro(crawled)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("get entries: %w", err)
	}
	return entries, nil
}

// SetRead marks the given entries as read or unread.
func (s *Store) SetRead(ctx context.Context, ids []int64, read bool) error {
	if err := s.setFlag(ctx, "is_read", ids, read); err != nil {
		return fmt.Errorf("set read: %w", err)
	}
	return nil
}

// SetStarred stars or unstars the given entries.
func (s *Store) SetStarred(ctx context.Context, ids []int64, starred bool) error {
	if err := s.setFlag(ctx, "is_starred", ids, starred); err != nil {
		return fmt.Errorf("set starred: %w", err)
	}
	return nil
}

// setFlag updates a boolean column; column is always a constant from this package.
func (s *Store) setFlag(ctx context.Context, column string, ids []int64, value bool) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders, args := inClause(ids)
	_, err := s.db.ExecContext(ctx, `UPDATE entries SET `+column+` = ? WHERE id IN (`+placeholders+`)`,
		append([]any{value}, args...)...)
	return err
}

// MarkAllRead marks every entry matching q as read. Ordering, Continuation
// and Limit are ignored.
func (s *Store) MarkAllRead(ctx context.Context, q Query) error {
	q.Continuation = 0
	where, args := q.where()
	_, err := s.db.ExecContext(ctx, `
		UPDATE entries SET is_read = 1 WHERE id IN (
			SELECT e.id FROM entries e JOIN feeds f ON f.id = e.feed_id WHERE `+where+`)`, args...)
	if err != nil {
		return fmt.Errorf("mark all read: %w", err)
	}
	return nil
}
