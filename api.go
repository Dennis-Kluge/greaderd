package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Google Reader API (the subset FreshRSS offers and NetNewsWire uses).
// Docs of the protocol are folklore; FreshRSS's p/api/greader.php is the reference.

const (
	itemIDPrefix     = "tag:google.com,2005:reader/item/"
	streamReadingAll = "user/-/state/com.google/reading-list"
	stateRead        = "read"
	stateStarred     = "starred"
	stateKeptUnread  = "kept-unread"
	stateReadingList = "reading-list"
	maxBodyBytes     = 4 << 20
)

type API struct {
	store      *Store
	fetcher    *Fetcher
	user       string
	password   string
	authToken  string // value of "Authorization: GoogleLogin auth=..."
	writeToken string // value of the T parameter
	log        *slog.Logger
}

func NewAPI(store *Store, fetcher *Fetcher, user, password string, log *slog.Logger) *API {
	sign := func(purpose string) string {
		mac := hmac.New(sha256.New, []byte(password))
		mac.Write([]byte(purpose + ":" + user))
		return hex.EncodeToString(mac.Sum(nil))
	}
	return &API{
		store:      store,
		fetcher:    fetcher,
		user:       user,
		password:   password,
		authToken:  user + "/" + sign("auth"),
		writeToken: sign("token")[:57],
		log:        log,
	}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /accounts/ClientLogin", a.clientLogin)

	mux.Handle("GET /reader/api/0/token", a.auth(a.token))
	mux.Handle("GET /reader/api/0/user-info", a.auth(a.userInfo))
	mux.Handle("GET /reader/api/0/tag/list", a.auth(a.tagList))
	mux.Handle("GET /reader/api/0/subscription/list", a.auth(a.subscriptionList))
	mux.Handle("GET /reader/api/0/subscription/export", a.auth(a.subscriptionExport))
	mux.Handle("POST /reader/api/0/subscription/import", a.auth(a.subscriptionImport))
	mux.Handle("GET /reader/api/0/unread-count", a.auth(a.unreadCount))
	mux.Handle("GET /reader/api/0/stream/items/ids", a.auth(a.streamItemIDs))

	mux.Handle("POST /reader/api/0/subscription/quickadd", a.write(a.quickAdd))
	mux.Handle("POST /reader/api/0/subscription/edit", a.write(a.subscriptionEdit))
	mux.Handle("POST /reader/api/0/rename-tag", a.write(a.renameTag))
	mux.Handle("POST /reader/api/0/disable-tag", a.write(a.disableTag))
	mux.Handle("POST /reader/api/0/stream/items/contents", a.write(a.streamItemContents))
	mux.Handle("POST /reader/api/0/edit-tag", a.write(a.editTag))
	mux.Handle("POST /reader/api/0/mark-all-as-read", a.write(a.markAllAsRead))

	// Clients configured for FreshRSS append /api/greader.php to the base URL.
	root := http.NewServeMux()
	root.Handle("/api/greader.php/", http.StripPrefix("/api/greader.php", mux))
	root.Handle("/", mux)
	return a.logRequests(root)
}

// --- middleware ---

func (a *API) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "GoogleLogin auth=")
		if !ok || !secureEqual(token, a.authToken) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		h(w, r)
	})
}

// write additionally requires the T token from /token.
func (a *API) write(h http.HandlerFunc) http.Handler {
	return a.auth(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if !secureEqual(r.Form.Get("T"), a.writeToken) {
			w.Header().Set("Google-Bad-Token", "true")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (a *API) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		// Only the path: query strings and bodies may carry credentials.
		a.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond))
	})
}

func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// --- handlers: auth ---

func (a *API) clientLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	userOK := secureEqual(r.PostForm.Get("Email"), a.user)
	passOK := secureEqual(r.PostForm.Get("Passwd"), a.password)
	if !userOK || !passOK {
		http.Error(w, "Error=BadAuthentication", http.StatusUnauthorized)
		return
	}
	writeText(w, fmt.Sprintf("SID=%s\nLSID=null\nAuth=%s\n", a.authToken, a.authToken))
}

