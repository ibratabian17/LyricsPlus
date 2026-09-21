# Architecture

LyricsPlus is a Go backend that aggregates, normalizes, and serves timed
(synced) lyrics — the "source of truth" for LyricsPlus clients — plus a
user-content (UGC) pipeline guarded by proof-of-work. This document describes
the Go layout of the LyricsPlus backend.

## Request flow

```
Client ──► HTTP (chi router)
              │  middleware chain
              │    CORS → Compression → Tracing → ConcurrencyLimit → RateLimit
              │    → QueryLimits (URL bytes, params, value len)
              │    → MethodOverride → PanicRecovery → BodyLimit (submit only)
              ▼
        handlers (api/handlers)
              ▼
        service  (normalize: V2 canonical, V2→V1, V2→TTML)
              ▼
        orchestrator
           Racer  (concurrent provider fetches, tunable timeout)
           Dedup  (singleflight + result dedup / most-canonical pick)
              ▼
        providers  (musixmatch, spotify, apple, qq — HTTP or local LLCPlex)
              │
              ▼
        parsers   (V1→V2 canonical, JSON/TTML/LRC/plain text)
```

## Components

### API layer — `internal/api`
- `server.go` — builds the chi router and wires everything.
- `middleware/` — `Tracing` (request logging via structured logger),
  `RateLimit` (sliding window per client IP; `/health`, `/readyz` exempt),
  `ConcurrencyLimit`, `QueryLimits`, `Compression`, `CORS`, `PanicRecovery`,
  `BodyLimit`, `MethodOverride`.
- `handlers/` — `Lyrics` (`/v1|/v2/lyrics/get`, `/v1/ttml/get`, `/v1/raw/get`),
  `Catalog` (`/v1/songlist/search`), `Pow` (`/v1/lyricsplus/challenge`,
  `/v1/lyricsplus/submit`), `Health` (`/health`, `/readyz`), and the `Metadata`
  handler (`/v1/metadata/get`).
- `openapi/` — embedded YAML spec + a self-contained `/docs` Swagger viewer.

### Service — `internal/service`
`Service.FetchLyrics` normalizes, caches, and returns the canonical V2 payload
plus diagnostics/processing-time; `FetchRaw` returns the untouched upstream body.

### Orchestrator — `internal/orchestrator`
- `Source` is the provider interface (`Name()`, `FetchLyrics(ctx, query)`).
- `Racer` fans out to listed sources concurrently under a timeout.
- `Dedup` dedupes identical results and picks the most canonical hit; it also
  performs singleflight so concurrent duplicate queries share one upstream fetch.

### Providers — `internal/providers`
Implement the `Source` interface with per-provider query building, HTTP clients,
and normalization into `domain.LyricsResponse`. A missing source emits a
`k_poe_of_unknown` placeholder so all lookups stay fused.

### Parsers — `internal/parsers`
Convert foreign formats (TTML, LRC, plain text, legacy V1 JSON) into the
canonical V2 structure and back (V2→V1, V2→TTML, key/part normalization).

### Storage — `internal/storage`
Embedded SQLite persistence:
- lyric cache (`persist find`, TTL-aware) with a background `dumper` that syncs
  changed rows to Google Drive;
- lyric submissions (UGC) as an append-only log;
- a watchdog that periodically re-opens the DB if a connection dropped.

### Config — `internal/config`
Environment-driven (`config.Load()`). Highlights:
- `DISABLE_LOGGING` (default `false`), `LOG_LEVEL`, `LOG_FORMAT` — structured logger toggle.
- `DISABLE_SLOW_PROVIDERS`, `PROVIDER_TIMEOUT_MS`.
- `RATE_LIMIT_*`, `MAX_URL_BYTES`, `MAX_QUERY_PARAMS`, `MAX_QUERY_VALUE_LEN`, `MAX_CONCURRENCY`.
- `LYRICSPLUS_*` — `ALLOW_SUBMISSIONS`, `MAX_BODY_BYTES`, `JWT_SECRET`,
  `CHALLENGE_TTL_MS`, `POW_DIFFICULTY`, `ACCEPT_VANDALISM`.
- `STORAGE_DB_PATH`, `GDRIVE_DB`, `GDRIVE_FOLDER_ID`, `GDRIVE_SERVICE_ACCOUNT_PATH`,
  `VIDEO_ENDPOINT`.

## Proof-of-work UGC submission

1. Client calls `/v1/lyricsplus/challenge`; a signed JWT containing a random
   challenge and difficulty is returned.
2. Client brute-forces a `nonce` so `sha256(challenge + nonce)` hex begins with
   `difficulty` zero bits.
3. Client POSTs `/v1/lyricsplus/submit` with the token, nonce, metadata, and a
   V2 lyrics payload.
4. Server verifies token signature + expiry + nonce, detects duplicates by
   normalized title/artist/duration, flags vandalism on malformed timing, then
   persists as a UGC submission.

## Health and readiness

- `/health` — liveness; always `200 {"status":"ok","database":"ok|not_configured"}`.
- `/readyz` — readiness; `200` when the DB pings, `503` when degraded.
- Both include `version` and `uptimeSeconds` and skip the rate limiter.

## Directory layout

```
cmd/server          main binary entrypoint
cmd/gdrive_sync     drive sync worker
internal/api        router, middleware, handlers, embedded OpenAPI
internal/orchestrator  racer + dedup + source interface
internal/providers  upstream lyric providers
internal/parsers    format conversion
internal/domain     shared types
internal/service    normalization + serving logic
internal/storage    sqlite + dumper + watchdog
internal/config     env config
internal/logger     structured logging (slog), disableable
internal/version    build version, injectable via -ldflags
docs/               this guide + endpoints.md
```