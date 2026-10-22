package greader

import (
	"net/url"
	"testing"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

func TestParseStreamQuery(t *testing.T) {
	tests := []struct {
		name    string
		params  url.Values
		want    streamQuery
		wantErr bool
	}{
		{
			name:   "defaults to the reading list",
			params: url.Values{},
			want:   streamQuery{Query: store.Query{Limit: defaultPageSize}},
		},
		{
			name:   "NetNewsWire unread request",
			params: url.Values{"s": {streamReadingList}, "n": {"1000"}, "xt": {"user/-/state/com.google/read"}, "c": {"77"}},
			want:   streamQuery{Query: store.Query{Limit: 1000, Read: store.Not, Continuation: 77}},
		},
		{
			name:   "time bounds and order",
			params: url.Values{"ot": {"1800000000"}, "nt": {"1800000100"}, "r": {"o"}},
			want: streamQuery{Query: store.Query{
				Limit:         defaultPageSize,
				OldestFirst:   true,
				CrawledAfter:  time.Unix(1800000000, 0),
				CrawledBefore: time.Unix(1800000100, 0),
			}},
		},
		{
			name:   "page size is capped",
			params: url.Values{"n": {"999999"}},
			want:   streamQuery{Query: store.Query{Limit: maxPageSize}},
		},
		{
			name:   "starred stream",
			params: url.Values{"s": {streamStarred}},
			want:   streamQuery{Query: store.Query{Limit: defaultPageSize, Starred: store.Only}},
		},
		{
			name:   "read stream",
			params: url.Values{"s": {"user/-/state/com.google/read"}},
			want:   streamQuery{Query: store.Query{Limit: defaultPageSize, Read: store.Only}},
		},
		{
			name:   "include filter",
			params: url.Values{"it": {"user/-/state/com.google/starred"}},
			want:   streamQuery{Query: store.Query{Limit: defaultPageSize, Starred: store.Only}},
		},
		{
			name:   "feed by ID",
			params: url.Values{"s": {"feed/12"}},
			want:   streamQuery{Query: store.Query{Limit: defaultPageSize}, feedRef: "12"},
		},
		{
			name:   "feed by URL",
			params: url.Values{"s": {"feed/https://a.example/feed"}},
			want:   streamQuery{Query: store.Query{Limit: defaultPageSize}, feedRef: "https://a.example/feed"},
		},
		{
			name:   "folder",
			params: url.Values{"s": {"user/-/label/Tech"}},
			want:   streamQuery{Query: store.Query{Limit: defaultPageSize, Folder: "Tech"}},
		},
		{name: "unsupported stream", params: url.Values{"s": {"user/-/state/com.google/like"}}, wantErr: true},
		{name: "invalid n", params: url.Values{"n": {"many"}}, wantErr: true},
		{name: "zero n", params: url.Values{"n": {"0"}}, wantErr: true},
		{name: "invalid ot", params: url.Values{"ot": {"yesterday"}}, wantErr: true},
		{name: "invalid continuation", params: url.Values{"c": {"next"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStreamQuery(tt.params)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseStreamQuery = %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStreamQuery: %v", err)
			}
			if !got.CrawledAfter.Equal(tt.want.CrawledAfter) || !got.CrawledBefore.Equal(tt.want.CrawledBefore) {
				t.Errorf("time bounds = %v..%v, want %v..%v",
					got.CrawledAfter, got.CrawledBefore, tt.want.CrawledAfter, tt.want.CrawledBefore)
			}
			got.CrawledAfter, got.CrawledBefore = tt.want.CrawledAfter, tt.want.CrawledBefore
			if got != tt.want {
				t.Errorf("parseStreamQuery = %+v, want %+v", got, tt.want)
			}
		})
	}
}
