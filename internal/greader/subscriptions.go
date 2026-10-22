package greader

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/opml"
	"github.com/Dennis-Kluge/greaderd/internal/store"
)

// tagList handles GET /reader/api/0/tag/list: the starred state and all folders.
func (s *Server) tagList(w http.ResponseWriter, r *http.Request) error {
	folders, err := s.store.Folders(r.Context())
	if err != nil {
		return err
	}
	resp := tagListResponse{Tags: []tag{{ID: streamStarred}}}
	for _, f := range folders {
		resp.Tags = append(resp.Tags, tag{ID: labelStreamID(f.Name), Type: "folder", UnreadCount: &f.Unread})
	}
	return writeJSON(w, resp)
}

// subscriptionList handles GET /reader/api/0/subscription/list.
func (s *Server) subscriptionList(w http.ResponseWriter, r *http.Request) error {
	feeds, err := s.store.Feeds(r.Context())
	if err != nil {
		return err
	}
	resp := subscriptionListResponse{Subscriptions: make([]subscription, 0, len(feeds))}
	for _, f := range feeds {
		categories := []category{}
		if f.Folder != "" {
			categories = append(categories, category{ID: labelStreamID(f.Folder), Label: f.Folder})
		}
		resp.Subscriptions = append(resp.Subscriptions, subscription{
			ID:         feedStreamID(f.ID),
			Title:      displayTitle(f),
			Categories: categories,
			URL:        f.URL,
			HTMLURL:    f.SiteURL,
		})
	}
	return writeJSON(w, resp)
}

// quickAdd handles POST /reader/api/0/subscription/quickadd. The quickadd
// parameter may be a feed URL or a web page that advertises a feed.
func (s *Server) quickAdd(w http.ResponseWriter, r *http.Request) error {
	query := strings.TrimPrefix(r.Form.Get("quickadd"), "feed/")
	// Like FreshRSS, failures are reported in the body; clients check numResults.
	fail := func(err error) error {
		return writeJSON(w, quickAddResponse{Query: query, Error: err.Error()})
	}
	if !isHTTPURL(query) {
		return fail(errors.New("invalid URL"))
	}

	ctx, cancel := context.WithTimeout(r.Context(), subscribeTimeout)
	defer cancel()
	feedURL, err := s.fetcher.Discover(ctx, query)
	if err != nil {
		return fail(err)
	}
	feed, err := s.subscribe(ctx, feedURL)
	if err != nil {
		return fail(err)
	}
	return writeJSON(w, quickAddResponse{
		NumResults: 1,
		Query:      query,
		StreamID:   feedStreamID(feed.ID),
		StreamName: displayTitle(feed),
	})
}

// subscribe adds the feed at feedURL and fetches it once, so clients see its
// title right away. A new feed that cannot be fetched is removed again.
func (s *Server) subscribe(ctx context.Context, feedURL string) (store.Feed, error) {
	feed, created, err := s.store.AddFeed(ctx, feedURL)
	if err != nil || !created {
		return feed, err
	}
	if err := s.fetcher.Refresh(ctx, feed); err != nil {
		// Clean up even if ctx expired during the fetch.
		delErr := s.store.DeleteFeed(context.WithoutCancel(ctx), feed.ID)
		return store.Feed{}, errors.Join(err, delErr)
	}
	return s.store.FeedByID(ctx, feed.ID)
}

// subscriptionEdit handles POST /reader/api/0/subscription/edit. The action
// ac applies to every stream s: "subscribe", "unsubscribe" or "edit" (the
// default), with an optional title t and folder changes a (add) and r (remove).
func (s *Server) subscriptionEdit(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	edit := feedEdit{title: r.Form.Get("t")}
	edit.addFolder, _ = parseLabel(r.Form.Get("a"))
	edit.removeFolder, _ = parseLabel(r.Form.Get("r"))

	for _, streamID := range r.Form["s"] {
		var err error
		switch action := r.Form.Get("ac"); action {
		case "subscribe":
			err = s.editSubscribe(ctx, streamID, edit)
		case "unsubscribe":
			err = s.unsubscribe(ctx, streamID)
		case "edit", "":
			err = s.editFeed(ctx, streamID, edit)
		default:
			return errorf(http.StatusBadRequest, "unsupported action %q", action)
		}
		if err != nil {
			return err
		}
	}
	writeText(w, "OK")
	return nil
}

