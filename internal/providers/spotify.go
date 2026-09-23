package providers

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
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

const spotifyName = "spotify"

const (
	spotifyBaseURL          = "https://api.spotify.com/v1"
	spotifyLyricsURL        = "https://spclient.wg.spotify.com/color-lyrics/v2/track/"
	spotifyAuthURL          = "https://accounts.spotify.com/api/token"
	spotifyDefaultSecretURL = "https://raw.githubusercontent.com/Thereallo1026/spotify-secrets/main/secrets/secretDict.json"
	spotifyDefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
)

var spotifyUserAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:126.0) Gecko/20100101 Firefox/126.0",
}

// getRandomSpotifyUserAgent returns a random entry from the parity UA list.
func getRandomSpotifyUserAgent() string {
	return spotifyUserAgents[rand.Intn(len(spotifyUserAgents))]
}

var fallbackSecrets = map[string][]int{
	"14": {62, 54, 109, 83, 107, 77, 41, 103, 45, 93, 114, 38, 41, 97, 64, 51, 95, 94, 95, 94},
	"13": {59, 92, 64, 70, 99, 78, 117, 75, 99, 103, 116, 67, 103, 51, 87, 63, 93, 59, 70, 45, 32},
	"12": {107, 81, 49, 57, 67, 93, 87, 81, 69, 67, 40, 93, 48, 50, 46, 91, 94, 113, 41, 108, 77, 107, 34},
}

// BestSecret returns the highest version fallback secret.
func BestSecret() []int {
	return fallbackSecrets["14"]
}

// SpotifyProvider fetches color lyrics from Spotify, rotating through a list
// of accounts on 401/429.
type SpotifyProvider struct {
	client *proxy.Client
	mgm    *AccountManager[config.SpotifyAccount]
	logger *logger.Logger

	webTokMu sync.Mutex
	webTok   map[int]*spotifyCachedToken
	apiTokMu sync.Mutex
	apiTok   map[int]*spotifyCachedToken

	secretsMu   sync.Mutex
	secretsDict map[string][]int
	secretsTime time.Time
}

type spotifyCachedToken struct {
	token   string
	expires time.Time
}

func NewSpotify(client *proxy.Client) *SpotifyProvider {
	return NewSpotifyWithConfig(client, config.Load().Provider)
}

func NewSpotifyWithConfig(client *proxy.Client, cfg config.Provider) *SpotifyProvider {
	accounts := cfg.SpotifyAccounts
	if len(accounts) == 0 {
		accounts = []config.SpotifyAccount{{
			NAMEID:        "SpotifyDefault",
			CLIENT_ID:     cfg.SpotifyClientID,
			CLIENT_SECRET: cfg.SpotifyClientSecret,
			COOKIE:        normalizeCookie(cfg.SpotifySpotifyDCCookie),
		}}
	}
	for i := range accounts {
		accounts[i].COOKIE = normalizeCookie(accounts[i].COOKIE)
	}
	return &SpotifyProvider{
		client:      client,
		mgm:         newAccountManager(accounts),
		webTok:      map[int]*spotifyCachedToken{},
		apiTok:      map[int]*spotifyCachedToken{},
		secretsDict: fallbackSecrets,
	}
}

// SetLogger attaches a logger for debug output.
func (p *SpotifyProvider) SetLogger(lg *logger.Logger) { p.logger = lg }

func (p *SpotifyProvider) debugf(format string, args ...any) {
	if p.logger == nil {
		return
	}
	p.logger.Debugf("spotify: "+format, args...)
}

func normalizeCookie(cookie string) string {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return ""
	}
	if !strings.HasPrefix(cookie, "sp_dc=") && !strings.Contains(cookie, ";") {
		return "sp_dc=" + cookie
	}
	return cookie
}

func (p *SpotifyProvider) Name() string { return spotifyName }

func (p *SpotifyProvider) Configured() bool {
	for i := 0; i < p.mgm.Count(); i++ {
		if acc, ok := p.mgm.At(i); ok && acc.IsConfigured() {
			return true
		}
	}
	return false
}

