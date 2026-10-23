# greaderd

A minimal, headless feed aggregator that speaks the Google Reader API the way
FreshRSS does (`p/api/greader.php`). Built for NetNewsWire using the
**FreshRSS** account type.

- Single Go binary, SQLite (`modernc.org/sqlite`, no cgo), feeds parsed with `gofeed`
- One user, configured via environment variables
- Feeds are refreshed every 15 minutes, politely: conditional requests,
  `Cache-Control`, `Retry-After` and backoff for broken feeds
- Old read entries are pruned automatically
- Article HTML is sanitized with `bluemonday`; feeds get icons

## Running

```bash
GREADERD_USER=dennis GREADERD_PASSWORD=secret go run ./cmd/greaderd
```

| Variable | Default | Description |
|---|---|---|
| `GREADERD_USER` | – (required) | Username for ClientLogin |
| `GREADERD_PASSWORD` | – (required) | Password for ClientLogin |
| `GREADERD_ADDR` | `:8080` | Listen address |
| `GREADERD_DB` | `greaderd.db` | Path to the SQLite database |
| `GREADERD_REFRESH_INTERVAL` | `15m` | Regular time between two refreshes of a feed |
| `GREADERD_RETENTION` | `90d` | How long read entries are kept after their feed dropped them (`90d`, `720h`; `0` keeps everything) |
| `GREADERD_TRUST_PROXY_HEADERS` | `false` | Take the client address from `X-Forwarded-For`; enable only behind a reverse proxy |
| `GREADERD_ALLOW_PRIVATE_FEEDS` | `false` | Allow feeds on loopback and private networks |

## Fetching

Each feed is scheduled on its own:

- **Regularly** every `GREADERD_REFRESH_INTERVAL`, with conditional requests
  (`ETag`, `Last-Modified`).
- **Less often** if the server's `Cache-Control: max-age` says the feed will
  not change sooner. Feeds are never fetched more often than the interval.
- **With backoff** when fetching fails: the delay doubles with every
  consecutive failure (15m, 30m, 1h, …) and honours `Retry-After` on
  `429`/`503`. The first success resets it.
- **At least once a day**, however long the server asks us to wait or however
  often a feed failed, so a recovered feed is back within a day.

When a feed redirects permanently (`301`/`308` all the way), greaderd stores
the new URL. Temporary redirects are followed but not stored.

### Retention

Every hour, greaderd deletes entries that are **read, not starred, and no
longer in their feed** for longer than `GREADERD_RETENTION`. Entries still
listed by the feed are kept however old they are; otherwise the next fetch
would add them again as unread. Unread and starred entries are never deleted.

SQLite reuses the freed space but does not shrink the file; run
`sqlite3 greaderd.db VACUUM` while greaderd is stopped to do that.

## Deployment

