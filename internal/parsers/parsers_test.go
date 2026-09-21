package parsers

import (
	"strconv"
	"strings"
	"testing"

	"lyricsplus/backend/internal/domain"
)

const sampleTTML = `<?xml version="1.0" encoding="utf-8"?>
<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" xmlns:ttm="http://www.w3.org/ns/ttml#metadata" xmlns:lyricsplus="http://lyricsplus.prjktla.my.id/lyric-ttml-internal" itunes:timing="Word" xml:lang="en">
  <head>
    <metadata>
      <ttm:title>Shake It Off</ttm:title>
      <ttm:agent type="person" xml:id="v1"><ttm:name>Taylor Swift</ttm:name></ttm:agent>
      <iTunesMetadata xmlns="http://music.apple.com/lyric-ttml-internal" leadingSilence="0.000">
        <songwriters><songwriter>Taylor Swift</songwriter></songwriters>
        <translations><translation type="subtitle" xml:lang="es"><text for="L1">¡Sacúdetelo!</text></translation></translations>
        <audio lyricOffset="0" role="primary"/>
        <lyricsplus:curator>Tester</lyricsplus:curator>
        <lyricsplus:kpoeTools>1.7-1-ConvertTTMLtoJSON-DOMParser</lyricsplus:kpoeTools>
      </iTunesMetadata>
    </metadata>
  </head>
  <body dur="00:01:03.500">
    <div itunes:song-part="Verse 1">
      <p begin="00:01:00.000" end="00:01:03.500" itunes:key="L1" ttm:agent="v1"><span begin="00:01:00.100" end="00:01:00.600">I </span><span begin="00:01:00.600" end="00:01:01.100">stay </span><span begin="00:01:01.100" end="00:01:01.600">out </span><span begin="00:01:01.600" end="00:01:02.000">too </span><span begin="00:01:02.000" end="00:01:02.800">late!</span></p>
    </div>
  </body>
</tt>`

func TestTTMLToJSONRoundtrip(t *testing.T) {
	resp, err := TTMLToJSON([]byte(sampleTTML))
	if err != nil {
		t.Fatalf("TTMLToJSON: %v", err)
	}
	if resp.Type != domain.SyncTypeWord {
		t.Errorf("type = %q", resp.Type)
	}
	if resp.KpoeTools == "" {
		t.Errorf("KpoeTools empty")
	}
	if resp.Metadata.Source != "Apple Music" {
		t.Errorf("source = %q", resp.Metadata.Source)
	}
	if resp.Metadata.Title != "Shake It Off" {
		t.Errorf("title = %q", resp.Metadata.Title)
	}
	if resp.Metadata.Language != "en" {
		t.Errorf("language = %q", resp.Metadata.Language)
	}
	if len(resp.Metadata.SongWriters) != 1 || resp.Metadata.SongWriters[0] != "Taylor Swift" {
		t.Errorf("songWriters = %q", resp.Metadata.SongWriters)
	}
	if len(resp.Metadata.Audio) != 1 || resp.Metadata.Audio[0].LyricOffset != "0" || resp.Metadata.Audio[0].Role != "primary" {
		t.Errorf("audio = %+v", resp.Metadata.Audio)
	}
	ag, ok := resp.Metadata.Agents["v1"]
	if !ok || ag.Name != "Taylor Swift" || ag.Alias != "v1" {
		t.Errorf("agents = %+v", resp.Metadata.Agents)
	}
	if len(resp.Lyrics) != 1 {
		t.Fatalf("lines = %d", len(resp.Lyrics))
	}
	line := resp.Lyrics[0]
	if line.Time != 60000 {
		t.Errorf("line time = %d", line.Time)
	}
	if line.Duration != 3500 {
		t.Errorf("line duration = %d", line.Duration)
	}
	if line.Text != "I stay out too late!" {
		t.Errorf("line text = %q", line.Text)
	}
	if len(line.Syllabus) != 5 {
		t.Fatalf("syllables = %d", len(line.Syllabus))
	}
	if line.Syllabus[0].Text != "I " {
		t.Errorf("syllable[0] text = %q", line.Syllabus[0].Text)
	}
	if line.Syllabus[0].Time != 60100 || line.Syllabus[0].Duration != 500 {
		t.Errorf("syllable[0] timing = %d/%d", line.Syllabus[0].Time, line.Syllabus[0].Duration)
	}
	if line.Syllabus[4].Text != "late!" {
		t.Errorf("syllable tail text lost: %q", line.Syllabus[4].Text)
	}
	if line.Syllabus[4].Time != 62000 || line.Syllabus[4].Duration != 800 {
		t.Errorf("syllable[4] timing = %d/%d", line.Syllabus[4].Time, line.Syllabus[4].Duration)
	}
	if line.Element.Key != "L1" || line.Element.Singer != "v1" {
		t.Errorf("element = %+v", line.Element)
	}
	if line.Element.SongPartIndex == nil || resp.Metadata.SongParts[*line.Element.SongPartIndex].Name != "Verse 1" {
		t.Errorf("song part mapping failed")
	}
	if tr := resp.Metadata.SongParts[0]; tr.Name != "Verse 1" || tr.Time == nil || *tr.Time != 60000 || tr.DivIndex == nil || *tr.DivIndex != 0 {
		t.Errorf("song part = %+v", resp.Metadata.SongParts[0])
	}
	if line.Translation == nil || line.Translation.Lang != "es" || line.Translation.Text != "¡Sacúdetelo!" {
		t.Errorf("translation not attached: %+v", line.Translation)
	}

	// Serialize back to TTML and re-parse.
	xmlOut, err := JSONToTTML(resp)
	if err != nil {
		t.Fatalf("JSONToTTML: %v", err)
	}
	re, err := TTMLToJSON(xmlOut)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if re.Metadata.Title != resp.Metadata.Title {
		t.Errorf("title mismatch after roundtrip")
	}
	if len(re.Lyrics) != len(resp.Lyrics) {
		t.Errorf("line count mismatch after roundtrip")
	}
	if len(re.Lyrics[0].Syllabus) != len(resp.Lyrics[0].Syllabus) {
		t.Errorf("syllable count mismatch after roundtrip")
	}
	if !strings.Contains(string(xmlOut), "<?xml") {
		t.Error("xml missing declaration")
	}
}

