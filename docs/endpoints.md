# API Endpoints

This document provides a detailed list and explanation of the API endpoints exposed by the LyricsPlus Backend. These endpoints allow for fetching lyrics, metadata, and submitting user contributions.

## General Structure

All primary API endpoints are organized modularly within the `src/modules/` directory. Each module's handler defines its specific routes.

## Client Header Recommendation

Clients may optionally include the `X-Client-Package` request header to identify the application making the request. This header should specify the app name and project link/source, and may include a URL to the application's homepage or project page. For example:

`X-Client-Package: YouLyPlus <https://github.com/ibratabian17/YouLyPlus>`

This is optional, but useful for custom controlling systems or analytics.

## Available Endpoints

### Lyrics Retrieval

#### Search Priority with Multiple Identifiers

When both `title`/`artist` and an `isrc` or `platformId` are provided in a lyrics retrieval request, the system prioritizes accuracy by leveraging all available information:

1.  **Full Search Scope**: The system performs a comprehensive search across all configured lyric sources (including external APIs like Apple Music, Musixmatch, Spotify, and QQ Music), not just the local cache.
2.  **Cache Lookup**: During the initial cache check, if a cached entry matches the provided `isrc` or `platformId`, it is considered a perfect match and is returned immediately. This ensures that specific versions identified by an ID are prioritized.
3.  **External API Disambiguation**: If the song is not found in the cache, external APIs are queried using the `title` and `artist`. The provided `isrc` or `platformId` is then used to filter and select the most accurate song from the results returned by the external service.

This approach ensures that providing an `isrc` or `platformId` alongside `title` and `artist` yields the most precise result possible, whether from cached data or external providers.

*   **`GET /v1/lyrics/get`**
    *   **Description**: Fetches synchronized lyrics for a given song in a legacy format, primarily for older YouLy+ clients. Searches can be performed using `title` and `artist`, or by providing an `isrc` or `platformId` for a direct cache lookup.
    *   **Parameters**:
        *   `title` (conditional): The title of the song. Required if `isrc` and `platformId` are not provided.
        *   `artist` (conditional): The artist of the song. Required if `isrc` and `platformId` are not provided.
        *   `album` (optional): The album of the song.
        *   `duration` (optional): The duration of the song in seconds format (for miliseconds use decimals, e.g 232.123).
        *   `isrc` (conditional): The ISRC of the song. Can be used as an alternative to `title` and `artist` for cached lookups.
        *   `platformId` (conditional): The platform-specific ID of the song (e.g., Apple Music ID, Musixmatch Track ID, Spotify Song ID, QQ Music Mid). Can be used as an alternative to `title` and `artist` for cached lookups.
        *   `source` (optional): Comma-separated list of preferred lyric sources (e.g., `musixmatch,spotify,qq`).
        *   `forceReload` (optional): Set to `true` to bypass cache and force a reload of lyrics.
    *   **Response**: Returns lyrics in a legacy format (v1).

*   **`GET /v2/lyrics/get`**
    *   **Description**: Fetches synchronized lyrics for a given song in an improved format with better word-grouping. Searches can be performed using `title` and `artist`, or by providing an `isrc` or `platformId` for a direct cache lookup.
    *   **Parameters**:
        *   `title` (conditional): The title of the song. Required if `isrc` and `platformId` are not provided.
        *   `artist` (conditional): The artist of the song. Required if `isrc` and `platformId` are not provided.
        *   `album` (optional): The album of the song.
        *   `duration` (optional): The duration of the song in seconds format (for miliseconds use decimals, e.g 232.123).
        *   `isrc` (conditional): The ISRC of the song. Can be used as an alternative to `title` and `artist` for cached lookups.
        *   `platformId` (conditional): The platform-specific ID of the song (e.g., Apple Music ID, Musixmatch Track ID, Spotify Song ID, QQ Music Mid). Can be used as an alternative to `title` and `artist` for cached lookups.
        *   `source` (optional): Comma-separated list of preferred lyric sources (e.g., `musixmatch,spotify,qq`).
        *   `forceReload` (optional): Set to `true` to bypass cache and force a reload of lyrics.
    *   **Response**: Returns lyrics in the default format (v2).

