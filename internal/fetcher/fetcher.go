// Package fetcher downloads and parses feeds and stores their new entries.
package fetcher

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

const (
	userAgent    = "greaderd/0.1"
	maxFeedBytes = 10 << 20

	defaultInterval = 15 * time.Minute
	defaultWorkers  = 4
	defaultTimeout  = 30 * time.Second

	// pollInterval is how often Run looks for due feeds. It only bounds the
	// delay before a new or due feed is fetched; each feed is still fetched
	// at most once per Options.Interval.
	pollInterval = time.Minute
)

// Store is the persistence the Fetcher needs.
type Store interface {
	DueFeeds(ctx context.Context, before time.Time) ([]store.Feed, error)
	SaveFetch(ctx context.Context, feedID int64, r store.FetchResult) (int, error)
	MarkFetched(ctx context.Context, feedID int64, at time.Time, fetchErr error) error
}

// Options configure a Fetcher. Zero values select defaults.
type Options struct {
	// Interval is the minimum time between two fetches of a feed (default 15m).
	Interval time.Duration
	// Workers is the number of feeds fetched concurrently (default 4).
	Workers int
	// AllowPrivateNetworks permits feeds on loopback, private and other
	// non-public addresses. Leave it off when greaderd is reachable from the
	// internet; see ErrForbiddenAddress.
	AllowPrivateNetworks bool
	// Client performs the HTTP requests. The default has a 30s timeout and
	// enforces AllowPrivateNetworks; a custom client must do so itself.
	Client *http.Client
	// Logger receives refresh results (default: slog.Default()).
	Logger *slog.Logger
}

// Fetcher refreshes feeds and discovers feed URLs. It is safe for concurrent use.
type Fetcher struct {
	store    Store
	client   *http.Client
	interval time.Duration
	workers  int
	log      *slog.Logger
}

// New returns a Fetcher that stores results in s.
func New(s Store, opts Options) *Fetcher {
	f := &Fetcher{
		store:    s,
		client:   opts.Client,
		interval: opts.Interval,
		workers:  opts.Workers,
		log:      opts.Logger,
	}
	if f.client == nil {
		f.client = newClient(opts.AllowPrivateNetworks)
	}
	if f.interval <= 0 {
		f.interval = defaultInterval
	}
	if f.workers <= 0 {
		f.workers = defaultWorkers
	}
	if f.log == nil {
		f.log = slog.Default()
	}
	return f
}

// Run refreshes due feeds until ctx is cancelled. Failures are logged, not
// returned, so one broken feed does not stop the others.
func (f *Fetcher) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		f.refreshDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (f *Fetcher) refreshDue(ctx context.Context) {
	feeds, err := f.store.DueFeeds(ctx, time.Now().Add(-f.interval))
	if err != nil {
		if ctx.Err() == nil {
			f.log.Error("list due feeds", "err", err)
		}
		return
	}
	sem := make(chan struct{}, f.workers)
	var wg sync.WaitGroup
	for _, feed := range feeds {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := f.Refresh(ctx, feed); err != nil && ctx.Err() == nil {
				f.log.Warn("refresh feed", "feed", feed.ID, "url", feed.URL, "err", err)
			}
		})
	}
	wg.Wait()
}

// Refresh fetches one feed and stores its new entries. A failed fetch is
// recorded as well, so the feed waits a full interval before the next attempt.
func (f *Fetcher) Refresh(ctx context.Context, feed store.Feed) error {
	now := time.Now()
	result, err := f.fetch(ctx, feed, now)
	if err != nil {
		markErr := f.store.MarkFetched(ctx, feed.ID, now, err)
		return errors.Join(fmt.Errorf("refresh feed %d: %w", feed.ID, err), markErr)
	}
	if result == nil {
		return f.store.MarkFetched(ctx, feed.ID, now, nil)
	}
	n, err := f.store.SaveFetch(ctx, feed.ID, *result)
	if err != nil {
		return err
	}
	if n > 0 {
		f.log.Info("new entries", "feed", feed.ID, "count", n)
	}
	return nil
}

// fetch downloads and parses feed. It returns nil if the feed was not modified.
func (f *Fetcher) fetch(ctx context.Context, feed store.Feed, now time.Time) (*store.FetchResult, error) {
	resp, err := f.get(ctx, feed.URL, feed.ETag, feed.LastModified)
	if err != nil {
		return nil, err
	}
	if resp.notModified {
		return nil, nil
	}
	parsed, err := gofeed.NewParser().Parse(bytes.NewReader(resp.body))
	if err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}

	result := &store.FetchResult{
		FetchedAt:    now,
		ETag:         resp.etag,
		LastModified: resp.lastModified,
		Title:        parsed.Title,
		SiteURL:      parsed.Link,
	}
	// Clients derive favicons from the site URL; fall back to the feed's origin.
	if result.SiteURL == "" {
		if u, err := url.Parse(feed.URL); err == nil {
			result.SiteURL = u.Scheme + "://" + u.Host + "/"
		}
	}
	// Feeds list entries newest first; store them oldest first so that entry
	// IDs follow publication order.
	for i := len(parsed.Items) - 1; i >= 0; i-- {
		result.Entries = append(result.Entries, entryFromItem(parsed.Items[i], now))
	}
	return result, nil
}

func entryFromItem(it *gofeed.Item, crawled time.Time) store.Entry {
	published := crawled
	if it.PublishedParsed != nil {
		published = *it.PublishedParsed
	} else if it.UpdatedParsed != nil {
		published = *it.UpdatedParsed
	}
	content := it.Content
	if content == "" {
		content = it.Description
	}
	var author string
	if len(it.Authors) > 0 && it.Authors[0] != nil {
		author = it.Authors[0].Name
	}
	guid := it.GUID
	if guid == "" {
		guid = it.Link
	}
	if guid == "" {
		sum := sha1.Sum([]byte(it.Title + "\x00" + content))
		guid = "sha1:" + hex.EncodeToString(sum[:])
	}
	return store.Entry{
		GUID:      guid,
		Title:     it.Title,
		URL:       it.Link,
		Author:    author,
		Content:   content,
		Published: published,
		Crawled:   crawled,
	}
}

type response struct {
	body         []byte
	etag         string
	lastModified string
	notModified  bool
}

// get performs a conditional GET and reads at most maxFeedBytes of the body.
func (f *Fetcher) get(ctx context.Context, rawURL, etag, lastModified string) (response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return response{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return response{notModified: true}, nil
	case resp.StatusCode >= 300:
		return response{}, fmt.Errorf("GET %s: unexpected status %s", rawURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return response{}, fmt.Errorf("GET %s: read body: %w", rawURL, err)
	}
	return response{
		body:         body,
		etag:         resp.Header.Get("ETag"),
		lastModified: resp.Header.Get("Last-Modified"),
	}, nil
}
