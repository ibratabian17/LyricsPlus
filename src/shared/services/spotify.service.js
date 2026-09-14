import { SPOTIFY } from "../config.js";
import { FileUtils } from "../utils/file.util.js";
import { SimilarityUtils } from "../utils/similarity.util.js";
import { spotifyAccountManager } from "../config.js";
import { convertSpotifyToJSON } from "../parsers/spotify.parser.js";
import { logger } from '../utils/logger.util.js';
import { fetchWithProxy } from '../utils/fetch.util.js';

const webTokenCaches = new WeakMap();
const webTokenPromises = new WeakMap();
const spotifyTokenCaches = new WeakMap();
const spotifyTokenPromises = new WeakMap();

const SECRET_CIPHER_DICT_URL = "https://raw.githubusercontent.com/Thereallo1026/spotify-secrets/main/secrets/secretDict.json";

const FALLBACK_SECRETS = {
    "14": [62, 54, 109, 83, 107, 77, 41, 103, 45, 93, 114, 38, 41, 97, 64, 51, 95, 94, 95, 94],
    "13": [59, 92, 64, 70, 99, 78, 117, 75, 99, 103, 116, 67, 103, 51, 87, 63, 93, 59, 70, 45, 32],
    "12": [107, 81, 49, 57, 67, 93, 87, 81, 69, 67, 40, 93, 48, 50, 46, 91, 94, 113, 41, 108, 77, 107, 34],
};

const SECRET_CACHE = {
    dict: FALLBACK_SECRETS,
    lastUpdated: 0,
    updateInterval: 4 * 60 * 60 * 1000
};

const MAX_RETRIES = 3;

const USER_AGENTS = Object.freeze([
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:126.0) Gecko/20100101 Firefox/126.0"
]);

export class SpotifyService {

    // --- Public API Methods ---