func TestV1V2Roundtrip(t *testing.T) {
	v2 := &domain.LyricsResponse{
		Type: domain.SyncTypeWord,
		Metadata: domain.LyricsMetadata{
			Source:    "spotify",
			Title:     "Song",
			Artist:    "Artist",
			SongParts: []domain.SongPart{{Name: "Chorus"}},
		},
		Lyrics: []domain.Line{
			{
				Time: 1000, Duration: 2000, Text: "hello world",
				Syllabus: []domain.Syllable{
					{Time: 1000, Duration: 500, Text: "hello "},
					{Time: 1500, Duration: 1500, Text: "world"},
				},
				Element: domain.LineElement{Key: "L1", Singer: "v1"},
			},
		},
	}
	v1 := V2ToV1(v2)
	if len(v1.Lyrics) != 2 {
		t.Fatalf("segments = %d", len(v1.Lyrics))
	}
	if v1.Lyrics[0].IsLineEnding != 0 || v1.Lyrics[1].IsLineEnding != 1 {
		t.Errorf("isLineEnding flags wrong: %d %d", v1.Lyrics[0].IsLineEnding, v1.Lyrics[1].IsLineEnding)
	}
	if v1.Type != "syllable" {
		t.Errorf("v1 type = %q", v1.Type)
	}

	back := V1ToV2(v1)
	if len(back.Lyrics) != 1 {
		t.Fatalf("rebuilt lines = %d", len(back.Lyrics))
	}
	if back.Lyrics[0].Text != "hello world" {
		t.Errorf("rebuilt text = %q", back.Lyrics[0].Text)
	}
	if back.Type != domain.SyncTypeWord {
		t.Errorf("rebuilt type = %q", back.Type)
	}
}

func TestV1ToV2LineMode(t *testing.T) {
	v1 := &domain.V1Response{
		Type: string(domain.SyncTypeLine),
		Lyrics: []domain.V1Segment{
			{Time: 1000, Duration: 500, Text: "a", IsLineEnding: 1},
			{Time: 1600, Duration: 500, Text: "b", IsLineEnding: 1},
		},
	}
	back := V1ToV2(v1)
	if len(back.Lyrics) != 2 {
		t.Fatalf("lines = %d", len(back.Lyrics))
	}
	if back.Type != domain.SyncTypeLine || len(back.Lyrics[0].Syllabus) != 0 {
		t.Errorf("type/syllabus = %q/%d", back.Type, len(back.Lyrics[0].Syllabus))
	}
	if back.KpoeTools != "2.0-LPlusBcknd," {
		t.Errorf("kpoe = %q", back.KpoeTools)
	}
}

