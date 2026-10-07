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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/parsers"
	"lyricsplus/backend/internal/proxy"
	"lyricsplus/backend/internal/similarity"
)

const qqmusicName = "qq"

const (
	qqAPIEndpoint      = "https://u.y.qq.com/cgi-bin/musicu.fcg"
	qqSmartboxEndpoint = "https://c.y.qq.com/splcloud/fcgi-bin/smartbox_new.fcg"
)

type QQMusicProvider struct {
	client         *proxy.Client
	cookie         string
	logger         *logger.Logger
	guid           string
	sessionMu      sync.RWMutex
	sessionUID     string
	sessionSID     string
	sessionSavedAt time.Time
}

func NewQQMusic(client *proxy.Client, cookie string) *QQMusicProvider {
	return &QQMusicProvider{
		client: client,
		cookie: cookie,
		guid:   getGUID(),
	}
}

func (p *QQMusicProvider) SetLogger(lg *logger.Logger) { p.logger = lg }

func (p *QQMusicProvider) debugf(format string, args ...any) {
	if p.logger == nil {
		return
	}
	p.logger.Debugf("qqmusic: "+format, args...)
}

func (p *QQMusicProvider) Name() string     { return qqmusicName }
func (p *QQMusicProvider) Configured() bool { return true }

func (p *QQMusicProvider) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	songMid := ""
	var songID int64
	if isQQMid(q.PlatformID) {
		songMid = q.PlatformID
	} else if id, err := strconv.ParseInt(q.PlatformID, 10, 64); err == nil && id > 0 {
		songID = id
	}
	songTitle := q.Title
	songArtist := q.Artist
	songAlbum := q.Album
	songDuration := q.Duration

	if songMid == "" && songID == 0 {
		if q.IDOnly() {
			p.debugf("id-only query, no song resolved")
			return nil, nil
		}

		queries := []string{}
		t := strings.TrimSpace(q.Title)
		a := strings.TrimSpace(q.Artist)
		if t != "" && a != "" {
			queries = append(queries, t+" "+a)
			queries = append(queries, a+" "+t)
		}
		if t != "" {
			queries = append(queries, t)
		}
		if a != "" {
			queries = append(queries, a)
		}

		var best *similarity.BestMatchResult
		for _, query := range queries {
			songs, err := p.search(ctx, query)
			if err != nil || len(songs) == 0 {
				continue
			}
			var candidates []similarity.SongCandidate
			for _, s := range songs {
				singer := ""
				if len(s.Singer) > 0 {
					singer = s.Singer[0].Name
				}
				title := s.Title
				if title == "" {
					title = s.Name
				}
				albumTitle := s.Album.Title
				if albumTitle == "" {
					albumTitle = s.Album.Name
				}
				platID := s.Mid
				if platID == "" && s.ID > 0 {
					platID = strconv.FormatInt(s.ID, 10)
				}
				candidates = append(candidates, similarity.SongCandidate{
					Title:      title,
					Artist:     singer,
					Album:      albumTitle,
					DurationMs: s.Interval * 1000,
					PlatformID: platID,
					Data:       s,
				})
			}
			if len(candidates) == 0 {
				continue
			}

			durationSec := float64(q.Duration) / 1000.0
			match := similarity.FindBestSongMatch(candidates, q.Title, q.Artist, q.Album, durationSec, q.ISRC, q.PlatformID)
			if match != nil {
				best = match
				break
			}
		}

		if best == nil {
			p.debugf("no similarity match for %q / %q across queries", q.Title, q.Artist)
			return nil, nil
		}
		p.debugf("matched song %q mid=%s", best.Candidate.Title, best.Candidate.PlatformID)

		matchedSong := best.Candidate.Data.(songItem)
		songMid = matchedSong.Mid
		songID = matchedSong.ID
		if matchedSong.Title != "" {
			songTitle = matchedSong.Title
		} else if matchedSong.Name != "" {
			songTitle = matchedSong.Name
		}
		if len(matchedSong.Singer) > 0 && matchedSong.Singer[0].Name != "" {
			songArtist = matchedSong.Singer[0].Name
		}
		if matchedSong.Album.Title != "" {
			songAlbum = matchedSong.Album.Title
		} else if matchedSong.Album.Name != "" {
			songAlbum = matchedSong.Album.Name
		}
		if matchedSong.Interval > 0 {
			songDuration = matchedSong.Interval * 1000
		}
	}

	if songMid == "" && songID == 0 {
		return nil, nil
	}

	qrcContent, err := p.fetchQRC(ctx, songMid, songID)
	if err != nil || qrcContent == "" {
		p.debugf("QRC fetch failed for mid=%s id=%d (err=%v)", songMid, songID, err)
		return nil, nil
	}

	platID := songMid
	if platID == "" && songID > 0 {
		platID = strconv.FormatInt(songID, 10)
	}

	exactMeta := parsers.ExactMetadata{
		Title:      songTitle,
		Artist:     songArtist,
		Album:      songAlbum,
		DurationMs: songDuration,
		PlatformID: platID,
	}

	resp := parsers.ParseQQQRC(qrcContent, exactMeta)
	if resp == nil || len(resp.Lyrics) == 0 {
		p.debugf("no parseable lyrics for mid=%s id=%d", songMid, songID)
		return nil, nil
	}
	p.debugf("lyrics parsed lines=%d mid=%s id=%d", len(resp.Lyrics), songMid, songID)

	resp.Metadata.Source = "QQ Music"
	if songTitle != "" {
		resp.Metadata.Title = songTitle
	}
	if songArtist != "" {
		resp.Metadata.Artist = songArtist
	}
	if songAlbum != "" {
		resp.Metadata.Album = songAlbum
	}
	if songDuration > 0 {
		tMin := songDuration / 60000
		tSec := float64(songDuration%60000) / 1000.0
		resp.Metadata.TotalDuration = fmt.Sprintf("%d:%06.3f", tMin, tSec)
	}
	resp.Cached = domain.CacheNone
	resp.RawData = qrcContent
	var durSec *float64
	if songDuration > 0 {
		s := float64(songDuration) / 1000.0
		durSec = &s
	} else if q.Duration > 0 {
		s := float64(q.Duration) / 1000.0
		durSec = &s
	}

	resp.ProcessingTime = &domain.ProcessTiming{
		SelectedSongMetadata: &domain.PickedSongMetadata{
			Source:         "QQ Music",
			Title:          songTitle,
			Artist:         songArtist,
			Album:          songAlbum,
			Duration:       durSec,
			SongISRC:       q.ISRC,
			SongPlatformID: platID,
		},
	}

	return resp, nil
}

