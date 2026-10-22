package main

import (
	"encoding/xml"
	"net/http"
)

type opmlDoc struct {
	XMLName xml.Name      `xml:"opml"`
	Version string        `xml:"version,attr"`
	Title   string        `xml:"head>title"`
	Body    []opmlOutline `xml:"body>outline"`
}

type opmlOutline struct {
	Text     string        `xml:"text,attr"`
	Title    string        `xml:"title,attr,omitempty"`
	Type     string        `xml:"type,attr,omitempty"`
	XMLURL   string        `xml:"xmlUrl,attr,omitempty"`
	HTMLURL  string        `xml:"htmlUrl,attr,omitempty"`
	Outlines []opmlOutline `xml:"outline"`
}

func (a *API) subscriptionImport(w http.ResponseWriter, r *http.Request) {
	var doc opmlDoc
	if err := xml.NewDecoder(r.Body).Decode(&doc); err != nil {
		http.Error(w, "invalid OPML", http.StatusBadRequest)
		return
	}
	// Feeds are only registered here; the fetcher picks them up within a minute.
	var walk func(outlines []opmlOutline, folder string) error
	walk = func(outlines []opmlOutline, folder string) error {
		for _, o := range outlines {
			if o.XMLURL == "" {
				name := o.Text
				if name == "" {
					name = o.Title
				}
				// OPML allows deeper nesting; Google Reader only has one folder level.
				if folder != "" {
					name = folder
				}
				if err := walk(o.Outlines, name); err != nil {
					return err
				}
				continue
			}
			if !isHTTPURL(o.XMLURL) {
				continue
			}
			feed, created, err := a.store.AddFeed(r.Context(), o.XMLURL)
			if err != nil {
				return err
			}
			if !created {
				continue
			}
			if title := firstNonEmpty(o.Title, o.Text); title != "" {
				if err := a.store.SetFeedTitle(r.Context(), feed.ID, title); err != nil {
					return err
				}
			}
			if folder != "" {
				if err := a.store.SetFeedFolder(r.Context(), feed.ID, folder); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(doc.Body, ""); err != nil {
		a.serverError(w, err)
		return
	}
	writeText(w, "OK")
}

func (a *API) subscriptionExport(w http.ResponseWriter, r *http.Request) {
	feeds, err := a.store.Feeds(r.Context())
	if err != nil {
		a.serverError(w, err)
		return
	}
	doc := opmlDoc{Version: "2.0", Title: "greaderd subscriptions"}
	folderIndex := map[string]int{}
	for _, f := range feeds {
		o := opmlOutline{Text: displayTitle(f), Title: displayTitle(f), Type: "rss", XMLURL: f.URL, HTMLURL: f.SiteURL}
		if f.Folder == "" {
			doc.Body = append(doc.Body, o)
			continue
		}
		i, ok := folderIndex[f.Folder]
		if !ok {
			i = len(doc.Body)
			folderIndex[f.Folder] = i
			doc.Body = append(doc.Body, opmlOutline{Text: f.Folder, Title: f.Folder})
		}
		doc.Body[i].Outlines = append(doc.Body[i].Outlines, o)
	}

	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.Header().Set("Content-Disposition", `attachment; filename="greaderd.opml"`)
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		a.log.Error("encode OPML", "err", err)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
