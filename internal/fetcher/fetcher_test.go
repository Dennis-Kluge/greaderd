package fetcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

const testRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel>
<title>Test Feed</title>
<item><guid>b</guid><title>Second</title><pubDate>Tue, 20 Oct 2026 12:00:00 GMT</pubDate></item>
<item><guid>a</guid><title>First</title><pubDate>Mon, 19 Oct 2026 12:00:00 GMT</pubDate></item>
</channel></rss>`

// fakeStore records what the Fetcher writes.
type fakeStore struct {
	saved   []store.FetchResult
	marked  []error
	markErr error
}

func (s *fakeStore) DueFeeds(context.Context, time.Time) ([]store.Feed, error) { return nil, nil }

func (s *fakeStore) SaveFetch(_ context.Context, _ int64, r store.FetchResult) (int, error) {
	s.saved = append(s.saved, r)
	return len(r.Entries), nil
}

func (s *fakeStore) MarkFetched(_ context.Context, _ int64, _ time.Time, fetchErr error) error {
	s.marked = append(s.marked, fetchErr)
	return s.markErr
}

// newTestFetcher allows private networks because httptest listens on loopback.
func newTestFetcher(s Store) *Fetcher {
	return New(s, Options{
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		AllowPrivateNetworks: true,
	})
}

func TestRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, testRSS)
	}))
	defer srv.Close()

	s := &fakeStore{}
	f := newTestFetcher(s)
	feed := store.Feed{ID: 1, URL: srv.URL + "/feed.xml"}

	if err := f.Refresh(context.Background(), feed); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	if len(s.saved) != 1 {
		t.Fatalf("saved %d results, want 1", len(s.saved))
	}
	got := s.saved[0]
	if got.Title != "Test Feed" || got.ETag != `"v1"` || got.SiteURL != srv.URL+"/" {
		t.Errorf("result = %+v", got)
	}
	if len(got.Entries) != 2 || got.Entries[0].GUID != "a" || got.Entries[1].GUID != "b" {
		t.Errorf("entries not oldest first: %+v", got.Entries)
	}

	feed.ETag = got.ETag
	if err := f.Refresh(context.Background(), feed); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if len(s.saved) != 1 || len(s.marked) != 1 || s.marked[0] != nil {
		t.Errorf("not-modified feed: saved=%d marked=%v", len(s.saved), s.marked)
	}
}

func TestRefreshFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()

	markErr := errors.New("disk full")
	s := &fakeStore{markErr: markErr}
	err := newTestFetcher(s).Refresh(context.Background(), store.Feed{ID: 1, URL: srv.URL})
	if err == nil {
		t.Fatal("Refresh succeeded for a failing feed")
	}
	if !errors.Is(err, markErr) {
		t.Errorf("error %v does not include the MarkFetched failure", err)
	}
	if len(s.marked) != 1 || s.marked[0] == nil {
		t.Errorf("failure not recorded: %v", s.marked)
	}
}

func TestDiscover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed.xml":
			io.WriteString(w, testRSS)
		case "/blog/":
			io.WriteString(w, `<html><head><link rel="alternate" type="application/rss+xml" href="../feed.xml"></head></html>`)
		default:
			io.WriteString(w, `<html><head><title>no feed</title></head></html>`)
		}
	}))
	defer srv.Close()

	f := newTestFetcher(&fakeStore{})
	tests := []struct {
		path    string
		want    string
		wantErr bool
	}{
		{path: "/feed.xml", want: srv.URL + "/feed.xml"},
		{path: "/blog/", want: srv.URL + "/feed.xml"},
		{path: "/plain", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, err := f.Discover(context.Background(), srv.URL+tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Discover = %q, want error", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("Discover = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestFindFeedLink(t *testing.T) {
	tests := []struct {
		name string
		page string
		want string
	}{
		{"rss", `<head><link rel="alternate" type="application/rss+xml" href="/rss"></head>`, "/rss"},
		{"atom self-closing", `<head><link rel="alternate" type="application/atom+xml" href="/atom"/></head>`, "/atom"},
		{"case and extra rel tokens", `<head><LINK REL="Alternate home" TYPE="Application/RSS+XML" HREF="/x"></head>`, "/x"},
		{"first of several", `<head><link rel="stylesheet" href="/s.css"><link rel="alternate" type="application/feed+json" href="/j"><link rel="alternate" type="application/rss+xml" href="/r"></head>`, "/j"},
		{"html alternate is not a feed", `<head><link rel="alternate" type="text/html" href="/de"></head>`, ""},
		{"links in body are ignored", `<head></head><body><link rel="alternate" type="application/rss+xml" href="/rss"></body>`, ""},
		{"no links", `<html></html>`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findFeedLink([]byte(tt.page)); got != tt.want {
				t.Errorf("findFeedLink = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEntryFromItem(t *testing.T) {
	crawled := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	published := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		item          gofeed.Item
		wantGUID      string
		wantPublished time.Time
		wantContent   string
	}{
		{
			name:          "guid and content",
			item:          gofeed.Item{GUID: "g", Link: "l", Content: "c", Description: "d", PublishedParsed: &published},
			wantGUID:      "g",
			wantPublished: published,
			wantContent:   "c",
		},
		{
			name:          "link as guid, description as content, crawl time as date",
			item:          gofeed.Item{Link: "l", Description: "d"},
			wantGUID:      "l",
			wantPublished: crawled,
			wantContent:   "d",
		},
		{
			name:          "hash as guid",
			item:          gofeed.Item{Title: "t", Content: "c", UpdatedParsed: &published},
			wantGUID:      "sha1:d18042ca9738f8009bd8a288ce99950e577e18be", // sha1("t\x00c")
			wantPublished: published,
			wantContent:   "c",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := entryFromItem(&tt.item, crawled)
			if e.GUID != tt.wantGUID || !e.Published.Equal(tt.wantPublished) || e.Content != tt.wantContent || !e.Crawled.Equal(crawled) {
				t.Errorf("entryFromItem = %+v", e)
			}
		})
	}
}
