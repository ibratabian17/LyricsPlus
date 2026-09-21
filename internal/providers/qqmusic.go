package providers

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/parsers"
	"lyricsplus/backend/internal/proxy"
	"lyricsplus/backend/internal/similarity"
)

const qqmusicName = "qq"

const qqAPIEndpoint = "https://u.y.qq.com/cgi-bin/musics.fcg"

// QQMusicProvider fetches lyrics from QQ Music.
type QQMusicProvider struct {
	client *proxy.Client
	cookie string
}

func NewQQMusic(client *proxy.Client, cookie string) *QQMusicProvider {
	return &QQMusicProvider{client: client, cookie: cookie}
}

func (p *QQMusicProvider) Name() string     { return qqmusicName }
func (p *QQMusicProvider) Configured() bool { return true }

func (p *QQMusicProvider) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	songMid := ""
	if isQQMid(q.PlatformID) {
		songMid = q.PlatformID
	}
	songTitle := q.Title
	songArtist := q.Artist
	songAlbum := q.Album
	songDuration := q.Duration

	if songMid == "" {
		if q.IDOnly() {
			return nil, nil
		}

		query := strings.TrimSpace(q.Title + " " + q.Artist)
		if query == "" {
			return nil, nil
		}

		songs, err := p.search(ctx, query)
		if err != nil || len(songs) == 0 {
			return nil, nil
		}

		candidates := make([]similarity.SongCandidate, len(songs))
		for i, s := range songs {
			singer := ""
			if len(s.Singer) > 0 {
				singer = s.Singer[0].Name
			}
			candidates[i] = similarity.SongCandidate{
				Title:      s.Title,
				Artist:     singer,
				Album:      s.Album.Title,
				DurationMs: s.Interval * 1000,
				PlatformID: s.Mid,
				Data:       s,
			}
		}

		durationSec := float64(q.Duration) / 1000.0
		best := similarity.FindBestSongMatch(candidates, q.Title, q.Artist, q.Album, durationSec, q.ISRC, q.PlatformID)
		if best == nil {
			return nil, nil
		}

		matchedSong := best.Candidate.Data.(songItem)
		songMid = matchedSong.Mid
		if matchedSong.Title != "" {
			songTitle = matchedSong.Title
		}
		if len(matchedSong.Singer) > 0 && matchedSong.Singer[0].Name != "" {
			songArtist = matchedSong.Singer[0].Name
		}
		if matchedSong.Album.Title != "" {
			songAlbum = matchedSong.Album.Title
		}
		if matchedSong.Interval > 0 {
			songDuration = matchedSong.Interval * 1000
		}
	}

	if songMid == "" {
		return nil, nil
	}

	qrcContent, err := p.fetchQRC(ctx, songMid)
	if err != nil || qrcContent == "" {
		return nil, err
	}

	exactMeta := parsers.ExactMetadata{
		Title:      songTitle,
		Artist:     songArtist,
		Album:      songAlbum,
		DurationMs: songDuration,
		PlatformID: songMid,
	}

	resp := parsers.ParseQQQRC(qrcContent, exactMeta)
	if resp == nil || len(resp.Lyrics) == 0 {
		return nil, nil
	}

	resp.Metadata.Source = "QQ Music"
	resp.Metadata.Title = ""
	resp.Metadata.Artist = ""
	resp.Metadata.Album = ""
	if songDuration > 0 {
		tMin := songDuration / 60000
		tSec := float64(songDuration%60000) / 1000.0
		resp.Metadata.TotalDuration = fmt.Sprintf("%d:%06.3f", tMin, tSec)
	}
	resp.Cached = domain.CacheNone
	resp.RawData = qrcContent
	resp.ProcessingTime = &domain.ProcessTiming{
		SelectedSongMetadata: &domain.PickedSongMetadata{
			Source:         "QQ Music",
			Title:          songTitle,
			Artist:         songArtist,
			Album:          songAlbum,
			SongISRC:       q.ISRC,
			SongPlatformID: songMid,
		},
	}

	return resp, nil
}

type songItem struct {
	Mid      string `json:"mid"`
	Title    string `json:"title"`
	Interval int    `json:"interval"`
	Singer   []struct {
		Name string `json:"name"`
	} `json:"singer"`
	Album struct {
		Title string `json:"title"`
	} `json:"album"`
}

