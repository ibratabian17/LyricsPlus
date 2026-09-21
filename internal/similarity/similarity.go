// Package similarity provides string similarity scoring for lyric matching.
package similarity

import (
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/mozillazg/go-unidecode"
)

func normalizeString(str string) string {
	if str == "" {
		return ""
	}
	s := nonWordSpace.ReplaceAllString(strings.ToLower(str), " ")
	s = collapseSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func getNGrams(str string, size int) map[string]bool {
	if str == "" || len(str) < size {
		return map[string]bool{}
	}
	set := make(map[string]bool)
	for i := 0; i <= len(str)-size; i++ {
		set[str[i:i+size]] = true
	}
	return set
}

// SorensenDice returns the set-based bigram overlap coefficient.
func SorensenDice(str1, str2 string) float64 {
	if str1 == "" && str2 == "" {
		return 1.0
	}
	if str1 == "" || str2 == "" {
		return 0.0
	}
	bigrams1 := getNGrams(str1, 2)
	bigrams2 := getNGrams(str2, 2)
	if len(bigrams1) == 0 && len(bigrams2) == 0 {
		return 1.0
	}
	if len(bigrams1) == 0 || len(bigrams2) == 0 {
		return 0.0
	}
	intersection := 0
	for g := range bigrams1 {
		if bigrams2[g] {
			intersection++
		}
	}
	return (2 * float64(intersection)) / (float64(len(bigrams1)) + float64(len(bigrams2)))
}

func levenshteinDistance(str1, str2 string) int {
	if str1 == str2 {
		return 0
	}
	if len(str1) == 0 {
		return len(str2)
	}
	if len(str2) == 0 {
		return len(str1)
	}
	if len(str1) > len(str2) {
		str1, str2 = str2, str1
	}
	prevRow := make([]int, len(str1)+1)
	currRow := make([]int, len(str1)+1)
	for i := range prevRow {
		prevRow[i] = i
	}
	for j := 1; j <= len(str2); j++ {
		currRow[0] = j
		for i := 1; i <= len(str1); i++ {
			cost := 0
			if str1[i-1] != str2[j-1] {
				cost = 1
			}
			currRow[i] = minInt(currRow[i-1]+1, minInt(prevRow[i]+1, prevRow[i-1]+cost))
		}
		prevRow, currRow = currRow, prevRow
	}
	return prevRow[len(str1)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// LevenshteinNorm returns 1 - Levenshtein/max(len).
func LevenshteinNorm(str1, str2 string) float64 {
	maxLen := math.Max(float64(len(str1)), float64(len(str2)))
	if maxLen == 0 {
		return 1.0
	}
	return 1.0 - float64(levenshteinDistance(str1, str2))/maxLen
}

// Analysis is the result of AnalyzeTitle.
type Analysis struct {
	BaseTitle       string
	Tags            map[string]bool
	FeatArtists     []string
	BracketContents []string
}

var (
	featPieces           = []string{"feat.", "feat", "ft.", "ft", "featuring", "with"}
	nonWordSpace         = regexp.MustCompile(`[^\w\s]`)
	collapseSpace        = regexp.MustCompile(`\s+`)
	keepTitleChars       = regexp.MustCompile(`[^\w\s\[\](){}]`)
	bracketRegex         = regexp.MustCompile(`(?:\(([^)]*)\)|\[([^\]]*)\]|\{([^}]*)\})`)
	stripSquare          = regexp.MustCompile(`\[[^\]]*\]`)
	stripParen           = regexp.MustCompile(`\([^)]*\)`)
	stripBrace           = regexp.MustCompile(`\{[^}]*\}`)
	trailingDashSegment  = regexp.MustCompile(`\s-\s.*$`)
	stripLeadingArticle  = regexp.MustCompile(`(?i)^(?:the\s+|a\s+|an\s+)`)
	stripTrailingArticle = regexp.MustCompile(`(?i)\s+(?:the|a|an)$`)
	artistSeparator      = regexp.MustCompile(`(?i)\s*(?:&|and|vs\.?|versus|x|feat\.?|ft\.?|featuring|with|,)\s*`)
	artistChunkSep       = regexp.MustCompile(`\s*[,&]\s*`)
	bracketStripper      = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)`)
	stripAlbumSingleEP   = regexp.MustCompile(`(?i)\s+-\s+(?:single|ep)\s*$`)
	stripAlbumNoise      = regexp.MustCompile(`(?i)\b(?:deluxe|anniversary|special|expanded|remastered|remaster|edition|version)\b`)
	translit             = unidecode.Unidecode
)

var anchorTagPatterns = []struct {
	re  *regexp.Regexp
	tag string
}{
	{regexp.MustCompile(`(?:[([-]|\s-\s)(remix|mix|rmx)(?:\W|$)`), "remix"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(live|concert)(?:\W|$)`), "live"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(acoustic|unplugged)(?:\W|$)`), "acoustic"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(instrumental|karaoke)(?:\W|$)`), "instrumental"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(radio\s?edit|single\s?edit)(?:\W|$)`), "radioedit"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(remaster(?:ed)?|rerecorded?)(?:\W|$)`), "remastered"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(explicit|clean|censored)(?:\W|$)`), "explicit"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(demo|rough\s?mix?)(?:\W|$)`), "demo"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(extended|ext(?:\s|$))(?:\W|$)`), "extended"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(deluxe|anniversary|special)(?:\W|$)`), "deluxe"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(mono|stereo)(?:\W|$)`), "mono"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(edit|version|ver\s)(?:\W|$)`), "version"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(sing.?along)(?:\W|$)`), "singalong"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(cover|tribute)(?:\W|$)`), "cover"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(nightcore|slowed|sped.?up)(?:\W|$)`), "speedvariant"},
	{regexp.MustCompile(`(?:[([-]|\s-\s)(lofi|lo.fi)(?:\W|$)`), "lofi"},
}

var contentKeywordMap = []struct {
	kw  *regexp.Regexp
	tag string
}{
	{regexp.MustCompile(`\b(?:remix|rmx)\b`), "remix"},
	{regexp.MustCompile(`\bmix\b`), "mix"},
	{regexp.MustCompile(`\b(?:live|concert)\b`), "live"},
	{regexp.MustCompile(`\b(?:acoustic|unplugged)\b`), "acoustic"},
	{regexp.MustCompile(`\binstrumental\b`), "instrumental"},
	{regexp.MustCompile(`\bkaraoke\b`), "karaoke"},
	{regexp.MustCompile(`\bextended\b`), "extended"},
	{regexp.MustCompile(`\b(?:edit|edited)\b`), "version"},
	{regexp.MustCompile(`\b(?:version|ver)\b`), "version"},
	{regexp.MustCompile(`\b(?:remaster(?:ed)?)\b`), "remastered"},
	{regexp.MustCompile(`\b(?:demo)\b`), "demo"},
	{regexp.MustCompile(`\b(?:cover|tribute)\b`), "cover"},
	{regexp.MustCompile(`\b(?:nightcore|slowed)\b`), "speedvariant"},
	{regexp.MustCompile(`\b(?:lofi|lo.fi)\b`), "lofi"},
	{regexp.MustCompile(`\b(?:explicit|clean)\b`), "explicit"},
	{regexp.MustCompile(`\b(?:mono|stereo)\b`), "mono"},
	{regexp.MustCompile(`\bradio\s?edit\b`), "radioedit"},
	{regexp.MustCompile(`\b(?:original\s+mix|original\s+version)\b`), "originalversion"},
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func isBracketByte(b byte) bool {
	return b == '(' || b == '[' || b == ']' || b == '{' || b == '}'
}

// extractFeatPat greedily finds "(feat|ft|featuring|with) <artists>" segments
// whose captured artists are followed by whitespace + a bracket or end of string.
func extractFeatPat(s string) ([]string, string) {
	var artists []string
	rest := s
	lower := strings.ToLower(rest)
	for {
		bestIdx := -1
		bestWord := ""
		for _, w := range featPieces {
			i := strings.Index(lower, w)
			if i < 0 {
				continue
			}
			if i > 0 && !isSpaceByte(lower[i-1]) {
				continue
			}
			e := i + len(w)
			if e < len(lower) && !isSpaceByte(lower[e]) {
				continue
			}
			if bestIdx < 0 || i < bestIdx || (i == bestIdx && len(w) > len(bestWord)) {
				bestIdx, bestWord = i, w
			}
		}
		if bestIdx < 0 {
			break
		}
		e := bestIdx + len(bestWord)
		j := e
		for j < len(rest) && isSpaceByte(rest[j]) {
			j++
		}
		k := j
		for k < len(rest) && !isBracketByte(rest[k]) {
			k++
		}
		seg := strings.TrimSpace(rest[j:k])
		lk := strings.TrimLeft(rest[k:], " \t\r\n")
		if seg == "" || (lk != "" && !strings.ContainsAny(lk[:1], "()[]{}")) {
			break
		}
		for _, chunk := range artistChunkSep.Split(seg, -1) {
			chunk = strings.TrimSpace(chunk)
			if chunk != "" {
				artists = append(artists, chunk)
			}
		}
		rest = rest[:bestIdx] + " " + rest[k:]
		lower = strings.ToLower(rest)
	}
	return artists, rest
}

// AnalyzeTitle splits a title into its base, tags, feature artists, and bracket contents.
func AnalyzeTitle(title string) Analysis {
	if strings.TrimSpace(title) == "" {
		return Analysis{Tags: map[string]bool{}, FeatArtists: []string{}, BracketContents: []string{}}
	}
	tags := map[string]bool{}
	var featArtists []string
	var bracketContents []string

	cleanTitle := strings.ToLower(title)

	fa, cleaned := extractFeatPat(cleanTitle)
	featArtists = append(featArtists, fa...)
	cleanTitle = cleaned

	cleanTitle = keepTitleChars.ReplaceAllString(cleanTitle, " ")
	cleanTitle = collapseSpace.ReplaceAllString(cleanTitle, " ")

	for _, p := range anchorTagPatterns {
		if p.re.MatchString(cleanTitle) {
			tags[p.tag] = true
		}
	}

	bracketMatch := bracketRegex.FindAllStringSubmatch(cleanTitle, -1)
	for _, m := range bracketMatch {
		content := m[1]
		if content == "" {
			content = m[2]
		}
		if content == "" {
			content = m[3]
		}
		content = strings.TrimSpace(content)
		if content != "" {
			bracketContents = append(bracketContents, content)
			for _, ck := range contentKeywordMap {
				if ck.kw.MatchString(content) {
					tags[ck.tag] = true
				}
			}
		}
	}

	cleanTitle = stripSquare.ReplaceAllString(cleanTitle, " ")
	cleanTitle = stripParen.ReplaceAllString(cleanTitle, " ")
	cleanTitle = stripBrace.ReplaceAllString(cleanTitle, " ")
	cleanTitle = trailingDashSegment.ReplaceAllString(cleanTitle, " ")
	cleanTitle = collapseSpace.ReplaceAllString(cleanTitle, " ")
	cleanTitle = strings.TrimSpace(cleanTitle)
	cleanTitle = stripLeadingArticle.ReplaceAllString(cleanTitle, "")
	cleanTitle = stripTrailingArticle.ReplaceAllString(cleanTitle, "")

	return Analysis{BaseTitle: cleanTitle, Tags: tags, FeatArtists: featArtists, BracketContents: bracketContents}
}

// normalizeArtistName normalizes an artist name for comparison.
func normalizeArtistName(artist string) string {
	if artist == "" {
		return ""
	}
	normalized := strings.ToLower(bracketStripper.ReplaceAllString(artist, ""))
	var parts []string
	for _, name := range artistSeparator.Split(normalized, -1) {
		name = strings.ReplaceAll(name, "the", "")
		name = collapseSpace.ReplaceAllString(name, " ")
		name = strings.TrimSpace(name)
		if len(name) > 0 {
			parts = append(parts, name)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

var criticalTagSet = map[string]bool{
	"live": true, "acoustic": true, "remix": true, "mix": true,
	"instrumental": true, "karaoke": true, "cover": true,
	"speedvariant": true, "lofi": true,
}

func containsString(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func sharesAny(a, b []string) bool {
	for _, x := range a {
		if containsString(b, x) {
			return true
		}
	}
	return false
}

func criticalTagsOf(tags map[string]bool) []string {
	var out []string
	for t := range tags {
		if criticalTagSet[t] {
			out = append(out, t)
		}
	}
	return out
}

func containsNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			return true
		}
	}
	return false
}

// TitleSimilarity scores how closely two titles match.
func TitleSimilarity(title1, title2 string) float64 {
	computeScore := func(t1, t2 string) float64 {
		if t1 == "" || t2 == "" {
			return 0
		}
		analysis1 := AnalyzeTitle(t1)
		analysis2 := AnalyzeTitle(t2)
		crit1 := criticalTagsOf(analysis1.Tags)
		crit2 := criticalTagsOf(analysis2.Tags)

		if analysis1.BaseTitle == analysis2.BaseTitle && len(analysis1.BaseTitle) > 0 {
			oneSideCritical := false
			for _, t := range crit1 {
				if !analysis2.Tags[t] {
					oneSideCritical = true
				}
			}
			for _, t := range crit2 {
				if !analysis1.Tags[t] {
					oneSideCritical = true
				}
			}
			if oneSideCritical {
				bothConflict := len(crit1) > 0 && len(crit2) > 0 && !sharesAny(crit1, crit2)
				if bothConflict {
					return 0.65
				}
				return 0.72
			}
			candHasExtra := len(analysis1.BracketContents) > len(analysis2.BracketContents)
			queryHasExtra := len(analysis2.BracketContents) > len(analysis1.BracketContents)
			if candHasExtra || queryHasExtra {
				return 0.88
			}
			return 1.0
		}

		diceScore := SorensenDice(analysis1.BaseTitle, analysis2.BaseTitle)
		if diceScore < 0.2 {
			return 0
		}
		maxLength := math.Max(float64(len(analysis1.BaseTitle)), float64(len(analysis2.BaseTitle)))
		levenshteinScore := 0.0
		if maxLength > 0 {
			levenshteinScore = 1 - float64(levenshteinDistance(analysis1.BaseTitle, analysis2.BaseTitle))/maxLength
		}
		baseSimilarity := diceScore*0.7 + levenshteinScore*0.3

		tagPenalty := 0.0
		if len(crit1) > 0 && len(crit2) > 0 {
			hasConflict := true
			for _, t := range crit1 {
				if containsString(crit2, t) {
					hasConflict = false
				}
			}
			if hasConflict {
				tagPenalty = 0.4
			}
		} else if len(crit1) > 0 || len(crit2) > 0 {
			tagPenalty = 0.15
		}

		return math.Max(0, baseSimilarity-tagPenalty)
	}

	scoreOriginal := computeScore(title1, title2)
	if scoreOriginal >= 0.8 {
		return scoreOriginal
	}
	if !containsNonASCII(title1) && !containsNonASCII(title2) {
		return scoreOriginal
	}
	return math.Max(scoreOriginal, computeScore(translit(title1), translit(title2)))
}

// ArtistSimilarity scores how closely two artists match.
func ArtistSimilarity(artist1, artist2 string, analysis1, analysis2 *Analysis) float64 {
	computeScore := func(a1, a2 string, feats1, feats2 []string) float64 {
		if a1 == "" || a2 == "" {
			return 0
		}
		norm1 := normalizeArtistName(a1)
		norm2 := normalizeArtistName(a2)
		if norm1 == norm2 {
			return 1.0
		}

		allArtists1 := map[string]bool{}
		allArtists2 := map[string]bool{}
		for _, a := range artistChunkSep.Split(a1, -1) {
			if n := normalizeArtistName(a); n != "" {
				allArtists1[n] = true
			}
		}
		for _, a := range artistChunkSep.Split(a2, -1) {
			if n := normalizeArtistName(a); n != "" {
				allArtists2[n] = true
			}
		}
		for _, f := range feats1 {
			if n := normalizeArtistName(f); n != "" {
				allArtists1[n] = true
			}
		}
		for _, f := range feats2 {
			if n := normalizeArtistName(f); n != "" {
				allArtists2[n] = true
			}
		}
		artists1Array := make([]string, 0, len(allArtists1))
		for a := range allArtists1 {
			artists1Array = append(artists1Array, a)
		}
		artists2Array := make([]string, 0, len(allArtists2))
		for a := range allArtists2 {
			artists2Array = append(artists2Array, a)
		}

		overlap1to2 := 0
		for _, n := range artists1Array {
			if allArtists2[n] {
				overlap1to2++
			}
		}
		overlap2to1 := 0
		for _, n := range artists2Array {
			if allArtists1[n] {
				overlap2to1++
			}
		}
		minSize := len(artists1Array)
		if len(artists2Array) < minSize {
			minSize = len(artists2Array)
		}
		maxOverlap := overlap1to2
		if overlap2to1 > maxOverlap {
			maxOverlap = overlap2to1
		}

		if minSize > 0 && maxOverlap == minSize {
			return 1.0
		}
		if maxOverlap > 0 {
			denom := len(artists1Array)
			if len(artists2Array) > denom {
				denom = len(artists2Array)
			}
			return 0.7 + 0.3*float64(maxOverlap)/float64(denom)
		}
		return SorensenDice(norm1, norm2)
	}

	var feats1, feats2 []string
	if analysis1 != nil {
		feats1 = analysis1.FeatArtists
	}
	if analysis2 != nil {
		feats2 = analysis2.FeatArtists
	}
	scoreOriginal := computeScore(artist1, artist2, feats1, feats2)
	if scoreOriginal >= 0.8 {
		return scoreOriginal
	}
	if !containsNonASCII(artist1) && !containsNonASCII(artist2) {
		return scoreOriginal
	}
	scoreRomanized := computeScore(
		translit(artist1), translit(artist2),
		unidecodeSlice(feats1), unidecodeSlice(feats2),
	)
	return math.Max(scoreOriginal, scoreRomanized)
}

func unidecodeSlice(xs []string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, translit(x))
	}
	return out
}

// DurationScore returns the tiered decay for an absolute duration difference in SECONDS.
func DurationScore(diff float64) float64 {
	switch {
	case diff == 0:
		return 1.0
	case diff <= 1.0:
		return 0.98
	case diff <= 2.0:
		return 0.95
	case diff <= 4.0:
		return 0.85
	case diff <= 7.0:
		return 0.70
	case diff <= 12.0:
		return 0.50
	case diff <= 20.0:
		return 0.30
	case diff <= 35.0:
		return 0.15
	case diff <= 60.0:
		return 0.05
	default:
		return 0.0
	}
}

// DurationSimilarity scores two durations in seconds; missing/<=0 values score 0.7.
func DurationSimilarity(d1, d2 float64) float64 {
	if d1 <= 0 || d2 <= 0 {
		return 0.7
	}
	return DurationScore(math.Abs(d1 - d2))
}

// AlbumSimilarity scores how closely two albums match.
func AlbumSimilarity(album1, album2 string) float64 {
	if album1 == "" || album2 == "" {
		return 0.5
	}
	stripNoise := func(s string) string {
		s = stripAlbumSingleEP.ReplaceAllString(s, "")
		s = stripAlbumNoise.ReplaceAllString(s, "")
		return collapseSpace.ReplaceAllString(s, " ")
	}
	norm1 := normalizeString(stripNoise(album1))
	norm2 := normalizeString(stripNoise(album2))
	if norm1 == norm2 {
		return 1.0
	}
	dice := math.Max(
		SorensenDice(norm1, norm2),
		SorensenDice(normalizeString(stripNoise(translit(album1))), normalizeString(stripNoise(translit(album2)))),
	)
	if dice < 0.2 {
		return 0.1
	}
	return dice
}

// Components holds the per-criterion similarity scores.
type Components struct {
	TitleScore    float64
	ArtistScore   float64
	AlbumScore    float64
	DurationScore float64
}

// Weights holds the per-criterion weights.
type Weights struct {
	Title    float64
	Artist   float64
	Album    float64
	Duration float64
}

// ScoreInfo is the full result of SongSimilarity.
type ScoreInfo struct {
	Score      float64
	Reason     string
	Components Components
	Weights    Weights
	QueryDur   float64
	CandDur    float64
}

// SongSimilarity scores a candidate track against a query. Durations are in seconds.
func SongSimilarity(candTitle, candArtist, candAlbum string, candDur float64,
	queryTitle, queryArtist, queryAlbum string, queryDur float64) ScoreInfo {

	if candTitle == "" || candArtist == "" {
		return ScoreInfo{Score: 0, Reason: "Missing title or artist"}
	}

	queryTitleAnalysis := AnalyzeTitle(queryTitle)
	candTitleAnalysis := AnalyzeTitle(candTitle)

	titleScore := TitleSimilarity(candTitle, queryTitle)
	artistScore := ArtistSimilarity(candArtist, queryArtist, &candTitleAnalysis, &queryTitleAnalysis)
	albumScore := AlbumSimilarity(candAlbum, queryAlbum)
	durationScore := DurationSimilarity(candDur, queryDur)

	const titleThreshold = 0.7
	const artistThreshold = 0.6

	if titleScore < titleThreshold {
		return ScoreInfo{
			Score:      math.Min(0.4, titleScore*0.5),
			Reason:     "Title similarity too low",
			Components: Components{titleScore, artistScore, albumScore, durationScore},
			QueryDur:   queryDur, CandDur: candDur,
		}
	}
	if artistScore < artistThreshold {
		return ScoreInfo{
			Score:      math.Min(0.5, artistScore*0.7),
			Reason:     "Artist similarity too low",
			Components: Components{titleScore, artistScore, albumScore, durationScore},
			QueryDur:   queryDur, CandDur: candDur,
		}
	}
	if queryDur > 0 && candDur > 0 {
		durDiff := math.Abs(queryDur - candDur)
		if durDiff > 60 {
			return ScoreInfo{
				Score:      math.Min(0.45, (titleScore+artistScore)/2*0.6),
				Reason:     "Duration mismatch critical",
				Components: Components{titleScore, artistScore, albumScore, durationScore},
				QueryDur:   queryDur, CandDur: candDur,
			}
		}
		if durDiff > 30 {
			return ScoreInfo{
				Score:      math.Min(0.58, (titleScore+artistScore)/2*0.75),
				Reason:     "Duration mismatch severe",
				Components: Components{titleScore, artistScore, albumScore, durationScore},
				QueryDur:   queryDur, CandDur: candDur,
			}
		}
	}

	hasDuration := queryDur > 0 && candDur > 0
	hasAlbum := queryAlbum != "" && candAlbum != ""

	var weights Weights
	switch {
	case hasAlbum && hasDuration:
		weights = Weights{0.30, 0.30, 0.20, 0.20}
	case hasAlbum:
		weights = Weights{0.38, 0.38, 0.24, 0.00}
	case hasDuration:
		weights = Weights{0.38, 0.35, 0.05, 0.22}
	default:
		weights = Weights{0.52, 0.42, 0.06, 0.00}
	}

	finalScore := titleScore*weights.Title +
		artistScore*weights.Artist +
		albumScore*weights.Album +
		durationScore*weights.Duration

	reason := "Good match"
	if titleScore == 1.0 && artistScore >= 0.9 {
		finalScore = math.Min(1.0, finalScore+0.05)
		reason = "Exact title and artist match"
	}

	queryHasBrackets := len(queryTitleAnalysis.BracketContents) > 0
	candHasBrackets := len(candTitleAnalysis.BracketContents) > 0
	if candHasBrackets && !queryHasBrackets {
		finalScore = math.Max(0, finalScore-0.07)
		if reason == "Good match" {
			reason = "Version divergence penalty"
		}
	}

	return ScoreInfo{
		Score:      math.Min(1.0, math.Max(0, finalScore)),
		Reason:     reason,
		Components: Components{titleScore, artistScore, albumScore, durationScore},
		Weights:    weights,
		QueryDur:   queryDur, CandDur: candDur,
	}
}

// MatchScore is the ms-based convenience wrapper used by the cache tier.
func MatchScore(queryTitle, queryArtist, queryAlbum string, queryMs int,
	candTitle, candArtist, candAlbum string, candMs int, _, _ bool) float64 {
	info := SongSimilarity(candTitle, candArtist, candAlbum, float64(candMs)/1000,
		queryTitle, queryArtist, queryAlbum, float64(queryMs)/1000)
	return info.Score
}

// SongCandidate represents a candidate track from search results.
type SongCandidate struct {
	Title      string
	Artist     string
	Album      string
	DurationMs int
	ISRC       string
	PlatformID string
	Data       interface{}
}

// BestMatchResult encapsulates the winning candidate and its score information.
type BestMatchResult struct {
	Candidate SongCandidate
	ScoreInfo ScoreInfo
}

// FindBestSongMatch evaluates candidates against the query metadata, returning the best match if above 0.70 threshold.
func FindBestSongMatch(candidates []SongCandidate, queryTitle, queryArtist, queryAlbum string, queryDurationSec float64, songISRC, songPlatformID string) *BestMatchResult {
	if len(candidates) == 0 || queryTitle == "" {
		return nil
	}

	var valid []SongCandidate
	for _, c := range candidates {
		if c.Title != "" && c.Artist != "" {
			valid = append(valid, c)
		}
	}
	if len(valid) == 0 {
		return nil
	}

	type scored struct {
		cand      SongCandidate
		scoreInfo ScoreInfo
	}
	scoredList := make([]scored, 0, len(valid))
	for _, cand := range valid {
		var info ScoreInfo
		if songISRC != "" && cand.ISRC != "" && strings.EqualFold(songISRC, cand.ISRC) {
			info = ScoreInfo{
				Score:      1.0,
				Reason:     "Exact ISRC match",
				Components: Components{TitleScore: 1, ArtistScore: 1, AlbumScore: 1, DurationScore: 1},
				CandDur:    float64(cand.DurationMs) / 1000.0,
				QueryDur:   queryDurationSec,
			}
		} else if songPlatformID != "" && cand.PlatformID != "" && songPlatformID == cand.PlatformID {
			info = ScoreInfo{
				Score:      1.0,
				Reason:     "Exact Platform ID match",
				Components: Components{TitleScore: 1, ArtistScore: 1, AlbumScore: 1, DurationScore: 1},
				CandDur:    float64(cand.DurationMs) / 1000.0,
				QueryDur:   queryDurationSec,
			}
		} else {
			candDur := float64(cand.DurationMs) / 1000.0
			info = SongSimilarity(cand.Title, cand.Artist, cand.Album, candDur, queryTitle, queryArtist, queryAlbum, queryDurationSec)
		}
		scoredList = append(scoredList, scored{cand: cand, scoreInfo: info})
	}

	sort.Slice(scoredList, func(i, j int) bool {
		si := scoredList[i].scoreInfo
		sj := scoredList[j].scoreInfo
		if math.Abs(si.Score-sj.Score) > 0.001 {
			return si.Score > sj.Score
		}
		hasDurI := scoredList[i].cand.DurationMs > 0
		hasDurJ := scoredList[j].cand.DurationMs > 0
		if hasDurI != hasDurJ {
			return hasDurI
		}
		if queryDurationSec > 0 {
			return si.Components.DurationScore > sj.Components.DurationScore
		}
		return false
	})

	best := scoredList[0]
	const confidenceThreshold = 0.70
	if best.scoreInfo.Score < confidenceThreshold {
		return nil
	}

	return &BestMatchResult{
		Candidate: best.cand,
		ScoreInfo: best.scoreInfo,
	}
}
