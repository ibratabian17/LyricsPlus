# LyricsPlus HTTP API — Endpoint Reference

This document is the prose reference for the LyricsPlus backend API. The
machine-readable contract lives at [`internal/api/openapi/openapi.yaml`](../internal/api/openapi/openapi.yaml)
and is served at `/openapi.yaml` (interactive viewer: `/docs`).

## Conventions

- All responses are JSON unless noted otherwise.
- Successful lyrics endpoints set `Cache-Control: public, max-age=3600, s-maxage=86400, immutable`.
- Misses and errors set `Cache-Control: no-store`.
- A sliding-window rate limit applies per client IP (default `RATE_LIMIT_REQUESTS=20`
  per `RATE_LIMIT_WINDOW_MS=10000`). Health probes (`/health`, `/readyz`) are exempt.
- A global concurrency limit (default `MAX_CONCURRENCY=15000`) returns
  `503 {"error":"Server Busy", ...}` with `Retry-After`.
- Query guardrails: URL bytes (`MAX_URL_BYTES`), param count (`MAX_QUERY_PARAMS`),
  and per-value length (`MAX_QUERY_VALUE_LEN`) return `400`/`414` on violation.

## Shared query parameters (lyrics endpoints)

These apply to `/v1/lyrics/get`, `/v2/lyrics/get`, `/v1/ttml/get`, `/v1/raw/get`.

| Parameter    | Type    | Required | Description |
|--------------|---------|----------|-------------|
| `title`      | string  | conditional | Track title. Required unless `isrc` or `platformId` is present. |
| `artist`     | string  | conditional | Artist name. Required unless `isrc` or `platformId` is present. |
| `album`      | string  | no | Album name, used as a matching hint. |
| `duration`   | number  | no | Track duration in seconds, may carry millisecond decimals (e.g. `232.123`). Coerced to milliseconds. |
| `isrc`       | string  | conditional | ISRC code; enables direct cache lookup. |
| `platformId` | string  | conditional | Upstream platform track ID; enables direct cache lookup. |
| `source`     | string  | no | Comma-separated provider preference, e.g. `musixmatch,spotify,qq`. |
| `forceReload`| boolean | no | `true` bypasses every cache layer and races providers fresh. |

Validation failure returns `400 {"error": "Missing required parameters: (title and artist) or isrc or platformId"}`.

## `/v1/lyrics/get`

Returns lyrics in the legacy flat V1 format (a single `lyrics[]` of token
segments converted from the canonical V2 payload).

- `200` — V1 payload (`type`: `"syllable"` or `"Line"`), `Cache-Control: public, ... immutable`.
- `400` — missing required parameters.
- `404` — `{"error":"Lyrics not found"}`.
- `503` — concurrency limit hit.

## `/v2/lyrics/get`

Returns the canonical V2 payload. This is the most complete format.

`200` response shape (abridged):

```json
{
  "type": "Word | Line | None",
  "KpoeTools": "2.0-LPlusBcknd,...",
  "metadata": {
    "source": "...",
    "title": "...",
    "artist": "...",
    "album": "...",
    "songWriters": [],
    "leadingSilence": "...",
    "agents": { "v1": { "type": "person", "name": "...", "alias": "v1" } },
    "songParts": [ { "name": "Verse", "time": 0 } ],
    "language": "en",
    "totalDuration": "...",
    "curator": "...",
    "audio": []
  },
  "lyrics": [
    {
      "time": 0,
      "duration": 5400,
      "text": "Line 1",
      "syllabus": [ { "time": 0, "duration": 300, "text": "Word1" } ],
      "element": { "key": "L1", "singer": "v1", "songPartIndex": 0 }
    }
  ],
  "cached": "None | GDrive | Database | UserJSON",
  "processingTime": {
    "timeElapsed": 512,
    "lastProcessed": 1730000000000,
    "totalElapsedMs": 512,
    "winnerSource": "Spotify",
    "syncPriority": 3,
    "sourcesStatus": { "apple": { "status": "OK", "elapsedMs": 123 } },
    "selectedSongMetadata": { "source": "Spotify", "title": "...", "artist": "...", "album": "..." }
  }
}
```

Sync priority used internally: word/syllable sync = 3, line sync = 2, plain = 1, none = 0.

## `/v1/ttml/get`

Returns the best hit translated to Apple Music TTML:

```json
{ "ttml": "<tt xmlns=\"...\">...</tt>", "processingTime": { ... } }
```

`200` — `{"ttml": "...", "processingTime": {...}}`.
`500` — `{"error":"TTML conversion failed"}` when serialization is impossible.

## `/v1/raw/get`

Returns the unparsed upstream payload verbatim. Content-Type follows the source:
- `application/xml` for `apple`, `qq`
- `application/json` for `musixmatch`, `spotify`

Headers: `X-Lyrics-Source` and `X-Processing-Time`.
`404` — `{"error":"Raw data is not available for this result","source":"..."}`.

## `/v1/songlist/search`

Queries Apple Music, Spotify and Musixmatch in parallel, then normalizes and
merges, de-duplicating by ISRC first, then title/artist/album.

- `400` — `{"error":"Missing required parameter: q (query)"}` when `q` is empty.
- `200` — `{"results": [ SongCatalogItem... ], "processingTime": {...}}`.

`SongCatalogItem` fields: `id` (map of platform → id), `sourceId`, `title`,
`artist`, `album`, `albumArtUrl`, `durationMs`, `isrc`, `songwriters`,
`availability`, `externalUrls`.

## `/v1/metadata/get`

Apple Music track metadata. Requires `title` and `artist` (plus optional `album`
and `duration`). Returns `{"metadata": {...}}` with `id`, `title`, `artist`,
`album`, `albumArtUrl`, `durationMs`, `isrc`, `songwriters`.
`404` — `{"error":"Could not find metadata"}`.

## `/v1/lyricsplus/challenge` (GET)

Issues a short-lived signed JWT PoW challenge. Response (never cached):

```json
{ "token": "<signed-jwt>", "difficulty": 5 }
```

Solving requires a `nonce` such that the hexadecimal SHA-256 of
`<challenge-from-jwt> + nonce` begins with `difficulty` zero bits. The token
expires after `LYRICSPLUS_CHALLENGE_TTL_MS` (default 600000 ms).

## `/v1/lyricsplus/submit` (POST)

Validates the PoW and persists normalized UGC lyrics.

Request body (`application/json`, limit `MAX_BODY_BYTES` default 1 MiB → `413`):

```json
{
  "proofOfWorkToken": "<from challenge>",
  "nonce": "<pow solution>",
  "songTitle": "Hello",
  "songArtist": "Adele",
  "songAlbum": "25",
  "songDuration": "232.123",
  "songISRC": "GBUM71705975",
  "songPlatformId": "...",
  "lyricsData": { "...v2 lyrics payload..." },
  "forceUpload": false
}
```

Responses:
- `200` — `{"success": true, "message": "Lyrics submitted successfully", "filename": "<canonical>"}`.
- `400` — missing PoW, invalid proof, missing required fields, invalid lyrics data, or suspected vandalism.
- `503` — submissions disabled (`ALLOW_SUBMISSIONS=false`).
- `413` — body exceeds the limit.