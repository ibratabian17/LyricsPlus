package providers

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/parsers"
	"lyricsplus/backend/internal/proxy"
	"lyricsplus/backend/internal/similarity"
)

const mxmSigningKey = "IEJ5E8XFaHQvIQNfs7IC"

const (
	mxmWebBaseURL       = "https://apic-desktop.musixmatch.com/ws/1.1"
	mxmAndroidBaseURL   = "https://apic.musixmatch.com/ws/1.1/"
	androidAppID        = "android-player-v1.0"
	androidUserAgent    = "Dalvik/2.1.0 (Linux; U; Android 16; Pixel 8 Pro Build/BP31.250502.008)"
	mxmDefaultWebCookie = "AWSELB=55578B011601B1EF8BC274C33F9043CA947F99DCFF0A80541772015CA2B39C35C0F9E1C932D31725A7310BCAEB0C37431E024E2B45320B7F2C84490C2C97351FDE34690157"
	mxmDefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	mxmTokenExpiry      = 12 * time.Hour
	mxmSearchCacheTTL   = 5 * time.Minute
)

type mxmTrackCacheEntry struct {
	tracks  []mxmTrack
	track   *mxmTrack
	created time.Time
}

type mxmSharedState struct {
	mgm            *AccountManager[config.MusixmatchAccount]
	webMu          sync.RWMutex
	webTok         map[int]cachedWebToken
	cookieDisabled map[int]bool
	androidMu      sync.Mutex
	android        map[int]*mxmAndroidState
	searchCache    *lru.Cache[string, *mxmTrackCacheEntry]
	richsyncCache  *lru.Cache[int64, json.RawMessage]
}

type MusixmatchProvider struct {
	client *proxy.Client
	name   string
	word   bool
	mgm    *AccountManager[config.MusixmatchAccount]
	logger *logger.Logger
	state  *mxmSharedState
}

type cachedWebToken struct {
	token     string
	expires   time.Time
	failUntil time.Time
}

type mxmAndroidState struct {
	currentToken string
	isLoggedIn   bool
	expiresAt    time.Time
	failUntil    time.Time
	initializing bool
	initDoneCh   chan struct{}
	initErr      error
}

func NewMusixmatch(client *proxy.Client) *MusixmatchProvider {
	return NewMusixmatchWithConfig(client, config.Load().Provider, "musixmatch", false)
}

func NewMusixmatchWordSync(client *proxy.Client) *MusixmatchProvider {
	return NewMusixmatchWithConfig(client, config.Load().Provider, "musixmatch-word", true)
}

func NewMusixmatchWithConfig(client *proxy.Client, cfg config.Provider, pname string, word bool) *MusixmatchProvider {
	accounts := cfg.MusixmatchAccounts
	if len(accounts) == 0 {
		accounts = []config.MusixmatchAccount{{
			NAMEID:     "MusixmatchGuest",
			AUTH_TYPE:  "web",
			COOKIE:     cfg.MusixmatchCookie,
			USER_AGENT: cfg.MusixmatchUserAgent,
		}}
	}
	mgm := newAccountManager(accounts)
	sc, _ := lru.New[string, *mxmTrackCacheEntry](1024)
	rc, _ := lru.New[int64, json.RawMessage](512)
	st := &mxmSharedState{
		mgm:            mgm,
		webTok:         map[int]cachedWebToken{},
		cookieDisabled: map[int]bool{},
		android:        map[int]*mxmAndroidState{},
		searchCache:    sc,
		richsyncCache:  rc,
	}
	return &MusixmatchProvider{
		client: client,
		name:   pname,
		word:   word,
		mgm:    mgm,
		state:  st,
	}
}

func (p *MusixmatchProvider) ShareStateFrom(other *MusixmatchProvider) {
	if p == nil || other == nil || other.state == nil {
		return
	}
	p.state = other.state
	p.mgm = other.mgm
}

func (p *MusixmatchProvider) SetLogger(lg *logger.Logger) { p.logger = lg }

func (p *MusixmatchProvider) debugf(format string, args ...any) {
	if p.logger == nil {
		return
	}
	p.logger.Debugf(p.name+": "+format, args...)
}

func isValidMxmToken(t string) bool {
	t = strings.TrimSpace(t)
	if len(t) < 10 || strings.Contains(t, "UpgradeOnly") {
		return false
	}
	return strings.Trim(t, "0") != ""
}

var mxmHoneypotLyricMarkers = []string{"wob gopini den", "tefe woxica fero"}

