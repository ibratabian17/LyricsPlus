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
	mxmTokenExpiry      = 10 * time.Minute
)

// MusixmatchProvider fetches RichSync or subtitle lyrics from Musixmatch.
// Accounts are rotated through on request failure, and each pass routes by the
// account AUTH_TYPE ("web" vs "android").
type MusixmatchProvider struct {
	client *proxy.Client
	name   string
	word   bool
	mgm    *AccountManager[config.MusixmatchAccount]
	logger *logger.Logger

	webMu     sync.RWMutex
	webTok    map[int]cachedWebToken
	androidMu sync.Mutex
	android   map[int]*mxmAndroidState
}

// cachedWebToken holds a fetched web user-token for an account index.
type cachedWebToken struct {
	token   string
	expires time.Time
}

// mxmAndroidState holds the per-account android client state.
type mxmAndroidState struct {
	currentToken string
	isLoggedIn   bool
	expiresAt    time.Time
	initializing bool
	initDoneCh   chan struct{}
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
	p := &MusixmatchProvider{
		client:  client,
		name:    pname,
		word:    word,
		mgm:     newAccountManager(accounts),
		webTok:  map[int]cachedWebToken{},
		android: map[int]*mxmAndroidState{},
	}
	return p
}

// SetLogger attaches a logger for debug output.
func (p *MusixmatchProvider) SetLogger(lg *logger.Logger) { p.logger = lg }

func (p *MusixmatchProvider) debugf(format string, args ...any) {
	if p.logger == nil {
		return
	}
	p.logger.Debugf(p.name+": "+format, args...)
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
	for accountIdx := 0; ; {
		res, err := p.fetchLyricsWithAccount(ctx, q, accountIdx)
		if err == nil {
			return res, nil
		}
		next, hasNext := p.mgm.Next(accountIdx)
		if !hasNext {
			break
		}
		accountIdx = next
	}
	return nil, nil
}

