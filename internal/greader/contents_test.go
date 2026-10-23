package greader

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// login returns a client in the state NetNewsWire is in after ClientLogin.
func login(t *testing.T, base string) *client {
	t.Helper()
	c := &client{t: t, base: base, auth: deriveAuthToken("dennis", "secret")}
	c.token = strings.TrimSpace(c.ok(http.MethodGet, "/reader/api/0/token", nil))
	return c
}

func TestStreamContents(t *testing.T) {
	api, origin := newTestServer(t)
	c := login(t, api.URL)
	var added quickAddResponse
	c.json(http.MethodPost, "/reader/api/0/subscription/quickadd",
		url.Values{"T": {c.token}, "quickadd": {origin.URL + "/feed.xml"}}, &added)
	if added.NumResults != 1 {
		t.Fatalf("quickadd: %+v", added)
	}

	contents := func(path string, params url.Values) streamContentsResponse {
		t.Helper()
		var resp streamContentsResponse
		c.json(http.MethodGet, path, params, &resp)
		return resp
	}
	titles := func(resp streamContentsResponse) []string {
		out := []string{}
		for _, it := range resp.Items {
			out = append(out, it.Title)
		}
		return out
	}

	const base = "/reader/api/0/stream/contents"
	tests := []struct {
		name   string
		path   string
		params url.Values
		wantID string
		want   []string
	}{
		{"default stream", base, nil, streamReadingList, []string{"Third", "Second", "First"}},
		{"stream in path", base + "/" + streamReadingList, nil, streamReadingList, []string{"Third", "Second", "First"}},
		{"oldest first", base + "/" + streamReadingList, url.Values{"r": {"o"}}, streamReadingList, []string{"First", "Second", "Third"}},
		{"feed in path", base + "/" + added.StreamID, nil, added.StreamID, []string{"Third", "Second", "First"}},
		{"stream as s", base, url.Values{"s": {added.StreamID}}, added.StreamID, []string{"Third", "Second", "First"}},
		{"folder", base + "/user/-/label/Uncategorized", nil, "user/-/label/Uncategorized", []string{"Third", "Second", "First"}},
		{"unknown feed", base + "/feed/999", nil, "feed/999", []string{}},
		{"starred", base + "/" + streamStarred, nil, streamStarred, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := contents(tt.path, tt.params)
			if got := titles(resp); !slices.Equal(got, tt.want) || resp.ID != tt.wantID {
				t.Errorf("%s = %v (id %q), want %v (id %q)", tt.path, got, resp.ID, tt.want, tt.wantID)
			}
		})
	}

	// Paging.
	first := contents(base, url.Values{"n": {"2"}})
	if got := titles(first); !slices.Equal(got, []string{"Third", "Second"}) || first.Continuation == "" {
		t.Fatalf("first page = %v, continuation %q", got, first.Continuation)
	}
	second := contents(base, url.Values{"n": {"2"}, "c": {first.Continuation}})
	if got := titles(second); !slices.Equal(got, []string{"First"}) || second.Continuation != "" {
		t.Errorf("second page = %v, continuation %q", got, second.Continuation)
	}

	// Excluding read items.
	c.write("/reader/api/0/edit-tag", url.Values{"i": {first.Items[0].ID}, "a": {"user/-/state/com.google/read"}})
	unread := contents(base, url.Values{"xt": {"user/-/state/com.google/read"}})
	if got := titles(unread); !slices.Equal(got, []string{"Second", "First"}) {
		t.Errorf("unread = %v", got)
	}

	if status, _, _ := c.do(http.MethodGet, base+"/user/-/state/com.google/like", nil); status != http.StatusBadRequest {
		t.Errorf("unsupported stream: status %d", status)
	}
}
