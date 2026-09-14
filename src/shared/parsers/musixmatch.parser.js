// --- Data Parsing & Conversion ---

export function convertMusixmatchToJSON(musixmatchData, requireWordSync = false) {
    if (!musixmatchData?.lyrics?.message?.body) return null;

    const body = musixmatchData.lyrics.message.body;
    let rawLyrics = [];
    let lyricsCopyright = '';
    let type = "Line";

    if (body.richsync) {
        const richsync = body.richsync;
        rawLyrics = parseRichsyncToRaw(richsync.richsync_body, requireWordSync);
        lyricsCopyright = richsync.lyrics_copyright;
        type = requireWordSync ? "Word" : "Line";
    } else if (body.subtitle) {
        const subtitle = body.subtitle;
        rawLyrics = parseSubtitleToRaw(subtitle.subtitle_body);
        lyricsCopyright = subtitle.lyrics_copyright;
        type = "Line";
    } else {
        return null;
    }

    let processedData = { lyrics: [], songParts: [] };
    
    if (type === "Word") {
        processedData = processWordSyncLines(rawLyrics);
    } else {
        processedData = processSubtitleLines(rawLyrics);
    }

    return {
        type: type,
        KpoeTools: "1.2-MusixmatchToJSON",
        metadata: {
            source: "Musixmatch",
            songWriters: extractSongwriters(lyricsCopyright),
            leadingSilence: "0.000",
            songParts: processedData.songParts
        },
        lyrics: processedData.lyrics
    };
}

function parseSubtitleToRaw(subtitleBody) {
    if (!subtitleBody) return [];
    
    return subtitleBody.split('\n').reduce((acc, line) => {
        const match = line.match(/\[(\d{2}):(\d{2}\.\d{2})\](.*)/);
        if (match) {
            const [, min, sec, text] = match;
            const time = Math.round((parseInt(min, 10) * 60 + parseFloat(sec)) * 1000);
            acc.push({ time, text: text.trim() });
        }
        return acc;
    }, []).sort((a, b) => a.time - b.time);
}

function parseRichsyncToRaw(richsyncBody, requireWordSync) {
    if (!richsyncBody) return [];
    
    try {
        const data = JSON.parse(richsyncBody);
        
        return data.map(lineData => {
            const lineStart = Math.round(lineData.ts * 1000);
            const lineEnd = Math.round(lineData.te * 1000);
            
            const lineObj = {
                time: lineStart,
                endTime: lineEnd,
                text: lineData.x || "",
                syllabus: []
            };

            if (requireWordSync && Array.isArray(lineData.l)) {
                lineObj.syllabus = lineData.l.map((word, index, words) => {
                    const wordStart = lineStart + Math.round(word.o * 1000);
                    
                    const nextWord = words[index + 1];
                    const nextStart = nextWord 
                        ? lineStart + Math.round(nextWord.o * 1000) 
                        : lineEnd;
                    
                    return {
                        time: wordStart,
                        duration: Math.max(0, nextStart - wordStart),
                        text: word.c
                    };
                });
            }
            
            return lineObj;
        });
    } catch (e) {
        console.error("Richsync parsing failed", e);
        return [];
    }
}

function processSubtitleLines(lines) {
    let songParts = [];
    let partIndex = 0;
    let lastLineEnd = 0;

    const lyrics = lines.map((line, index) => {
        let duration = 3000;
        const nextLine = lines[index + 1];
        
        if (nextLine) {
            duration = Math.max(0, nextLine.time - line.time);
        }

        if (index > 0 && (line.time - lastLineEnd) > 15000) {
            partIndex++;
        }

        lastLineEnd = line.time + duration;

        if (!songParts[partIndex]) {
            songParts[partIndex] = { name: "", time: line.time, duration: 0 };
        }
        songParts[partIndex].duration = lastLineEnd - songParts[partIndex].time;

        return {
            time: line.time,
            duration: duration,
            text: line.text,
            element: {
                songPartIndex: partIndex
            }
        };
    }).filter(line => line.text !== '');

    return { lyrics, songParts };
}

function processWordSyncLines(lines) {
    let partIndex = 0;
    let prevLineNaturalEnd = 0;
    let songParts = [];

    const lyrics = lines.map((line, lineIndex) => {
        const cleanedSyllabus = mergeSpacesAndFixDurations(line.syllabus);
        const fullText = cleanedSyllabus.map(w => w.text).join('');

        let naturalDuration = 0;
        if (cleanedSyllabus.length > 0) {
            const lastWord = cleanedSyllabus[cleanedSyllabus.length - 1];
            naturalDuration = (lastWord.time + lastWord.duration) - line.time;
        } else {
            naturalDuration = line.endTime - line.time;
        }

        if (lineIndex > 0) {
            if ((line.time - prevLineNaturalEnd) > 15000) {
                partIndex++;
            }
        }
        
        prevLineNaturalEnd = line.time + naturalDuration;

        let duration = naturalDuration;
        const nextLine = lines[lineIndex + 1];

        // Add 2s padding to duration
        // But ensure we DO NOT overlap the next line
        // cuz musixmatch has bad ux sync tools, so we follow how apple does it
        if (nextLine) {
            const paddedDuration = duration + 2000;
            const timeToNextLine = nextLine.time - line.time;

            duration = Math.min(paddedDuration, timeToNextLine - 10); 
        }

        if (!songParts[partIndex]) {
            songParts[partIndex] = { name: "", time: line.time, duration: 0 };
        }
        
        const calculatedLineEnd = line.time + Math.max(0, duration);
        songParts[partIndex].duration = calculatedLineEnd - songParts[partIndex].time;

        return {
            time: line.time,
            duration: Math.max(0, duration),
            text: fullText,
            syllabus: cleanedSyllabus,
            element: {
                songPartIndex: partIndex
            }
        };
    });

    return { lyrics, songParts };
}

function mergeSpacesAndFixDurations(rawSyllabus) {
    const merged = [];
    
    for (let i = 0; i < rawSyllabus.length; i++) {
        const current = { ...rawSyllabus[i] };
        const next = rawSyllabus[i + 1];

        if (next && /^\s+$/.test(next.text)) {
            current.text += next.text;

            // this just improve ux, original youly+ behavior
            if (next.duration < 100) {
                current.duration += next.duration;
            } else {
                // maybe it's??
            }
            
            i++; 
        }

        merged.push(current);
    }
    return merged;
}

export function extractSongwriters(copyrightString) {
    if (!copyrightString) return [];
    const match = copyrightString.match(/Writer\(s\):\s*([^\n]+)/i);
    return match ? match[1].split(',').map(name => name.trim()) : [];
}
