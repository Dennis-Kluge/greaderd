package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func addFeed(t *testing.T, s *Store, url, folder string) Feed {
	t.Helper()
	f, _, err := s.AddFeed(t.Context(), url)
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if folder != "" {
		if err := s.SetFeedFolder(t.Context(), f.ID, folder); err != nil {
			t.Fatalf("SetFeedFolder: %v", err)
		}
	}
	f, err = s.FeedByID(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("FeedByID: %v", err)
	}
	return f
}

func folderNames(t *testing.T, s *Store) []string {
	t.Helper()
	folders, err := s.Folders(t.Context())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	var names []string
	for _, f := range folders {
		names = append(names, f.Name)
	}
	return names
}

func feedFolder(t *testing.T, s *Store, id int64) string {
	t.Helper()
	f, err := s.FeedByID(t.Context(), id)
	if err != nil {
		t.Fatalf("FeedByID: %v", err)
	}
	return f.Folder
}

func TestOpenCreatesDefaultFolder(t *testing.T) {
	s := openTestStore(t)
	folders, err := s.Folders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].ID != DefaultFolderID || folders[0].Name != "Uncategorized" {
		t.Errorf("folders = %+v", folders)
	}
	if f := addFeed(t, s, "https://a.example/feed", ""); f.Folder != "Uncategorized" {
		t.Errorf("new feed in folder %q", f.Folder)
	}
}

func TestOpenMigratesLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The unversioned prototype schema allowed feeds without a folder.
	_, err = db.Exec(`
		CREATE TABLE folders (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
		CREATE TABLE feeds (
			id INTEGER PRIMARY KEY, url TEXT NOT NULL UNIQUE, title TEXT NOT NULL DEFAULT '',
			site_url TEXT NOT NULL DEFAULT '', folder_id INTEGER REFERENCES folders(id) ON DELETE SET NULL,
			etag TEXT NOT NULL DEFAULT '', last_modified TEXT NOT NULL DEFAULT '',
			last_fetched_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '');
		INSERT INTO feeds (url) VALUES ('https://legacy.example/feed');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for range 2 { // migrating twice must be a no-op
		s, err := Open(t.Context(), path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		f, err := s.FeedByURL(t.Context(), "https://legacy.example/feed")
		if err != nil || f.Folder != "Uncategorized" {
			t.Errorf("legacy feed = %+v, %v", f, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAddFeedIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	first, created, err := s.AddFeed(t.Context(), "https://a.example/feed")
	if err != nil || !created {
		t.Fatalf("first AddFeed = %v, %v", created, err)
	}
	second, created, err := s.AddFeed(t.Context(), "https://a.example/feed")
	if err != nil || created || second.ID != first.ID {
		t.Errorf("second AddFeed = %+v, %v, %v", second, created, err)
	}
}

func TestFolderLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	a := addFeed(t, s, "https://a.example/feed", "Tech")
	b := addFeed(t, s, "https://b.example/feed", "")

	if err := s.RenameFolder(ctx, "Tech", "News"); err != nil {
		t.Fatal(err)
	}
	if got := feedFolder(t, s, a.ID); got != "News" {
		t.Errorf("after rename: feed in %q", got)
	}

	// Renaming the default folder keeps it the default.
	if err := s.RenameFolder(ctx, "Uncategorized", "Inbox"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFolder(ctx, "Inbox"); !errors.Is(err, ErrDefaultFolder) {
		t.Errorf("deleting renamed default folder: %v", err)
	}

	// Renaming the default folder onto another merges into the default.
	if err := s.RenameFolder(ctx, "Inbox", "News"); err != nil {
		t.Fatal(err)
	}
	if got, want := folderNames(t, s), []string{"News"}; !reflect.DeepEqual(got, want) {
		t.Errorf("folders after merge = %v, want %v", got, want)
	}
	if err := s.DeleteFolder(ctx, "News"); !errors.Is(err, ErrDefaultFolder) {
		t.Errorf("merged folder is not the default: %v", err)
	}

	// Deleting a folder moves its feeds to the default folder.
	if err := s.SetFeedFolder(ctx, b.ID, "Temp"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFolder(ctx, "Temp"); err != nil {
		t.Fatal(err)
	}
	if got := feedFolder(t, s, b.ID); got != "News" {
		t.Errorf("after delete: feed in %q", got)
	}
	if err := s.DeleteFolder(ctx, "does not exist"); err != nil {
		t.Errorf("deleting a missing folder: %v", err)
	}
}

func TestNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	tests := []struct {
		name string
		call func() error
	}{
		{"FeedByID", func() error { _, err := s.FeedByID(ctx, 99); return err }},
		{"FeedByURL", func() error { _, err := s.FeedByURL(ctx, "https://x.example/"); return err }},
		{"DeleteFeed", func() error { return s.DeleteFeed(ctx, 99) }},
		{"SetFeedTitle", func() error { return s.SetFeedTitle(ctx, 99, "t") }},
		{"SetFeedFolder", func() error { return s.SetFeedFolder(ctx, 99, "x") }},
		{"RenameFolder", func() error { return s.RenameFolder(ctx, "nope", "x") }},
		{"MarkNotModified", func() error { return s.MarkNotModified(ctx, 99, time.Now(), time.Now()) }},
		{"MarkFailed", func() error { return s.MarkFailed(ctx, 99, time.Now(), time.Now(), errors.New("x")) }},
		{"MoveFeed", func() error { _, err := s.MoveFeed(ctx, 99, "https://x.example/"); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, ErrNotFound) {
				t.Errorf("error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestSaveFetch(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	feed := addFeed(t, s, "https://a.example/feed", "")
	if err := s.SetFeedTitle(ctx, feed.ID, "My title"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	result := FetchResult{
		FetchedAt: now,
		ETag:      `"v1"`,
		Title:     "Feed title",
		SiteURL:   "https://a.example/",
		Entries: []Entry{
			{GUID: "a", Title: "A", Published: now, Crawled: now},
			{GUID: "b", Title: "B", Published: now, Crawled: now},
		},
	}
	if n, err := s.SaveFetch(ctx, feed.ID, result); err != nil || n != 2 {
		t.Fatalf("first SaveFetch = %d, %v", n, err)
	}
	if err := s.SetRead(ctx, []int64{1}, true); err != nil {
		t.Fatal(err)
	}

	result.Entries = append(result.Entries, Entry{GUID: "c", Title: "C", Published: now, Crawled: now})
	if n, err := s.SaveFetch(ctx, feed.ID, result); err != nil || n != 1 {
		t.Fatalf("second SaveFetch = %d, %v", n, err)
	}

	got, err := s.FeedByID(ctx, feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "My title" || got.ETag != `"v1"` || got.SiteURL != "https://a.example/" ||
		got.LastFetchedAt.Unix() != now.Unix() {
		t.Errorf("feed = %+v", got)
	}
	entries, err := s.Entries(ctx, []int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || !entries[2].Read || entries[2].GUID != "a" || entries[0].FeedTitle != "My title" {
		t.Errorf("entries = %+v", entries)
	}
}

// seedEntries creates feed A in folder Tech with entries 1–3 and feed B in the
// default folder with entries 4–5. Entry n is crawled n seconds after base.
// Entry 2 is read and entry 5 is starred.
func seedEntries(t *testing.T, s *Store, base time.Time) (a, b Feed) {
	t.Helper()
	ctx := t.Context()
	a = addFeed(t, s, "https://a.example/feed", "Tech")
	b = addFeed(t, s, "https://b.example/feed", "")
	entry := func(n int) Entry {
		at := base.Add(time.Duration(n) * time.Second)
		return Entry{GUID: string(rune('0' + n)), Published: at, Crawled: at}
	}
	for _, f := range []struct {
		id int64
		ns []int
	}{{a.ID, []int{1, 2, 3}}, {b.ID, []int{4, 5}}} {
		var entries []Entry
		for _, n := range f.ns {
			entries = append(entries, entry(n))
		}
		if _, err := s.SaveFetch(ctx, f.id, FetchResult{FetchedAt: base, Entries: entries}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetRead(ctx, []int64{2}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStarred(ctx, []int64{5}, true); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func ids(refs []ItemRef) []int64 {
	out := []int64{}
	for _, r := range refs {
		out = append(out, r.ID)
	}
	return out
}

func TestItemIDs(t *testing.T) {
	s := openTestStore(t)
	base := time.Unix(1_800_000_000, 0)
	a, _ := seedEntries(t, s, base)

	tests := []struct {
		name string
		q    Query
		want []int64
	}{
		{"all, newest first", Query{}, []int64{5, 4, 3, 2, 1}},
		{"oldest first", Query{OldestFirst: true}, []int64{1, 2, 3, 4, 5}},
		{"unread", Query{Read: Not}, []int64{5, 4, 3, 1}},
		{"read", Query{Read: Only}, []int64{2}},
		{"starred", Query{Starred: Only}, []int64{5}},
		{"one feed", Query{FeedID: a.ID}, []int64{3, 2, 1}},
		{"one folder", Query{Folder: "Tech"}, []int64{3, 2, 1}},
		{"unknown folder", Query{Folder: "None"}, []int64{}},
		{"crawled after", Query{CrawledAfter: base.Add(4 * time.Second)}, []int64{5, 4}},
		{"crawled before", Query{CrawledBefore: base.Add(3 * time.Second)}, []int64{2, 1}},
		{"continuation", Query{Continuation: 3}, []int64{2, 1}},
		{"continuation, oldest first", Query{Continuation: 3, OldestFirst: true}, []int64{4, 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.q.Limit = 10
			refs, continuation, err := s.ItemIDs(t.Context(), tt.q)
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(refs); !reflect.DeepEqual(got, tt.want) || continuation != 0 {
				t.Errorf("ItemIDs = %v (continuation %d), want %v", got, continuation, tt.want)
			}
		})
	}
}

func TestItemIDsPaging(t *testing.T) {
	s := openTestStore(t)
	base := time.Unix(1_800_000_000, 0)
	seedEntries(t, s, base)

	var pages [][]int64
	q := Query{Limit: 2}
	for {
		refs, continuation, err := s.ItemIDs(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, ids(refs))
		if continuation == 0 {
			break
		}
		q.Continuation = continuation
	}
	if want := [][]int64{{5, 4}, {3, 2}, {1}}; !reflect.DeepEqual(pages, want) {
		t.Errorf("pages = %v, want %v", pages, want)
	}

	if _, _, err := s.ItemIDs(t.Context(), Query{}); err == nil {
		t.Error("ItemIDs without a limit succeeded")
	}
}

func TestMarkAllRead(t *testing.T) {
	s := openTestStore(t)
	a, _ := seedEntries(t, s, time.Unix(1_800_000_000, 0))
	if err := s.MarkAllRead(t.Context(), Query{FeedID: a.ID, Limit: 1, Continuation: 99}); err != nil {
		t.Fatal(err)
	}
	refs, _, err := s.ItemIDs(t.Context(), Query{Read: Not, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(refs), []int64{5, 4}; !reflect.DeepEqual(got, want) {
		t.Errorf("unread after MarkAllRead = %v, want %v", got, want)
	}
}
