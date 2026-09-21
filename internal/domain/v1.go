package domain

// V1Response is the payload returned by /v1/lyrics/get (flat syllable segments).
type V1Response struct {
	Type               string         `json:"type"` // "syllable" or "Line"
	KpoeTools          string         `json:"KpoeTools"`
	Metadata           LyricsMetadata `json:"metadata"`
	IgnoreSponsorblock *bool          `json:"ignoreSponsorblock,omitempty"`
	Lyrics             []V1Segment    `json:"lyrics"`
	Cached             CacheLevel     `json:"cached"`
	ProcessingTime     *ProcessTiming `json:"processingTime,omitempty"`
}

// V1Segment is a single flat lyrics token.
type V1Segment struct {
	Time         int              `json:"time"`
	Duration     int              `json:"duration"`
	Text         string           `json:"text"`
	IsLineEnding int              `json:"isLineEnding"` // 1 on line terminal, 0 otherwise
	Element      V1SegmentElement `json:"element"`
}

// V1SegmentElement carries per-segment identity.
type V1SegmentElement struct {
	Key          string `json:"key"`
	SongPart     string `json:"songPart"`
	Singer       string `json:"singer"`
	IsBackground bool   `json:"isBackground,omitempty"`
}
