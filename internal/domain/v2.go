package domain

// SyncType describes the synchronization fidelity of a lyrics payload.
type SyncType string

const (
	SyncTypeWord     SyncType = "Word"
	SyncTypeLine     SyncType = "Line"
	SyncTypeNone     SyncType = "None"
	SyncTypeSyllable SyncType = "syllable" // legacy alias
)

// CacheLevel records where the response was served from.
type CacheLevel string

const (
	CacheNone     CacheLevel = "None"
	CacheGDrive   CacheLevel = "GDrive"
	CacheDatabase CacheLevel = "Database"
	CacheUserJSON CacheLevel = "UserJSON"
)

// LyricsResponse is the canonical V2 payload returned by /v2/lyrics/get.
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

// LyricsMetadata carries track and credit information.
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
}

// Agent models a credited performer or group used as a voice identity.
type Agent struct {
	Type  string `json:"type"`  // "person" or "group"
	Name  string `json:"name"`  // e.g. "Taylor Swift"
	Alias string `json:"alias"` // e.g. "v1", "v2"
}

// SongPart describes a structural section of the track.
type SongPart struct {
	Name     string `json:"name"`               // "Verse", "Chorus", ...
	Time     *int   `json:"time,omitempty"`     // Start offset in ms
	Duration *int   `json:"duration,omitempty"` // Duration in ms
	DivIndex *int   `json:"divIndex,omitempty"` // For TTML mapping
}

// AudioMetadata carries audio stream hints.
type AudioMetadata struct {
	LyricOffset string `json:"lyricOffset,omitempty"`
	Role        string `json:"role,omitempty"`
}

// Line is a synchronized line of lyrics.
type Line struct {
	Time            int              `json:"time"`     // ms from track start
	Duration        int              `json:"duration"` // ms
	Text            string           `json:"text"`
	IsLineEnding    *int             `json:"isLineEnding,omitempty"` // legacy V1 line terminal flag
	Syllabus        []Syllable       `json:"syllabus"`               // populated for word sync
	Element         LineElement      `json:"element"`
	Translation     *Translation     `json:"translation,omitempty"`
	Transliteration *Transliteration `json:"transliteration,omitempty"`
}

// Syllable is a word/word-segment with a timestamp.
type Syllable struct {
	Time         int    `json:"time"`     // ms
	Duration     int    `json:"duration"` // ms
	Text         string `json:"text"`     // token string (trailing space if appropriate)
	IsBackground bool   `json:"isBackground,omitempty"`
	Synthetic    bool   `json:"synthetic,omitempty"` // true if interpolated by alignment
}

// LineElement links a line to its key, singer and song part.
type LineElement struct {
	Key           string `json:"key"`                     // "L1", "v1", ...
	Singer        string `json:"singer,omitempty"`        // agent alias
	SongPartIndex *int   `json:"songPartIndex,omitempty"` // index into Metadata.SongParts
	SongPart      string `json:"songPart,omitempty"`      // deprecated V1 string name
	IsBackground  bool   `json:"isBackground,omitempty"`
}

// Translation is a localized subtitle for a line.
type Translation struct {
	Lang string `json:"lang"`
	Text string `json:"text"`
}

// Transliteration is a romanized/transliterated form of a line.
type Transliteration struct {
	Lang     string     `json:"lang"`
	Text     string     `json:"text"`
	Syllabus []Syllable `json:"syllabus,omitempty"`
}

// SourceStatus records a provider's outcome in the diagnostics.
type SourceStatus struct {
	Status    string `json:"status"` // "OK", "BAD", "RTO", "SKIP"
	ElapsedMs *int64 `json:"elapsedMs,omitempty"`
}

// PickedSongMetadata identifies the song the pipeline resolved for the query.
type PickedSongMetadata struct {
	Source string `json:"source"`
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
}

// ProcessTiming measures server-side processing windows and carries the
// orchestration diagnostics for the request.
type ProcessTiming struct {
	TimeElapsed          int64                   `json:"timeElapsed"`
	LastProcessed        int64                   `json:"lastProcessed"`
	TotalElapsedMs       int64                   `json:"totalElapsedMs"`
	WinnerSource         *string                 `json:"winnerSource,omitempty"`
	SyncPriority         *int                    `json:"syncPriority,omitempty"`
	SourcesStatus        map[string]SourceStatus `json:"sourcesStatus,omitempty"`
	SelectedSongMetadata *PickedSongMetadata     `json:"selectedSongMetadata,omitempty"`
}
