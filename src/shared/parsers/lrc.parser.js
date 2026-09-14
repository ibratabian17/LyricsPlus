/**
 * Konversi format LRCLIB API menjadi format JSON yang diinginkan
 * @param {Object} data - Data dari API LRC
 * @returns {Object} - Data JSON hasil konversi
 */
export function convertLRCLIBtoJSON(data) {
  const syncedLyrics = data.syncedLyrics || '';
  const duration = data.duration * 1000;
  const lines = syncedLyrics.split('\n');
  const beats = [0];

  let lyrics = [];
  let currentTime = 0;
  let lineOffset = 0;

  for (let i = beats[beats.length - 1] + 1000; i <= duration; i += 1000) {
    beats.push(i);
  }

  lines.forEach((line, index) => {
    const match = line.match(/^\[(\d+):(\d+)\.(\d+)]\s*(.*)$/);
    if (match) {
      const minutes = parseInt(match[1], 10);
      const seconds = parseInt(match[2], 10);
      const milliseconds = Math.round(parseInt(match[3], 10) * (1000 / (10 ** match[3].length)));
      const text = match[4];

      const time = minutes * 60000 + seconds * 1000 + milliseconds;

      lyrics.push({
        time,
        duration: 0,
        text,
        isLineEnding: 1,
        element: {
          key: `L${lineOffset + 1}`,
          songPart: "",
          singer: "",
        },
      });

      currentTime = time;
      lineOffset++;
    }
  });

  for (let i = 0; i < lyrics.length - 1; i++) {
    lyrics[i].duration = lyrics[i + 1].time - lyrics[i].time;
  }
  if (lyrics.length > 0) {
    lyrics[lyrics.length - 1].duration = duration - lyrics[lyrics.length - 1].time;
  }

  const filteredLyrics = lyrics.filter(item => item.text !== '')

  return {
    KpoeTools: "2.0-LPlusBcknd",
    type: "line",
    lyrics: filteredLyrics,
  };
}
