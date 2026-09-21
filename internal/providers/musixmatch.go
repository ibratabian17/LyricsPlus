package providers

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
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
)

// MusixmatchProvider fetches RichSync or subtitle lyrics from Musixmatch.
type MusixmatchProvider struct {
	client    *proxy.Client
	name      string
	word      bool
	token     string
	userAgent string

	mu           sync.RWMutex
	cachedToken  string
	tokenExpires time.Time
}

func NewMusixmatch(client *proxy.Client) *MusixmatchProvider {
	return NewMusixmatchWithConfig(client, config.Load().Provider, "musixmatch", false)
}

func NewMusixmatchWordSync(client *proxy.Client) *MusixmatchProvider {
	return NewMusixmatchWithConfig(client, config.Load().Provider, "musixmatch-word", true)
}

func NewMusixmatchWithConfig(client *proxy.Client, cfg config.Provider, pname string, word bool) *MusixmatchProvider {
	token := cfg.MusixmatchCookie
	if token == "" {
		token = mxmDefaultWebCookie
	}
	ua := cfg.MusixmatchUserAgent
	if ua == "" {
		ua = mxmDefaultUserAgent
	}
	p := &MusixmatchProvider{
		client:    client,
		name:      pname,
		word:      word,
		token:     token,
		userAgent: ua,
	}
	if tok := extractTokenFromCookie(token); tok != "" {
		p.cachedToken = tok
		p.tokenExpires = time.Now().Add(24 * time.Hour)
	}
	return p
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

func (p *MusixmatchProvider) Name() string     { return p.name }
func (p *MusixmatchProvider) Configured() bool { return true }

func (p *MusixmatchProvider) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	var matchedTrack *mxmTrack

	if q.ISRC != "" {
		tr, err := p.SearchByISRC(ctx, q.ISRC)
		if err == nil && tr != nil {
			matchedTrack = tr
		}
	}

	if matchedTrack == nil {
		if q.IDOnly() {
			return nil, nil
		}
		durationSec := float64(q.Duration) / 1000.0
		matchedTrack, _ = p.SearchBestMatch(ctx, q.Title, q.Artist, q.Album, durationSec, q.ISRC)
	}

	if matchedTrack == nil {
		return nil, nil
	}

	lyricsEnv, err := p.fetchLyricsFromAPI(ctx, matchedTrack.TrackID, p.word)
	if err != nil || lyricsEnv == nil {
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
		return nil, nil
	}

	if p.word && converted.Type != domain.SyncTypeWord {
		return nil, nil
	}

	converted.Cached = domain.CacheNone
	converted.RawData = string(envelopeJSON)
	return converted, nil
}

type mxmTrack struct {
	TrackID     int64    `json:"track_id"`
	TrackName   string   `json:"track_name"`
	ArtistName  string   `json:"artist_name"`
	AlbumName   string   `json:"album_name"`
	TrackLength int      `json:"track_length"`
	TrackISRC   string   `json:"track_isrc"`
	WriterList  []string `json:"writer_list,omitempty"`
}

type trackWrapper struct {
	Track mxmTrack `json:"track"`
}

