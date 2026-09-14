export function convertQQToJSON(qrcString, exactMetadata = {}) {
    if (!qrcString || (!qrcString.includes('<QrcInfos>') && !qrcString.includes('LyricContent='))) {
        return null;
    }

    try {
        // DOMParser normalizes newlines in attributes to spaces, so extract
        // LyricContent with regex instead. Use a lazy match terminated by the
        // next attribute or tag close to handle literal quotes inside the value.
        const attrMatch = qrcString.match(/LyricContent="([\s\S]*?)"\s*(?:\/?>|[a-zA-Z]+=)/);
        const lyricContent = attrMatch
            ? attrMatch[1].replace(/&quot;/g, '"').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&')
            : qrcString;

        const { lines, agents } = parseQRC(lyricContent, exactMetadata);

        if (lines.length === 0) return null;

        return {
            type: "Word",
            KpoeTools: "1.0-LPlusBcknd",
            metadata: {
                source: "QQ Music",
                songWriters: [],
                leadingSilence: "0.000",
                agents
            },
            lyrics: lines
        };
    } catch {
        return null;
    }
}

// Parses [startMs,durationMs] — the line-level timestamp.
// Returns { startTime, duration, rest } or null.
function parseLineTime(src) {
    if (src[0] !== '[') return null;
    const close = src.indexOf(']');
    if (close === -1) return null;
    const comma = src.indexOf(',', 1);
    if (comma === -1 || comma > close) return null;
    const startTime = parseInt(src.slice(1, comma), 10);
    const duration = parseInt(src.slice(comma + 1, close), 10);
    if (isNaN(startTime) || isNaN(duration)) return null;
    return { startTime, duration, rest: src.slice(close + 1) };
}

// Parses (startMs,durationMs) — a word-level timestamp.
// Returns { startTime, duration, tokenLen } or null.
function parseWordTime(src) {
    if (src[0] !== '(') return null;
    const close = src.indexOf(')');
    if (close === -1) return null;
    const comma = src.indexOf(',', 1);
    if (comma === -1 || comma > close) return null;
    const startTime = parseInt(src.slice(1, comma), 10);
    const duration = parseInt(src.slice(comma + 1, close), 10);
    if (isNaN(startTime) || isNaN(duration)) return null;
    return { startTime, duration, tokenLen: close + 1 };
}

// Extracts timed syllables from the word portion of a QRC line.
// Scans character-by-character for (startMs,durationMs) tokens so that '('
// inside lyric text (e.g. "(feat. X)") doesn't break the parse.
function parseWords(src) {
    const words = [];
    let pos = 0;

    while (pos < src.length) {
        let found = false;
        for (let i = pos; i < src.length; i++) {
            if (src[i] !== '(') continue;
            const token = parseWordTime(src.slice(i));
            if (token) {
                words.push({ text: src.slice(pos, i), time: token.startTime, duration: token.duration });
                pos = i + token.tokenLen;
                found = true;
                break;
            }
        }
        if (!found) break;
    }

    return words;
}