func TestV1ToV2SongPartsDerived(t *testing.T) {
	v1 := &domain.V1Response{
		Type: "syllable",
		Lyrics: []domain.V1Segment{
			{Time: 1000, Duration: 200, Text: "a ", Element: domain.V1SegmentElement{Key: "L1", SongPart: "Verse", Singer: "v1"}},
			{Time: 1200, Duration: 200, Text: "b", Element: domain.V1SegmentElement{Key: "L1", SongPart: "Verse", Singer: "v1"}, IsLineEnding: 1},
			{Time: 3000, Duration: 300, Text: "c", Element: domain.V1SegmentElement{Key: "L2", SongPart: "Chorus", Singer: "v1"}, IsLineEnding: 1},
		},
	}
	back := V1ToV2(v1)
	if len(back.Lyrics) != 2 {
		t.Fatalf("lines = %d", len(back.Lyrics))
	}
	if back.Lyrics[0].Time != 1000 || back.Lyrics[0].Duration != 400 || back.Lyrics[0].Text != "a b" {
		t.Errorf("line0 = %d/%d %q", back.Lyrics[0].Time, back.Lyrics[0].Duration, back.Lyrics[0].Text)
	}
	if len(back.Metadata.SongParts) != 2 {
		t.Fatalf("songParts = %d", len(back.Metadata.SongParts))
	}
	if back.Metadata.SongParts[0].Name != "Verse" || *back.Metadata.SongParts[0].Time != 1000 || *back.Metadata.SongParts[0].Duration != 400 {
		t.Errorf("part0 = %+v", back.Metadata.SongParts[0])
	}
	if back.Metadata.SongParts[1].Name != "Chorus" || *back.Metadata.SongParts[1].Duration != 300 {
		t.Errorf("part1 = %+v", back.Metadata.SongParts[1])
	}
	if back.Lyrics[0].Element.SongPartIndex == nil || *back.Lyrics[0].Element.SongPartIndex != 0 {
		t.Errorf("songPartIndex = %+v", back.Lyrics[0].Element.SongPartIndex)
	}
}

func TestV2ToV1ResolvesSongPart(t *testing.T) {
	idx0, idx1 := 0, 1
	v2 := &domain.LyricsResponse{
		Type: domain.SyncTypeLine,
		Metadata: domain.LyricsMetadata{
			SongParts: []domain.SongPart{{Name: "Verse"}, {Name: "Chorus"}},
		},
		Lyrics: []domain.Line{
			{Time: 100, Duration: 100, Text: "a", Element: domain.LineElement{Key: "L1", SongPartIndex: &idx0}},
			{Time: 200, Duration: 100, Text: "b", Element: domain.LineElement{Key: "L2", SongPart: "Bridge"}},
		},
	}
	v1 := V2ToV1(v2)
	if len(v1.Lyrics) != 2 {
		t.Fatalf("segs = %d", len(v1.Lyrics))
	}
	if v1.Lyrics[0].Element.SongPart != "Verse" || v1.Lyrics[1].Element.SongPart != "Bridge" {
		t.Errorf("songParts = %q/%q", v1.Lyrics[0].Element.SongPart, v1.Lyrics[1].Element.SongPart)
	}
	if v1.Lyrics[1].Element.IsBackground {
		t.Errorf("unexpected isBackground")
	}
	_ = idx1
}

