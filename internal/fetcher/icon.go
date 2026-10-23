package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

const (
	// iconRecheck is how often a feed's icon is looked for again, so sites
	// that change their icon are picked up eventually.
	iconRecheck = 7 * 24 * time.Hour

	// iconTimeout bounds looking for an icon, which happens during refreshes
	// and must not hold up subscribing to a feed for long.
	iconTimeout = 10 * time.Second
)

// errNoIcon is returned when a site declares no icon and has no /favicon.ico.
var errNoIcon = errors.New("no icon found")

// refreshIcon looks for the icon of the feed's site if it has not been
// looked for recently. Failures keep the previous icon and are only logged
// at debug level: many sites have no icon, and the feed itself is fine.
func (f *Fetcher) refreshIcon(ctx context.Context, feed store.Feed, siteURL string, now time.Time) {
	if siteURL == "" || now.Sub(feed.IconCheckedAt) < iconRecheck {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, iconTimeout)
	defer cancel()

	icon, err := f.discoverIcon(ctx, siteURL)
	if err != nil {
		f.log.Debug("discover icon", "feed", feed.ID, "site", siteURL, "err", err)
		icon = feed.IconURL
	}
	if err := f.store.SetFeedIcon(ctx, feed.ID, icon, now); err != nil && ctx.Err() == nil {
		f.log.Warn("store icon", "feed", feed.ID, "err", err)
	}
}

// discoverIcon returns the URL of the best icon declared in the head of the
// page at siteURL, or the site's /favicon.ico if it serves an image there.
func (f *Fetcher) discoverIcon(ctx context.Context, siteURL string) (string, error) {
	if resp, err := f.get(ctx, siteURL, "", ""); err == nil {
		if href := findIconLink(resp.body); href != "" {
			return resolve(resp.url, href)
		}
	}
	favicon, err := resolve(siteURL, "/favicon.ico")
	if err != nil {
		return "", err
	}
	if err := f.checkImage(ctx, favicon); err != nil {
		return "", fmt.Errorf("%w: %v", errNoIcon, err)
	}
	return favicon, nil
}

// iconRank orders the rel values of icon links; higher is better. Apple
// touch icons are usually larger than favicons and look better in lists.
var iconRank = map[string]int{
	"apple-touch-icon":             3,
	"apple-touch-icon-precomposed": 3,
	"icon":                         2,
}

// findIconLink returns the href of the best icon link in the page's head, or
// "" if there is none. Among links of the same rank, the first wins.
func findIconLink(page []byte) string {
	best, bestRank := "", 0
	for _, l := range headLinks(page) {
		rank := 0
		for _, t := range strings.Fields(strings.ToLower(l.rel)) {
			rank = max(rank, iconRank[t])
		}
		if rank > bestRank {
			best, bestRank = l.href, rank
		}
	}
	return best
}

// checkImage verifies that rawURL serves an image.
func (f *Fetcher) checkImage(ctx context.Context, rawURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	// Some servers send .ico files as application/octet-stream.
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "application/octet-stream") {
		return fmt.Errorf("GET %s: content type %q is not an image", rawURL, ct)
	}
	return nil
}
