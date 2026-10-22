# AGENTS.md

Guidelines for humans and coding agents working on greaderd.

## Project

greaderd is a headless feed aggregator that implements the FreshRSS flavour of
the Google Reader API. The primary client is NetNewsWire using the **FreshRSS**
account type. There is no web UI and there will not be one.

Reference implementations for protocol behaviour:

- Server: FreshRSS [`p/api/greader.php`](https://github.com/FreshRSS/FreshRSS/blob/edge/p/api/greader.php)
- Client: NetNewsWire [`Modules/Account/Sources/Account/ReaderAPI`](https://github.com/Ranchero-Software/NetNewsWire/tree/main/Modules/Account/Sources/Account/ReaderAPI)

When changing protocol behaviour, check both and note the reason in a comment.

## Layout

The prototype started as a single `main` package. New code follows, and
existing code is being moved into, this layout:

| Path | Responsibility |
|---|---|
| `cmd/greaderd/` | `main`: read configuration, wire dependencies, run the server. No business logic. |
| `internal/store/` | SQLite persistence: schema, migrations, queries. The only package that contains SQL. |
| `internal/fetcher/` | Fetching, parsing and discovering feeds; the refresh loop. |
| `internal/greader/` | The Google Reader HTTP API: routing, auth, handlers, wire types, item ID encoding. |
| `internal/opml/` | Parsing and rendering OPML, independent of HTTP and storage. |

Dependencies point inwards: `cmd` → `greader`/`fetcher` → `store`. `store`
imports no other internal package.

## Commands

```bash
go build ./...
go test -race ./...
go vet ./...
gofmt -l .        # must print nothing
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
```

Run all of them before committing.

## Go conventions

Follow [Effective Go](https://go.dev/doc/effective_go),
[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) and the
[Google Go Style Guide](https://google.github.io/styleguide/go/). In particular:

**Documentation**
- Every package has a package comment (in `doc.go` when it needs more than a line).
- Every exported identifier has a doc comment that starts with its name and is a full sentence.
- Comments explain *why*, not *what*. Protocol quirks name the client or server
  behaviour they accommodate (e.g. "NetNewsWire parses the last path segment as hex").

**Errors**
- Never discard an error. If ignoring one is deliberate, assign it to `_` and say why.
- Wrap with context: `fmt.Errorf("refresh feed %d: %w", id, err)`.
- Export sentinel errors (`store.ErrNotFound`) and match them with `errors.Is`.
- Handle an error once: either return it or log it at a boundary (HTTP handler,
  refresh loop), never both.

**Design**
- `context.Context` is the first parameter of every function that does I/O; never store it in a struct.
- No package-level mutable state and no `init` side effects; pass dependencies through constructors.
- Define small interfaces where they are consumed (e.g. `greader` declares the
  store methods it needs), not where they are implemented.
- Use typed structs for JSON and XML wire formats, not `map[string]any`. Field
  names and JSON tags mirror the Google Reader API exactly.
- Prefer separate, explicit methods over stringly-typed parameters
  (`MarkRead(ids)` instead of `SetFlag(ids, "is_read", true)`).
- Standard library first: `net/http` with method patterns on `ServeMux`, `log/slog`,
  `database/sql`. New dependencies need a reason in the commit message.

**Logging**
- Use `log/slog` with structured key/value pairs.
- Never log passwords, tokens, `Authorization` headers, request bodies or query strings.

**Concurrency**
- Every goroutine has an owner that can stop it via context cancellation.
- Tests run with `-race`.

## Testing

- Unit tests live next to the code (`*_test.go`) and are table-driven where there
  are several cases.
- HTTP behaviour is tested with `httptest`; tests never touch the network.
- `TestNetNewsWireFlow` replays NetNewsWire's sync sequence. It must keep passing;
  extend it whenever protocol behaviour changes.

## Protocol invariants

These are easy to break and hard to debug from the client side:

- `stream/items/ids` returns item IDs as decimal strings; `stream/items/contents`
  returns them in long form, `tag:google.com,2005:reader/item/%016x`. Both forms
  are accepted as input.
- Every feed is in a folder. NetNewsWire's FreshRSS account disallows feeds at the
  root, so the folder `Uncategorized` (id 1) always exists and cannot be deleted.
- `published` is in seconds, `crawlTimeMsec` in milliseconds (string),
  `timestampUsec` in microseconds (string).
- All routes are served under both `/` and `/api/greader.php/`.
- Write endpoints require the `T` token; `subscription/import` does not, because
  NetNewsWire does not send it there.

## Commits and docs

- Commit messages are in English: imperative subject line of at most 72
  characters, body explaining why.
- Everything in the repository is in English.
- Keep `README.md` in sync with endpoints and configuration.