func (p *SpotifyProvider) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	if !p.Configured() {
		return nil, nil
	}

	trackID := q.PlatformID
	songTitle := q.Title
	songArtist := q.Artist
	songAlbum := q.Album
	songISRC := q.ISRC

	if trackID == "" {
		var tracks []spotifyTrack
		if q.ISRC != "" {
			var err error
			tracks, err = p.SearchTrack(ctx, "isrc:"+q.ISRC)
			if err != nil || len(tracks) == 0 {
				p.debugf("no ISRC match for isrc:%s (err=%v)", q.ISRC, err)
				tracks = nil
			} else {
				p.debugf("ISRC search isrc:%s returned %d tracks", q.ISRC, len(tracks))
			}
		}
		if len(tracks) == 0 {
			if q.IDOnly() {
				p.debugf("id-only query, no track resolved")
				return nil, nil
			}
			searchQ := q.Title + " artist:" + q.Artist
			var err error
			tracks, err = p.SearchTrack(ctx, searchQ)
			if err != nil || len(tracks) == 0 {
				p.debugf("search %q returned no tracks (err=%v)", searchQ, err)
				return nil, nil
			}
			p.debugf("search %q returned %d tracks", searchQ, len(tracks))
		}

		candidates := make([]similarity.SongCandidate, len(tracks))
		for i, t := range tracks {
			var artistNames []string
			for _, a := range t.Artists {
				artistNames = append(artistNames, a.Name)
			}
			candidates[i] = similarity.SongCandidate{
				Title:      t.Name,
				Artist:     strings.Join(artistNames, ", "),
				Album:      t.Album.Name,
				DurationMs: t.DurationMs,
				ISRC:       t.ExternalIDs.ISRC,
				PlatformID: t.ID,
				Data:       t,
			}
		}

		durationSec := float64(q.Duration) / 1000.0
		best := similarity.FindBestSongMatch(candidates, q.Title, q.Artist, q.Album, durationSec, q.ISRC, q.PlatformID)
		if best == nil {
			p.debugf("no similarity match among %d candidates for %q / %q", len(candidates), q.Title, q.Artist)
			return nil, nil
		}
		p.debugf("matched track %q id=%s (score match)", best.Candidate.Title, best.Candidate.PlatformID)
		trackID = best.Candidate.PlatformID
		if best.Candidate.Title != "" {
			songTitle = best.Candidate.Title
		}
		if best.Candidate.Artist != "" {
			songArtist = best.Candidate.Artist
		}
		if best.Candidate.Album != "" {
			songAlbum = best.Candidate.Album
		}
		if best.Candidate.ISRC != "" {
			songISRC = best.Candidate.ISRC
		}
	}

	if trackID == "" {
		return nil, nil
	}

	lyricsJSON, err := p.fetchColorLyrics(ctx, trackID)
	if err != nil || lyricsJSON == nil {
		p.debugf("color lyrics fetch failed for track %s (err=%v)", trackID, err)
		return nil, err
	}

	converted, err := parsers.ConvertSpotifyToJSON(lyricsJSON)
	if err != nil || converted == nil || len(converted.Lyrics) == 0 {
		p.debugf("no parseable lyrics for track %s (err=%v)", trackID, err)
		return nil, nil
	}
	p.debugf("lyrics parsed lines=%d track=%s", len(converted.Lyrics), trackID)

	if songWriters, werr := p.fetchSpotifySongwriters(ctx, trackID); werr == nil {
		converted.Metadata.SongWriters = songWriters
	} else {
		converted.Metadata.SongWriters = []string{}
	}

	if converted.Metadata.Source == "" {
		converted.Metadata.Source = "Spotify"
	}
	converted.Cached = domain.CacheNone
	converted.RawData = string(lyricsJSON)
	converted.ProcessingTime = &domain.ProcessTiming{
		SelectedSongMetadata: &domain.PickedSongMetadata{
			Source:         "Spotify",
			Title:          songTitle,
			Artist:         songArtist,
			Album:          songAlbum,
			SongISRC:       songISRC,
			SongPlatformID: trackID,
		},
	}
	return converted, nil
}

