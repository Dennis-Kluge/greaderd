package fetcher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

func TestFindIconLink(t *testing.T) {
	tests := []struct {
		name string
		page string
		want string
	}{
		{"favicon", `<head><link rel="icon" href="/favicon.png"></head>`, "/favicon.png"},
		{"shortcut icon", `<head><link rel="shortcut icon" href="/favicon.ico"></head>`, "/favicon.ico"},
		{
			"apple touch icon wins",
			`<head><link rel="icon" href="/small.png"><link rel="apple-touch-icon" href="/large.png"></head>`,
			"/large.png",
		},
		{
			"first of equal rank",
			`<head><link rel="icon" sizes="16x16" href="/16.png"><link rel="icon" sizes="32x32" href="/32.png"></head>`,
			"/16.png",
		},
		{"mask icons are ignored", `<head><link rel="mask-icon" href="/mask.svg"></head>`, ""},
		{"no icon", `<head><link rel="stylesheet" href="/s.css"></head>`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findIconLink([]byte(tt.page)); got != tt.want {
				t.Errorf("findIconLink = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDiscoverIcon(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/declared/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html><head><link rel="apple-touch-icon" href="touch.png"></head></html>`)
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Write([]byte{0, 0, 1, 0})
	})
	mux.HandleFunc("/plain/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html><head></head></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := newTestFetcher(&fakeStore{})
	ctx := context.Background()
	if got, err := f.discoverIcon(ctx, srv.URL+"/declared/"); err != nil || got != srv.URL+"/declared/touch.png" {
		t.Errorf("declared icon = %q, %v", got, err)
	}
	if got, err := f.discoverIcon(ctx, srv.URL+"/plain/"); err != nil || got != srv.URL+"/favicon.ico" {
		t.Errorf("favicon fallback = %q, %v", got, err)
	}

	noFavicon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html><head></head></html>`) // also for /favicon.ico: not an image
	}))
	defer noFavicon.Close()
	if got, err := f.discoverIcon(ctx, noFavicon.URL); !errors.Is(err, errNoIcon) {
		t.Errorf("no icon = %q, %v; want errNoIcon", got, err)
	}
}

func TestRefreshLooksForIconsWeekly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed.xml":
			io.WriteString(w, testRSS)
		default:
			io.WriteString(w, `<html><head><link rel="icon" href="/icon.png"></head></html>`)
		}
	}))
	defer srv.Close()

	tests := []struct {
		name        string
		checkedAt   time.Time
		previous    string
		wantChecked bool
	}{
		{name: "never checked", wantChecked: true},
		{name: "checked recently", checkedAt: time.Now().Add(-time.Hour), previous: "https://old/icon"},
		{name: "checked long ago", checkedAt: time.Now().Add(-8 * 24 * time.Hour), previous: "https://old/icon", wantChecked: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &fakeStore{}
			feed := store.Feed{ID: 1, URL: srv.URL + "/feed.xml", IconURL: tt.previous, IconCheckedAt: tt.checkedAt}
			if err := newTestFetcher(s).Refresh(context.Background(), feed); err != nil {
				t.Fatal(err)
			}
			if !tt.wantChecked {
				if len(s.icons) != 0 {
					t.Errorf("icon looked up again: %v", s.icons)
				}
				return
			}
			if len(s.icons) != 1 || s.icons[0] != srv.URL+"/icon.png" {
				t.Errorf("icons = %v, want [%s/icon.png]", s.icons, srv.URL)
			}
		})
	}
}
