import { qrc_decrypt } from '../utils/tripledes.util.js';
import { convertQQToJSON } from '../parsers/qq.parser.js';
import { fetchWithTimeout } from '../utils/timeout.util.js';
import { FileUtils } from '../utils/file.util.js';
import { SimilarityUtils } from '../utils/similarity.util.js';
import crypto from 'crypto';
import { logger } from '../utils/logger.util.js';

const API_CONFIG = {
    versionCode: 13020508,
    endpoint: "https://u.y.qq.com/cgi-bin/musics.fcg",
};

export class QQService {

    // --- Public API ---

    static async fetchLyrics(originalSongTitle, originalSongArtist, originalSongAlbum, originalSongDuration, songISRC, songPlatformId, gd, forceReload, env, cacheOnly = false) {
        let songTitle = originalSongTitle;
        let songArtist = originalSongArtist;
        let songAlbum = originalSongAlbum;
        let songDuration = originalSongDuration;
        let isrc = songISRC;
        let platformId = songPlatformId;

        try {
            const checkCache = async (title, artist, album, duration, isrc, platformId) => {
                logger.debug('QQService: Checking cache for QQ lyrics...');
                let existingFile;
                const isIdOnlySearch = (!title || !artist) && (isrc || platformId);

                if (isIdOnlySearch) {
                    existingFile = await FileUtils.findExactQqByIds(gd, isrc, platformId);
                } else {
                    existingFile = await FileUtils.findExistingQq(gd, title, artist, album, duration, isrc, platformId);
                }

                if (!forceReload && existingFile) {
                    try {
                        const content = await gd.fetchFile(existingFile.id);
                        if (content) {
                            const exactMetadata = {
                                title: title, artist: artist, album: album, durationMs: duration ? duration * 1000 : null, isrc: isrc, platformId: platformId
                            };
                            const converted = convertQQToJSON(content, exactMetadata);
                            if (converted) {
                                converted.cached = 'GDrive';
                                return {
                                    success: true,
                                    data: converted,
                                    source: 'QQ',
                                    rawData: content,
                                    existingFile: existingFile
                                };
                            }
                        }
                    } catch (error) {
                        logger.warn('Failed to fetch existing QQ file from GDrive, will refetch.', error);
                    }
                }
                logger.debug('QQ lyrics not found in cache (initial check).');
                return null;
            };

            const initialCacheResult = await checkCache(songTitle, songArtist, songAlbum, songDuration, isrc, platformId);
            if (initialCacheResult) {
                logger.debug('QQ lyrics found in cache (initial check).');
                return initialCacheResult;
            }

            if (cacheOnly) {
                logger.debug('QQService: cacheOnly is true and no cache hit. Skipping remote fetch.');
                return null;
            }

            const isIdOnlySearch = (!originalSongTitle || !originalSongArtist) && (songISRC || songPlatformId);
            if (isIdOnlySearch) {
                logger.debug('ID-only search failed to find a cache match. Aborting QQ search.');
                return null;
            }

            logger.debug('Searching QQ Music for lyrics...');

            // Step 1: Search for the song
            const query = [originalSongTitle, originalSongArtist].filter(Boolean).join(' ');
            if (!query) return null;

            const searchParams = {
                searchid: this.getSearchID(),
                query: query,
                search_type: 0, // SONG
                num_per_page: 5,
                page_num: 1,
                highlight: 1,
                grp: 1,
            };

            const searchData = await this.apiRequest(
                'music.search.SearchCgiService',
                'DoSearchForQQMusicMobile',
                searchParams
            );

            const items = searchData.body?.item_song || [];
            if (items.length === 0) {
                logger.warn('No suitable match found in QQ Music search.');
                return null;
            }

            // Score items
            const candidates = items.map(item => ({
                attributes: {
                    name: item.title,
                    artistName: item.singer?.[0]?.name,
                    albumName: item.album?.title,
                    durationInMillis: item.interval ? item.interval * 1000 : undefined,
                    isrc: undefined,
                    platformId: item.mid
                },
                originalItem: item
            }));

            const bestMatchWrap = SimilarityUtils.findBestSongMatch(candidates, songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId);
            if (!bestMatchWrap) {
                logger.warn('No suitable match found in QQ Music search after similarity check.');
                return null;
            }

            const bestMatch = bestMatchWrap.candidate.originalItem;
            const songMid = bestMatch.mid;

            songTitle = bestMatch.title || originalSongTitle;
            songArtist = bestMatch.singer?.[0]?.name || originalSongArtist;
            songAlbum = bestMatch.album?.title || originalSongAlbum;
            songDuration = bestMatch.interval || originalSongDuration;
            platformId = songMid;

            const exactMetadata = {
                title: songTitle,
                artist: songArtist,
                album: songAlbum,
                durationMs: songDuration ? songDuration * 1000 : null,
                isrc: null,
                platformId: platformId
            };

            const postSearchCacheResult = await checkCache(songTitle, songArtist, songAlbum, songDuration, isrc, platformId);
            if (postSearchCacheResult) {
                logger.debug('QQ lyrics found in cache (post-search check).');
                return postSearchCacheResult;
            }

            // Step 2: Fetch the lyrics for the song
            const lyricParams = {
                crypt: 1,
                ct: 11,
                cv: 13020508,
                lrc_t: 0,
                qrc: 1, // Request QRC
                qrc_t: 0,
                roma: 0,
                roma_t: 0,
                trans: 0,
                trans_t: 0,
                type: 1,
                songMid: songMid
            };

            const lyricData = await this.apiRequest(
                "music.musichallSong.PlayLyricInfo",
                "GetPlayLyricInfo",
                lyricParams
            );

            let qrcContent = "";

            if (lyricData.qrc) {
                qrcContent = await this.processLyric(lyricData.qrc);
            }

            if (!qrcContent && lyricData.lyric) {
                qrcContent = await this.processLyric(lyricData.lyric);
            }

            if (!qrcContent) {
                logger.warn('Lyrics string empty in QQ API response.');
                return null;
            }

            // Step 3: Parse and standardise
            const convertedToJson = convertQQToJSON(qrcContent, exactMetadata);

            if (!convertedToJson || !convertedToJson.lyrics || convertedToJson.lyrics.length === 0) {
                logger.warn('QQ lyrics parsing resulted in empty array.');
                return null;
            }

            convertedToJson.metadata = convertedToJson.metadata || {};
            // Enhance metadata accuracy
            convertedToJson.metadata.title = exactMetadata.title;
            convertedToJson.metadata.artist = exactMetadata.artist;
            convertedToJson.metadata.album = exactMetadata.album;

            const durMs = (exactMetadata.durationMs || 0)
            const tMin = Math.floor(durMs / 60000)
            const tSec = ((durMs % 60000) / 1000).toFixed(3)
            convertedToJson.metadata.totalDuration = tMin + ':' + String(tSec).padStart(6, '0')

            convertedToJson.cached = 'None';

            return { success: true, data: convertedToJson, source: 'QQ', rawData: qrcContent, exactMetadata };

        } catch (error) {
            logger.warn('QQ Music lyrics fetch failed:', error);
            return null;
        }
    }

