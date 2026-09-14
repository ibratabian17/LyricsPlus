// utils/similarityUtils.js

import { transliterate as tr } from 'transliteration';
import { logger } from './logger.util.js';

export class SimilarityUtils {

    static normalizeString(str) {
        if (!str) return '';
        return str.toLowerCase()
            .replace(/[^\w\s]/g, ' ')
            .replace(/\s+/g, ' ')
            .trim();
    }

    static getNGrams(str, size = 2) {
        if (!str || str.length < size) return new Set();
        const ngrams = new Set();
        for (let i = 0; i <= str.length - size; i++) {
            ngrams.add(str.substring(i, i + size));
        }
        return ngrams;
    }

    static getDiceCoefficient(str1, str2) {
        if (!str1 && !str2) return 1.0;
        if (!str1 || !str2) return 0.0;

        const bigrams1 = this.getNGrams(str1, 2);
        const bigrams2 = this.getNGrams(str2, 2);

        if (bigrams1.size === 0 && bigrams2.size === 0) return 1.0;
        if (bigrams1.size === 0 || bigrams2.size === 0) return 0.0;

        const intersection = new Set([...bigrams1].filter(x => bigrams2.has(x)));
        return (2 * intersection.size) / (bigrams1.size + bigrams2.size);
    }

    static levenshteinDistance(str1, str2) {
        if (str1 === str2) return 0;
        if (!str1.length) return str2.length;
        if (!str2.length) return str1.length;

        if (str1.length > str2.length) {
            [str1, str2] = [str2, str1];
        }

        let prevRow = Array.from({ length: str1.length + 1 }, (_, i) => i);
        let currRow = Array.from({ length: str1.length + 1 }, () => 0);

        for (let j = 1; j <= str2.length; j++) {
            currRow[0] = j;
            for (let i = 1; i <= str1.length; i++) {
                const cost = str1[i - 1] === str2[j - 1] ? 0 : 1;
                currRow[i] = Math.min(
                    prevRow[i] + 1,
                    currRow[i - 1] + 1,
                    prevRow[i - 1] + cost
                );
            }
            [prevRow, currRow] = [currRow, prevRow];
        }

        return prevRow[str1.length];
    }