// feedEdit holds the optional changes of a subscription/edit request.
type feedEdit struct {
	title        string
	addFolder    string
	removeFolder string
}

func (s *Server) editSubscribe(ctx context.Context, streamID string, edit feedEdit) error {
	feedURL, ok := parseFeedStream(streamID)
	if !ok || !isHTTPURL(feedURL) {
		return errorf(http.StatusBadRequest, "invalid feed %q", streamID)
	}
	feed, err := s.subscribe(ctx, feedURL)
	if err != nil {
		return err
	}
	return s.applyEdit(ctx, feed, edit)
}

func (s *Server) unsubscribe(ctx context.Context, streamID string) error {
	feed, err := s.feedByStreamID(ctx, streamID)
	if err != nil {
		return err
	}
	return s.store.DeleteFeed(ctx, feed.ID)
}

func (s *Server) editFeed(ctx context.Context, streamID string, edit feedEdit) error {
	feed, err := s.feedByStreamID(ctx, streamID)
	if err != nil {
		return err
	}
	return s.applyEdit(ctx, feed, edit)
}

func (s *Server) applyEdit(ctx context.Context, feed store.Feed, edit feedEdit) error {
	if edit.title != "" {
		if err := s.store.SetFeedTitle(ctx, feed.ID, edit.title); err != nil {
			return err
		}
	}
	switch {
	case edit.addFolder != "":
		// A feed has exactly one folder, so adding one also moves the feed.
		return s.store.SetFeedFolder(ctx, feed.ID, edit.addFolder)
	case edit.removeFolder != "" && edit.removeFolder == feed.Folder:
		return s.store.SetFeedFolder(ctx, feed.ID, "")
	}
	return nil
}

// renameTag handles POST /reader/api/0/rename-tag with the label streams s and dest.
func (s *Server) renameTag(w http.ResponseWriter, r *http.Request) error {
	from, ok := parseLabel(r.Form.Get("s"))
	if !ok {
		return errorf(http.StatusBadRequest, "invalid source label %q", r.Form.Get("s"))
	}
	to, ok := parseLabel(r.Form.Get("dest"))
	if !ok {
		return errorf(http.StatusBadRequest, "invalid destination label %q", r.Form.Get("dest"))
	}
	if err := s.store.RenameFolder(r.Context(), from, to); err != nil {
		return err
	}
	writeText(w, "OK")
	return nil
}

// disableTag handles POST /reader/api/0/disable-tag, which deletes the
// folders given as s. Their feeds move to the default folder.
func (s *Server) disableTag(w http.ResponseWriter, r *http.Request) error {
	for _, streamID := range r.Form["s"] {
		name, ok := parseLabel(streamID)
		if !ok {
			return errorf(http.StatusBadRequest, "invalid label %q", streamID)
		}
		if err := s.store.DeleteFolder(r.Context(), name); err != nil {
			return err
		}
	}
	writeText(w, "OK")
	return nil
}