    static async fetchLyrics(originalSongTitle, originalSongArtist, originalSongAlbum, originalSongDuration, songISRC, songPlatformId, gd, forceReload, cacheOnly = false) {
        let songTitle = originalSongTitle;
        let songArtist = originalSongArtist;
        let songAlbum = originalSongAlbum;
        let songDuration = originalSongDuration;
        let isrc = songISRC;
        let platformId = songPlatformId;

        try {
            const checkCache = async (title, artist, album, duration, isrc, platformId) => {
                let existingSpotifyFile;
                const isIdOnlySearch = (!title || !artist) && (isrc || platformId);

                if (isIdOnlySearch) {
                    existingSpotifyFile = await FileUtils.findExactSpByIds(gd, isrc, platformId);
                } else {
                    existingSpotifyFile = await FileUtils.findExistingSp(gd, title, artist, album, duration, isrc, platformId);
                }

                if (!forceReload && existingSpotifyFile) {
                    try {
                        const jsonContent = await gd.fetchFile(existingSpotifyFile.id);
                        if (jsonContent) {
                            const parsed = JSON.parse(jsonContent);
                            const converted = convertSpotifyToJSON(parsed);
                            converted.cached = 'GDrive';
                            return {
                                success: true,
                                data: converted,
                                source: 'Spotify',
                                rawData: parsed,
                                existingFile: existingSpotifyFile
                            };
                        }
                    } catch (error) {
                        logger.warn('Failed to fetch existing Spotify file from GDrive, will refetch.', error);
                    }
                }
                return null;
            };

            const initialCacheResult = await checkCache(songTitle, songArtist, songAlbum, songDuration, isrc, platformId);
            if (initialCacheResult) {
                logger.debug('Spotify lyrics found in cache (initial check).');
                return initialCacheResult;
            }

            if (cacheOnly) {
                logger.debug('SpotifyService: cacheOnly is true and no cache hit. Skipping remote fetch.');
                return null;
            }

            // Prioritize ISRC search when available
            let spotifyTracks = null;
            if (songISRC) {
                logger.debug(`Searching Spotify by ISRC: ${songISRC}`);
                try {
                    const isrcSearchQuery = `isrc:${encodeURIComponent(songISRC)}`;
                    const response = await this.makeSpotifyRequest(
                        `${SPOTIFY.BASE_URL}/search?q=${isrcSearchQuery}&type=track&limit=10`, {}
                    );
                    const data = await response.json();
                    spotifyTracks = data.tracks?.items?.length ? data.tracks.items : null;
                    if (spotifyTracks) {
                        logger.debug(`Spotify ISRC search found ${spotifyTracks.length} result(s) for ISRC: ${songISRC}`);
                    }
                } catch (error) {
                    logger.warn('Spotify ISRC search failed:', error);
                }
            }

            // Fall back to title/artist search if ISRC didn't find anything
            if (!spotifyTracks) {
                const isIdOnlySearch = (!originalSongTitle || !originalSongArtist) && (songISRC || songPlatformId);
                if (isIdOnlySearch) {
                    logger.debug('ISRC search found no match and no title/artist provided. Aborting Spotify search.');
                    return null;
                }
                spotifyTracks = await this.searchSpotifySong(originalSongTitle, originalSongArtist);
            }

            if (!spotifyTracks || spotifyTracks.length === 0) {
                logger.warn('No Spotify tracks found for search query.');
                return null;
            }

            const bestMatch = SimilarityUtils.findBestSongMatch(
                spotifyTracks.map(track => ({
                    attributes: {
                        name: track.name,
                        artistName: track.artists.map(a => a.name).join(', '),
                        albumName: track.album.name,
                        durationInMillis: track.duration_ms,
                        isrc: track.external_ids?.isrc || null,
                        platformId: track.id
                    },
                    id: track.id
                })),
                originalSongTitle,
                originalSongArtist,
                originalSongAlbum,
                originalSongDuration
            );

            if (!bestMatch) {
                logger.warn('No suitable Spotify track match found.');
                return null;
            }

            const spotifyTrack = spotifyTracks.find(t => t.id === bestMatch.candidate.id);
            if (!spotifyTrack) {
                logger.warn('Matched Spotify track not found in original search results.');
                return null;
            }

            songTitle = spotifyTrack.name;
            songArtist = spotifyTrack.artists.map(a => a.name).join(', ');
            songAlbum = spotifyTrack.album.name;
            songDuration = spotifyTrack.duration_ms / 1000;
            isrc = spotifyTrack.external_ids?.isrc || null;
            platformId = spotifyTrack.id;

            logger.debug(`Selected match: ${songArtist} - ${songTitle} (Album: ${songAlbum}, Duration: ${songDuration}s, ISRC: ${isrc}, PlatformId: ${platformId})`);

            const postSearchCacheResult = await checkCache(songTitle, songArtist, songAlbum, songDuration, isrc, platformId);
            if (postSearchCacheResult) {
                logger.debug('Spotify lyrics found in cache (post-search check).');
                return postSearchCacheResult;
            }

            const spotifyLyrics = await this.fetchSpotifyLyrics(spotifyTrack.id);
            if (!spotifyLyrics?.lyrics) {
                logger.warn('No lyrics found for Spotify track.');
                return null;
            }

            try {
                spotifyLyrics.lyrics.songWriters = await this.fetchSpotifySongwriters(spotifyTrack.id);
            } catch (error) {
                logger.warn('Failed to fetch songwriters for lyrics:', error);
                spotifyLyrics.lyrics.songWriters = [];
            }

            const convertedLyrics = convertSpotifyToJSON(spotifyLyrics);
            convertedLyrics.cached = 'None';

            return {
                success: true,
                data: convertedLyrics,
                source: 'Spotify',
                rawData: spotifyLyrics,
                exactMetadata: {
                    title: songTitle,
                    artist: songArtist,
                    album: songAlbum,
                    durationMs: songDuration * 1000,
                    platformId: platformId // Add platformId to exactMetadata
                }
            };
        } catch (error) {
            logger.warn('Spotify lyrics fetch failed:', error);
            return null;
        }
    }

    static async searchSpotifySong(title, artist) {
        const searchQuery = `${encodeURIComponent(title)} artist:${encodeURIComponent(artist)}`;
        const response = await this.makeSpotifyRequest(
            `${SPOTIFY.BASE_URL}/search?q=${searchQuery}&type=track&limit=10`, {}
        );
        const data = await response.json();
        return data.tracks?.items?.length ? data.tracks.items : [];
    }

