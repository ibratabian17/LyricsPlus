package providers

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/similarity"
)

const (
	candidateToleranceMs = 2000
	gapApple             = 0.35
	gapQQ                = 0.20

	wordMaxQQMerge       = 3
	wordCrossWordPenalty = 0.20

	sylMaxQQMerge       = 12
	sylCrossWordPenalty = 9999.0

	seqPenaltyRate   = 1e-7
	intraWindowNudge = 0.05

	synthBailThreshold = 0.50
	offsetMinMs        = 1000
	offsetMaxMADMs     = 3000
	infCost            = 1e9
)

type LineSource interface {
	FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error)
}

// QapleService orchestrates the Qaple merge between QQ word sync and Apple/Musixmatch line sync.
type QapleService struct {
	qqSource LineSource
	apple    LineSource
	mxm      LineSource
}

func NewQapleService(qq, apple, mxm LineSource) *QapleService {
	return &QapleService{
		qqSource: qq,
		apple:    apple,
		mxm:      mxm,
	}
}

func (s *QapleService) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	if s.qqSource == nil || (s.apple == nil && s.mxm == nil) {
		return nil, nil
	}

	type fetchResult struct {
		resp   *domain.LyricsResponse
		source string
		err    error
	}

	qqCh := make(chan fetchResult, 1)
	lineCh := make(chan fetchResult, 1)

	go func() {
		resp, err := s.qqSource.FetchLyrics(ctx, q)
		qqCh <- fetchResult{resp: resp, source: "QQ", err: err}
	}()

	// Fetch Line sync (Apple, fallback to Musixmatch)
	go func() {
		if s.apple != nil {
			resp, err := s.apple.FetchLyrics(ctx, q)
			if err == nil && resp != nil && len(resp.Lyrics) > 0 {
				lineCh <- fetchResult{resp: resp, source: "Apple", err: nil}
				return
			}
		}
		if s.mxm != nil {
			resp, err := s.mxm.FetchLyrics(ctx, q)
			if err == nil && resp != nil && len(resp.Lyrics) > 0 {
				lineCh <- fetchResult{resp: resp, source: "Musixmatch", err: nil}
				return
			}
		}
		lineCh <- fetchResult{resp: nil, source: "", err: nil}
	}()

	qqRes := <-qqCh
	if qqRes.err != nil || qqRes.resp == nil || len(qqRes.resp.Lyrics) == 0 {
		return nil, nil
	}

	lineRes := <-lineCh
	if lineRes.resp == nil || len(lineRes.resp.Lyrics) == 0 {
		return nil, nil
	}

	merged := MergeAppleMetadataIntoWordSync(lineRes.resp, qqRes.resp)
	if merged == nil {
		return nil, nil
	}

	merged.Metadata.Source = fmt.Sprintf("Lyrics+ (via %s with QQ)", lineRes.source)
	merged.Cached = domain.CacheNone
	winner := "qaple"
	if merged.ProcessingTime == nil {
		merged.ProcessingTime = &domain.ProcessTiming{}
	}
	merged.ProcessingTime.WinnerSource = &winner
	if lineRes.resp != nil && lineRes.resp.ProcessingTime != nil && lineRes.resp.ProcessingTime.SelectedSongMetadata != nil {
		merged.ProcessingTime.SelectedSongMetadata = lineRes.resp.ProcessingTime.SelectedSongMetadata
	} else if qqRes.resp != nil && qqRes.resp.ProcessingTime != nil && qqRes.resp.ProcessingTime.SelectedSongMetadata != nil {
		merged.ProcessingTime.SelectedSongMetadata = qqRes.resp.ProcessingTime.SelectedSongMetadata
	}
	return merged, nil
}

