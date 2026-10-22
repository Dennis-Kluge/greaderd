package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"
)

const (
	userAgent    = "greaderd/0.1"
	maxFeedBytes = 10 << 20
)

type Fetcher struct {
	store    *Store
	client   *http.Client
	interval time.Duration
	workers  int
	log      *slog.Logger
}

func NewFetcher(store *Store, interval time.Duration, log *slog.Logger) *Fetcher {
	return &Fetcher{
		store:    store,
		client:   &http.Client{Timeout: 30 * time.Second},
		interval: interval,
		workers:  4,
		log:      log,
	}
}

// Run refreshes due feeds until ctx is cancelled.
func (f *Fetcher) Run(ctx context.Context) {
	f.refreshDue(ctx)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.refreshDue(ctx)
		}
	}
}

func (f *Fetcher) refreshDue(ctx context.Context) {
	feeds, err := f.store.DueFeeds(ctx, time.Now().Add(-f.interval).Unix())
	if err != nil {
		f.log.Error("list due feeds", "err", err)
		return
	}
	sem := make(chan struct{}, f.workers)
	var wg sync.WaitGroup
	for _, feed := range feeds {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := f.Refresh(ctx, feed); err != nil {
				f.log.Warn("refresh feed", "feed", feed.ID, "url", feed.URL, "err", err)
			}
		})
	}
	wg.Wait()
}

// Refresh fetches one feed and stores new entries.
func (f *Fetcher) Refresh(ctx context.Context, feed Feed) error {
	body, resp, err := f.get(ctx, feed.URL, feed.ETag, feed.LastModified)
	if err != nil {
		f.store.MarkFetched(ctx, feed.ID, err.Error())
		return err
	}
	if resp.StatusCode == http.StatusNotModified {
		return f.store.MarkFetched(ctx, feed.ID, "")
	}

	parsed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		f.store.MarkFetched(ctx, feed.ID, err.Error())
		return fmt.Errorf("parse: %w", err)
	}

	now := time.Now()
	result := FetchResult{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Title:        parsed.Title,
		SiteURL:      parsed.Link,
	}
	// Clients derive favicons from the site URL; fall back to the feed's origin.
	if result.SiteURL == "" {
		if u, err := url.Parse(feed.URL); err == nil {
			result.SiteURL = u.Scheme + "://" + u.Host + "/"
		}
	}
	// Feeds list newest first; insert oldest first so ids follow publication order.
	for i := len(parsed.Items) - 1; i >= 0; i-- {
		result.Entries = append(result.Entries, entryFromItem(parsed.Items[i], now))
	}

	n, err := f.store.SaveFetch(ctx, feed.ID, result)
	if err != nil {
		return err
	}
	if n > 0 {
		f.log.Info("new entries", "feed", feed.ID, "count", n)
	}
	return nil
}

func entryFromItem(it *gofeed.Item, now time.Time) Entry {
	published := now
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
	return Entry{
		GUID:        guid,
		Title:       it.Title,
		URL:         it.Link,
		Author:      author,
		Content:     content,
		Published:   published.Unix(),
		CrawledUsec: now.UnixMicro(),
	}
}

var alternateLink = regexp.MustCompile(`(?is)<link\b[^>]*>`)
var attr = regexp.MustCompile(`(?is)\b(rel|type|href)\s*=\s*["']([^"']*)["']`)

// Discover returns rawURL if it is a feed, otherwise the first feed advertised
// via <link rel="alternate"> in the HTML page.
func (f *Fetcher) Discover(ctx context.Context, rawURL string) (string, error) {
	body, _, err := f.get(ctx, rawURL, "", "")
	if err != nil {
		return "", err
	}
	if _, err := gofeed.NewParser().Parse(bytes.NewReader(body)); err == nil {
		return rawURL, nil
	}
	base, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	for _, tag := range alternateLink.FindAll(body, -1) {
		attrs := map[string]string{}
		for _, m := range attr.FindAllSubmatch(tag, -1) {
			attrs[string(bytes.ToLower(m[1]))] = string(m[2])
		}
		switch attrs["type"] {
		case "application/rss+xml", "application/atom+xml", "application/feed+json", "application/json":
		default:
			continue
		}
		if attrs["rel"] != "alternate" || attrs["href"] == "" {
			continue
		}
		ref, err := url.Parse(attrs["href"])
		if err != nil {
			continue
		}
		return base.ResolveReference(ref).String(), nil
	}
	return "", fmt.Errorf("no feed found at %s", rawURL)
}

func (f *Fetcher) get(ctx context.Context, rawURL, etag, lastModified string) ([]byte, *http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, resp, nil
	}
	if resp.StatusCode >= 300 {
		return nil, resp, fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	return body, resp, err
}
