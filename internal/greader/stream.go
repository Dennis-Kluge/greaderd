package greader

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

// Page sizes for the n parameter. NetNewsWire asks for 1000 IDs per page.
const (
	defaultPageSize = 20
	maxPageSize     = 10000
)

// streamQuery is a parsed stream request. For feed streams ("feed/<ref>"),
// feedRef holds the feed's ID or URL, which resolve turns into Query.FeedID.
type streamQuery struct {
	store.Query
	feedRef string
}

// parseStreamQuery translates the stream parameters s (stream ID), n (page
// size), r ("o" for oldest first), ot and nt (crawl time bounds in seconds),
// xt (exclude state), it (include state) and c (continuation).
func parseStreamQuery(v url.Values) (streamQuery, error) {
	q := streamQuery{Query: store.Query{Limit: defaultPageSize, OldestFirst: v.Get("r") == "o"}}

	if n := v.Get("n"); n != "" {
		limit, err := strconv.Atoi(n)
		if err != nil || limit <= 0 {
			return q, errorf(http.StatusBadRequest, "invalid n %q", n)
		}
		q.Limit = min(limit, maxPageSize)
	}
	var err error
	if q.CrawledAfter, err = parseUnixParam(v, "ot"); err != nil {
		return q, err
	}
	if q.CrawledBefore, err = parseUnixParam(v, "nt"); err != nil {
		return q, err
	}
	if c := v.Get("c"); c != "" {
		if q.Continuation, err = strconv.ParseInt(c, 10, 64); err != nil {
			return q, errorf(http.StatusBadRequest, "invalid continuation %q", c)
		}
	}

	s := v.Get("s")
	if s == "" {
		s = streamReadingList
	}
	if ref, ok := parseFeedStream(s); ok {
		q.feedRef = ref
	} else if name, ok := parseLabel(s); ok {
		q.Folder = name
	} else {
		switch parseState(s) {
		case stateReadingList:
			// All entries.
		case stateRead:
			q.Read = store.Only
		case stateStarred:
			q.Starred = store.Only
		default:
			return q, errorf(http.StatusBadRequest, "unsupported stream %q", s)
		}
	}

	for _, xt := range v["xt"] {
		switch parseState(xt) {
		case stateRead:
			q.Read = store.Not
		case stateStarred:
			q.Starred = store.Not
		}
	}
	for _, it := range v["it"] {
		switch parseState(it) {
		case stateRead:
			q.Read = store.Only
		case stateStarred:
			q.Starred = store.Only
		}
	}
	return q, nil
}

// parseUnixParam parses an optional parameter in unix seconds.
func parseUnixParam(v url.Values, name string) (time.Time, error) {
	s := v.Get(name)
	if s == "" {
		return time.Time{}, nil
	}
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil || sec < 0 {
		return time.Time{}, errorf(http.StatusBadRequest, "invalid %s %q", name, s)
	}
	if sec == 0 {
		return time.Time{}, nil
	}
	return time.Unix(sec, 0), nil
}

// resolve returns the store query for q. found is false if q names a feed
// that does not exist, in which case the stream is empty.
func (s *Server) resolve(ctx context.Context, q streamQuery) (query store.Query, found bool, err error) {
	if q.feedRef == "" {
		return q.Query, true, nil
	}
	feed, err := s.feedByRef(ctx, q.feedRef)
	if errors.Is(err, store.ErrNotFound) {
		return q.Query, false, nil
	}
	if err != nil {
		return q.Query, false, err
	}
	q.FeedID = feed.ID
	return q.Query, true, nil
}

// streamItemIDs handles GET /reader/api/0/stream/items/ids.
func (s *Server) streamItemIDs(w http.ResponseWriter, r *http.Request) error {
	sq, err := parseStreamQuery(r.URL.Query())
	if err != nil {
		return err
	}
	q, found, err := s.resolve(r.Context(), sq)
	if err != nil {
		return err
	}
	resp := itemIDsResponse{ItemRefs: []itemRef{}}
	if !found {
		return writeJSON(w, resp)
	}
	refs, continuation, err := s.store.ItemIDs(r.Context(), q)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		resp.ItemRefs = append(resp.ItemRefs, itemRef{
			ID:              strconv.FormatInt(ref.ID, 10),
			DirectStreamIDs: []string{},
			TimestampUsec:   formatUsec(ref.Crawled),
		})
	}
	if continuation > 0 {
		resp.Continuation = strconv.FormatInt(continuation, 10)
	}
	return writeJSON(w, resp)
}

