import { handleSongLyrics } from "./lyrics.controller.js";
import { v2Tov1 } from "../../shared/parsers/kpoe.parser.js";
import { convertJsonToTTML } from "../../shared/parsers/ttml.parser.js";
import GoogleDrive from "../../shared/utils/googleDrive.util.js";

const gd = new GoogleDrive();

// Helpers
function buildProcessingTime(handlerStartTime, diagnostics = {}) {
    const now = Date.now();
    return {
        timeElapsed:    now - handlerStartTime,
        lastProcessed:  now,
        totalElapsedMs: diagnostics.totalElapsedMs,
        ...diagnostics
    };
}

// Handlers
export async function handleLyricsRequest(c) {
    const startTime = Date.now();
    const query = c.req.query();
    const songTitle = query.title;
    const songArtist = query.artist;
    const format = c.get('format') || query.format || 'v2';

    const songISRC = query.isrc;
    const songPlatformId = query.platformId;

    if ((!songTitle || !songArtist) && !songISRC && !songPlatformId) {
        return c.json(
            {
                error: "Missing required parameters: (title and artist) or isrc or platformId",
            },
            400
        );
    }

    const songAlbum = query.album || "";
    const songDuration = query.duration;
    const source = query.source;
    const forceReload = query.forceReload === "true";

    const result = await handleSongLyrics(
        songTitle,
        songArtist,
        songAlbum,
        songDuration,
        songISRC,
        songPlatformId,
        gd,
        source ? source.split(",") : undefined,
        forceReload,
        c.env,
        c
    );

    let data;
    if (result.success) {
        switch (format) {
            case 'v1':
                data = v2Tov1(result.data);
                break;
            case 'ttml':
                try {
                    data = { ttml: convertJsonToTTML(result.data) };
                } catch (e) {
                    console.error("Error converting to TTML:", e);
                    data = result.data;
                }
                break;
            case 'v2':
            default:
                data = result.data;
                break;
        }
    } else {
        data = { error: result.data };
    }

    data.processingTime = buildProcessingTime(startTime, result.diagnostics);

    const headers = result.success
        ? { "Cache-Control": "public, max-age=3600, immutable" }
        : { "Cache-Control": "no-store" };

    return c.json(data, result.status || (result.success ? 200 : 400), headers);
}

export async function handleRawLyricsRequest(c) {
    const startTime = Date.now();
    const query = c.req.query();
    const songTitle = query.title;
    const songArtist = query.artist;

    const songISRC = query.isrc;
    const songPlatformId = query.platformId;

    if ((!songTitle || !songArtist) && !songISRC && !songPlatformId) {
        return c.json(
            {
                error: "Missing required parameters: (title and artist) or isrc or platformId",
            },
            400
        );
    }

    const songAlbum = query.album || "";
    const songDuration = query.duration;
    const source = query.source;
    const forceReload = query.forceReload === "true";

    const result = await handleSongLyrics(
        songTitle,
        songArtist,
        songAlbum,
        songDuration,
        songISRC,
        songPlatformId,
        gd,
        source ? source.split(",") : undefined,
        forceReload,
        c.env,
        c
    );

    const processingTime = buildProcessingTime(startTime, result.diagnostics);

    if (!result.success) {
        return c.json(
            {
                error: result.data,
                processingTime,
            },
            result.status || 400,
            { "Cache-Control": "no-store" }
        );
    }

    if (!result.rawData) {
        return c.json(
            {
                error: "Raw data is not available for this result",
                source: result.source,
                processingTime,
            },
            404,
            { "Cache-Control": "no-store" }
        );
    }

    const rawSource = result.source ? result.source.toLowerCase().replace('-word', '') : '';
    let contentType = 'application/octet-stream';
    let body = result.rawData;

    if (rawSource === 'apple') {
        contentType = 'application/xml';
    } else if (rawSource === 'qq') {
        contentType = 'application/xml';
    } else if (rawSource === 'musixmatch' || rawSource === 'spotify') {
        contentType = 'application/json';
        if (typeof body !== 'string') {
            body = JSON.stringify(body);
        }
    } else if (typeof body === 'object') {
        contentType = 'application/json';
        body = JSON.stringify(body);
    }

    return c.body(body, 200, {
        "Content-Type": contentType,
        "Cache-Control": "public, max-age=3600, immutable",
        "X-Lyrics-Source": result.source || "unknown",
        "X-Processing-Time": `${Date.now() - startTime}ms`,
    });
}