type spotifyTrack struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	DurationMs int    `json:"duration_ms"`
	Album      struct {
		Name   string `json:"name"`
		Images []struct {
			URL string `json:"url"`
		} `json:"images"`
	} `json:"album"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	ExternalIDs struct {
		ISRC string `json:"isrc"`
	} `json:"external_ids"`
	ExternalURLs struct {
		Spotify string `json:"spotify"`
	} `json:"external_urls"`
}

func (p *SpotifyProvider) getWebToken(ctx context.Context, accountIdx int) (string, error) {
	p.webTokMu.Lock()
	if tok, ok := p.webTok[accountIdx]; ok && time.Now().Before(tok.expires) {
		p.webTokMu.Unlock()
		return tok.token, nil
	}
	p.webTokMu.Unlock()

	acc, ok := p.mgm.At(accountIdx)
	if !ok || acc.COOKIE == "" {
		return "", fmt.Errorf("spotify: no web-token account available")
	}
	cookie := normalizeCookie(acc.COOKIE)

	totp, ver, err := p.generateTOTP(ctx)
	if err != nil {
		return "", err
	}

	headers := make(http.Header)
	headers.Set("Cookie", cookie)
	headers.Set("User-Agent", getRandomSpotifyUserAgent())
	headers.Set("app-platform", "WebPlayer")
	headers.Set("Referer", "https://open.spotify.com/")

	tokenEndpoint := fmt.Sprintf("https://open.spotify.com/api/token?reason=transport&productType=web-player&totp=%s&totpServer=%s&totpVer=%s",
		url.QueryEscape(totp), url.QueryEscape(totp), url.QueryEscape(strconv.Itoa(ver)))

	resp, err := p.client.Get(ctx, tokenEndpoint, headers)
	if err != nil || (resp != nil && resp.StatusCode != http.StatusOK) {
		if resp != nil {
			_ = resp.Body.Close()
		}
		// Retry with reason=init
		initEndpoint := fmt.Sprintf("https://open.spotify.com/api/token?reason=init&productType=web-player&totp=%s&totpServer=%s&totpVer=%s",
			url.QueryEscape(totp), url.QueryEscape(totp), url.QueryEscape(strconv.Itoa(ver)))
		resp, err = p.client.Get(ctx, initEndpoint, headers)
		if err != nil {
			return "", err
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("spotify web token error %d: %s", resp.StatusCode, string(b))
	}

	var res struct {
		ClientID                string `json:"clientId"`
		AccessToken             string `json:"accessToken"`
		AccessTokenExpirationMs int64  `json:"accessTokenExpirationTimestampMs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("spotify: empty access token")
	}

	tok := &spotifyCachedToken{token: res.AccessToken}
	if res.AccessTokenExpirationMs > 0 {
		tok.expires = time.UnixMilli(res.AccessTokenExpirationMs)
	} else {
		tok.expires = time.Now().Add(50 * time.Minute)
	}

	p.webTokMu.Lock()
	p.webTok[accountIdx] = tok
	p.webTokMu.Unlock()

	return tok.token, nil
}

func (p *SpotifyProvider) getAPIToken(ctx context.Context, accountIdx int) (string, error) {
	acc, ok := p.mgm.At(accountIdx)
	if !ok {
		return "", fmt.Errorf("spotify: no account available")
	}
	// Without CLIENT_ID/SECRET fall back to the web token flow (which also
	// works for lyrics via API search results).
	if acc.CLIENT_ID == "" || acc.CLIENT_SECRET == "" {
		return p.getWebToken(ctx, accountIdx)
	}

	p.apiTokMu.Lock()
	if tok, ok := p.apiTok[accountIdx]; ok && time.Now().Before(tok.expires) {
		p.apiTokMu.Unlock()
		return tok.token, nil
	}
	p.apiTokMu.Unlock()

	authBasic := base64.StdEncoding.EncodeToString([]byte(acc.CLIENT_ID + ":" + acc.CLIENT_SECRET))
	headers := make(http.Header)
	headers.Set("Authorization", "Basic "+authBasic)
	headers.Set("Content-Type", "application/x-www-form-urlencoded")

	body := []byte("grant_type=client_credentials")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spotifyAuthURL, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	for k, vv := range headers {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("spotify client credentials returned no token")
	}

	tok := &spotifyCachedToken{token: res.AccessToken, expires: time.Now().Add(time.Duration(res.ExpiresIn-60) * time.Second)}

	p.apiTokMu.Lock()
	p.apiTok[accountIdx] = tok
	p.apiTokMu.Unlock()
	return tok.token, nil
}