func (a *API) token(w http.ResponseWriter, r *http.Request) {
	writeText(w, a.writeToken+"\n")
}

func (a *API) userInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{
		"userId":        "1",
		"userName":      a.user,
		"userProfileId": "1",
		"userEmail":     a.user,
	})
}

// --- handlers: tags and subscriptions ---

type tagJSON struct {
	ID          string `json:"id"`
	Type        string `json:"type,omitempty"`
	UnreadCount *int   `json:"unread_count,omitempty"`
}

func (a *API) tagList(w http.ResponseWriter, r *http.Request) {
	folders, err := a.store.Folders(r.Context())
	if err != nil {
		a.serverError(w, err)
		return
	}
	tags := []tagJSON{{ID: "user/-/state/com.google/starred"}}
	for _, f := range folders {
		tags = append(tags, tagJSON{ID: labelID(f.Name), Type: "folder", UnreadCount: &f.Unread})
	}
	writeJSON(w, map[string]any{"tags": tags})
}

type categoryJSON struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type subscriptionJSON struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	Categories []categoryJSON `json:"categories"`
	URL        string         `json:"url"`
	HTMLURL    string         `json:"htmlUrl"`
	IconURL    string         `json:"iconUrl"`
}

func (a *API) subscriptionList(w http.ResponseWriter, r *http.Request) {
	feeds, err := a.store.Feeds(r.Context())
	if err != nil {
		a.serverError(w, err)
		return
	}
	subs := make([]subscriptionJSON, 0, len(feeds))
	for _, f := range feeds {
		cats := []categoryJSON{}
		if f.Folder != "" {
			cats = append(cats, categoryJSON{ID: labelID(f.Folder), Label: f.Folder})
		}
		subs = append(subs, subscriptionJSON{
			ID:         feedStreamID(f.ID),
			Title:      displayTitle(f),
			Categories: cats,
			URL:        f.URL,
			HTMLURL:    f.SiteURL,
		})
	}
	writeJSON(w, map[string]any{"subscriptions": subs})
}

