import { QQService } from "./qq.service.js";
import { AppleMusicService } from "./appleMusic.service.js";
import { MusixmatchService } from "./musixmatch.service.js";
import { mergeAppleMetadataIntoWordSync } from "../utils/merge.util.js";
import { logger } from '../utils/logger.util.js';
import { FileUtils } from "../utils/file.util.js";
import { runWithFetchSignal } from '../utils/fetch.util.js';

async function withTimeout(promise, ms, sourceStr) {
    let timeoutId;
    try {
        return await Promise.race([
            promise,
            new Promise((_, reject) => { timeoutId = setTimeout(() => reject(new Error(`Timeout fetching from ${sourceStr}`)), ms); })
        ]);
    } catch (err) {
        logger.error(`QapleService: ${err.message}`);
        return null;
    } finally {
        clearTimeout(timeoutId);
    }
}

async function saveResultIfNeeded(sourceStr, result, gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, env) {
    if (result && result.success && result.data && result.data.lyrics) {
        if (result.rawData && result.data.cached !== 'GDrive' && result.data.cached !== 'Database') {
            const exactSongTitle = result.exactMetadata?.title || result.data.metadata?.title || songTitle;
            const exactSongArtist = result.exactMetadata?.artist || result.data.metadata?.artist || songArtist;
            const exactSongAlbum = result.exactMetadata?.album || result.data.metadata?.album || songAlbum;
            const exactSongDuration = result.exactMetadata?.durationMs ? result.exactMetadata.durationMs / 1000 : (result.data.metadata?.durationMs ? result.data.metadata.durationMs / 1000 : songDuration);
            const exactSongISRC = result.exactMetadata?.isrc || result.data.metadata?.isrc || songISRC;
            const exactSongPlatformId = result.exactMetadata?.platformId || result.data.metadata?.platformId || songPlatformId;
            
            const fileName = await FileUtils.generateUniqueFileName(exactSongTitle, exactSongArtist, exactSongAlbum, exactSongDuration, exactSongISRC, exactSongPlatformId);
            
            await FileUtils.saveBestLyrics(sourceStr, fileName, result.rawData, result.data, gd, exactSongTitle, exactSongArtist, exactSongAlbum, exactSongDuration, exactSongISRC, exactSongPlatformId, env);
        }
    }
}

export class QapleService {
    static async fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, env, sources) {
        
        // Define the QQ Fetch task
        const fetchQQ = async () => {
            logger.debug('QapleService: Attempting to fetch word-sync from QQ...');
            const result = await withTimeout(
                QQService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, env, false),
                10000,
                'QQ'
            );
            await saveResultIfNeeded('qq', result, gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, env);
            return result;
        };

        const fetchLineSync = async () => {
            logger.debug('QapleService: Attempting to fetch line-sync from Apple Music...');
            const appleResult = await withTimeout(
                AppleMusicService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, sources || [], false),
                10000,
                'Apple Music'
            );
            await saveResultIfNeeded('apple', appleResult, gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, env);

            if (appleResult && appleResult.success && appleResult.data && appleResult.data.lyrics) {
                return { data: appleResult.data, source: 'Apple' };
            }

            logger.debug('QapleService: Apple Music fetch failed, falling back to Musixmatch line-sync...');
            const mxmResult = await withTimeout(
                MusixmatchService.fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, env, false, true),
                10000,
                'Musixmatch'
            );
            await saveResultIfNeeded('musixmatch', mxmResult, gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, env);
            
            if (mxmResult && mxmResult.success && mxmResult.data && mxmResult.data.lyrics) {
                return { data: mxmResult.data, source: 'Musixmatch' };
            }

            return null;
        };

        // Start both fetch processes concurrently
        const qqPromise = fetchQQ();
        const lineSyncController = new AbortController();
        const lineSyncPromise = runWithFetchSignal(lineSyncController.signal, fetchLineSync);

        // Wait QQ first. If QQ fails, we can immediately return to the user
        const qqResult = await qqPromise;

        if (!qqResult || !qqResult.success || !qqResult.data || !qqResult.data.lyrics) {
            lineSyncController.abort();
            logger.debug('QapleService: QQ word-sync fetch failed, aborting Qaple merge.');
            return null;
        }

        // QQ succeeds, await the line-sync result
        const lineSyncResultData = await lineSyncPromise;

        if (!lineSyncResultData) {
            logger.debug('QapleService: No line-sync component available, aborting Qaple merge.');
            return null;
        }

        const { data: lineSyncResult, source: lineSyncSource } = lineSyncResultData;

        // Merge the components
        const mergedData = mergeAppleMetadataIntoWordSync(lineSyncResult, qqResult.data);

        if (!mergedData) {
            logger.debug('QapleService: Merge aborted (already word-synced). Returning null.');
            return null;
        }

        mergedData.metadata = mergedData.metadata || {};
        mergedData.metadata.source = `Lyrics+ (via ${lineSyncSource} with QQ)`;

        return {
            success: true,
            data: mergedData,
            source: 'qaple',
            exactMetadata: qqResult.exactMetadata
        };
    }
}
