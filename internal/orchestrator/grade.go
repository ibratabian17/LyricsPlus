package orchestrator

import (
	"lyricsplus/backend/internal/domain"
)

// Priority grades a lyrics payload per the sync fidelity table.
const (
	PriorityFailed = 0
	PriorityUnsync = 1
	PriorityLine   = 2
	PriorityWord   = 3
)

// HasSyllableSync reports whether the payload contains verified syllable
// timestamps (>=80% of lines carry one or more timestamped syllabus tokens).
func HasSyllableSync(resp *domain.LyricsResponse) bool {
	if resp == nil || len(resp.Lyrics) == 0 {
		return false
	}
	total := 0
	withSyl := 0
	for _, l := range resp.Lyrics {
		total++
		if len(l.Syllabus) > 0 {
			withSyl++
		}
	}
	if total == 0 {
		return false
	}
	ratio := float64(withSyl) / float64(total)
	return ratio >= 0.8
}

// Grade evaluates a payload and returns its sync priority (0-3).
func Grade(resp *domain.LyricsResponse, source string) int {
	if resp == nil || len(resp.Lyrics) == 0 {
		return PriorityFailed
	}
	switch resp.Type {
	case domain.SyncTypeWord, domain.SyncTypeSyllable:
		switch source {
		case "apple", "lyricsplus", "qaple", "musixmatch":
			if HasSyllableSync(resp) {
				return PriorityWord
			}
			// Word-typed but unverified syllable data downgrades to line.
			return PriorityLine
		default:
			return PriorityWord
		}
	case domain.SyncTypeLine:
		return PriorityLine
	case domain.SyncTypeNone, "":
		return PriorityUnsync
	}
	return PriorityUnsync
}

// abilitySourceOrder returns the source list per the id-only rule.
func sourceOrder(query domain.SearchQuery, preferred []string) []string {
	if len(preferred) > 0 {
		return preferred
	}
	if query.IDOnly() {
		return []string{"apple", "lyricsplus", "qq", "musixmatch"}
	}
	return []string{"apple", "lyricsplus", "deezer", "qq", "musixmatch-word", "musixmatch"}
}