func TestNormalizeV2FlatV1ToNestedV2(t *testing.T) {
	isEnd0 := 0
	isEnd1 := 1
	flatResp := &domain.LyricsResponse{
		Type: "Word",
		Lyrics: []domain.Line{
			{Time: 23876, Duration: 432, Text: "Ter", IsLineEnding: &isEnd0, Element: domain.LineElement{Key: "L1", SongPart: "INTRO", Singer: "v1"}},
			{Time: 24308, Duration: 408, Text: "u", IsLineEnding: &isEnd0, Element: domain.LineElement{Key: "L1", SongPart: "INTRO", Singer: "v1"}},
			{Time: 24716, Duration: 1428, Text: "kir ", IsLineEnding: &isEnd1, Element: domain.LineElement{Key: "L1", SongPart: "INTRO", Singer: "v1"}},
			{Time: 31406, Duration: 474, Text: "Tak ", IsLineEnding: &isEnd0, Element: domain.LineElement{Key: "L3", SongPart: "INTRO", Singer: "v1"}},
			{Time: 31880, Duration: 468, Text: "a", IsLineEnding: &isEnd1, Element: domain.LineElement{Key: "L3", SongPart: "INTRO", Singer: "v1"}},
		},
	}

	nested := NormalizeV2(flatResp)
	if nested == nil {
		t.Fatalf("NormalizeV2 returned nil")
	}
	if nested.Type != domain.SyncTypeWord {
		t.Errorf("expected type Word, got %q", nested.Type)
	}
	if len(nested.Lyrics) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(nested.Lyrics))
	}
	if nested.Lyrics[0].Text != "Terukir" {
		t.Errorf("line 0 text = %q, want 'Terukir'", nested.Lyrics[0].Text)
	}
	if len(nested.Lyrics[0].Syllabus) != 3 {
		t.Fatalf("line 0 syllabus count = %d, want 3", len(nested.Lyrics[0].Syllabus))
	}
	if nested.Lyrics[0].IsLineEnding != nil {
		t.Errorf("expected nil IsLineEnding on nested V2 line")
	}
	if nested.Lyrics[0].Element.SongPartIndex == nil || *nested.Lyrics[0].Element.SongPartIndex != 0 {
		t.Errorf("expected SongPartIndex 0, got %+v", nested.Lyrics[0].Element.SongPartIndex)
	}
	if len(nested.Metadata.SongParts) != 1 || nested.Metadata.SongParts[0].Name != "INTRO" {
		t.Errorf("metadata songParts = %+v", nested.Metadata.SongParts)
	}

	// Test V2ToV1 roundtrip
	v1 := V2ToV1(nested)
	if v1.Type != "syllable" {
		t.Errorf("v1.Type = %q, want 'syllable'", v1.Type)
	}
	if len(v1.Lyrics) != 5 {
		t.Fatalf("expected 5 flat segments, got %d", len(v1.Lyrics))
	}
	if v1.Lyrics[2].IsLineEnding != 1 || v1.Lyrics[4].IsLineEnding != 1 {
		t.Errorf("v1 line ending flags wrong: seg2=%d, seg4=%d", v1.Lyrics[2].IsLineEnding, v1.Lyrics[4].IsLineEnding)
	}
	if v1.Lyrics[0].IsLineEnding != 0 {
		t.Errorf("seg0 should not be line ending: %d", v1.Lyrics[0].IsLineEnding)
	}
}

func TestNormalizeV2BackgroundVocalsInSameLine(t *testing.T) {
	isEnd0 := 0
	isEnd1 := 1
	flatResp := &domain.LyricsResponse{
		Type: "Word",
		Lyrics: []domain.Line{
			{Time: 1000, Duration: 500, Text: "Hello ", IsLineEnding: &isEnd0, Element: domain.LineElement{Key: "L1", Singer: "v1"}},
			{Time: 1500, Duration: 500, Text: "(hello)", IsLineEnding: &isEnd1, Element: domain.LineElement{Key: "L2", Singer: "v1", IsBackground: true}},
		},
	}

	nested := NormalizeV2(flatResp)
	if len(nested.Lyrics) != 1 {
		t.Fatalf("expected 1 line for background vocal without preceding isLineEnding, got %d", len(nested.Lyrics))
	}
	if nested.Lyrics[0].Text != "Hello (hello)" {
		t.Errorf("text = %q, want 'Hello (hello)'", nested.Lyrics[0].Text)
	}
	if len(nested.Lyrics[0].Syllabus) != 2 {
		t.Fatalf("syllabus count = %d, want 2", len(nested.Lyrics[0].Syllabus))
	}
	if !nested.Lyrics[0].Syllabus[1].IsBackground {
		t.Errorf("expected syllabus[1] to have IsBackground = true")
	}

	v1 := V2ToV1(nested)
	if len(v1.Lyrics) != 2 {
		t.Fatalf("expected 2 flat segments, got %d", len(v1.Lyrics))
	}
	if v1.Lyrics[0].IsLineEnding != 0 || v1.Lyrics[1].IsLineEnding != 1 {
		t.Errorf("line endings: %d, %d", v1.Lyrics[0].IsLineEnding, v1.Lyrics[1].IsLineEnding)
	}
	if !v1.Lyrics[1].Element.IsBackground {
		t.Errorf("expected v1.Lyrics[1].Element.IsBackground = true")
	}
}

