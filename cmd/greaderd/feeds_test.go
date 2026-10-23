package main

import (
	"bytes"
	"errors"
	"flag"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

func TestPrintFeeds(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	feeds := []store.Feed{
		{
			ID: 1, Title: "Healthy", URL: "https://a.example/feed", Unread: 3,
			LastFetchedAt: now.Add(-5 * time.Minute), NextFetchAt: now.Add(10 * time.Minute),
		},
		{
			ID: 2, Title: "Broken", URL: "https://b.example/feed",
			LastFetchedAt: now.Add(-3 * time.Hour), NextFetchAt: now.Add(-time.Minute),
			ErrorCount: 4, LastError: "GET https://b.example/feed: unexpected status 404 Not Found",
		},
		{ID: 3, URL: "https://c.example/" + strings.Repeat("x", 80)},
	}

	var all bytes.Buffer
	if err := printFeeds(&all, feeds, false, now); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(all.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines:\n%s", len(lines), all.String())
	}
	for _, want := range []string{"5m ago", "in 10m", "3h ago", "due", "never", "404 Not Found", "…"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, all.String())
		}
	}

	var failing bytes.Buffer
	if err := printFeeds(&failing, feeds, true, now); err != nil {
		t.Fatal(err)
	}
	if out := failing.String(); !strings.Contains(out, "Broken") || strings.Contains(out, "Healthy") {
		t.Errorf("-failing output:\n%s", out)
	}
}

func TestShort(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "<1m"},
		{15 * time.Minute, "15m"},
		{90 * time.Minute, "1h"},
		{47 * time.Hour, "47h"},
		{72 * time.Hour, "3d"},
	}
	for _, tt := range tests {
		if got := short(tt.d); got != tt.want {
			t.Errorf("short(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestRunFeeds(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	feed, _, err := st.AddFeed(t.Context(), "https://a.example/feed")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFailed(t.Context(), feed.ID, time.Now(), time.Now().Add(time.Hour), errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "GREADERD_DB" {
			return dbPath
		}
		return ""
	}

	var out bytes.Buffer
	if err := runFeeds(t.Context(), getenv, []string{"-failing"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "https://a.example/feed") || !strings.Contains(out.String(), "boom") {
		t.Errorf("output:\n%s", out.String())
	}

	if err := runFeeds(t.Context(), getenv, []string{"-h"}, &out); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
	if err := runFeeds(t.Context(), getenv, []string{"extra"}, &out); err == nil {
		t.Error("extra argument accepted")
	}
}
