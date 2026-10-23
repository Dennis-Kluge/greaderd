package fetcher

import (
	"context"
	"errors"
	"fmt"
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
	saved       []store.FetchResult
	notModified []time.Time // next fetch times
	failed      []failure
	moves       []string
	icons       []string
	prunedAt    []time.Time
	markErr     error
}

type failure struct {
	next time.Time
	err  error
}

func (s *fakeStore) DueFeeds(context.Context, time.Time) ([]store.Feed, error) { return nil, nil }

func (s *fakeStore) SaveFetch(_ context.Context, _ int64, r store.FetchResult) (int, error) {
	s.saved = append(s.saved, r)
	return len(r.Entries), nil
}

func (s *fakeStore) MarkNotModified(_ context.Context, _ int64, _, next time.Time) error {
	s.notModified = append(s.notModified, next)
	return s.markErr
}

func (s *fakeStore) MarkFailed(_ context.Context, _ int64, _, next time.Time, err error) error {
	s.failed = append(s.failed, failure{next: next, err: err})
	return s.markErr
}

func (s *fakeStore) MoveFeed(_ context.Context, _ int64, url string) (bool, error) {
	s.moves = append(s.moves, url)
	return true, nil
}

func (s *fakeStore) SetFeedIcon(_ context.Context, _ int64, iconURL string, _ time.Time) error {
	s.icons = append(s.icons, iconURL)
	return nil
}

func (s *fakeStore) PruneEntries(_ context.Context, cutoff time.Time) (int64, error) {
	s.prunedAt = append(s.prunedAt, cutoff)
	return 0, nil
}

// newTestFetcher allows private networks because httptest listens on loopback.
func newTestFetcher(s Store) *Fetcher {
	return New(s, Options{
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		AllowPrivateNetworks: true,
	})
}

// assertAbout fails unless got is within a few seconds of time.Now()+want.
func assertAbout(t *testing.T, what string, got time.Time, want time.Duration) {
	t.Helper()
	if d := time.Until(got); d < want-5*time.Second || d > want+time.Second {
		t.Errorf("%s in %v, want %v", what, d.Round(time.Second), want)
	}
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
	assertAbout(t, "next fetch", got.NextFetchAt, defaultInterval)

	feed.ETag = got.ETag
	if err := f.Refresh(context.Background(), feed); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if len(s.saved) != 1 || len(s.notModified) != 1 || len(s.failed) != 0 {
		t.Fatalf("not-modified feed: saved=%d notModified=%d failed=%d", len(s.saved), len(s.notModified), len(s.failed))
	}
	assertAbout(t, "next fetch after 304", s.notModified[0], defaultInterval)
	if len(s.moves) != 0 {
		t.Errorf("unexpected moves: %v", s.moves)
	}
}

func TestRefreshFailureBacksOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()

	tests := []struct {
		previousFailures int
		want             time.Duration
	}{
		{0, defaultInterval},
		{1, 2 * defaultInterval},
		{3, 8 * defaultInterval},
		{20, maxDelay},
	}
	for _, tt := range tests {
		s := &fakeStore{}
		feed := store.Feed{ID: 1, URL: srv.URL, ErrorCount: tt.previousFailures}
		if err := newTestFetcher(s).Refresh(context.Background(), feed); err == nil {
			t.Fatal("Refresh succeeded for a failing feed")
		}
		if len(s.failed) != 1 {
			t.Fatalf("failures recorded: %d", len(s.failed))
		}
		assertAbout(t, fmt.Sprintf("retry after %d failures", tt.previousFailures+1), s.failed[0].next, tt.want)
	}
}

func TestRefreshReportsStoreErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()

	markErr := errors.New("disk full")
	err := newTestFetcher(&fakeStore{markErr: markErr}).Refresh(context.Background(), store.Feed{ID: 1, URL: srv.URL})
	if !errors.Is(err, markErr) {
		t.Errorf("error %v does not include the MarkFailed failure", err)
	}
}

func TestRefreshHonoursServerHints(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    time.Duration
		failed  bool
	}{
		{
			name: "long max-age",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "public, max-age=7200")
				io.WriteString(w, testRSS)
			},
			want: 2 * time.Hour,
		},
		{
			name: "short max-age keeps the interval",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "max-age=60")
				io.WriteString(w, testRSS)
			},
			want: defaultInterval,
		},
		{
			name: "huge max-age is capped",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "max-age=99999999999")
				io.WriteString(w, testRSS)
			},
			want: maxDelay,
		},
		{
			name: "Retry-After in seconds",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "10800")
				http.Error(w, "slow down", http.StatusTooManyRequests)
			},
			want:   3 * time.Hour,
			failed: true,
		},
		{
			name: "Retry-After as a date",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", time.Now().Add(5*time.Hour).UTC().Format(http.TimeFormat))
				http.Error(w, "maintenance", http.StatusServiceUnavailable)
			},
			want:   5 * time.Hour,
			failed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			s := &fakeStore{}
			err := newTestFetcher(s).Refresh(context.Background(), store.Feed{ID: 1, URL: srv.URL})
			if tt.failed {
				if err == nil || len(s.failed) != 1 {
					t.Fatalf("Refresh = %v, failures %d", err, len(s.failed))
				}
				assertAbout(t, "retry", s.failed[0].next, tt.want)
				return
			}
			if err != nil || len(s.saved) != 1 {
				t.Fatalf("Refresh = %v, saved %d", err, len(s.saved))
			}
			assertAbout(t, "next fetch", s.saved[0].NextFetchAt, tt.want)
		})
	}
}

func TestRefreshFollowsPermanentRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, testRSS) })
	mux.Handle("/moved", http.RedirectHandler("/feed", http.StatusMovedPermanently))
	mux.Handle("/moved-308", http.RedirectHandler("/feed", http.StatusPermanentRedirect))
	mux.Handle("/temporary", http.RedirectHandler("/feed", http.StatusFound))
	mux.Handle("/moved-then-temporary", http.RedirectHandler("/temporary", http.StatusMovedPermanently))
	mux.Handle("/moved-twice", http.RedirectHandler("/moved", http.StatusMovedPermanently))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []struct {
		path string
		want string // new URL, or "" for no move
	}{
		{"/feed", ""},
		{"/moved", srv.URL + "/feed"},
		{"/moved-308", srv.URL + "/feed"},
		{"/moved-twice", srv.URL + "/feed"},
		{"/temporary", ""},
		{"/moved-then-temporary", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			s := &fakeStore{}
			if err := newTestFetcher(s).Refresh(context.Background(), store.Feed{ID: 1, URL: srv.URL + tt.path}); err != nil {
				t.Fatal(err)
			}
			var got string
			if len(s.moves) > 0 {
				got = s.moves[0]
			}
			if got != tt.want || len(s.saved) != 1 {
				t.Errorf("moved to %q (saved %d), want %q", got, len(s.saved), tt.want)
			}
		})
	}
}

func TestPrune(t *testing.T) {
	s := &fakeStore{}
	f := New(s, Options{Retention: 90 * 24 * time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	f.prune(context.Background())
	if len(s.prunedAt) != 1 {
		t.Fatalf("PruneEntries called %d times", len(s.prunedAt))
	}
	assertAbout(t, "cutoff", s.prunedAt[0], -90*24*time.Hour)
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
