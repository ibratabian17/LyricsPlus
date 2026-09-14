// services/lyricsPlusService.js
import { FileUtils } from "../utils/file.util.js";
import { v1Tov2, normalizeV2 } from "../parsers/kpoe.parser.js";
import { GDRIVE } from "../config.js";
import { logger } from '../utils/logger.util.js';

const uploadLocks = new Map();

export class LyricsPlusService {

    static async withUploadLock(key, operation) {
        const previous = uploadLocks.get(key) || Promise.resolve();
        const current = previous.catch(() => {}).then(operation);
        uploadLocks.set(key, current);
        try {
            return await current;
        } finally {
            if (uploadLocks.get(key) === current) uploadLocks.delete(key);
        }
    }

    static async fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload = false, cacheOnly = false) {
        try {
            let userJsonFile;
            const isIdOnlySearch = (!songTitle || !songArtist) && (songISRC || songPlatformId);

            if (isIdOnlySearch) {
                userJsonFile = await FileUtils.findExactUserJSONByIds(gd, songISRC, songPlatformId);
            } else {
                userJsonFile = await FileUtils.findUserJSON(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId);
            }

            if (userJsonFile) {
                const jsonContent = await gd.fetchFile(userJsonFile.id);
                if (jsonContent) {
                    let parsedJson = JSON.parse(jsonContent);

                    const isV1Format = parsedJson.lyrics?.length > 0 &&
                        typeof parsedJson.lyrics[0].syllabus === 'undefined';

                    // Convert v1 -> v2 first, then normalize either way.
                    // normalizeV2 is a no-op if the data is already in new format.
                    let lyricsData = normalizeV2(isV1Format ? v1Tov2(parsedJson) : parsedJson);

                    if (isV1Format) {
                        logger.debug("V1 lyrics format detected. Converted and normalized to V2.");
                    }

                    if (FileUtils.hasSyllableSync(lyricsData) || FileUtils.hasLineSync(lyricsData)) {
                        lyricsData.metadata = lyricsData.metadata || {};
                        lyricsData.metadata.source = 'Lyrics+';
                        lyricsData.cached = 'UserJSON';
                        return { success: true, data: lyricsData, source: 'lyricsplus', existingFile: userJsonFile };
                    }
                }
            }
        } catch (error) {
            logger.warn('Failed to check user JSON:', error);
        }
        return null;
    }

    static async uploadTimelineLyrics(gd, songTitle, songArtist, songAlbum, songDuration, lyricsData, forceUpload = false, songISRC = null, songPlatformId = null) {
        const fileName = await FileUtils.generateUniqueFileName(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId);
        return this.withUploadLock(fileName, () => this.uploadTimelineLyricsUnlocked(
            gd, songTitle, songArtist, songAlbum, songDuration, lyricsData, forceUpload, songISRC, songPlatformId, fileName
        ));
    }

    static async uploadTimelineLyricsUnlocked(gd, songTitle, songArtist, songAlbum, songDuration, lyricsData, forceUpload, songISRC, songPlatformId, fileName) {
        try {
            if (!lyricsData.type || !lyricsData.metadata || !lyricsData.lyrics) {
                return { success: false, error: "Missing required fields: type or lyrics" };
            }

            // Ensure uploaded data is always in new format
            lyricsData = normalizeV2(lyricsData);

            const fullFileName = `${fileName}.json`;

            const existingUGCFile = await FileUtils.findExistingFile(
                gd,
                songTitle,
                songArtist,
                songAlbum,
                songDuration,
                songISRC,
                songPlatformId,
                GDRIVE.USERTML_JSON,
                'application/json'
            );

            const isExactMatch = existingUGCFile && existingUGCFile.name === fullFileName;

            if (existingUGCFile && isExactMatch && forceUpload) {
                const previousContent = await gd.fetchFile(existingUGCFile.id);
                if (previousContent) {
                    const previousData = JSON.parse(previousContent);
                    if (this.isVandalismUpdate(previousData, lyricsData)) {
                        logger.warn("Vandalism detected in the update. Update aborted.");
                        return { success: false, error: "Vandalism detected. Update aborted." };
                    }
                }
                await gd.updateFile(existingUGCFile.id, JSON.stringify(lyricsData));
                logger.debug(`Updated existing file: ${fullFileName}`);
            } else if (!existingUGCFile || !isExactMatch) {
                await gd.uploadFileWithFallback(
                    fullFileName,
                    'application/json',
                    JSON.stringify(lyricsData),
                    GDRIVE.USERTML_JSON
                );
                logger.debug(`Uploaded new file: ${fullFileName}`);
            } else {
                logger.debug("File already exists and forceUpload is false. No upload performed.");
                return { success: false, error: "File exists. Set forceUpload to true to update." };
            }
            return { success: true };
        } catch (error) {
            logger.error("Error uploading timeline lyrics:", error);
            return { success: false, error };
        }
    }

    static isVandalismUpdate(previousData, newData) {
        if (!Array.isArray(newData?.lyrics) || newData.lyrics.length === 0) return true;
        const hasInvalidTimeline = newData.lyrics.some(line =>
            !Number.isFinite(Number(line?.time)) || !Number.isFinite(Number(line?.duration)) ||
            Number(line.time) < 0 || Number(line.duration) < 0
        );
        if (hasInvalidTimeline) return true;

        const textLength = data => (data?.lyrics || []).reduce((total, line) => total + String(line?.text || '').trim().length, 0);
        const previousTextLength = textLength(previousData);
        const newTextLength = textLength(newData);
        return newTextLength === 0 || (previousTextLength >= 100 && newTextLength < previousTextLength * 0.2);
    }
}
