import { AppleMusicService } from "../../shared/services/appleMusic.service.js";
import { DeezerService } from "../../shared/services/deezer.service.js";
import { MusixmatchService } from "../../shared/services/musixmatch.service.js";
import { SpotifyService } from "../../shared/services/spotify.service.js";
import { QQService } from "../../shared/services/qq.service.js";
import { LyricsPlusService } from "../../shared/services/lyricsPlus.service.js";
import { QapleService } from "../../shared/services/qaple.service.js";
import { FileUtils } from "../../shared/utils/file.util.js";
import GoogleDrive from "../../shared/utils/googleDrive.util.js";
import { runWithFetchSignal } from '../../shared/utils/fetch.util.js';

import { logger } from '../../shared/utils/logger.util.js';

const gd = new GoogleDrive();

/**
 * Deduplicate concurrent identical lyrics requests. Multiple clients asking for
 * the same song (e.g. a popular track during a spike) share a single in-flight
 * fetch pipeline, so backends like GDrive are only hit once.
 */
const inFlightLyrics = new Map();
const IN_FLIGHT_TTL_MS = 30_000;

function buildRequestKey(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, preferredSources) {
    const norm = (v) => String(v || '').trim().toLowerCase().replace(/\s+/g, ' ');
    const srcKey = Array.isArray(preferredSources) ? preferredSources.join(',') : '';
    return [
        norm(songTitle), norm(songArtist), norm(songAlbum),
        String(Math.round(Number(songDuration) || 0)),
        norm(songISRC), norm(songPlatformId),
        srcKey
    ].join('::');
}

function clearExpiredLyricsInFlight() {
    if (inFlightLyrics.size < 5000) return;
    const now = Date.now();
    for (const [key, entry] of inFlightLyrics) {
        if (now - entry.createdAt > IN_FLIGHT_TTL_MS) inFlightLyrics.delete(key);
    }
}

/**
 * Thin tracker passed around so every fetch site can record its own outcome.
 * @typedef {{ status: 'OK'|'BAD'|'RTO'|'SKIP', startedAt: number, elapsedMs: number|null }} SourceEntry
 */
function createSourceTracker(sources) {
    /** @type {Map<string, SourceEntry>} */
    const map = new Map();
    for (const s of sources) {
        map.set(s, { status: 'SKIP', startedAt: 0, elapsedMs: null });
    }
    return map;
}

function raceWithEarlyExit(promises, getPriority, threshold) {
    if (promises.length === 0) {
        return Promise.resolve({ winner: null, all: [] });
    }

    return new Promise((resolve) => {
        const results = new Array(promises.length).fill(undefined);
        const pending = new Set(promises.map((_, i) => i));
        let won = false;

        const tryResolve = () => {
            if (won) return;

            const bestIdx = results.findIndex(
                (r, i) => !pending.has(i) && r && getPriority(r) >= threshold
            );

            if (bestIdx === -1) {
                if (pending.size === 0) resolve({ winner: null, all: results });
                return;
            }

            const bestPriority = getPriority(results[bestIdx]);
            const blockedByEarlier = [...pending].some(i => i < bestIdx) && bestPriority < 3;
            if (blockedByEarlier) return;

            won = true;
            for (const pendingIndex of pending) promises[pendingIndex].abort?.();
            resolve({ winner: results[bestIdx], all: results });
        };

        promises.forEach((p, i) => {
            Promise.resolve(p)
                .catch(e => { logger.error(`Fetch error:`, e); return null; })
                .then(result => {
                    if (won) return;
                    results[i] = result;
                    pending.delete(i);
                    tryResolve();
                });
        });
    });
}

async function withTimeout(promise, ms, label = 'source', controller) {
    let timeoutId;
    const timeout = new Promise((_, reject) => {
        timeoutId = setTimeout(() => {
            controller.abort();
            reject(new Error(`Timeout after ${ms}ms: ${label}`));
        }, ms);
    });
    try {
        return await Promise.race([promise, timeout]);
    } finally {
        clearTimeout(timeoutId);
    }
}

