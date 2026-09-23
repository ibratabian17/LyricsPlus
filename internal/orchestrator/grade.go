package orchestrator

import (
	"strings"

	"lyricsplus/backend/internal/domain"
)

// Priority grades a lyrics payload per the sync fidelity table.
const (
	PriorityFailed = 0
	PriorityUnsync = 1
	PriorityLine   = 2
	PriorityWord   = 3
)

// HasSyllableSync reports whether the payload carries word/syllable sync.
func HasSyllableSync(resp *domain.LyricsResponse) bool {
	if resp == nil {
		return false
	}
	switch resp.Type {
	case domain.SyncTypeWord, domain.SyncTypeSyllable:
		return true
	}
	for _, l := range resp.Lyrics {
		if len(l.Syllabus) > 0 {
			return true
		}
	}
	return false
}

// Grade evaluates a payload and returns its sync priority (0-3).
func Grade(resp *domain.LyricsResponse, source string) int {
	if resp == nil || len(resp.Lyrics) == 0 {
		return PriorityFailed
	}

	contains := func(s, sub string) bool { return strings.Contains(s, sub) }
	contentTrusted := false
	if s := strings.ToLower(source); s != "" {
		contentTrusted = contains(s, "apple") || contains(s, "lyricsplus") || contains(s, "qaple")
	}

	switch resp.Type {
	case domain.SyncTypeWord, domain.SyncTypeSyllable:
		return PriorityWord
	case domain.SyncTypeLine:
		if contentTrusted && HasSyllableSync(resp) {
			return PriorityWord
		}
		return PriorityLine
	case domain.SyncTypeNone, "":
		return PriorityUnsync
	}
	return PriorityUnsync
}

// SourceOrder returns the source list per the id-only rule.
func SourceOrder(query domain.SearchQuery, preferred []string) []string {
	if len(preferred) > 0 {
		return preferred
	}
	if query.IDOnly() {
		return []string{"apple", "lyricsplus", "qq", "musixmatch"}
	}
	return []string{"apple", "lyricsplus", "deezer", "qq", "musixmatch-word", "musixmatch"}
}
