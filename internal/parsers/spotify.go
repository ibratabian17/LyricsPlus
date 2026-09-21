package parsers

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"

	"lyricsplus/backend/internal/domain"
)

type spotifySyllable struct {
	StartTimeMs json.RawMessage `json:"startTimeMs"`
	EndTimeMs   json.RawMessage `json:"endTimeMs"`
	Text        string          `json:"text"`
	Verse       string          `json:"verse"`
}

type spotifyLine struct {
	StartTimeMs json.RawMessage   `json:"startTimeMs"`
	EndTimeMs   json.RawMessage   `json:"endTimeMs"`
	Words       string            `json:"words"`
	Syllables   []spotifySyllable `json:"syllables"`
}

type spotifyLyrics struct {
	ProviderDisplayName string        `json:"providerDisplayName"`
	SongWriters         []string      `json:"songWriters"`
	Lines               []spotifyLine `json:"lines"`
}

type spotifyPayload struct {
	Lyrics *spotifyLyrics `json:"lyrics"`
}

// ConvertSpotifyToJSON converts a Spotify color-lyrics payload into V2.
func ConvertSpotifyToJSON(spotifyPayloadData []byte) (*domain.LyricsResponse, error) {
	var outer spotifyPayload
	if err := json.Unmarshal(spotifyPayloadData, &outer); err != nil {
		return nil, err
	}
	var spotifyLyricsData spotifyLyrics
	if outer.Lyrics != nil {
		spotifyLyricsData = *outer.Lyrics
	}

	songWriters := spotifyLyricsData.SongWriters
	hasDetailedTiming := false
	for _, l := range spotifyLyricsData.Lines {
		if len(l.Syllables) > 0 {
			hasDetailedTiming = true
			break
		}
	}
	finalType := "Line"
	if hasDetailedTiming {
		finalType = "Word"
	}

	result := &domain.LyricsResponse{
		Type:      domain.SyncType(finalType),
		KpoeTools: "2.0-LPlusBcknd",
		Metadata: domain.LyricsMetadata{
			Source:         spotifyLyricsData.ProviderDisplayName,
			LeadingSilence: "0.000",
			SongWriters:    songWriters,
		},
		Lyrics: []domain.Line{},
	}

	if !hasDetailedTiming {
		for i, line := range spotifyLyricsData.Lines {
			var fallback float64
			if i+1 < len(spotifyLyricsData.Lines) {
				fallback = jsNumber(spotifyLyricsData.Lines[i+1].StartTimeMs) - jsNumber(line.StartTimeMs)
			}
			var verse string
			if len(line.Syllables) > 0 {
				verse = line.Syllables[0].Verse
			}
			entry := domain.Line{
				Time:     int(math.Round(jsNumber(line.StartTimeMs))),
				Duration: durationFromEnd(line.StartTimeMs, line.EndTimeMs, fallback),
				Text:     line.Words,
				Syllabus: []domain.Syllable{},
				Element: domain.LineElement{
					Key:      verse,
					SongPart: detectSongPart(line),
					Singer:   "",
				},
			}
			if entry.Text != "" && entry.Text != "♪" {
				result.Lyrics = append(result.Lyrics, entry)
			}
		}
	} else {
		for index, line := range spotifyLyricsData.Lines {
			hasWords := line.Words != "" && line.Words != "♪"
			noSyllables := len(line.Syllables) == 0
			if !hasWords && noSyllables {
				continue
			}

			currentLine := domain.Line{
				Time:     0,
				Duration: 0,
				Text:     "",
				Syllabus: []domain.Syllable{},
				Element: domain.LineElement{
					Key:      "L" + strconv.Itoa(index+1),
					SongPart: detectSongPart(line),
					Singer:   "v1",
				},
			}

			if len(line.Syllables) > 0 {
				for sylIndex, syl := range line.Syllables {
					if syl.Text == "" {
						continue
					}
					syllableText := syl.Text
					if shouldAddSpace(line.Syllables, sylIndex) {
						syllableText += " "
					}
					currentLine.Text += syllableText
					currentLine.Syllabus = append(currentLine.Syllabus, domain.Syllable{
						Time:     int(math.Round(jsNumber(syl.StartTimeMs))),
						Duration: durationFromEnd(syl.StartTimeMs, syl.EndTimeMs, 500),
						Text:     syllableText,
					})
				}

				var earliestTime int
				if len(currentLine.Syllabus) > 0 {
					earliestTime = currentLine.Syllabus[0].Time
				}
				var latestEndTime int
				if len(currentLine.Syllabus) > 0 {
					lastSyl := currentLine.Syllabus[len(currentLine.Syllabus)-1]
					latestEndTime = lastSyl.Time + lastSyl.Duration
				}

				currentLine.Time = earliestTime
				currentLine.Duration = latestEndTime - earliestTime
				currentLine.Text = strings.TrimSpace(currentLine.Text)
				result.Lyrics = append(result.Lyrics, currentLine)
			} else {
				var fallback float64
				if index+1 < len(spotifyLyricsData.Lines) {
					fallback = jsNumber(spotifyLyricsData.Lines[index+1].StartTimeMs) - jsNumber(line.StartTimeMs)
				}
				result.Lyrics = append(result.Lyrics, domain.Line{
					Time:     int(math.Round(jsNumber(line.StartTimeMs))),
					Duration: durationFromEnd(line.StartTimeMs, line.EndTimeMs, fallback),
					Text:     line.Words,
					Syllabus: []domain.Syllable{},
					Element:  currentLine.Element,
				})
			}
		}
	}
	return result, nil
}