func TestParseQQQRC(t *testing.T) {
	qrc := `<?xml version="1.0" encoding="utf-8"?>
<QrcInfos><LyricInfo LyricCount="1">
<Lyric_1 LyricType="1" LyricContent="[1000,2000](1000,200)想(1300,200)你(1600,200)啦&amp;#10;[4000,1000]作词：某人 / 作曲：某人"/>
</LyricInfo></QrcInfos>`
	resp := ParseQQQRC(qrc, ExactMetadata{})
	if resp == nil {
		t.Fatalf("no lines parsed")
	}
	if resp.Type != domain.SyncTypeWord {
		t.Errorf("type = %q, want Word", resp.Type)
	}
	if resp.KpoeTools != "1.0-LPlusBcknd" {
		t.Errorf("KpoeTools = %q", resp.KpoeTools)
	}
	if resp.Metadata.Source != "QQ Music" {
		t.Errorf("source = %q", resp.Metadata.Source)
	}
	// Line 1: syllable text = the chunk BEFORE each (st,dur) token.
	if len(resp.Lyrics) != 1 {
		t.Fatalf("lines = %d", len(resp.Lyrics))
	}
	line := resp.Lyrics[0]
	if len(line.Syllabus) != 3 {
		t.Fatalf("tokens = %d", len(line.Syllabus))
	}
	if line.Syllabus[0].Text != "" || line.Syllabus[0].Time != 1000 || line.Syllabus[0].Duration != 200 {
		t.Errorf("syllabus[0] = %+v", line.Syllabus[0])
	}
	if line.Syllabus[1].Text != "想" || line.Syllabus[1].Time != 1300 {
		t.Errorf("syllabus[1] = %+v", line.Syllabus[1])
	}
	if line.Syllabus[2].Text != "你" {
		t.Errorf("syllabus[2] = %+v", line.Syllabus[2])
	}
	if line.Text != "想你" {
		t.Errorf("line text = %q", line.Text)
	}
	// The line header timing is used when no singer prefix was matched.
	if line.Time != 1000 || line.Duration != 2000 {
		t.Errorf("line timing = %d/%d", line.Time, line.Duration)
	}
	// Credits line has no timed syllables and should be dropped when no
	// current singer context exists.
}

func TestParseQQRCWithSinger(t *testing.T) {
	qrc := `<QrcInfos><LyricInfo><Lyric_1 LyricContent="[0,5000](0,200)周杰伦：(500,200)想(800,200)你(1100,200)啦"/></LyricInfo></QrcInfos>`
	resp := ParseQQQRC(qrc, ExactMetadata{})
	if resp == nil || len(resp.Lyrics) == 0 {
		t.Fatalf("no lines parsed")
	}
	line := resp.Lyrics[0]
	// Singer prefix split: syllabus[0].text becomes the lyrics remainder.
	if line.Element.Singer != "v1" {
		t.Errorf("singer alias = %q", line.Element.Singer)
	}
	if resp.Metadata.Agents["voice1"].Name != "周杰伦" {
		t.Errorf("agent name = %q", resp.Metadata.Agents["voice1"].Name)
	}
	// Remaining syllables: 想 你 (tail "啦" after the last token is dropped).
	if len(line.Syllabus) != 2 {
		t.Fatalf("syllabus after singer split = %d", len(line.Syllabus))
	}
	if line.Syllabus[0].Text != "想" {
		t.Errorf("first lyric syllable = %q", line.Syllabus[0].Text)
	}
	// Timing recomputed from remaining syllabus.
	if line.Time != 800 {
		t.Errorf("recomputed time = %d", line.Time)
	}
	if line.Text != "想你" {
		t.Errorf("line text = %q", line.Text)
	}
}

func TestParseQQRCEchoDrop(t *testing.T) {
	qrc := `<QrcInfos><LyricInfo><Lyric_1 LyricContent="[0,1000](0,100)想(200,100)你"/></LyricInfo></QrcInfos>`
	resp := ParseQQQRC(qrc, ExactMetadata{Title: "Test Song", Artist: "Artist"})
	if resp == nil {
		t.Fatalf("no lines parsed")
	}
	_ = resp
}