    static async fetchSpotifyLyrics(trackId) {
        const currentAccount = spotifyAccountManager.getCurrentAccount();
        if (!currentAccount) throw new Error("No Spotify account available for lyrics fetch.");

        const response = await this.makeSpotifyRequest(
            `${SPOTIFY.LYRICS_URL}${trackId}?format=json&vocalRemoval=false&market=from_token`, {
            headers: { "Cookie": currentAccount.COOKIE }
        }
        );
        return response.json();
    }

    /**
     * Fetches songwriters for a given Spotify track ID.
     * @param {string} trackId - The Spotify track ID.
     * @returns {Promise<string[]>} An array of songwriter names.
     */
    static async fetchSpotifySongwriters(trackId) {
        try {
            const currentAccount = spotifyAccountManager.getCurrentAccount();
            if (!currentAccount) throw new Error("No Spotify account available for fetching songwriters.");

            const url = `https://spclient.wg.spotify.com/track-credits-view/v0/experimental/${trackId}/credits`;
            const response = await this.makeSpotifyRequest(url, {
                headers: { "Cookie": currentAccount.COOKIE }
            });

            const data = await response.json();
            if (!data?.roleCredits) {
                logger.warn("Spotify track credits response missing roleCredits:", data);
                return [];
            }
            const writersRole = data.roleCredits.find(role => role.roleTitle?.toLowerCase() === 'writers');
            return writersRole ? writersRole.artists.map(artist => artist.name) : [];
        } catch (error) {
            logger.error("Error fetching Spotify songwriters:", error);
            return [];
        }
    }

    /**
     * Normalizes a Spotify track object into the custom song catalog format.
     * @param {object} track - The Spotify track object.
     * @returns {Promise<object>} The normalized song object.
     */
    static async normalizeSpotifySong(track, hydrate = true) {
        const songwriters = hydrate ? await this.fetchSpotifySongwriters(track.id) : [];
        const albumArtUrl = track.album.images.length > 0 ? track.album.images[0].url : null;
        const isrc = track.external_ids?.isrc || null;

        return {
            id: { spotify: track.id },
            sourceId: track.id,
            title: track.name,
            artist: track.artists.map(a => a.name).join(', '),
            album: track.album.name,
            albumArtUrl: albumArtUrl,
            durationMs: track.duration_ms,
            isrc: isrc,
            songwriters: songwriters,
            availability: ['Spotify'],
            externalUrls: { spotify: track.external_urls.spotify }
        };
    }

    // --- Core Request & Authentication Logic ---

    static async makeSpotifyRequest(url, options = {}, retries = 0, account = null) {
        try {
            const currentAccount = account || spotifyAccountManager.getCurrentAccount();
            if (!currentAccount) throw new Error("No Spotify account available.");

            const headers = {
                "User-Agent": this.getRandomUserAgent(),
                ...options.headers,
            };

            if (url.includes(SPOTIFY.LYRICS_URL) || url.includes("track-credits-view")) {
                if (!headers.Authorization) {
                    const { accessToken } = await this.getSpotifyWebToken(currentAccount);
                    headers.Authorization = `Bearer ${accessToken}`;
                }
                if (!headers.Cookie) headers.Cookie = currentAccount.COOKIE;
                headers["app-platform"] = "WebPlayer";
            } else if (url.includes(SPOTIFY.AUTH_URL) || url.includes(SPOTIFY.BASE_URL)) {
                if (!headers.Authorization) {
                    const token = await this.getSpotifyAuth(currentAccount);
                    headers.Authorization = `Bearer ${token}`;
                }
            }

            const response = await fetchWithProxy(url, { ...options, headers });

            if (!response.ok) {
                if ((response.status === 401 || response.status === 429) && retries < MAX_RETRIES) {
                    logger.warn(`Spotify API call failed with status ${response.status}. Retrying with next account...`);
                    const nextAccount = spotifyAccountManager.getNextAccount(currentAccount);
                    if (nextAccount) return this.makeSpotifyRequest(url, options, retries + 1, nextAccount);
                }
                const responseText = await response.text();
                let errorMessage = responseText;
                try {
                    const errorJson = JSON.parse(responseText);
                    errorMessage = errorJson.error?.message || errorJson.error_description || errorJson.error || responseText;
                } catch {}
                throw new Error(`Spotify API returned status ${response.status}: ${errorMessage}`);
            }
            return response;
        } catch (error) {
            logger.error("Error in makeSpotifyRequest:", error);
            throw error;
        }
    }