// doSpotifyRequest picks the current account, injects the right auth header
// for the URL kind, rotates to the next account on 401/429 (up to
// maxAccountRetries), and surfaces a structured error.
func (p *SpotifyProvider) doSpotifyRequest(ctx context.Context, urlstr string, extra http.Header, retries, accountIdx int) (*http.Response, error) {
	acc, ok := p.mgm.At(accountIdx)
	if !ok {
		return nil, fmt.Errorf("no Spotify account available")
	}

	headers := make(http.Header)
	for k, vv := range extra {
		for _, v := range vv {
			headers.Add(k, v)
		}
	}
	headers.Set("User-Agent", getRandomSpotifyUserAgent())

	switch {
	case strings.HasPrefix(urlstr, spotifyLyricsURL) || strings.Contains(urlstr, "track-credits-view"):
		if headers.Get("Authorization") == "" {
			token, err := p.getWebToken(ctx, accountIdx)
			if err != nil {
				return nil, err
			}
			headers.Set("Authorization", "Bearer "+token)
		}
		if headers.Get("Cookie") == "" {
			headers.Set("Cookie", normalizeCookie(acc.COOKIE))
		}
		headers.Set("app-platform", "WebPlayer")
	case strings.HasPrefix(urlstr, spotifyAuthURL) || strings.HasPrefix(urlstr, spotifyBaseURL):
		if headers.Get("Authorization") == "" {
			token, err := p.getAPIToken(ctx, accountIdx)
			if err != nil {
				return nil, err
			}
			headers.Set("Authorization", "Bearer "+token)
		}
	}

	var resp *http.Response
	var err error
	if strings.HasPrefix(urlstr, spotifyAuthURL) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, urlstr, strings.NewReader("grant_type=client_credentials"))
		if rerr != nil {
			return nil, rerr
		}
		for k, vv := range headers {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
		resp, err = p.client.Do(req)
	} else {
		resp, err = p.client.Get(ctx, urlstr, headers)
	}
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
		errMsg, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		message := parseSpotifyErrorMessage(errMsg)
		if retries < maxAccountRetries {
			if next, hasNext := p.mgm.Next(accountIdx); hasNext {
				return p.doSpotifyRequest(ctx, urlstr, extra, retries+1, next)
			}
		}
		return nil, fmt.Errorf("Spotify API returned status %d: %s", resp.StatusCode, message)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("Spotify API returned status %d: %s", resp.StatusCode, parseSpotifyErrorMessage(body))
	}

	return resp, nil
}

// parseSpotifyErrorMessage extracts the error message from a Spotify response:
// error.message -> error_description -> error -> raw text.
func parseSpotifyErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if payload.Error.Message != "" {
			return payload.Error.Message
		}
		if payload.ErrorDescription != "" {
			return payload.ErrorDescription
		}
	}
	var generic map[string]json.RawMessage
	if json.Unmarshal(body, &generic) == nil {
		if raw, ok := generic["error"]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return s
			}
		}
	}
	return string(body)
}

func (p *SpotifyProvider) SearchTrack(ctx context.Context, query string) ([]spotifyTrack, error) {
	searchURL := fmt.Sprintf("%s/search?q=%s&type=track&limit=10", spotifyBaseURL, url.QueryEscape(query))
	resp, err := p.doSpotifyRequest(ctx, searchURL, nil, 0, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var res struct {
		Tracks struct {
			Items []spotifyTrack `json:"items"`
		} `json:"tracks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return res.Tracks.Items, nil
}

func (p *SpotifyProvider) fetchColorLyrics(ctx context.Context, trackID string) ([]byte, error) {
	reqURL := fmt.Sprintf("%s%s?format=json&vocalRemoval=false&market=from_token", spotifyLyricsURL, trackID)
	resp, err := p.doSpotifyRequest(ctx, reqURL, nil, 0, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	return io.ReadAll(resp.Body)
}

// fetchSpotifySongwriters fetches credited writers for a track via the
// track-credits view, overriding the converted songWriters list.
func (p *SpotifyProvider) fetchSpotifySongwriters(ctx context.Context, trackID string) ([]string, error) {
	reqURL := fmt.Sprintf("https://spclient.wg.spotify.com/track-credits-view/v0/experimental/%s/credits", trackID)
	resp, err := p.doSpotifyRequest(ctx, reqURL, nil, 0, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var data struct {
		RoleCredits []struct {
			RoleTitle string `json:"roleTitle"`
			Artists   []struct {
				Name string `json:"name"`
			} `json:"artists"`
		} `json:"roleCredits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	for _, role := range data.RoleCredits {
		if strings.EqualFold(role.RoleTitle, "writers") {
			var writers []string
			for _, a := range role.Artists {
				writers = append(writers, a.Name)
			}
			return writers, nil
		}
	}
	return nil, nil
}

func (p *SpotifyProvider) generateTOTP(ctx context.Context) (string, int, error) {
	secrets := p.getSecrets(ctx)
	maxVer := 0
	for k := range secrets {
		if v, err := strconv.Atoi(k); err == nil && v > maxVer {
			maxVer = v
		}
	}
	if maxVer == 0 {
		return "", 0, fmt.Errorf("no spotify secrets found")
	}

	cipherBytes := secrets[strconv.Itoa(maxVer)]
	if len(cipherBytes) == 0 {
		return "", 0, fmt.Errorf("secret for version %d empty", maxVer)
	}

	secretBytes := DeriveSecretBytes(cipherBytes)
	serverTime, err := p.getServerTime(ctx)
	if err != nil {
		return "", 0, err
	}
	totp := GenerateTOTP(secretBytes, serverTime.Unix(), 6, 30)
	return totp, maxVer, nil
}

func (p *SpotifyProvider) getServerTime(ctx context.Context) (time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://open.spotify.com/", nil)
	if err == nil {
		req.Header.Set("User-Agent", spotifyDefaultUserAgent)
		resp, err := p.client.Do(req)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusOK {
				dateHeader := resp.Header.Get("Date")
				if dateHeader != "" {
					if t, err := http.ParseTime(dateHeader); err == nil {
						return t, nil
					}
				}
			}
		}
	}
	return time.Time{}, fmt.Errorf("failed to fetch spotify server time")
}

