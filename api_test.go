package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel>
<title>Test Feed</title><link>https://example.com/</link>
<item><guid>c</guid><title>Third</title><link>https://example.com/3</link><pubDate>Wed, 21 Oct 2026 12:00:00 GMT</pubDate><description>&lt;p&gt;three&lt;/p&gt;</description></item>
<item><guid>b</guid><title>Second</title><link>https://example.com/2</link><pubDate>Tue, 20 Oct 2026 12:00:00 GMT</pubDate><description>two</description></item>
<item><guid>a</guid><title>First</title><link>https://example.com/1</link><pubDate>Mon, 19 Oct 2026 12:00:00 GMT</pubDate><description>one</description></item>
</channel></rss>`

const testHTML = `<html><head><link rel="alternate" type="application/rss+xml" href="/feed.xml"></head></html>`

// client mimics what NetNewsWire's ReaderAPICaller sends.
type client struct {
	t     *testing.T
	base  string
	auth  string
	token string
}

func (c *client) do(method, path string, form url.Values) (int, http.Header, string) {
	c.t.Helper()
	var body io.Reader
	target := c.base + path
	if method == http.MethodGet && form != nil {
		target += "?" + form.Encode()
	} else if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.auth != "" {
		req.Header.Set("Authorization", "GoogleLogin auth="+c.auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func (c *client) ok(method, path string, form url.Values) string {
	c.t.Helper()
	status, _, body := c.do(method, path, form)
	if status != http.StatusOK {
		c.t.Fatalf("%s %s: status %d: %s", method, path, status, body)
	}
	return body
}

func (c *client) json(method, path string, form url.Values, v any) {
	c.t.Helper()
	if err := json.Unmarshal([]byte(c.ok(method, path, form)), v); err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
}

func (c *client) write(path string, form url.Values) {
	c.t.Helper()
	form.Set("T", c.token)
	if body := c.ok(http.MethodPost, path, form); body != "OK" {
		c.t.Fatalf("POST %s: got %q, want OK", path, body)
	}
}

type itemRefs struct {
	ItemRefs     []struct{ ID string } `json:"itemRefs"`
	Continuation string                `json:"continuation"`
}

func (c *client) ids(params url.Values) []string {
	c.t.Helper()
	params.Set("output", "json")
	params.Set("n", "1000")
	var ids []string
	for {
		var res itemRefs
		c.json(http.MethodGet, "/reader/api/0/stream/items/ids", params, &res)
		for _, r := range res.ItemRefs {
			ids = append(ids, r.ID)
		}
		if res.Continuation == "" {
			return ids
		}
		params.Set("c", res.Continuation)
	}
}

// nnwItemID formats an id the way NetNewsWire's itemIDParameter does.
func nnwItemID(decimal string) string {
	var n int64
	fmt.Sscan(decimal, &n)
	return fmt.Sprintf("tag:google.com,2005:reader/item/%.16x", n)
}

func TestNetNewsWireFlow(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed.xml":
			w.Header().Set("Content-Type", "application/rss+xml")
			io.WriteString(w, testRSS)
		default:
			io.WriteString(w, testHTML)
		}
	}))
	defer origin.Close()

	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := NewAPI(store, NewFetcher(store, time.Hour, log), "dennis", "secret", log)
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	c := &client{t: t, base: srv.URL + "/api/greader.php"}

	// Login.
	if status, _, _ := c.do(http.MethodPost, "/accounts/ClientLogin", url.Values{"Email": {"dennis"}, "Passwd": {"wrong"}}); status != http.StatusUnauthorized {
		t.Fatalf("bad password: status %d", status)
	}
	login := c.ok(http.MethodPost, "/accounts/ClientLogin", url.Values{"Email": {"dennis"}, "Passwd": {"secret"}})
	for _, line := range strings.Split(login, "\n") {
		if v, ok := strings.CutPrefix(line, "Auth="); ok {
			c.auth = v
		}
	}
	if c.auth == "" {
		t.Fatalf("no Auth in %q", login)
	}
	c.token = strings.TrimSpace(c.ok(http.MethodGet, "/reader/api/0/token", nil))

	// A fresh account already has a folder; NetNewsWire can't add feeds without one.
	var tags struct{ Tags []tagJSON }
	c.json(http.MethodGet, "/reader/api/0/tag/list", url.Values{"output": {"json"}}, &tags)
	if len(tags.Tags) != 2 || tags.Tags[1].ID != "user/-/label/Uncategorized" || tags.Tags[1].Type != "folder" {
		t.Fatalf("initial tags: %+v", tags)
	}
	if status, _, _ := c.do(http.MethodPost, "/reader/api/0/disable-tag",
		url.Values{"T": {c.token}, "s": {"user/-/label/Uncategorized"}}); status != http.StatusBadRequest {
		t.Fatalf("delete default folder: status %d", status)
	}

	// Write calls need the T token.
	if status, header, _ := c.do(http.MethodPost, "/reader/api/0/edit-tag", url.Values{"T": {"x"}}); status != http.StatusUnauthorized || header.Get("Google-Bad-Token") != "true" {
		t.Fatalf("bad token: status %d", status)
	}

	// Subscribe via the HTML page; the feed is discovered from <link rel=alternate>.
	var added struct {
		NumResults int    `json:"numResults"`
		StreamID   string `json:"streamId"`
		StreamName string `json:"streamName"`
	}
	c.json(http.MethodPost, "/reader/api/0/subscription/quickadd",
		url.Values{"T": {c.token}, "quickadd": {origin.URL + "/blog"}}, &added)
	if added.NumResults != 1 || added.StreamName != "Test Feed" {
		t.Fatalf("quickadd: %+v", added)
	}
	c.write("/reader/api/0/subscription/edit", url.Values{
		"ac": {"edit"}, "s": {added.StreamID}, "a": {"user/-/label/Tech"},
	})

	var subs struct {
		Subscriptions []subscriptionJSON `json:"subscriptions"`
	}
	c.json(http.MethodGet, "/reader/api/0/subscription/list", url.Values{"output": {"json"}}, &subs)
	if len(subs.Subscriptions) != 1 || subs.Subscriptions[0].URL != origin.URL+"/feed.xml" ||
		len(subs.Subscriptions[0].Categories) != 1 || subs.Subscriptions[0].Categories[0].Label != "Tech" {
		t.Fatalf("subscriptions: %+v", subs)
	}

	c.json(http.MethodGet, "/reader/api/0/tag/list", url.Values{"output": {"json"}}, &tags)
	if len(tags.Tags) != 3 || tags.Tags[1].ID != "user/-/label/Tech" || tags.Tags[1].Type != "folder" {
		t.Fatalf("tags: %+v", tags)
	}

	// Paging with a small page size.
	var page itemRefs
	c.json(http.MethodGet, "/reader/api/0/stream/items/ids",
		url.Values{"s": {streamReadingAll}, "n": {"2"}}, &page)
	if len(page.ItemRefs) != 2 || page.Continuation == "" {
		t.Fatalf("first page: %+v", page)
	}

	threeMonthsAgo := fmt.Sprint(time.Now().AddDate(0, -3, 0).Unix())
	all := c.ids(url.Values{"s": {streamReadingAll}, "ot": {threeMonthsAgo}})
	if len(all) != 3 {
		t.Fatalf("all ids: %v", all)
	}

	// Fetch contents with the ids encoded the NetNewsWire way.
	form := url.Values{"T": {c.token}, "output": {"json"}}
	for _, id := range all {
		form.Add("i", nnwItemID(id))
	}
	var contents struct {
		ID      string     `json:"id"`
		Updated int64      `json:"updated"`
		Items   []itemJSON `json:"items"`
	}
	c.json(http.MethodPost, "/reader/api/0/stream/items/contents", form, &contents)
	if len(contents.Items) != 3 {
		t.Fatalf("contents: %d items", len(contents.Items))
	}
	newest := contents.Items[0]
	if newest.Title != "Third" || newest.ID != nnwItemID(all[0]) || newest.Origin.StreamID != added.StreamID ||
		newest.Summary.Content != "<p>three</p>" || newest.Published != time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("newest item: %+v", newest)
	}

	// Mark read and starred.
	c.write("/reader/api/0/edit-tag", url.Values{"i": {nnwItemID(all[0])}, "a": {"user/-/state/com.google/read"}})
	c.write("/reader/api/0/edit-tag", url.Values{"i": {nnwItemID(all[1])}, "a": {"user/-/state/com.google/starred"}})

	unread := c.ids(url.Values{"s": {streamReadingAll}, "xt": {"user/-/state/com.google/read"}})
	if len(unread) != 2 {
		t.Fatalf("unread: %v", unread)
	}
	starred := c.ids(url.Values{"s": {"user/-/state/com.google/starred"}})
	if len(starred) != 1 || starred[0] != all[1] {
		t.Fatalf("starred: %v", starred)
	}

	c.write("/reader/api/0/edit-tag", url.Values{"i": {nnwItemID(all[0])}, "r": {"user/-/state/com.google/read"}})
	if unread := c.ids(url.Values{"s": {streamReadingAll}, "xt": {"user/-/state/com.google/read"}}); len(unread) != 3 {
		t.Fatalf("unread after mark unread: %v", unread)
	}

	// Folder rename keeps the feed in it.
	c.write("/reader/api/0/rename-tag", url.Values{"s": {"user/-/label/Tech"}, "dest": {"user/-/label/News"}})
	if ids := c.ids(url.Values{"s": {"user/-/label/News"}}); len(ids) != 3 {
		t.Fatalf("folder stream: %v", ids)
	}

	// Deleting a folder moves its feeds back to the default folder.
	c.write("/reader/api/0/disable-tag", url.Values{"s": {"user/-/label/News"}})
	if ids := c.ids(url.Values{"s": {"user/-/label/Uncategorized"}}); len(ids) != 3 {
		t.Fatalf("default folder after delete: %v", ids)
	}

	if opml := c.ok(http.MethodGet, "/reader/api/0/subscription/export", nil); !strings.Contains(opml, `xmlUrl="`+origin.URL+`/feed.xml"`) {
		t.Fatalf("export: %s", opml)
	}

	c.write("/reader/api/0/subscription/edit", url.Values{"ac": {"unsubscribe"}, "s": {added.StreamID}})
	if ids := c.ids(url.Values{"s": {streamReadingAll}}); len(ids) != 0 {
		t.Fatalf("after unsubscribe: %v", ids)
	}

	// Unauthenticated requests are rejected.
	c.auth = ""
	if status, _, _ := c.do(http.MethodGet, "/reader/api/0/tag/list", nil); status != http.StatusUnauthorized {
		t.Fatalf("no auth: status %d", status)
	}
}

func TestParseItemID(t *testing.T) {
	for _, in := range []string{"42", "tag:google.com,2005:reader/item/000000000000002a"} {
		id, err := parseItemID(in)
		if err != nil || id != 42 {
			t.Errorf("parseItemID(%q) = %d, %v", in, id, err)
		}
	}
	if got := longItemID(42); got != nnwItemID("42") {
		t.Errorf("longItemID(42) = %q", got)
	}
}
