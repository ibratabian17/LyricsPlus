package domain

type V1Response struct {
	Type               string         `json:"type"`
	KpoeTools          string         `json:"KpoeTools"`
	Metadata           LyricsMetadata `json:"metadata"`
	IgnoreSponsorblock *bool          `json:"ignoreSponsorblock,omitempty"`
	Lyrics             []V1Segment    `json:"lyrics"`
	Cached             CacheLevel     `json:"cached"`
	ProcessingTime     *ProcessTiming `json:"processingTime,omitempty"`
}

type V1Segment struct {
	Time         int              `json:"time"`
	Duration     int              `json:"duration"`
	Text         string           `json:"text"`
	IsLineEnding int              `json:"isLineEnding"`
	Element      V1SegmentElement `json:"element"`
}

type V1SegmentElement struct {
	Key          string `json:"key"`
	SongPart     string `json:"songPart"`
	Singer       string `json:"singer"`
	IsBackground bool   `json:"isBackground,omitempty"`
}
