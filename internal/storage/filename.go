package storage

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// CanonicalFilename is the exact on-disk naming convention.
// {Artist} - {Title} [ {Album} ] ( {DurationSec.2f} ) < {ISRC}::{PlatformID} >.{ext}
func CanonicalFilename(artist, title, album string, durationMs int, isrc, platformID, ext string) string {
	var b strings.Builder
	b.WriteString(cleanup(artist))
	b.WriteString(" - ")
	b.WriteString(cleanup(strings.TrimSpace(title)))
	if album != "" {
		b.WriteString(" [")
		b.WriteString(cleanup(album))
		b.WriteString("]")
	}
	if durationMs > 0 {
		fmt.Fprintf(&b, " (%.2f)", float64(durationMs)/1000.0)
	}
	ir := "null"
	if isrc != "" {
		ir = strings.TrimSpace(isrc)
	}
	pr := "null"
	if platformID != "" {
		pr = strings.TrimSpace(platformID)
	}
	b.WriteString(" <" + ir + "::" + pr + ">")
	if ext != "" {
		b.WriteString("." + ext)
	}
	return b.String()
}

// cleanup strips illegal chars and collapses whitespace.
func cleanup(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '[', ']', '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

var (
	reISRCPlatform = regexp.MustCompile(`<([^>]+?)::([^>]+?)>$`)
	reDuration     = regexp.MustCompile(`\s\((\d+(?:\.\d+)?)\)$`)
	reAlbum        = regexp.MustCompile(`\s\[([^\]]+)\]$`)
	reArtistTitle  = regexp.MustCompile(`^(.+?)\s*-\s*(.+)$`)
)

// ParsedFilename is the decomposed form of a canonical filename.
type ParsedFilename struct {
	Artist     string
	Title      string
	Album      string
	DurationMS int
	ISRC       string
	PlatformID string
}

// ParseFilename decomposes a canonical filename.
func ParseFilename(name string) ParsedFilename {
	p := ParsedFilename{}

	n := name
	if dot := strings.LastIndexByte(n, '.'); dot >= 0 {
		n = n[:dot]
	}

	nameWithoutIsrcPlatform := n
	if m := reISRCPlatform.FindStringSubmatch(n); m != nil {
		left := n[:strings.LastIndex(n, m[0])]
		right := n[len(m[0])+strings.LastIndex(n, m[0]):]
		n = strings.TrimSpace(left + right)
		nameWithoutIsrcPlatform = n
		if m[1] != "null" {
			p.ISRC = strings.TrimSpace(m[1])
		}
		if m[2] != "null" {
			p.PlatformID = strings.TrimSpace(m[2])
		}
	}

	nameWithoutDuration := nameWithoutIsrcPlatform
	if m := reDuration.FindStringSubmatch(nameWithoutIsrcPlatform); m != nil {
		if secs, err := strconv.ParseFloat(m[1], 64); err == nil {
			p.DurationMS = int(math.Round(secs * 1000))
		}
		nameWithoutDuration = strings.TrimSpace(reDuration.ReplaceAllString(nameWithoutIsrcPlatform, ""))
	}

	nameWithoutAlbum := nameWithoutDuration
	if m := reAlbum.FindStringSubmatch(nameWithoutDuration); m != nil {
		p.Album = strings.TrimSpace(m[1])
		nameWithoutAlbum = strings.TrimSpace(reAlbum.ReplaceAllString(nameWithoutDuration, ""))
	}

	if m := reArtistTitle.FindStringSubmatch(nameWithoutAlbum); m != nil {
		p.Artist = strings.TrimSpace(m[1])
		p.Title = strings.TrimSpace(m[2])
	} else {
		p.Title = strings.TrimSpace(nameWithoutAlbum)
	}
	return p
}

// stopWords are stripped during keyword extraction.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "feat": true,
	"ft": true, "featuring": true, "from": true, "this": true, "that": true,
	"you": true, "your": true, "are": true, "was": true, "were": true,
	"original": true, "version": true, "audio": true, "video": true,
}

var reStripPunct = regexp.MustCompile(`[<>[\](){}_\\/|:;!?,.*~` + "`" + `"@#$%^&+=]`)

func isCJK(r rune) bool {
	return (r >= 0x3040 && r <= 0x30FF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xAC00 && r <= 0xD7AF)
}

func hasCJK(s string) bool {
	for _, r := range s {
		if isCJK(r) {
			return true
		}
	}
	return false
}

// ExtractKeywords pulls up to 2 meaningful search keywords.
func ExtractKeywords(s string) []string {
	cleaned := strings.TrimSpace(reStripPunct.ReplaceAllString(s, " "))
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if cleaned == "" {
		return nil
	}
	words := strings.Fields(cleaned)
	var keywords []string
	for _, w := range words {
		if stopWords[strings.ToLower(w)] {
			continue
		}
		if hasCJK(w) || len(w) >= 2 {
			keywords = append(keywords, w)
		}
		if len(keywords) >= 2 {
			break
		}
	}
	if len(keywords) == 0 && len(words) > 0 {
		keywords = append(keywords, words[0])
	} else if len(keywords) == 0 && len(cleaned) > 0 {
		runes := []rune(cleaned)
		if len(runes) > 10 {
			runes = runes[:10]
		}
		keywords = append(keywords, string(runes))
	}
	return keywords
}
