package parsers

import (
	"strings"

	"lyricsplus/backend/internal/domain"
)

// NormalizeV2 migrates line elements using a legacy songPart string to a
// songPartIndex pointing into metadata.songParts, deriving time/duration for
// the newly created parts. Returns the input unchanged when lines are empty
// or already indexed.
func NormalizeV2(resp *domain.LyricsResponse) *domain.LyricsResponse {
	if resp == nil {
		return nil
	}
	lyrics := resp.Lyrics
	if len(lyrics) == 0 {
		return resp
	}
	allIndexed := true
	for i := range lyrics {
		if lyrics[i].Element.SongPartIndex == nil {
			allIndexed = false
			break
		}
	}
	if allIndexed {
		return resp
	}

	existingPartCount := len(resp.Metadata.SongParts)
	songParts := append([]domain.SongPart(nil), resp.Metadata.SongParts...)

	normalizedLyrics := make([]domain.Line, 0, len(lyrics))
	var currentPartName *string
	for _, line := range lyrics {
		if line.Element.SongPartIndex != nil {
			currentPartName = nil
			normalizedLyrics = append(normalizedLyrics, line)
			continue
		}
		partName := line.Element.SongPart
		if currentPartName == nil || partName != *currentPartName {
			currentPartName = &partName
			songParts = append(songParts, domain.SongPart{Name: partName})
		}
		idx := len(songParts) - 1
		line.Element.SongPart = ""
		line.Element.SongPartIndex = &idx
		normalizedLyrics = append(normalizedLyrics, line)
	}

	type partBounds struct {
		minTime int
		maxEnd  int
	}
	bounds := make(map[int]partBounds)
	for _, line := range normalizedLyrics {
		idx := line.Element.SongPartIndex
		if idx == nil || *idx < existingPartCount || *idx >= len(songParts) {
			continue
		}
		endTime := line.Time + line.Duration
		b, ok := bounds[*idx]
		if !ok || line.Time < b.minTime {
			b.minTime = line.Time
		}
		if !ok || endTime > b.maxEnd {
			b.maxEnd = endTime
		}
		bounds[*idx] = b
	}
	for idx, b := range bounds {
		part := &songParts[idx]
		minTime := b.minTime
		part.Time = &minTime
		duration := b.maxEnd - b.minTime
		part.Duration = &duration
	}

	resp.Metadata.SongParts = songParts
	resp.Lyrics = normalizedLyrics
	return resp
}

// V1ToV2 groups consecutive flat V1 segments into V2 Lines, then normalizes the
// result. Line-synced segments map one-to-one (with empty syllabus); syllable
// segments accumulate until isLineEnding == 1 and then finalize the group.
func V1ToV2(v1 *domain.V1Response) *domain.LyricsResponse {
	if v1 == nil {
		return nil
	}
	var groupedLyrics []domain.Line

	if v1.Type == string(domain.SyncTypeLine) {
		for _, seg := range v1.Lyrics {
			groupedLyrics = append(groupedLyrics, domain.Line{
				Time:     seg.Time,
				Duration: seg.Duration,
				Text:     seg.Text,
				Syllabus: []domain.Syllable{},
				Element: domain.LineElement{
					Key:      seg.Element.Key,
					SongPart: seg.Element.SongPart,
					Singer:   seg.Element.Singer,
				},
			})
		}
	} else {
		var current *domain.Line
		finalizeGroup := func() {
			if current == nil {
				return
			}
			earliest := current.Syllabus[0].Time
			latestEnd := 0
			for _, syl := range current.Syllabus {
				if syl.Time < earliest {
					earliest = syl.Time
				}
				if end := syl.Time + syl.Duration; end > latestEnd {
					latestEnd = end
				}
			}
			current.Time = earliest
			current.Duration = latestEnd - earliest
			current.Text = strings.TrimSpace(current.Text)
			groupedLyrics = append(groupedLyrics, *current)
		}
		for _, seg := range v1.Lyrics {
			if current == nil {
				current = &domain.Line{
					Time:     seg.Time,
					Syllabus: []domain.Syllable{},
					Element: domain.LineElement{
						Key:      seg.Element.Key,
						SongPart: seg.Element.SongPart,
						Singer:   seg.Element.Singer,
					},
				}
			}
			current.Text += seg.Text
			syl := domain.Syllable{
				Time:     seg.Time,
				Duration: seg.Duration,
				Text:     seg.Text,
			}
			if seg.Element.IsBackground {
				syl.IsBackground = true
			}
			current.Syllabus = append(current.Syllabus, syl)
			if seg.IsLineEnding == 1 {
				finalizeGroup()
				current = nil
			}
		}
		if current != nil {
			finalizeGroup()
		}
	}

	typ := domain.SyncType(v1.Type)
	if v1.Type == string(domain.SyncTypeSyllable) {
		typ = domain.SyncTypeWord
	}

	cached := v1.Cached
	if cached == "" {
		cached = domain.CacheNone
	}

	result := &domain.LyricsResponse{
		Type:               typ,
		KpoeTools:          "2.0-LPlusBcknd," + v1.KpoeTools,
		Metadata:           v1.Metadata,
		IgnoreSponsorblock: v1.IgnoreSponsorblock,
		Lyrics:             groupedLyrics,
		Cached:             cached,
	}
	return NormalizeV2(result)
}