func TestLRCParse(t *testing.T) {
	lrc := "[00:01.50]first line\n[00:05.00]second line\n[00:07.000]\n"
	resp := ConvertLRCLIBtoJSON(lrc, 8.0)
	if resp.Type != "line" || resp.KpoeTools != "2.0-LPlusBcknd" {
		t.Errorf("type/kpoe = %q/%q", resp.Type, resp.KpoeTools)
	}
	// Empty-text line is filtered out before duration assignment.
	if len(resp.Lyrics) != 2 {
		t.Fatalf("lines = %d", len(resp.Lyrics))
	}
	if resp.Lyrics[0].Time != 1500 {
		t.Errorf("first time = %d", resp.Lyrics[0].Time)
	}
	if resp.Lyrics[0].Duration != 3500 {
		t.Errorf("first duration = %d", resp.Lyrics[0].Duration)
	}
	// Durations are computed over ALL matched lines before empty-text filtering:
	// the empty line at 7s still contributes 7000-5000=2000.
	if resp.Lyrics[1].Time != 5000 || resp.Lyrics[1].Duration != 2000 {
		t.Errorf("second = %d/%d", resp.Lyrics[1].Time, resp.Lyrics[1].Duration)
	}
	if resp.Lyrics[0].Element.Key != "L1" || resp.Lyrics[1].Element.Key != "L2" {
		t.Errorf("keys = %q/%q", resp.Lyrics[0].Element.Key, resp.Lyrics[1].Element.Key)
	}

	// One-digit fraction scales by 10^len: [00:00.5] => 500ms.
	s := ConvertLRCLIBtoJSON("[00:00.5]a", 1.0)
	if s.Lyrics[0].Time != 500 {
		t.Errorf("0.5 time = %d", s.Lyrics[0].Time)
	}
	// Non-matching lines (no [mm:ss.x] prefix) are skipped.
	s = ConvertLRCLIBtoJSON("plain line\n[00:01.00]ok", 5.0)
	if len(s.Lyrics) != 1 || s.Lyrics[0].Text != "ok" {
		t.Errorf("skip non-matching: %+v", s.Lyrics)
	}
}