func (p *QQMusicProvider) search(ctx context.Context, query string) ([]songItem, error) {
	searchParams := map[string]interface{}{
		"searchid":     getSearchID(),
		"query":        query,
		"search_type":  0,
		"num_per_page": 5,
		"page_num":     1,
		"highlight":    1,
		"grp":          1,
	}

	raw, err := p.apiRequest(ctx, "music.search.SearchCgiService", "DoSearchForQQMusicMobile", searchParams)
	if err != nil {
		return nil, err
	}

	var res struct {
		Body struct {
			ItemSong []songItem `json:"item_song"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Body.ItemSong, nil
}

func (p *QQMusicProvider) fetchQRC(ctx context.Context, songMid string) (string, error) {
	lyricParams := map[string]interface{}{
		"crypt":   1,
		"ct":      11,
		"cv":      13020508,
		"lrc_t":   0,
		"qrc":     1,
		"qrc_t":   0,
		"roma":    0,
		"roma_t":  0,
		"trans":   0,
		"trans_t": 0,
		"type":    1,
		"songMid": songMid,
	}

	raw, err := p.apiRequest(ctx, "music.musichallSong.PlayLyricInfo", "GetPlayLyricInfo", lyricParams)
	if err != nil {
		return "", err
	}

	var res struct {
		QRC   interface{} `json:"qrc"`
		Lyric interface{} `json:"lyric"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}

	var content string
	if s, ok := res.QRC.(string); ok && s != "" {
		content = s
	} else if s, ok := res.Lyric.(string); ok && s != "" {
		content = s
	}
	if content == "" {
		return "", nil
	}

	return processLyric(content)
}

func processLyric(content string) (string, error) {
	if content == "" {
		return "", nil
	}

	// Plain LRC: wrap in QrcInfos XML if needed
	if strings.HasPrefix(content, "[") {
		if !strings.Contains(content, "<QrcInfos>") {
			escaped := strings.NewReplacer("&", "&amp;", "\"", "&quot;", "<", "&lt;", ">", "&gt;").Replace(content)
			return fmt.Sprintf("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<QrcInfos>\n<LyricInfo LyricCount=\"1\">\n<Lyric_1 LyricType=\"1\" LyricContent=\"%s\"/>\n</LyricInfo>\n</QrcInfos>", escaped), nil
		}
		return content, nil
	}

	// Hex-encoded 3DES string
	if len(content)%2 == 0 && isHexString(content) {
		dec, err := DecryptQRC(content)
		if err == nil && dec != "" {
			return dec, nil
		}
	}

	return content, nil
}

func isQQMid(id string) bool {
	return len(id) == 14 && strings.HasPrefix(id, "00")
}

func isHexString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func (p *QQMusicProvider) apiRequest(ctx context.Context, module, method string, params interface{}) (json.RawMessage, error) {
	reqKey := fmt.Sprintf("%s.%s", module, method)
	requestData := map[string]interface{}{
		"comm": buildCommonParams(),
		reqKey: map[string]interface{}{
			"module": module,
			"method": method,
			"param":  params,
		},
	}

	bodyBytes, err := json.Marshal(requestData)
	if err != nil {
		return nil, err
	}

	signature := Sign(string(bodyBytes))
	reqURL := fmt.Sprintf("%s?sign=%s", qqAPIEndpoint, signature)

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Referer", "https://y.qq.com/")
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	headers.Set("Origin", "https://y.qq.com")
	if p.cookie != "" {
		headers.Set("Cookie", p.cookie)
	}

	resp, err := p.client.Post(ctx, reqURL, headers, bodyBytes)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &root); err != nil {
		return nil, fmt.Errorf("qq api response not json: %w", err)
	}

	subData, ok := root[reqKey]
	if !ok {
		return nil, fmt.Errorf("qq api response missing %s", reqKey)
	}

	var check struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(subData, &check); err == nil && check.Code == 0 && len(check.Data) > 0 {
		return check.Data, nil
	}

	return subData, nil
}

func getSearchID() string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	e := int64(r.Intn(20) + 1)
	t := e * 18014398509481984
	n := int64(r.Intn(4194304)) * 4294967296
	rem := time.Now().UnixMilli() % 86400000
	return strconv.FormatInt(t+n+rem, 10)
}

func buildCommonParams() map[string]interface{} {
	return map[string]interface{}{
		"wid":        getGUID(),
		"cv":         13020508,
		"v":          13020508,
		"QIMEI36":    "8888888888888888",
		"ct":         "11",
		"tmeAppID":   "qqmusic",
		"format":     "json",
		"inCharset":  "utf-8",
		"outCharset": "utf-8",
		"uid":        "3931641530",
	}
}

func getGUID() string {
	const chars = "0123456789ABCDEF"
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	var b strings.Builder
	for i := 0; i < 32; i++ {
		b.WriteByte(chars[r.Intn(len(chars))])
	}
	return b.String()
}

// xorScrambleBytes is the 20-byte XOR mask from the signing algorithm.
var xorScrambleBytes = []byte{89, 39, 179, 150, 218, 82, 58, 252, 177, 52, 186, 123, 120, 64, 242, 133, 143, 161, 121, 179}

// Sign builds a zzc${part1}${b64}${part2} request signature.
func Sign(payload string) string {
	hash := sha1.Sum([]byte(payload))
	hashHex := hex.EncodeToString(hash[:])

	part1Idx := []int{23, 14, 6, 36, 16, 40, 7, 19}
	part1 := ""
	for _, i := range part1Idx {
		if i < 40 {
			part1 += string(hashHex[i])
		}
	}
	part2 := stringAt(hashHex, []int{16, 1, 32, 12, 19, 27, 8, 5})

	// XOR the last 20 raw hash bytes (hex char pairs) against the scramble values.
	scrambled := make([]byte, len(xorScrambleBytes))
	for i := 0; i < len(xorScrambleBytes); i++ {
		pair, err := strconv.ParseUint(hashHex[i*2:i*2+2], 16, 8)
		if err != nil {
			pair = 0
		}
		scrambled[i] = xorScrambleBytes[i] ^ byte(pair)
	}
	b64 := base64.StdEncoding.EncodeToString(scrambled)
	b64 = strings.NewReplacer("/", "", "+", "", "=", "").Replace(b64)
	return strings.ToLower(fmt.Sprintf("zzc%s%s%s", part1, b64, part2))
}

func stringAt(s string, idx []int) string {
	var b strings.Builder
	for _, i := range idx {
		if i >= 0 && i < len(s) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// ParseQRC is exposed for tests.
func ParseQRC(xmlText, defTitle, defArtist string) *domain.LyricsResponse {
	resp := parsers.ParseQQQRC(xmlText, parsers.ExactMetadata{Title: defTitle, Artist: defArtist})
	if resp == nil {
		return resp
	}
	if resp.Metadata.Title == "" {
		resp.Metadata.Title = defTitle
	}
	if resp.Metadata.Artist == "" {
		resp.Metadata.Artist = defArtist
	}
	return resp
}
