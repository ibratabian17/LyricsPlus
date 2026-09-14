import { SpotifyService } from "./spotify.service.js";
import { AppleMusicService } from "./appleMusic.service.js";
import { MusixmatchService } from "./musixmatch.service.js";
import { logger } from '../utils/logger.util.js';

export class SongCatalogService {
    /**
     * Searches for songs across Apple Music, Spotify, and Musixmatch, then merges
     * the results into a single, deduplicated list.
     *
     * The process prioritizes results from Apple Music, then Spotify, then Musixmatch
     * during the merge. Songs are matched primarily by their ISRC, falling back to a
     * composite key of title, artist, and album.
     *
     * @param {string} query - The search query (e.g., song title and/or artist).
     * @param {object} env - Environment variables, required for Musixmatch authentication.
     * @returns {Promise<Array<object>>} A promise that resolves to an array of normalized song metadata.
     */
    static async search(query, env) {
        const results = await Promise.allSettled([
            this._searchAppleMusic(query),
            this._searchSpotify(query),
            this._searchMusixmatch(query, env)
        ]);

        const allResults = results.flatMap(result => {
            if (result.status === 'fulfilled') {
                return result.value;
            } else {
                logger.error("A search service failed:", result.reason);
                return [];
            }
        });

        return this._mergeSearchResults(allResults);
    }

    /**
     * @private
     * Merges and deduplicates normalized song results from various services.
     * @param {Array<object>} results - A flat array of normalized songs.
     * @returns {Array<object>} A deduplicated array of merged songs.
     */
    static _mergeSearchResults(results) {
        const finalResultsMap = new Map();
        const sourceOrder = { 'Apple Music': 1, 'Spotify': 2, 'Musixmatch': 3 };

        const sortedResults = results.sort((a, b) => {
            const sourceA = a.availability[0];
            const sourceB = b.availability[0];
            return sourceOrder[sourceA] - sourceOrder[sourceB];
        });

        for (const song of sortedResults) {
            const key = song.isrc || `${song.title}-${song.artist}-${song.album}`;
            const existingSong = finalResultsMap.get(key);

            if (existingSong) {
                Object.assign(existingSong.id, song.id);
                Object.assign(existingSong.externalUrls, song.externalUrls);
                existingSong.songwriters = [...new Set([...(existingSong.songwriters || []), ...(song.songwriters || [])])];
                existingSong.availability = [...new Set([...existingSong.availability, ...song.availability])];
                existingSong.albumArtUrl ??= song.albumArtUrl;
                existingSong.durationMs ??= song.durationMs;
            } else {
                finalResultsMap.set(key, { ...song });
            }
        }

        return Array.from(finalResultsMap.values());
    }

    /**
     * @private
     * Searches Apple Music and normalizes the results.
     */
    static async _searchAppleMusic(query) {
        try {
            const storefront = await AppleMusicService.getStorefront();
            const searchData = await AppleMusicService.searchSong(query, storefront);
            const songsData = searchData.results?.songs?.data || [];

            const normalized = await Promise.allSettled(
                songsData.slice(0, 10).map(song => AppleMusicService.normalizeAppleMusicSong(song, storefront, false))
            );
            return normalized.filter(result => result.status === 'fulfilled').map(result => result.value);
        } catch (error) {
            logger.error("Error searching Apple Music:", error);
            return [];
        }
    }

    /**
     * @private
     * Searches Spotify and normalizes the results.
     */
    static async _searchSpotify(query) {
        try {
            // Note: Spotify's search is more effective with an artist, but we use the query for the title.
            const spotifyTracks = await SpotifyService.searchSpotifySong(query, "");
            const normalized = await Promise.allSettled(
                spotifyTracks.slice(0, 10).map(track => SpotifyService.normalizeSpotifySong(track, false))
            );
            return normalized.filter(result => result.status === 'fulfilled').map(result => result.value);
        } catch (error) {
            logger.error("Error searching Spotify:", error);
            return [];
        }
    }

    /**
     * @private
     * Searches Musixmatch and normalizes the results.
     */
    static async _searchMusixmatch(query, env) {
        try {
            const searchData = await MusixmatchService.searchTrack(query, null, env);
            const tracksData = searchData.message?.body?.track_list || [];

            const normalized = await Promise.allSettled(
                tracksData.slice(0, 10).map(trackResult => {
                    const track = trackResult?.track || trackResult;
                    return MusixmatchService.normalizeMusixmatchSong(track, null, env, false);
                })
            );
            return normalized.filter(result => result.status === 'fulfilled').map(result => result.value);
        } catch (error) {
            logger.error("Error searching Musixmatch:", error);
            return [];
        }
    }
}
