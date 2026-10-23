package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

// runFeeds implements "greaderd feeds [-failing]": it prints the
// subscriptions and their fetch status, so broken feeds can be found without
// a web interface. It opens the database directly, which SQLite allows while
// the server is running.
func runFeeds(ctx context.Context, getenv func(string) string, args []string, out io.Writer) (err error) {
	fs := flag.NewFlagSet("feeds", flag.ContinueOnError)
	fs.SetOutput(out)
	failing := fs.Bool("failing", false, "only list feeds whose last fetch failed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	dbPath := getenv("GREADERD_DB")
	if dbPath == "" {
		dbPath = defaultDBPath
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()

	feeds, err := st.Feeds(ctx)
	if err != nil {
		return err
	}
	return printFeeds(out, feeds, *failing, time.Now())
}

// printFeeds writes feeds as a table.
func printFeeds(w io.Writer, feeds []store.Feed, failingOnly bool, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tURL\tUNREAD\tLAST FETCH\tNEXT FETCH\tFAILURES\tLAST ERROR")
	for _, f := range feeds {
		if failingOnly && f.ErrorCount == 0 {
			continue
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\t%d\t%s\n",
			f.ID, truncate(f.Title, 30), truncate(f.URL, 50), f.Unread,
			since(f.LastFetchedAt, now), until(f.NextFetchAt, now),
			f.ErrorCount, truncate(f.LastError, 60))
	}
	return tw.Flush()
}

func since(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return short(now.Sub(t)) + " ago"
}

func until(t, now time.Time) string {
	if !t.After(now) {
		return "due"
	}
	return "in " + short(t.Sub(now))
}

// short formats d coarsely, which is all a status table needs.
func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

// truncate shortens s to at most n runes, marking the cut with "…".
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
