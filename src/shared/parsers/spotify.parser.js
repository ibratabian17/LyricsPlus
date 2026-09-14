// --- Data Conversion & Processing ---

export function convertSpotifyToJSON(spotifyPayload) {
    const spotifyLyrics = spotifyPayload.lyrics || spotifyPayload;
    const songWriters = spotifyLyrics.songWriters || [];
    const hasDetailedTiming = spotifyLyrics.lines?.some(line => line.syllables && line.syllables.length > 0);
    const originalType = hasDetailedTiming ? "syllable" : "Line";
    const finalType = originalType === "syllable" ? "Word" : "Line";

    const result = {
        type: finalType,
        KpoeTools: "2.0-LPlusBcknd",
        metadata: {
            source: spotifyLyrics.providerDisplayName,
            leadingSilence: "0.000",
            songWriters: songWriters
        },
        lyrics: []
    };

    if (originalType === "Line") {
        result.lyrics = (spotifyLyrics.lines || []).map((line, index) => ({
            time: Math.round(Number(line.startTimeMs)),
            duration: durationFromEnd(line.startTimeMs, line.endTimeMs, Number(spotifyLyrics.lines[index + 1]?.startTimeMs) - Number(line.startTimeMs)),
            text: line.words,
            syllabus: [],
            element: {
                key: line.syllables?.[0]?.verse || "",
                songPart: detectSongPart(line),
                singer: ""
            }
        })).filter(line => line.text && line.text !== '♪');
    } else {
        (spotifyLyrics.lines || []).forEach((line, index) => {
            if ((!line.words || line.words === '♪') && (!line.syllables || line.syllables.length === 0)) return;

            const currentLine = {
                time: 0,
                duration: 0,
                text: "",
                syllabus: [],
                element: { key: `L${index + 1}`, songPart: detectSongPart(line), singer: "v1" }
            };

            if (line.syllables?.length > 0) {
                line.syllables.forEach((syl, sylIndex) => {
                    if (syl.text === '') return;
                    const syllableText = syl.text + (shouldAddSpace(line.syllables, sylIndex) ? " " : "");
                    currentLine.text += syllableText;
                    currentLine.syllabus.push({
                        time: Math.round(Number(syl.startTimeMs)),
                        duration: durationFromEnd(syl.startTimeMs, syl.endTimeMs, 500),
                        text: syllableText,
                    });
                });

                const earliestTime = currentLine.syllabus[0]?.time || 0;
                const lastSyllable = currentLine.syllabus[currentLine.syllabus.length - 1];
                const latestEndTime = (lastSyllable?.time || 0) + (lastSyllable?.duration || 0);

                currentLine.time = earliestTime;
                currentLine.duration = latestEndTime - earliestTime;
                currentLine.text = currentLine.text.trim();
                result.lyrics.push(currentLine);
            } else {
                result.lyrics.push({
                    time: Math.round(Number(line.startTimeMs)),
                    duration: durationFromEnd(line.startTimeMs, line.endTimeMs, Number(spotifyLyrics.lines[index + 1]?.startTimeMs) - Number(line.startTimeMs)),
                    text: line.words,
                    syllabus: [],
                    element: currentLine.element
                });
            }
        });
    }
    return result;
}

function durationFromEnd(startTimeMs, endTimeMs, fallback) {
    const start = Number(startTimeMs);
    const end = Number(endTimeMs);
    const hasEnd = endTimeMs !== null && endTimeMs !== '' && Number.isFinite(end);
    const duration = Number.isFinite(start) && hasEnd ? end - start : fallback;
    return Math.max(0, Math.round(Number.isFinite(duration) ? duration : 0));
}

export function detectSongPart(line) {
    const text = (line?.words || "").toLowerCase();
    if (text.includes("[verse]") || text.includes("verse")) return "Verse";
    if (text.includes("[chorus]") || text.includes("chorus")) return "Chorus";
    if (text.includes("[bridge]") || text.includes("bridge")) return "Bridge";
    if (text.includes("[intro]") || text.includes("intro")) return "Intro";
    if (text.includes("[outro]") || text.includes("outro")) return "Outro";
    return "";
}

export function shouldAddSpace(syllables, currentIndex) {
    if (currentIndex >= syllables.length - 1) return false;
    const currentSyl = syllables[currentIndex];
    const nextSyl = syllables[currentIndex + 1];
    if (nextSyl.startTimeMs - currentSyl.endTimeMs > 100) return true;
    return nextSyl.text.match(/^[A-Z]/) || currentSyl.text.match(/[.,!?]$/) || nextSyl.text.match(/^[.,!?]/);
}
