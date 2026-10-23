package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

func dueIDs(t *testing.T, s *Store, now time.Time) []int64 {
	t.Helper()
	feeds, err := s.DueFeeds(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, f := range feeds {
		ids = append(ids, f.ID)
	}
	return ids
}

func TestScheduling(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	now := time.Unix(1_800_000_000, 0)
	a := addFeed(t, s, "https://a.example/feed", "")
	b := addFeed(t, s, "https://b.example/feed", "")

	if got := dueIDs(t, s, now); !reflect.DeepEqual(got, []int64{a.ID, b.ID}) {
		t.Fatalf("new feeds due = %v, want both", got)
	}

	if _, err := s.SaveFetch(ctx, a.ID, FetchResult{FetchedAt: now, NextFetchAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(ctx, b.ID, now, now.Add(30*time.Minute), errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(ctx, b.ID, now, now.Add(30*time.Minute), errors.New("boom again")); err != nil {
		t.Fatal(err)
	}
	if got := dueIDs(t, s, now); len(got) != 0 {
		t.Errorf("due right after fetching = %v", got)
	}
	if got := dueIDs(t, s, now.Add(30*time.Minute)); !reflect.DeepEqual(got, []int64{b.ID}) {
		t.Errorf("due after 30m = %v, want [%d]", got, b.ID)
	}

	failed, err := s.FeedByID(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.ErrorCount != 2 || failed.LastError != "boom again" || !failed.NextFetchAt.Equal(now.Add(30*time.Minute)) {
		t.Errorf("failed feed = %+v", failed)
	}

	// Success, whether with content or not modified, resets the error count.
	if err := s.MarkNotModified(ctx, b.ID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.FeedByID(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ErrorCount != 0 || recovered.LastError != "" {
		t.Errorf("recovered feed = %+v", recovered)
	}
}

func TestMoveFeed(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	a := addFeed(t, s, "https://a.example/old", "")
	addFeed(t, s, "https://b.example/feed", "")

	moved, err := s.MoveFeed(ctx, a.ID, "https://a.example/new")
	if err != nil || !moved {
		t.Fatalf("MoveFeed = %v, %v", moved, err)
	}
	if got, err := s.FeedByID(ctx, a.ID); err != nil || got.URL != "https://a.example/new" {
		t.Errorf("feed after move = %+v, %v", got, err)
	}

	// The URL is taken by another subscription: keep both as they are.
	moved, err = s.MoveFeed(ctx, a.ID, "https://b.example/feed")
	if err != nil || moved {
		t.Errorf("MoveFeed onto another feed = %v, %v", moved, err)
	}
}

func TestPruneEntries(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	feed := addFeed(t, s, "https://a.example/feed", "")
	t0 := time.Unix(1_800_000_000, 0)
	entry := func(guid string, at time.Time) Entry {
		return Entry{GUID: guid, Published: at, Crawled: at}
	}

	// First fetch lists a–e.
	first := FetchResult{FetchedAt: t0, Entries: []Entry{
		entry("a", t0), entry("b", t0), entry("c", t0), entry("d", t0), entry("e", t0),
	}}
	if _, err := s.SaveFetch(ctx, feed.ID, first); err != nil {
		t.Fatal(err)
	}
	// a, b, c and e are read; c is also starred; d stays unread.
	if err := s.SetRead(ctx, []int64{1, 2, 3, 5}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStarred(ctx, []int64{3}, true); err != nil {
		t.Fatal(err)
	}

	// A month later the feed only lists b and e.
	t1 := t0.Add(30 * 24 * time.Hour)
	second := FetchResult{FetchedAt: t1, Entries: []Entry{entry("b", t0), entry("e", t0)}}
	if _, err := s.SaveFetch(ctx, feed.ID, second); err != nil {
		t.Fatal(err)
	}
	// A not-modified fetch keeps the content of the second fetch current.
	if err := s.MarkNotModified(ctx, feed.ID, t1.Add(time.Hour), t1.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Nothing was last seen before t0, so nothing goes.
	if n, err := s.PruneEntries(ctx, t0); err != nil || n != 0 {
		t.Fatalf("PruneEntries(t0) = %d, %v", n, err)
	}
	// Even far in the future, only a goes: read, unstarred and gone from the feed.
	n, err := s.PruneEntries(ctx, t1.Add(365*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("PruneEntries = %d, %v; want 1", n, err)
	}
	refs, _, err := s.ItemIDs(ctx, Query{Limit: 10, OldestFirst: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(refs), []int64{2, 3, 4, 5}; !slices.Equal(got, want) {
		t.Errorf("remaining entries = %v, want %v", got, want)
	}

	// The next fetch must not bring the pruned entry back if the feed no
	// longer lists it, and existing entries keep their read state.
	if added, err := s.SaveFetch(ctx, feed.ID, second); err != nil || added != 0 {
		t.Errorf("refetch added %d entries, %v", added, err)
	}
}

func TestPruneEntriesWaitsForFirstFetch(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	feed := addFeed(t, s, "https://a.example/feed", "")
	t0 := time.Unix(1_800_000_000, 0)
	if _, err := s.SaveFetch(ctx, feed.ID, FetchResult{FetchedAt: t0, Entries: []Entry{
		{GUID: "a", Published: t0, Crawled: t0},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRead(ctx, []int64{1}, true); err != nil {
		t.Fatal(err)
	}
	// The feed has only been fetched once; the entry is in its latest content.
	if n, err := s.PruneEntries(ctx, t0.Add(time.Hour)); err != nil || n != 0 {
		t.Errorf("PruneEntries = %d, %v; want 0", n, err)
	}
}

func TestMigration2KeepsData(t *testing.T) {
	ms, err := loadMigrations(migrationFiles, migrationDir)
	if err != nil {
		t.Fatal(err)
	}
	db := openRawDB(t)
	ctx := t.Context()
	if err := migrate(ctx, db, ms[:1]); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO feeds (id, url, folder_id, last_fetched_at) VALUES (1, 'https://a.example/feed', 1, 1800000000);
		INSERT INTO entries (feed_id, guid, published, crawled_usec, is_read)
		VALUES (1, 'a', 1800000000, 1800000000000000, 1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, ms[:2]); err != nil {
		t.Fatal(err)
	}

	var nextFetch, errorCount, lastSuccess, lastSeen int64
	err = db.QueryRowContext(ctx, `
		SELECT f.next_fetch_at, f.error_count, f.last_success_usec, e.last_seen_usec
		FROM feeds f JOIN entries e ON e.feed_id = f.id`).Scan(&nextFetch, &errorCount, &lastSuccess, &lastSeen)
	if err != nil {
		t.Fatal(err)
	}
	// Existing feeds are due at once; entries count as seen when crawled, and
	// nothing can be pruned before the feed has been fetched again.
	if nextFetch != 0 || errorCount != 0 || lastSuccess != 0 || lastSeen != 1800000000000000 {
		t.Errorf("after migration: next=%d errors=%d success=%d seen=%d", nextFetch, errorCount, lastSuccess, lastSeen)
	}
}
