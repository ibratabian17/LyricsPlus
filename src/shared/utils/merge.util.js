const CANDIDATE_TOLERANCE_MS  = 2000;
const GAP_APPLE               = 0.35;
const GAP_QQ                  = 0.20;

const WORD_MAX_QQ_MERGE       = 3;
const WORD_CROSS_WORD_PENALTY = 0.20;  // per word boundary crossed on the N-Apple→1-QQ path

const SYL_MAX_QQ_MERGE        = 12;   // long words like "mendewasakanku" can be 7+ syllables
const SYL_CROSS_WORD_PENALTY  = 9999; // syllables never straddle Apple word boundaries

const SEQ_PENALTY_RATE        = 1e-7; // earlier seqIdx wins on tied cost
const INTRA_WINDOW_NUDGE      = 0.05; // token closer to lineStart wins on tied cost

const SYNTH_BAIL_THRESHOLD    = 0.50; // abort merge if >50 % of tokens are synthetic
const OFFSET_MIN_MS           = 1000; // ignore sub-second systematic offsets
const OFFSET_MAX_MAD_MS       = 3000; // reject offset estimate if QQ spread is too wide

//  Mode detection 

// Returns 'syllable' if QQ entries look like sub-word fragments, 'word' otherwise.
// Heuristics: >55% of entries are <4 chars, or >40% end with a hyphen.
function detectQQMode(wordSyncData) {
    const lines = wordSyncData?.lyrics ?? [];
    let totalEntries = 0, shortEntries = 0, hyphenEntries = 0;
    const SAMPLE = Math.min(lines.length, 30);

    for (let i = 0; i < SAMPLE; i++) {
        for (const syl of (lines[i].syllabus ?? [])) {
            const t = (syl.text ?? '').trim();
            if (!t) continue;
            totalEntries++;
            if (t.replace(/[^a-zA-ZÀ-ÿ]/g, '').length < 4) shortEntries++;
            if (t.endsWith('-')) hyphenEntries++;
        }
    }

    if (totalEntries === 0) return 'word';
    if (shortEntries / totalEntries > 0.55) return 'syllable';
    if (hyphenEntries / totalEntries > 0.40) return 'syllable';
    return 'word';
}

//  Text utilities 

