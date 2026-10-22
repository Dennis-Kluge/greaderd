package greader

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Dennis-Kluge/greaderd/internal/store"
)

// apiFunc is an HTTP handler that reports failures as errors; serve turns
// them into responses.
type apiFunc func(w http.ResponseWriter, r *http.Request) error

// statusError is an error whose status and message are sent to the client.
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

func errorf(status int, format string, args ...any) error {
	return &statusError{status: status, msg: fmt.Sprintf(format, args...)}
}

// serve adapts h to http.Handler. Errors without a status are logged and
// answered with 500, so internal details never reach the client.
func (s *Server) serve(h apiFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := h(w, r)
		if err == nil {
			return
		}
		var se *statusError
		switch {
		case errors.As(err, &se):
			http.Error(w, se.msg, se.status)
		case errors.Is(err, store.ErrNotFound):
			http.Error(w, "Not Found", http.StatusNotFound)
		case errors.Is(err, store.ErrDefaultFolder):
			http.Error(w, "The default folder cannot be deleted", http.StatusBadRequest)
		default:
			s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	})
}

// writeJSON sends v as JSON. It encodes into a buffer first, so an encoding
// error still becomes a 500 instead of a truncated body.
func writeJSON(w http.ResponseWriter, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Like FreshRSS, which does not escape HTML characters either.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	writeBody(w, buf.Bytes())
	return nil
}

// writeText sends s as plain text, which the protocol uses for "OK" and tokens.
func writeText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	writeBody(w, []byte(s))
}

// writeBody writes a response body. A failed write means the client has gone
// away, so there is nobody left to report the error to.
func writeBody(w http.ResponseWriter, b []byte) {
	_, _ = w.Write(b)
}

// logRequests logs method, path, status and duration of every request except
// health checks, which would drown everything else. It never logs query
// strings or bodies, which may carry credentials.
func (s *Server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == healthPath {
			h.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond))
	})
}

// statusRecorder remembers the status code written to a ResponseWriter.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
