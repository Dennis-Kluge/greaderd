-- Icons for clients that show the iconUrl of subscriptions.

-- URL of the icon the feed's site declares, or its /favicon.ico; '' if none.
ALTER TABLE feeds ADD COLUMN icon_url TEXT NOT NULL DEFAULT '';
-- When the icon was last looked for (unix seconds; 0 = never).
ALTER TABLE feeds ADD COLUMN icon_checked_at INTEGER NOT NULL DEFAULT 0;
