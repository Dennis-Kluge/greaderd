package fetcher

import (
	"net/http"
	"testing"
	"time"
)

func TestSuccessDelay(t *testing.T) {
	tests := []struct {
		interval, maxAge, want time.Duration
	}{
		{15 * time.Minute, 0, 15 * time.Minute},
		{15 * time.Minute, time.Minute, 15 * time.Minute},
		{15 * time.Minute, 2 * time.Hour, 2 * time.Hour},
		{15 * time.Minute, 48 * time.Hour, maxDelay},
		{48 * time.Hour, 0, 48 * time.Hour},
		{48 * time.Hour, 72 * time.Hour, 48 * time.Hour},
	}
	for _, tt := range tests {
		if got := successDelay(tt.interval, tt.maxAge); got != tt.want {
			t.Errorf("successDelay(%v, %v) = %v, want %v", tt.interval, tt.maxAge, got, tt.want)
		}
	}
}

func TestFailureDelay(t *testing.T) {
	tests := []struct {
		interval   time.Duration
		failures   int
		retryAfter time.Duration
		want       time.Duration
	}{
		{15 * time.Minute, 1, 0, 15 * time.Minute},
		{15 * time.Minute, 2, 0, 30 * time.Minute},
		{15 * time.Minute, 5, 0, 4 * time.Hour},
		{15 * time.Minute, 7, 0, 16 * time.Hour},
		{15 * time.Minute, 8, 0, maxDelay},
		{15 * time.Minute, 1000, 0, maxDelay},
		{15 * time.Minute, 1, 2 * time.Hour, 2 * time.Hour},
		{15 * time.Minute, 5, time.Hour, 4 * time.Hour},
		{15 * time.Minute, 1, 72 * time.Hour, maxDelay},
		{48 * time.Hour, 3, 0, 48 * time.Hour},
	}
	for _, tt := range tests {
		if got := failureDelay(tt.interval, tt.failures, tt.retryAfter); got != tt.want {
			t.Errorf("failureDelay(%v, %d, %v) = %v, want %v", tt.interval, tt.failures, tt.retryAfter, got, tt.want)
		}
	}
}

func TestCacheMaxAge(t *testing.T) {
	tests := []struct {
		header []string
		want   time.Duration
	}{
		{nil, 0},
		{[]string{"max-age=3600"}, time.Hour},
		{[]string{"public, MAX-AGE=600, must-revalidate"}, 10 * time.Minute},
		{[]string{`max-age="120"`}, 2 * time.Minute},
		{[]string{"no-cache"}, 0},
		{[]string{"s-maxage=600"}, 0},
		{[]string{"max-age=-5"}, 0},
		{[]string{"max-age=soon"}, 0},
		{[]string{"private", "max-age=60"}, time.Minute},
		{[]string{"max-age=9223372036854775807"}, maxDelay},
	}
	for _, tt := range tests {
		h := http.Header{"Cache-Control": tt.header}
		if got := cacheMaxAge(h); got != tt.want {
			t.Errorf("cacheMaxAge(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"120", 2 * time.Minute},
		{"0", 0},
		{"-1", 0},
		{"Fri, 23 Oct 2026 15:00:00 GMT", 3 * time.Hour},
		{"Fri, 23 Oct 2026 11:00:00 GMT", 0},
		{"Sat, 31 Oct 2026 12:00:00 GMT", maxDelay},
		{"9223372036854775807", maxDelay},
		{"tomorrow", 0},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.header != "" {
			h.Set("Retry-After", tt.header)
		}
		if got := retryAfter(h, now); got != tt.want {
			t.Errorf("retryAfter(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}