// jsNumber parses a number-like JSON value, defaulting to 0 for empty/null/missing values.
func jsNumber(raw json.RawMessage) float64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0
		}
		f, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return 0
		}
		return f
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0
	}
	return f
}

func numberFinite(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return false
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return false
		}
		f, err := strconv.ParseFloat(str, 64)
		return err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return false
	}
	return !math.IsInf(f, 0) && !math.IsNaN(f)
}

func durationFromEnd(startTimeMs, endTimeMs json.RawMessage, fallback float64) int {
	start := jsNumber(startTimeMs)
	hasEnd := endTimeMs != nil && string(endTimeMs) != "" && string(endTimeMs) != "null" && numberFinite(endTimeMs)
	duration := fallback
	if numberFinite(startTimeMs) && hasEnd {
		duration = jsNumber(endTimeMs) - start
	}
	if math.IsNaN(duration) || math.IsInf(duration, 0) {
		duration = 0
	}
	return int(math.Max(0, math.Round(duration)))
}

// detectSongPart classifies a line as a known song part from its words.
func detectSongPart(line spotifyLine) string {
	text := strings.ToLower(line.Words)
	if strings.Contains(text, "[verse]") || strings.Contains(text, "verse") {
		return "Verse"
	}
	if strings.Contains(text, "[chorus]") || strings.Contains(text, "chorus") {
		return "Chorus"
	}
	if strings.Contains(text, "[bridge]") || strings.Contains(text, "bridge") {
		return "Bridge"
	}
	if strings.Contains(text, "[intro]") || strings.Contains(text, "intro") {
		return "Intro"
	}
	if strings.Contains(text, "[outro]") || strings.Contains(text, "outro") {
		return "Outro"
	}
	return ""
}

var (
	reCapStart   = regexp.MustCompile(`^[A-Z]`)
	rePunctEnd   = regexp.MustCompile(`[.,!?]$`)
	rePunctStart = regexp.MustCompile(`^[.,!?]`)
)

// shouldAddSpace decides whether a word gap belongs between syllables.
func shouldAddSpace(syllables []spotifySyllable, currentIndex int) bool {
	if currentIndex >= len(syllables)-1 {
		return false
	}
	currentSyl := syllables[currentIndex]
	nextSyl := syllables[currentIndex+1]
	if jsNumber(nextSyl.StartTimeMs)-jsNumber(currentSyl.EndTimeMs) > 100 {
		return true
	}
	return reCapStart.MatchString(nextSyl.Text) ||
		rePunctEnd.MatchString(currentSyl.Text) ||
		rePunctStart.MatchString(nextSyl.Text)
}
