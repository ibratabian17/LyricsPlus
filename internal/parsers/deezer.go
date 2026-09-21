// Package parsers converts foreign lyric formats to and from the canonical V2 payload.
package parsers

import (
	"encoding/json"
	"strings"

	"lyricsplus/backend/internal/domain"
)

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
		Milliseconds int    `json:"milliseconds"`
		Duration     int    `json:"duration"`
		Line         string `json:"line"`
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

// NormalizeDeezerLyrics normalizes the Deezer lyric payload into V2. Returns
// nil when the track or lyrics are missing.
func NormalizeDeezerLyrics(data []byte) (*domain.LyricsResponse, error) {
	var p deezerPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if p.Track == nil || p.Track.Lyrics == nil {
		return nil, nil
	}
	lyricsData := p.Track.Lyrics

	songWriters := []string(nil)
	if lyricsData.Writers != "" {
		songWriters = strings.Split(lyricsData.Writers, ", ")
	}

	result := &domain.LyricsResponse{
		Type: domain.SyncTypeLine,
		Metadata: domain.LyricsMetadata{
			Source:      "Deezer",
			SongWriters: songWriters,
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
				Time:     line.Milliseconds,
				Duration: line.Duration,
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
