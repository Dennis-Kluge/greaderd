// Package greader implements the Google Reader API as offered by FreshRSS
// (p/api/greader.php), limited to what NetNewsWire and similar clients use.
//
// The protocol has no formal specification. Behaviour follows FreshRSS and
// NetNewsWire's ReaderAPI client; comments name the client behaviour where a
// detail exists only for compatibility.
package greader

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

const (
	// maxBodyBytes limits request bodies, including OPML imports.
	maxBodyBytes = 4 << 20

	// subscribeTimeout bounds feed discovery and the first fetch on subscribe.
	subscribeTimeout = 30 * time.Second
)

// Store is the persistence the API needs.
type Store interface {
	Folders(ctx context.Context) ([]store.Folder, error)
	RenameFolder(ctx context.Context, from, to string) error
	DeleteFolder(ctx context.Context, name string) error

	Feeds(ctx context.Context) ([]store.Feed, error)
	FeedByID(ctx context.Context, id int64) (store.Feed, error)
	FeedByURL(ctx context.Context, url string) (store.Feed, error)
	AddFeed(ctx context.Context, url string) (feed store.Feed, created bool, err error)
	DeleteFeed(ctx context.Context, id int64) error
	SetFeedTitle(ctx context.Context, id int64, title string) error
	SetFeedFolder(ctx context.Context, id int64, folder string) error

	ItemIDs(ctx context.Context, q store.Query) (refs []store.ItemRef, continuation int64, err error)
	Entries(ctx context.Context, ids []int64) ([]store.Entry, error)
	SetRead(ctx context.Context, ids []int64, read bool) error
	SetStarred(ctx context.Context, ids []int64, starred bool) error
	MarkAllRead(ctx context.Context, q store.Query) error
}

// Fetcher finds and fetches feeds when a client subscribes.
type Fetcher interface {
	Discover(ctx context.Context, rawURL string) (string, error)
	Refresh(ctx context.Context, feed store.Feed) error
}

// Credentials are the login of the single user.
type Credentials struct {
	User     string
	Password string
}

// Server serves the Google Reader API.
type Server struct {
	store      Store
	fetcher    Fetcher
	creds      Credentials
	authToken  string // value of "Authorization: GoogleLogin auth=..."
	writeToken string // value of the T parameter
	log        *slog.Logger
}

// New returns a Server for the given user.
func New(s Store, f Fetcher, creds Credentials, log *slog.Logger) *Server {
	authToken, writeToken := deriveTokens(creds)
	return &Server{
		store:      s,
		fetcher:    f,
		creds:      creds,
		authToken:  authToken,
		writeToken: writeToken,
		log:        log,
	}
}

// Handler returns the HTTP handler for all API routes. Every route is served
// both at the root and under /api/greader.php, where clients configured for
// FreshRSS expect it.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	public := func(pattern string, h apiFunc) { mux.Handle(pattern, s.serve(h)) }
	read := func(pattern string, h apiFunc) { mux.Handle(pattern, s.serve(s.requireAuth(h))) }
	write := func(pattern string, h apiFunc) { mux.Handle(pattern, s.serve(s.requireWriteToken(h))) }

	public("POST /accounts/ClientLogin", s.clientLogin)

	read("GET /reader/api/0/token", s.token)
	read("GET /reader/api/0/user-info", s.userInfo)
	read("GET /reader/api/0/tag/list", s.tagList)
	read("GET /reader/api/0/subscription/list", s.subscriptionList)
	read("GET /reader/api/0/subscription/export", s.subscriptionExport)
	read("GET /reader/api/0/unread-count", s.unreadCount)
	read("GET /reader/api/0/stream/items/ids", s.streamItemIDs)
	// NetNewsWire sends no T token with OPML imports.
	read("POST /reader/api/0/subscription/import", s.subscriptionImport)

	write("POST /reader/api/0/subscription/quickadd", s.quickAdd)
	write("POST /reader/api/0/subscription/edit", s.subscriptionEdit)
	write("POST /reader/api/0/rename-tag", s.renameTag)
	write("POST /reader/api/0/disable-tag", s.disableTag)
	write("POST /reader/api/0/stream/items/contents", s.streamItemContents)
	write("POST /reader/api/0/edit-tag", s.editTag)
	write("POST /reader/api/0/mark-all-as-read", s.markAllAsRead)

	root := http.NewServeMux()
	root.Handle("/api/greader.php/", http.StripPrefix("/api/greader.php", mux))
	root.Handle("/", mux)
	return s.logRequests(root)
}