function trackedFetch(source, fetchFn, tracker, timeoutMs) {
    const entry = tracker.get(source);
    entry.startedAt = Date.now();
    entry.status = 'BAD';

    const controller = new AbortController();
    const sourcePromise = runWithFetchSignal(controller.signal, fetchFn);
    const racePromise = withTimeout(sourcePromise, timeoutMs, source, controller)
        .then(result => {
            entry.elapsedMs = Date.now() - entry.startedAt;
            entry.status = result && result.success && result.data?.lyrics?.length > 0
                ? 'OK'
                : 'BAD';
            return result;
        })
        .catch(err => {
            entry.elapsedMs = Date.now() - entry.startedAt;
            entry.status = err?.message?.startsWith('Timeout') ? 'RTO' : 'BAD';
            return null;
        });

    racePromise.abort = () => controller.abort();
    return racePromise;
}

export async function handleSongLyrics(
    songTitle = "",
    songArtist = "",
    songAlbum = "",
    songDuration = "",
    songISRC = null,
    songPlatformId = null,
    gd,
    preferredSources = [],
    forceReload = false,
    env,
    ctx = null
) {
    if (!forceReload) {
        const key = buildRequestKey(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, preferredSources);
        const existing = inFlightLyrics.get(key);
        if (existing) {
            logger.debug(`[Dedup] Reusing in-flight lyrics request for: ${songArtist} - ${songTitle}`);
            return existing.promise;
        }

        const promise = _handleSongLyricsInner(
            songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId,
            gd, preferredSources, forceReload, env, ctx
        ).finally(() => inFlightLyrics.delete(key));

        clearExpiredLyricsInFlight();
        inFlightLyrics.set(key, { promise, createdAt: Date.now() });
        return promise;
    }

    return _handleSongLyricsInner(
        songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId,
        gd, preferredSources, forceReload, env, ctx
    );
}

