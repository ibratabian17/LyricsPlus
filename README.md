# LyricsPlus Backend

Go implementation of the LyricsPlus lyric API. It aggregates, normalizes, and
serves timed (synced) lyrics to LyricsPlus clients, and hosts a proof-of-work
protected user-content (UGC) submission pipeline.

## Features

- Canonical V2 lyric payload with word/line/plain sync, plus legacy V1, Apple
  Music TTML, raw upstream passthrough, and nearest-by ISRC/preferred-source
  searches.
- Concurrent multi-provider racing (`apple`, `musixmatch`, `spotify`, `qq`,
  `deezer`, plus the LLCPlex/Qaple UGC fallback) with singleflight de-duplication.
- SQLite-backed caching with background Google Drive sync.
- Proof-of-work (SHA-256 challenge) gated UGC submissions with vandalism checks.
- Structured logging (`internal/logger`) that can be toggled on/off at runtime.
- Liveness/readiness endpoints (`/health`, `/readyz`) exempt from rate limits.
- OpenAPI 3.0.3 specification served at `/openapi.yaml` with an interactive
  viewer at `/docs`.

## Quick start

Requires Go 1.27+ and (optionally) `make` / Docker.

```sh
# local
make build          # -> bin/server
make run            # http://localhost:3000

# docker
make docker-up      # build + run via docker-compose (healthcheck attached)
```

Smoke test:

```sh
curl "localhost:3000/v1/lyrics/get?title=Hello&artist=Adele"
curl localhost:3000/health
```

## Configuration

All configuration is environment-driven with sensible defaults (see
`internal/config/config.go`). Notable variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `3000` | HTTP listen address |
| `DISABLE_LOGGING` | `false` | Set `true` to turn the logger off entirely |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `LOG_FORMAT` | `text` | `text` \| `json` |
| `RATE_LIMIT_REQUESTS` | `20` | Sliding-window requests per IP |
| `RATE_LIMIT_WINDOW_MS` | `10000` | Window length in ms |
| `MAX_CONCURRENCY` | `15000` | Global in-flight requests before `503` |
| `MAX_URL_BYTES` | `4096` | Reject longer request URLs with `414` |
| `MAX_QUERY_PARAMS` | `20` | Reject requests with too many query params |
| `MAX_QUERY_VALUE_LEN` | `500` | Reject query values longer than this |
| `PROVIDER_TIMEOUT_MS` | `8000` | Per-provider fetch timeout |
| `ALLOW_SUBMISSIONS` | `true` | Enable/disable the UGC submit endpoint |
| `JWT_SECRET` | dev default | HMAC secret for PoW challenge tokens |
| `LYRICSPLUS_POW_DIFFICULTY` | `5` | Leading-zero bits required for nonce |
| `LYRICSPLUS_CHALLENGE_TTL_MS` | `600000` | Challenge token lifetime |
| `MAX_BODY_BYTES` | `1048576` | Submit body limit (`413`) |
| `SQLITE_PATH` | `database/lyrics_cache.db` | Lyric cache DB location |
| `GDRIVE_ENABLED` | `true` | Google Drive caching/sync on start |
| `DAILY_DUMP_ENABLED` | `true` | Periodic DB snapshot + GDrive upload |

Provider credentials (Spotify, Musixmatch, Apple, QQ, Deezer) are read from
their matching `*_COOKIE` / `_TOKEN` / `_ID` environment variables; leave unset
to rely on the built-in defaults.

## API surface

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/v1/lyrics/get` | Legacy flat V1 lyrics |
| GET | `/v2/lyrics/get` | Canonical V2 lyrics |
| GET | `/v1/ttml/get` | Apple Music TTML output |
| GET | `/v1/raw/get` | Untouched upstream payload |
| GET | `/v1/songlist/search` | Catalog search across providers |
| GET | `/v1/metadata/get` | Apple Music track metadata |
| GET | `/v1/lyricsplus/challenge` | Issue PoW challenge token |
| POST | `/v1/lyricsplus/submit` | Submit UGC lyrics (PoW-gated) |
| GET | `/health` | Liveness probe |
| GET | `/readyz` | Readiness probe (DB ping) |
| GET | `/openapi.yaml` | OpenAPI 3.0.3 spec |
| GET | `/docs` | Swagger UI viewer |
| GET | `/` | Service banner |

See [`docs/endpoints.md`](docs/endpoints.md) for parameter and response details
and [`docs/architecture.md`](docs/architecture.md) for the component model.

## Development

```sh
make test-race      # unit + black-box HTTP tests with the race detector
make lint           # golangci-lint (falls back to go vet if not installed)
make fmt-check      # gofmt gate (CI enforces this too)
make hooks-install  # enable the pre-commit hook (.githooks/pre-commit)
```

Build version injection:

```sh
VERSION=v1.2.3 make build
```

The version is reported by `/health` and `/readyz`.