// unreadCount handles GET /reader/api/0/unread-count with counts per feed,
// per folder and for the reading list.
func (s *Server) unreadCount(w http.ResponseWriter, r *http.Request) error {
	feeds, err := s.store.Feeds(r.Context())
	if err != nil {
		return err
	}
	type tally struct {
		count  int
		newest time.Time
	}
	add := func(t *tally, f store.Feed) {
		t.count += f.Unread
		if f.NewestCrawled.After(t.newest) {
			t.newest = f.NewestCrawled
		}
	}
	toWire := func(id string, t tally) unreadCount {
		return unreadCount{ID: id, Count: t.count, NewestItemTimestampUsec: formatUsec(t.newest)}
	}

	counts := make([]unreadCount, 0, len(feeds)+1)
	var total tally
	folders := map[string]*tally{}
	var folderOrder []string
	for _, f := range feeds {
		counts = append(counts, toWire(feedStreamID(f.ID), tally{f.Unread, f.NewestCrawled}))
		add(&total, f)
		if f.Folder == "" {
			continue
		}
		t, ok := folders[f.Folder]
		if !ok {
			t = &tally{}
			folders[f.Folder] = t
			folderOrder = append(folderOrder, f.Folder)
		}
		add(t, f)
	}
	for _, name := range folderOrder {
		counts = append(counts, toWire(labelStreamID(name), *folders[name]))
	}
	counts = append(counts, toWire(streamReadingList, total))
	return writeJSON(w, unreadCountResponse{Max: total.count, UnreadCounts: counts})
}

// subscriptionImport handles POST /reader/api/0/subscription/import with an
// OPML document as the body. Feeds are only registered; the fetcher picks
// them up within a minute.
func (s *Server) subscriptionImport(w http.ResponseWriter, r *http.Request) error {
	subs, err := opml.Parse(r.Body)
	if err != nil {
		return errorf(http.StatusBadRequest, "invalid OPML: %v", err)
	}
	for _, sub := range subs {
		if !isHTTPURL(sub.FeedURL) {
			continue
		}
		if err := s.importFeed(r.Context(), sub); err != nil {
			return err
		}
	}
	writeText(w, "OK")
	return nil
}

// importFeed adds one OPML subscription; existing feeds are left unchanged.
func (s *Server) importFeed(ctx context.Context, sub opml.Subscription) error {
	feed, created, err := s.store.AddFeed(ctx, sub.FeedURL)
	if err != nil || !created {
		return err
	}
	if sub.Title != "" {
		if err := s.store.SetFeedTitle(ctx, feed.ID, sub.Title); err != nil {
			return err
		}
	}
	if sub.Folder != "" {
		return s.store.SetFeedFolder(ctx, feed.ID, sub.Folder)
	}
	return nil
}

// subscriptionExport handles GET /reader/api/0/subscription/export.
func (s *Server) subscriptionExport(w http.ResponseWriter, r *http.Request) error {
	feeds, err := s.store.Feeds(r.Context())
	if err != nil {
		return err
	}
	subs := make([]opml.Subscription, 0, len(feeds))
	for _, f := range feeds {
		subs = append(subs, opml.Subscription{
			Title:   displayTitle(f),
			FeedURL: f.URL,
			SiteURL: f.SiteURL,
			Folder:  f.Folder,
		})
	}
	var buf bytes.Buffer
	if err := opml.Write(&buf, "greaderd subscriptions", subs); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.Header().Set("Content-Disposition", `attachment; filename="greaderd.opml"`)
	writeBody(w, buf.Bytes())
	return nil
}

// feedByStreamID resolves a "feed/<id or URL>" stream ID.
func (s *Server) feedByStreamID(ctx context.Context, streamID string) (store.Feed, error) {
	ref, ok := parseFeedStream(streamID)
	if !ok {
		return store.Feed{}, errorf(http.StatusBadRequest, "invalid feed %q", streamID)
	}
	return s.feedByRef(ctx, ref)
}

// feedByRef looks a feed up by its numeric ID or, failing that, its URL.
func (s *Server) feedByRef(ctx context.Context, ref string) (store.Feed, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return s.store.FeedByID(ctx, id)
	}
	return s.store.FeedByURL(ctx, ref)
}

// displayTitle falls back to the URL for feeds that were never fetched.
func displayTitle(f store.Feed) string {
	if f.Title != "" {
		return f.Title
	}
	return f.URL
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// formatUsec formats t as microseconds since the epoch; the zero time is "0".
func formatUsec(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixMicro(), 10)
}