func (p *SpotifyProvider) getSecrets(ctx context.Context) map[string][]int {
	p.secretsMu.Lock()
	defer p.secretsMu.Unlock()
	if time.Since(p.secretsTime) < 4*time.Hour && len(p.secretsDict) > 0 {
		return p.secretsDict
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spotifyDefaultSecretURL, nil)
	if err == nil {
		resp, err := p.client.Do(req)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusOK {
				var dict map[string][]int
				if json.NewDecoder(resp.Body).Decode(&dict) == nil && len(dict) > 0 {
					p.secretsDict = dict
					p.secretsTime = time.Now()
					return dict
				}
			}
		}
	}

	// On failure keep the last-good dict untouched, without stamping the time,
	// so the next call retries the fetch instead of suppressing it for 4h.
	return p.secretsDict
}

// DeriveSecretBytes transforms cipher bytes:
// transformed[t] = e XOR ((t mod 33) + 9), stringified digits concatenated.
func DeriveSecretBytes(cipherBytes []int) []byte {
	var transformed []byte
	for t, e := range cipherBytes {
		val := e ^ ((t % 33) + 9)
		transformed = append(transformed, strconv.Itoa(val)...)
	}
	return transformed
}

// GenerateTOTP computes an RFC 6238 HMAC-SHA1 OTP.
func GenerateTOTP(secretBytes []byte, timestamp int64, digits int, interval int64) string {
	counter := uint64(timestamp / interval)
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, counter)

	mac := hmac.New(sha1.New, secretBytes)
	mac.Write(buf)
	h := mac.Sum(nil)

	offset := h[len(h)-1] & 0x0f
	binaryCode := binary.BigEndian.Uint32(h[offset:offset+4]) & 0x7fffffff
	otp := binaryCode % uint32(math.Pow10(digits))

	return fmt.Sprintf("%0*d", digits, otp)
}

// NormalizeSong converts a Spotify Track into domain.SongCatalogItem.
func (p *SpotifyProvider) NormalizeSong(track spotifyTrack) domain.SongCatalogItem {
	var artistNames []string
	for _, a := range track.Artists {
		artistNames = append(artistNames, a.Name)
	}

	var artURL *string
	if len(track.Album.Images) > 0 {
		artURL = &track.Album.Images[0].URL
	}
	var isrc *string
	if track.ExternalIDs.ISRC != "" {
		isrc = &track.ExternalIDs.ISRC
	}

	return domain.SongCatalogItem{
		ID:           map[string]string{"spotify": track.ID},
		SourceID:     track.ID,
		Title:        track.Name,
		Artist:       strings.Join(artistNames, ", "),
		Album:        track.Album.Name,
		AlbumArtURL:  artURL,
		DurationMs:   int64(track.DurationMs),
		ISRC:         isrc,
		Availability: []string{"Spotify"},
		ExternalURLs: map[string]string{"spotify": track.ExternalURLs.Spotify},
	}
}

// SearchCatalog searches Spotify tracks and normalizes the top 10 results.
func (p *SpotifyProvider) SearchCatalog(ctx context.Context, query string) ([]domain.SongCatalogItem, error) {
	tracks, err := p.SearchTrack(ctx, query+" artist:")
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