func TestConvertMusixmatchRichSync(t *testing.T) {
	rich := `[{"ts":1.1,"te":2.2,"x":"hello world","l":[{"c":"hello","o":0},{"c":" ","o":0.5},{"c":"world","o":0.9}]}]`
	data := []byte(`{"track":{"track_id":"1"},"type":"richsync","lyrics":{"message":{"body":{"richsync":{"richsync_body":` + strconv.Quote(rich) + `,"lyrics_copyright":"Writer(s): Taylor Swift, Max Martin"}}}}}`)

	resp, err := ConvertMusixmatchToJSON(data, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Type != "Word" || resp.KpoeTools != "1.2-MusixmatchToJSON" {
		t.Errorf("type/kpoe = %q/%q", resp.Type, resp.KpoeTools)
	}
	if resp.Metadata.Source != "Musixmatch" || resp.Metadata.LeadingSilence != "0.000" {
		t.Errorf("metadata = %+v", resp.Metadata)
	}
	if len(resp.Metadata.SongWriters) != 2 || resp.Metadata.SongWriters[0] != "Taylor Swift" {
		t.Errorf("songwriters = %q", resp.Metadata.SongWriters)
	}
	if len(resp.Lyrics) != 1 {
		t.Fatalf("lines = %d", len(resp.Lyrics))
	}
	l := resp.Lyrics[0]
	if l.Time != 1100 {
		t.Errorf("line time = %d", l.Time)
	}
	// naturalDuration = (lastWord.time+duration) - line.time  (no next line => no +2s pad)
	if l.Duration != (2000+200)-1100 {
		t.Errorf("line duration = %d", l.Duration)
	}
	// whitespace word merged into "hello" with <100ms rule intact (500ms != 0 case keeps text)
	if len(l.Syllabus) != 2 {
		t.Fatalf("syllables = %d", len(l.Syllabus))
	}
	if l.Syllabus[0].Text != "hello " || l.Syllabus[0].Time != 1100 {
		t.Errorf("syllable[0] = %+v", l.Syllabus[0])
	}
	if l.Text != "hello world" {
		t.Errorf("text = %q", l.Text)
	}
	if l.Syllabus[1].Time != 2000 || l.Syllabus[1].Duration != 200 {
		t.Errorf("syllable[1] = %+v", l.Syllabus[1])
	}
}

func TestConvertMusixmatchSubtitle(t *testing.T) {
	data := []byte(`{"track":{"track_id":"1"},"type":"subtitle","lyrics":{"message":{"body":{"subtitle":{"subtitle_body":"[00:01.00]first\n[00:05.50]second","lyrics_copyright":"Writer(s): Bob"}}}}}`)

	resp, err := ConvertMusixmatchToJSON(data, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Type != "Line" {
		t.Errorf("type = %q", resp.Type)
	}
	// subtitle requires subtitle lines only; word sync unavailable.
	if len(resp.Lyrics) != 2 {
		t.Fatalf("lines = %d", len(resp.Lyrics))
	}
	if resp.Lyrics[0].Time != 1000 || resp.Lyrics[0].Duration != 4500 {
		t.Errorf("line0 = %d/%d", resp.Lyrics[0].Time, resp.Lyrics[0].Duration)
	}
	if resp.Lyrics[1].Time != 5500 || resp.Lyrics[1].Duration != 3000 {
		t.Errorf("line1 (last default 3000) = %d/%d", resp.Lyrics[1].Time, resp.Lyrics[1].Duration)
	}
	// Empty-text lines filtered after processing.
	data2 := []byte(`{"lyrics":{"message":{"body":{"subtitle":{"subtitle_body":"[00:01.00]\n[00:02.00]ok","lyrics_copyright":""}}}}}`)
	resp2, err := ConvertMusixmatchToJSON(data2, false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(resp2.Lyrics) != 1 || resp2.Lyrics[0].Text != "ok" {
		t.Errorf("filter empty: %+v", resp2.Lyrics)
	}
	// The empty line at 1s is filtered out AFTER durations are computed, but the
	// empty line is still consumed when computing the delta; "ok" is last => 3000.
	if resp2.Lyrics[0].Duration != 3000 {
		t.Errorf("duration = %d", resp2.Lyrics[0].Duration)
	}
	// No body -> nil.
	resp3, err := ConvertMusixmatchToJSON([]byte(`{"lyrics":{"message":{"body":{}}}}`), false)
	if err != nil || resp3 != nil {
		t.Errorf("empty body: %v / %+v", err, resp3)
	}
}

func TestConvertSpotifyToJSON(t *testing.T) {
	data := []byte(`{
  "lyrics": {
    "syncType": "LINE_SYNCED",
    "providerDisplayName": "Musixmatch",
    "songWriters": ["A", "B"],
    "lines": [
      {"startTimeMs": 1000, "words": "hello world",
       "syllables": [
         {"startTimeMs": 1000, "endTimeMs": 1500, "text": "Hello"},
         {"startTimeMs": 1700, "endTimeMs": 2000, "text": "world"}
       ]}
    ]
  }
}`)
	resp, err := ConvertSpotifyToJSON(data)
	if err != nil {
		t.Fatalf("spotify parse: %v", err)
	}
	if resp.Type != domain.SyncTypeWord || resp.KpoeTools != "2.0-LPlusBcknd" {
		t.Errorf("type/kpoe = %q/%q", resp.Type, resp.KpoeTools)
	}
	if resp.Metadata.Source != "Musixmatch" {
		t.Errorf("source = %q", resp.Metadata.Source)
	}
	if len(resp.Metadata.SongWriters) != 2 {
		t.Errorf("songWriters = %v", resp.Metadata.SongWriters)
	}
	// 200ms gap > 100ms => trailing space added on "Hello".
	if len(resp.Lyrics[0].Syllabus) != 2 {
		t.Fatalf("syllables = %d", len(resp.Lyrics[0].Syllabus))
	}
	if !strings.HasSuffix(resp.Lyrics[0].Syllabus[0].Text, " ") {
		t.Errorf("expected trailing space, got %q", resp.Lyrics[0].Syllabus[0].Text)
	}
	if resp.Lyrics[0].Syllabus[0].Time != 1000 || resp.Lyrics[0].Syllabus[0].Duration != 500 {
		t.Errorf("syllable[0] timing = %d/%d", resp.Lyrics[0].Syllabus[0].Time, resp.Lyrics[0].Syllabus[0].Duration)
	}
	if resp.Lyrics[0].Time != 1000 || resp.Lyrics[0].Duration != 1000 {
		t.Errorf("line timing = %d/%d", resp.Lyrics[0].Time, resp.Lyrics[0].Duration)
	}
	if resp.Lyrics[0].Text != "Hello world" {
		t.Errorf("text = %q", resp.Lyrics[0].Text)
	}
	if resp.Lyrics[0].Element.Key != "L1" || resp.Lyrics[0].Element.Singer != "v1" {
		t.Errorf("element = %+v", resp.Lyrics[0].Element)
	}
}

func TestSpotifyLineModeAndSongPart(t *testing.T) {
	data := []byte(`{"lyrics":{"syncType":"LINE_SYNCED","providerDisplayName":"Spotify","lines":[
  {"startTimeMs":1000,"endTimeMs":3000,"words":"[Verse 1] hem"},
  {"startTimeMs":3000,"words":"♪"},
  {"startTimeMs":4000,"words":"chorus here"}
]}}`)
	resp, err := ConvertSpotifyToJSON(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Type != domain.SyncTypeLine {
		t.Errorf("type = %q", resp.Type)
	}
	// "♪" lines are filtered out in line mode.
	if len(resp.Lyrics) != 2 {
		t.Fatalf("lines = %d", len(resp.Lyrics))
	}
	if resp.Lyrics[0].Duration != 2000 { // endTimeMs given
		t.Errorf("line0 duration = %d", resp.Lyrics[0].Duration)
	}
	if resp.Lyrics[1].Duration != 0 { // no end, last line => fallback NaN => 0
		t.Errorf("line1 duration = %d", resp.Lyrics[1].Duration)
	}
	if resp.Lyrics[0].Element.SongPart != "Verse" || resp.Lyrics[1].Element.SongPart != "Chorus" {
		t.Errorf("songParts = %q/%q", resp.Lyrics[0].Element.SongPart, resp.Lyrics[1].Element.SongPart)
	}
	// Line mode keeps empty syllabus and singer "".
	if len(resp.Lyrics[0].Syllabus) != 0 || resp.Lyrics[0].Element.Singer != "" {
		t.Errorf("line mode element = %+v", resp.Lyrics[0].Element)
	}
}

func TestNormalizeDeezerWordByWord(t *testing.T) {
	data := []byte(`{"track":{"lyrics":{"writers":"Taylor Swift, Max Martin","copyright":"(c)","licence":"(L)","synchronizedWordByWordLines":[{"start":1000,"end":3500,"words":[{"start":1000,"end":1500,"word":"Hello"},{"start":1500,"end":2000,"word":"world"}]}]}}}`)
	resp, err := NormalizeDeezerLyrics(data)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Type != domain.SyncTypeWord {
		t.Errorf("type = %q", resp.Type)
	}
	if resp.Metadata.Source != "Deezer" || resp.Metadata.SongWriters[1] != "Max Martin" {
		t.Errorf("metadata = %+v", resp.Metadata)
	}
	if len(resp.Lyrics) != 1 {
		t.Fatalf("lines = %d", len(resp.Lyrics))
	}
	l := resp.Lyrics[0]
	if l.Time != 1000 || l.Duration != 2500 {
		t.Errorf("line = %d/%d", l.Time, l.Duration)
	}
	if l.Text != "Hello world" {
		t.Errorf("text = %q", l.Text)
	}
	if len(l.Syllabus) != 2 || l.Syllabus[0].Text != "Hello " || l.Syllabus[1].Text != "world" {
		t.Errorf("syllabus = %+v", l.Syllabus)
	}
	if l.Syllabus[1].Duration != 500 {
		t.Errorf("syllable dur = %d", l.Syllabus[1].Duration)
	}
}

func TestNormalizeDeezerSynchronizedLines(t *testing.T) {
	data := []byte(`{"track":{"lyrics":{"synchronizedLines":[{"milliseconds":1000,"duration":2000,"line":"first"}]}}}`)
	resp, err := NormalizeDeezerLyrics(data)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Type != domain.SyncTypeLine || len(resp.Lyrics) != 1 {
		t.Fatalf("type/lines = %q/%d", resp.Type, len(resp.Lyrics))
	}
	if resp.Lyrics[0].Time != 1000 || resp.Lyrics[0].Duration != 2000 || resp.Lyrics[0].Text != "first" {
		t.Errorf("line = %+v", resp.Lyrics[0])
	}
}

func TestNormalizeDeezerText(t *testing.T) {
	data := []byte(`{"track":{"lyrics":{"text":"line one\r\nline two"}}}`)
	resp, err := NormalizeDeezerLyrics(data)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Type != domain.SyncTypeNone || len(resp.Lyrics) != 2 {
		t.Fatalf("type/lines = %q/%d", resp.Type, len(resp.Lyrics))
	}
	if resp.Lyrics[0].Text != "line one" || resp.Lyrics[1].Text != "line two" {
		t.Errorf("lines = %+v", resp.Lyrics)
	}
}

func TestNormalizeDeezerNil(t *testing.T) {
	resp, err := NormalizeDeezerLyrics([]byte(`{"track":{"lyrics":{}}}`))
	if err != nil || resp == nil || resp.Type != domain.SyncTypeLine {
		t.Errorf("empty: %v / %+v", err, resp)
	}
	resp, err = NormalizeDeezerLyrics([]byte(`{"track":{}}`))
	if err != nil || resp != nil {
		t.Errorf("no lyrics: %v / %+v", err, resp)
	}
}