// detectQQMode checks whether QQ lyrics are syllable or word level.
func detectQQMode(wordSyncData *domain.LyricsResponse) string {
	if wordSyncData == nil || len(wordSyncData.Lyrics) == 0 {
		return "word"
	}
	sampleSize := len(wordSyncData.Lyrics)
	if sampleSize > 30 {
		sampleSize = 30
	}

	totalEntries := 0
	shortEntries := 0
	hyphenEntries := 0

	for i := 0; i < sampleSize; i++ {
		for _, syl := range wordSyncData.Lyrics[i].Syllabus {
			t := strings.TrimSpace(syl.Text)
			if t == "" {
				continue
			}
			totalEntries++
			letters := 0
			for _, r := range t {
				if unicode.IsLetter(r) {
					letters++
				}
			}
			if letters < 4 {
				shortEntries++
			}
			if strings.HasSuffix(t, "-") {
				hyphenEntries++
			}
		}
	}

	if totalEntries == 0 {
		return "word"
	}
	if float64(shortEntries)/float64(totalEntries) > 0.55 {
		return "syllable"
	}
	if float64(hyphenEntries)/float64(totalEntries) > 0.40 {
		return "syllable"
	}
	return "word"
}

var nonWordPunct = regexp.MustCompile(`[^\w\s']`)
var leadingApostrophe = regexp.MustCompile(`(^|\s)'+`)
var multiSpace = regexp.MustCompile(`\s+`)