function normalizeForMatch(text) {
    if (!text) return '';
    return text.toLowerCase()
        .replace(/\*+/g, '')
        .replace(/[^\w\s']/g, '')
        .replace(/(?<!\w)'+/g, '')  // strip leading/floating apostrophes (e.g. "'ku" → "ku")
        .replace(/\s+/g, ' ')
        .trim();
}

function diceCoefficient(a, b) {
    if (!a && !b) return 1.0;
    if (!a || !b) return 0.0;
    if (a.length < 2 || b.length < 2) return a === b ? 1.0 : 0.0;
    const bg1 = new Set(), bg2 = new Set();
    for (let i = 0; i < a.length - 1; i++) bg1.add(a.substring(i, i + 2));
    for (let i = 0; i < b.length - 1; i++) bg2.add(b.substring(i, i + 2));
    let hit = 0;
    for (const g of bg1) if (bg2.has(g)) hit++;
    return (2 * hit) / (bg1.size + bg2.size);
}

function textSimilarity(n1, n2) {
    if (!n1 || !n2) return 0;
    if (n1 === n2) return 1.0;

    const short = Math.min(n1.length, n2.length);
    if (short <= 2) {
        if (n1.startsWith(n2) || n2.startsWith(n1)) return 0.7;
        return 0;
    }

    if (n1.length >= 4 && n2.includes(n1)) return 0.8 + 0.2 * (n1.length / n2.length);
    if (n2.length >= 4 && n1.includes(n2)) return n2.length / n1.length;

    const w1 = n1.split(' ').filter(w => w.length > 1);
    const w2 = new Set(n2.split(' ').filter(w => w.length > 1));
    if (w1.length >= 2 && w2.size >= 2) {
        let overlap = 0;
        for (const w of w1) if (w2.has(w)) overlap++;
        const r = overlap / w1.length;
        if (r >= 0.5) return 0.6 + r * 0.35;
    }

    return diceCoefficient(n1, n2);
}

// Rewards a partial syllable prefix so the DP stays committed while building
// up "men"→"mende"→"mendewa"→"mendewasakanku" across multiple QQ tokens.
function textSimilaritySyllable(appleNorm, combinedSylNorm) {
    if (!appleNorm || !combinedSylNorm) return 0;
    if (appleNorm === combinedSylNorm) return 1.0;
    if (appleNorm.startsWith(combinedSylNorm))
        return 0.5 + 0.5 * (combinedSylNorm.length / appleNorm.length);
    if (combinedSylNorm.startsWith(appleNorm))
        return appleNorm.length / combinedSylNorm.length;
    return textSimilarity(appleNorm, combinedSylNorm);
}

//  Tokenisation 

function splitHyphenated(word) {
    const parts = [];
    let cur = '';
    for (let i = 0; i < word.length; i++) {
        cur += word[i];
        if (
            word[i] === '-' &&
            i > 0 && i < word.length - 1 &&
            /[a-zA-ZÀ-ÿ\d]/.test(word[i - 1]) &&
            /[a-zA-ZÀ-ÿ\d]/.test(word[i + 1])
        ) {
            parts.push(cur);
            cur = '';
        }
    }
    if (cur) parts.push(cur);
    return parts.length > 1 ? parts : [word];
}

function tokenizeAppleLine(text) {
    const tokens = [];
    for (const word of text.split(/\s+/).filter(Boolean)) {
        const parts = splitHyphenated(word);
        for (let i = 0; i < parts.length; i++) {
            tokens.push({
                text:      parts[i],
                norm:      normalizeForMatch(parts[i]),
                wordFinal: i === parts.length - 1,
            });
        }
    }
    return tokens;
}

//  Timing offset detection 

// Compares Apple line texts against QQ line texts to detect a systematic time
// offset (e.g. Apple cues lines early for display; QQ timestamps vocal onsets).
// Returns the median delta in ms, or 0 if the offset is negligible / inconsistent.
function estimateLineOffset(appleLines, wordLines) {
    const deltas = [];
    for (const al of appleLines) {
        if (!al.time || !al.text) continue;
        const aNorm = normalizeForMatch(al.text);
        let bestSim = 0.5, bestDelta = null;
        for (const ql of wordLines) {
            if (ql.time == null) continue;
            const sim = textSimilarity(aNorm, normalizeForMatch(ql.text ?? ''));
            if (sim > bestSim) { bestSim = sim; bestDelta = ql.time - al.time; }
        }
        if (bestDelta !== null) deltas.push(bestDelta);
    }
    if (!deltas.length) return 0;
    deltas.sort((a, b) => a - b);
    const median = deltas[Math.floor(deltas.length / 2)];
    const mad    = deltas.reduce((s, d) => s + Math.abs(d - median), 0) / deltas.length;
    if (mad > OFFSET_MAX_MAD_MS) {
        console.debug(`estimateLineOffset: spread too wide (MAD ${mad.toFixed(0)} ms) – skipping offset`);
        return 0;
    }
    if (Math.abs(median) < OFFSET_MIN_MS) return 0;
    console.debug(`estimateLineOffset: ${median > 0 ? '+' : ''}${median} ms (MAD ${mad.toFixed(0)} ms)`);
    return median;
}

//  QQ pool 

function flattenQQIntoWordPool(wordLines) {
    const pool = [];
    let seq = 0;

    for (const line of wordLines) {
        for (const syl of (line.syllabus || [])) {
            const words = (syl.text ?? '').trim().split(/\s+/).filter(Boolean);
            if (!words.length) continue;

            if (words.length === 1) {
                pool.push({
                    text: words[0],
                    norm: normalizeForMatch(words[0]),
                    time: syl.time,
                    duration: syl.duration,
                    seqIdx: seq++,
                });
            } else {
                const totalLen = words.reduce((s, w) => s + w.length, 0) || words.length;
                let t = syl.time;
                for (const w of words) {
                    const dur = Math.round((w.length / totalLen) * syl.duration);
                    pool.push({
                        text: w,
                        norm: normalizeForMatch(w),
                        time: t,
                        duration: dur,
                        seqIdx: seq++,
                    });
                    t += dur;
                }
            }
        }
    }

    return pool;
}

function collectCandidates(appleLines, qqPool, lineOffset = 0) {
    const map = new Map();
    for (let i = 0; i < appleLines.length; i++) map.set(i, []);

    for (const tok of qqPool) {
        for (let a = 0; a < appleLines.length; a++) {
            const al = appleLines[a];
            // Skip lines whose timing was never populated – they would create a
            // bogus [-tolerance, +tolerance] window around 0 and collect every
            // early QQ token.
            if ((al.time == null || al.time === 0) && !al.duration) continue;
            // Shift Apple's window by the detected systematic offset so that QQ
            // tokens (which may be timed to vocal onset rather than display cue)
            // still land inside the candidate window.
            const centre = al.time + lineOffset;
            const lo = centre - CANDIDATE_TOLERANCE_MS;
            const hi = centre + (al.duration || 0) + CANDIDATE_TOLERANCE_MS;
            if (tok.time >= lo && tok.time <= hi) map.get(a).push(tok);
        }
    }
    return map;
}

//  DP alignment 

function alignTokensToQQ(appleTokens, qqTokens, lineStart, lineEnd, mode) {
    const A = appleTokens.length;
    const Q = qqTokens.length;

    if (A === 0) return { matches: [], residuals: [...qqTokens] };
    if (Q === 0) return { matches: new Array(A).fill(null), residuals: [] };

    const isSyllable     = mode === 'syllable';
    const MAX_QQ_MERGE   = isSyllable ? SYL_MAX_QQ_MERGE       : WORD_MAX_QQ_MERGE;
    const CROSS_WORD_PEN = isSyllable ? SYL_CROSS_WORD_PENALTY  : WORD_CROSS_WORD_PENALTY;
    const simFn          = isSyllable ? textSimilaritySyllable   : textSimilarity;

    const lineDur           = Math.max(1, lineEnd - lineStart);
    const TIME_PENALTY_RATE = 1.5 / lineDur;

    const timePenalty = (t) => {
        if (t < lineStart) return (lineStart - t) * TIME_PENALTY_RATE;
        if (t > lineEnd)   return (t - lineEnd)   * TIME_PENALTY_RATE;
        return ((t - lineStart) / lineDur) * INTRA_WINDOW_NUDGE;
    };

    const INF = 1e9;
    const dp  = Array.from({ length: A + 1 }, () => new Float64Array(Q + 1).fill(INF));
    const op  = Array.from({ length: A + 1 }, () => new Array(Q + 1).fill(null));
    dp[0][0] = 0;

    for (let a = 0; a <= A; a++) {
        for (let q = 0; q <= Q; q++) {
            const cur = dp[a][q];
            if (cur >= INF) continue;

            if (a < A) {
                // 1 Apple : 1..MAX_QQ_MERGE QQ tokens
                for (let dq = 1; dq <= MAX_QQ_MERGE && q + dq <= Q; dq++) {
                    let combined = '', tPenalty = 0, seqPenalty = 0;
                    for (let k = 0; k < dq; k++) {
                        combined   += qqTokens[q + k].norm;
                        tPenalty   += timePenalty(qqTokens[q + k].time);
                        seqPenalty += qqTokens[q + k].seqIdx * SEQ_PENALTY_RATE;
                    }
                    const sim = simFn(appleTokens[a].norm, combined);
                    const c   = cur + (1 - sim) + (dq - 1) * GAP_QQ + tPenalty / dq + seqPenalty;
                    if (c < dp[a + 1][q + dq]) {
                        dp[a + 1][q + dq] = c;
                        op[a + 1][q + dq] = { type: 'match', da: 1, dq };
                    }
                }

                // N Apple tokens : 1 QQ token
                if (q < Q) {
                    for (let da = 2; a + da <= A; da++) {
                        let combined = '', crossWordPenalty = 0;
                        for (let k = 0; k < da; k++) {
                            combined += appleTokens[a + k].norm;
                            if (k < da - 1 && appleTokens[a + k].wordFinal)
                                crossWordPenalty += CROSS_WORD_PEN;
                        }
                        if (crossWordPenalty >= INF) break;
                        // Only allow merge when lengths are roughly comparable.
                        // Prevents "di"+"sini"+"sini"... absorbing tokens just because
                        // combined.startsWith(qqNorm) or dice stabilises at 0.4.
                        if (combined.length > qqTokens[q].norm.length * 1.5) break;
                        const sim = textSimilarity(combined, qqTokens[q].norm);
                        if (sim < 0.40) break;
                        const seqPenalty = qqTokens[q].seqIdx * SEQ_PENALTY_RATE;
                        const c = cur + (1 - sim)
                                + timePenalty(qqTokens[q].time) + crossWordPenalty + seqPenalty;
                        if (c < dp[a + da][q + 1]) {
                            dp[a + da][q + 1] = c;
                            op[a + da][q + 1] = { type: 'match', da, dq: 1 };
                        }
                    }
                }

                const ca = cur + GAP_APPLE;
                if (ca < dp[a + 1][q]) { dp[a + 1][q] = ca; op[a + 1][q] = { type: 'skip_apple' }; }
            }

            if (q < Q) {
                const cq = cur + GAP_QQ;
                if (cq < dp[a][q + 1]) { dp[a][q + 1] = cq; op[a][q + 1] = { type: 'skip_qq' }; }
            }
        }
    }

    const matches   = new Array(A).fill(null);
    const residuals = [];
    let a = A, q = Q;

    while (a > 0 || q > 0) {
        const o = op[a][q];
        if (!o) break;

        if (o.type === 'match') {
            const { da, dq } = o;

            if (da === 1) {
                const fused = qqTokens.slice(q - dq, q);
                if (dq === 1) {
                    matches[a - 1] = { time: fused[0].time, duration: fused[0].duration };
                } else {
                    matches[a - 1] = {
                        parts: fused.map(t => ({ time: t.time, duration: t.duration, norm: t.norm })),
                    };
                }
                a -= 1; q -= dq;
            } else {
                // N Apple tokens matched to 1 QQ token — emit as a single merged syllable
                // using the original Apple text and QQ's exact timing.
                const qqTok = qqTokens[q - 1];
                matches[a - da] = { time: qqTok.time, duration: qqTok.duration, mergeCount: da };
                for (let k = 1; k < da; k++) matches[a - da + k] = { absorbed: true };
                a -= da; q -= 1;
            }
        } else if (o.type === 'skip_apple') {
            a--;
        } else {
            residuals.push(qqTokens[q - 1]);
            q--;
        }
    }

    residuals.sort((x, y) => x.seqIdx - y.seqIdx);
    return { matches, residuals };
}

//  Syllabus builder 

function anchorSyntheticsWithResiduals(runTokens, residuals, leftEnd, rightStart) {
    const n      = runTokens.length;
    const result = new Array(n).fill(null);
    if (!residuals.length) return result;

    const inGap = residuals.filter(r => r.time >= leftEnd - 80 && r.time <= rightStart + 80);
    if (!inGap.length) return result;

    const used = new Set();
    let minSeq = -1;

    for (let k = 0; k < n; k++) {
        let bestSim = 0.55, bestIdx = -1;
        for (let r = 0; r < inGap.length; r++) {
            if (used.has(r) || inGap[r].seqIdx <= minSeq) continue;
            const sim = textSimilarity(runTokens[k].norm, inGap[r].norm);
            if (sim > bestSim) { bestSim = sim; bestIdx = r; }
        }
        if (bestIdx >= 0) {
            result[k] = { time: inGap[bestIdx].time, duration: inGap[bestIdx].duration };
            minSeq    = inGap[bestIdx].seqIdx;
            used.add(bestIdx);
        }
    }

    return result;
}

function splitWordByNorms(appleText, qqNorms) {
    if (qqNorms.length <= 1) return [appleText];

    const m     = appleText.match(/^(.*?)([,\.\s!?»«"'()[\]{}]*)$/s);
    const base  = m ? m[1] : appleText;
    const trail = m ? m[2] : '';
    const parts = [];
    let pos = 0;

    for (let i = 0; i < qqNorms.length; i++) {
        if (i === qqNorms.length - 1 || pos >= base.length) {
            parts.push(base.slice(pos) + trail);
            break;
        }
        const len = Math.min(qqNorms[i].length, base.length - pos);
        parts.push(base.slice(pos, pos + len));
        pos += len;
    }

    return parts;
}

function buildMergedText(appleTokens, startIdx, count) {
    let text = appleTokens[startIdx].text;
    for (let k = 1; k < count; k++) {
        const sep = appleTokens[startIdx + k - 1].wordFinal ? ' ' : '';
        text += sep + appleTokens[startIdx + k].text;
    }
    return text;
}

function buildSyllabus(appleTokens, matches, residuals, lineStart, lineEnd) {
    const n = appleTokens.length;
    if (n === 0) return [];

    const expandedTokens = [];
    const expandedTiming = [];

    let i = 0;
    while (i < n) {
        const m   = matches[i];
        const tok = appleTokens[i];

        if (m?.absorbed) {
            i++; continue;
        }

        if (m?.mergeCount && m.mergeCount > 1) {
            const mergedText = buildMergedText(appleTokens, i, m.mergeCount);
            expandedTokens.push({
                text:      mergedText,
                norm:      normalizeForMatch(mergedText),
                wordFinal: appleTokens[i + m.mergeCount - 1].wordFinal,
            });
            expandedTiming.push({ time: m.time, duration: m.duration });
            i += m.mergeCount;
            continue;
        }

        if (m?.parts) {
            const textParts = splitWordByNorms(tok.text, m.parts.map(p => p.norm));
            for (let k = 0; k < m.parts.length; k++) {
                expandedTokens.push({
                    text:      textParts[k] ?? '',
                    norm:      m.parts[k].norm,
                    wordFinal: k === m.parts.length - 1,
                });
                expandedTiming.push({ time: m.parts[k].time, duration: m.parts[k].duration });
            }
        } else {
            expandedTokens.push(tok);
            expandedTiming.push(m ? { time: m.time, duration: m.duration } : null);
        }

        i++;
    }

    const N = expandedTokens.length;

    let ei = 0;
    while (ei < N) {
        if (expandedTiming[ei] !== null) { ei++; continue; }
        let j = ei;
        while (j < N && expandedTiming[j] === null) j++;
        const leftEnd    = ei > 0 && expandedTiming[ei - 1] ? expandedTiming[ei - 1].time + expandedTiming[ei - 1].duration : lineStart;
        const rightStart = j  < N && expandedTiming[j]      ? expandedTiming[j].time                                         : lineEnd;
        const anchored   = anchorSyntheticsWithResiduals(expandedTokens.slice(ei, j), residuals, leftEnd, rightStart);
        for (let k = 0; k < anchored.length; k++) {
            if (anchored[k]) expandedTiming[ei + k] = anchored[k];
        }
        ei = j;
    }

    const result = [];
    ei = 0;
    while (ei < N) {
        const isLineLast    = ei === N - 1;
        const trailingSpace = !isLineLast && expandedTokens[ei].wordFinal && !expandedTokens[ei].text.endsWith(' ') ? ' ' : '';

        if (expandedTiming[ei] !== null) {
            result.push({ text: expandedTokens[ei].text + trailingSpace, time: expandedTiming[ei].time, duration: expandedTiming[ei].duration });
            ei++;
        } else {
            let j = ei;
            while (j < N && expandedTiming[j] === null) j++;
            const leftEnd    = ei > 0 && expandedTiming[ei - 1] ? expandedTiming[ei - 1].time + expandedTiming[ei - 1].duration : lineStart;
            const rightStart = j  < N && expandedTiming[j]      ? expandedTiming[j].time                                         : lineEnd;
            const totalTime  = Math.max(0, rightStart - leftEnd);
            const run        = expandedTokens.slice(ei, j);
            const totalLen   = run.reduce((s, t) => s + t.text.replace(/-$/, '').length, 0) || run.length;
            let t = leftEnd;

            for (let k = 0; k < run.length; k++) {
                const gi       = ei + k;
                const trailing = gi < N - 1 && run[k].wordFinal ? ' ' : '';
                const charLen  = run[k].text.replace(/-$/, '').length;
                const dur      = Math.round(Math.max(50, (charLen / totalLen) * totalTime));
                result.push({ text: run[k].text + trailing, time: t, duration: dur, synthetic: true });
                t += dur;
            }
            ei = j;
        }
    }

    // Enforce monotonically non-decreasing syllable start times.
    //
    // This can be violated when ALL matched QQ tokens for a line fall before
    // Apple's lineStart (the QQ data ran slightly early).  In that case a
    // synthetic token placed at lineStart ends up AFTER the QQ-timed tokens
    // that follow it, producing reversed times that confuse renderers.
    //
    // Example (L68 "Make it clap"):
    //   Apple lineStart = 184 933 ms
    //   QQ tokens       = it@184 755, clap@184 924  (both before lineStart)
    //   → synthetic "Make" lands at 184 933, then "it" regresses to 184 755
    //
    // The fix keeps the first-seen time as the floor and pushes any regressing
    // token to immediately follow its predecessor.
    for (let i = 1; i < result.length; i++) {
        if (result[i].time < result[i - 1].time) {
            result[i] = {
                ...result[i],
                time:      result[i - 1].time + result[i - 1].duration,
                synthetic: true,
            };
        }
    }

    return result;
}

//  None-sync retime 
function retimeAppleLinesFromQQ(appleLines, wordLines) {
    const timedQQ = wordLines.filter(
        l => (l.time != null && l.time > 0) || (l.duration != null && l.duration > 0)
    );
    if (!timedQQ.length) return appleLines;

    const qqNorms = timedQQ.map(l => normalizeForMatch(l.text ?? ''));
    const result  = appleLines.map(l => ({ ...l }));
    const matched = new Array(result.length).fill(false);
    let qqCursor  = 0;

    for (let a = 0; a < result.length; a++) {
        const aNorm = normalizeForMatch(result[a].text ?? '');
        if (!aNorm) continue;

        let bestSim = 0.25, bestIdx = -1;
        const searchEnd = Math.min(timedQQ.length, qqCursor + 10);
        for (let q = qqCursor; q < searchEnd; q++) {
            const sim = textSimilarity(aNorm, qqNorms[q]);
            if (sim > bestSim) { bestSim = sim; bestIdx = q; }
        }

        if (bestIdx >= 0) {
            let startIdx = bestIdx;
            while (startIdx > qqCursor) {
                const prevNorm = qqNorms[startIdx - 1];
                if (prevNorm && prevNorm.length >= 4 && aNorm.includes(prevNorm)) {
                    startIdx--;
                } else {
                    break;
                }
            }

            let endIdx = bestIdx;
            while (endIdx + 1 < timedQQ.length) {
                const nextNorm = qqNorms[endIdx + 1];
                if (nextNorm && nextNorm.length >= 4 && aNorm.includes(nextNorm)) {
                    endIdx++;
                } else {
                    break;
                }
            }

            const startTime = timedQQ[startIdx].time;
            const lastLine  = timedQQ[endIdx];
            const endTime   = lastLine.time + (lastLine.duration || 0);

            result[a] = {
                ...result[a],
                time:     startTime,
                duration: Math.max(endTime - startTime, timedQQ[bestIdx].duration),
            };
            matched[a] = true;
            qqCursor   = endIdx + 1;
        }
    }

    const lastQQ = timedQQ[timedQQ.length - 1];
    const qqEnd  = lastQQ.time + (lastQQ.duration || 0);

    let a = 0;
    while (a < result.length) {
        if (matched[a]) { a++; continue; }

        // Find the bounds of this unmatched run.
        const runStart = a;
        while (a < result.length && !matched[a]) a++;
        const runEnd = a; // exclusive
        const runLen = runEnd - runStart;

        // Nearest matched neighbours.
        let prevA = runStart - 1; while (prevA >= 0 && !matched[prevA]) prevA--;
        const prevEnd   = prevA >= 0 ? result[prevA].time + (result[prevA].duration || 0) : 0;
        const nextStart = runEnd < result.length ? result[runEnd].time : qqEnd;

        const totalGap = Math.max(0, nextStart - prevEnd);
        const slot     = totalGap / runLen;

        for (let k = 0; k < runLen; k++) {
            result[runStart + k] = {
                ...result[runStart + k],
                time:     Math.round(prevEnd + k * slot),
                duration: Math.round(slot),
            };
        }
    }

    return result;
}

function transferBackground(appleLine, outLine) {
    if (!appleLine.syllabus?.length || !outLine.syllabus?.length) return;
    const bg = appleLine.syllabus.filter(s => s.isBackground);
    if (!bg.length) return;
    for (const ws of outLine.syllabus) {
        for (const b of bg) {
            if (Math.min(ws.time + ws.duration, b.time + b.duration) - Math.max(ws.time, b.time) > 0) {
                ws.isBackground = true; break;
            }
        }
    }
}

//  Main export 

export function mergeAppleMetadataIntoWordSync(appleData, wordSyncData) {
    const isWordSynced = ['word', 'syllable'].includes(appleData?.type?.toLowerCase());
    if (isWordSynced) {
        console.debug('mergeAppleMetadataIntoWordSync: Apple data is already word-synced, aborting merge.');
        return null;
    }

    if (!appleData?.lyrics?.length || !wordSyncData?.lyrics?.length) return wordSyncData;

    const appleLines = appleData.lyrics;

    const wordLines  = wordSyncData.lyrics;

    const validTimingLines = appleLines.filter(
        l => (l.time != null && l.time > 0) || (l.duration != null && l.duration > 0)
    );
    if (validTimingLines.length === 0) {
        console.debug('mergeAppleMetadataIntoWordSync: Apple data has no valid timing (None sync) – retiming from QQ.');
        const retimed = retimeAppleLinesFromQQ(appleLines, wordLines);
        return mergeAppleMetadataIntoWordSync({ ...appleData, lyrics: retimed }, wordSyncData);
    }
    const appleMeta  = appleData.metadata   || {};
    const wordMeta   = wordSyncData.metadata || {};

    const mode = detectQQMode(wordSyncData);
    console.debug(`mergeAppleMetadataIntoWordSync: QQ mode = "${mode}"`);

    const qqPool           = flattenQQIntoWordPool(wordLines);
    const lineOffset       = estimateLineOffset(appleLines, wordLines);
    const candidatesByLine = collectCandidates(appleLines, qqPool, lineOffset);

    const mergedLyrics = [];
    let totalWords = 0, totalSynth = 0;

    for (let a = 0; a < appleLines.length; a++) {
        const appleLine  = appleLines[a];
        const candidates = candidatesByLine.get(a) || [];
        const tokens     = tokenizeAppleLine(appleLine.text ?? '');
        const lineStart  = appleLine.time ?? 0;
        const lineEnd    = lineStart + (appleLine.duration || 0);

        // If this individual line has no timing (e.g. a lone untimed entry in an
        // otherwise valid Apple payload), skip alignment entirely and fall back
        // to QQ's own timing for every token.
        if (lineStart === 0 && lineEnd === 0) {
            console.debug(`Line [${a}] "${appleLine.text}": skipped – no Apple timing`);
            const syllabus = buildSyllabus(tokens, new Array(tokens.length).fill(null), [], lineStart, lineEnd);
            mergedLyrics.push({
                time:     lineStart,
                duration: appleLine.duration || 0,
                text:     appleLine.text,
                syllabus,
                element: {
                    key:           appleLine.element?.key           ?? '',
                    singer:        appleLine.element?.singer        ?? '',
                    songPartIndex: appleLine.element?.songPartIndex,
                },
                ...(appleLine.translation     ? { translation:     appleLine.translation }     : {}),
                ...(appleLine.transliteration ? { transliteration: appleLine.transliteration } : {}),
            });
            totalWords += tokens.length;
            totalSynth += tokens.length;
            continue;
        }

        const { matches, residuals } = alignTokensToQQ(tokens, candidates, lineStart, lineEnd, mode);
        const syllabus               = buildSyllabus(tokens, matches, residuals, lineStart, lineEnd);

        const synth = matches.filter(m => m === null).length;
        totalSynth += synth;
        totalWords += tokens.length;

        if (synth > 0) {
            console.debug(`Line [${a}] "${appleLine.text}": ${synth}/${tokens.length} token(s) synthesized`);
        }

        const line = {
            time:     lineStart,
            duration: appleLine.duration || 0,
            text:     appleLine.text,
            syllabus,
            element: {
                key:           appleLine.element?.key           ?? '',
                singer:        appleLine.element?.singer        ?? '',
                songPartIndex: appleLine.element?.songPartIndex,
            },
        };

        if (appleLine.translation)     line.translation     = appleLine.translation;
        if (appleLine.transliteration) line.transliteration = appleLine.transliteration;

        transferBackground(appleLine, line);
        mergedLyrics.push(line);
    }

    console.debug(`QQ/Apple merge (${mode}): ${mergedLyrics.length} lines, ${totalSynth}/${totalWords} tokens synthesized`);

    // Bail out if too many tokens had to be synthesized – this usually means the
    // two sources are too far out of alignment to produce a useful result.  The
    // caller should fall back to the raw QQ or Apple data instead.
    const synthRatio = totalWords > 0 ? totalSynth / totalWords : 0;
    if (synthRatio > SYNTH_BAIL_THRESHOLD) {
        console.debug(
            `QQ/Apple merge aborted: ${(synthRatio * 100).toFixed(1)} % synthetic ` +
            `(${totalSynth}/${totalWords}) exceeds ${SYNTH_BAIL_THRESHOLD * 100} % threshold`
        );
        return null;
    }

    const mergedMetadata = {
        source:         `QQ/Apple (${mode})`,
        songWriters:    appleMeta.songWriters?.length ? appleMeta.songWriters : (wordMeta.songWriters || []),
        leadingSilence: wordMeta.leadingSilence || '0.000',
        agents:         Object.keys(appleMeta.agents || {}).length > 0 ? appleMeta.agents : (wordMeta.agents || {}),
        songParts:      appleMeta.songParts?.length ? appleMeta.songParts : [],
        language:       appleMeta.language     || wordMeta.language     || '',
        totalDuration:  appleMeta.totalDuration || wordMeta.totalDuration || '',
    };

    if (wordMeta.title)  mergedMetadata.title  = wordMeta.title;
    if (wordMeta.artist) mergedMetadata.artist = wordMeta.artist;
    if (wordMeta.album)  mergedMetadata.album  = wordMeta.album;

    return { ...wordSyncData, type: 'Word', metadata: mergedMetadata, lyrics: mergedLyrics };
}