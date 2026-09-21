package parsers

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"lyricsplus/backend/internal/domain"
)

var lrcLineRe = regexp.MustCompile(`^\[(\d+):(\d+)\.(\d+)\]\s*(.*)$`)

// ConvertLRCLIBtoJSON parses a synced LRC string into a V2 line payload;
// durationSec is the track duration in seconds used for the final line's duration.
func ConvertLRCLIBtoJSON(syncedLyrics string, durationSec float64) *domain.LyricsResponse {
	durationMs := int(durationSec * 1000)

	type lrcLine struct {
		time     int
		duration int
		text     string
		key      string
	}
	lines := []lrcLine{}
	lineOffset := 0

	for _, raw := range strings.Split(syncedLyrics, "\n") {
		m := lrcLineRe.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		mins, _ := strconv.Atoi(m[1])
		secs, _ := strconv.Atoi(m[2])
		fr, _ := strconv.Atoi(m[3])
		ms := int(math.Round(float64(fr) * (1000.0 / math.Pow10(len(m[3])))))

		time := mins*60000 + secs*1000 + ms
		lineOffset++
		lines = append(lines, lrcLine{
			time: time,
			text: m[4],
			key:  "L" + strconv.Itoa(lineOffset),
		})
	}

	for i := 0; i < len(lines)-1; i++ {
		lines[i].duration = lines[i+1].time - lines[i].time
	}
	if len(lines) > 0 {
		lines[len(lines)-1].duration = durationMs - lines[len(lines)-1].time
	}

	var lyrics []domain.Line
	for _, l := range lines {
		if l.text == "" {
			continue
		}
		lyrics = append(lyrics, domain.Line{
			Time:     l.time,
			Duration: l.duration,
			Text:     l.text,
			Syllabus: []domain.Syllable{},
			Element: domain.LineElement{
				Key:      l.key,
				SongPart: "",
				Singer:   "",
			},
		})
	}

	return &domain.LyricsResponse{
		Type:      "line",
		KpoeTools: "2.0-LPlusBcknd",
		Lyrics:    lyrics,
	}
}
