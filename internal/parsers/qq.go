package parsers

import (
	"regexp"
	"strconv"
	"strings"

	"lyricsplus/backend/internal/domain"
)

// ExactMetadata carries the exact-match metadata for a QQ query.
type ExactMetadata struct {
	Title      string
	Artist     string
	Album      string
	DurationMs int
	ISRC       string
	PlatformID string
}

// ParseQQQRC converts a QRC string (XML containing a LyricContent="..." attr,
// or a raw QRC/LRC text) into a V2 JSON payload. Returns nil when no QRC is
// detected or no lyric lines parse.
func ParseQQQRC(qrcString string, exactMetadata ExactMetadata) *domain.LyricsResponse {
	if qrcString == "" || (!strings.Contains(qrcString, "<QrcInfos>") && !strings.Contains(qrcString, "LyricContent=")) {
		return nil
	}

	lyricContent := qrcString
	if m := lyricContentAttrRe.FindStringSubmatch(qrcString); m != nil {
		lyricContent = decodeEntities(m[1])
	}

	lines, agents := parseQRC(lyricContent, exactMetadata)
	if len(lines) == 0 {
		return nil
	}

	return &domain.LyricsResponse{
		Type:      domain.SyncTypeWord,
		KpoeTools: "1.0-LPlusBcknd",
		Metadata: domain.LyricsMetadata{
			Source:         "QQ Music",
			SongWriters:    []string{},
			LeadingSilence: "0.000",
			Agents:         agents,
		},
		Lyrics: lines,
	}
}

var lyricContentAttrRe = regexp.MustCompile(`LyricContent="([\s\S]*?)"\s*(?:/?>|[a-zA-Z]+=)`)

func decodeEntities(s string) string {
	repl := strings.NewReplacer("&quot;", "\"", "&lt;", "<", "&gt;", ">", "&amp;", "&")
	return repl.Replace(s)
}

// parseLineTime parses a [startMs,durationMs] line-level header.
func parseLineTime(src string) (startTime, duration int, rest string, ok bool) {
	if len(src) == 0 || src[0] != '[' {
		return 0, 0, "", false
	}
	closeIdx := strings.IndexByte(src, ']')
	if closeIdx < 0 {
		return 0, 0, "", false
	}
	comma := strings.IndexByte(src[1:], ',')
	if comma < 0 || comma+1 > closeIdx {
		return 0, 0, "", false
	}
	startTime, err1 := strconv.Atoi(src[1 : 1+comma])
	duration, err2 := strconv.Atoi(src[2+comma : closeIdx])
	if err1 != nil || err2 != nil {
		return 0, 0, "", false
	}
	return startTime, duration, src[closeIdx+1:], true
}

// parseWordTime parses a (startMs,durationMs) word-level token.
func parseWordTime(src string) (startTime, duration, tokenLen int, ok bool) {
	if len(src) == 0 || src[0] != '(' {
		return 0, 0, 0, false
	}
	closeIdx := strings.IndexByte(src, ')')
	if closeIdx < 0 {
		return 0, 0, 0, false
	}
	comma := strings.IndexByte(src[1:], ',')
	if comma < 0 || comma+1 > closeIdx {
		return 0, 0, 0, false
	}
	startTime, err1 := strconv.Atoi(src[1 : 1+comma])
	duration, err2 := strconv.Atoi(src[2+comma : closeIdx])
	if err1 != nil || err2 != nil {
		return 0, 0, 0, false
	}
	return startTime, duration, closeIdx + 1, true
}