    static async getSpotifyWebToken(account = null) {
        const currentAccount = account || spotifyAccountManager.getCurrentAccount();
        if (!currentAccount) throw new Error("No Spotify account available for web token authentication.");

        const cached = webTokenCaches.get(currentAccount);
        if (cached?.expiry > Date.now()) return cached;
        if (webTokenPromises.has(currentAccount)) return webTokenPromises.get(currentAccount);
        const promise = this._fetchSpotifyWebToken(currentAccount).finally(() => webTokenPromises.delete(currentAccount));
        webTokenPromises.set(currentAccount, promise);
        return promise;
    }

    static async _fetchSpotifyWebToken(currentAccount) {
        try {
            const { totp, totpVer } = await this.generateSpotifyTOTP();
            const headers = {
                "Cookie": currentAccount.COOKIE || "",
                "User-Agent": this.getRandomUserAgent(),
                "app-platform": "WebPlayer",
                "Referer": "https://open.spotify.com/"
            };

            const transportParams = new URLSearchParams({ reason: 'transport', productType: 'web-player', totp, totpServer: totp, totpVer: totpVer.toString() });
            let response = await fetchWithProxy(`https://open.spotify.com/api/token?${transportParams}`, { headers });

            if (!response.ok) {
                logger.warn(`Token request with reason=transport failed (${response.status}). Retrying with reason=init.`);
                const initParams = new URLSearchParams({ reason: 'init', productType: 'web-player', totp, totpServer: totp, totpVer: totpVer.toString() });
                response = await fetchWithProxy(`https://open.spotify.com/api/token?${initParams}`, { headers });
            }

            if (!response.ok) {
                const errorBody = await response.text();
                let errorMsg = errorBody;
                try {
                    const errorJson = JSON.parse(errorBody);
                    errorMsg = errorJson.error?.message || errorJson.error_description || errorJson.error || errorBody;
                } catch {}
                throw new Error(`Failed to get Spotify web token after retries. Status: ${response.status}, Body: ${errorMsg}`);
            }

            const data = await response.json();
            if (!data.clientId || !data.accessToken) {
                logger.debug("Spotify token response missing critical data:", data);
                throw new Error("Failed to get Spotify web tokens: Invalid response structure.");
            }

            const cached = {
                clientId: data.clientId,
                accessToken: data.accessToken,
                expiry: Number(data.accessTokenExpirationTimestampMs)
            };
            webTokenCaches.set(currentAccount, cached);
            return cached;
        } catch (error) {
            logger.error("Error fetching Spotify web tokens:", error);
            throw error;
        }
    }


    static async getSpotifyAuth(account = null) {
        const currentAccount = account || spotifyAccountManager.getCurrentAccount();
        if (!currentAccount) {
            logger.error("No Spotify account available for client credentials authentication.");
            return null;
        }

        const cached = spotifyTokenCaches.get(currentAccount);
        if (cached?.expiry > Date.now()) return cached.token;
        if (spotifyTokenPromises.has(currentAccount)) return spotifyTokenPromises.get(currentAccount);
        const promise = this._fetchSpotifyAuth(currentAccount).finally(() => spotifyTokenPromises.delete(currentAccount));
        spotifyTokenPromises.set(currentAccount, promise);
        return promise;
    }

    static async _fetchSpotifyAuth(currentAccount) {
        try {
            const encoded = btoa(`${currentAccount.CLIENT_ID}:${currentAccount.CLIENT_SECRET}`);
            const response = await this.makeSpotifyRequest(SPOTIFY.AUTH_URL, {
                method: 'POST',
                headers: {
                    'Authorization': `Basic ${encoded}`,
                    'Content-Type': 'application/x-www-form-urlencoded'
                },
                body: 'grant_type=client_credentials'
            }, 0, currentAccount);

            const data = await response.json();
            if (data.access_token) {
                spotifyTokenCaches.set(currentAccount, {
                    token: data.access_token,
                    expiry: Date.now() + (data.expires_in * 1000),
                });
                return data.access_token;
            } else {
                throw new Error("Failed to get Spotify token: access_token not in response.");
            }
        } catch (error) {
            logger.error("Error fetching Spotify auth token:", error);
            return null;
        }
    }

