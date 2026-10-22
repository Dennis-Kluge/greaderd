package greader

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

	"github.com/Dennis-Kluge/greaderd/internal/fetcher"
	"github.com/Dennis-Kluge/greaderd/internal/store"
)

const testRSS = `<?xml version="1.0"?>
<rss version="2.0"><channel>
<title>Test Feed</title><link>https://example.com/</link>
<item><guid>c</guid><title>Third</title><link>https://example.com/3</link><pubDate>Wed, 21 Oct 2026 12:00:00 GMT</pubDate><description>&lt;p&gt;three&lt;/p&gt;</description></item>
<item><guid>b</guid><title>Second</title><link>https://example.com/2</link><pubDate>Tue, 20 Oct 2026 12:00:00 GMT</pubDate><description>two</description></item>
<item><guid>a</guid><title>First</title><link>https://example.com/1</link><pubDate>Mon, 19 Oct 2026 12:00:00 GMT</pubDate><description>one</description></item>
</channel></rss>`

const testHTML = `<html><head><link rel="alternate" type="application/rss+xml" href="/feed.xml"></head></html>`

// client sends requests the way NetNewsWire's ReaderAPICaller does.
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
	req, err := http.NewRequestWithContext(c.t.Context(), method, target, body)
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
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

// write sends a write request with the T token and expects "OK".
func (c *client) write(path string, form url.Values) {
	c.t.Helper()
	form.Set("T", c.token)
	if body := c.ok(http.MethodPost, path, form); body != "OK" {
		c.t.Fatalf("POST %s: got %q, want OK", path, body)
	}
}

// ids follows continuations and returns all item IDs of a stream.
func (c *client) ids(params url.Values) []string {
	c.t.Helper()
	params.Set("output", "json")
	params.Set("n", "1000")
	ids := []string{}
	for {
		var res itemIDsResponse
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

// nnwItemIDs converts decimal IDs from items/ids the way NetNewsWire does
// before sending them back.
func nnwItemIDs(t *testing.T, ids ...string) []string {
	t.Helper()
	var out []string
	for _, id := range ids {
		var n int64
		if _, err := fmt.Sscan(id, &n); err != nil {
			t.Fatal(err)
		}
		out = append(out, nnwItemID(n))
	}
	return out
}

func newTestServer(t *testing.T) (api *httptest.Server, origin *httptest.Server) {
	t.Helper()
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed.xml":
			w.Header().Set("Content-Type", "application/rss+xml")
			io.WriteString(w, testRSS)
		default:
			io.WriteString(w, testHTML)
		}
	}))
	t.Cleanup(origin.Close)

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// httptest listens on loopback, which the fetcher refuses by default.
	f := fetcher.New(st, fetcher.Options{Logger: log, AllowPrivateNetworks: true})
	srv := New(st, f, Credentials{User: "dennis", Password: "secret"}, Options{Logger: log})
	api = httptest.NewServer(srv.Handler())
	t.Cleanup(api.Close)
	return api, origin
}

