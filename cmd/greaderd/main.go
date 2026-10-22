// Command greaderd is a headless feed aggregator that speaks the Google Reader
// API as implemented by FreshRSS, so clients like NetNewsWire can sync with it.
//
// It is configured through environment variables:
//
//	GREADERD_USER              username for ClientLogin (required)
//	GREADERD_PASSWORD          password for ClientLogin (required)
//	GREADERD_ADDR              listen address (default ":8080")
//	GREADERD_DB                path to the SQLite database (default "greaderd.db")
//	GREADERD_REFRESH_INTERVAL  time between two fetches of a feed (default "15m")
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/fetcher"
	"github.com/Dennis-Kluge/greaderd/internal/greader"
	"github.com/Dennis-Kluge/greaderd/internal/store"
)

const shutdownTimeout = 10 * time.Second

type config struct {
	addr            string
	dbPath          string
	creds           greader.Credentials
	refreshInterval time.Duration
}

func main() {
	os.Exit(runMain())
}

// runMain returns the exit code, so deferred calls run before os.Exit.
func runMain() int {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, log); err != nil {
		log.Error("fatal", "err", err)
		return 1
	}
	return 0
}

// loadConfig reads the configuration through getenv.
func loadConfig(getenv func(string) string) (config, error) {
	envOr := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	cfg := config{
		addr:   envOr("GREADERD_ADDR", ":8080"),
		dbPath: envOr("GREADERD_DB", "greaderd.db"),
		creds: greader.Credentials{
			User:     getenv("GREADERD_USER"),
			Password: getenv("GREADERD_PASSWORD"),
		},
	}
	if cfg.creds.User == "" || cfg.creds.Password == "" {
		return config{}, errors.New("GREADERD_USER and GREADERD_PASSWORD must be set")
	}
	interval, err := time.ParseDuration(envOr("GREADERD_REFRESH_INTERVAL", "15m"))
	if err != nil || interval <= 0 {
		return config{}, errors.New("GREADERD_REFRESH_INTERVAL must be a positive duration such as 15m")
	}
	cfg.refreshInterval = interval
	return cfg, nil
}

// run serves the API and refreshes feeds until ctx is cancelled.
func run(ctx context.Context, cfg config, log *slog.Logger) error {
	st, err := store.Open(ctx, cfg.dbPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("close database", "err", err)
		}
	}()

	// Deferred in reverse: stop the fetcher, wait for it, then close the store.
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	f := fetcher.New(st, fetcher.Options{Interval: cfg.refreshInterval, Logger: log})
	wg.Go(func() { f.Run(ctx) })

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           greader.New(st, f, cfg.creds, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Info("listening", "addr", cfg.addr, "db", cfg.dbPath, "refresh", cfg.refreshInterval)
	return serve(ctx, srv)
}

// serve runs srv until ctx is cancelled and then shuts it down gracefully.
func serve(ctx context.Context, srv *http.Server) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down server: %w", err)
	}
	return nil
}
