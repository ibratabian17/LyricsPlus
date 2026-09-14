import { GDRIVE, CACHE_CONFIG } from "../config.js";
import { SimilarityUtils } from "./similarity.util.js";

const cacheWriteLocks = new Map();
const recentSavesCache = new Map();
const RECENT_SAVE_TTL_MS = 60 * 1000;
const MAX_RECENT_SAVES = 2000;

function getContentHash(data) {
    const str = typeof data === 'string' ? data : JSON.stringify(data);
    let hash = 0;
    for (let i = 0; i < str.length; i++) {
        hash = (hash << 5) - hash + str.charCodeAt(i);
        hash |= 0;
    }
    return hash;
}

/**
 * A utility class for handling file operations, specifically for generating filenames
 * and finding existing files on Google Drive based on song metadata.
 */
export class FileUtils {
    static _escapeDriveQueryLiteral(value) {
        return String(value).replace(/\\/g, '\\\\').replace(/'/g, "\\'");
    }

    static _extractKeywords(text) {
        if (!text) return [];
        const STOP_WORDS = new Set([
            'the', 'and', 'for', 'with', 'feat', 'ft', 'featuring', 'from', 'this', 'that',
            'you', 'your', 'are', 'was', 'were', 'original', 'version', 'audio', 'video'
        ]);
        const cleaned = String(text)
            .replace(/[<>[\](){}_\\/|:;!?,.*~`"@#$%^&+=]/g, ' ')
            .replace(/\s+/g, ' ')
            .trim();
        if (!cleaned) return [];

        const words = cleaned.split(' ').map(w => w.trim()).filter(Boolean);
        const keywords = [];

        for (const w of words) {
            const lower = w.toLowerCase();
            if (STOP_WORDS.has(lower)) continue;
            // Support CJK (Han, Hiragana, Katakana, Hangul) or alphanumeric length >= 2
            const isCjk = /[\u3040-\u30ff\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff\uac00-\ud7af]/.test(w);
            if (isCjk || w.length >= 2) {
                keywords.push(w);
            }
            if (keywords.length >= 2) break;
        }

        // If no keywords met length >= 2 or CJK, fallback to first non-empty word or whole cleaned string
        if (keywords.length === 0 && words.length > 0) {
            keywords.push(words[0]);
        } else if (keywords.length === 0 && cleaned.length > 0) {
            keywords.push(cleaned.slice(0, 10));
        }

        return keywords;
    }

    /**
     * Parses a structured song filename into its constituent parts.
     * @param {string} filename - The filename to parse (e.g., "Artist - Title [Album] (185.75).ext").
     * @returns {object} An object containing the parsed title, artist, album, and duration.
     * @private
     */
    static _parseFileName(filename) {
        const fileInfo = {
            title: undefined,
            artist: undefined,
            album: undefined,
            duration: undefined,
            isrc: undefined,
            platformId: undefined,
        };

        const nameWithoutExt = filename.includes('.') ? filename.substring(0, filename.lastIndexOf('.')) : filename;

        const isrcPlatformMatch = nameWithoutExt.match(/<([^>]+?)::([^>]+?)>$/);
        let nameWithoutIsrcPlatform = nameWithoutExt;
        if (isrcPlatformMatch) {
            fileInfo.isrc = isrcPlatformMatch[1] === 'null' ? undefined : isrcPlatformMatch[1].trim();
            fileInfo.platformId = isrcPlatformMatch[2] === 'null' ? undefined : isrcPlatformMatch[2].trim();
            nameWithoutIsrcPlatform = nameWithoutExt.replace(/<[^>]+?::[^>]+?>$/, '').trim();
        }

        const durationMatch = nameWithoutIsrcPlatform.match(/\s\((\d+(?:\.\d+)?)\)$/);
        let nameWithoutDuration = nameWithoutIsrcPlatform;
        if (durationMatch) {
            fileInfo.duration = parseFloat(durationMatch[1]);
            nameWithoutDuration = nameWithoutIsrcPlatform.replace(/\s\(\d+(?:\.\d+)?\)$/, '');
        }

        const albumMatch = nameWithoutDuration.match(/\s\[([^\]]+)\]$/);
        let nameWithoutAlbum = nameWithoutDuration;
        if (albumMatch) {
            fileInfo.album = albumMatch[1].trim();
            nameWithoutAlbum = nameWithoutDuration.replace(/\s\[[^\]]+\]$/, '');
        }

        const artistTitleMatch = nameWithoutAlbum.match(/^(.+?)\s*-\s*(.+)$/);
        if (artistTitleMatch) {
            fileInfo.artist = artistTitleMatch[1].trim();
            fileInfo.title = artistTitleMatch[2].trim();
        } else {
            fileInfo.title = nameWithoutAlbum.trim();
        }

        return fileInfo;
    }

    /**
     * Saves a GDrive-sourced file into the local DB cache, skipping the write
     * when an equivalent row already exists (prevents write amplification on
     * low-end hosts). Fire-and-forget from callers.
     */
    static async _backfillDbRow(parsed, file, mimeType, folderID, content) {
        const { db } = await import('./db.util.js');
        const folderId = Array.isArray(folderID) ? folderID[0] : folderID;

        if (parsed.isrc || parsed.platformId) {
            const existing = await db.findExact([folderId], mimeType, parsed.isrc, parsed.platformId);
            if (existing && existing.content) return;
        }

        await db.saveRow({
            id: crypto.randomUUID(),
            gdrive_id: file.id,
            file_name: file.name,
            folder_id: folderId,
            mime_type: mimeType,
            content,
            title: parsed.title,
            artist: parsed.artist,
            album: parsed.album,
            duration: parsed.duration,
            isrc: parsed.isrc,
            platform_id: parsed.platformId
        });
    }

    /**
     * Generates a unique, filesystem-safe filename from song metadata.
     * @param {string} songTitle - The title of the song.
     * @param {string} songArtist - The artist of the song.
     * @param {string|null} songAlbum - The album of the song.
     * @param {number|null} songDuration - The duration of the song in seconds.
     * @param {string|null} songISRC - The ISRC of the song.
     * @param {string|null} songPlatformId - The platform-specific ID of the song.
     * @returns {string} A sanitized, unique filename.
     */
    static async generateUniqueFileName(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId) {
        if (!songTitle || !songArtist) {
            console.warn("Missing song title or artist for filename generation.");
            return `unknown-${Date.now()}`;
        }

        function cleanup(text) {
            return text.replace(/[<>:"/\\|?*]/g, '').replace(/\s+/g, ' ').trim()
        }

        const albumPart = songAlbum ? ` [${cleanup(songAlbum)}]` : '';
        const durationPart = songDuration ? ` (${formatDuration(songDuration)})` : '';
        const isrcPart = (songISRC != null) ? String(songISRC).trim() : 'null';
        const platformIdPart = (songPlatformId != null) ? String(songPlatformId).trim() : 'null';
        const isrcPlatformPart = ` <${isrcPart}::${platformIdPart}>`;

        const filename = `${cleanup(songArtist)} - ${cleanup(songTitle.trim())}${albumPart}${durationPart}${isrcPlatformPart}`;

        return filename.trim();
    }

    /**
     * Searches for an existing file on Google Drive based on an exact match of ISRC or platform ID.
     * @param {object} gd - An authenticated Google Drive API instance.
     * @param {string|null} songISRC - The ISRC of the song to search for.
     * @param {string|null} songPlatformId - The platform-specific ID of the song.
     * @param {string} folderID - The ID of the Google Drive folder to search in.
     * @param {string} mimeType - The MIME type of the file to search for.
     * @returns {Promise<object|null>} The matching file object or null if not found.
     */
    static async findExactMatchByIds(gd, songISRC, songPlatformId, folderID, mimeType) {
        const folderIDs = (Array.isArray(folderID) ? folderID : [folderID]).filter(Boolean);
        if (folderIDs.length === 0) return null;
        if (!mimeType) throw new Error("MIME type is not provided.");
        if (!songISRC && !songPlatformId) return null;

        // Try local DB first
        if (CACHE_CONFIG.DB_ENABLED) {
            try {
                const { db } = await import('./db.util.js');
                const row = await db.findExact(folderIDs, mimeType, songISRC, songPlatformId);
                if (row) {
                    return {
                        id: `db:${row.id}`,
                        name: row.file_name,
                        mimeType: row.mime_type,
                        isDb: true
                    };
                }
            } catch (err) {
                console.error("Error checking exact match in local DB:", err);
            }
        }

        // Fallback to GDrive
        if (CACHE_CONFIG.GDRIVE_ENABLED) {
            if (!gd) throw new Error("Google Drive instance is not provided.");
            try {
                const folderIDs = Array.isArray(folderID) ? folderID : [folderID];
                const parentsQuery = `(${folderIDs.map(id => `'${this._escapeDriveQueryLiteral(id)}' in parents`).join(' or ')})`;
                let queryParts = [`mimeType = '${this._escapeDriveQueryLiteral(mimeType)}'`, parentsQuery];
                let searchTerm = songISRC || songPlatformId;

                queryParts.push(`name contains '${this._escapeDriveQueryLiteral(searchTerm)}'`);

                const query = queryParts.join(' and ');
                const { files } = await gd.searchFiles(query);

                if (!files || files.length === 0) return null;

                for (const file of files) {
                    const parsed = FileUtils._parseFileName(file.name);
                    let matched = false;
                    if (songISRC && parsed.isrc === songISRC) {
                        matched = true;
                    }
                    if (songPlatformId && parsed.platformId === songPlatformId) {
                        matched = true;
                    }
                    
                    if (matched) {
                        // Populate local DB cache asynchronously in background if DB is enabled
                        if (CACHE_CONFIG.DB_ENABLED) {
                            (async () => {
                                try {
                                    const parsed = FileUtils._parseFileName(file.name);
                                    const content = await gd.fetchFile(file.id);
                                    FileUtils._backfillDbRow(parsed, file, mimeType, folderID, content);
                                } catch (cacheErr) {
                                    console.error("Failed to populate DB cache from GDrive:", cacheErr);
                                }
                            })();
                        }
                        return file;
                    }
                }
            } catch (error) {
                console.error("Error searching for exact match by ID on GDrive:", error);
            }
        }

        return null;
    }

    /**
     * Searches for an existing file on Google Drive based on song metadata.
     * @param {object} gd - An authenticated Google Drive API instance.
     * @param {string} songTitle - The title of the song to search for.
     * @param {string} songArtist - The artist of the song.
     * @param {string|null} songAlbum - The album of the song.
     * @param {number|null} songDuration - The duration of the song in seconds.
     * @param {string|null} songISRC - The ISRC of the song.
     * @param {string|null} songPlatformId - The platform-specific ID of the song.
     * @param {string} folderID - The ID of the Google Drive folder to search in.
     * @param {string} mimeType - The MIME type of the file to search for.
     * @returns {Promise<object|null>} The best matching file object or null if not found.
     */
    static async findExistingFile(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, folderID, mimeType) {
        const folderIDs = (Array.isArray(folderID) ? folderID : [folderID]).filter(Boolean);
        if (folderIDs.length === 0) return null;
        if (!mimeType) throw new Error("MIME type is not provided.");
        if (!songTitle || !songArtist) return null;

        const keywords = [
            ...this._extractKeywords(songTitle),
            ...this._extractKeywords(songArtist),
            ...this._extractKeywords(songAlbum)
        ].map(k => this._escapeDriveQueryLiteral(k));

        if (keywords.length === 0) {
            if (songISRC || songPlatformId) {
                return this.findExactMatchByIds(gd, songISRC, songPlatformId, folderIDs, mimeType);
            }
            return null;
        }

        // 1. Try local DB first
        if (CACHE_CONFIG.DB_ENABLED) {
            try {
                const { db } = await import('./db.util.js');
                const rows = await db.findExisting(folderIDs, mimeType, keywords);
                
                if (rows && rows.length > 0) {
                    const adaptedCandidates = rows.map(row => {
                        return {
                            attributes: {
                                name: row.title,
                                artistName: row.artist,
                                albumName: row.album,
                                durationInMillis: row.duration ? row.duration * 1000 : undefined,
                                isrc: row.isrc,
                                platformId: row.platform_id,
                            },
                            originalFile: {
                                id: `db:${row.id}`,
                                name: row.file_name,
                                mimeType: row.mime_type,
                                isDb: true
                            },
                        };
                    });

                    const bestMatch = SimilarityUtils.findBestSongMatch(
                        adaptedCandidates,
                        songTitle,
                        songArtist,
                        songAlbum,
                        songDuration,
                        songISRC,
                        songPlatformId
                    );

                    if (bestMatch?.scoreInfo?.score > 0) {
                        console.debug("Top file match found in local DB with score:", bestMatch.scoreInfo.score);
                        return bestMatch.candidate.originalFile;
                    }
                }
            } catch (err) {
                console.error("Error searching in local DB cache:", err);
            }
        }

        // 2. Fallback to GDrive
        if (CACHE_CONFIG.GDRIVE_ENABLED) {
            if (!gd) throw new Error("Google Drive instance is not provided.");
            try {
                if (keywords.length === 0) return null;

                const folderIDs = Array.isArray(folderID) ? folderID : [folderID];
                const parentsQuery = `(${folderIDs.map(id => `'${this._escapeDriveQueryLiteral(id)}' in parents`).join(' or ')})`;
                const query = `${keywords.map(k => `name contains '${this._escapeDriveQueryLiteral(k)}'`).join(' and ')} and mimeType = '${this._escapeDriveQueryLiteral(mimeType)}' and ${parentsQuery}`;
                const { files } = await gd.searchFiles(query);

                if (!files || files.length === 0) return null;

                const adaptedCandidates = files.map(file => {
                    const parsed = FileUtils._parseFileName(file.name);
                    return {
                        attributes: {
                            name: parsed.title,
                            artistName: parsed.artist,
                            albumName: parsed.album,
                            durationInMillis: parsed.duration ? parsed.duration * 1000 : undefined,
                            isrc: parsed.isrc,
                            platformId: parsed.platformId,
                        },
                        originalFile: file,
                    };
                });

                const bestMatch = SimilarityUtils.findBestSongMatch(
                    adaptedCandidates,
                    songTitle,
                    songArtist,
                    songAlbum,
                    songDuration,
                    songISRC,
                    songPlatformId
                );

                if (bestMatch?.scoreInfo?.score > 0) {
                    console.debug("Top file match found on GDrive with score:", bestMatch.scoreInfo.score);
                    const file = bestMatch.candidate.originalFile;
                    
                    // Populate local DB cache asynchronously in background if DB is enabled
                    if (CACHE_CONFIG.DB_ENABLED) {
                        (async () => {
                            try {
                                const parsed = FileUtils._parseFileName(file.name);
                                const content = await gd.fetchFile(file.id);
                                FileUtils._backfillDbRow(parsed, file, mimeType, folderID, content);
                            } catch (cacheErr) {
                                console.error("Failed to populate DB cache from GDrive:", cacheErr);
                            }
                        })();
                    }
                    return file;
                }
            } catch (error) {
                console.error("Error searching for existing file on GDrive:", error);
            }
        }

        return null;
    }

    // --- Specific File Type Finders ---
    
    /**
     * Saves the best lyrics to Google Drive.
     * @param {string} source - The source of the lyrics (e.g., 'apple', 'musixmatch', 'spotify').
     * @param {string} fileName - The base file name for the lyrics.
     * @param {object|string} rawData - The raw data fetched from the source.
     * @param {object} convertedData - The converted lyrics data.
     * @param {object} gd - Google Drive handler.
     * @param {string} songTitle - Song title
     * @param {string} songArtist - Song artist
     * @param {string} songAlbum - Song album
     * @param {number} songDuration - Song duration
     * @param {string|null} songISRC - The ISRC of the song.
     * @param {string|null} songPlatformId - The platform-specific ID of the song.
     * @param {object} env - The Hono context environment object.
     */
    static async saveBestLyrics(...args) {
        const source = String(args[0]).toLowerCase();
        const fileName = args[1];
        const rawData = args[2];
        const key = `${source}:${fileName}`;

        // Deduplicate rapid identical saves within 60s
        const contentHash = getContentHash(rawData);
        const recent = recentSavesCache.get(key);
        if (recent && recent.contentHash === contentHash && Date.now() - recent.timestamp < RECENT_SAVE_TTL_MS) {
            return recent.result;
        }

        const previous = cacheWriteLocks.get(key) || Promise.resolve();
        const current = previous.catch(() => {}).then(async () => {
            const res = await this._saveBestLyricsUnlocked(...args);
            if (recentSavesCache.size >= MAX_RECENT_SAVES) {
                const now = Date.now();
                for (const [k, val] of recentSavesCache) {
                    if (now - val.timestamp >= RECENT_SAVE_TTL_MS) {
                        recentSavesCache.delete(k);
                    }
                }
                if (recentSavesCache.size >= MAX_RECENT_SAVES) {
                    const oldest = recentSavesCache.keys().next().value;
                    if (oldest) recentSavesCache.delete(oldest);
                }
            }
            recentSavesCache.set(key, { timestamp: Date.now(), contentHash, result: res });
            return res;
        });
        cacheWriteLocks.set(key, current);
        try {
            return await current;
        } finally {
            if (cacheWriteLocks.get(key) === current) cacheWriteLocks.delete(key);
        }
    }

    static async _saveBestLyricsUnlocked(source, fileName, rawData, convertedData, gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, env) {
        let fileId;
        const normalizedSource = source.toLowerCase().replace('-word', '');
        try {
            let folderId, mimeType, extension, payload;

            if (normalizedSource === 'apple') {
                folderId = GDRIVE.CACHED_TTML;
                mimeType = 'application/xml';
                extension = 'ttml';
                payload = rawData;
            } else if (normalizedSource === 'musixmatch') {
                folderId = GDRIVE.CACHED_MUSIXMATCH;
                mimeType = 'application/json';
                extension = 'json';
                payload = typeof rawData === 'string' ? rawData : JSON.stringify(rawData);
            } else if (normalizedSource === 'spotify') {
                folderId = GDRIVE.CACHED_SPOTIFY;
                mimeType = 'application/json';
                extension = 'json';
                payload = typeof rawData === 'string' ? rawData : JSON.stringify(rawData);
            } else if (normalizedSource === 'qq') {
                folderId = GDRIVE.CACHED_QQ;
                mimeType = 'application/xml';
                extension = 'qrc';
                payload = rawData;
            } else if (normalizedSource === 'deezer') {
                folderId = GDRIVE.CACHED_DEEZER;
                mimeType = 'application/json';
                extension = 'json';
                payload = typeof rawData === 'string' ? rawData : JSON.stringify(rawData);
            } else {
                throw new Error(`Unsupported lyrics cache source: ${source}`);
            }

            // 1. First check by exact ISRC / Platform ID
            let existingFile = null;
            if (songISRC || songPlatformId) {
                existingFile = await this.findExactMatchByIds(gd, songISRC, songPlatformId, folderId, mimeType);
            }

            // 2. Fallback to fuzzy metadata search
            if (!existingFile) {
                existingFile = await this.findExistingFile(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, folderId, mimeType);
            }

            if (existingFile) {
                fileId = await gd.updateFile(existingFile.id, payload);
            } else {
                fileId = await gd.uploadFileWithFallback(
                    `${fileName}.${extension}`,
                    mimeType,
                    payload,
                    folderId
                );
            }
            console.log(`[ASYNC] Successfully saved best lyrics for song ${songTitle} by ${songArtist} from ${source} to Google Drive.`);
            return fileId;
        } catch (error) {
            console.error(`[ASYNC] Failed to save lyrics from ${source}:`, error);
            throw error;
        }
    }

    static async findExistingTTML(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId) {
        return this.findExistingFile(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, GDRIVE.CACHED_TTML, 'application/xml');
    }

    static async findExistingSp(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId) {
        return this.findExistingFile(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, GDRIVE.CACHED_SPOTIFY, 'application/json');
    }

    static async findExistingMusixmatch(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId) {
        return this.findExistingFile(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, GDRIVE.CACHED_MUSIXMATCH, 'application/json');
    }

    static async findExistingQq(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId) {
        return this.findExistingFile(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, GDRIVE.CACHED_QQ, 'application/xml');
    }

    static async findUserJSON(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId) {
        return this.findExistingFile(gd, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, GDRIVE.USERTML_JSON, 'application/json');
    }

    static async findExactTTMLByIds(gd, songISRC, songPlatformId) {
        return this.findExactMatchByIds(gd, songISRC, songPlatformId, GDRIVE.CACHED_TTML, 'application/xml');
    }

    static async findExactSpByIds(gd, songISRC, songPlatformId) {
        return this.findExactMatchByIds(gd, songISRC, songPlatformId, GDRIVE.CACHED_SPOTIFY, 'application/json');
    }

    static async findExactMusixmatchByIds(gd, songISRC, songPlatformId) {
        return this.findExactMatchByIds(gd, songISRC, songPlatformId, GDRIVE.CACHED_MUSIXMATCH, 'application/json');
    }

    static async findExactQqByIds(gd, songISRC, songPlatformId) {
        return this.findExactMatchByIds(gd, songISRC, songPlatformId, GDRIVE.CACHED_QQ, 'application/xml');
    }

    static async findExactUserJSONByIds(gd, songISRC, songPlatformId) {
        return this.findExactMatchByIds(gd, songISRC, songPlatformId, GDRIVE.USERTML_JSON, 'application/json');
    }

    // --- JSON Content Checks ---

    /**
     * Checks if a given JSON object contains word-level or syllable-level sync data.
     * @param {object} json - The JSON object to check.
     * @returns {boolean} True if syllable sync information is present.
     */
    static hasSyllableSync(json) {
        if (!json) return false;
        const type = String(json.type || '').toUpperCase();
        if (type === "WORD" || type === "SYLLABLE") return true;
        if (Array.isArray(json.lyrics) && json.lyrics.some(l => Array.isArray(l.syllabus) && l.syllabus.length > 0)) {
            return true;
        }
        return false;
    }

    /**
     * Checks if a given JSON object contains line-level sync data.
     * @param {object} json - The JSON object to check.
     * @returns {boolean} True if line sync information is present.
     */
    static hasLineSync(json) {
        if (!json) return false;
        const type = String(json.type || '').toUpperCase();
        if (type === "LINE") return true;
        if (Array.isArray(json.lyrics) && json.lyrics.length > 0 && !this.hasSyllableSync(json)) {
            return true;
        }
        return false;
    }
}

/**
 * Formats a duration in seconds to a string with two decimal places.
 * @param {number} durationInSeconds - The duration in seconds.
 * @returns {string} The formatted duration string, or an empty string if input is invalid.
 * @private
 */
function formatDuration(durationInSeconds) {
    if (durationInSeconds === undefined || durationInSeconds === null) return '';
    return Number(durationInSeconds).toFixed(2);
}
