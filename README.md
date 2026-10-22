# greaderd

Minimaler Feed-Aggregator ohne UI, der das Google-Reader-API so spricht wie
FreshRSS (`p/api/greader.php`). Gedacht für NetNewsWire mit dem Kontotyp
**FreshRSS**.

- Ein Go-Binary, SQLite (`modernc.org/sqlite`, kein cgo), Feeds via `gofeed`
- Ein Nutzer, konfiguriert per Umgebungsvariablen
- Feeds werden alle 15 Minuten abgeholt (mit ETag/Last-Modified)

## Starten

```bash
GREADERD_USER=dennis GREADERD_PASSWORD=geheim go run .
```

| Variable | Default | Bedeutung |
|---|---|---|
| `GREADERD_USER` | – (Pflicht) | Benutzername für ClientLogin |
| `GREADERD_PASSWORD` | – (Pflicht) | Passwort für ClientLogin |
| `GREADERD_ADDR` | `:8080` | Listen-Adresse |
| `GREADERD_DB` | `greaderd.db` | Pfad zur SQLite-Datei |
| `GREADERD_REFRESH_INTERVAL` | `15m` | Abstand zwischen zwei Abrufen eines Feeds |

## NetNewsWire

Konto hinzufügen → **FreshRSS** → API-URL `http://<host>:8080/api/greader.php`
(oder einfach `http://<host>:8080`), Benutzer und Passwort wie oben.

Feeds können in NetNewsWire hinzugefügt oder per OPML importiert werden.
Wie bei FreshRSS liegt jeder Feed in einem Ordner: NetNewsWire erlaubt beim
FreshRSS-Kontotyp keine Feeds auf oberster Ebene. Es gibt deshalb immer den
Ordner `Uncategorized` (umbenennbar, nicht löschbar); gelöschte Ordner geben
ihre Feeds dorthin ab.
Beim Hinzufügen einer Website-URL wird der Feed über
`<link rel="alternate">` gefunden.

## Endpunkte

Alle Pfade gibt es unter `/` und unter `/api/greader.php/`.

| Methode | Pfad | |
|---|---|---|
| POST | `/accounts/ClientLogin` | `Email`, `Passwd` → `Auth=` |
| GET | `/reader/api/0/token` | Schreib-Token `T` |
| GET | `/reader/api/0/user-info` | |
| GET | `/reader/api/0/tag/list` | Ordner |
| GET | `/reader/api/0/subscription/list` | Feeds |
| POST | `/reader/api/0/subscription/quickadd` | Feed hinzufügen |
| POST | `/reader/api/0/subscription/edit` | `ac=subscribe/unsubscribe/edit`, `t`, `a`, `r` |
| POST | `/reader/api/0/subscription/import` | OPML im Body |
| GET | `/reader/api/0/subscription/export` | OPML |
| POST | `/reader/api/0/rename-tag`, `/disable-tag` | Ordner umbenennen/löschen |
| GET | `/reader/api/0/unread-count` | |
| GET | `/reader/api/0/stream/items/ids` | `s`, `n`, `r`, `ot`, `nt`, `xt`, `it`, `c` |
| POST | `/reader/api/0/stream/items/contents` | `i` (dezimal oder lange Hex-Form) |
| POST | `/reader/api/0/edit-tag` | `read`, `starred`, `kept-unread` |
| POST | `/reader/api/0/mark-all-as-read` | `s`, `ts` (µs) |

Alle Endpunkte außer ClientLogin brauchen `Authorization: GoogleLogin auth=<Auth>`,
alle POST-Endpunkte außer dem OPML-Import zusätzlich `T`.

## Bewusst weggelassen (Prototyp)

- `stream/contents` (braucht NetNewsWire nicht, andere Clients wie Reeder schon)
- HTML-Sanitizing der Artikelinhalte
- Aufräumen alter Artikel, Favicons, WebSub
- Mehrere Nutzer, mehrere Ordner pro Feed
- Ohne TLS – für den Betrieb hinter einen Reverse-Proxy (Caddy o. ä.) stellen

## Tests

```bash
go test ./...
```

`TestNetNewsWireFlow` spielt den Ablauf von NetNewsWire nach: Login, Token,
Feed per Discovery hinzufügen, Ordner setzen, IDs seitenweise laden, Inhalte
mit NetNewsWire-kodierten IDs holen, Read/Starred setzen, Ordner umbenennen,
OPML-Export, Abbestellen.