    // --- TOTP & Secrets Management ---
    /*
    - Top Secrets code are based from https://github.com/KRTirtho/spotube/blob/59f298a935c87077a6abd50656f8a4ead44bd979/lib/provider/authentication/authentication.dart#L135
    - However, it's converted from https://github.com/akashrchandran/spotify-lyrics-api/pull/47 & https://github.com/Aran404/SpotAPI/commit/9061bdd53bbfc4b983394593bad6b7d4464245ed
    */

    static async generateSpotifyTOTP() {
        const secrets = await this.getSecrets();
        const totpVer = Math.max(...Object.keys(secrets).map(Number));
        const secretCipherBytes = secrets[totpVer.toString()];

        if (!secretCipherBytes) throw new Error(`Secret for TOTP version ${totpVer} not found.`);

        const transformed = secretCipherBytes.map((e, t) => e ^ ((t % 33) + 9));
        const joined = transformed.join('');
        const derivedSecretBytes = new TextEncoder().encode(joined);

        const serverTimeResponse = await fetchWithProxy("https://open.spotify.com/", { method: 'HEAD' });
        if (!serverTimeResponse.ok || !serverTimeResponse.headers.has('date')) {
            throw new Error(`Failed to fetch Spotify server time: ${serverTimeResponse.status}`);
        }
        const serverDate = serverTimeResponse.headers.get('date');
        const serverTimeSeconds = Math.floor(new Date(serverDate).getTime() / 1000);

        const totp = await this.generateTOTP(derivedSecretBytes, serverTimeSeconds);
        return { totp, totpVer };
    }

    static async generateTOTP(secretBytes, timestamp, digits = 6, interval = 30) {
        const counter = Math.floor(timestamp / interval);
        const counterBuffer = new ArrayBuffer(8);
        const view = new DataView(counterBuffer);
        view.setUint32(0, Math.floor(counter / Math.pow(2, 32)));
        view.setUint32(4, counter % Math.pow(2, 32));

        const hmac = await this.hmacSha1(secretBytes, new Uint8Array(counterBuffer));
        const offset = hmac[19] & 0x0f;
        const binary = ((hmac[offset] & 0x7f) << 24) |
            ((hmac[offset + 1] & 0xff) << 16) |
            ((hmac[offset + 2] & 0xff) << 8) |
            (hmac[offset + 3] & 0xff);
        const otp = binary % Math.pow(10, digits);
        return otp.toString().padStart(digits, '0');
    }

    static async getSecrets() {
        if (Date.now() - SECRET_CACHE.lastUpdated > SECRET_CACHE.updateInterval) {
            await this.updateSecrets();
        }
        return SECRET_CACHE.dict;
    }

    static async updateSecrets() {
        logger.debug("Attempting to update Spotify TOTP secrets...");
        try {
            const response = await fetchWithProxy(SECRET_CIPHER_DICT_URL);
            if (!response.ok) throw new Error(`Failed to fetch secrets, status: ${response.status}`);

            const newSecrets = await response.json();
            if (typeof newSecrets === 'object' && Object.keys(newSecrets).length > 0) {
                SECRET_CACHE.dict = newSecrets;
                SECRET_CACHE.lastUpdated = Date.now();
                logger.debug("Successfully updated Spotify TOTP secrets.");
            } else {
                throw new Error("Fetched secrets data is invalid.");
            }
        } catch (error) {
            logger.warn(`Could not update Spotify secrets. Using cached/fallback version. Reason: ${error.message}`);
        }
    }

    static async hmacSha1(key, message) {
        const encoder = new TextEncoder();
        const keyData = typeof key === 'string' ? encoder.encode(key) : key;
        const messageData = typeof message === 'string' ? encoder.encode(message) : message;
        const cryptoKey = await crypto.subtle.importKey('raw', keyData, { name: 'HMAC', hash: 'SHA-1' }, false, ['sign']);
        return new Uint8Array(await crypto.subtle.sign('HMAC', cryptoKey, messageData));
    }

    // --- Utilities ---

    static extractSpDcCookie(cookieString) {
        const match = cookieString.split("; ").find(c => c.trim().startsWith("sp_dc="));
        return match ? match.trim() : null;
    }

    static getRandomUserAgent() {
        return USER_AGENTS[Math.floor(Math.random() * USER_AGENTS.length)];
    }
}