greaderd has no TLS of its own. `deploy/` contains a Docker Compose setup that
puts it behind [Caddy](https://caddyserver.com), which obtains and renews a
certificate automatically:

```bash
cd deploy
cp .env.example .env   # set GREADERD_DOMAIN, GREADERD_USER, GREADERD_PASSWORD
docker compose up -d --build
```

The domain's DNS record must point to the server, and ports 80 and 443 must be
reachable. The database lives in the `greaderd-data` volume; back up
`/data/greaderd.db` from it.

The image is also usable on its own: it is a static binary on distroless,
runs as a non-root user, stores data in `/data` and checks its health with
`greaderd healthcheck`.

### Security

- **Feeds on private networks are refused.** Otherwise anyone who can add a
  feed could make the server fetch internal services such as cloud metadata
  endpoints (SSRF). The check applies to every connection, including redirects.
  Set `GREADERD_ALLOW_PRIVATE_FEEDS=true` for a local installation that should
  read feeds from your own network.
- **Failed logins are rate limited:** after 5 failures within 15 minutes a
  client gets `429 Too Many Requests` until the window ends. Behind a proxy,
  `GREADERD_TRUST_PROXY_HEADERS=true` is required so that clients are told
  apart (the Compose setup sets it). Synced clients are unaffected, since they
  keep their token and only log in again when it stops working.
- `GET /healthz` needs no authentication and only reports whether the
  database answers; the Caddyfile hides it from the internet.

## NetNewsWire

Add account → **FreshRSS** → API URL `https://<domain>/api/greader.php`
(or `http://localhost:8080` locally), with the username and password from above.

Feeds can be added in NetNewsWire or imported via OPML.
As in FreshRSS, every feed lives in a folder: NetNewsWire does not allow feeds
at the top level of a FreshRSS account. There is therefore always an
`Uncategorized` folder (can be renamed, cannot be deleted); feeds of deleted
folders move there.

When adding a website URL, the feed is discovered via
`<link rel="alternate">`.

## Other clients

Besides the calls NetNewsWire makes, greaderd serves `stream/contents`, which
Reeder, ReadKit and similar clients use to load full items in one request.
Subscriptions carry an `iconUrl`: the best icon the feed's site declares
(Apple touch icon, then favicon) or its `/favicon.ico`, looked up weekly.

Article HTML comes from third parties and is sanitized before it is sent to
any client: scripts, styles, event handlers, forms and iframes are removed,
except for YouTube and Vimeo embeds. Formatting, links, images and tables stay.

## Checking on feeds

There is no web interface; `greaderd feeds` lists the subscriptions with
their unread count, last and next fetch and consecutive failures:

```bash
GREADERD_DB=greaderd.db greaderd feeds -failing
```

In the Compose setup: `docker compose exec greaderd /greaderd feeds`.

## Endpoints

All paths are served under both `/` and `/api/greader.php/`.

| Method | Path | |
|---|---|---|
| POST | `/accounts/ClientLogin` | `Email`, `Passwd` → `Auth=` |
| GET | `/reader/api/0/token` | Write token `T` |
| GET | `/reader/api/0/user-info` | |
| GET | `/reader/api/0/tag/list` | Folders |
| GET | `/reader/api/0/subscription/list` | Feeds |
| POST | `/reader/api/0/subscription/quickadd` | Add a feed |
| POST | `/reader/api/0/subscription/edit` | `ac=subscribe/unsubscribe/edit`, `t`, `a`, `r` |
| POST | `/reader/api/0/subscription/import` | OPML in the request body |
| GET | `/reader/api/0/subscription/export` | OPML |
| POST | `/reader/api/0/rename-tag`, `/disable-tag` | Rename/delete folders |
| GET | `/reader/api/0/unread-count` | |
| GET | `/reader/api/0/stream/items/ids` | `s`, `n`, `r`, `ot`, `nt`, `xt`, `it`, `c` |
| GET | `/reader/api/0/stream/contents/<stream>` | Full items; same parameters, stream in the path or as `s` |
| POST | `/reader/api/0/stream/items/contents` | `i` (decimal or long hex form) |
| POST | `/reader/api/0/edit-tag` | `read`, `starred`, `kept-unread` |
| POST | `/reader/api/0/mark-all-as-read` | `s`, `ts` (µs) |

All endpoints except ClientLogin require `Authorization: GoogleLogin auth=<Auth>`;
all POST endpoints except the OPML import additionally require `T`.

`GET /healthz` (root only) answers `ok` while the database is reachable.

## Intentionally left out

- Multiple users, multiple folders per feed
- WebSub, full-text extraction
- A web interface

## Tests

```bash
go test -race ./...
```

GitHub Actions runs gofmt, `go vet`, staticcheck and the tests on every push
and pull request, and builds the Docker image and checks its health.

`AGENTS.md` describes the package layout and the conventions the code follows.

`TestNetNewsWireFlow` replays NetNewsWire's sync flow: login, token, adding a
feed via discovery, setting a folder, paging through item IDs, fetching
contents with NetNewsWire-encoded IDs, setting read/starred, renaming and
deleting folders, OPML export, and unsubscribing.
