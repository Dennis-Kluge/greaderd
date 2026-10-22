# greaderd

A minimal, headless feed aggregator that speaks the Google Reader API the way
FreshRSS does (`p/api/greader.php`). Built for NetNewsWire using the
**FreshRSS** account type.

- Single Go binary, SQLite (`modernc.org/sqlite`, no cgo), feeds parsed with `gofeed`
- One user, configured via environment variables
- Feeds are refreshed every 15 minutes (with ETag/Last-Modified)

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
| `GREADERD_REFRESH_INTERVAL` | `15m` | Time between two refreshes of a feed |

## NetNewsWire

Add account → **FreshRSS** → API URL `http://<host>:8080/api/greader.php`
(or just `http://<host>:8080`), with the username and password from above.

Feeds can be added in NetNewsWire or imported via OPML.
As in FreshRSS, every feed lives in a folder: NetNewsWire does not allow feeds
at the top level of a FreshRSS account. There is therefore always an
`Uncategorized` folder (can be renamed, cannot be deleted); feeds of deleted
folders move there.

When adding a website URL, the feed is discovered via
`<link rel="alternate">`.

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
| POST | `/reader/api/0/stream/items/contents` | `i` (decimal or long hex form) |
| POST | `/reader/api/0/edit-tag` | `read`, `starred`, `kept-unread` |
| POST | `/reader/api/0/mark-all-as-read` | `s`, `ts` (µs) |

All endpoints except ClientLogin require `Authorization: GoogleLogin auth=<Auth>`;
all POST endpoints except the OPML import additionally require `T`.

## Intentionally left out (prototype)

- `stream/contents` (not needed by NetNewsWire, but by other clients such as Reeder)
- HTML sanitizing of article content
- Pruning old articles, favicons, WebSub
- Multiple users, multiple folders per feed
- No TLS – run it behind a reverse proxy (Caddy or similar)

## Tests

```bash
go test -race ./...
```

`AGENTS.md` describes the package layout and the conventions the code follows.

`TestNetNewsWireFlow` replays NetNewsWire's sync flow: login, token, adding a
feed via discovery, setting a folder, paging through item IDs, fetching
contents with NetNewsWire-encoded IDs, setting read/starred, renaming and
deleting folders, OPML export, and unsubscribing.