    // --- Internal API ---

    static getSearchID() {
        const e = Math.floor(Math.random() * 20) + 1;
        const t = e * 18014398509481984;
        const n = Math.floor(Math.random() * 4194304) * 4294967296;
        const r = Date.now() % (24 * 60 * 60 * 1000);
        return String(t + n + r);
    }

    static buildCommonParams() {
        return {
            wid: this.getGuid(),
            cv: API_CONFIG.versionCode,
            v: API_CONFIG.versionCode,
            QIMEI36: "8888888888888888",
            ct: "11",
            tmeAppID: "qqmusic",
            format: "json",
            inCharset: "utf-8",
            outCharset: "utf-8",
            uid: "3931641530",
        };
    }

    static getGuid() {
        const chars = "0123456789ABCDEF";
        let guid = "";
        for (let i = 0; i < 32; i++) {
            guid += chars[Math.floor(Math.random() * chars.length)];
        }
        return guid;
    }

    static sha1(text) {
        return crypto.createHash('sha1').update(text).digest('hex').toUpperCase();
    }

    static generateSign(requestData) {
        const jsonStr = JSON.stringify(requestData);
        const hash = this.sha1(jsonStr);

        const part1Indexes = [23, 14, 6, 36, 16, 40, 7, 19].filter(x => x < 40);
        const part1 = part1Indexes.map(i => hash[i] || "").join("");

        const part2Indexes = [16, 1, 32, 12, 19, 27, 8, 5];
        const part2 = part2Indexes.map(i => hash[i] || "").join("");

        const scrambleValues = [89, 39, 179, 150, 218, 82, 58, 252, 177, 52, 186, 123, 120, 64, 242, 133, 143, 161, 121, 179];
        const part3Bytes = Buffer.alloc(20);
        for (let i = 0; i < scrambleValues.length; i++) {
            const hexValue = parseInt(hash.slice(i * 2, i * 2 + 2), 16);
            part3Bytes[i] = scrambleValues[i] ^ hexValue;
        }

        let b64Part = part3Bytes.toString('base64');
        b64Part = b64Part.replace(/[\/+=]/g, "");

        return `zzc${part1}${b64Part}${part2}`.toLowerCase();
    }

