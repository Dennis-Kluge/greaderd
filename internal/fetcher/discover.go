package fetcher

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/mmcdole/gofeed"
	"golang.org/x/net/html"
)

// feedTypes are the MIME types of <link rel="alternate"> elements that
// advertise a feed.
var feedTypes = map[string]bool{
	"application/rss+xml":   true,
	"application/atom+xml":  true,
	"application/feed+json": true,
	"application/json":      true,
}

// Discover returns rawURL if it points to a feed. Otherwise it treats rawURL
// as an HTML page and returns the first feed advertised in its head.
func (f *Fetcher) Discover(ctx context.Context, rawURL string) (string, error) {
	resp, err := f.get(ctx, rawURL, "", "")
	if err != nil {
		return "", fmt.Errorf("discover feed at %s: %w", rawURL, err)
	}
	if _, err := gofeed.NewParser().Parse(bytes.NewReader(resp.body)); err == nil {
		return rawURL, nil
	}
	base, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("discover feed at %s: %w", rawURL, err)
	}
	href := findFeedLink(resp.body)
	if href == "" {
		return "", fmt.Errorf("discover feed at %s: no feed found", rawURL)
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", fmt.Errorf("discover feed at %s: invalid feed link %q: %w", rawURL, href, err)
	}
	return base.ResolveReference(ref).String(), nil
}

// findFeedLink returns the href of the first feed link before <body>, or ""
// if there is none.
func findFeedLink(page []byte) string {
	z := html.NewTokenizer(bytes.NewReader(page))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "body":
				return ""
			case "link":
				if href := feedHref(z, hasAttr); href != "" {
					return href
				}
			}
		}
	}
}

// feedHref returns the href of the current <link> tag if it advertises a feed.
func feedHref(z *html.Tokenizer, hasAttr bool) string {
	var rel, typ, href string
	for hasAttr {
		var key, val []byte
		key, val, hasAttr = z.TagAttr()
		switch string(key) {
		case "rel":
			rel = string(val)
		case "type":
			typ = string(val)
		case "href":
			href = string(val)
		}
	}
	if !hasToken(rel, "alternate") || !feedTypes[strings.ToLower(strings.TrimSpace(typ))] {
		return ""
	}
	return strings.TrimSpace(href)
}

// hasToken reports whether the space-separated list contains token, ignoring case.
func hasToken(list, token string) bool {
	for _, t := range strings.Fields(list) {
		if strings.EqualFold(t, token) {
			return true
		}
	}
	return false
}