func (a *API) quickAdd(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimPrefix(r.Form.Get("quickadd"), "feed/")
	if !isHTTPURL(query) {
		writeJSON(w, map[string]any{"numResults": 0, "query": query, "error": "invalid URL"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	feedURL, err := a.fetcher.Discover(ctx, query)
	if err != nil {
		writeJSON(w, map[string]any{"numResults": 0, "query": query, "error": err.Error()})
		return
	}
	feed, err := a.subscribe(ctx, feedURL)
	if err != nil {
		writeJSON(w, map[string]any{"numResults": 0, "query": query, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{
		"numResults": 1,
		"query":      query,
		"streamId":   feedStreamID(feed.ID),
		"streamName": displayTitle(feed),
	})
}

// subscribe adds a feed and fetches it once; a new feed that fails to fetch is removed again.
func (a *API) subscribe(ctx context.Context, feedURL string) (Feed, error) {
	feed, created, err := a.store.AddFeed(ctx, feedURL)
	if err != nil || !created {
		return feed, err
	}
	if err := a.fetcher.Refresh(ctx, feed); err != nil {
		a.store.DeleteFeed(context.WithoutCancel(ctx), feed.ID)
		return Feed{}, err
	}
	return a.store.FeedByID(ctx, feed.ID)
}

func (a *API) subscriptionEdit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	action := r.Form.Get("ac")
	title := r.Form.Get("t")
	addLabel, _ := parseLabel(r.Form.Get("a"))
	removeLabel, _ := parseLabel(r.Form.Get("r"))

	for _, s := range r.Form["s"] {
		var feed Feed
		var err error
		if action == "subscribe" {
			feedURL := strings.TrimPrefix(s, "feed/")
			if !isHTTPURL(feedURL) {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			feed, err = a.subscribe(ctx, feedURL)
		} else {
			feed, err = a.resolveFeed(ctx, s)
		}
		if errors.Is(err, errNotFound) {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		if err != nil {
			a.serverError(w, err)
			return
		}

		if action == "unsubscribe" {
			err = a.store.DeleteFeed(ctx, feed.ID)
		} else {
			if title != "" {
				err = errors.Join(err, a.store.SetFeedTitle(ctx, feed.ID, title))
			}
			switch {
			case addLabel != "":
				err = errors.Join(err, a.store.SetFeedFolder(ctx, feed.ID, addLabel))
			case removeLabel != "" && removeLabel == feed.Folder:
				err = errors.Join(err, a.store.SetFeedFolder(ctx, feed.ID, ""))
			}
		}
		if err != nil {
			a.serverError(w, err)
			return
		}
	}
	writeText(w, "OK")
}

func (a *API) renameTag(w http.ResponseWriter, r *http.Request) {
	from, ok1 := parseLabel(r.Form.Get("s"))
	to, ok2 := parseLabel(r.Form.Get("dest"))
	if !ok1 || !ok2 || to == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	err := a.store.RenameFolder(r.Context(), from, to)
	if errors.Is(err, errNotFound) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if err != nil {
		a.serverError(w, err)
		return
	}
	writeText(w, "OK")
}

func (a *API) disableTag(w http.ResponseWriter, r *http.Request) {
	for _, s := range r.Form["s"] {
		name, ok := parseLabel(s)
		if !ok {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		err := a.store.DeleteFolder(r.Context(), name)
		if errors.Is(err, errDefaultFolderLock) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			a.serverError(w, err)
			return
		}
	}
	writeText(w, "OK")
}

func (a *API) unreadCount(w http.ResponseWriter, r *http.Request) {
	feeds, err := a.store.Feeds(r.Context())
	if err != nil {
		a.serverError(w, err)
		return
	}
	type countJSON struct {
		ID     string `json:"id"`
		Count  int    `json:"count"`
		Newest string `json:"newestItemTimestampUsec"`
	}
	type tally struct {
		count  int
		newest int64
	}
	add := func(t *tally, f Feed) {
		t.count += f.Unread
		t.newest = max(t.newest, f.NewestUsec)
	}
	toJSON := func(id string, t tally) countJSON {
		return countJSON{ID: id, Count: t.count, Newest: strconv.FormatInt(t.newest, 10)}
	}

	counts := []countJSON{}
	var total tally
	folders := map[string]*tally{}
	var folderOrder []string
	for _, f := range feeds {
		counts = append(counts, toJSON(feedStreamID(f.ID), tally{f.Unread, f.NewestUsec}))
		add(&total, f)
		if f.Folder != "" {
			t := folders[f.Folder]
			if t == nil {
				t = &tally{}
				folders[f.Folder] = t
				folderOrder = append(folderOrder, f.Folder)
			}
			add(t, f)
		}
	}
	for _, name := range folderOrder {
		counts = append(counts, toJSON(labelID(name), *folders[name]))
	}
	counts = append(counts, toJSON(streamReadingAll, total))
	writeJSON(w, map[string]any{"max": total.count, "unreadcounts": counts})
}

// --- handlers: streams and items ---

func (a *API) streamItemIDs(w http.ResponseWriter, r *http.Request) {
	q, err := a.streamQuery(r.Context(), r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	entries, continuation, err := a.store.ItemIDs(r.Context(), q)
	if err != nil {
		a.serverError(w, err)
		return
	}
	type refJSON struct {
		ID              string   `json:"id"`
		DirectStreamIDs []string `json:"directStreamIds"`
		TimestampUsec   string   `json:"timestampUsec"`
	}
	refs := make([]refJSON, 0, len(entries))
	for _, e := range entries {
		refs = append(refs, refJSON{
			ID:              strconv.FormatInt(e.ID, 10),
			DirectStreamIDs: []string{},
			TimestampUsec:   strconv.FormatInt(e.CrawledUsec, 10),
		})
	}
	resp := map[string]any{"itemRefs": refs}
	if continuation > 0 {
		resp["continuation"] = strconv.FormatInt(continuation, 10)
	}
	writeJSON(w, resp)
}

type itemJSON struct {
	ID            string      `json:"id"`
	CrawlTimeMsec string      `json:"crawlTimeMsec"`
	TimestampUsec string      `json:"timestampUsec"`
	Published     int64       `json:"published"`
	Updated       int64       `json:"updated"`
	Title         string      `json:"title"`
	Author        string      `json:"author,omitempty"`
	Canonical     []hrefJSON  `json:"canonical"`
	Alternate     []hrefJSON  `json:"alternate"`
	Categories    []string    `json:"categories"`
	Origin        originJSON  `json:"origin"`
	Summary       contentJSON `json:"summary"`
	Content       contentJSON `json:"content"`
}

type hrefJSON struct {
	Href string `json:"href"`
	Type string `json:"type,omitempty"`
}

type originJSON struct {
	StreamID string `json:"streamId"`
	Title    string `json:"title"`
	HTMLURL  string `json:"htmlUrl"`
}

type contentJSON struct {
	Content string `json:"content"`
}

func (a *API) streamItemContents(w http.ResponseWriter, r *http.Request) {
	ids, err := parseItemIDs(r.Form["i"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	entries, err := a.store.EntriesByIDs(r.Context(), ids)
	if err != nil {
		a.serverError(w, err)
		return
	}
	items := make([]itemJSON, 0, len(entries))
	for _, e := range entries {
		cats := []string{streamReadingAll}
		if e.Folder != "" {
			cats = append(cats, labelID(e.Folder))
		}
		if e.Read {
			cats = append(cats, stateID(stateRead))
		}
		if e.Starred {
			cats = append(cats, stateID(stateStarred))
		}
		links := []hrefJSON{}
		if e.URL != "" {
			links = append(links, hrefJSON{Href: e.URL, Type: "text/html"})
		}
		items = append(items, itemJSON{
			ID:            longItemID(e.ID),
			CrawlTimeMsec: strconv.FormatInt(e.CrawledUsec/1000, 10),
			TimestampUsec: strconv.FormatInt(e.CrawledUsec, 10),
			Published:     e.Published,
			Updated:       e.Published,
			Title:         e.Title,
			Author:        e.Author,
			Canonical:     links,
			Alternate:     links,
			Categories:    cats,
			Origin: originJSON{
				StreamID: feedStreamID(e.FeedID),
				Title:    e.FeedTitle,
				HTMLURL:  e.FeedSiteURL,
			},
			Summary: contentJSON{Content: e.Content},
			Content: contentJSON{Content: e.Content},
		})
	}
	writeJSON(w, map[string]any{
		"id":      streamReadingAll,
		"updated": time.Now().Unix(),
		"items":   items,
	})
}

func (a *API) editTag(w http.ResponseWriter, r *http.Request) {
	ids, err := parseItemIDs(r.Form["i"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	apply := func(tags []string, add bool) error {
		for _, tag := range tags {
			var err error
			switch parseState(tag) {
			case stateRead:
				err = a.store.SetFlag(ctx, ids, "is_read", add)
			case stateKeptUnread:
				if add {
					err = a.store.SetFlag(ctx, ids, "is_read", false)
				}
			case stateStarred:
				err = a.store.SetFlag(ctx, ids, "is_starred", add)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := errors.Join(apply(r.Form["a"], true), apply(r.Form["r"], false)); err != nil {
		a.serverError(w, err)
		return
	}
	writeText(w, "OK")
}

func (a *API) markAllAsRead(w http.ResponseWriter, r *http.Request) {
	q, err := a.streamQuery(r.Context(), url.Values{"s": {r.Form.Get("s")}})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ts := r.Form.Get("ts"); ts != "" {
		usec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			http.Error(w, "invalid ts", http.StatusBadRequest)
			return
		}
		q.UntilUsec = usec + 1
	}
	if err := a.store.MarkAllRead(r.Context(), q); err != nil {
		a.serverError(w, err)
		return
	}
	writeText(w, "OK")
}

// streamQuery translates the s, n, r, ot, nt, xt, it and c parameters.
func (a *API) streamQuery(ctx context.Context, v url.Values) (StreamQuery, error) {
	q := StreamQuery{Limit: 20, Oldest: v.Get("r") == "o"}
	if n, err := strconv.Atoi(v.Get("n")); err == nil && n > 0 {
		q.Limit = min(n, 10000)
	}
	if ot, err := strconv.ParseInt(v.Get("ot"), 10, 64); err == nil && ot > 0 {
		q.SinceUsec = ot * 1_000_000
	}
	if nt, err := strconv.ParseInt(v.Get("nt"), 10, 64); err == nil && nt > 0 {
		q.UntilUsec = nt * 1_000_000
	}
	if c := v.Get("c"); c != "" {
		cont, err := strconv.ParseInt(c, 10, 64)
		if err != nil {
			return q, fmt.Errorf("invalid continuation")
		}
		q.Continuation = cont
	}

	s := v.Get("s")
	if s == "" {
		s = streamReadingAll
	}
	switch {
	case strings.HasPrefix(s, "feed/"):
		feed, err := a.resolveFeed(ctx, s)
		switch {
		case errors.Is(err, errNotFound):
			q.FeedID = -1
		case err != nil:
			return q, err
		default:
			q.FeedID = feed.ID
		}
	case isLabel(s):
		q.Folder, _ = parseLabel(s)
	default:
		switch parseState(s) {
		case stateReadingList:
		case stateRead:
			q.OnlyRead = true
		case stateStarred:
			q.OnlyStarred = true
		default:
			return q, fmt.Errorf("unsupported stream %q", s)
		}
	}

	for _, xt := range v["xt"] {
		switch parseState(xt) {
		case stateRead:
			q.ExcludeRead = true
		case stateStarred:
			q.ExcludeStarred = true
		}
	}
	for _, it := range v["it"] {
		switch parseState(it) {
		case stateRead:
			q.OnlyRead = true
		case stateStarred:
			q.OnlyStarred = true
		}
	}
	return q, nil
}

func (a *API) resolveFeed(ctx context.Context, streamID string) (Feed, error) {
	ref := strings.TrimPrefix(streamID, "feed/")
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return a.store.FeedByID(ctx, id)
	}
	return a.store.FeedByURL(ctx, ref)
}

func (a *API) serverError(w http.ResponseWriter, err error) {
	a.log.Error("request failed", "err", err)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}

// --- ids and stream names ---

func feedStreamID(id int64) string { return "feed/" + strconv.FormatInt(id, 10) }
func labelID(name string) string   { return "user/-/label/" + name }
func stateID(state string) string  { return "user/-/state/com.google/" + state }

// longItemID renders the hex form NetNewsWire expects in item contents.
func longItemID(id int64) string { return fmt.Sprintf("%s%016x", itemIDPrefix, uint64(id)) }

// parseItemID accepts the long hex form and the short decimal form.
func parseItemID(s string) (int64, error) {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		u, err := strconv.ParseUint(s[i+1:], 16, 64)
		return int64(u), err
	}
	return strconv.ParseInt(s, 10, 64)
}

func parseItemIDs(values []string) ([]int64, error) {
	ids := make([]int64, 0, len(values))
	for _, v := range values {
		id, err := parseItemID(v)
		if err != nil {
			return nil, fmt.Errorf("invalid item id %q", v)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// splitUserPath strips "user/<id>/" and returns the remainder, e.g. "label/Tech".
func splitUserPath(s string) (string, bool) {
	rest, ok := strings.CutPrefix(s, "user/")
	if !ok {
		return "", false
	}
	_, rest, ok = strings.Cut(rest, "/")
	return rest, ok
}

func isLabel(s string) bool {
	_, ok := parseLabel(s)
	return ok
}

func parseLabel(s string) (string, bool) {
	rest, ok := splitUserPath(s)
	if !ok {
		return "", false
	}
	return strings.CutPrefix(rest, "label/")
}

func parseState(s string) string {
	rest, ok := splitUserPath(s)
	if !ok {
		return ""
	}
	state, _ := strings.CutPrefix(rest, "state/com.google/")
	return state
}

// --- helpers ---

func displayTitle(f Feed) string {
	if f.Title != "" {
		return f.Title
	}
	return f.URL
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func writeText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Write([]byte(s))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}
