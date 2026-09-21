package parsers

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"lyricsplus/backend/internal/domain"
)

// mxmEnvelope is { track, ...lyricsResult } where lyricsResult = { lyrics: { message: { body } } }.
type mxmEnvelope struct {
	Lyrics struct {
		Message struct {
			Body *struct {
				Richsync *struct {
					RichsyncBody    string `json:"richsync_body"`
					LyricsCopyright string `json:"lyrics_copyright"`
				} `json:"richsync"`
				Subtitle *struct {
					SubtitleBody    string `json:"subtitle_body"`
					LyricsCopyright string `json:"lyrics_copyright"`
				} `json:"subtitle"`
			} `json:"body"`
		} `json:"message"`
	} `json:"lyrics"`
}

type mxmRawLine struct {
	time     int
	endTime  int
	text     string
	syllabus []domain.Syllable
}

// ConvertMusixmatchToJSON converts a Musixmatch richsync/subtitle payload into
// V2. Returns nil when there is no convertible lyrics body.
func ConvertMusixmatchToJSON(data []byte, requireWordSync bool) (*domain.LyricsResponse, error) {
	var env mxmEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.Lyrics.Message.Body == nil {
		return nil, nil
	}
	body := env.Lyrics.Message.Body

	var rawLyrics []mxmRawLine
	lyricsCopyright := ""
	var typeStr string

	if body.Richsync != nil {
		rawLyrics = parseRichsyncToRaw(body.Richsync.RichsyncBody, requireWordSync)
		lyricsCopyright = body.Richsync.LyricsCopyright
		if requireWordSync {
			typeStr = "Word"
		} else {
			typeStr = "Line"
		}
	} else if body.Subtitle != nil {
		rawLyrics = parseSubtitleToRaw(body.Subtitle.SubtitleBody)
		lyricsCopyright = body.Subtitle.LyricsCopyright
		typeStr = "Line"
	} else {
		return nil, nil
	}

	lyrics, songParts := processLines(rawLyrics, typeStr == "Word")

	return &domain.LyricsResponse{
		Type:      domain.SyncType(typeStr),
		KpoeTools: "1.2-MusixmatchToJSON",
		Metadata: domain.LyricsMetadata{
			Source:         "Musixmatch",
			SongWriters:    extractSongwriters(lyricsCopyright),
			LeadingSilence: "0.000",
			SongParts:      songParts,
		},
		Lyrics: lyrics,
	}, nil
}

func parseSubtitleToRaw(subtitleBody string) []mxmRawLine {
	if subtitleBody == "" {
		return nil
	}
	var out []mxmRawLine
	for _, line := range strings.Split(subtitleBody, "\n") {
		m := mxmSubtitleRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		mins, _ := strconv.Atoi(m[1])
		secs, _ := strconv.ParseFloat(m[2], 64)
		time := int(math.Round((float64(mins)*60 + secs) * 1000))
		out = append(out, mxmRawLine{time: time, text: strings.TrimSpace(m[3])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].time < out[j].time })
	return out
}

var mxmSubtitleRe = regexp.MustCompile(`\[(\d{2}):(\d{2}\.\d{2})\](.*)`)

func parseRichsyncToRaw(richsyncBody string, requireWordSync bool) []mxmRawLine {
	if richsyncBody == "" {
		return nil
	}
	var data []struct {
		TS float64 `json:"ts"`
		Te float64 `json:"te"`
		X  string  `json:"x"`
		L  []struct {
			O float64 `json:"o"`
			C string  `json:"c"`
		} `json:"l"`
	}
	if err := json.Unmarshal([]byte(richsyncBody), &data); err != nil {
		return nil
	}

	out := make([]mxmRawLine, 0, len(data))
	for _, lineData := range data {
		lineStart := int(math.Round(lineData.TS * 1000))
		lineEnd := int(math.Round(lineData.Te * 1000))
		lineObj := mxmRawLine{
			time:     lineStart,
			endTime:  lineEnd,
			text:     lineData.X,
			syllabus: []domain.Syllable{},
		}

		if requireWordSync && lineData.L != nil {
			lineObj.syllabus = make([]domain.Syllable, 0, len(lineData.L))
			for i, word := range lineData.L {
				wordStart := lineStart + int(math.Round(word.O*1000))
				var nextStart int
				if i+1 < len(lineData.L) {
					nextStart = lineStart + int(math.Round(lineData.L[i+1].O*1000))
				} else {
					nextStart = lineEnd
				}
				lineObj.syllabus = append(lineObj.syllabus, domain.Syllable{
					Time:     wordStart,
					Duration: int(math.Max(0, float64(nextStart-wordStart))),
					Text:     word.C,
				})
			}
		}
		out = append(out, lineObj)
	}
	return out
}