// TestNetNewsWireFlow replays NetNewsWire's sequence of requests.
func TestNetNewsWireFlow(t *testing.T) {
	api, origin := newTestServer(t)
	c := &client{t: t, base: api.URL + "/api/greader.php"}

	// Login.
	if status, _, _ := c.do(http.MethodPost, "/accounts/ClientLogin",
		url.Values{"Email": {"dennis"}, "Passwd": {"wrong"}}); status != http.StatusUnauthorized {
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

	// Write calls need the T token.
	status, header, _ := c.do(http.MethodPost, "/reader/api/0/edit-tag", url.Values{"T": {"x"}})
	if status != http.StatusUnauthorized || header.Get("Google-Bad-Token") != "true" {
		t.Fatalf("bad token: status %d", status)
	}

	// A fresh account already has a folder; NetNewsWire can't add feeds without one.
	var tags tagListResponse
	c.json(http.MethodGet, "/reader/api/0/tag/list", url.Values{"output": {"json"}}, &tags)
	if len(tags.Tags) != 2 || tags.Tags[1].ID != "user/-/label/Uncategorized" || tags.Tags[1].Type != "folder" {
		t.Fatalf("initial tags: %+v", tags)
	}
	if status, _, _ := c.do(http.MethodPost, "/reader/api/0/disable-tag",
		url.Values{"T": {c.token}, "s": {"user/-/label/Uncategorized"}}); status != http.StatusBadRequest {
		t.Fatalf("delete default folder: status %d", status)
	}

	// Subscribe via the HTML page; the feed is discovered from <link rel=alternate>.
	var added quickAddResponse
	c.json(http.MethodPost, "/reader/api/0/subscription/quickadd",
		url.Values{"T": {c.token}, "quickadd": {origin.URL + "/blog"}}, &added)
	if added.NumResults != 1 || added.StreamName != "Test Feed" {
		t.Fatalf("quickadd: %+v", added)
	}
	c.write("/reader/api/0/subscription/edit", url.Values{
		"ac": {"edit"}, "s": {added.StreamID}, "a": {"user/-/label/Tech"},
	})

	var subs subscriptionListResponse
	c.json(http.MethodGet, "/reader/api/0/subscription/list", url.Values{"output": {"json"}}, &subs)
	if len(subs.Subscriptions) != 1 || subs.Subscriptions[0].URL != origin.URL+"/feed.xml" ||
		len(subs.Subscriptions[0].Categories) != 1 || subs.Subscriptions[0].Categories[0].Label != "Tech" {
		t.Fatalf("subscriptions: %+v", subs)
	}

	c.json(http.MethodGet, "/reader/api/0/tag/list", url.Values{"output": {"json"}}, &tags)
	if len(tags.Tags) != 3 || tags.Tags[1].ID != "user/-/label/Tech" {
		t.Fatalf("tags: %+v", tags)
	}

	// Paging with a small page size.
	var page itemIDsResponse
	c.json(http.MethodGet, "/reader/api/0/stream/items/ids", url.Values{"s": {streamReadingList}, "n": {"2"}}, &page)
	if len(page.ItemRefs) != 2 || page.Continuation == "" {
		t.Fatalf("first page: %+v", page)
	}

	threeMonthsAgo := fmt.Sprint(time.Now().AddDate(0, -3, 0).Unix())
	all := c.ids(url.Values{"s": {streamReadingList}, "ot": {threeMonthsAgo}})
	if len(all) != 3 {
		t.Fatalf("all ids: %v", all)
	}

	// Fetch contents with the IDs encoded the NetNewsWire way.
	var contents streamContentsResponse
	c.json(http.MethodPost, "/reader/api/0/stream/items/contents",
		url.Values{"T": {c.token}, "output": {"json"}, "i": nnwItemIDs(t, all...)}, &contents)
	if len(contents.Items) != 3 {
		t.Fatalf("contents: %d items", len(contents.Items))
	}
	newest := contents.Items[0]
	if newest.Title != "Third" || newest.ID != nnwItemIDs(t, all[0])[0] || newest.Origin.StreamID != added.StreamID ||
		newest.Summary.Content != "<p>three</p>" || newest.Published != time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("newest item: %+v", newest)
	}

	// Mark read and starred.
	c.write("/reader/api/0/edit-tag", url.Values{"i": nnwItemIDs(t, all[0]), "a": {"user/-/state/com.google/read"}})
	c.write("/reader/api/0/edit-tag", url.Values{"i": nnwItemIDs(t, all[1]), "a": {"user/-/state/com.google/starred"}})

	unread := c.ids(url.Values{"s": {streamReadingList}, "xt": {"user/-/state/com.google/read"}})
	if len(unread) != 2 {
		t.Fatalf("unread: %v", unread)
	}
	starred := c.ids(url.Values{"s": {streamStarred}})
	if len(starred) != 1 || starred[0] != all[1] {
		t.Fatalf("starred: %v", starred)
	}

	c.write("/reader/api/0/edit-tag", url.Values{"i": nnwItemIDs(t, all[0]), "r": {"user/-/state/com.google/read"}})
	if unread := c.ids(url.Values{"s": {streamReadingList}, "xt": {"user/-/state/com.google/read"}}); len(unread) != 3 {
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

	c.write("/reader/api/0/mark-all-as-read", url.Values{"s": {added.StreamID}})
	if unread := c.ids(url.Values{"xt": {"user/-/state/com.google/read"}}); len(unread) != 0 {
		t.Fatalf("unread after mark-all-as-read: %v", unread)
	}

	if opml := c.ok(http.MethodGet, "/reader/api/0/subscription/export", nil); !strings.Contains(opml, `xmlUrl="`+origin.URL+`/feed.xml"`) {
		t.Fatalf("export: %s", opml)
	}

	c.write("/reader/api/0/subscription/edit", url.Values{"ac": {"unsubscribe"}, "s": {added.StreamID}})
	if ids := c.ids(url.Values{"s": {streamReadingList}}); len(ids) != 0 {
		t.Fatalf("after unsubscribe: %v", ids)
	}
	if ids := c.ids(url.Values{"s": {added.StreamID}}); len(ids) != 0 {
		t.Fatalf("stream of deleted feed: %v", ids)
	}

	// Unauthenticated requests are rejected.
	c.auth = ""
	if status, _, _ := c.do(http.MethodGet, "/reader/api/0/tag/list", nil); status != http.StatusUnauthorized {
		t.Fatalf("no auth: status %d", status)
	}
}

func TestOPMLImport(t *testing.T) {
	api, _ := newTestServer(t)
	c := &client{t: t, base: api.URL, auth: deriveAuthToken("dennis", "secret")}

	doc := `<opml><body>
		<outline text="Tech"><outline text="A" xmlUrl="https://a.example/feed"/></outline>
		<outline text="B" xmlUrl="https://b.example/feed"/>
		<outline text="Invalid" xmlUrl="javascript:alert(1)"/>
	</body></opml>`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		api.URL+"/reader/api/0/subscription/import", strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/xml")
	req.Header.Set("Authorization", "GoogleLogin auth="+c.auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import: status %d", resp.StatusCode)
	}

	var subs subscriptionListResponse
	c.json(http.MethodGet, "/reader/api/0/subscription/list", nil, &subs)
	got := map[string]string{}
	for _, s := range subs.Subscriptions {
		got[s.Title] = s.Categories[0].Label
	}
	if len(got) != 2 || got["A"] != "Tech" || got["B"] != "Uncategorized" {
		t.Errorf("imported subscriptions: %v", got)
	}
}

func deriveAuthToken(user, password string) string {
	auth, _ := deriveTokens(Credentials{User: user, Password: password})
	return auth
}
