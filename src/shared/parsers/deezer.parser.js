export function normalizeDeezerLyrics(deezerLyrics) {
    if (!deezerLyrics?.track?.lyrics) return null;

    const lyricsData = deezerLyrics.track.lyrics;
    const result = {
        type: 'Line',
        metadata: {
            source: 'Deezer',
            songWriters: lyricsData.writers ? lyricsData.writers.split(', ') : [],
            copyright: lyricsData.copyright || '',
            licence: lyricsData.licence || '',
        },
        lyrics: [],
    };

    if (lyricsData.synchronizedWordByWordLines && lyricsData.synchronizedWordByWordLines.length > 0) {
        result.type = 'Word';
        result.lyrics = lyricsData.synchronizedWordByWordLines.map((line) => {
            const syllabus = (line.words || []).map((w, i, arr) => ({
                text: w.word + (i < arr.length - 1 ? ' ' : ''),
                time: w.start,
                duration: w.end - w.start,
            }));
            const text = syllabus.map((w) => w.text).join('');
            return {
                time: line.start,
                duration: line.end - line.start,
                text: text,
                syllabus: syllabus,
            };
        });
    } else if (lyricsData.synchronizedLines && lyricsData.synchronizedLines.length > 0) {
        result.type = 'Line';
        result.lyrics = lyricsData.synchronizedLines.map((line) => ({
            time: parseInt(line.milliseconds, 10) || 0,
            duration: parseInt(line.duration, 10) || 0,
            text: line.line,
        }));
    } else if (lyricsData.text) {
        result.type = 'None';
        result.lyrics = lyricsData.text.split(/\r?\n/).map((line) => ({ text: line }));
    }

    return result;
}
