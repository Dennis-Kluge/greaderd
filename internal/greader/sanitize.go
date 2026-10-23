package greader

import (
	"regexp"

	"github.com/microcosm-cc/bluemonday"
)

// embedSource matches the video players whose iframes are allowed in content.
var embedSource = regexp.MustCompile(`^https://(www\.youtube(-nocookie)?\.com/embed/|player\.vimeo\.com/video/)`)

// newHTMLPolicy returns the policy that article content is sanitized with
// before it is sent to clients. Feeds are third-party HTML; not every client
// renders it as carefully as NetNewsWire.
//
// It extends bluemonday's policy for user-generated content, which keeps
// formatting, links, images and tables but drops scripts, styles, event
// handlers and forms, by the video embeds that are common in feeds.
// Sanitizing on output rather than when storing means a stricter or looser
// policy also applies to entries that are already stored.
func newHTMLPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowElements("iframe")
	p.AllowAttrs("src").Matching(embedSource).OnElements("iframe")
	p.AllowAttrs("width", "height").Matching(bluemonday.Number).OnElements("iframe")
	p.AllowAttrs("allowfullscreen").OnElements("iframe")
	p.AllowAttrs("loading").Matching(regexp.MustCompile(`^(lazy|eager)$`)).OnElements("img", "iframe")
	return p
}