// Parses one trimmed QRC line. Returns null for metadata tags ([ti:...]) and
// any line that doesn't start with a valid [startMs,durationMs] header.
function parseLine(src) {
    if (/^\[[a-zA-Z]+:/.test(src)) return null;

    const lineTime = parseLineTime(src);
    if (!lineTime) return null;

    const syllabus = parseWords(lineTime.rest);
    const text = syllabus.map(w => w.text).join('');

    return { time: lineTime.startTime, duration: lineTime.duration, text, syllabus, element: {} };
}

function parseQRC(qrcContent, exactMetadata) {
    const lines = [];
    const agentsCtx = { agents: {}, aliases: {}, nextVoiceId: 1, currentSinger: null };

    for (const raw of qrcContent.split('\n')) {
        const trimmed = raw.trim();
        if (!trimmed) continue;

        const parsed = parseLine(trimmed);
        if (!parsed) continue;

        if (extractSinger(parsed, agentsCtx, exactMetadata, lines.length < 5)) {
            lines.push(parsed);
        }
    }

    return { lines, agents: agentsCtx.agents };
}

// Returns true if the name is a production credit rather than a singer label
// (e.g. "作词", "Produced by", "Guitar").
function isMetadataPrefix(name) {
    const n = name.toLowerCase().replace(/\s+/g, '');
    const known = [
        "词", "作词", "曲", "作曲", "编曲", "和声", "混音", "吉他", "制作人", "演唱", "原唱", "翻唱", "后期",
        "和音", "录音", "策划", "伴奏", "美工", "海报", "旁白",
        "writtenby", "producedby", "composedby", "arrangedby", "mixing", "mastering",
        "vocal", "vocals", "guitar", "bass", "drums", "producer", "lyricist", "composer", "arranger"
    ];
    return known.includes(n) || n.endsWith('词') || n.endsWith('曲') || n.endsWith('声') || n.endsWith('音');
}

// Detects "SingerName: lyrics..." prefixes, assigns agent metadata, and strips
// the prefix from the line. Returns false if the line should be dropped entirely.
function extractSinger(parsedLine, agentsCtx, exactMetadata, isFirstFewLines) {
    if (!parsedLine.syllabus.length) {
        if (agentsCtx.currentSinger) parsedLine.element = { singer: agentsCtx.currentSinger };
        return true;
    }

    // Drop lines near the start that just echo the track title or artist name.
    if (isFirstFewLines && exactMetadata && (exactMetadata.title || exactMetadata.artist)) {
        const text = parsedLine.text.toLowerCase().replace(/[\s-]/g, '');
        const title = (exactMetadata.title || '').toLowerCase().replace(/[\s-]/g, '');
        const artist = (exactMetadata.artist || '').toLowerCase().replace(/[\s-]/g, '');

        if (title && text.includes(title) && (!artist || text.includes(artist)) && text.length < title.length + artist.length + 15) return false;
        if (artist && text === artist) return false;
    }

    if (agentsCtx.currentSinger) parsedLine.element = { singer: agentsCtx.currentSinger };

    // Accumulate syllables until we hit a colon or exceed a sane prefix length.
    let accText = "";
    let syllablesToRemove = 0;
    for (const syl of parsedLine.syllabus) {
        accText += syl.text;
        syllablesToRemove++;
        if (accText.includes(':') || accText.includes('：')) break;
        if (accText.length > 25) return true;
    }

    // Case A: entire accumulated text is "Name:" with nothing after the colon.
    const fullMatch = accText.match(/^([^:：]+)\s*[:：]\s*$/);
    if (fullMatch) {
        const singerName = fullMatch[1].trim();
        if (isMetadataPrefix(singerName)) return false;
        if (singerName.length > 15) return true;

        parsedLine.syllabus = parsedLine.syllabus.slice(syllablesToRemove);
        parsedLine.text = parsedLine.text.substring(accText.length);
        assignAgent(singerName, parsedLine, agentsCtx);
        agentsCtx.currentSinger = agentsCtx.aliases[singerName];
        updateLineTiming(parsedLine);
        return true;
    }

    // Case B: "Name: lyrics..." packed into the first syllable.
    const splitMatch = parsedLine.syllabus[0].text.match(/^([^:：]+)\s*[:：]\s*(.+)?$/);
    if (splitMatch && splitMatch[1].length < 20) {
        const singerName = splitMatch[1].trim();
        if (isMetadataPrefix(singerName)) return false;

        const remainder = splitMatch[2] || "";
        if (remainder.length > 0) {
            parsedLine.syllabus[0].text = remainder;
        } else {
            parsedLine.syllabus = parsedLine.syllabus.slice(1);
        }

        assignAgent(singerName, parsedLine, agentsCtx);
        agentsCtx.currentSinger = agentsCtx.aliases[singerName];
        parsedLine.text = parsedLine.text.replace(/^[^:：]+[:：]\s*/, '');
        updateLineTiming(parsedLine);
    }

    return true;
}

function assignAgent(singerName, parsedLine, agentsCtx) {
    if (!agentsCtx.aliases[singerName]) {
        const upper = singerName.toUpperCase();
        const type = (upper === "合" || upper === "ALL" || upper === "合唱") ? "group" : "person";
        const alias = `v${agentsCtx.nextVoiceId++}`;
        agentsCtx.aliases[singerName] = alias;
        agentsCtx.agents[`voice${agentsCtx.nextVoiceId - 1}`] = { type, name: singerName, alias };
    }
    parsedLine.element = { singer: agentsCtx.aliases[singerName] };
}

function updateLineTiming(parsedLine) {
    if (!parsedLine.syllabus.length) return;
    const last = parsedLine.syllabus[parsedLine.syllabus.length - 1];
    parsedLine.time = parsedLine.syllabus[0].time;
    parsedLine.duration = (last.time + last.duration) - parsedLine.time;
}
