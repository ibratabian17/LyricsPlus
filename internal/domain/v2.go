package domain

type SyncType string

const (
	SyncTypeWord     SyncType = "Word"
	SyncTypeLine     SyncType = "Line"
	SyncTypeNone     SyncType = "None"
	SyncTypeSyllable SyncType = "syllable"
)

type CacheLevel string

const (
	CacheNone     CacheLevel = "None"
	CacheGDrive   CacheLevel = "GDrive"
	CacheDatabase CacheLevel = "Database"
	CacheUserJSON CacheLevel = "UserJSON"
)

type LyricsResponse struct {
	Type               SyncType       `json:"type"`
	KpoeTools          string         `json:"KpoeTools"`
	Metadata           LyricsMetadata `json:"metadata"`
	IgnoreSponsorblock *bool          `json:"ignoreSponsorblock,omitempty"`
	Lyrics             []Line         `json:"lyrics"`
	Cached             CacheLevel     `json:"cached"`
	ProcessingTime     *ProcessTiming `json:"processingTime,omitempty"`
	RawData            string         `json:"-"`
}

type LyricsMetadata struct {
	Source         string           `json:"source"`
	Title          string           `json:"title,omitempty"`
	Artist         string           `json:"artist,omitempty"`
	Album          string           `json:"album,omitempty"`
	SongWriters    []string         `json:"songWriters"`
	LeadingSilence string           `json:"leadingSilence,omitempty"`
	Agents         map[string]Agent `json:"agents,omitempty"`
	SongParts      []SongPart       `json:"songParts,omitempty"`
	Language       string           `json:"language,omitempty"`
	TotalDuration  string           `json:"totalDuration,omitempty"`
	Curator        string           `json:"curator,omitempty"`
	Audio          []AudioMetadata  `json:"audio,omitempty"`
	Copyright      string           `json:"copyright,omitempty"`
	Licence        string           `json:"licence,omitempty"`
}

type Agent struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Alias string `json:"alias"`
}

type SongPart struct {
	Name     string `json:"name"`
	Time     *int   `json:"time,omitempty"`
	Duration *int   `json:"duration,omitempty"`
	DivIndex *int   `json:"divIndex,omitempty"`
}

type AudioMetadata struct {
	LyricOffset string `json:"lyricOffset,omitempty"`
	Role        string `json:"role,omitempty"`
}

type Line struct {
	Time            int              `json:"time"`
	Duration        int              `json:"duration"`
	Text            string           `json:"text"`
	IsLineEnding    *int             `json:"isLineEnding,omitempty"`
	Syllabus        []Syllable       `json:"syllabus,omitempty"`
	Element         LineElement      `json:"element"`
	Translation     *Translation     `json:"translation,omitempty"`
	Transliteration *Transliteration `json:"transliteration,omitempty"`
}

type Syllable struct {
	Time         int    `json:"time"`
	Duration     int    `json:"duration"`
	Text         string `json:"text"`
	IsBackground bool   `json:"isBackground,omitempty"`
	Synthetic    bool   `json:"synthetic,omitempty"`
}

type LineElement struct {
	Key           string `json:"key"`
	Singer        string `json:"singer,omitempty"`
	SongPartIndex *int   `json:"songPartIndex,omitempty"`
	SongPart      string `json:"songPart,omitempty"`
	IsBackground  bool   `json:"isBackground,omitempty"`
}

type Translation struct {
	Lang string `json:"lang"`
	Text string `json:"text"`
}

type Transliteration struct {
	Lang     string     `json:"lang"`
	Text     string     `json:"text"`
	Syllabus []Syllable `json:"syllabus,omitempty"`
}

type SourceStatus struct {
	Status    string `json:"status"`
	ElapsedMs *int64 `json:"elapsedMs,omitempty"`
}

type PickedSongMetadata struct {
	Source         string   `json:"source"`
	Title          string   `json:"title,omitempty"`
	Artist         string   `json:"artist,omitempty"`
	Album          string   `json:"album,omitempty"`
	Duration       *float64 `json:"duration,omitempty"`
	SongISRC       string   `json:"songISRC,omitempty"`
	SongPlatformID string   `json:"songPlatformId,omitempty"`
}

type ProcessTiming struct {
	TimeElapsed          int64                   `json:"timeElapsed"`
	LastProcessed        int64                   `json:"lastProcessed"`
	TotalElapsedMs       int64                   `json:"totalElapsedMs"`
	WinnerSource         *string                 `json:"winnerSource,omitempty"`
	SyncPriority         *int                    `json:"syncPriority,omitempty"`
	SourcesStatus        map[string]SourceStatus `json:"sourcesStatus,omitempty"`
	SelectedSongMetadata *PickedSongMetadata     `json:"selectedSongMetadata,omitempty"`
}
