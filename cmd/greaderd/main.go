// Command greaderd is a headless feed aggregator that speaks the Google Reader
// API as implemented by FreshRSS, so clients like NetNewsWire can sync with it.
//
// Usage:
//
//	greaderd              run the server
//	greaderd healthcheck  exit 0 if the server on GREADERD_ADDR is healthy
//
// It is configured through environment variables:
//
//	GREADERD_USER                 username for ClientLogin (required)
//	GREADERD_PASSWORD             password for ClientLogin (required)
//	GREADERD_ADDR                 listen address (default ":8080")
//	GREADERD_DB                   path to the SQLite database (default "greaderd.db")
//	GREADERD_REFRESH_INTERVAL     time between two fetches of a feed (default "15m")
//	GREADERD_RETENTION            keep read entries this long after their feed dropped them,
//	                              e.g. "90d" or "720h"; "0" keeps them forever (default "90d")
//	GREADERD_TRUST_PROXY_HEADERS  take client addresses from X-Forwarded-For (default false)
//	GREADERD_ALLOW_PRIVATE_FEEDS  allow feeds on private networks (default false)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/fetcher"
	"github.com/Dennis-Kluge/greaderd/internal/greader"
	"github.com/Dennis-Kluge/greaderd/internal/store"
)

const (
	defaultAddr = ":8080"

	shutdownTimeout = 10 * time.Second

	// Server timeouts. Writes may take as long as subscribing to a feed,
	// which waits for its first fetch.
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
)

type config struct {
	addr              string
	dbPath            string
	creds             greader.Credentials
	refreshInterval   time.Duration
	retention         time.Duration
	trustProxyHeaders bool
	allowPrivateFeeds bool
}

func main() {
	os.Exit(runMain(os.Args[1:]))
}

// runMain returns the exit code, so deferred calls run before os.Exit.
func runMain(args []string) int {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(args) > 0 {
		if args[0] != "healthcheck" || len(args) > 1 {
			log.Error("usage: greaderd [healthcheck]")
			return 2
		}
		if err := healthcheck(os.Getenv); err != nil {
			log.Error("unhealthy", "err", err)
			return 1
		}
		return 0
	}

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
		addr:   envOr("GREADERD_ADDR", defaultAddr),
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
	if cfg.retention, err = parseRetention(envOr("GREADERD_RETENTION", "90d")); err != nil {
		return config{}, err
	}
	if cfg.trustProxyHeaders, err = parseBool(getenv, "GREADERD_TRUST_PROXY_HEADERS"); err != nil {
		return config{}, err
	}
	if cfg.allowPrivateFeeds, err = parseBool(getenv, "GREADERD_ALLOW_PRIVATE_FEEDS"); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// parseRetention parses a duration that may also be given in days ("90d"),
// since time.ParseDuration stops at hours. "0" disables pruning.
func parseRetention(v string) (time.Duration, error) {
	invalid := fmt.Errorf(`GREADERD_RETENTION must be a duration such as "90d" or "720h", or "0", not %q`, v)
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, invalid
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, invalid
	}
	return d, nil
}

// parseBool reads an optional boolean variable; unset means false.
func parseBool(getenv func(string) string, key string) (bool, error) {
	v := getenv(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, not %q", key, v)
	}
	return b, nil
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

	f := fetcher.New(st, fetcher.Options{
		Interval:             cfg.refreshInterval,
		Retention:            cfg.retention,
		AllowPrivateNetworks: cfg.allowPrivateFeeds,
		Logger:               log,
	})
	wg.Go(func() { f.Run(ctx) })

	api := greader.New(st, f, cfg.creds, greader.Options{
		TrustProxyHeaders: cfg.trustProxyHeaders,
		Logger:            log,
	})
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	log.Info("listening", "addr", cfg.addr, "db", cfg.dbPath, "refresh", cfg.refreshInterval, "retention", cfg.retention,
		"trust_proxy_headers", cfg.trustProxyHeaders, "allow_private_feeds", cfg.allowPrivateFeeds)
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

// healthcheck asks the local server for its health. It exists because the
// distroless container image has no curl or wget for a HEALTHCHECK.
func healthcheck(getenv func(string) string) error {
	addr := getenv("GREADERD_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	url, err := healthURL(addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return nil
}

// healthURL returns the health check URL for a listen address; an empty or
// unspecified host means the server listens on loopback too.
func healthURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid GREADERD_ADDR %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