    static async apiRequest(module, method, params) {
        const requestData = {
            comm: this.buildCommonParams(),
            [`${module}.${method}`]: {
                module: module,
                method: method,
                param: params,
            },
        };

        const signature = this.generateSign(requestData);
        const url = `${API_CONFIG.endpoint}?sign=${signature}`;

        const headers = {
            "Content-Type": "application/json",
            "Referer": "https://y.qq.com/",
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
            "Origin": "https://y.qq.com",
        };

        const response = await fetchWithTimeout(url, {
            method: "POST",
            headers: headers,
            body: JSON.stringify(requestData),
        });

        const responseText = await response.text();
        let data;
        try {
            data = JSON.parse(responseText);
        } catch (e) {
            logger.error("QQ API returned non-JSON response:", responseText.substring(0, 200));
            throw new Error(`Invalid response structure (not JSON), status: ${response.status || 'unknown'}`);
        }

        const result = data[`${module}.${method}`];

        if (!result) {
            throw new Error("Invalid response structure");
        }

        if (result.code !== 0) {
            throw new Error(`API error: code=${result.code}`);
        }

        return result.data || result;
    }

    static async processLyric(content) {
        if (content == null || content === '') return "";

        if (typeof content === 'number') {
            return "";
        }

        if (typeof content !== 'string') {
            content = String(content);
        }

        if (content.startsWith('[')) {
            // Basic LRC
            // Actually, QQ can also wrap LRC into a similar XML structure, or return plain LRC
            // Let's create an artificial QRC wrapper if it's purely textual LRC so it parses well,
            // or just let convertQQToJSON fall back if it detects missing XML.
            // For now, if it's LRC format, our parser wants QRC XML.
            // Let's wrap it.
            if (!content.includes("<QrcInfos>")) {
                const escapedContent = content.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
                return `<?xml version="1.0" encoding="utf-8"?>\n<QrcInfos>\n<LyricInfo LyricCount="1">\n<Lyric_1 LyricType="1" LyricContent="${escapedContent}"/>\n</LyricInfo>\n</QrcInfos>`;
            }
            return content;
        }

        if (content.length % 2 === 0 && /^[0-9A-Fa-f]+$/.test(content)) {
            try {
                const decrypted = await qrc_decrypt(content);
                return decrypted || "";
            } catch (e) {
                logger.warn("Lyric decryption failed:", e.message);
                return "";
            }
        }

        return content;
    }
}
