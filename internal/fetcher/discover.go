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
	href := findFeedLink(resp.body)
	if href == "" {
		return "", fmt.Errorf("discover feed at %s: no feed found", rawURL)
	}
	feedURL, err := resolve(resp.url, href)
	if err != nil {
		return "", fmt.Errorf("discover feed at %s: %w", rawURL, err)
	}
	return feedURL, nil
}

// resolve resolves href relative to the page at base.
func resolve(base, href string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", fmt.Errorf("invalid link %q: %w", href, err)
	}
	return b.ResolveReference(ref).String(), nil
}

// findFeedLink returns the href of the first feed link in the page's head,
// or "" if there is none.
func findFeedLink(page []byte) string {
	for _, l := range headLinks(page) {
		if hasToken(l.rel, "alternate") && feedTypes[strings.ToLower(l.typ)] {
			return l.href
		}
	}
	return ""
}

// linkTag is a <link> element with an href.
type linkTag struct {
	rel  string
	typ  string
	href string
}

// headLinks returns the <link> elements with an href before <body>.
func headLinks(page []byte) []linkTag {
	var links []linkTag
	z := html.NewTokenizer(bytes.NewReader(page))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return links
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "body":
				return links
			case "link":
				if l := readLink(z, hasAttr); l.href != "" {
					links = append(links, l)
				}
			}
		}
	}
}

// readLink reads the attributes of the current <link> tag.
func readLink(z *html.Tokenizer, hasAttr bool) linkTag {
	var l linkTag
	for hasAttr {
		var key, val []byte
		key, val, hasAttr = z.TagAttr()
		switch string(key) {
		case "rel":
			l.rel = string(val)
		case "type":
			l.typ = strings.TrimSpace(string(val))
		case "href":
			l.href = strings.TrimSpace(string(val))
		}
	}
	return l
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
