-- Per-feed fetch scheduling and the bookkeeping needed to prune old entries.

-- When the feed is due next (unix seconds; 0 = now). The fetcher derives it
-- from the refresh interval, Cache-Control, Retry-After and failure backoff.
ALTER TABLE feeds ADD COLUMN next_fetch_at INTEGER NOT NULL DEFAULT 0;
-- Consecutive failed fetches, for exponential backoff.
ALTER TABLE feeds ADD COLUMN error_count INTEGER NOT NULL DEFAULT 0;
-- When the feed last returned content (unix microseconds; 0 = never).
ALTER TABLE feeds ADD COLUMN last_success_usec INTEGER NOT NULL DEFAULT 0;

-- When a fetch last listed the entry (unix microseconds). Entries still in
-- the feed must not be pruned, or the next fetch would add them as unread.
ALTER TABLE entries ADD COLUMN last_seen_usec INTEGER NOT NULL DEFAULT 0;
UPDATE entries SET last_seen_usec = crawled_usec;

CREATE INDEX feeds_next_fetch ON feeds (next_fetch_at);
CREATE INDEX entries_last_seen ON entries (last_seen_usec);
