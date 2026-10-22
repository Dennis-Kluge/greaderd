// Package opml reads and writes OPML subscription lists.
package opml

import (
	"encoding/xml"
	"fmt"
	"io"
)

// Subscription is a feed listed in an OPML document.
type Subscription struct {
	Title   string
	FeedURL string
	SiteURL string
	// Folder is the name of the enclosing outline; empty at the top level.
	Folder string
}

type document struct {
	XMLName xml.Name  `xml:"opml"`
	Version string    `xml:"version,attr"`
	Title   string    `xml:"head>title"`
	Body    []outline `xml:"body>outline"`
}

type outline struct {
	Text     string    `xml:"text,attr"`
	Title    string    `xml:"title,attr,omitempty"`
	Type     string    `xml:"type,attr,omitempty"`
	XMLURL   string    `xml:"xmlUrl,attr,omitempty"`
	HTMLURL  string    `xml:"htmlUrl,attr,omitempty"`
	Outlines []outline `xml:"outline"`
}

// Parse reads the subscriptions of an OPML document. Outlines without an
// xmlUrl are folders. Google Reader has only one folder level, so feeds in
// nested folders are assigned to their top-level folder.
func Parse(r io.Reader) ([]Subscription, error) {
	var doc document
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse OPML: %w", err)
	}
	var subs []Subscription
	var walk func(outlines []outline, folder string)
	walk = func(outlines []outline, folder string) {
		for _, o := range outlines {
			if o.XMLURL == "" {
				name := folder
				if name == "" {
					name = firstNonEmpty(o.Text, o.Title)
				}
				walk(o.Outlines, name)
				continue
			}
			subs = append(subs, Subscription{
				Title:   firstNonEmpty(o.Title, o.Text),
				FeedURL: o.XMLURL,
				SiteURL: o.HTMLURL,
				Folder:  folder,
			})
		}
	}
	walk(doc.Body, "")
	return subs, nil
}

// Write renders subs as an OPML 2.0 document. Subscriptions are grouped by
// folder in order of first appearance.
func Write(w io.Writer, title string, subs []Subscription) error {
	doc := document{Version: "2.0", Title: title}
	folderIndex := map[string]int{}
	for _, s := range subs {
		o := outline{Text: s.Title, Title: s.Title, Type: "rss", XMLURL: s.FeedURL, HTMLURL: s.SiteURL}
		if s.Folder == "" {
			doc.Body = append(doc.Body, o)
			continue
		}
		i, ok := folderIndex[s.Folder]
		if !ok {
			i = len(doc.Body)
			folderIndex[s.Folder] = i
			doc.Body = append(doc.Body, outline{Text: s.Folder, Title: s.Folder})
		}
		doc.Body[i].Outlines = append(doc.Body[i].Outlines, o)
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return fmt.Errorf("write OPML: %w", err)
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("write OPML: %w", err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