// V2ToV1 explodes each V2 Line into flat V1 segments. Line-synced data maps to a
// single segment with isLineEnding 1; word-synced data emits one segment per
// syllable with isLineEnding set on the terminal syllable. songPart strings are
// resolved from metadata.songParts.
func V2ToV1(v2 *domain.LyricsResponse) *domain.V1Response {
	if v2 == nil {
		return nil
	}

	songParts := v2.Metadata.SongParts
	resolveSongPart := func(el domain.LineElement) string {
		if el.SongPart != "" {
			return el.SongPart
		}
		if el.SongPartIndex != nil && *el.SongPartIndex >= 0 && *el.SongPartIndex < len(songParts) {
			return songParts[*el.SongPartIndex].Name
		}
		return ""
	}

	var flatLyrics []domain.V1Segment
	toSegment := func(line domain.Line, isEnd int, el domain.V1SegmentElement) domain.V1Segment {
		return domain.V1Segment{
			Time:         line.Time,
			Duration:     line.Duration,
			Text:         line.Text,
			IsLineEnding: isEnd,
			Element:      el,
		}
	}

	if v2.Type == domain.SyncTypeLine {
		for _, line := range v2.Lyrics {
			rest := domain.V1SegmentElement{
				Key:      line.Element.Key,
				Singer:   line.Element.Singer,
				SongPart: resolveSongPart(line.Element),
			}
			flatLyrics = append(flatLyrics, toSegment(line, 1, rest))
		}
	} else {
		for _, line := range v2.Lyrics {
			rest := domain.V1SegmentElement{
				Key:      line.Element.Key,
				Singer:   line.Element.Singer,
				SongPart: resolveSongPart(line.Element),
			}
			if len(line.Syllabus) == 0 {
				flatLyrics = append(flatLyrics, toSegment(line, 1, rest))
				continue
			}
			for i, syl := range line.Syllabus {
				el := rest
				if syl.IsBackground {
					el.IsBackground = true
				}
				isEnd := 0
				if i == len(line.Syllabus)-1 {
					isEnd = 1
				}
				flatLyrics = append(flatLyrics, domain.V1Segment{
					Time:         syl.Time,
					Duration:     syl.Duration,
					Text:         syl.Text,
					IsLineEnding: isEnd,
					Element:      el,
				})
			}
		}
	}

	typ := string(v2.Type)
	if v2.Type == domain.SyncTypeWord {
		typ = string(domain.SyncTypeSyllable)
	}

	cached := v2.Cached
	if cached == "" {
		cached = domain.CacheNone
	}

	return &domain.V1Response{
		Type:               typ,
		KpoeTools:          "2.0-V2toV1," + v2.KpoeTools,
		Metadata:           v2.Metadata,
		IgnoreSponsorblock: v2.IgnoreSponsorblock,
		Lyrics:             flatLyrics,
		Cached:             cached,
	}
}
