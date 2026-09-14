import { logger } from '../utils/logger.util.js';
import { FileUtils } from '../utils/file.util.js';
import { DEEZER, GDRIVE, deezerAccountManager } from '../config.js';
import { normalizeDeezerLyrics } from '../parsers/deezer.parser.js';
import { SimilarityUtils } from '../utils/similarity.util.js';
import { fetchWithProxy } from '../utils/fetch.util.js';

const CACHE = {
	jwt: null,
	refreshToken: null,
};
let authenticationPromise = null;

export class DeezerService {

	/**
	 * Authenticate with Deezer using the new Auth API
	 */
	static async authenticate(env) {
		if (authenticationPromise) return authenticationPromise;
		authenticationPromise = this._authenticate(env).finally(() => { authenticationPromise = null; });
		return authenticationPromise;
	}

	static async _authenticate(env) {
		const currentAccount = deezerAccountManager.getCurrentAccount();
		const accountToken = currentAccount?.REFRESH_TOKEN || currentAccount?.ARL;
		const runtimeEnv = typeof process !== 'undefined' ? process.env : {};
		const token = env?.DEEZER_REFRESH_TOKEN || runtimeEnv.DEEZER_REFRESH_TOKEN || env?.DEEZER_ARL || runtimeEnv.DEEZER_ARL || accountToken;

		if (!token) {
			logger.error('Deezer credentials (DEEZER_REFRESH_TOKEN or DEEZER_ARL) are not set.');
			return false;
		}

		try {
			logger.log('Authenticating with Deezer Auth API...');

			const url = DEEZER.AUTH_URL;
			let cleanToken = token.trim();
			if (cleanToken.startsWith('refresh-token=')) {
				cleanToken = cleanToken.replace('refresh-token=', '');
			}
			const isArl = token === env?.DEEZER_ARL || token === runtimeEnv.DEEZER_ARL || token === currentAccount?.ARL;
			const cookieString = isArl ? `arl=${cleanToken}` : `refresh-token=${cleanToken}`;
			const response = await fetchWithProxy(url, {
				method: 'POST',
				headers: {
					'User-Agent':
						'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36',
					'Content-Type': 'application/json',
					Accept: '*/*',
					Cookie: cookieString,
				},
				body: '{}',
			});

			const rawText = await response.text();
			let data = {};
			try {
				data = JSON.parse(rawText);
			} catch (e) {
				logger.error('Failed to parse Deezer auth response as JSON');
			}
			if (data.jwt) {
				CACHE.jwt = data.jwt;

				let newRefreshToken = data.refresh_token;
				const setCookieHeaders = response.headers.getSetCookie ? response.headers.getSetCookie() : [];
				for (const cookie of setCookieHeaders) {
					if (cookie.startsWith('refresh-token=')) {
						newRefreshToken = cookie.split(';')[0].substring('refresh-token='.length);
					}
				}

				CACHE.refreshToken = newRefreshToken || token;

				logger.log('Deezer authentication successful.');
				return true;
			} else {
				logger.error('Failed to obtain JWT from Deezer auth.', data);
				return false;
			}
		} catch (error) {
			logger.error('Error during Deezer authentication:', error);
			return false;
		}
	}

	/**
	 * Search for tracks on Deezer.
	 */
	static async searchTrack(query, limit = 10, index = 0) {
		try {
			const url = new URL(DEEZER.SEARCH_URL);
			url.searchParams.append('q', query);
			url.searchParams.append('limit', limit.toString());
			url.searchParams.append('index', index.toString());

			const response = await fetchWithProxy(url.toString());
			const data = await response.json();
			return data.data || [];
		} catch (e) {
			logger.error('Deezer search error:', e);
			return [];
		}
	}

	static normalizeDeezerSong(track) {
		return {
			id: track.id.toString(),
			title: track.title,
			artist: track.artist?.name || '',
			album: track.album?.title || '',
			durationMs: track.duration * 1000,
			albumArtUrl: track.album?.cover_xl || track.album?.cover_large,
			isrc: track.isrc || null,
			availability: ['Deezer'],
			externalUrls: { deezer: track.link },
		};
	}

	static async _checkCache(title, artist, album, duration, isrc, platformId, gd, forceReload) {
		if (forceReload || !GDRIVE.CACHED_DEEZER) return null;

		let file;
		const isIdOnlySearch = (!title || !artist) && (isrc || platformId);

		if (isIdOnlySearch) {
			file = await FileUtils.findExactMatchByIds(gd, isrc, platformId, GDRIVE.CACHED_DEEZER, 'application/json');
		} else {
			file = await FileUtils.findExistingFile(
				gd,
				title,
				artist,
				album,
				duration,
				isrc,
				platformId,
				GDRIVE.CACHED_DEEZER,
				'application/json',
			);
		}

		if (file) {
			try {
				const content = await gd.fetchFile(file.id);
				if (content) {
					const parsed = JSON.parse(content);
					const converted = normalizeDeezerLyrics(parsed);
					if (converted) {
						converted.cached = 'GDrive';
						return { success: true, data: converted, source: 'Deezer', rawData: parsed, existingFile: file };
					}
				}
			} catch (error) {
				logger.warn('Failed to process Deezer cache file:', error);
			}
		}
		return null;
	}

