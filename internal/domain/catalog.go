package domain

import (
	"strconv"
	"strings"
)

type SongCatalogItem struct {
	ID           map[string]string `json:"id"`
	SourceID     string            `json:"sourceId"`
	Title        string            `json:"title"`
	Artist       string            `json:"artist"`
	Album        string            `json:"album"`
	AlbumArtURL  *string           `json:"albumArtUrl"`
	DurationMs   int64             `json:"durationMs"`
	ISRC         *string           `json:"isrc"`
	Songwriters  []string          `json:"songwriters"`
	Availability []string          `json:"availability"`
	ExternalURLs map[string]string `json:"externalUrls"`
}

type SearchQuery struct {
	Title       string
	Artist      string
	Album       string
	Duration    int
	ISRC        string
	PlatformID  string
	Sources     []string
	ForceReload bool
}

func (q SearchQuery) IDOnly() bool {
	return (q.ISRC != "" || q.PlatformID != "") && q.Title == "" && q.Artist == ""
}

func (q SearchQuery) NormalizeKey() string {
	sources := "default"
	if len(q.Sources) > 0 {
		sources = ""
		for i, s := range q.Sources {
			if i > 0 {
				sources += ","
			}
			sources += s
		}
	}
	return strings.Join([]string{
		strings.ToLower(q.Title),
		strings.ToLower(q.Artist),
		strings.ToLower(q.Album),
		strconv.Itoa(q.Duration),
		strings.ToLower(q.ISRC),
		strings.ToLower(q.PlatformID),
		sources,
	}, "::")
}

func (q SearchQuery) ContentKey() string {
	return strings.Join([]string{
		strings.ToLower(q.Title),
		strings.ToLower(q.Artist),
		strings.ToLower(q.Album),
		strconv.Itoa(q.Duration),
		strings.ToLower(q.ISRC),
		strings.ToLower(q.PlatformID),
	}, "::")
}