func (p *MusixmatchProvider) fetchLyricsWithAccount(ctx context.Context, q domain.SearchQuery, accountIdx int) (*domain.LyricsResponse, error) {
	var matchedTrack *mxmTrack

	if q.ISRC != "" {
		tr, err := p.searchMatcher(ctx, url.Values{"q_track_isrc": {q.ISRC}}, accountIdx)
		if err == nil && tr != nil && tr.TrackID != 0 {
			matchedTrack = tr
			p.debugf("ISRC search isrc:%s matched track %d", q.ISRC, tr.TrackID)
		} else if err != nil {
			p.debugf("ISRC search isrc:%s failed (err=%v)", q.ISRC, err)
		}
	}

	if matchedTrack == nil {
		if q.IDOnly() {
			p.debugf("id-only query, no track resolved")
			return nil, nil
		}
		durationSec := float64(q.Duration) / 1000.0
		matchedTrack, _ = p.searchBestMatch(ctx, q.Title, q.Artist, q.Album, durationSec, q.ISRC, accountIdx)
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

	if p.word && converted.Type != domain.SyncTypeWord {
		p.debugf("word-sync requested but track %d returned type %q", matchedTrack.TrackID, converted.Type)
		return nil, nil
	}
	p.debugf("lyrics parsed lines=%d word=%t track=%d", len(converted.Lyrics), p.word, matchedTrack.TrackID)

	converted.Metadata.Source = "Musixmatch"
	converted.Cached = domain.CacheNone
	converted.RawData = string(envelopeJSON)
	converted.ProcessingTime = &domain.ProcessTiming{
		SelectedSongMetadata: &domain.PickedSongMetadata{
			Source:         "Musixmatch",
			Title:          matchedTrack.TrackName,
			Artist:         matchedTrack.ArtistName,
			Album:          matchedTrack.AlbumName,
			SongISRC:       matchedTrack.TrackISRC,
			SongPlatformID: strconv.FormatInt(matchedTrack.TrackID, 10),
		},
	}
	return converted, nil
}

// mxmWriterList is the writer_list payload, which the API returns as an array
// of objects ({writer_name}) but can also be a plain array of strings.
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

// getUserToken returns a per-account web token from the cache, refreshed every
// hour. Android accounts have no web token.
func (p *MusixmatchProvider) getUserToken(ctx context.Context, accountIdx int) (string, error) {
	if p.isAndroid(accountIdx) {
		return "", nil
	}
	acc, err := p.account(accountIdx)
	if err != nil {
		return "", err
	}

	p.webMu.RLock()
	if t, ok := p.webTok[accountIdx]; ok && time.Now().Before(t.expires) {
		p.webMu.RUnlock()
		return t.token, nil
	}
	p.webMu.RUnlock()

	if tok := extractTokenFromCookie(acc.COOKIE); tok != "" {
		p.webMu.Lock()
		p.webTok[accountIdx] = cachedWebToken{token: tok, expires: time.Now().Add(time.Hour)}
		p.webMu.Unlock()
		return tok, nil
	}

	raw, err := p.webRawRequest(ctx, "token.get", nil, accountIdx)
	if err != nil {
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
	if tok == "" || strings.Contains(tok, "UpgradeOnly") {
		return "", fmt.Errorf("musixmatch: invalid user token")
	}

	p.webMu.Lock()
	p.webTok[accountIdx] = cachedWebToken{token: tok, expires: time.Now().Add(time.Hour)}
	p.webMu.Unlock()
	return tok, nil
}

// makeWebRequest issues a GET to the web API with the account's UA/Cookie and
// auth query params, failing when the HTTP call errors or the JSON header
// status_code is not 200.
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
		if statusCheck.Message.Header.StatusCode != 200 {
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
	// _makeWebRequest always injects app_id; usertoken only when available.
	params.Set("app_id", "web-desktop-app-v1.0")

	reqURL := fmt.Sprintf("%s/%s?%s", mxmWebBaseURL, strings.TrimPrefix(endpoint, "/"), params.Encode())

	headers := make(http.Header)
	headers.Set("authority", "apic-desktop.musixmatch.com")
	headers.Set("User-Agent", acc.USER_AGENT)
	headers.Set("Cookie", acc.COOKIE)
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

// webTrackSearch is the web flavor of track.search (used when the account is
// web AUTH_TYPE).
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
	if err != nil {
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

// searchMatcher is the advanced track search for both web and android.
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
		if err != nil {
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
	if err != nil {
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
	return p.searchTrack(ctx, query, 0)
}

func (p *MusixmatchProvider) searchTrack(ctx context.Context, query string, accountIdx int) ([]mxmTrack, error) {
	if !p.isAndroid(accountIdx) {
		return p.webTrackSearch(ctx, query, accountIdx)
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	params := url.Values{
		"q":                 {query},
		"part":              {"track_artist,artist_image"},
		"track_fields_set":  {"android_track_list"},
		"artist_fields_set": {"android_track_list_artist"},
		"page":              {"1"},
		"page_size":         {"5"},
	}
	raw, err := p.androidRequest(ctx, "macro.search", params, nil, accountIdx)
	if err != nil {
		return nil, err
	}

	var res struct {
		Message struct {
			Body struct {
				MacroResultList struct {
					TrackList []trackWrapper `json:"track_list"`
				} `json:"macro_result_list"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}

	out := make([]mxmTrack, len(res.Message.Body.MacroResultList.TrackList))
	for i, tw := range res.Message.Body.MacroResultList.TrackList {
		out[i] = tw.Track
	}
	return out, nil
}

func (p *MusixmatchProvider) SearchBestMatch(ctx context.Context, title, artist, album string, durationSec float64, songISRC string) (*mxmTrack, error) {
	return p.searchBestMatch(ctx, title, artist, album, durationSec, songISRC, 0)
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
		if err == nil && len(tracks) > 0 {
			if songISRC != "" {
				for _, tr := range tracks {
					if strings.EqualFold(tr.TrackISRC, songISRC) {
						return &tr, nil
					}
				}
			}
			candidates = append(candidates, tracks...)
		}
	}

	if len(candidates) == 0 {
		return nil, nil
	}

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

	best := similarity.FindBestSongMatch(simCandidates, title, artist, album, durationSec, songISRC, "")
	if best == nil {
		return nil, nil
	}

	idx := best.Candidate.Data.(int)
	return &candidates[idx], nil
}

func (p *MusixmatchProvider) fetchLyricsFromAPI(ctx context.Context, trackID int64, requireWordSync bool, accountIdx int) (json.RawMessage, error) {
	trackIDStr := strconv.FormatInt(trackID, 10)

	if requireWordSync {
		params := url.Values{"track_id": {trackIDStr}}
		return p.fetchRichsync(ctx, params, accountIdx)
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
			return richData, nil
		}
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

// androidState returns (creating if needed) the client state for an account.
func (p *MusixmatchProvider) androidState(accountIdx int) *mxmAndroidState {
	p.androidMu.Lock()
	defer p.androidMu.Unlock()
	st, ok := p.android[accountIdx]
	if !ok {
		st = &mxmAndroidState{}
		p.android[accountIdx] = st
	}
	return st
}

// newAndroidGuid returns a 32-hex-char GUID (vector of hex like a UUID sans
// dashes).
func newAndroidGuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// getApiSignature computes a day-scoped HMAC-SHA1 signature.
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

// buildAndroidSignedParams returns params enriched with the app signature,
// and (for token.get) a timestamp and fresh GUID.
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

// makeAndroidRequest performs the raw android HTTP call (GET or POST).
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

// initializeAndroid fetches a fresh android token, then logs in via
// credential.post.
func (p *MusixmatchProvider) initializeAndroid(ctx context.Context, accountIdx int, st *mxmAndroidState) error {
	acc, err := p.account(accountIdx)
	if err != nil {
		return err
	}
	if acc.EMAIL == "" || acc.PASSWORD == "" {
		return fmt.Errorf("musixmatch: android account requires EMAIL and PASSWORD")
	}

	tokResp, err := p.fetchAndroidToken(ctx, accountIdx)
	if err != nil {
		return err
	}

	st.currentToken = tokResp
	st.expiresAt = time.Now().Add(mxmTokenExpiry)
	st.isLoggedIn = false

	// Fresh tokens require a credential.post login (loginNeeded=true).
	return p.androidLogin(ctx, acc.EMAIL, acc.PASSWORD, st)
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

// createUserLoginBody builds the credential.post payload for email/password login.
func createUserLoginBody(email, password string) []byte {
	return emailLoginBody(email, password)
}

// androidLogin runs credential.post and marks the state logged in.
func (p *MusixmatchProvider) androidLogin(ctx context.Context, email, password string, st *mxmAndroidState) error {
	params := buildAndroidSignedParams("credential.post", url.Values{}, st.currentToken)
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
	st.isLoggedIn = true
	return nil
}

// ensureLoggedIn initializes (and logs in) the android client state once.
func (p *MusixmatchProvider) ensureLoggedIn(ctx context.Context, accountIdx int) error {
	st := p.androidState(accountIdx)

	if st.currentToken != "" && st.isLoggedIn && st.expiresAt.After(time.Now().Add(30*time.Second)) {
		return nil
	}

	st.initializing = true
	err := p.initializeAndroid(ctx, accountIdx, st)
	st.initializing = false
	return err
}

// androidRequest signs params with the current token, makes the call, and
// refreshes + retries once on 401.
func (p *MusixmatchProvider) androidRequest(ctx context.Context, endpoint string, params url.Values, body []byte, accountIdx int) (json.RawMessage, error) {
	if err := p.ensureLoggedIn(ctx, accountIdx); err != nil {
		return nil, err
	}
	st := p.androidState(accountIdx)

	signed := buildAndroidSignedParams(endpoint, params, st.currentToken)
	raw, err := p.makeAndroidRequest(ctx, endpoint, signed, body)
	if err == nil {
		var data struct {
			Message struct {
				Header struct {
					StatusCode int `json:"status_code"`
				} `json:"header"`
			} `json:"message"`
		}
		// A 401 status_code means the token expired and we must re-login.
		if json.Unmarshal(raw, &data) == nil && data.Message.Header.StatusCode != 401 {
			return raw, nil
		}
	}

	// Token expired or invalid (401): refresh and retry once.
	if err := p.ensureLoggedIn(ctx, accountIdx); err != nil {
		return nil, err
	}
	signed = buildAndroidSignedParams(endpoint, params, st.currentToken)
	return p.makeAndroidRequest(ctx, endpoint, signed, body)
}

// Signature computes base64url(HMAC-SHA1(key, endpoint+YYYYMMDD)) for Android.
func Signature(endpoint string, now time.Time) string {
	return getApiSignature(endpoint, now)
}

// SignedURL appends the required signature query parameters for Android.
func SignedURL(endpoint string) string {
	// Endpoint includes a query string already (e.g. ".../track.richsync.get?track_id=1").
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	// Sign the endpoint after the Android base URL (e.g. "track.richsync.get").
	signTarget := endpoint
	if strings.HasPrefix(endpoint, mxmAndroidBaseURL) {
		signTarget = strings.TrimPrefix(endpoint, mxmAndroidBaseURL)
	}
	if i := strings.IndexByte(signTarget, '?'); i >= 0 {
		signTarget = signTarget[:i]
	}
	return endpoint + sep + "signature=" + Signature(signTarget, time.Now()) + "&signature_protocol=sha1"
}

// NormalizeSong converts a Musixmatch Track into domain.SongCatalogItem.
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

// SearchCatalog searches Musixmatch tracks and normalizes the top 10 results.
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