	/**
	 * Fetch lyrics using the Deezer GraphQL API.
	 */
	static async fetchLyrics(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload, env) {
		try {
			const cached = await this._checkCache(songTitle, songArtist, songAlbum, songDuration, songISRC, songPlatformId, gd, forceReload);
			if (cached) return cached;

			let trackId = songPlatformId;
			if (!trackId) {
				logger.log(`Deezer fetchLyrics: searching track "${songTitle} ${songArtist}"`);
				const tracks = await this.searchTrack(`${songTitle} ${songArtist}`);
				if (tracks && tracks.length > 0) {
					const candidates = tracks.map((track) => {
						const normalized = this.normalizeDeezerSong(track);
						return {
							attributes: {
								name: normalized.title,
								artistName: normalized.artist,
								albumName: normalized.album,
								durationInMillis: normalized.durationMs,
								isrc: normalized.isrc,
								platformId: normalized.id,
							},
							originalTrack: track,
						};
					});

					const bestMatch = SimilarityUtils.findBestSongMatch(
						candidates,
						songTitle,
						songArtist,
						songAlbum,
						songDuration,
						songISRC,
						songPlatformId,
					);

					if (bestMatch && bestMatch.scoreInfo.score > 0) {
						trackId = bestMatch.candidate.originalTrack.id;
						logger.log(`Deezer fetchLyrics: found best match trackId ${trackId} with score ${bestMatch.scoreInfo.score}`);
					} else {
						logger.log(`Deezer fetchLyrics: no suitable match found in search results`);
					}
				} else {
					logger.log(`Deezer fetchLyrics: no track found`);
				}
			} else {
				logger.log(`Deezer fetchLyrics: using provided trackId ${trackId}`);
			}

			if (!trackId) return null;

			logger.log(`Deezer fetchLyrics: fetching lyrics for trackId ${trackId}`);
			const lyrics = await this.getLyrics(trackId, env);
			if (!lyrics || !lyrics.track || !lyrics.track.lyrics) {
				logger.log(`Deezer fetchLyrics: no lyrics returned for trackId ${trackId}`);
				return null;
			}

			logger.log(`Deezer fetchLyrics: successfully fetched lyrics`);
			const converted = normalizeDeezerLyrics(lyrics);
			return {
				success: true,
				data: converted,
				source: 'Deezer',
				rawData: lyrics,
			};
		} catch (error) {
			logger.error('Deezer fetch lyrics error:', error);
			return null;
		}
	}

	static async getLyrics(trackId, env = {}, retryCount = 0) {
		if (!CACHE.jwt) {
			const success = await this.authenticate(env);
			if (!success) {
				logger.warn('Skipping Deezer lyrics fetch: unauthenticated (DEEZER_ARL/DEEZER_REFRESH_TOKEN not set).');
				return null;
			}
		}

		try {
			const bodyQuery = JSON.stringify({
				operationName: 'GetLyrics',
				variables: { trackId: trackId.toString() },
				query:
					'query GetLyrics($trackId: String!) {\n  track(trackId: $trackId) {\n    id\n    lyrics {\n      id\n      text\n      ...SynchronizedWordByWordLines\n      ...SynchronizedLines\n      licence\n      copyright\n      writers\n      __typename\n    }\n    __typename\n  }\n}\n\nfragment SynchronizedWordByWordLines on Lyrics {\n  id\n  synchronizedWordByWordLines {\n    start\n    end\n    words {\n      start\n      end\n      word\n      __typename\n    }\n    __typename\n  }\n  __typename\n}\n\nfragment SynchronizedLines on Lyrics {\n  id\n  synchronizedLines {\n    lrcTimestamp\n    line\n    lineTranslated\n    milliseconds\n    duration\n    __typename\n  }\n  __typename\n}',
			});

			const response = await fetchWithProxy(DEEZER.GRAPHQL_URL, {
				method: 'POST',
				headers: {
					accept: '*/*',
					authorization: `Bearer ${CACHE.jwt}`,
					'content-type': 'application/json',
				},
				body: bodyQuery,
				mode: 'cors',
				credentials: 'include',
			});

			if (!response.ok) throw new Error(`Deezer GraphQL returned status ${response.status}`);
			const data = await response.json();

			if (data.errors && data.errors.length > 0) {
				logger.error(`Deezer GraphQL Error: ${JSON.stringify(data.errors)}`);

				// Optional: Handle token expiration and retry
				if (retryCount === 0 && (data.errors[0]?.message?.includes('token') || data.errors[0]?.message?.includes('Unauthorized'))) {
					logger.log('Deezer API token might be expired, refreshing...');
					CACHE.jwt = null;
					const retryAuth = await this.authenticate(env);
					if (retryAuth) {
						return this.getLyrics(trackId, env, retryCount + 1);
					}
				}
				return null;
			}

			return data.data || null;
		} catch (e) {
			logger.error('Deezer fetch lyrics error:', e);
			return null;
		}
	}
}