func normalizeForMatch(text string) string {
	if text == "" {
		return ""
	}
	s := strings.ToLower(text)
	s = strings.ReplaceAll(s, "*", "")
	s = nonWordPunct.ReplaceAllString(s, "")
	s = leadingApostrophe.ReplaceAllString(s, "$1")
	s = multiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func textSimilarity(n1, n2 string) float64 {
	if n1 == "" || n2 == "" {
		if n1 == "" && n2 == "" {
			return 1.0
		}
		return 0
	}
	if n1 == n2 {
		return 1.0
	}

	short := len(n1)
	if len(n2) < short {
		short = len(n2)
	}
	if short <= 2 {
		if strings.HasPrefix(n1, n2) || strings.HasPrefix(n2, n1) {
			return 0.7
		}
		return 0
	}

	if len(n1) >= 4 && strings.Contains(n2, n1) {
		return 0.8 + 0.2*(float64(len(n1))/float64(len(n2)))
	}
	if len(n2) >= 4 && strings.Contains(n1, n2) {
		return float64(len(n2)) / float64(len(n1))
	}

	w1 := strings.Fields(n1)
	w2 := strings.Fields(n2)
	var filtered1, filtered2 []string
	for _, w := range w1 {
		if len(w) > 1 {
			filtered1 = append(filtered1, w)
		}
	}
	set2 := make(map[string]bool)
	for _, w := range w2 {
		if len(w) > 1 {
			filtered2 = append(filtered2, w)
			set2[w] = true
		}
	}

	if len(filtered1) >= 2 && len(filtered2) >= 2 {
		overlap := 0
		for _, w := range filtered1 {
			if set2[w] {
				overlap++
			}
		}
		r := float64(overlap) / float64(len(filtered1))
		if r >= 0.5 {
			return 0.6 + r*0.35
		}
	}

	return similarity.SorensenDice(n1, n2)
}

func textSimilaritySyllable(appleNorm, combinedSylNorm string) float64 {
	if appleNorm == "" || combinedSylNorm == "" {
		return 0
	}
	if appleNorm == combinedSylNorm {
		return 1.0
	}
	if strings.HasPrefix(appleNorm, combinedSylNorm) {
		return 0.5 + 0.5*(float64(len(combinedSylNorm))/float64(len(appleNorm)))
	}
	if strings.HasPrefix(combinedSylNorm, appleNorm) {
		return float64(len(appleNorm)) / float64(len(combinedSylNorm))
	}
	return textSimilarity(appleNorm, combinedSylNorm)
}

func splitHyphenated(word string) []string {
	var parts []string
	var cur strings.Builder
	runes := []rune(word)
	for i := 0; i < len(runes); i++ {
		cur.WriteRune(runes[i])
		if runes[i] == '-' && i > 0 && i < len(runes)-1 &&
			(unicode.IsLetter(runes[i-1]) || unicode.IsDigit(runes[i-1])) &&
			(unicode.IsLetter(runes[i+1]) || unicode.IsDigit(runes[i+1])) {
			parts = append(parts, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	if len(parts) > 1 {
		return parts
	}
	return []string{word}
}

type appleToken struct {
	text      string
	norm      string
	wordFinal bool
}

func tokenizeAppleLine(text string) []appleToken {
	var tokens []appleToken
	for _, word := range strings.Fields(text) {
		parts := splitHyphenated(word)
		for i, p := range parts {
			tokens = append(tokens, appleToken{
				text:      p,
				norm:      normalizeForMatch(p),
				wordFinal: i == len(parts)-1,
			})
		}
	}
	return tokens
}

func estimateLineOffset(appleLines, wordLines []domain.Line) int {
	var deltas []int
	for _, al := range appleLines {
		if al.Time <= 0 || al.Text == "" {
			continue
		}
		aNorm := normalizeForMatch(al.Text)
		bestSim := 0.5
		var bestDelta *int
		for _, ql := range wordLines {
			sim := textSimilarity(aNorm, normalizeForMatch(ql.Text))
			if sim > bestSim {
				bestSim = sim
				d := ql.Time - al.Time
				bestDelta = &d
			}
		}
		if bestDelta != nil {
			deltas = append(deltas, *bestDelta)
		}
	}

	if len(deltas) == 0 {
		return 0
	}
	sort.Ints(deltas)
	median := deltas[len(deltas)/2]

	madSum := 0.0
	for _, d := range deltas {
		madSum += math.Abs(float64(d - median))
	}
	mad := madSum / float64(len(deltas))

	if mad > offsetMaxMADMs {
		return 0
	}
	if math.Abs(float64(median)) < offsetMinMs {
		return 0
	}
	return median
}

type qqPoolToken struct {
	text     string
	norm     string
	time     int
	duration int
	seqIdx   int
}

func flattenQQIntoWordPool(wordLines []domain.Line) []qqPoolToken {
	var pool []qqPoolToken
	seq := 0

	for _, line := range wordLines {
		for _, syl := range line.Syllabus {
			words := strings.Fields(syl.Text)
			if len(words) == 0 {
				continue
			}
			if len(words) == 1 {
				pool = append(pool, qqPoolToken{
					text:     words[0],
					norm:     normalizeForMatch(words[0]),
					time:     syl.Time,
					duration: syl.Duration,
					seqIdx:   seq,
				})
				seq++
			} else {
				totalLen := 0
				for _, w := range words {
					totalLen += len(w)
				}
				if totalLen == 0 {
					totalLen = len(words)
				}
				t := syl.Time
				for _, w := range words {
					dur := int(math.Round((float64(len(w)) / float64(totalLen)) * float64(syl.Duration)))
					pool = append(pool, qqPoolToken{
						text:     w,
						norm:     normalizeForMatch(w),
						time:     t,
						duration: dur,
						seqIdx:   seq,
					})
					seq++
					t += dur
				}
			}
		}
	}
	return pool
}

func collectCandidates(appleLines []domain.Line, qqPool []qqPoolToken, lineOffset int) map[int][]qqPoolToken {
	m := make(map[int][]qqPoolToken, len(appleLines))
	for a := range appleLines {
		m[a] = nil
	}

	for _, tok := range qqPool {
		for a, al := range appleLines {
			if al.Time == 0 && al.Duration == 0 {
				continue
			}
			centre := al.Time + lineOffset
			lo := centre - candidateToleranceMs
			hi := centre + al.Duration + candidateToleranceMs
			if tok.time >= lo && tok.time <= hi {
				m[a] = append(m[a], tok)
			}
		}
	}
	return m
}

type dpOp struct {
	opType string // "match", "skip_apple", "skip_qq"
	da     int
	dq     int
}

type matchItem struct {
	time       int
	duration   int
	parts      []partTiming
	mergeCount int
	absorbed   bool
}

type partTiming struct {
	time     int
	duration int
	norm     string
}

func alignTokensToQQ(appleTokens []appleToken, qqTokens []qqPoolToken, lineStart, lineEnd int, mode string) ([]*matchItem, []qqPoolToken) {
	A := len(appleTokens)
	Q := len(qqTokens)

	if A == 0 {
		return nil, qqTokens
	}
	if Q == 0 {
		return make([]*matchItem, A), nil
	}

	isSyllable := mode == "syllable"
	maxQQMerge := wordMaxQQMerge
	crossWordPen := wordCrossWordPenalty
	if isSyllable {
		maxQQMerge = sylMaxQQMerge
		crossWordPen = sylCrossWordPenalty
	}

	lineDur := math.Max(1, float64(lineEnd-lineStart))
	timePenaltyRate := 1.5 / lineDur

	timePenalty := func(t int) float64 {
		ft := float64(t)
		fls := float64(lineStart)
		fle := float64(lineEnd)
		if ft < fls {
			return (fls - ft) * timePenaltyRate
		}
		if ft > fle {
			return (ft - fle) * timePenaltyRate
		}
		return ((ft - fls) / lineDur) * intraWindowNudge
	}

	simFn := textSimilarity
	if isSyllable {
		simFn = textSimilaritySyllable
	}

	dp := make([][]float64, A+1)
	op := make([][]dpOp, A+1)
	for i := range dp {
		dp[i] = make([]float64, Q+1)
		for j := range dp[i] {
			dp[i][j] = infCost
		}
		op[i] = make([]dpOp, Q+1)
	}
	dp[0][0] = 0

	for a := 0; a <= A; a++ {
		for q := 0; q <= Q; q++ {
			cur := dp[a][q]
			if cur >= infCost {
				continue
			}

			if a < A {
				// 1 Apple : 1..maxQQMerge QQ tokens
				for dq := 1; dq <= maxQQMerge && q+dq <= Q; dq++ {
					var combined strings.Builder
					tPenalty := 0.0
					seqPenalty := 0.0
					for k := 0; k < dq; k++ {
						combined.WriteString(qqTokens[q+k].norm)
						tPenalty += timePenalty(qqTokens[q+k].time)
						seqPenalty += float64(qqTokens[q+k].seqIdx) * seqPenaltyRate
					}
					sim := simFn(appleTokens[a].norm, combined.String())
					c := cur + (1 - sim) + float64(dq-1)*gapQQ + (tPenalty / float64(dq)) + seqPenalty
					if c < dp[a+1][q+dq] {
						dp[a+1][q+dq] = c
						op[a+1][q+dq] = dpOp{opType: "match", da: 1, dq: dq}
					}
				}

				// N Apple tokens : 1 QQ token
				if q < Q {
					for da := 2; a+da <= A; da++ {
						var combined strings.Builder
						crossWordPenalty := 0.0
						for k := 0; k < da; k++ {
							combined.WriteString(appleTokens[a+k].norm)
							if k < da-1 && appleTokens[a+k].wordFinal {
								crossWordPenalty += crossWordPen
							}
						}
						if crossWordPenalty >= infCost {
							break
						}
						if float64(combined.Len()) > float64(len(qqTokens[q].norm))*1.5 {
							break
						}
						sim := textSimilarity(combined.String(), qqTokens[q].norm)
						if sim < 0.40 {
							break
						}
						seqPenalty := float64(qqTokens[q].seqIdx) * seqPenaltyRate
						c := cur + (1 - sim) + timePenalty(qqTokens[q].time) + crossWordPenalty + seqPenalty
						if c < dp[a+da][q+1] {
							dp[a+da][q+1] = c
							op[a+da][q+1] = dpOp{opType: "match", da: da, dq: 1}
						}
					}
				}

				// Skip Apple
				ca := cur + gapApple
				if ca < dp[a+1][q] {
					dp[a+1][q] = ca
					op[a+1][q] = dpOp{opType: "skip_apple", da: 1}
				}
			}

			// Skip QQ
			if q < Q {
				cq := cur + gapQQ
				if cq < dp[a][q+1] {
					dp[a][q+1] = cq
					op[a][q+1] = dpOp{opType: "skip_qq", dq: 1}
				}
			}
		}
	}

	matches := make([]*matchItem, A)
	var residuals []qqPoolToken
	a := A
	q := Q

	for a > 0 || q > 0 {
		o := op[a][q]
		if o.opType == "" {
			break
		}

		if o.opType == "match" {
			da := o.da
			dq := o.dq
			if da == 1 {
				if dq == 1 {
					matches[a-1] = &matchItem{
						time:     qqTokens[q-1].time,
						duration: qqTokens[q-1].duration,
					}
				} else {
					fused := qqTokens[q-dq : q]
					parts := make([]partTiming, len(fused))
					for k, f := range fused {
						parts[k] = partTiming{time: f.time, duration: f.duration, norm: f.norm}
					}
					matches[a-1] = &matchItem{parts: parts}
				}
				a--
				q -= dq
			} else {
				qqTok := qqTokens[q-1]
				matches[a-da] = &matchItem{
					time:       qqTok.time,
					duration:   qqTok.duration,
					mergeCount: da,
				}
				for k := 1; k < da; k++ {
					matches[a-da+k] = &matchItem{absorbed: true}
				}
				a -= da
				q--
			}
		} else if o.opType == "skip_apple" {
			a--
		} else {
			residuals = append(residuals, qqTokens[q-1])
			q--
		}
	}

	sort.Slice(residuals, func(i, j int) bool {
		return residuals[i].seqIdx < residuals[j].seqIdx
	})

	return matches, residuals
}

func anchorSyntheticsWithResiduals(runTokens []appleToken, residuals []qqPoolToken, leftEnd, rightStart int) []*matchItem {
	n := len(runTokens)
	result := make([]*matchItem, n)
	if len(residuals) == 0 {
		return result
	}

	var inGap []qqPoolToken
	for _, r := range residuals {
		if r.time >= leftEnd-80 && r.time <= rightStart+80 {
			inGap = append(inGap, r)
		}
	}
	if len(inGap) == 0 {
		return result
	}

	used := make(map[int]bool)
	minSeq := -1

	for k := 0; k < n; k++ {
		bestSim := 0.55
		bestIdx := -1
		for rIdx, r := range inGap {
			if used[rIdx] || r.seqIdx <= minSeq {
				continue
			}
			sim := textSimilarity(runTokens[k].norm, r.norm)
			if sim > bestSim {
				bestSim = sim
				bestIdx = rIdx
			}
		}
		if bestIdx >= 0 {
			result[k] = &matchItem{time: inGap[bestIdx].time, duration: inGap[bestIdx].duration}
			minSeq = inGap[bestIdx].seqIdx
			used[bestIdx] = true
		}
	}
	return result
}

var punctTrailRe = regexp.MustCompile(`^(.*?)([,\.\s!?»«"'()[\]{}]*)$`)

func splitWordByNorms(appleText string, qqNorms []string) []string {
	if len(qqNorms) <= 1 {
		return []string{appleText}
	}

	m := punctTrailRe.FindStringSubmatch(appleText)
	base := appleText
	trail := ""
	if len(m) >= 3 {
		base = m[1]
		trail = m[2]
	}
	baseRunes := []rune(base)
	var parts []string
	pos := 0

	for i := 0; i < len(qqNorms); i++ {
		if i == len(qqNorms)-1 || pos >= len(baseRunes) {
			parts = append(parts, string(baseRunes[pos:])+trail)
			break
		}
		normRunes := []rune(qqNorms[i])
		length := len(normRunes)
		if len(baseRunes)-pos < length {
			length = len(baseRunes) - pos
		}
		parts = append(parts, string(baseRunes[pos:pos+length]))
		pos += length
	}
	return parts
}

func buildMergedText(appleTokens []appleToken, startIdx, count int) string {
	var b strings.Builder
	b.WriteString(appleTokens[startIdx].text)
	for k := 1; k < count; k++ {
		sep := ""
		if appleTokens[startIdx+k-1].wordFinal {
			sep = " "
		}
		b.WriteString(sep + appleTokens[startIdx+k].text)
	}
	return b.String()
}

func buildSyllabus(appleTokens []appleToken, matches []*matchItem, residuals []qqPoolToken, lineStart, lineEnd int) []domain.Syllable {
	n := len(appleTokens)
	if n == 0 {
		return nil
	}

	var expandedTokens []appleToken
	var expandedTiming []*matchItem

	i := 0
	for i < n {
		m := matches[i]
		tok := appleTokens[i]

		if m != nil && m.absorbed {
			i++
			continue
		}

		if m != nil && m.mergeCount > 1 {
			mergedText := buildMergedText(appleTokens, i, m.mergeCount)
			expandedTokens = append(expandedTokens, appleToken{
				text:      mergedText,
				norm:      normalizeForMatch(mergedText),
				wordFinal: appleTokens[i+m.mergeCount-1].wordFinal,
			})
			expandedTiming = append(expandedTiming, &matchItem{time: m.time, duration: m.duration})
			i += m.mergeCount
			continue
		}

		if m != nil && len(m.parts) > 0 {
			norms := make([]string, len(m.parts))
			for k, p := range m.parts {
				norms[k] = p.norm
			}
			textParts := splitWordByNorms(tok.text, norms)
			for k := 0; k < len(m.parts); k++ {
				txt := ""
				if k < len(textParts) {
					txt = textParts[k]
				}
				expandedTokens = append(expandedTokens, appleToken{
					text:      txt,
					norm:      m.parts[k].norm,
					wordFinal: k == len(m.parts)-1,
				})
				expandedTiming = append(expandedTiming, &matchItem{time: m.parts[k].time, duration: m.parts[k].duration})
			}
		} else {
			expandedTokens = append(expandedTokens, tok)
			if m != nil {
				expandedTiming = append(expandedTiming, &matchItem{time: m.time, duration: m.duration})
			} else {
				expandedTiming = append(expandedTiming, nil)
			}
		}
		i++
	}

	N := len(expandedTokens)

	ei := 0
	for ei < N {
		if expandedTiming[ei] != nil {
			ei++
			continue
		}
		j := ei
		for j < N && expandedTiming[j] == nil {
			j++
		}
		leftEnd := lineStart
		if ei > 0 && expandedTiming[ei-1] != nil {
			leftEnd = expandedTiming[ei-1].time + expandedTiming[ei-1].duration
		}
		rightStart := lineEnd
		if j < N && expandedTiming[j] != nil {
			rightStart = expandedTiming[j].time
		}
		anchored := anchorSyntheticsWithResiduals(expandedTokens[ei:j], residuals, leftEnd, rightStart)
		for k := 0; k < len(anchored); k++ {
			if anchored[k] != nil {
				expandedTiming[ei+k] = anchored[k]
			}
		}
		ei = j
	}

	var result []domain.Syllable
	ei = 0
	for ei < N {
		isLineLast := ei == N-1
		trailingSpace := ""
		if !isLineLast && expandedTokens[ei].wordFinal && !strings.HasSuffix(expandedTokens[ei].text, " ") {
			trailingSpace = " "
		}

		if expandedTiming[ei] != nil {
			result = append(result, domain.Syllable{
				Text:     expandedTokens[ei].text + trailingSpace,
				Time:     expandedTiming[ei].time,
				Duration: expandedTiming[ei].duration,
			})
			ei++
		} else {
			j := ei
			for j < N && expandedTiming[j] == nil {
				j++
			}
			leftEnd := lineStart
			if ei > 0 && expandedTiming[ei-1] != nil {
				leftEnd = expandedTiming[ei-1].time + expandedTiming[ei-1].duration
			}
			rightStart := lineEnd
			if j < N && expandedTiming[j] != nil {
				rightStart = expandedTiming[j].time
			}
			totalTime := rightStart - leftEnd
			if totalTime < 0 {
				totalTime = 0
			}
			run := expandedTokens[ei:j]
			totalLen := 0
			for _, t := range run {
				totalLen += len(strings.TrimSuffix(t.text, "-"))
			}
			if totalLen == 0 {
				totalLen = len(run)
			}
			t := leftEnd
			for k := 0; k < len(run); k++ {
				gi := ei + k
				trailing := ""
				if gi < N-1 && run[k].wordFinal {
					trailing = " "
				}
				charLen := len(strings.TrimSuffix(run[k].text, "-"))
				dur := int(math.Round(math.Max(50, (float64(charLen)/float64(totalLen))*float64(totalTime))))
				result = append(result, domain.Syllable{
					Text:      run[k].text + trailing,
					Time:      t,
					Duration:  dur,
					Synthetic: true,
				})
				t += dur
			}
			ei = j
		}
	}

	// Enforce monotonicity
	for idx := 1; idx < len(result); idx++ {
		if result[idx].Time < result[idx-1].Time {
			result[idx].Time = result[idx-1].Time + result[idx-1].Duration
			result[idx].Synthetic = true
		}
	}

	return result
}

func transferBackground(appleLine domain.Line, outLine *domain.Line) {
	if len(appleLine.Syllabus) == 0 || len(outLine.Syllabus) == 0 {
		return
	}
	var bg []domain.Syllable
	for _, s := range appleLine.Syllabus {
		if s.IsBackground {
			bg = append(bg, s)
		}
	}
	if len(bg) == 0 {
		return
	}
	for i := range outLine.Syllabus {
		ws := &outLine.Syllabus[i]
		for _, b := range bg {
			overlap := math.Min(float64(ws.Time+ws.Duration), float64(b.Time+b.Duration)) -
				math.Max(float64(ws.Time), float64(b.Time))
			if overlap > 0 {
				ws.IsBackground = true
				break
			}
		}
	}
}

// RetimeAppleLinesFromQQ retimes Apple lines if they lack timestamps.
func RetimeAppleLinesFromQQ(appleLines, wordLines []domain.Line) []domain.Line {
	var timedQQ []domain.Line
	for _, l := range wordLines {
		if l.Time > 0 || l.Duration > 0 {
			timedQQ = append(timedQQ, l)
		}
	}
	if len(timedQQ) == 0 {
		return appleLines
	}

	qqNorms := make([]string, len(timedQQ))
	for i, l := range timedQQ {
		qqNorms[i] = normalizeForMatch(l.Text)
	}

	result := make([]domain.Line, len(appleLines))
	copy(result, appleLines)
	matched := make([]bool, len(result))
	qqCursor := 0

	for a := 0; a < len(result); a++ {
		aNorm := normalizeForMatch(result[a].Text)
		if aNorm == "" {
			continue
		}

		bestSim := 0.25
		bestIdx := -1
		searchEnd := len(timedQQ)
		if qqCursor+10 < searchEnd {
			searchEnd = qqCursor + 10
		}
		for q := qqCursor; q < searchEnd; q++ {
			sim := textSimilarity(aNorm, qqNorms[q])
			if sim > bestSim {
				bestSim = sim
				bestIdx = q
			}
		}

		if bestIdx >= 0 {
			startIdx := bestIdx
			for startIdx > qqCursor {
				prevNorm := qqNorms[startIdx-1]
				if len(prevNorm) >= 4 && strings.Contains(aNorm, prevNorm) {
					startIdx--
				} else {
					break
				}
			}

			endIdx := bestIdx
			for endIdx+1 < len(timedQQ) {
				nextNorm := qqNorms[endIdx+1]
				if len(nextNorm) >= 4 && strings.Contains(aNorm, nextNorm) {
					endIdx++
				} else {
					break
				}
			}

			startTime := timedQQ[startIdx].Time
			lastLine := timedQQ[endIdx]
			endTime := lastLine.Time + lastLine.Duration

			dur := endTime - startTime
			if timedQQ[bestIdx].Duration > dur {
				dur = timedQQ[bestIdx].Duration
			}

			result[a].Time = startTime
			result[a].Duration = dur
			matched[a] = true
			qqCursor = endIdx + 1
		}
	}

	lastQQ := timedQQ[len(timedQQ)-1]
	qqEnd := lastQQ.Time + lastQQ.Duration

	a := 0
	for a < len(result) {
		if matched[a] {
			a++
			continue
		}
		runStart := a
		for a < len(result) && !matched[a] {
			a++
		}
		runEnd := a
		runLen := runEnd - runStart

		prevA := runStart - 1
		for prevA >= 0 && !matched[prevA] {
			prevA--
		}
		prevEnd := 0
		if prevA >= 0 {
			prevEnd = result[prevA].Time + result[prevA].Duration
		}
		nextStart := qqEnd
		if runEnd < len(result) {
			nextStart = result[runEnd].Time
		}
		totalGap := math.Max(0, float64(nextStart-prevEnd))
		slot := totalGap / float64(runLen)

		for k := 0; k < runLen; k++ {
			result[runStart+k].Time = int(math.Round(float64(prevEnd) + float64(k)*slot))
			result[runStart+k].Duration = int(math.Round(slot))
		}
	}

	return result
}

// MergeAppleMetadataIntoWordSync merges Apple Line sync with QQ Word sync
func MergeAppleMetadataIntoWordSync(appleData, wordSyncData *domain.LyricsResponse) *domain.LyricsResponse {
	if appleData == nil || wordSyncData == nil {
		if wordSyncData != nil {
			return wordSyncData
		}
		return appleData
	}

	typeLower := strings.ToLower(string(appleData.Type))
	if typeLower == "word" || typeLower == "syllable" {
		return nil
	}

	if len(appleData.Lyrics) == 0 || len(wordSyncData.Lyrics) == 0 {
		return wordSyncData
	}

	appleLines := appleData.Lyrics
	wordLines := wordSyncData.Lyrics

	hasValidTiming := false
	for _, l := range appleLines {
		if l.Time > 0 || l.Duration > 0 {
			hasValidTiming = true
			break
		}
	}
	if !hasValidTiming {
		retimed := RetimeAppleLinesFromQQ(appleLines, wordLines)
		cp := *appleData
		cp.Lyrics = retimed
		return MergeAppleMetadataIntoWordSync(&cp, wordSyncData)
	}

	mode := detectQQMode(wordSyncData)
	qqPool := flattenQQIntoWordPool(wordLines)
	lineOffset := estimateLineOffset(appleLines, wordLines)
	candidatesByLine := collectCandidates(appleLines, qqPool, lineOffset)

	var mergedLyrics []domain.Line
	totalWords := 0
	totalSynth := 0

	for a := 0; a < len(appleLines); a++ {
		appleLine := appleLines[a]
		candidates := candidatesByLine[a]
		tokens := tokenizeAppleLine(appleLine.Text)
		lineStart := appleLine.Time
		lineEnd := lineStart + appleLine.Duration

		if lineStart == 0 && lineEnd == 0 {
			syllabus := buildSyllabus(tokens, make([]*matchItem, len(tokens)), nil, lineStart, lineEnd)
			mergedLyrics = append(mergedLyrics, domain.Line{
				Time:            lineStart,
				Duration:        appleLine.Duration,
				Text:            appleLine.Text,
				Syllabus:        syllabus,
				Element:         appleLine.Element,
				Translation:     appleLine.Translation,
				Transliteration: appleLine.Transliteration,
			})
			totalWords += len(tokens)
			totalSynth += len(tokens)
			continue
		}

		matches, residuals := alignTokensToQQ(tokens, candidates, lineStart, lineEnd, mode)
		syllabus := buildSyllabus(tokens, matches, residuals, lineStart, lineEnd)

		synth := 0
		for _, m := range matches {
			if m == nil {
				synth++
			}
		}
		totalSynth += synth
		totalWords += len(tokens)

		line := domain.Line{
			Time:            lineStart,
			Duration:        appleLine.Duration,
			Text:            appleLine.Text,
			Syllabus:        syllabus,
			Element:         appleLine.Element,
			Translation:     appleLine.Translation,
			Transliteration: appleLine.Transliteration,
		}
		transferBackground(appleLine, &line)
		mergedLyrics = append(mergedLyrics, line)
	}

	synthRatio := 0.0
	if totalWords > 0 {
		synthRatio = float64(totalSynth) / float64(totalWords)
	}
	if synthRatio > synthBailThreshold {
		return nil
	}

	appleMeta := appleData.Metadata
	wordMeta := wordSyncData.Metadata

	writers := appleMeta.SongWriters
	if len(writers) == 0 {
		writers = wordMeta.SongWriters
	}
	agents := appleMeta.Agents
	if len(agents) == 0 {
		agents = wordMeta.Agents
	}

	lang := appleMeta.Language
	if lang == "" {
		lang = wordMeta.Language
	}
	totalDur := appleMeta.TotalDuration
	if totalDur == "" {
		totalDur = wordMeta.TotalDuration
	}

	mergedMetadata := domain.LyricsMetadata{
		Source:         fmt.Sprintf("QQ/Apple (%s)", mode),
		Title:          wordMeta.Title,
		Artist:         wordMeta.Artist,
		Album:          wordMeta.Album,
		SongWriters:    writers,
		LeadingSilence: wordMeta.LeadingSilence,
		Agents:         agents,
		SongParts:      appleMeta.SongParts,
		Language:       lang,
		TotalDuration:  totalDur,
	}
	if mergedMetadata.LeadingSilence == "" {
		mergedMetadata.LeadingSilence = "0.000"
	}
	if mergedMetadata.Title == "" {
		mergedMetadata.Title = appleMeta.Title
	}
	if mergedMetadata.Artist == "" {
		mergedMetadata.Artist = appleMeta.Artist
	}
	if mergedMetadata.Album == "" {
		mergedMetadata.Album = appleMeta.Album
	}

	out := *wordSyncData
	out.Type = domain.SyncTypeWord
	out.Metadata = mergedMetadata
	out.Lyrics = mergedLyrics

	return &out
}