type songSinger struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type songAlbum struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Name  string `json:"name"`
}

type songItem struct {
	ID       int64        `json:"id"`
	Mid      string       `json:"mid"`
	Title    string       `json:"title"`
	Name     string       `json:"name"`
	Interval int          `json:"interval"`
	Singer   []songSinger `json:"singer"`
	Album    songAlbum    `json:"album"`
}

func (p *QQMusicProvider) search(ctx context.Context, query string) ([]songItem, error) {
	searchParams := map[string]interface{}{
		"searchid":     getSearchID(),
		"query":        query,
		"search_type":  0,
		"num_per_page": 10,
		"page_num":     1,
		"highlight":    1,
		"grp":          1,
	}

	raw, err := p.apiRequest(ctx, "music.search.SearchCgiService", "DoSearchForQQMusicMobile", searchParams)
	if err == nil {
		var res struct {
			Body struct {
				ItemSong []songItem `json:"item_song"`
			} `json:"body"`
		}
		if err := json.Unmarshal(raw, &res); err == nil && len(res.Body.ItemSong) > 0 {
			return res.Body.ItemSong, nil
		}
	}

	adaptorParams := map[string]interface{}{
		"searchid":    getSearchID(),
		"query":       query,
		"search_type": 100,
		"page_num":    10,
		"page_id":     1,
		"highlight":   1,
		"grp":         1,
	}
	if raw, err := p.apiRequest(ctx, "music.adaptor.SearchAdaptor", "do_search_v2", adaptorParams); err == nil {
		var res struct {
			Body struct {
				Song struct {
					Items []songItem `json:"items"`
				} `json:"song"`
			} `json:"body"`
		}
		if err := json.Unmarshal(raw, &res); err == nil && len(res.Body.Song.Items) > 0 {
			return res.Body.Song.Items, nil
		}
	}

	if items, err := p.searchSmartbox(ctx, query); err == nil && len(items) > 0 {
		return items, nil
	}

	if items, err := p.searchSmartboxCgi(ctx, query); err == nil && len(items) > 0 {
		return items, nil
	}

	return nil, err
}