async function _handleSongLyricsInner(
    songTitle = "",
    songArtist = "",
    songAlbum = "",
    songDuration = "",
    songISRC = null,
    songPlatformId = null,
    gd,
    preferredSources = [],
    forceReload = false,
    env,
    ctx = null
) {
    const globalStart = Date.now();

    const initialFileName = await FileUtils.generateUniqueFileName(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId);
    logger.debug('Looking for:', initialFileName, forceReload ? '(Force reload enabled)' : '');

    let sources;
    const isIdOnlySearch = (!songTitle || !songArtist) && (songISRC || songPlatformId);

    if (isIdOnlySearch) {
        sources = ['apple', 'lyricsplus', 'qq', 'musixmatch'];
    } else {
        sources = preferredSources.length > 0 ? preferredSources : ['apple', 'lyricsplus', 'deezer', 'qq', 'musixmatch-word', 'musixmatch'];
    }

    // Initialise every source as SKIP; only fetched ones will be updated
    const tracker = createSourceTracker(sources);

    const getSyncPriority = (result) => {
        if (!result || !result.data) return 0;

        const sourceType = result.source ? result.source.toLowerCase() : '';
        const data = result.data;
        const syncType = data.type ? data.type.toUpperCase() : '';

        if (sourceType.includes('musixmatch') || sourceType.includes('spotify') || sourceType.includes('qq') || sourceType.includes('deezer')) {
            if (syncType === 'WORD' || syncType === 'SYLLABLE') return 3;
            if (syncType === 'LINE') return 2;
            return 1;
        }

        if (sourceType.includes('apple') || sourceType.includes('lyricsplus') || sourceType.includes('qaple')) {
            return FileUtils.hasSyllableSync(data) ? 3 : syncType == 'LINE' ? 2 : 1;
        }

        return 0;
    };

    const fetchSource = (source) => {
        switch (source) {
            case 'apple':
                logger.debug(`Attempting AppleMusic Fetch`);
                return AppleMusicService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, sources);
            case 'lyricsplus':
                return (async () => {
                    logger.debug(`Attempting LyricsPlus Fetch`);
                    const lpResult = await LyricsPlusService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload);
                    if (lpResult && getSyncPriority(lpResult) === 3) {
                        return lpResult;
                    }
                    logger.debug(`Attempting Qaple Fetch (as part of LyricsPlus fallback)`);
                    const qapleResult = await QapleService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, env, sources);

                    // Only prefer qaple if it's actually better than what LyricsPlus found
                    if (qapleResult && getSyncPriority(qapleResult) > getSyncPriority(lpResult)) {
                        return qapleResult;
                    }
                    return lpResult;
                })();
            case 'deezer':
                logger.debug(`Attempting Deezer Fetch`);
                return DeezerService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, env);
            case 'musixmatch-word':
                logger.debug(`Attempting MusixMatch (Word Sync) Fetch`);
                return MusixmatchService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, env, true);
            case 'musixmatch':
                logger.debug(`Attempting MusixMatch (Line/Any Sync) Fetch`);
                return MusixmatchService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, env, false);
            case 'spotify':
                logger.debug(`Attempting Spotify (as MusixMatch alt) Fetch`);
                return SpotifyService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload);
            case 'qq':
                logger.debug(`Attempting QQ Fetch`);
                return QQService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, env);
            default:
                return Promise.resolve(null);
        }
    };

    const SOURCE_TIMEOUT_MS = 8000;
    const fetchWithTracking = (source) =>
        trackedFetch(source, () => fetchSource(source), tracker, SOURCE_TIMEOUT_MS);

    const saveResult = async (result) => {
        const exactSongTitle = result.exactMetadata?.title || result.data.metadata?.title || songTitle;
        const exactSongArtist = result.exactMetadata?.artist || result.data.metadata?.artist || songArtist;
        const exactSongAlbum = result.exactMetadata?.album || result.data.metadata?.album || songAlbum;
        const exactSongDuration = result.exactMetadata?.durationMs ? result.exactMetadata.durationMs / 1000 : (result.data.metadata?.durationMs ? result.data.metadata.durationMs / 1000 : songDuration);
        const exactSongISRC = result.exactMetadata?.isrc || result.data.metadata?.isrc || songISRC;
        const exactSongPlatformId = result.exactMetadata?.platformId || result.data.metadata?.platformId || songPlatformId;

        const finalFileName = await FileUtils.generateUniqueFileName(exactSongTitle, exactSongArtist, exactSongAlbum, exactSongDuration, exactSongISRC, exactSongPlatformId);

        if (result.rawData && result.data.cached !== 'GDrive' && result.data.cached !== 'Database' && result.source !== 'qaple') {
            const savePromise = FileUtils.saveBestLyrics(
                result.source.toLowerCase().replace('-word', ''),
                finalFileName,
                result.rawData,
                result.data,
                gd,
                exactSongTitle,
                exactSongArtist,
                exactSongAlbum,
                exactSongDuration,
                exactSongISRC,
                exactSongPlatformId,
                env
            ).catch(err => logger.error('Background cache save failed:', err));

            let hasWaitUntil = false;
            try {
                if (ctx?.executionCtx?.waitUntil) {
                    hasWaitUntil = true;
                }
            } catch (e) {}

            if (hasWaitUntil) {
                ctx.executionCtx.waitUntil(savePromise);
            } else {
                // Never block the response on cache writes. The save path can
                // stall for seconds on GDrive searches/uploads; on a 2-core host
                // that stall is what saturates concurrency and 503s everything.
                // Fire-and-forget: the save still completes in the background.
                savePromise;
            }
        }
    };

    // Build diagnostics object to attach to the winning result
    const buildDiagnostics = (winner) => {
        const sourcesStatus = {};
        for (const [source, entry] of tracker.entries()) {
            sourcesStatus[source] = {
                status: entry.status,
                ...(entry.elapsedMs !== null ? { elapsedMs: entry.elapsedMs } : {}),
            };
        }

        return {
            ...(winner ? {
                winnerSource:  winner.source || null,
                syncPriority:  winner ? getSyncPriority(winner) : null,
                cachedMeta: winner.existingFile && winner.existingFile.name ? { id: winner.existingFile.id, ...FileUtils._parseFileName(winner.existingFile.name) } : undefined
            } : {}),
            sourcesStatus,
            totalElapsedMs: Date.now() - globalStart,
        };
    };

    const isValidResult = (r) => r && r.success && r.data && r.data.lyrics && r.data.lyrics.length > 0;

    const firstTwoSources = sources.slice(0, 2);
    const firstTwoPromises = firstTwoSources.map(source => fetchWithTracking(source));

    const { winner: earlyWinner, all: firstTwoResults } = await raceWithEarlyExit(
        firstTwoPromises,
        (r) => isValidResult(r) ? getSyncPriority(r) : 0,
        3
    );

    if (earlyWinner) {
        await saveResult(earlyWinner);
        earlyWinner.diagnostics = buildDiagnostics(earlyWinner);
        return earlyWinner;
    }

    const successfulFirstTwo = firstTwoResults.filter(isValidResult);

    if (successfulFirstTwo.length > 0) {
        const bestFirstTwo = successfulFirstTwo.reduce((best, cur) =>
            getSyncPriority(cur) > getSyncPriority(best) ? cur : best
        );
        const bestPriority = getSyncPriority(bestFirstTwo);

        if (bestPriority === 2) {
            logger.debug(`Found line sync from first two sources, checking for word sync in remaining sources`);
            const remainingSources = sources.slice(2).filter(s => s !== 'musixmatch' && s !== 'spotify');

            if (remainingSources.length > 0) {
                const { winner: remainingWinner } = await raceWithEarlyExit(
                    remainingSources.map(source => fetchWithTracking(source)),
                    (r) => isValidResult(r) ? getSyncPriority(r) : 0,
                    3
                );

                if (remainingWinner) {
                    logger.debug(`Found word sync from remaining sources: ${remainingWinner.source}`);
                    await saveResult(remainingWinner);
                    remainingWinner.diagnostics = buildDiagnostics(remainingWinner);
                    return remainingWinner;
                }
            }

            logger.debug(`Using line sync result from first two sources: ${bestFirstTwo.source}`);
            await saveResult(bestFirstTwo);
            bestFirstTwo.diagnostics = buildDiagnostics(bestFirstTwo);
            return bestFirstTwo;
        }
    }

    logger.debug('No suitable lyrics from first two sources or priority <= 1, fetching all remaining sources');
    const remainingSources = sources.slice(2);

    const { winner: remainingEarlyWinner, all: remainingResults } = await raceWithEarlyExit(
        remainingSources.map(source => fetchWithTracking(source)),
        (r) => isValidResult(r) ? getSyncPriority(r) : 0,
        3
    );

    if (remainingEarlyWinner) {
        await saveResult(remainingEarlyWinner);
        remainingEarlyWinner.diagnostics = buildDiagnostics(remainingEarlyWinner);
        return remainingEarlyWinner;
    }

    const allSuccessful = [
        ...successfulFirstTwo,
        ...remainingResults.filter(isValidResult)
    ];

    if (allSuccessful.length > 0) {
        const bestResult = allSuccessful.reduce((best, cur) =>
            getSyncPriority(cur) > getSyncPriority(best) ? cur : best
        );
        await saveResult(bestResult);
        bestResult.diagnostics = buildDiagnostics(bestResult);
        return bestResult;
    }

    return {
        success: false,
        status: 404,
        diagnostics: buildDiagnostics(null),
        data: {
            message: `Lyrics not found in sources: ${sources.join(', ')}`,
            status: 404,
            details: {
                searchedSources: sources,
                songInfo: {
                    title: songTitle,
                    artist: songArtist,
                    album: songAlbum
                }
            }
        }
    };
}