// parseWords extracts timed syllables, taking the text chunk BEFORE each
// (startMs,durationMs) token as the syllable text and dropping any tail.
func parseWords(src string) []domain.Syllable {
	var words []domain.Syllable
	pos := 0
	for pos < len(src) {
		found := false
		for i := pos; i < len(src); i++ {
			if src[i] != '(' {
				continue
			}
			if st, du, tokenLen, ok := parseWordTime(src[i:]); ok {
				words = append(words, domain.Syllable{
					Text:     src[pos:i],
					Time:     st,
					Duration: du,
				})
				pos = i + tokenLen
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	return words
}

type parsedLine struct {
	Time     int
	Duration int
	Text     string
	Syllabus []domain.Syllable
	Element  domain.LineElement
}

// parseLine returns a parsed line or nil for metadata tags / bad headers.
// Element.Key is intentionally left empty here.
func parseLine(src string) *parsedLine {
	if tagRe.MatchString(src) {
		return nil
	}
	st, du, rest, ok := parseLineTime(src)
	if !ok {
		return nil
	}
	syllabus := parseWords(rest)
	text := ""
	for _, s := range syllabus {
		text += s.Text
	}
	return &parsedLine{Time: st, Duration: du, Text: text, Syllabus: syllabus, Element: domain.LineElement{}}
}

var tagRe = regexp.MustCompile(`^\[[a-zA-Z]+:`)

func parseQRC(qrcContent string, exactMetadata ExactMetadata) ([]domain.Line, map[string]domain.Agent) {
	var lines []domain.Line
	ctx := &agentsCtx{agents: map[string]domain.Agent{}, aliases: map[string]string{}, nextVoiceID: 1}

	for _, raw := range strings.Split(qrcContent, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		parsed := parseLine(trimmed)
		if parsed == nil {
			continue
		}
		if extractSinger(parsed, ctx, exactMetadata, len(lines) < 5) {
			lines = append(lines, domain.Line{
				Time:     parsed.Time,
				Duration: parsed.Duration,
				Text:     parsed.Text,
				Syllabus: parsed.Syllabus,
				Element:  parsed.Element,
			})
		}
	}
	return lines, ctx.agents
}

type agentsCtx struct {
	agents        map[string]domain.Agent
	aliases       map[string]string
	nextVoiceID   int
	currentSinger string
}

// isMetadataPrefix reports whether a name is a production credit label.
func isMetadataPrefix(name string) bool {
	n := strings.ToLower(stripAllSpace(name))
	known := []string{
		"词", "作词", "曲", "作曲", "编曲", "和声", "混音", "吉他", "制作人", "演唱", "原唱", "翻唱", "后期",
		"和音", "录音", "策划", "伴奏", "美工", "海报", "旁白",
		"writtenby", "producedby", "composedby", "arrangedby", "mixing", "mastering",
		"vocal", "vocals", "guitar", "bass", "drums", "producer", "lyricist", "composer", "arranger",
	}
	for _, k := range known {
		if n == k {
			return true
		}
	}
	return strings.HasSuffix(n, "词") || strings.HasSuffix(n, "曲") || strings.HasSuffix(n, "声") || strings.HasSuffix(n, "音")
}

func stripAllSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
}

var (
	fullMatchRe  = regexp.MustCompile(`^([^:：]+)\s*[:：]\s*$`)
	splitMatchRe = regexp.MustCompile(`^([^:：]+)\s*[:：]\s*(.+)?$`)
)

// extractSinger handles "Name: lyrics..." prefixes, assigns agent metadata and
// strips the prefix. Returns false to drop the line entirely.
func extractSinger(parsed *parsedLine, ctx *agentsCtx, exactMetadata ExactMetadata, isFirstFewLines bool) bool {
	if len(parsed.Syllabus) == 0 {
		if ctx.currentSinger != "" {
			parsed.Element = domain.LineElement{Singer: ctx.currentSinger}
		}
		return true
	}

	// Drop lines near the start that just echo the track title or artist name.
	if isFirstFewLines && (exactMetadata.Title != "" || exactMetadata.Artist != "") {
		text := normalizedEcho(parsed.Text)
		title := normalizedEcho(exactMetadata.Title)
		artist := normalizedEcho(exactMetadata.Artist)
		if title != "" && strings.Contains(text, title) && (artist == "" || strings.Contains(text, artist)) &&
			len(text) < len(title)+len(artist)+15 {
			return false
		}
		if artist != "" && text == artist {
			return false
		}
	}

	if ctx.currentSinger != "" {
		parsed.Element = domain.LineElement{Singer: ctx.currentSinger}
	}

	accText := ""
	syllablesToRemove := 0
	for _, syl := range parsed.Syllabus {
		accText += syl.Text
		syllablesToRemove++
		if strings.ContainsAny(accText, ":：") {
			break
		}
		if len(accText) > 25 {
			return true
		}
	}

	// Case A: the entire accumulated text is "Name:" with nothing after the colon.
	if m := fullMatchRe.FindStringSubmatch(accText); m != nil {
		singerName := strings.TrimSpace(m[1])
		if isMetadataPrefix(singerName) {
			return false
		}
		if len([]rune(singerName)) > 15 {
			return true
		}
		parsed.Syllabus = parsed.Syllabus[syllablesToRemove:]
		parsed.Text = trimPrefixRunes(parsed.Text, len([]rune(accText)))
		assignAgent(singerName, parsed, ctx)
		ctx.currentSinger = ctx.aliases[singerName]
		updateLineTiming(parsed)
		return true
	}

	// Case B: "Name: lyrics..." packed into the first syllable.
	if len(parsed.Syllabus) > 0 {
		if m := splitMatchRe.FindStringSubmatch(parsed.Syllabus[0].Text); m != nil && len([]rune(m[1])) < 20 {
			singerName := strings.TrimSpace(m[1])
			if isMetadataPrefix(singerName) {
				return false
			}
			remainder := ""
			if len(m) > 2 {
				remainder = m[2]
			}
			if len(remainder) > 0 {
				parsed.Syllabus[0].Text = remainder
			} else {
				parsed.Syllabus = parsed.Syllabus[1:]
			}
			assignAgent(singerName, parsed, ctx)
			ctx.currentSinger = ctx.aliases[singerName]
			parsed.Text = prefixColonRe.ReplaceAllString(parsed.Text, "")
			updateLineTiming(parsed)
		}
	}

	return true
}

var prefixColonRe = regexp.MustCompile(`^[^:：]+[:：]\s*`)

func normalizedEcho(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r == ' ' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func assignAgent(singerName string, parsed *parsedLine, ctx *agentsCtx) {
	alias, ok := ctx.aliases[singerName]
	if !ok {
		upper := strings.ToUpper(singerName)
		agentType := "person"
		if upper == "合" || upper == "ALL" || upper == "合唱" {
			agentType = "group"
		}
		alias = "v" + strconv.Itoa(ctx.nextVoiceID)
		ctx.nextVoiceID++
		ctx.aliases[singerName] = alias
		ctx.agents["voice"+strconv.Itoa(ctx.nextVoiceID-1)] = domain.Agent{
			Type:  agentType,
			Name:  singerName,
			Alias: alias,
		}
	}
	parsed.Element = domain.LineElement{Singer: alias}
}

func trimPrefixRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if n >= len(r) {
		return ""
	}
	return string(r[n:])
}

func updateLineTiming(parsed *parsedLine) {
	if len(parsed.Syllabus) == 0 {
		return
	}
	last := parsed.Syllabus[len(parsed.Syllabus)-1]
	parsed.Time = parsed.Syllabus[0].Time
	parsed.Duration = (last.Time + last.Duration) - parsed.Time
}