*   **`GET /v1/ttml/get`**
    *   **Description**: Retrieves lyrics specifically formatted in TTML (Timed Text Markup Language), often used for Apple Music applications. Searches can be performed using `title` and `artist`, or by providing an `isrc` or `platformId` for a direct cache lookup.
    *   **Parameters**:
        *   `title` (conditional): The title of the song. Required if `isrc` and `platformId` are not provided.
        *   `artist` (conditional): The artist of the song. Required if `isrc` and `platformId` are not provided.
        *   `album` (optional): The album of the song.
        *   `duration` (optional): The duration of the song in seconds format (for miliseconds use decimals, e.g 232.123).
        *   `isrc` (conditional): The ISRC of the song. Can be used as an alternative to `title` and `artist` for cached lookups.
        *   `platformId` (conditional): The platform-specific ID of the song (e.g., Apple Music ID, Musixmatch Track ID, Spotify Song ID, QQ Music Mid). Can be used as an alternative to `title` and `artist` for cached lookups.
        *   `source` (optional): Comma-separated list of preferred lyric sources (e.g., `musixmatch,spotify,qq`).
        *   `forceReload` (optional): Set to `true` to bypass cache and force a reload of lyrics.
    *   **Response**: Returns lyrics content in Apple's TTML format.

*   **`GET /v1/raw/get`**
    *   **Description**: Retrieves the raw, unprocessed lyrics file from the original source. Returns the data in its native format (TTML for Apple Music, JSON for Musixmatch/Spotify, QRC/XML for QQ Music) instead of converting it to the LyricsPlus format. Useful for clients that need the original source data.
    *   **Parameters**:
        *   `title` (conditional): The title of the song. Required if `isrc` and `platformId` are not provided.
        *   `artist` (conditional): The artist of the song. Required if `isrc` and `platformId` are not provided.
        *   `album` (optional): The album of the song.
        *   `duration` (optional): The duration of the song in seconds format (for miliseconds use decimals, e.g 232.123).
        *   `isrc` (conditional): The ISRC of the song. Can be used as an alternative to `title` and `artist` for cached lookups.
        *   `platformId` (conditional): The platform-specific ID of the song (e.g., Apple Music ID, Musixmatch Track ID, Spotify Song ID, QQ Music Mid). Can be used as an alternative to `title` and `artist` for cached lookups.
        *   `source` (optional): Comma-separated list of preferred lyric sources (e.g., `musixmatch,spotify,qq`).
        *   `forceReload` (optional): Set to `true` to bypass cache and force a reload of lyrics.
    *   **Response**: Returns the raw lyrics file with the appropriate `Content-Type` header (`application/xml` for Apple/QQ, `application/json` for Musixmatch/Spotify). Includes `X-Lyrics-Source` and `X-Processing-Time` headers.


### Song Catalog

*   **`GET /v1/songlist/search`**
    *   **Description**: Searches the song catalog for songs matching a given query.
    *   **Parameters**:
        *   `q` (required): The search query (e.g., song title, artist name).
    *   **Response**: Returns a list of songs matching the query.

### Metadata Retrieval

*   **`GET /v1/metadata/get`**
    *   **Description**: Obtains comprehensive metadata for a song, primarily from Apple Music.
    *   **Parameters**:
        *   `title` (required): The title of the song.
        *   `artist` (required): The artist of the song.
        *   `album` (optional): The album of the song.
        *   `duration` (optional): The duration of the song in milliseconds.
    *   **Response**: Returns song details such as title, artist, album, cover art URL, release date, etc.

### User Submissions

*   **`GET /v1/lyricsplus/challenge`**
    *   **Description**: Initiates a Proof-of-Work (PoW) challenge for user submission. Clients should request this endpoint to get a challenge token and difficulty before submitting lyrics.
    *   **Parameters**: None.
    *   **Response**: Returns a JSON object containing `token` (a JWT challenge) and `difficulty` (the required PoW difficulty).

*   **`POST /v1/lyricsplus/submit`**
    *   **Description**: Allows users to submit their own synchronized lyrics to the LyricsPlus database after successfully completing a Proof-of-Work challenge.
    *   **Request Body**:
        *   `proofOfWorkToken` (required): The JWT token obtained from `/v1/lyricsplus/challenge`.
        *   `nonce` (required): The solution to the Proof-of-Work challenge.
        *   `songTitle` (required): The title of the song.
        *   `songArtist` (required): The artist of the song.
        *   `songAlbum` (optional): The album of the song.
        *   `songDuration` (required): The duration of the song in seconds.
        *   `lyricsData` (required): The synchronized lyrics content (e.g., in LRC or KPOE format).
        *   `forceUpload` (optional): Boolean. Replacing an existing file requires the `x-submission-update-key` header and a configured `SUBMISSION_UPDATE_KEY`.
    *   **Response**: Confirmation of submission or error details.

## Error Handling

All endpoints will return standard HTTP status codes for success and error conditions. Error responses will typically include a JSON object with an `error` message and potentially an `errorCode`.

*   `200 OK`: Request successful.
*   `400 Bad Request`: Invalid parameters or request body.
*   `404 Not Found`: Resource (e.g., song, lyrics) not found.
*   `500 Internal Server Error`: Server-side error.

---

*Note: This document provides a high-level overview. For exact parameter names, types, and detailed response schemas, please refer to the API specification or the source code within `src/modules/`.*