func (p *QQMusicProvider) searchSmartbox(ctx context.Context, query string) ([]songItem, error) {
	reqURL := fmt.Sprintf("%s?key=%s&format=json", qqSmartboxEndpoint, url.QueryEscape(query))
	headers := make(http.Header)
	headers.Set("Referer", "https://y.qq.com/")
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	if p.cookie != "" {
		headers.Set("Cookie", p.cookie)
	}

	resp, err := p.client.Get(ctx, reqURL, headers)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var res struct {
		Code int `json:"code"`
		Data struct {
			Song struct {
				ItemList []struct {
					Mid    string `json:"mid"`
					Name   string `json:"name"`
					Singer string `json:"singer"`
				} `json:"itemlist"`
			} `json:"song"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	if res.Code != 0 || len(res.Data.Song.ItemList) == 0 {
		return nil, nil
	}

	var items []songItem
	for _, it := range res.Data.Song.ItemList {
		if it.Mid == "" {
			continue
		}
		items = append(items, songItem{
			Mid:   it.Mid,
			Title: it.Name,
			Singer: []songSinger{
				{Name: it.Singer},
			},
		})
	}
	return items, nil
}

func (p *QQMusicProvider) searchSmartboxCgi(ctx context.Context, query string) ([]songItem, error) {
	smartboxParams := map[string]interface{}{
		"search_id":    getSearchID(),
		"query":        query,
		"num_per_page": 10,
		"page_idx":     0,
	}

	raw, err := p.apiRequest(ctx, "music.smartboxCgi.SmartBoxCgi", "GetSmartBoxResult", smartboxParams)
	if err != nil {
		return nil, err
	}

	var res struct {
		Items []struct {
			AssociateItem []struct {
				AssociateID   []int64 `json:"associate_id"`
				AssociateType int     `json:"associate_type"`
			} `json:"associate_item"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}

	var songIDs []int64
	seen := make(map[int64]bool)
	for _, it := range res.Items {
		for _, assoc := range it.AssociateItem {
			if assoc.AssociateType == 1 {
				for _, id := range assoc.AssociateID {
					if id > 0 && !seen[id] {
						seen[id] = true
						songIDs = append(songIDs, id)
						if len(songIDs) >= 10 {
							break
						}
					}
				}
			}
		}
		if len(songIDs) >= 10 {
			break
		}
	}

	if len(songIDs) == 0 {
		return nil, nil
	}

	return p.getTracksByID(ctx, songIDs)
}

func (p *QQMusicProvider) getTracksByID(ctx context.Context, songIDs []int64) ([]songItem, error) {
	types := make([]int, len(songIDs))
	stamps := make([]int, len(songIDs))

	trackParams := map[string]interface{}{
		"ctx":          0,
		"client":       1,
		"types":        types,
		"modify_stamp": stamps,
		"ids":          songIDs,
	}

	raw, err := p.apiRequest(ctx, "music.trackInfo.UniformRuleCtrl", "CgiGetTrackInfo", trackParams)
	if err != nil {
		return nil, err
	}

	var res struct {
		Tracks []struct {
			ID       int64        `json:"id"`
			Mid      string       `json:"mid"`
			Name     string       `json:"name"`
			Title    string       `json:"title"`
			Interval int          `json:"interval"`
			Singer   []songSinger `json:"singer"`
			Album    songAlbum    `json:"album"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}

	var items []songItem
	for _, t := range res.Tracks {
		if t.Mid == "" && t.ID == 0 {
			continue
		}
		title := t.Title
		if title == "" {
			title = t.Name
		}
		albumTitle := t.Album.Title
		if albumTitle == "" {
			albumTitle = t.Album.Name
		}
		items = append(items, songItem{
			ID:       t.ID,
			Mid:      t.Mid,
			Title:    title,
			Interval: t.Interval,
			Singer:   t.Singer,
			Album: songAlbum{
				ID:    t.Album.ID,
				Title: albumTitle,
			},
		})
	}
	return items, nil
}

func (p *QQMusicProvider) fetchQRC(ctx context.Context, songMid string, songID int64) (string, error) {
	tryFetch := func(param map[string]interface{}) (string, error) {
		raw, err := p.apiRequest(ctx, "music.musichallSong.PlayLyricInfo", "GetPlayLyricInfo", param)
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

		var qrcContent string
		if s, ok := res.QRC.(string); ok && s != "" {
			if proc, err := processLyric(s); err == nil && proc != "" {
				qrcContent = proc
			}
		}
		if qrcContent == "" {
			if s, ok := res.Lyric.(string); ok && s != "" {
				if proc, err := processLyric(s); err == nil && proc != "" {
					qrcContent = proc
				}
			}
		}
		return qrcContent, nil
	}

	baseParams := func() map[string]interface{} {
		return map[string]interface{}{
			"crypt":   1,
			"ct":      11,
			"cv":      20090008,
			"lrc_t":   0,
			"qrc":     1,
			"qrc_t":   0,
			"roma":    0,
			"roma_t":  0,
			"trans":   0,
			"trans_t": 0,
			"type":    1,
		}
	}

	if songMid != "" {
		p1 := baseParams()
		p1["songMid"] = songMid
		if content, err := tryFetch(p1); err == nil && content != "" {
			return content, nil
		}
	}

	if songID > 0 {
		p2 := baseParams()
		p2["songId"] = songID
		if content, err := tryFetch(p2); err == nil && content != "" {
			return content, nil
		}
	}

	return "", nil
}

func processLyric(content string) (string, error) {
	if content == "" {
		return "", nil
	}

	if strings.HasPrefix(content, "[") {
		if !strings.Contains(content, "<QrcInfos>") {
			escaped := strings.NewReplacer("&", "&amp;", "\"", "&quot;", "<", "&lt;", ">", "&gt;").Replace(content)
			return fmt.Sprintf("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<QrcInfos>\n<LyricInfo LyricCount=\"1\">\n<Lyric_1 LyricType=\"1\" LyricContent=\"%s\"/>\n</LyricInfo>\n</QrcInfos>", escaped), nil
		}
		return content, nil
	}

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

func (p *QQMusicProvider) ensureSession(ctx context.Context) (string, string) {
	p.sessionMu.RLock()
	if p.sessionUID != "" && p.sessionSID != "" && time.Since(p.sessionSavedAt) < 24*time.Hour {
		u, s := p.sessionUID, p.sessionSID
		p.sessionMu.RUnlock()
		return u, s
	}
	p.sessionMu.RUnlock()

	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if p.sessionUID != "" && p.sessionSID != "" && time.Since(p.sessionSavedAt) < 24*time.Hour {
		return p.sessionUID, p.sessionSID
	}

	raw, err := p.doApiRequest(ctx, "music.getSession.session", "GetSession", map[string]interface{}{
		"uid":    "",
		"vkey":   0,
		"caller": 2,
	}, "", "")
	if err == nil {
		var res struct {
			Session struct {
				UID interface{} `json:"uid"`
				SID string      `json:"sid"`
			} `json:"session"`
		}
		if err := json.Unmarshal(raw, &res); err == nil && res.Session.SID != "" {
			var uidStr string
			switch v := res.Session.UID.(type) {
			case string:
				uidStr = v
			case float64:
				uidStr = strconv.FormatInt(int64(v), 10)
			case int64:
				uidStr = strconv.FormatInt(v, 10)
			}
			if uidStr != "" {
				p.sessionUID = uidStr
				p.sessionSID = res.Session.SID
				p.sessionSavedAt = time.Now()
				p.debugf("GetSession success: uid=%s sid=%s", p.sessionUID, p.sessionSID)
				return p.sessionUID, p.sessionSID
			}
		}
	} else {
		p.debugf("GetSession failed: %v", err)
	}
	p.debugf("GetSession empty result")
	return p.sessionUID, p.sessionSID
}

func (p *QQMusicProvider) apiRequest(ctx context.Context, module, method string, params interface{}) (json.RawMessage, error) {
	uid, sid := p.ensureSession(ctx)
	return p.doApiRequest(ctx, module, method, params, uid, sid)
}

func (p *QQMusicProvider) doApiRequest(ctx context.Context, module, method string, params interface{}, uid, sid string) (json.RawMessage, error) {
	const reqKey = "req_0"
	requestData := map[string]interface{}{
		"comm": p.buildCommonParams(uid, sid),
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

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Referer", "https://y.qq.com/")
	headers.Set("User-Agent", "QQMusic 20090008(android 15)")
	headers.Set("Origin", "https://y.qq.com")
	if p.cookie != "" {
		headers.Set("Cookie", p.cookie)
	}

	resp, err := p.client.Post(ctx, qqAPIEndpoint, headers, bodyBytes)
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

func (p *QQMusicProvider) buildCommonParams(uid, sid string) map[string]interface{} {
	guid := p.guid
	if guid == "" {
		guid = getGUID()
	}
	aid := guid
	if len(aid) > 16 {
		aid = aid[:16]
	}
	nowSec := time.Now().Unix()
	comm := map[string]interface{}{
		"ct":           "11",
		"cv":           "20090008",
		"v":            "20090008",
		"chid":         "10003505",
		"tmeAppID":     "qqmusic",
		"tmeLoginType": "2",
		"QIMEI36":      "e7d72b057a3f66bb0ded13ec10001121aa07",
		"traceid":      fmt.Sprintf("10002_%s_%d", guid, nowSec),
		"OpenUDID":     guid,
		"udid":         guid,
		"OpenUDID2":    guid,
		"aid":          aid,
		"phonetype":    "V2408A",
		"os_ver":       "15",
	}
	if uid != "" {
		comm["uid"] = uid
	}
	if sid != "" {
		comm["sid"] = sid
	}
	return comm
}

func getGUID() string {
	const chars = "0123456789abcdef"
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	var b strings.Builder
	for i := 0; i < 32; i++ {
		b.WriteByte(chars[r.Intn(len(chars))])
	}
	return b.String()
}

var xorScrambleBytes = []byte{89, 39, 179, 150, 218, 82, 58, 252, 177, 52, 186, 123, 120, 64, 242, 133, 143, 161, 121, 179}

func Sign(payload string) string {
	hash := sha1.Sum([]byte(payload))
	hashHex := strings.ToUpper(hex.EncodeToString(hash[:]))

	part1Idx := []int{23, 14, 6, 36, 16, 7, 19}
	part1 := stringAt(hashHex, part1Idx)
	part2 := stringAt(hashHex, []int{16, 1, 32, 12, 19, 27, 8, 5})

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

func (p *QQMusicProvider) NormalizeSong(item songItem) domain.SongCatalogItem {
	title := item.Title
	if title == "" {
		title = item.Name
	}
	singer := ""
	if len(item.Singer) > 0 {
		singer = item.Singer[0].Name
	}
	album := item.Album.Title
	if album == "" {
		album = item.Album.Name
	}
	platID := item.Mid
	if platID == "" && item.ID > 0 {
		platID = strconv.FormatInt(item.ID, 10)
	}

	idMap := map[string]string{}
	if item.Mid != "" {
		idMap["qq"] = item.Mid
	}
	if item.ID > 0 {
		idMap["qq_id"] = strconv.FormatInt(item.ID, 10)
	}

	var artURL *string
	if item.Album.ID > 0 || item.Mid != "" {
		u := fmt.Sprintf("https://y.gtimg.cn/music/photo_new/T002R300x300M000%s.jpg", platID)
		artURL = &u
	}

	return domain.SongCatalogItem{
		ID:           idMap,
		SourceID:     platID,
		Title:        title,
		Artist:       singer,
		Album:        album,
		AlbumArtURL:  artURL,
		DurationMs:   int64(item.Interval * 1000),
		Availability: []string{"QQ Music"},
		ExternalURLs: map[string]string{
			"qq": fmt.Sprintf("https://y.qq.com/n/ryqq/songDetail/%s", platID),
		},
	}
}

func (p *QQMusicProvider) SearchCatalog(ctx context.Context, query string) ([]domain.SongCatalogItem, error) {
	songs, err := p.search(ctx, query)
	if err != nil {
		return nil, err
	}
	limit := len(songs)
	if limit > 10 {
		limit = 10
	}
	items := make([]domain.SongCatalogItem, limit)
	for i := 0; i < limit; i++ {
		items[i] = p.NormalizeSong(songs[i])
	}
	return items, nil
}

func (p *QQMusicProvider) GetMetadata(ctx context.Context, title, artist, album string, durationSec float64) (map[string]interface{}, error) {
	query := strings.TrimSpace(title + " " + artist)
	if query == "" {
		query = title
	}
	songs, err := p.search(ctx, query)
	if err != nil || len(songs) == 0 {
		return nil, err
	}
	var candidates []similarity.SongCandidate
	for _, s := range songs {
		singer := ""
		if len(s.Singer) > 0 {
			singer = s.Singer[0].Name
		}
		t := s.Title
		if t == "" {
			t = s.Name
		}
		al := s.Album.Title
		if al == "" {
			al = s.Album.Name
		}
		platID := s.Mid
		if platID == "" && s.ID > 0 {
			platID = strconv.FormatInt(s.ID, 10)
		}
		candidates = append(candidates, similarity.SongCandidate{
			Title:      t,
			Artist:     singer,
			Album:      al,
			DurationMs: s.Interval * 1000,
			PlatformID: platID,
			Data:       s,
		})
	}
	best := similarity.FindBestSongMatch(candidates, title, artist, album, durationSec, "", "")
	if best == nil {
		return nil, nil
	}
	matched := best.Candidate.Data.(songItem)
	tName := matched.Title
	if tName == "" {
		tName = matched.Name
	}
	singerName := ""
	if len(matched.Singer) > 0 {
		singerName = matched.Singer[0].Name
	}
	alName := matched.Album.Title
	if alName == "" {
		alName = matched.Album.Name
	}
	platID := matched.Mid
	if platID == "" && matched.ID > 0 {
		platID = strconv.FormatInt(matched.ID, 10)
	}
	meta := map[string]interface{}{
		"name":       tName,
		"artistName": singerName,
		"albumName":  alName,
		"durationMs": matched.Interval * 1000,
		"id":         platID,
		"source":     "QQ Music",
		"url":        fmt.Sprintf("https://y.qq.com/n/ryqq/songDetail/%s", platID),
	}
	return meta, nil
}