func processLines(rawLines []mxmRawLine, wordSync bool) ([]domain.Line, []domain.SongPart) {
	if wordSync {
		return processWordSyncLines(rawLines)
	}
	return processSubtitleLines(rawLines)
}

func processSubtitleLines(lines []mxmRawLine) ([]domain.Line, []domain.SongPart) {
	var songParts []domain.SongPart
	partIndex := 0
	lastLineEnd := 0

	built := make([]domain.Line, 0, len(lines))
	for i, line := range lines {
		duration := 3000
		if i+1 < len(lines) {
			duration = int(math.Max(0, float64(lines[i+1].time-line.time)))
		}

		if i > 0 && line.time-lastLineEnd > 15000 {
			partIndex++
		}
		lastLineEnd = line.time + duration

		if len(songParts) <= partIndex {
			t := line.time
			d := 0
			songParts = append(songParts, domain.SongPart{Name: "", Time: &t, Duration: &d})
		}
		partDur := lastLineEnd - *songParts[partIndex].Time
		songParts[partIndex].Duration = &partDur

		idx := partIndex
		built = append(built, domain.Line{
			Time:     line.time,
			Duration: duration,
			Text:     line.text,
			Element:  domain.LineElement{SongPartIndex: &idx},
		})
	}

	var lyrics []domain.Line
	for _, l := range built {
		if l.Text == "" {
			continue
		}
		lyrics = append(lyrics, l)
	}
	return lyrics, songParts
}

func processWordSyncLines(lines []mxmRawLine) ([]domain.Line, []domain.SongPart) {
	var songParts []domain.SongPart
	partIndex := 0
	prevLineNaturalEnd := 0

	var lyrics []domain.Line
	for i, line := range lines {
		cleanedSyllabus := mergeSpacesAndFixDurations(line.syllabus)
		var fullText strings.Builder
		for _, w := range cleanedSyllabus {
			fullText.WriteString(w.Text)
		}

		naturalDuration := 0
		if len(cleanedSyllabus) > 0 {
			lastWord := cleanedSyllabus[len(cleanedSyllabus)-1]
			naturalDuration = (lastWord.Time + lastWord.Duration) - line.time
		} else {
			naturalDuration = line.endTime - line.time
		}

		if i > 0 && line.time-prevLineNaturalEnd > 15000 {
			partIndex++
		}
		prevLineNaturalEnd = line.time + naturalDuration

		duration := naturalDuration
		if i+1 < len(lines) {
			paddedDuration := duration + 2000
			timeToNextLine := lines[i+1].time - line.time
			duration = int(math.Min(float64(paddedDuration), float64(timeToNextLine-10)))
		}

		if len(songParts) <= partIndex {
			t := line.time
			d := 0
			songParts = append(songParts, domain.SongPart{Name: "", Time: &t, Duration: &d})
		}
		calculatedLineEnd := line.time + int(math.Max(0, float64(duration)))
		partDur := calculatedLineEnd - *songParts[partIndex].Time
		songParts[partIndex].Duration = &partDur

		idx := partIndex
		lyrics = append(lyrics, domain.Line{
			Time:     line.time,
			Duration: int(math.Max(0, float64(duration))),
			Text:     fullText.String(),
			Syllabus: cleanedSyllabus,
			Element:  domain.LineElement{SongPartIndex: &idx},
		})
	}

	return lyrics, songParts
}

var mxmSpaceRe = regexp.MustCompile(`^\s+$`)

// mergeSpacesAndFixDurations merges whitespace-only tokens into the preceding
// syllable, absorbing their duration only when it is under 100ms.
func mergeSpacesAndFixDurations(rawSyllabus []domain.Syllable) []domain.Syllable {
	var merged []domain.Syllable
	for i := 0; i < len(rawSyllabus); i++ {
		current := rawSyllabus[i]

		if i+1 < len(rawSyllabus) {
			next := rawSyllabus[i+1]
			if mxmSpaceRe.MatchString(next.Text) {
				current.Text += next.Text
				if next.Duration < 100 {
					current.Duration += next.Duration
				}
				i++
			}
		}
		merged = append(merged, current)
	}
	return merged
}

var wsRx = regexp.MustCompile(`Writer\(s\):\s*([^\n]+)`)

// extractSongwriters parses songwriters out of the lyrics copyright line.
func extractSongwriters(copyrightString string) []string {
	if copyrightString == "" {
		return nil
	}
	m := wsRx.FindStringSubmatch(copyrightString)
	if m == nil {
		return nil
	}
	names := strings.Split(m[1], ",")
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, strings.TrimSpace(n))
	}
	return out
}
