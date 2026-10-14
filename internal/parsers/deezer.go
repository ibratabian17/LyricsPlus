package parsers

import (
	"encoding/json"
	"strconv"
	"strings"

	"lyricsplus/backend/internal/domain"
)

type flexInt int

func (fi *flexInt) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		*fi = 0
		return nil
	}
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*fi = 0
		return nil
	}
	if v, err := strconv.Atoi(s); err == nil {
		*fi = flexInt(v)
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*fi = flexInt(f)
		return nil
	}
	*fi = 0
	return nil
}

type deezerTrackLyrics struct {
	SynchronizedWordByWordLines []struct {
		Start int `json:"start"`
		End   int `json:"end"`
		Words []struct {
			Start int    `json:"start"`
			End   int    `json:"end"`
			Word  string `json:"word"`
		} `json:"words"`
	} `json:"synchronizedWordByWordLines"`
	SynchronizedLines []struct {
		Milliseconds flexInt `json:"milliseconds"`
		Duration     flexInt `json:"duration"`
		Line         string  `json:"line"`
	} `json:"synchronizedLines"`
	Text      string `json:"text"`
	Writers   string `json:"writers"`
	Copyright string `json:"copyright"`
	Licence   string `json:"licence"`
}

type deezerPayload struct {
	Track *struct {
		Lyrics *deezerTrackLyrics `json:"lyrics"`
	} `json:"track"`
}

func NormalizeDeezerLyrics(data []byte) (*domain.LyricsResponse, error) {
	var p deezerPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if p.Track == nil || p.Track.Lyrics == nil {
		return nil, nil
	}
	lyricsData := p.Track.Lyrics

	songWriters := []string{}
	if lyricsData.Writers != "" {
		songWriters = strings.Split(lyricsData.Writers, ", ")
	}

	result := &domain.LyricsResponse{
		Type: domain.SyncTypeLine,
		Metadata: domain.LyricsMetadata{
			Source:      "Deezer",
			SongWriters: songWriters,
			Copyright:   lyricsData.Copyright,
			Licence:     lyricsData.Licence,
		},
		Lyrics: []domain.Line{},
	}

	if len(lyricsData.SynchronizedWordByWordLines) > 0 {
		result.Type = domain.SyncTypeWord
		for _, line := range lyricsData.SynchronizedWordByWordLines {
			var syllabus []domain.Syllable
			for i, w := range line.Words {
				text := w.Word
				if i < len(line.Words)-1 {
					text += " "
				}
				syllabus = append(syllabus, domain.Syllable{
					Text:     text,
					Time:     w.Start,
					Duration: w.End - w.Start,
				})
			}
			var text strings.Builder
			for _, w := range syllabus {
				text.WriteString(w.Text)
			}
			result.Lyrics = append(result.Lyrics, domain.Line{
				Time:     line.Start,
				Duration: line.End - line.Start,
				Text:     text.String(),
				Syllabus: syllabus,
			})
		}
	} else if len(lyricsData.SynchronizedLines) > 0 {
		result.Type = domain.SyncTypeLine
		for _, line := range lyricsData.SynchronizedLines {
			result.Lyrics = append(result.Lyrics, domain.Line{
				Time:     int(line.Milliseconds),
				Duration: int(line.Duration),
				Text:     line.Line,
			})
		}
	} else if lyricsData.Text != "" {
		result.Type = domain.SyncTypeNone
		for _, line := range strings.Split(lyricsData.Text, "\n") {
			line = strings.TrimSuffix(line, "\r")
			result.Lyrics = append(result.Lyrics, domain.Line{
				Text: line,
			})
		}
	}

	return result, nil
}
