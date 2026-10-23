package fetcher

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxDelay caps how long a feed waits for its next fetch, however slow the
// server asks us to be and however often it failed, so a feed that recovers
// is back within a day. A refresh interval above it takes precedence.
const maxDelay = 24 * time.Hour

// maxDelaySeconds bounds header values before converting them to a
// time.Duration, which would overflow for absurd values.
const maxDelaySeconds = int64(maxDelay / time.Second)

// successDelay is the time until the next fetch after a successful one: the
// refresh interval, or longer if the server's Cache-Control max-age says the
// feed will not change sooner. Feeds are never fetched more often than the
// interval, whatever max-age says.
func successDelay(interval, maxAge time.Duration) time.Duration {
	return min(max(interval, maxAge), max(interval, maxDelay))
}

// failureDelay is the time until the next attempt after failures consecutive
// failed fetches: the interval, doubled for every further failure, and at
// least what the server asked for in Retry-After.
func failureDelay(interval time.Duration, failures int, retryAfter time.Duration) time.Duration {
	limit := max(interval, maxDelay)
	d := interval
	for i := 1; i < failures && d < limit; i++ {
		d *= 2
	}
	return min(max(d, retryAfter), limit)
}

// cacheMaxAge returns the max-age directive of the Cache-Control header, or 0
// if there is none.
func cacheMaxAge(h http.Header) time.Duration {
	for _, v := range h.Values("Cache-Control") {
		for _, directive := range strings.Split(v, ",") {
			name, value, ok := strings.Cut(strings.TrimSpace(directive), "=")
			if !ok || !strings.EqualFold(name, "max-age") {
				continue
			}
			secs, err := strconv.ParseInt(strings.Trim(value, `"`), 10, 64)
			if err != nil || secs <= 0 {
				return 0
			}
			return time.Duration(min(secs, maxDelaySeconds)) * time.Second
		}
	}
	return 0
}

// retryAfter parses the Retry-After header, which holds either a delay in
// seconds or an HTTP date. It returns 0 if the header is missing or invalid.
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(min(secs, maxDelaySeconds)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(t.Sub(now), 0), maxDelay)
	}
	return 0
}
