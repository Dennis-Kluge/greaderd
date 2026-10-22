// Command greaderd is a headless feed aggregator that speaks the Google Reader
// API as implemented by FreshRSS, so clients like NetNewsWire can sync with it.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	addr := env("GREADERD_ADDR", ":8080")
	dbPath := env("GREADERD_DB", "greaderd.db")
	user := os.Getenv("GREADERD_USER")
	password := os.Getenv("GREADERD_PASSWORD")
	if user == "" || password == "" {
		return errors.New("GREADERD_USER and GREADERD_PASSWORD must be set")
	}
	interval, err := time.ParseDuration(env("GREADERD_REFRESH_INTERVAL", "15m"))
	if err != nil {
		return err
	}

	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fetcher := NewFetcher(store, interval, log)
	go fetcher.Run(ctx)

	srv := &http.Server{
		Addr:              addr,
		Handler:           NewAPI(store, fetcher, user, password, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Info("listening", "addr", addr, "db", dbPath, "refresh", interval)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
