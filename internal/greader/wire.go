package greader

// Wire types of the Google Reader API. Field names and JSON tags follow the
// responses of FreshRSS; NetNewsWire's decoders (ReaderAPI*.swift) define
// which fields are required.

type userInfoResponse struct {
	UserID        string `json:"userId"`
	UserName      string `json:"userName"`
	UserProfileID string `json:"userProfileId"`
	UserEmail     string `json:"userEmail"`
}

type tagListResponse struct {
	Tags []tag `json:"tags"`
}

type tag struct {
	ID          string `json:"id"`
	Type        string `json:"type,omitempty"`
	UnreadCount *int   `json:"unread_count,omitempty"`
}

type subscriptionListResponse struct {
	Subscriptions []subscription `json:"subscriptions"`
}

type subscription struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Categories []category `json:"categories"`
	URL        string     `json:"url"`
	HTMLURL    string     `json:"htmlUrl"`
	IconURL    string     `json:"iconUrl"`
}

type category struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type quickAddResponse struct {
	NumResults int    `json:"numResults"`
	Query      string `json:"query"`
	StreamID   string `json:"streamId,omitempty"`
	StreamName string `json:"streamName,omitempty"`
	Error      string `json:"error,omitempty"`
}

type unreadCountResponse struct {
	Max          int           `json:"max"`
	UnreadCounts []unreadCount `json:"unreadcounts"`
}

type unreadCount struct {
	ID                      string `json:"id"`
	Count                   int    `json:"count"`
	NewestItemTimestampUsec string `json:"newestItemTimestampUsec"`
}

type itemIDsResponse struct {
	ItemRefs     []itemRef `json:"itemRefs"`
	Continuation string    `json:"continuation,omitempty"`
}

type itemRef struct {
	ID              string   `json:"id"` // decimal
	DirectStreamIDs []string `json:"directStreamIds"`
	TimestampUsec   string   `json:"timestampUsec"`
}

type streamContentsResponse struct {
	ID      string `json:"id"`
	Updated int64  `json:"updated"` // unix seconds
	Items   []item `json:"items"`
}

type item struct {
	ID            string   `json:"id"`            // long hex form
	CrawlTimeMsec string   `json:"crawlTimeMsec"` // unix milliseconds
	TimestampUsec string   `json:"timestampUsec"` // unix microseconds
	Published     int64    `json:"published"`     // unix seconds
	Updated       int64    `json:"updated"`       // unix seconds
	Title         string   `json:"title"`
	Author        string   `json:"author,omitempty"`
	Canonical     []link   `json:"canonical"`
	Alternate     []link   `json:"alternate"`
	Categories    []string `json:"categories"`
	Origin        origin   `json:"origin"`
	Summary       content  `json:"summary"`
	Content       content  `json:"content"`
}

type link struct {
	Href string `json:"href"`
	Type string `json:"type,omitempty"`
}

type origin struct {
	StreamID string `json:"streamId"`
	Title    string `json:"title"`
	HTMLURL  string `json:"htmlUrl"`
}

type content struct {
	Content string `json:"content"`
}