    static analyzeTitle(title) {
        if (!title) return { baseTitle: '', tags: new Set(), featArtists: [], bracketContents: [] };

        const tags = new Set();
        const featArtists = [];
        const bracketContents = [];

        let cleanTitle = title.toLowerCase();

        const featRegex = /(?:\s+(?:feat\.?|ft\.?|featuring|with)\s+([^()[\]]+))(?=\s*[()[\]]|$)/gi;
        let featMatch;
        while ((featMatch = featRegex.exec(cleanTitle)) !== null) {
            const artists = featMatch[1].split(/\s*[&,]\s*/).map(a => a.trim()).filter(Boolean);
            featArtists.push(...artists);
        }
        cleanTitle = cleanTitle.replace(featRegex, ' ');

        cleanTitle = cleanTitle
            .replace(/[^\w\s\[\](){}]/g, ' ')
            .replace(/\s+/g, ' ')
            .trim();

        const anchorTagPatterns = [
            { re: /(?:[([-]|\s-\s)(remix|mix|rmx)(?:\W|$)/gi,             tag: 'remix'       },
            { re: /(?:[([-]|\s-\s)(live|concert)(?:\W|$)/gi,               tag: 'live'        },
            { re: /(?:[([-]|\s-\s)(acoustic|unplugged)(?:\W|$)/gi,         tag: 'acoustic'    },
            { re: /(?:[([-]|\s-\s)(instrumental|karaoke)(?:\W|$)/gi,       tag: 'instrumental'},
            { re: /(?:[([-]|\s-\s)(radio\s?edit|single\s?edit)(?:\W|$)/gi, tag: 'radioedit'   },
            { re: /(?:[([-]|\s-\s)(remaster(?:ed)?|rerecorded?)(?:\W|$)/gi,tag: 'remastered'  },
            { re: /(?:[([-]|\s-\s)(explicit|clean|censored)(?:\W|$)/gi,    tag: 'explicit'    },
            { re: /(?:[([-]|\s-\s)(demo|rough\s?mix?)(?:\W|$)/gi,          tag: 'demo'        },
            { re: /(?:[([-]|\s-\s)(extended|ext(?:\s|$))(?:\W|$)/gi,       tag: 'extended'    },
            { re: /(?:[([-]|\s-\s)(deluxe|anniversary|special)(?:\W|$)/gi, tag: 'deluxe'      },
            { re: /(?:[([-]|\s-\s)(mono|stereo)(?:\W|$)/gi,                tag: 'mono'        },
            { re: /(?:[([-]|\s-\s)(edit|version|ver\s)(?:\W|$)/gi,         tag: 'version'     },
            { re: /(?:[([-]|\s-\s)(sing.?along)(?:\W|$)/gi,                tag: 'singalong'   },
            { re: /(?:[([-]|\s-\s)(cover|tribute)(?:\W|$)/gi,              tag: 'cover'       },
            { re: /(?:[([-]|\s-\s)(nightcore|slowed|sped.?up)(?:\W|$)/gi,  tag: 'speedvariant'},
            { re: /(?:[([-]|\s-\s)(lofi|lo.fi)(?:\W|$)/gi,                 tag: 'lofi'        },
        ];

        anchorTagPatterns.forEach(({ re, tag }) => {
            re.lastIndex = 0;
            if (re.test(cleanTitle)) tags.add(tag);
        });

        const bracketRegex = /(?:\(([^)]*)\)|\[([^\]]*)\]|\{([^}]*)\})/g;
        const contentKeywordMap = [
            { kw: /\b(?:remix|rmx)\b/,          tag: 'remix'         },
            { kw: /\bmix\b/,                     tag: 'mix'           },
            { kw: /\b(?:live|concert)\b/,        tag: 'live'          },
            { kw: /\b(?:acoustic|unplugged)\b/,  tag: 'acoustic'      },
            { kw: /\binstrumental\b/,            tag: 'instrumental'  },
            { kw: /\bkaraoke\b/,                 tag: 'karaoke'       },
            { kw: /\bextended\b/,                tag: 'extended'      },
            { kw: /\b(?:edit|edited)\b/,         tag: 'version'       },
            { kw: /\b(?:version|ver)\b/,         tag: 'version'       },
            { kw: /\b(?:remaster(?:ed)?)\b/,     tag: 'remastered'    },
            { kw: /\b(?:demo)\b/,                tag: 'demo'          },
            { kw: /\b(?:cover|tribute)\b/,       tag: 'cover'         },
            { kw: /\b(?:nightcore|slowed)\b/,    tag: 'speedvariant'  },
            { kw: /\b(?:lofi|lo.fi)\b/,          tag: 'lofi'          },
            { kw: /\b(?:explicit|clean)\b/,      tag: 'explicit'      },
            { kw: /\b(?:mono|stereo)\b/,         tag: 'mono'          },
            { kw: /\bradio\s?edit\b/,            tag: 'radioedit'     },
            { kw: /\b(?:original\s+mix|original\s+version)\b/, tag: 'originalversion' },
        ];

        let bracketMatch;
        while ((bracketMatch = bracketRegex.exec(cleanTitle)) !== null) {
            const content = (bracketMatch[1] ?? bracketMatch[2] ?? bracketMatch[3]).trim();
            if (content) {
                bracketContents.push(content);
                contentKeywordMap.forEach(({ kw, tag }) => {
                    if (kw.test(content)) tags.add(tag);
                });
            }
        }

        cleanTitle = cleanTitle
            .replace(/\[[^\]]*\]/g, ' ')
            .replace(/\([^)]*\)/g, ' ')
            .replace(/\{[^}]*\}/g, ' ')
            .replace(/\s-\s.*$/, ' ')
            .replace(/\s+/g, ' ')
            .trim()
            .replace(/^(?:the\s+|a\s+|an\s+)/i, '')
            .replace(/\s+(?:the|a|an)$/i, '');

        return { baseTitle: cleanTitle, tags, featArtists, bracketContents };
    }

    static normalizeArtistName(artist) {
        if (!artist) return '';

        let normalized = artist.toLowerCase()
            .replace(/\[[^\]]*\]/g, '')
            .replace(/\([^)]*\)/g, '');

        const separatorRegex = /\s*(?:&|and|vs\.?|versus|x|feat\.?|ft\.?|featuring|with|,)\s*/gi;
        const artists = normalized
            .split(separatorRegex)
            .map(name => name.replace(/\bthe\b/g, '').replace(/\s+/g, ' ').trim())
            .filter(name => name.length > 0);

        return artists.sort().join(' ');
    }

    static calculateTitleSimilarity(title1, title2) {
        const computeScore = (t1, t2) => {
            if (!t1 || !t2) return 0;
            const analysis1 = this.analyzeTitle(t1);
            const analysis2 = this.analyzeTitle(t2);

            const CRITICAL_TAGS = new Set(['live', 'acoustic', 'remix', 'mix', 'instrumental', 'karaoke', 'cover', 'speedvariant', 'lofi']);

            if (analysis1.baseTitle === analysis2.baseTitle && analysis1.baseTitle.length > 0) {
                const crit1 = [...analysis1.tags].filter(t => CRITICAL_TAGS.has(t));
                const crit2 = [...analysis2.tags].filter(t => CRITICAL_TAGS.has(t));

                const oneSideCritical =
                    crit1.some(t => !analysis2.tags.has(t)) ||
                    crit2.some(t => !analysis1.tags.has(t));

                if (oneSideCritical) {
                    const bothConflict = crit1.length > 0 && crit2.length > 0 &&
                        !crit1.some(t => crit2.includes(t));
                    return bothConflict ? 0.65 : 0.72;
                }

                const candHasExtra  = analysis1.bracketContents.length > analysis2.bracketContents.length;
                const queryHasExtra = analysis2.bracketContents.length > analysis1.bracketContents.length;
                if (candHasExtra || queryHasExtra) return 0.88;

                return 1.0;
            }

            const diceScore = this.getDiceCoefficient(analysis1.baseTitle, analysis2.baseTitle);
            if (diceScore < 0.2) return 0;

            const maxLength = Math.max(analysis1.baseTitle.length, analysis2.baseTitle.length);
            const levenshteinScore = maxLength > 0 ?
                1 - (this.levenshteinDistance(analysis1.baseTitle, analysis2.baseTitle) / maxLength) : 0;

            let baseSimilarity = (diceScore * 0.7) + (levenshteinScore * 0.3);

            const tags1Critical = [...analysis1.tags].filter(t => CRITICAL_TAGS.has(t));
            const tags2Critical = [...analysis2.tags].filter(t => CRITICAL_TAGS.has(t));

            let tagPenalty = 0;
            if (tags1Critical.length > 0 && tags2Critical.length > 0) {
                const hasConflict = !tags1Critical.some(t => tags2Critical.includes(t));
                if (hasConflict) tagPenalty = 0.4;
            } else if (tags1Critical.length > 0 || tags2Critical.length > 0) {
                tagPenalty = 0.15;
            }

            return Math.max(0, baseSimilarity - tagPenalty);
        };

        const scoreOriginal = computeScore(title1, title2);
        if (scoreOriginal >= 0.8) return scoreOriginal;

        const hasNonAscii = /[^\u0000-\u007F]/.test(title1) || /[^\u0000-\u007F]/.test(title2);
        if (!hasNonAscii) return scoreOriginal;

        return Math.max(
            scoreOriginal,
            computeScore(tr(title1), tr(title2))
        );
    }

    static calculateArtistSimilarity(artist1, artist2, title1Analysis = null, title2Analysis = null) {
        const computeScore = (a1, a2, feats1, feats2) => {
            if (!a1 || !a2) return 0;

            const norm1 = this.normalizeArtistName(a1);
            const norm2 = this.normalizeArtistName(a2);

            if (norm1 === norm2) return 1.0;

            const allArtists1 = new Set();
            const allArtists2 = new Set();

            a1.split(/\s*[,&]\s*/).forEach(a => {
                const normalized = this.normalizeArtistName(a);
                if (normalized) allArtists1.add(normalized);
            });

            a2.split(/\s*[,&]\s*/).forEach(a => {
                const normalized = this.normalizeArtistName(a);
                if (normalized) allArtists2.add(normalized);
            });

            if (feats1) feats1.forEach(f => {
                const normalized = this.normalizeArtistName(f);
                if (normalized) allArtists1.add(normalized);
            });

            if (feats2) feats2.forEach(f => {
                const normalized = this.normalizeArtistName(f);
                if (normalized) allArtists2.add(normalized);
            });

            const artists1Array = [...allArtists1];
            const artists2Array = [...allArtists2];

            const overlap1to2 = artists1Array.filter(name1 => artists2Array.includes(name1)).length;
            const overlap2to1 = artists2Array.filter(name2 => artists1Array.includes(name2)).length;

            const minSize = Math.min(artists1Array.length, artists2Array.length);
            const maxOverlap = Math.max(overlap1to2, overlap2to1);

            if (minSize > 0 && maxOverlap === minSize) return 1.0;

            if (maxOverlap > 0) {
                return 0.7 + (0.3 * maxOverlap / Math.max(artists1Array.length, artists2Array.length));
            }

            return this.getDiceCoefficient(norm1, norm2);
        };

        const scoreOriginal = computeScore(
            artist1,
            artist2,
            title1Analysis?.featArtists,
            title2Analysis?.featArtists
        );

        if (scoreOriginal >= 0.8) return scoreOriginal;

        const hasNonAscii = /[^\u0000-\u007F]/.test(artist1) || /[^\u0000-\u007F]/.test(artist2);
        if (!hasNonAscii) return scoreOriginal;

        const scoreRomanized = computeScore(
            tr(artist1),
            tr(artist2),
            title1Analysis?.featArtists?.map(f => tr(f)),
            title2Analysis?.featArtists?.map(f => tr(f))
        );

        return Math.max(scoreOriginal, scoreRomanized);
    }

    static calculateDurationSimilarity(duration1, duration2) {
        if (duration1 == null || duration2 == null || duration1 <= 0 || duration2 <= 0) return 0.7;

        const diff = Math.abs(duration1 - duration2);
        if (diff === 0)   return 1.0;
        if (diff <= 1.0)  return 0.98;
        if (diff <= 2.0)  return 0.95;
        if (diff <= 4.0)  return 0.85;
        if (diff <= 7.0)  return 0.70;
        if (diff <= 12.0) return 0.50;
        if (diff <= 20.0) return 0.30;
        if (diff <= 35.0) return 0.15;
        if (diff <= 60.0) return 0.05;
        return 0.0;
    }

    static calculateAlbumSimilarity(album1, album2) {
        if (!album1 || !album2) return 0.5;

        const stripNoise = (s) => s
            .replace(/\s+-\s+(?:single|ep)\s*$/gi, '')
            .replace(/\b(?:deluxe|anniversary|special|expanded|remastered|remaster|edition|version)\b/gi, '')
            .replace(/\s+/g, ' ')
            .trim();

        const norm1 = this.normalizeString(stripNoise(album1));
        const norm2 = this.normalizeString(stripNoise(album2));

        if (norm1 === norm2) return 1.0;

        const dice = Math.max(
            this.getDiceCoefficient(norm1, norm2),
            this.getDiceCoefficient(
                this.normalizeString(stripNoise(tr(album1))),
                this.normalizeString(stripNoise(tr(album2)))
            )
        );

        return dice < 0.2 ? 0.1 : dice;
    }

    static calculateSongSimilarity(candidate, queryTitle, queryArtist, queryAlbum, queryDuration, queryISRC, queryPlatformId) {
        const attrs = candidate?.attributes || candidate;
        if (!attrs) return { score: 0, reason: 'Invalid candidate' };

        const candTitle = attrs.name || attrs.title || '';
        const candArtist = attrs.artistName || attrs.artist || '';
        const candAlbum = attrs.albumName || attrs.album || '';
        const candISRC = attrs.isrc;
        const candPlatformId = attrs.platformId;

        if (queryISRC && candISRC && queryISRC === candISRC) {
            return { score: 1.0, reason: 'Exact ISRC match', components: { titleScore: 1, artistScore: 1, albumScore: 1, durationScore: 1 } };
        }

        if (queryPlatformId && candPlatformId && queryPlatformId === candPlatformId) {
            return { score: 1.0, reason: 'Exact Platform ID match', components: { titleScore: 1, artistScore: 1, albumScore: 1, durationScore: 1 } };
        }

        if (!candTitle || !candArtist) {
            return { score: 0, reason: 'Missing title or artist', components: { titleScore: 0, artistScore: 0, albumScore: 0, durationScore: 0 } };
        }

        let candDuration;
        if (attrs.durationInMillis) {
            candDuration = attrs.durationInMillis / 1000;
        } else if (attrs.durationMs) {
            candDuration = attrs.durationMs / 1000;
        } else if (attrs.duration) {
            candDuration = attrs.duration > 1000 ? attrs.duration / 1000 : attrs.duration;
        }

        const queryTitleAnalysis = this.analyzeTitle(queryTitle);
        const candTitleAnalysis  = this.analyzeTitle(candTitle);

        const titleScore    = this.calculateTitleSimilarity(candTitle, queryTitle);
        const artistScore   = this.calculateArtistSimilarity(
            candArtist, queryArtist || '', candTitleAnalysis, queryTitleAnalysis
        );
        const albumScore    = this.calculateAlbumSimilarity(candAlbum, queryAlbum);
        const durationScore = this.calculateDurationSimilarity(candDuration, queryDuration);

        const titleThreshold  = 0.7;
        const artistThreshold = 0.6;

        if (titleScore < titleThreshold) {
            return {
                score: Math.min(0.4, titleScore * 0.5),
                reason: `Title similarity too low: ${titleScore.toFixed(3)}`,
                components: { titleScore, artistScore, albumScore, durationScore },
                durations: { query: queryDuration, candidate: candDuration }
            };
        }

        if (artistScore < artistThreshold) {
            return {
                score: Math.min(0.5, artistScore * 0.7),
                reason: `Artist similarity too low: ${artistScore.toFixed(3)}`,
                components: { titleScore, artistScore, albumScore, durationScore },
                durations: { query: queryDuration, candidate: candDuration }
            };
        }

        if (queryDuration > 0 && candDuration > 0) {
            const durDiff = Math.abs(queryDuration - candDuration);
            if (durDiff > 60) {
                return {
                    score: Math.min(0.45, (titleScore + artistScore) / 2 * 0.6),
                    reason: `Duration mismatch critical: ${durDiff.toFixed(1)}s`,
                    components: { titleScore, artistScore, albumScore, durationScore },
                    durations: { query: queryDuration, candidate: candDuration }
                };
            }
            if (durDiff > 30) {
                return {
                    score: Math.min(0.58, (titleScore + artistScore) / 2 * 0.75),
                    reason: `Duration mismatch severe: ${durDiff.toFixed(1)}s`,
                    components: { titleScore, artistScore, albumScore, durationScore },
                    durations: { query: queryDuration, candidate: candDuration }
                };
            }
        }

        const hasDuration = queryDuration > 0 && candDuration > 0;
        const hasAlbum    = !!(queryAlbum && candAlbum);

        let weights;
        if (hasAlbum && hasDuration) {
            weights = { title: 0.30, artist: 0.30, album: 0.20, duration: 0.20 };
        } else if (hasAlbum) {
            weights = { title: 0.38, artist: 0.38, album: 0.24, duration: 0.00 };
        } else if (hasDuration) {
            weights = { title: 0.38, artist: 0.35, album: 0.05, duration: 0.22 };
        } else {
            weights = { title: 0.52, artist: 0.42, album: 0.06, duration: 0.00 };
        }

        let finalScore = (titleScore    * weights.title)    +
                         (artistScore   * weights.artist)   +
                         (albumScore    * weights.album)    +
                         (durationScore * weights.duration);

        let reason = 'Good match';

        if (titleScore === 1.0 && artistScore >= 0.9) {
            finalScore = Math.min(1.0, finalScore + 0.05);
            reason = 'Exact title and artist match';
        }

        const queryHasBrackets = queryTitleAnalysis.bracketContents.length > 0;
        const candHasBrackets  = candTitleAnalysis.bracketContents.length  > 0;
        if (candHasBrackets && !queryHasBrackets) {
            finalScore = Math.max(0, finalScore - 0.07);
            if (reason === 'Good match') reason = 'Version divergence penalty';
        }

        return {
            score: Math.min(1.0, Math.max(0, finalScore)),
            reason,
            components: { titleScore, artistScore, albumScore, durationScore },
            weights,
            durations: { query: queryDuration, candidate: candDuration }
        };
    }

    static findBestSongMatch(candidates, queryTitle, queryArtist, queryAlbum, queryDuration, songISRC, songPlatformId) {
        if (!candidates?.length || !queryTitle) return null;

        const validCandidates = candidates.filter(c => {
            const attrs = c?.attributes || c;
            const title = attrs?.name || attrs?.title;
            const artist = attrs?.artistName || attrs?.artist;
            return title && artist;
        });

        if (validCandidates.length === 0) return null;

        console.debug(`🎵 Matching: "${queryArtist || 'Unknown'}" - "${queryTitle}"${queryDuration ? ` [${queryDuration}s]` : ''} (${validCandidates.length} candidates)`);

        const scoredCandidates = validCandidates.map(candidate => {
            const scoreInfo = this.calculateSongSimilarity(
                candidate, queryTitle, queryArtist, queryAlbum, queryDuration, songISRC, songPlatformId
            );
            return { candidate, scoreInfo };
        });

        scoredCandidates.sort((a, b) => {
            if (Math.abs(a.scoreInfo.score - b.scoreInfo.score) > 0.001) {
                return b.scoreInfo.score - a.scoreInfo.score;
            }

            const aHasDuration = a.scoreInfo.durations?.candidate != null;
            const bHasDuration = b.scoreInfo.durations?.candidate != null;
            if (aHasDuration !== bHasDuration) return bHasDuration ? 1 : -1;

            if (queryDuration !== undefined) {
                const bDur = b.scoreInfo.components?.durationScore ?? 0;
                const aDur = a.scoreInfo.components?.durationScore ?? 0;
                return bDur - aDur;
            }
            return 0;
        });

        if (scoredCandidates.length > 1) {
            const top = scoredCandidates.slice(0, Math.min(3, scoredCandidates.length));
            top.forEach(({ candidate, scoreInfo }, i) => {
                const a = candidate?.attributes || candidate;
                const t = a.name || a.title;
                const ar = a.artistName || a.artist;
                const c = scoreInfo.components;
                console.debug(
                    `  #${i + 1} [${scoreInfo.score.toFixed(3)}] "${ar}" - "${t}" ` +
                    `(title:${c.titleScore?.toFixed(2)} artist:${c.artistScore?.toFixed(2)} ` +
                    `album:${c.albumScore?.toFixed(2)} dur:${c.durationScore?.toFixed(2)}) — ${scoreInfo.reason}`
                );
            });
        }

        const bestMatch = scoredCandidates[0];
        const confidenceThreshold = 0.70;

        if (bestMatch.scoreInfo.score < confidenceThreshold) {
            console.debug(`❌ No match: score ${bestMatch.scoreInfo.score.toFixed(3)} < ${confidenceThreshold}`);
            console.log(bestMatch);
            return null;
        }

        if (scoredCandidates.length > 1) {
            const secondBest = scoredCandidates[1];
            const scoreGap = bestMatch.scoreInfo.score - secondBest.scoreInfo.score;

            if (scoreGap < 0.05 && bestMatch.scoreInfo.score < 0.9) {
                console.debug(`⚠️ Ambiguous match (gap: ${scoreGap.toFixed(3)}), selecting first`);
            }
        }

        const attrs = bestMatch.candidate?.attributes || bestMatch.candidate;
        console.debug(`✅ Match: "${attrs.artistName || attrs.artist}" - "${attrs.name || attrs.title}" [${bestMatch.scoreInfo.score.toFixed(3)}] — ${bestMatch.scoreInfo.reason}`);

        return bestMatch;
    }
}