// streamItemContents handles POST /reader/api/0/stream/items/contents with
// the entries given as repeated i parameters.
func (s *Server) streamItemContents(w http.ResponseWriter, r *http.Request) error {
	ids, err := parseItemIDs(r.Form["i"])
	if err != nil {
		return errorf(http.StatusBadRequest, "%v", err)
	}
	entries, err := s.store.Entries(r.Context(), ids)
	if err != nil {
		return err
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		items = append(items, newItem(e))
	}
	return writeJSON(w, streamContentsResponse{
		ID:      streamReadingList,
		Updated: time.Now().Unix(),
		Items:   items,
	})
}

func newItem(e store.Entry) item {
	categories := []string{streamReadingList}
	if e.Folder != "" {
		categories = append(categories, labelStreamID(e.Folder))
	}
	if e.Read {
		categories = append(categories, stateStreamID(stateRead))
	}
	if e.Starred {
		categories = append(categories, stateStreamID(stateStarred))
	}
	links := []link{}
	if e.URL != "" {
		links = append(links, link{Href: e.URL, Type: "text/html"})
	}
	return item{
		ID:            longItemID(e.ID),
		CrawlTimeMsec: strconv.FormatInt(e.Crawled.UnixMilli(), 10),
		TimestampUsec: strconv.FormatInt(e.Crawled.UnixMicro(), 10),
		Published:     e.Published.Unix(),
		Updated:       e.Published.Unix(),
		Title:         e.Title,
		Author:        e.Author,
		Canonical:     links,
		Alternate:     links,
		Categories:    categories,
		Origin: origin{
			StreamID: feedStreamID(e.FeedID),
			Title:    e.FeedTitle,
			HTMLURL:  e.FeedSiteURL,
		},
		Summary: content{Content: e.Content},
		Content: content{Content: e.Content},
	}
}

// editTag handles POST /reader/api/0/edit-tag, which adds (a) or removes (r)
// states on the entries given as i.
func (s *Server) editTag(w http.ResponseWriter, r *http.Request) error {
	ids, err := parseItemIDs(r.Form["i"])
	if err != nil {
		return errorf(http.StatusBadRequest, "%v", err)
	}
	ctx := r.Context()
	for _, tag := range r.Form["a"] {
		if err := s.applyState(ctx, ids, parseState(tag), true); err != nil {
			return err
		}
	}
	for _, tag := range r.Form["r"] {
		if err := s.applyState(ctx, ids, parseState(tag), false); err != nil {
			return err
		}
	}
	writeText(w, "OK")
	return nil
}

// applyState adds or removes a state. Other tags (broadcast, like, …) are
// ignored, as in FreshRSS.
func (s *Server) applyState(ctx context.Context, ids []int64, state string, add bool) error {
	switch state {
	case stateRead:
		return s.store.SetRead(ctx, ids, add)
	case stateStarred:
		return s.store.SetStarred(ctx, ids, add)
	case stateKeptUnread:
		if add {
			return s.store.SetRead(ctx, ids, false)
		}
	}
	return nil
}

// markAllAsRead handles POST /reader/api/0/mark-all-as-read for stream s,
// optionally limited to entries crawled up to ts (microseconds).
func (s *Server) markAllAsRead(w http.ResponseWriter, r *http.Request) error {
	sq, err := parseStreamQuery(url.Values{"s": {r.Form.Get("s")}})
	if err != nil {
		return err
	}
	if ts := r.Form.Get("ts"); ts != "" {
		usec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return errorf(http.StatusBadRequest, "invalid ts %q", ts)
		}
		sq.CrawledBefore = time.UnixMicro(usec + 1)
	}
	q, found, err := s.resolve(r.Context(), sq)
	if err != nil {
		return err
	}
	if found {
		if err := s.store.MarkAllRead(r.Context(), q); err != nil {
			return err
		}
	}
	writeText(w, "OK")
	return nil
}