func (p *MusixmatchProvider) getUserToken(ctx context.Context) (string, error) {
	p.mu.RLock()
	if p.cachedToken != "" && time.Now().Before(p.tokenExpires) {
		tok := p.cachedToken
		p.mu.RUnlock()
		return tok, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cachedToken != "" && time.Now().Before(p.tokenExpires) {
		return p.cachedToken, nil
	}

	reqURL := fmt.Sprintf("%s/token.get?app_id=web-desktop-app-v1.0", mxmWebBaseURL)
	headers := make(http.Header)
	headers.Set("authority", "apic-desktop.musixmatch.com")
	headers.Set("User-Agent", p.userAgent)
	headers.Set("Cookie", p.token)
	headers.Set("Origin", "https://musixmatch.com")

	resp, err := p.client.Get(ctx, reqURL, headers)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var res struct {
		Message struct {
			Header struct {
				StatusCode int `json:"status_code"`
			} `json:"header"`
			Body struct {
				UserToken string `json:"user_token"`
			} `json:"body"`
		} `json:"message"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	tok := res.Message.Body.UserToken
	if tok == "" || strings.Contains(tok, "UpgradeOnly") {
		return "", fmt.Errorf("musixmatch: invalid user token")
	}

	p.cachedToken = tok
	p.tokenExpires = time.Now().Add(50 * time.Minute)
	return tok, nil
}

func (p *MusixmatchProvider) makeWebRequest(ctx context.Context, endpoint string, params url.Values) (json.RawMessage, error) {
	token, err := p.getUserToken(ctx)
	if err != nil {
		return nil, err
	}

	if params == nil {
		params = url.Values{}
	}
	params.Set("app_id", "web-desktop-app-v1.0")
	params.Set("usertoken", token)

	reqURL := fmt.Sprintf("%s/%s?%s", mxmWebBaseURL, strings.TrimPrefix(endpoint, "/"), params.Encode())

	headers := make(http.Header)
	headers.Set("authority", "apic-desktop.musixmatch.com")
	headers.Set("User-Agent", p.userAgent)
	headers.Set("Cookie", p.token)
	headers.Set("Origin", "https://musixmatch.com")

	resp, err := p.client.Get(ctx, reqURL, headers)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
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
	if err := json.Unmarshal(body, &statusCheck); err == nil {
		if statusCheck.Message.Header.StatusCode != 200 && statusCheck.Message.Header.StatusCode != 0 {
			return nil, fmt.Errorf("musixmatch web error (%d): %s",
				statusCheck.Message.Header.StatusCode, statusCheck.Message.Header.Hint)
		}
	}

	return body, nil
}

func (p *MusixmatchProvider) SearchByISRC(ctx context.Context, isrc string) (*mxmTrack, error) {
	vals := url.Values{}
	vals.Set("q_track_isrc", isrc)

	raw, err := p.makeWebRequest(ctx, "matcher.track.get", vals)
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
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	vals := url.Values{}
	vals.Set("page_size", "5")
	vals.Set("f_has_lyrics", "true")
	vals.Set("page", "1")
	vals.Set("q", query)

	raw, err := p.makeWebRequest(ctx, "track.search", vals)
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

func (p *MusixmatchProvider) SearchBestMatch(ctx context.Context, title, artist, album string, durationSec float64, songISRC string) (*mxmTrack, error) {
	queries := []string{
		strings.TrimSpace(title + " " + artist),
		strings.TrimSpace(title),
	}

	var candidates []mxmTrack
	for _, q := range queries {
		if q == "" {
			continue
		}
		tracks, err := p.SearchTrack(ctx, q)
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

func (p *MusixmatchProvider) fetchLyricsFromAPI(ctx context.Context, trackID int64, requireWordSync bool) (json.RawMessage, error) {
	trackIDStr := strconv.FormatInt(trackID, 10)

	if requireWordSync {
		vals := url.Values{}
		vals.Set("track_id", trackIDStr)
		return p.makeWebRequest(ctx, "track.richsync.get", vals)
	}

	// Try richsync first
	valsRich := url.Values{}
	valsRich.Set("track_id", trackIDStr)
	richData, err := p.makeWebRequest(ctx, "track.richsync.get", valsRich)
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

	// Fallback to subtitle
	valsSub := url.Values{}
	valsSub.Set("track_id", trackIDStr)
	valsSub.Set("subtitle_format", "dfxp")
	return p.makeWebRequest(ctx, "track.subtitle.get", valsSub)
}

// Signature computes base64url(HMAC-SHA1(key, endpoint+YYYYMMDD)) for Android.
func Signature(endpoint string, now time.Time) string {
	data := endpoint + now.Format("20060102")
	mac := hmac.New(sha1.New, []byte(mxmSigningKey))
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// SignedURL appends the required signature query parameters for Android.
func SignedURL(endpoint string) string {
	sig := Signature(endpoint, time.Now())
	return endpoint + "&signature=" + sig + "&signature_protocol=sha1"
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
