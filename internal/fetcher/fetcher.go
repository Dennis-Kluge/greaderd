// Package fetcher downloads and parses feeds, stores their new entries and
// prunes old ones.
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
	// delay before a due feed is fetched; when a feed is due is decided per
	// feed (see schedule.go).
	pollInterval = time.Minute

	// pruneInterval is how often Run deletes entries past the retention period.
	pruneInterval = time.Hour
)

// Store is the persistence the Fetcher needs.
type Store interface {
	DueFeeds(ctx context.Context, now time.Time) ([]store.Feed, error)
	SaveFetch(ctx context.Context, feedID int64, r store.FetchResult) (int, error)
	MarkNotModified(ctx context.Context, feedID int64, at, next time.Time) error
	MarkFailed(ctx context.Context, feedID int64, at, next time.Time, fetchErr error) error
	MoveFeed(ctx context.Context, feedID int64, url string) (bool, error)
	SetFeedIcon(ctx context.Context, feedID int64, iconURL string, checkedAt time.Time) error
	PruneEntries(ctx context.Context, cutoff time.Time) (int64, error)
}

// Options configure a Fetcher. Zero values select defaults.
type Options struct {
	// Interval is the regular time between two fetches of a feed (default
	// 15m). Servers can ask for longer via Cache-Control and Retry-After, and
	// failing feeds back off exponentially, up to a day.
	Interval time.Duration
	// Retention is how long read, unstarred entries are kept after their
	// feed stopped listing them. Zero keeps all entries forever.
	Retention time.Duration
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
	store     Store
	client    *http.Client
	interval  time.Duration
	retention time.Duration
	workers   int
	log       *slog.Logger
}

// New returns a Fetcher that stores results in s.
func New(s Store, opts Options) *Fetcher {
	f := &Fetcher{
		store:     s,
		client:    opts.Client,
		interval:  opts.Interval,
		retention: opts.Retention,
		workers:   opts.Workers,
		log:       opts.Logger,
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

// Run refreshes due feeds and prunes old entries until ctx is cancelled.
// Failures are logged, not returned, so one broken feed does not stop the
// others.
func (f *Fetcher) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var lastPrune time.Time
	for {
		f.refreshDue(ctx)
		if f.retention > 0 && time.Since(lastPrune) >= pruneInterval {
			f.prune(ctx)
			lastPrune = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (f *Fetcher) refreshDue(ctx context.Context) {
	feeds, err := f.store.DueFeeds(ctx, time.Now())
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
				f.log.Warn("refresh feed", "feed", feed.ID, "url", feed.URL,
					"failures", feed.ErrorCount+1, "err", err)
			}
		})
	}
	wg.Wait()
}

// prune deletes entries past the retention period.
func (f *Fetcher) prune(ctx context.Context) {
	n, err := f.store.PruneEntries(ctx, time.Now().Add(-f.retention))
	if err != nil {
		if ctx.Err() == nil {
			f.log.Error("prune entries", "err", err)
		}
		return
	}
	if n > 0 {
		f.log.Info("pruned entries", "count", n)
	}
}

// Refresh fetches one feed, stores its new entries and schedules its next
// fetch. Failures are recorded too, so the feed backs off before the next
// attempt.
func (f *Fetcher) Refresh(ctx context.Context, feed store.Feed) error {
	now := time.Now()
	resp, err := f.get(ctx, feed.URL, feed.ETag, feed.LastModified)
	if err == nil && resp.movedTo != "" {
		f.move(ctx, feed, resp.movedTo)
	}
	var result *store.FetchResult
	if err == nil && !resp.notModified {
		result, err = parse(resp, now)
	}
	if err != nil {
		next := now.Add(failureDelay(f.interval, feed.ErrorCount+1, retryAfterOf(err)))
		markErr := f.store.MarkFailed(ctx, feed.ID, now, next, err)
		return errors.Join(fmt.Errorf("refresh feed %d: %w", feed.ID, err), markErr)
	}

	next := now.Add(successDelay(f.interval, resp.maxAge))
	if resp.notModified {
		if err := f.store.MarkNotModified(ctx, feed.ID, now, next); err != nil {
			return err
		}
		f.refreshIcon(ctx, feed, feed.SiteURL, now)
		return nil
	}
	result.NextFetchAt = next
	n, err := f.store.SaveFetch(ctx, feed.ID, *result)
	if err != nil {
		return err
	}
	if n > 0 {
		f.log.Info("new entries", "feed", feed.ID, "count", n)
	}
	f.refreshIcon(ctx, feed, result.SiteURL, now)
	return nil
}

// move follows a permanent redirect by updating the feed's URL. A failure is
// only logged: the fetch itself succeeded, and the redirect is seen again
// next time.
func (f *Fetcher) move(ctx context.Context, feed store.Feed, to string) {
	moved, err := f.store.MoveFeed(ctx, feed.ID, to)
	switch {
	case err != nil:
		f.log.Warn("move feed", "feed", feed.ID, "from", feed.URL, "to", to, "err", err)
	case moved:
		f.log.Info("feed moved permanently", "feed", feed.ID, "from", feed.URL, "to", to)
	default:
		f.log.Info("feed moved to a URL that is already subscribed; keeping both",
			"feed", feed.ID, "from", feed.URL, "to", to)
	}
}

// parse turns a response body into a FetchResult.
func parse(resp response, now time.Time) (*store.FetchResult, error) {
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
		if u, err := url.Parse(resp.url); err == nil {
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

// response is the part of an HTTP response the fetcher uses.
type response struct {
	url          string // after redirects
	body         []byte
	etag         string
	lastModified string
	notModified  bool
	// maxAge is the Cache-Control max-age, or 0.
	maxAge time.Duration
	// movedTo is the new URL if the feed redirected permanently, or "".
	movedTo string
}

// httpStatusError is an unexpected HTTP status, with the delay the server
// asked for in Retry-After, if any.
type httpStatusError struct {
	url        string
	status     string
	retryAfter time.Duration
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("GET %s: unexpected status %s", e.url, e.status)
}

// retryAfterOf returns the Retry-After delay carried by err, or 0.
func retryAfterOf(err error) time.Duration {
	var se *httpStatusError
	if errors.As(err, &se) {
		return se.retryAfter
	}
	return 0
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

	r := response{
		url:     resp.Request.URL.String(),
		maxAge:  cacheMaxAge(resp.Header),
		movedTo: permanentRedirect(resp),
	}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		r.notModified = true
		return r, nil
	case resp.StatusCode >= 300:
		return response{}, &httpStatusError{
			url:        rawURL,
			status:     resp.Status,
			retryAfter: retryAfter(resp.Header, time.Now()),
		}
	}
	if r.body, err = io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes)); err != nil {
		return response{}, fmt.Errorf("GET %s: read body: %w", rawURL, err)
	}
	r.etag = resp.Header.Get("ETag")
	r.lastModified = resp.Header.Get("Last-Modified")
	return r, nil
}

// permanentRedirect returns the final URL of resp if every redirect that led
// there was permanent (301 or 308), and "" otherwise. A temporary redirect
// anywhere in the chain means the original URL is still the one to use.
func permanentRedirect(resp *http.Response) string {
	final := resp.Request
	if final == nil || final.Response == nil {
		return ""
	}
	for req := final; req.Response != nil; req = req.Response.Request {
		switch req.Response.StatusCode {
		case http.StatusMovedPermanently, http.StatusPermanentRedirect:
		default:
			return ""
		}
	}
	return final.URL.String()
}
