package parsers

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"lyricsplus/backend/internal/domain"
)

// ttml namespaces used by Apple Music.
const (
	nsTT           = "http://www.w3.org/ns/ttml"
	nsITunesInt    = "http://music.apple.com/lyric-ttml-internal"
	nsITunesExt    = "http://itunes.apple.com/lyric-ttml-extensions"
	nsTTM          = "http://www.w3.org/ns/ttml#metadata"
	nsXML          = "http://www.w3.org/XML/1998/namespace"
	nsLyricsPlus   = "http://lyricsplus.prjktla.my.id/lyric-ttml-internal"
	kpoeTTML       = "1.7-1-ConvertTTMLtoJSON-DOMParser"
	ttmlLeadingSil = "0.000"
)

// txmlNode is a minimal DOM node exposing document-traversal semantics
// (getElementsByTagName, textContent, sibling text nodes, attribute lookup by namespace).
type txmlNode struct {
	local    string // local name
	space    string // namespace URI
	attrs    []xml.Attr
	parent   *txmlNode
	children []*txmlNode
	isText   bool
	data     string
}

// buildTTMLDOM parses an XML document into a lightweight DOM tree.
func buildTTMLDOM(data []byte) (*txmlNode, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	root := &txmlNode{}
	stack := []*txmlNode{root}
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &txmlNode{local: t.Name.Local, space: t.Name.Space, attrs: t.Attr}
			parent := stack[len(stack)-1]
			n.parent = parent
			parent.children = append(parent.children, n)
			stack = append(stack, n)
		case xml.CharData:
			if len(t) > 0 {
				n := &txmlNode{isText: true, data: string(t)}
				parent := stack[len(stack)-1]
				n.parent = parent
				parent.children = append(parent.children, n)
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
	if len(root.children) == 0 {
		return nil, fmt.Errorf("ttml: empty document")
	}
	for _, c := range root.children {
		if !c.isText {
			return c, nil
		}
	}
	return nil, fmt.Errorf("ttml: no root element")
}

// descendants returns all descendant elements (in document order) whose local
func descendants(n *txmlNode, local string) []*txmlNode {
	var out []*txmlNode
	var walk func(*txmlNode)
	walk = func(c *txmlNode) {
		for _, ch := range c.children {
			if ch.isText {
				continue
			}
			if ch.local == local {
				out = append(out, ch)
			}
			walk(ch)
		}
	}
	walk(n)
	return out
}

// getAttrValue does a namespace-scoped lookup first, then a plain local-name
// fallback. Returns "" when absent.
func getAttrValue(n *txmlNode, nsURI, local string) (string, bool) {
	if n == nil {
		return "", false
	}
	if nsURI != "" {
		for _, a := range n.attrs {
			if a.Name.Space == nsURI && a.Name.Local == local {
				return a.Value, true
			}
		}
	}
	for _, a := range n.attrs {
		if a.Name.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

func getAttr(n *txmlNode, nsURI, local string) string {
	v, _ := getAttrValue(n, nsURI, local)
	return v
}

// textContent concatenates the text of every descendant text node.
func textContent(n *txmlNode) string {
	if n == nil {
		return ""
	}
	if n.isText {
		return n.data
	}
	var sb strings.Builder
	var walk func(*txmlNode)
	walk = func(c *txmlNode) {
		if c.isText {
			sb.WriteString(c.data)
			return
		}
		for _, ch := range c.children {
			walk(ch)
		}
	}
	walk(n)
	return sb.String()
}

// directText returns the concatenation of a node's immediate text children.
func directText(n *txmlNode) string {
	if n == nil {
		return ""
	}
	var sb strings.Builder
	for _, ch := range n.children {
		if ch.isText {
			sb.WriteString(ch.data)
		}
	}
	return sb.String()
}

// tailText collects following text-node siblings.
func tailText(n *txmlNode) string {
	if n == nil || n.parent == nil {
		return ""
	}
	idx := -1
	for i, ch := range n.parent.children {
		if ch == n {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ""
	}
	var sb strings.Builder
	for i := idx + 1; i < len(n.parent.children); i++ {
		if n.parent.children[i].isText {
			sb.WriteString(n.parent.children[i].data)
		} else {
			break
		}
	}
	return sb.String()
}

// insideBackgroundWrapper walks ancestors (up to, excluding, paragraph) and
// returns true if any has ttm:role="x-bg".
func insideBackgroundWrapper(node, paragraph *txmlNode) bool {
	cur := node.parent
	for cur != nil && cur != paragraph {
		if getAttr(cur, nsTTM, "role") == "x-bg" {
			return true
		}
		cur = cur.parent
	}
	return false
}

// parseTTMLTime parses "hh:mm:ss.mmm", "mm:ss.mmm" or "ss.mmm" into ms.
func parseTTMLTime(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var hh, mm int
	ssm := s
	if parts := strings.Split(s, ":"); len(parts) == 3 {
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return 0, false
		}
		hh, mm = h, m
		ssm = parts[2]
	} else if parts := strings.Split(s, ":"); len(parts) == 2 {
		m, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, false
		}
		mm = m
		ssm = parts[1]
	}
	secParts := strings.Split(ssm, ".")
	sec, err := strconv.Atoi(secParts[0])
	if err != nil {
		return 0, false
	}
	milli := 0
	if len(secParts) == 2 {
		ms := secParts[1]
		if len(ms) > 3 {
			ms = ms[:3]
		}
		for len(ms) < 3 {
			ms += "0"
		}
		milli, err = strconv.Atoi(ms)
		if err != nil {
			return 0, false
		}
	}
	return (hh*3600+mm*60+sec)*1000 + milli, true
}

// ttmlTimeToMs parses "hh:mm:ss.mmm", "mm:ss.mmm" or plain seconds.
func ttmlTimeToMs(timeStr string) int {
	if timeStr == "" {
		return 0
	}
	parts := strings.Split(timeStr, ":")
	var totalMs float64
	switch len(parts) {
	case 3:
		h, _ := strconv.ParseFloat(parts[0], 64)
		m, _ := strconv.ParseFloat(parts[1], 64)
		s, _ := strconv.ParseFloat(parts[2], 64)
		totalMs = (h*3600 + m*60 + s) * 1000
	case 2:
		m, _ := strconv.ParseFloat(parts[0], 64)
		s, _ := strconv.ParseFloat(parts[1], 64)
		totalMs = (m*60 + s) * 1000
	default:
		totalMs, _ = strconv.ParseFloat(parts[0], 64)
		totalMs *= 1000
	}
	if math.IsNaN(totalMs) {
		return 0
	}
	return int(math.Round(totalMs))
}

// TTMLToJSON converts an Apple Music TTML XML document into V2 JSON.
func TTMLToJSON(xmlData []byte) (*domain.LyricsResponse, error) {
	doc, err := buildTTMLDOM(xmlData)
	if err != nil {
		return nil, err
	}

	// The iTunes namespace switches between internal and external depending on
	// what the root declares. encoding/xml resolves prefixes, so detect by
	// scanning for any attribute carrying the external URI.
	itunesNS := nsITunesInt
	var scanNS func(*txmlNode)
	scanNS = func(n *txmlNode) {
		if n.isText {
			return
		}
		for _, a := range n.attrs {
			if a.Name.Space == nsITunesExt {
				itunesNS = nsITunesExt
				return
			}
		}
		for _, ch := range n.children {
			scanNS(ch)
		}
	}
	scanNS(doc)

	timingMode := getAttr(doc, itunesNS, "timing")
	if timingMode == "" {
		timingMode = string(domain.SyncTypeWord)
	}

	metadata := domain.LyricsMetadata{
		Source:      "Apple Music",
		SongWriters: []string{},
		Agents:      map[string]domain.Agent{},
		SongParts:   []domain.SongPart{},
		Language:    getAttr(doc, nsXML, "lang"),
	}

	bodyEls := descendants(doc, "body")
	if len(bodyEls) > 0 {
		metadata.TotalDuration = getAttr(bodyEls[0], "", "dur")
	}

	headEls := descendants(doc, "head")
	var headEl *txmlNode
	if len(headEls) > 0 {
		headEl = headEls[0]
	}
	var itunesMetaEl *txmlNode
	if headEl != nil {
		im := descendants(headEl, "iTunesMetadata")
		if len(im) > 0 {
			itunesMetaEl = im[0]
		}
	}

	// Agents.
	if headEl != nil {
		for _, a := range descendants(headEl, "agent") {
			agentID := getAttr(a, nsXML, "id")
			if agentID == "" {
				continue
			}
			typ := getAttr(a, "", "type")
			if typ == "" {
				typ = "person"
			}
			name := ""
			if names := descendants(a, "name"); len(names) > 0 {
				name = strings.TrimSpace(textContent(names[0]))
			}
			metadata.Agents[agentID] = domain.Agent{
				Type:  typ,
				Name:  name,
				Alias: strings.Replace(agentID, "voice", "v", 1),
			}
		}
	}

	// Title & songwriters. iTunesMetadata is preferred when present; the
	// head-level <metadata> is used as the fallback container so the
	// generator's own output (title in <metadata>) round-trips.
	containers := []*txmlNode{}
	if itunesMetaEl != nil {
		containers = append(containers, itunesMetaEl)
	}
	if headEl != nil {
		if m := descendants(headEl, "metadata"); len(m) > 0 {
			containers = append(containers, m[0])
		}
	}
	for _, c := range containers {
		if metadata.Title == "" {
			if t := descendants(c, "title"); len(t) > 0 {
				metadata.Title = strings.TrimSpace(textContent(t[0]))
			}
		}
		if len(metadata.SongWriters) == 0 {
			if sws := descendants(c, "songwriters"); len(sws) > 0 {
				for _, sw := range descendants(sws[0], "songwriter") {
					if n := strings.TrimSpace(textContent(sw)); n != "" {
						metadata.SongWriters = append(metadata.SongWriters, n)
					}
				}
			}
		}
	}

	if itunesMetaEl != nil {
		if ls, ok := getAttrValue(itunesMetaEl, "", "leadingSilence"); ok {
			metadata.LeadingSilence = ls
		}

		for _, audioEl := range descendants(itunesMetaEl, "audio") {
			audioObj := domain.AudioMetadata{}
			has := false
			if lo, ok := getAttrValue(audioEl, "", "lyricOffset"); ok {
				audioObj.LyricOffset = lo
				has = true
			}
			if role, ok := getAttrValue(audioEl, "", "role"); ok {
				audioObj.Role = role
				has = true
			}
			if has {
				metadata.Audio = append(metadata.Audio, audioObj)
			}
		}

		if curator := descendants(itunesMetaEl, "curator"); len(curator) > 0 {
			if c := strings.TrimSpace(textContent(curator[0])); c != "" {
				metadata.Curator = c
			}
		}
		if kpoe := descendants(itunesMetaEl, "kpoeTools"); len(kpoe) > 0 {
			_ = kpoe
		}
	}

	// Translations / transliterations keyed by <text for="KEY">.
	translationMap := map[string]*domain.Translation{}
	transliterationMap := map[string]*domain.Transliteration{}
	if itunesMetaEl != nil {
		if translationsNode := descendants(itunesMetaEl, "translations"); len(translationsNode) > 0 {
			for _, transNode := range descendants(translationsNode[0], "translation") {
				lang := getAttr(transNode, nsXML, "lang")
				for _, textNode := range descendants(transNode, "text") {
					lineID := getAttr(textNode, "", "for")
					if lineID == "" {
						continue
					}
					translationMap[lineID] = &domain.Translation{
						Lang: lang,
						Text: strings.TrimSpace(textContent(textNode)),
					}
				}
			}
		}

		if translitsNode := descendants(itunesMetaEl, "transliterations"); len(translitsNode) > 0 {
			for _, transNode := range descendants(translitsNode[0], "transliteration") {
				lang := getAttr(transNode, nsXML, "lang")
				for _, textNode := range descendants(transNode, "text") {
					lineID := getAttr(textNode, "", "for")
					if lineID == "" {
						continue
					}
					spans := []*txmlNode{}
					for _, s := range descendants(textNode, "span") {
						if getAttr(s, "", "begin") != "" {
							spans = append(spans, s)
						}
					}
					if len(spans) > 0 {
						syllabus := []domain.Syllable{}
						var fullText strings.Builder
						processed := map[*txmlNode]bool{}
						for _, span := range spans {
							if processed[span] {
								continue
							}
							processed[span] = true
							spanText := directText(span)
							tail := tailText(span)
							if tail != "" {
								spanText += tail
							}
							if strings.TrimSpace(spanText) == "" {
								continue
							}
							begin := getAttr(span, "", "begin")
							end := getAttr(span, "", "end")
							syl := domain.Syllable{
								Time:     ttmlTimeToMs(begin),
								Duration: ttmlTimeToMs(end) - ttmlTimeToMs(begin),
								Text:     spanText,
							}
							syllabus = append(syllabus, syl)
							fullText.WriteString(spanText)
						}
						transliterationMap[lineID] = &domain.Transliteration{
							Lang:     lang,
							Text:     strings.TrimSpace(fullText.String()),
							Syllabus: syllabus,
						}
					} else {
						transliterationMap[lineID] = &domain.Transliteration{
							Lang: lang,
							Text: strings.TrimSpace(textContent(textNode)),
						}
					}
				}
			}
		}
	}

	var lyrics []domain.Line
	divs := descendants(doc, "div")

	for i, div := range divs {
		songPart := getAttr(div, itunesNS, "song-part")
		if songPart == "" {
			songPart = getAttr(div, itunesNS, "songPart")
		}
		ps := descendants(div, "p")

		divBegin := getAttr(div, "", "begin")
		divEnd := getAttr(div, "", "end")
		if (divBegin == "" || divEnd == "") && len(ps) > 0 {
			if divBegin == "" {
				divBegin = getAttr(ps[0], "", "begin")
			}
			if divEnd == "" {
				divEnd = getAttr(ps[len(ps)-1], "", "end")
			}
		}

		partTime := ttmlTimeToMs(divBegin)
		partDur := ttmlTimeToMs(divEnd) - ttmlTimeToMs(divBegin)
		if partDur < 0 {
			partDur = 0
		}
		divIndex := i
		metadata.SongParts = append(metadata.SongParts, domain.SongPart{
			Name:     songPart,
			Time:     &partTime,
			Duration: &partDur,
			DivIndex: &divIndex,
		})

		for _, p := range ps {
			key := getAttr(p, itunesNS, "key")
			singerID := getAttr(p, nsTTM, "agent")
			singer := strings.Replace(singerID, "voice", "v", 1)

			pBegin := getAttr(p, "", "begin")
			pEnd := getAttr(p, "", "end")

			idx := i
			currentLine := domain.Line{
				Element: domain.LineElement{Key: key, Singer: singer, SongPartIndex: &idx},
			}
			if pBegin != "" && pEnd != "" {
				currentLine.Time = ttmlTimeToMs(pBegin)
				currentLine.Duration = ttmlTimeToMs(pEnd) - ttmlTimeToMs(pBegin)
			}

			if timingMode == string(domain.SyncTypeWord) {
				allSpans := []*txmlNode{}
				for _, s := range descendants(p, "span") {
					if getAttr(s, "", "begin") != "" {
						allSpans = append(allSpans, s)
					}
				}

				if len(allSpans) > 0 {
					processed := map[*txmlNode]bool{}
					for _, sp := range allSpans {
						if processed[sp] {
							continue
						}
						isBg := insideBackgroundWrapper(sp, p)
						if isBg {
							for _, nested := range descendants(sp, "span") {
								processed[nested] = true
							}
						}
						processed[sp] = true

						begin := getAttr(sp, "", "begin")
						if begin == "" {
							begin = "0"
						}
						end := getAttr(sp, "", "end")
						if end == "" {
							end = "0"
						}

						spanText := directText(sp)
						tail := tailText(sp)
						if tail != "" {
							spanText += tail
						}
						if strings.TrimSpace(spanText) == "" && (tail == "" || !strings.Contains(tail, " ")) {
							continue
						}

						syl := domain.Syllable{
							Time:     ttmlTimeToMs(begin),
							Duration: ttmlTimeToMs(end) - ttmlTimeToMs(begin),
							Text:     spanText,
						}
						if isBg {
							syl.IsBackground = true
						}
						currentLine.Syllabus = append(currentLine.Syllabus, syl)
						currentLine.Text += spanText
					}
				} else {
					currentLine.Text = strings.TrimSpace(textContent(p))
				}
			} else {
				currentLine.Text = strings.TrimSpace(textContent(p))
				if timingMode == string(domain.SyncTypeNone) || (pBegin == "" && pEnd == "") {
					currentLine.Time = 0
					currentLine.Duration = 0
				}
			}

			if currentLine.Text == "" && len(currentLine.Syllabus) == 0 {
				continue
			}
			if key != "" {
				if tr, ok := translationMap[key]; ok {
					currentLine.Translation = tr
				}
				if tl, ok := transliterationMap[key]; ok {
					currentLine.Transliteration = tl
				}
			}
			lyrics = append(lyrics, currentLine)
		}
	}

	return &domain.LyricsResponse{
		Type:      domain.SyncType(timingMode),
		KpoeTools: kpoeTTML,
		Metadata:  metadata,
		Lyrics:    lyrics,
	}, nil
}

// JSONToTTML serializes V2 JSON back into Apple Music compatible TTML XML.
func JSONToTTML(resp *domain.LyricsResponse) ([]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("ttml: nil response")
	}
	var b strings.Builder

	formatTime := func(ms int) string {
		if ms < 0 {
			ms = 0
		}
		r := int(math.Round(float64(ms)))
		m := r / 60000
		s := (r % 60000) / 1000
		msPart := r % 1000
		return fmt.Sprintf("%02d:%02d.%03d", m, s, msPart)
	}

	escapeHTML := func(text string) string {
		if text == "" {
			return text
		}
		r := strings.NewReplacer(
			"&", "&amp;",
			"<", "&lt;",
			">", "&gt;",
			`"`, "&quot;",
			"'", "&#x27;",
		)
		return r.Replace(text)
	}

	extractTextAndSpace := func(fullText string) (pre, text, post string) {
		if fullText == "" {
			return "", "", ""
		}
		start := 0
		for start < len(fullText) {
			r, size := utf8.DecodeRuneInString(fullText[start:])
			if !unicode.IsSpace(r) {
				break
			}
			start += size
		}
		end := len(fullText)
		for end > start {
			r, size := utf8.DecodeLastRuneInString(fullText[:end])
			if !unicode.IsSpace(r) {
				break
			}
			end -= size
		}
		return fullText[:start], fullText[start:end], fullText[end:]
	}

	metadata := resp.Metadata
	songPartsArray := metadata.SongParts
	isNewFormat := false
	for _, l := range resp.Lyrics {
		if l.Element.SongPartIndex != nil {
			isNewFormat = true
			break
		}
	}

	agents := map[string]domain.Agent{}
	for k, v := range metadata.Agents {
		agents[k] = v
	}
	usedSingers := map[string]bool{}
	for _, l := range resp.Lyrics {
		if l.Element.Singer != "" {
			usedSingers[l.Element.Singer] = true
		}
	}
	for alias := range usedSingers {
		existing := ""
		for id, ag := range agents {
			if ag.Alias == alias || id == alias {
				existing = id
				break
			}
		}
		if existing == "" {
			agents[alias] = domain.Agent{Type: "person", Name: "", Alias: alias}
		}
	}

	timingMode := "Word"
	if resp.Type != "" {
		timingMode = string(resp.Type)
	}
	docLang := metadata.Language
	if docLang == "" {
		docLang = "en"
	}

	findAgentID := func(alias string) string {
		if alias == "" {
			return ""
		}
		for id, ag := range agents {
			if ag.Alias == alias || id == alias {
				return id
			}
		}
		return alias
	}

	capitalize := func(s string) string {
		if s == "" {
			return s
		}
		r, size := utf8.DecodeRuneInString(s)
		return strings.ToUpper(string(r)) + s[size:]
	}

	findSongPartEntry := func(songPartIndex *int) *domain.SongPart {
		if songPartIndex == nil {
			return nil
		}
		for i := range songPartsArray {
			sp := &songPartsArray[i]
			if sp.DivIndex != nil && *sp.DivIndex == *songPartIndex {
				return &songPartsArray[i]
			}
		}
		if *songPartIndex >= 0 && *songPartIndex < len(songPartsArray) {
			return &songPartsArray[*songPartIndex]
		}
		return nil
	}

	resolveSongPart := func(el domain.LineElement) string {
		if el.SongPart != "" {
			return capitalize(el.SongPart)
		}
		if sp := findSongPartEntry(el.SongPartIndex); sp != nil && sp.Name != "" {
			return capitalize(sp.Name)
		}
		return ""
	}

	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" xmlns:ttm="http://www.w3.org/ns/ttml#metadata" xmlns:lyricsplus="http://lyricsplus.prjktla.my.id/lyric-ttml-internal" itunes:timing="` + escapeHTML(timingMode) + `" xml:lang="` + escapeHTML(docLang) + `">`)
	b.WriteString(`<head><metadata>`)
	if metadata.Title != "" {
		b.WriteString(`<ttm:title>` + escapeHTML(metadata.Title) + `</ttm:title>`)
	}

	agentIDs := make([]string, 0, len(agents))
	for id := range agents {
		agentIDs = append(agentIDs, id)
	}
	sort.Strings(agentIDs)
	for _, id := range agentIDs {
		ag := agents[id]
		typ := ag.Type
		if typ == "" {
			typ = "person"
		}
		if ag.Name != "" {
			b.WriteString(`<ttm:agent type="` + escapeHTML(typ) + `" xml:id="` + escapeHTML(id) + `"><ttm:name>` + escapeHTML(ag.Name) + `</ttm:name></ttm:agent>`)
		} else {
			b.WriteString(`<ttm:agent type="` + escapeHTML(typ) + `" xml:id="` + escapeHTML(id) + `"/>`)
		}
	}

	leadingSilence := metadata.LeadingSilence
	if leadingSilence == "" {
		leadingSilence = ttmlLeadingSil
	}
	b.WriteString(`<iTunesMetadata xmlns="http://music.apple.com/lyric-ttml-internal" leadingSilence="` + escapeHTML(leadingSilence) + `">`)

	translationsByLang := map[string][]struct{ key, text string }{}
	transliterationsByLang := map[string][]struct {
		key string
		tr  domain.Transliteration
	}{}
	for _, line := range resp.Lyrics {
		key := line.Element.Key
		if key == "" {
			continue
		}
		if line.Translation != nil && line.Translation.Text != "" {
			tlLang := line.Translation.Lang
			if tlLang == "" {
				tlLang = docLang
			}
			translationsByLang[tlLang] = append(translationsByLang[tlLang], struct{ key, text string }{key, line.Translation.Text})
		}
		if line.Transliteration != nil {
			tLang := line.Transliteration.Lang
			transliterationsByLang[tLang] = append(transliterationsByLang[tLang], struct {
				key string
				tr  domain.Transliteration
			}{key, *line.Transliteration})
		}
	}

	transLangs := make([]string, 0, len(translationsByLang))
	for l := range translationsByLang {
		transLangs = append(transLangs, l)
	}
	sort.Strings(transLangs)
	if len(transLangs) > 0 {
		b.WriteString(`<translations>`)
		for _, tLang := range transLangs {
			b.WriteString(`<translation type="subtitle" xml:lang="` + escapeHTML(tLang) + `">`)
			for _, e := range translationsByLang[tLang] {
				b.WriteString(`<text for="` + escapeHTML(e.key) + `">` + escapeHTML(e.text) + `</text>`)
			}
			b.WriteString(`</translation>`)
		}
		b.WriteString(`</translations>`)
	}

	if len(metadata.SongWriters) > 0 {
		b.WriteString(`<songwriters>`)
		for _, sw := range metadata.SongWriters {
			b.WriteString(`<songwriter>` + escapeHTML(sw) + `</songwriter>`)
		}
		b.WriteString(`</songwriters>`)
	}

	for _, audioEntry := range metadata.Audio {
		if audioEntry.LyricOffset == "" && audioEntry.Role == "" {
			continue
		}
		b.WriteString(`<audio`)
		if audioEntry.LyricOffset != "" {
			b.WriteString(` lyricOffset="` + escapeHTML(audioEntry.LyricOffset) + `"`)
		}
		if audioEntry.Role != "" {
			b.WriteString(` role="` + escapeHTML(audioEntry.Role) + `"`)
		}
		b.WriteString(`/>`)
	}

	translitLangs := make([]string, 0, len(transliterationsByLang))
	for l := range transliterationsByLang {
		translitLangs = append(translitLangs, l)
	}
	sort.Strings(translitLangs)
	if len(translitLangs) > 0 {
		b.WriteString(`<transliterations>`)
		for _, tLang := range translitLangs {
			b.WriteString(`<transliteration xml:lang="` + escapeHTML(tLang) + `">`)
			for _, e := range transliterationsByLang[tLang] {
				b.WriteString(`<text for="` + escapeHTML(e.key) + `">`)
				if len(e.tr.Syllabus) > 0 {
					for _, syl := range e.tr.Syllabus {
						pre, text, post := extractTextAndSpace(syl.Text)
						b.WriteString(pre)
						b.WriteString(`<span begin="` + formatTime(syl.Time) + `" end="` + formatTime(syl.Time+syl.Duration) + `">` + escapeHTML(text) + `</span>`)
						b.WriteString(post)
					}
				} else {
					b.WriteString(escapeHTML(e.tr.Text))
				}
				b.WriteString(`</text>`)
			}
			b.WriteString(`</transliteration>`)
		}
		b.WriteString(`</transliterations>`)
	}

	if metadata.Curator != "" {
		b.WriteString(`<lyricsplus:curator>` + escapeHTML(metadata.Curator) + `</lyricsplus:curator>`)
	}
	if resp.KpoeTools != "" {
		b.WriteString(`<lyricsplus:kpoeTools>` + escapeHTML(resp.KpoeTools) + `</lyricsplus:kpoeTools>`)
	}
	b.WriteString(`</iTunesMetadata></metadata></head>`)

	totalDur := metadata.TotalDuration
	if totalDur == "" && len(resp.Lyrics) > 0 {
		last := resp.Lyrics[len(resp.Lyrics)-1]
		totalDur = formatTime(last.Time + last.Duration)
	}
	if totalDur == "" {
		totalDur = "00:00.000"
	}
	b.WriteString(`<body dur="` + escapeHTML(totalDur) + `">`)

	if len(resp.Lyrics) > 0 {
		var currentLines []domain.Line
		currentSongPart := ""
		currentSongPartIndex := (*int)(nil)
		lineSet := false

		flushDiv := func() {
			if len(currentLines) == 0 {
				return
			}
			partEntry := findSongPartEntry(currentSongPartIndex)
			lastLine := currentLines[len(currentLines)-1]
			divStart := currentLines[0].Time
			if partEntry != nil && partEntry.Time != nil {
				divStart = *partEntry.Time
			}
			divEnd := lastLine.Time + lastLine.Duration
			if partEntry != nil && partEntry.Time != nil && partEntry.Duration != nil {
				divEnd = *partEntry.Time + *partEntry.Duration
			}

			b.WriteString(`<div begin="` + formatTime(divStart) + `" end="` + formatTime(divEnd) + `"`)
			if currentSongPart != "" {
				b.WriteString(` itunes:songPart="` + escapeHTML(currentSongPart) + `"`)
			}
			b.WriteString(`>`)

			for _, line := range currentLines {
				agentID := findAgentID(line.Element.Singer)
				key := line.Element.Key
				b.WriteString(`<p begin="` + formatTime(line.Time) + `" end="` + formatTime(line.Time+line.Duration) + `"`)
				if key != "" {
					b.WriteString(` itunes:key="` + escapeHTML(key) + `"`)
				}
				if agentID != "" {
					b.WriteString(` ttm:agent="` + escapeHTML(agentID) + `"`)
				}
				b.WriteString(`>`)

				if timingMode == "Word" && len(line.Syllabus) > 0 {
					var bgBuffer []domain.Syllable
					flushBg := func() {
						if len(bgBuffer) == 0 {
							return
						}
						b.WriteString(`<span ttm:role="x-bg">`)
						for _, s := range bgBuffer {
							pre, text, post := extractTextAndSpace(s.Text)
							b.WriteString(pre)
							b.WriteString(`<span begin="` + formatTime(s.Time) + `" end="` + formatTime(s.Time+s.Duration) + `">` + escapeHTML(text) + `</span>`)
							b.WriteString(post)
						}
						b.WriteString(`</span>`)
						bgBuffer = nil
					}
					for _, syl := range line.Syllabus {
						if syl.IsBackground {
							bgBuffer = append(bgBuffer, syl)
						} else {
							flushBg()
							pre, text, post := extractTextAndSpace(syl.Text)
							b.WriteString(pre)
							b.WriteString(`<span begin="` + formatTime(syl.Time) + `" end="` + formatTime(syl.Time+syl.Duration) + `">` + escapeHTML(text) + `</span>`)
							b.WriteString(post)
						}
					}
					flushBg()
				} else {
					b.WriteString(escapeHTML(line.Text))
				}
				b.WriteString(`</p>`)
			}
			b.WriteString(`</div>`)
		}

		for _, line := range resp.Lyrics {
			songPart := resolveSongPart(line.Element)
			songPartIndex := line.Element.SongPartIndex

			if !lineSet {
				lineSet = true
				currentSongPart = songPart
				currentSongPartIndex = songPartIndex
			}

			shouldSplit := false
			if isNewFormat {
				shouldSplit = !idxEqual(songPartIndex, currentSongPartIndex)
			} else {
				shouldSplit = songPart != currentSongPart
			}

			if shouldSplit && len(currentLines) > 0 {
				flushDiv()
				currentLines = nil
				currentSongPart = songPart
				currentSongPartIndex = songPartIndex
			}
			currentLines = append(currentLines, line)
		}
		flushDiv()
	}

	b.WriteString(`</body></tt>`)
	return []byte(b.String()), nil
}

func idxEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