func isMxmHoneypotLyrics(text string) bool {
	l := strings.ToLower(text)
	for _, m := range mxmHoneypotLyricMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// isMxmHoneypotTrack reports decoy catalogue entries. A specific decoy track is
// still allowed when the query explicitly asks for it.
func isMxmHoneypotTrack(trackName, artistName, query string) bool {
	artist := strings.ToLower(artistName)
	if strings.Contains(artist, "cat music") {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(trackName))
	q := strings.ToLower(query)
	switch {
	case name == "casual" && strings.Contains(artist, "doja cat"):
		return !strings.Contains(q, "casual")
	case name == "nokia" && strings.Contains(artist, "drake"):
		return !strings.Contains(q, "nokia")
	}
	return false
}

func filterMxmHoneypotTracks(tracks []mxmTrack, query string) []mxmTrack {
	if len(tracks) == 0 {
		return tracks
	}
	out := make([]mxmTrack, 0, len(tracks))
	for _, t := range tracks {
		if isMxmHoneypotTrack(t.TrackName, t.ArtistName, query) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func extractTokenFromCookie(cookie string) string {
	idx := strings.Index(cookie, "musixmatchUserToken=")
	if idx == -1 {
		return ""
	}
	sub := cookie[idx+len("musixmatchUserToken="):]
	if end := strings.Index(sub, ";"); end != -1 {
		sub = sub[:end]
	}
	decoded, err := url.QueryUnescape(sub)
	if err != nil {
		decoded = sub
	}
	var payload struct {
		Tokens map[string]string `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(decoded), &payload); err == nil {
		if tok, ok := payload.Tokens["web-desktop-app-v1.0"]; ok && tok != "" {
			return tok
		}
	}
	return ""
}

func (p *MusixmatchProvider) Name() string { return p.name }
func (p *MusixmatchProvider) Configured() bool {
	for i := 0; i < p.mgm.Count(); i++ {
		if acc, ok := p.mgm.At(i); ok && acc.IsConfigured() {
			return true
		}
	}
	return false
}

func (p *MusixmatchProvider) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	if !p.Configured() {
		return nil, nil
	}
	startIdx := p.mgm.CurrentIndex()
	accountIdx := startIdx
	tried := 0
	total := p.mgm.Count()
	var lastErr error
	for tried < total {
		tried++
		res, err := p.fetchLyricsWithAccount(ctx, q, accountIdx)
		if err == nil && res != nil {
			return res, nil
		}
		if err == nil && res == nil {
			return nil, nil
		}
		lastErr = err
		p.debugf("musixmatch account %d error: %v, rotating", accountIdx, err)
		p.mgm.MarkBad(accountIdx, 20*time.Second)
		next, hasNext := p.mgm.Next(accountIdx)
		if !hasNext || next == startIdx {
			break
		}
		accountIdx = next
	}
	return nil, lastErr
}

func (p *MusixmatchProvider) fetchLyricsWithAccount(ctx context.Context, q domain.SearchQuery, accountIdx int) (*domain.LyricsResponse, error) {
	var matchedTrack *mxmTrack

	if q.ISRC != "" {
		isrcKey := "isrc:" + strings.ToLower(strings.TrimSpace(q.ISRC))
		if p.state.searchCache != nil {
			if cached, ok := p.state.searchCache.Get(isrcKey); ok && cached != nil && time.Since(cached.created) < mxmSearchCacheTTL {
				matchedTrack = cached.track
			}
		}
		if matchedTrack == nil {
			tr, err := p.searchMatcher(ctx, url.Values{"q_track_isrc": {q.ISRC}}, accountIdx)
			if err == nil && tr != nil && tr.TrackID != 0 {
				matchedTrack = tr
				if p.state.searchCache != nil {
					p.state.searchCache.Add(isrcKey, &mxmTrackCacheEntry{track: tr, created: time.Now()})
				}
				p.debugf("ISRC search isrc:%s matched track %d", q.ISRC, tr.TrackID)
			} else if err != nil {
				p.debugf("ISRC search isrc:%s failed (err=%v)", q.ISRC, err)
			}
		}
	}

	if matchedTrack == nil {
		if q.IDOnly() {
			p.debugf("id-only query, no track resolved")
			return nil, nil
		}
		durationSec := float64(q.Duration) / 1000.0
		var searchErr error
		matchedTrack, searchErr = p.searchBestMatch(ctx, q.Title, q.Artist, q.Album, durationSec, q.ISRC, accountIdx)
		if searchErr != nil {
			return nil, searchErr
		}
		if matchedTrack != nil {
			p.debugf("best match track %q id=%d", matchedTrack.TrackName, matchedTrack.TrackID)
		}
	}

	if matchedTrack == nil {
		p.debugf("no track matched for %q / %q", q.Title, q.Artist)
		return nil, nil
	}

	lyricsEnv, err := p.fetchLyricsFromAPI(ctx, matchedTrack.TrackID, p.word, accountIdx)
	if err != nil || lyricsEnv == nil {
		p.debugf("lyrics fetch failed for track %d (err=%v)", matchedTrack.TrackID, err)
		return nil, err
	}

	envelopeData := map[string]interface{}{
		"track":  matchedTrack,
		"lyrics": lyricsEnv,
	}
	envelopeJSON, err := json.Marshal(envelopeData)
	if err != nil {
		return nil, err
	}

	converted, err := parsers.ConvertMusixmatchToJSON(envelopeJSON, p.word)
	if err != nil || converted == nil || len(converted.Lyrics) == 0 {
		p.debugf("no parseable lyrics for track %d (err=%v)", matchedTrack.TrackID, err)
		return nil, nil
	}

	for _, ln := range converted.Lyrics {
		if isMxmHoneypotLyrics(ln.Text) {
			p.debugf("honeypot lyrics for track %d, discarding", matchedTrack.TrackID)
			return nil, nil
		}
	}

	if p.word && converted.Type != domain.SyncTypeWord {
		p.debugf("word-sync requested but track %d returned type %q", matchedTrack.TrackID, converted.Type)
		return nil, nil
	}
	p.debugf("lyrics parsed lines=%d word=%t track=%d", len(converted.Lyrics), p.word, matchedTrack.TrackID)

	converted.Metadata.Source = "Musixmatch"
	converted.Cached = domain.CacheNone
	var durSec *float64
	if matchedTrack.TrackLength > 0 {
		s := float64(matchedTrack.TrackLength)
		durSec = &s
	} else if q.Duration > 0 {
		s := float64(q.Duration) / 1000.0
		durSec = &s
	}

	converted.ProcessingTime = &domain.ProcessTiming{
		SelectedSongMetadata: &domain.PickedSongMetadata{
			Source:         "Musixmatch",
			Title:          matchedTrack.TrackName,
			Artist:         matchedTrack.ArtistName,
			Album:          matchedTrack.AlbumName,
			Duration:       durSec,
			SongISRC:       matchedTrack.TrackISRC,
			SongPlatformID: strconv.FormatInt(matchedTrack.TrackID, 10),
		},
	}
	return converted, nil
}

type mxmWriterList []string

func (w *mxmWriterList) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*w = nil
		return nil
	}
	var names []string
	if err := json.Unmarshal(data, &names); err == nil {
		*w = names
		return nil
	}
	var objects []struct {
		WriterName string `json:"writer_name"`
	}
	if err := json.Unmarshal(data, &objects); err == nil {
		out := make([]string, 0, len(objects))
		for _, o := range objects {
			out = append(out, o.WriterName)
		}
		*w = out
		return nil
	}
	return fmt.Errorf("cannot unmarshal writer_list")
}

func (w mxmWriterList) MarshalJSON() ([]byte, error) { return json.Marshal([]string(w)) }

type mxmTrack struct {
	TrackID     int64         `json:"track_id"`
	TrackName   string        `json:"track_name"`
	ArtistName  string        `json:"artist_name"`
	AlbumName   string        `json:"album_name"`
	TrackLength int           `json:"track_length"`
	TrackISRC   string        `json:"track_isrc"`
	WriterList  mxmWriterList `json:"writer_list,omitempty"`
}

type trackWrapper struct {
	Track mxmTrack `json:"track"`
}

func (p *MusixmatchProvider) account(accountIdx int) (config.MusixmatchAccount, error) {
	acc, ok := p.mgm.At(accountIdx)
	if !ok {
		return config.MusixmatchAccount{}, fmt.Errorf("musixmatch: no account available")
	}
	return acc, nil
}

func (p *MusixmatchProvider) isAndroid(accountIdx int) bool {
	acc, err := p.account(accountIdx)
	if err != nil {
		return false
	}
	return strings.EqualFold(acc.AUTH_TYPE, "android")
}

func (p *MusixmatchProvider) getUserToken(ctx context.Context, accountIdx int) (string, error) {
	if p.isAndroid(accountIdx) {
		return "", nil
	}
	acc, err := p.account(accountIdx)
	if err != nil {
		return "", err
	}

	now := time.Now()
	p.state.webMu.RLock()
	t, hasTok := p.state.webTok[accountIdx]
	cookieDisabled := p.state.cookieDisabled[accountIdx]
	p.state.webMu.RUnlock()

	if hasTok {
		if t.token != "" && now.Before(t.expires) {
			return t.token, nil
		}
		if now.Before(t.failUntil) {
			return "", fmt.Errorf("musixmatch: web token in cooldown")
		}
	}

	if !cookieDisabled {
		if tok := extractTokenFromCookie(acc.COOKIE); isValidMxmToken(tok) {
			p.state.webMu.Lock()
			p.state.webTok[accountIdx] = cachedWebToken{token: tok, expires: time.Now().Add(time.Hour)}
			p.state.webMu.Unlock()
			return tok, nil
		}
	}

	raw, err := p.webRawRequest(ctx, "token.get", nil, accountIdx)
	if err != nil {
		p.state.webMu.Lock()
		p.state.webTok[accountIdx] = cachedWebToken{failUntil: time.Now().Add(15 * time.Second)}
		p.state.webMu.Unlock()
		return "", err
	}
	var res struct {
		Message struct {
			Body struct {
				UserToken string `json:"user_token"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	tok := res.Message.Body.UserToken
	if !isValidMxmToken(tok) {
		p.state.webMu.Lock()
		p.state.webTok[accountIdx] = cachedWebToken{failUntil: time.Now().Add(30 * time.Second)}
		p.state.webMu.Unlock()
		return "", fmt.Errorf("musixmatch: invalid user token")
	}

	p.state.webMu.Lock()
	p.state.webTok[accountIdx] = cachedWebToken{token: tok, expires: time.Now().Add(mxmTokenExpiry)}
	p.state.webMu.Unlock()
	return tok, nil
}

func (p *MusixmatchProvider) invalidateWebToken(accountIdx int) {
	p.state.webMu.Lock()
	delete(p.state.webTok, accountIdx)
	p.state.cookieDisabled[accountIdx] = true
	p.state.webMu.Unlock()
}

func (p *MusixmatchProvider) makeWebRequest(ctx context.Context, endpoint string, params url.Values, accountIdx int) (json.RawMessage, error) {
	raw, err := p.webRawRequest(ctx, endpoint, params, accountIdx)
	if err != nil {
		return nil, err
	}
	var statusCheck struct {
		Message struct {
			Header struct {
				StatusCode int    `json:"status_code"`
				Hint       string `json:"hint"`
			} `json:"header"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &statusCheck); err == nil {
		code := statusCheck.Message.Header.StatusCode
		if code == 401 || code == 402 || statusCheck.Message.Header.Hint == "renew" {
			p.invalidateWebToken(accountIdx)
			if params != nil && params.Get("usertoken") != "" {
				if newTok, tokErr := p.getUserToken(ctx, accountIdx); tokErr == nil && newTok != "" {
					params.Set("usertoken", newTok)
					if retryRaw, retryErr := p.webRawRequest(ctx, endpoint, params, accountIdx); retryErr == nil {
						if json.Unmarshal(retryRaw, &statusCheck) == nil && statusCheck.Message.Header.StatusCode == 200 {
							return retryRaw, nil
						}
					}
				}
			}
		}
		if statusCheck.Message.Header.StatusCode != 200 {
			if statusCheck.Message.Header.StatusCode == 404 {
				return nil, nil
			}
			return nil, fmt.Errorf("musixmatch web error (%d): %s",
				statusCheck.Message.Header.StatusCode, statusCheck.Message.Header.Hint)
		}
	}
	return raw, nil
}

func (p *MusixmatchProvider) webRawRequest(ctx context.Context, endpoint string, params url.Values, accountIdx int) (json.RawMessage, error) {
	acc, err := p.account(accountIdx)
	if err != nil {
		return nil, err
	}
	if params == nil {
		params = url.Values{}
	}

	params.Set("app_id", "web-desktop-app-v1.0")

	reqURL := fmt.Sprintf("%s/%s?%s", mxmWebBaseURL, strings.TrimPrefix(endpoint, "/"), params.Encode())

	headers := make(http.Header)
	headers.Set("authority", "apic-desktop.musixmatch.com")
	ua := acc.USER_AGENT
	if ua == "" {
		ua = mxmDefaultUserAgent
	}
	headers.Set("User-Agent", ua)
	if acc.COOKIE != "" {
		headers.Set("Cookie", acc.COOKIE)
	} else {
		headers.Set("Cookie", mxmDefaultWebCookie)
	}
	headers.Set("Origin", "https://musixmatch.com")

	resp, err := p.client.Get(ctx, reqURL, headers)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("musixmatch web request failed with status %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (p *MusixmatchProvider) webTrackSearch(ctx context.Context, query string, accountIdx int) ([]mxmTrack, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	vals := url.Values{}
	vals.Set("page_size", "5")
	vals.Set("f_has_lyrics", "true")
	vals.Set("page", "1")
	vals.Set("q", query)

	userToken, err := p.getUserToken(ctx, accountIdx)
	if err != nil {
		return nil, err
	}
	vals.Set("usertoken", userToken)

	raw, err := p.makeWebRequest(ctx, "track.search", vals, accountIdx)
	if err != nil || raw == nil {
		return nil, err
	}

	var res struct {
		Message struct {
			Body struct {
				TrackList []trackWrapper `json:"track_list"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}

	out := make([]mxmTrack, len(res.Message.Body.TrackList))
	for i, tw := range res.Message.Body.TrackList {
		out[i] = tw.Track
	}
	return out, nil
}

func (p *MusixmatchProvider) searchMatcher(ctx context.Context, params url.Values, accountIdx int) (*mxmTrack, error) {
	if p.isAndroid(accountIdx) {
		defaults := url.Values{
			"subtitle_format": {"dfxp"},
			"optional_calls":  {"track.richsync"},
			"part":            {"lyrics_crowd,user,lyrics_vote,track_lyrics_translation_status,lyrics_verified_by,labels,track_isrc,writer_list,credits"},
		}
		for k, v := range params {
			defaults[k] = v
		}
		raw, err := p.androidRequest(ctx, "matcher.track.get", defaults, nil, accountIdx)
		if err != nil || raw == nil {
			return nil, err
		}
		var res struct {
			Message struct {
				Body struct {
					Track *mxmTrack `json:"track"`
				} `json:"body"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, err
		}
		return res.Message.Body.Track, nil
	}

	userToken, err := p.getUserToken(ctx, accountIdx)
	if err != nil {
		return nil, err
	}
	if params == nil {
		params = url.Values{}
	}
	defaults := url.Values{
		"subtitle_format": {"dfxp"},
		"optional_calls":  {"track.richsync"},
		"part":            {"lyrics_crowd,user,lyrics_vote,track_lyrics_translation_status,lyrics_verified_by,labels,track_isrc,writer_list,credits"},
	}
	for k, v := range defaults {
		if _, set := params[k]; !set {
			params[k] = v
		}
	}
	params.Set("usertoken", userToken)
	raw, err := p.makeWebRequest(ctx, "matcher.track.get", params, accountIdx)
	if err != nil || raw == nil {
		return nil, err
	}

	var res struct {
		Message struct {
			Body struct {
				Track *mxmTrack `json:"track"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res.Message.Body.Track, nil
}

func (p *MusixmatchProvider) SearchTrack(ctx context.Context, query string) ([]mxmTrack, error) {
	return p.searchTrack(ctx, query, p.mgm.CurrentIndex())
}

func (p *MusixmatchProvider) searchTrack(ctx context.Context, query string, accountIdx int) ([]mxmTrack, error) {
	cleanQ := strings.ToLower(strings.TrimSpace(query))
	if cleanQ == "" {
		return nil, nil
	}
	cacheKey := "q:" + cleanQ
	if p.state.searchCache != nil {
		if entry, ok := p.state.searchCache.Get(cacheKey); ok && entry != nil && time.Since(entry.created) < mxmSearchCacheTTL {
			return entry.tracks, nil
		}
	}

	var out []mxmTrack
	var err error
	if !p.isAndroid(accountIdx) {
		out, err = p.webTrackSearch(ctx, query, accountIdx)
	} else {
		params := url.Values{
			"q":                 {query},
			"part":              {"track_artist,artist_image"},
			"track_fields_set":  {"android_track_list"},
			"artist_fields_set": {"android_track_list_artist"},
			"page":              {"1"},
			"page_size":         {"5"},
		}
		var raw json.RawMessage
		raw, err = p.androidRequest(ctx, "macro.search", params, nil, accountIdx)
		if err == nil && raw != nil {
			var res struct {
				Message struct {
					Body struct {
						MacroResultList struct {
							TrackList []trackWrapper `json:"track_list"`
						} `json:"macro_result_list"`
					} `json:"body"`
				} `json:"message"`
			}
			if uErr := json.Unmarshal(raw, &res); uErr == nil {
				out = make([]mxmTrack, len(res.Message.Body.MacroResultList.TrackList))
				for i, tw := range res.Message.Body.MacroResultList.TrackList {
					out[i] = tw.Track
				}
			} else {
				err = uErr
			}
		}
	}
	out = filterMxmHoneypotTracks(out, query)
	if err == nil && p.state.searchCache != nil {
		p.state.searchCache.Add(cacheKey, &mxmTrackCacheEntry{tracks: out, created: time.Now()})
	}
	return out, err
}

func (p *MusixmatchProvider) SearchBestMatch(ctx context.Context, title, artist, album string, durationSec float64, songISRC string) (*mxmTrack, error) {
	return p.searchBestMatch(ctx, title, artist, album, durationSec, songISRC, p.mgm.CurrentIndex())
}

func (p *MusixmatchProvider) searchBestMatch(ctx context.Context, title, artist, album string, durationSec float64, songISRC string, accountIdx int) (*mxmTrack, error) {
	queries := []string{
		strings.TrimSpace(title + " " + artist),
		strings.TrimSpace(title),
	}

	var candidates []mxmTrack
	for _, q := range queries {
		if q == "" {
			continue
		}
		tracks, err := p.searchTrack(ctx, q, accountIdx)
		if err != nil {
			if len(candidates) == 0 {
				return nil, err
			}
			continue
		}
		if len(tracks) > 0 {
			if songISRC != "" {
				for _, tr := range tracks {
					if strings.EqualFold(tr.TrackISRC, songISRC) {
						return &tr, nil
					}
				}
			}
			candidates = append(candidates, tracks...)
			simCandidates := make([]similarity.SongCandidate, len(candidates))
			for i, c := range candidates {
				simCandidates[i] = similarity.SongCandidate{
					Title:      c.TrackName,
					Artist:     c.ArtistName,
					Album:      c.AlbumName,
					DurationMs: c.TrackLength * 1000,
					ISRC:       c.TrackISRC,
					PlatformID: strconv.FormatInt(c.TrackID, 10),
					Data:       i,
				}
			}
			if best := similarity.FindBestSongMatch(simCandidates, title, artist, album, durationSec, songISRC, ""); best != nil {
				idx := best.Candidate.Data.(int)
				return &candidates[idx], nil
			}
		}
	}

	return nil, nil
}

func (p *MusixmatchProvider) fetchLyricsFromAPI(ctx context.Context, trackID int64, requireWordSync bool, accountIdx int) (json.RawMessage, error) {
	trackIDStr := strconv.FormatInt(trackID, 10)

	if p.state.richsyncCache != nil {
		if cachedRich, ok := p.state.richsyncCache.Get(trackID); ok && len(cachedRich) > 0 {
			return cachedRich, nil
		}
	}

	paramsRich := url.Values{"track_id": {trackIDStr}}
	richData, err := p.fetchRichsync(ctx, paramsRich, accountIdx)
	if err == nil && richData != nil {
		var check struct {
			Message struct {
				Body struct {
					Richsync *struct {
						RichsyncBody string `json:"richsync_body"`
					} `json:"richsync"`
				} `json:"body"`
			} `json:"message"`
		}
		if json.Unmarshal(richData, &check) == nil && check.Message.Body.Richsync != nil && check.Message.Body.Richsync.RichsyncBody != "" {
			if p.state.richsyncCache != nil {
				p.state.richsyncCache.Add(trackID, richData)
			}
			return richData, nil
		}
	}

	if requireWordSync {
		return nil, err
	}

	paramsSub := url.Values{
		"track_id":        {trackIDStr},
		"subtitle_format": {"lrc"},
	}
	return p.fetchSubtitle(ctx, paramsSub, accountIdx)
}

func (p *MusixmatchProvider) fetchRichsync(ctx context.Context, params url.Values, accountIdx int) (json.RawMessage, error) {
	if p.isAndroid(accountIdx) {
		return p.androidRequest(ctx, "track.richsync.get", params, nil, accountIdx)
	}
	userToken, err := p.getUserToken(ctx, accountIdx)
	if err != nil {
		return nil, err
	}
	params.Set("usertoken", userToken)
	return p.makeWebRequest(ctx, "track.richsync.get", params, accountIdx)
}

func (p *MusixmatchProvider) fetchSubtitle(ctx context.Context, params url.Values, accountIdx int) (json.RawMessage, error) {
	if p.isAndroid(accountIdx) {
		return p.androidRequest(ctx, "track.subtitle.get", params, nil, accountIdx)
	}
	userToken, err := p.getUserToken(ctx, accountIdx)
	if err != nil {
		return nil, err
	}
	params.Set("usertoken", userToken)
	return p.makeWebRequest(ctx, "track.subtitle.get", params, accountIdx)
}

func (p *MusixmatchProvider) androidState(accountIdx int) *mxmAndroidState {
	p.state.androidMu.Lock()
	defer p.state.androidMu.Unlock()
	st, ok := p.state.android[accountIdx]
	if !ok {
		st = &mxmAndroidState{}
		p.state.android[accountIdx] = st
	}
	return st
}

func newAndroidGuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func getApiSignature(endpoint string, dateTime time.Time) string {
	formattedDate := fmt.Sprintf("%04d%02d%02d",
		dateTime.UTC().Year(), int(dateTime.UTC().Month()), dateTime.UTC().Day())
	data := endpoint + formattedDate
	hm := hmac.New(sha1.New, []byte(mxmSigningKey))
	hm.Write([]byte(data))
	sig := base64.StdEncoding.EncodeToString(hm.Sum(nil))
	sig = strings.Replace(sig, "+", "-", -1)
	sig = strings.Replace(sig, "/", "_", -1)
	sig = strings.TrimRight(sig, "=")
	return sig
}

func buildAndroidSignedParams(endpoint string, params url.Values, currentToken string) url.Values {
	sig := getApiSignature(endpoint, time.Now())

	out := url.Values{}
	for k, v := range params {
		out[k] = v
	}
	out.Set("app_id", androidAppID)
	out.Set("usertoken", currentToken)
	out.Set("format", "json")
	out.Set("signature", sig)
	out.Set("signature_protocol", "sha1")

	if endpoint == "token.get" {
		out.Set("timestamp", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
		out.Set("guid", newAndroidGuid())
	}
	return out
}

func (p *MusixmatchProvider) makeAndroidRequest(ctx context.Context, endpoint string, params url.Values, body []byte) (json.RawMessage, error) {
	reqURL := mxmAndroidBaseURL + endpoint

	headers := make(http.Header)
	headers.Set("User-Agent", androidUserAgent)
	headers.Set("Connection", "Keep-Alive")
	headers.Set("Accept-Encoding", "gzip")
	headers.Set("x-mxm-endpoint", "default")
	headers.Set("Cookie", "x-mxm-token-guid="+newAndroidGuid()+"; mxm-encrypted-token=; x-mxm-user-id=; AWSELB=unknown")

	fullURL := reqURL + "?" + params.Encode()

	var resp *http.Response
	var err error
	if body != nil {
		headers.Set("Content-Type", "application/json")
		resp, err = p.client.Post(ctx, fullURL, headers, body)
	} else {
		resp, err = p.client.Get(ctx, fullURL, headers)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("musixmatch android request failed with status %d", resp.StatusCode)
	}
	return data, nil
}

func (p *MusixmatchProvider) initializeAndroid(ctx context.Context, accountIdx int) (string, error) {
	acc, err := p.account(accountIdx)
	if err != nil {
		return "", err
	}
	if acc.EMAIL == "" || acc.PASSWORD == "" {
		return "", fmt.Errorf("musixmatch: android account requires EMAIL and PASSWORD")
	}

	tokResp, err := p.fetchAndroidToken(ctx, accountIdx)
	if err != nil {
		return "", err
	}

	if err := p.androidLoginWithToken(ctx, acc.EMAIL, acc.PASSWORD, tokResp); err != nil {
		return "", err
	}
	return tokResp, nil
}

func emailLoginBody(email, password string) []byte {
	body := map[string]interface{}{
		"credential_list": []map[string]interface{}{
			{
				"credential": map[string]string{
					"type":     "mxm",
					"action":   "login",
					"email":    email,
					"password": password,
				},
			},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func (p *MusixmatchProvider) fetchAndroidToken(ctx context.Context, accountIdx int) (string, error) {
	params := url.Values{}
	signed := buildAndroidSignedParams("token.get", params, "")
	raw, err := p.makeAndroidRequest(ctx, "token.get", signed, nil)
	if err != nil {
		return "", err
	}
	var data struct {
		Message struct {
			Header struct {
				StatusCode int `json:"status_code"`
			} `json:"header"`
			Body struct {
				UserToken string `json:"user_token"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", err
	}
	if data.Message.Header.StatusCode != 200 {
		return "", fmt.Errorf("musixmatch: token request failed with status %d", data.Message.Header.StatusCode)
	}
	if data.Message.Body.UserToken == "" {
		return "", fmt.Errorf("musixmatch: no user token found")
	}
	return data.Message.Body.UserToken, nil
}

func createUserLoginBody(email, password string) []byte {
	return emailLoginBody(email, password)
}

func (p *MusixmatchProvider) androidLogin(ctx context.Context, email, password string, st *mxmAndroidState) error {
	if err := p.androidLoginWithToken(ctx, email, password, st.currentToken); err != nil {
		return err
	}
	st.isLoggedIn = true
	return nil
}

func (p *MusixmatchProvider) androidLoginWithToken(ctx context.Context, email, password, token string) error {
	params := buildAndroidSignedParams("credential.post", url.Values{}, token)
	body := emailLoginBody(email, password)
	raw, err := p.makeAndroidRequest(ctx, "credential.post", params, body)
	if err != nil {
		return err
	}
	var data struct {
		Message struct {
			Header struct {
				StatusCode int    `json:"status_code"`
				Hint       string `json:"hint"`
			} `json:"header"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	if data.Message.Header.StatusCode != 200 {
		return fmt.Errorf("musixmatch: login failed with status %d: %s", data.Message.Header.StatusCode, data.Message.Header.Hint)
	}
	return nil
}

func (p *MusixmatchProvider) ensureLoggedIn(ctx context.Context, accountIdx int) (string, error) {
	p.state.androidMu.Lock()
	st, ok := p.state.android[accountIdx]
	if !ok {
		st = &mxmAndroidState{}
		p.state.android[accountIdx] = st
	}
	now := time.Now()
	if st.currentToken != "" && st.isLoggedIn && st.expiresAt.After(now.Add(30*time.Second)) {
		tok := st.currentToken
		p.state.androidMu.Unlock()
		return tok, nil
	}
	if now.Before(st.failUntil) {
		p.state.androidMu.Unlock()
		return "", fmt.Errorf("musixmatch: android account in cooldown")
	}
	if st.initializing {
		waitCh := st.initDoneCh
		p.state.androidMu.Unlock()
		select {
		case <-waitCh:
			p.state.androidMu.Lock()
			tok := st.currentToken
			err := st.initErr
			p.state.androidMu.Unlock()
			return tok, err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	st.initializing = true
	doneCh := make(chan struct{})
	st.initDoneCh = doneCh
	st.initErr = nil
	p.state.androidMu.Unlock()

	initCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	tok, err := p.initializeAndroid(initCtx, accountIdx)
	cancel()

	p.state.androidMu.Lock()
	st.initializing = false
	st.initErr = err
	if err == nil && tok != "" {
		st.currentToken = tok
		st.isLoggedIn = true
		st.expiresAt = time.Now().Add(mxmTokenExpiry)
		st.failUntil = time.Time{}
	} else {
		st.currentToken = ""
		st.isLoggedIn = false
		st.failUntil = time.Now().Add(20 * time.Second)
	}
	close(doneCh)
	p.state.androidMu.Unlock()

	return tok, err
}

func (p *MusixmatchProvider) androidRequest(ctx context.Context, endpoint string, params url.Values, body []byte, accountIdx int) (json.RawMessage, error) {
	tok, err := p.ensureLoggedIn(ctx, accountIdx)
	if err != nil {
		return nil, err
	}

	signed := buildAndroidSignedParams(endpoint, params, tok)
	raw, err := p.makeAndroidRequest(ctx, endpoint, signed, body)
	if err == nil {
		var data struct {
			Message struct {
				Header struct {
					StatusCode int    `json:"status_code"`
					Hint       string `json:"hint"`
				} `json:"header"`
			} `json:"message"`
		}

		if json.Unmarshal(raw, &data) == nil {
			code := data.Message.Header.StatusCode
			if code == 200 {
				return raw, nil
			}
			if code == 404 {
				return nil, nil
			}
			if code != 401 && code != 402 {
				return nil, fmt.Errorf("musixmatch android error (%d): %s", code, data.Message.Header.Hint)
			}
		}
	}

	p.state.androidMu.Lock()
	if st, ok := p.state.android[accountIdx]; ok && st.currentToken == tok {
		st.currentToken = ""
		st.isLoggedIn = false
		st.expiresAt = time.Time{}
	}
	p.state.androidMu.Unlock()

	tok, err = p.ensureLoggedIn(ctx, accountIdx)
	if err != nil {
		return nil, err
	}
	signed = buildAndroidSignedParams(endpoint, params, tok)
	return p.makeAndroidRequest(ctx, endpoint, signed, body)
}

func Signature(endpoint string, now time.Time) string {
	return getApiSignature(endpoint, now)
}

func SignedURL(endpoint string) string {

	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}

	signTarget := endpoint
	if strings.HasPrefix(endpoint, mxmAndroidBaseURL) {
		signTarget = strings.TrimPrefix(endpoint, mxmAndroidBaseURL)
	}
	if i := strings.IndexByte(signTarget, '?'); i >= 0 {
		signTarget = signTarget[:i]
	}
	return endpoint + sep + "signature=" + Signature(signTarget, time.Now()) + "&signature_protocol=sha1"
}

func (p *MusixmatchProvider) NormalizeSong(track mxmTrack) domain.SongCatalogItem {
	var isrc *string
	if track.TrackISRC != "" {
		isrc = &track.TrackISRC
	}
	trackIDStr := strconv.FormatInt(track.TrackID, 10)

	return domain.SongCatalogItem{
		ID:           map[string]string{"musixmatch": trackIDStr},
		SourceID:     trackIDStr,
		Title:        track.TrackName,
		Artist:       track.ArtistName,
		Album:        track.AlbumName,
		DurationMs:   int64(track.TrackLength * 1000),
		ISRC:         isrc,
		Songwriters:  track.WriterList,
		Availability: []string{"Musixmatch"},
		ExternalURLs: map[string]string{
			"musixmatch": fmt.Sprintf("https://www.musixmatch.com/lyrics/%s/%s",
				url.QueryEscape(track.ArtistName), url.QueryEscape(track.TrackName)),
		},
	}
}

func (p *MusixmatchProvider) SearchCatalog(ctx context.Context, query string) ([]domain.SongCatalogItem, error) {
	tracks, err := p.SearchTrack(ctx, query)
	if err != nil {
		return nil, err
	}
	limit := len(tracks)
	if limit > 10 {
		limit = 10
	}
	items := make([]domain.SongCatalogItem, limit)
	for i := 0; i < limit; i++ {
		items[i] = p.NormalizeSong(tracks[i])
	}
	return items, nil
}

func (p *MusixmatchProvider) GetMetadata(ctx context.Context, title, artist, album string, durationSec float64) (map[string]interface{}, error) {
	query := strings.TrimSpace(title + " " + artist)
	if query == "" {
		query = title
	}
	tracks, err := p.SearchTrack(ctx, query)
	if err != nil || len(tracks) == 0 {
		return nil, err
	}
	var candidates []similarity.SongCandidate
	for _, t := range tracks {
		candidates = append(candidates, similarity.SongCandidate{
			Title:      t.TrackName,
			Artist:     t.ArtistName,
			Album:      t.AlbumName,
			DurationMs: t.TrackLength * 1000,
			ISRC:       t.TrackISRC,
			PlatformID: strconv.FormatInt(t.TrackID, 10),
			Data:       t,
		})
	}
	best := similarity.FindBestSongMatch(candidates, title, artist, album, durationSec, "", "")
	if best == nil {
		return nil, nil
	}
	matched := best.Candidate.Data.(mxmTrack)
	meta := map[string]interface{}{
		"name":        matched.TrackName,
		"artistName":  matched.ArtistName,
		"albumName":   matched.AlbumName,
		"durationMs":  int64(matched.TrackLength * 1000),
		"isrc":        matched.TrackISRC,
		"id":          strconv.FormatInt(matched.TrackID, 10),
		"songwriters": matched.WriterList,
		"source":      "Musixmatch",
	}
	return meta, nil
}
