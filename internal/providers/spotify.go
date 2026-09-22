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

const spotifyName = "spotify"

const (
	spotifyBaseURL          = "https://api.spotify.com/v1"
	spotifyLyricsURL        = "https://spclient.wg.spotify.com/color-lyrics/v2/track/"
	spotifyAuthURL          = "https://accounts.spotify.com/api/token"
	spotifyDefaultSecretURL = "https://raw.githubusercontent.com/Thereallo1026/spotify-secrets/main/secrets/secretDict.json"
	spotifyDefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
)

var fallbackSecrets = map[string][]int{
	"14": {62, 54, 109, 83, 107, 77, 41, 103, 45, 93, 114, 38, 41, 97, 64, 51, 95, 94, 95, 94},
	"13": {59, 92, 64, 70, 99, 78, 117, 75, 99, 103, 116, 67, 103, 51, 87, 63, 93, 59, 70, 45, 32},
	"12": {107, 81, 49, 57, 67, 93, 87, 81, 69, 67, 40, 93, 48, 50, 46, 91, 94, 113, 41, 108, 77, 107, 34},
}

// BestSecret returns the highest version fallback secret.
func BestSecret() []int {
	return fallbackSecrets["14"]
}

// SpotifyProvider fetches color lyrics from Spotify.
type SpotifyProvider struct {
	client       *proxy.Client
	spDc         string
	clientID     string
	clientSecret string

	webTokenMu  sync.Mutex
	webToken    string
	webExpires  time.Time
	apiTokenMu  sync.Mutex
	apiToken    string
	apiExpires  time.Time
	secretsMu   sync.Mutex
	secretsDict map[string][]int
	secretsTime time.Time
}

func NewSpotify(client *proxy.Client) *SpotifyProvider {
	return NewSpotifyWithConfig(client, config.Load().Provider)
}

func NewSpotifyWithConfig(client *proxy.Client, cfg config.Provider) *SpotifyProvider {
	return &SpotifyProvider{
		client:       client,
		spDc:         normalizeCookie(cfg.SpotifySpotifyDCCookie),
		clientID:     cfg.SpotifyClientID,
		clientSecret: cfg.SpotifyClientSecret,
		secretsDict:  fallbackSecrets,
	}
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

func (p *SpotifyProvider) Name() string     { return spotifyName }
func (p *SpotifyProvider) Configured() bool { return p.spDc != "" }

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
				tracks = nil
			}
		}
		if len(tracks) == 0 {
			if q.IDOnly() {
				return nil, nil
			}
			searchQ := strings.TrimSpace(q.Title + " " + q.Artist)
			var err error
			tracks, err = p.SearchTrack(ctx, searchQ)
			if err != nil || len(tracks) == 0 {
				return nil, nil
			}
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
			return nil, nil
		}
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
		return nil, err
	}

	converted, err := parsers.ConvertSpotifyToJSON(lyricsJSON)
	if err != nil || converted == nil || len(converted.Lyrics) == 0 {
		return nil, nil
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

func (p *SpotifyProvider) getWebToken(ctx context.Context) (string, error) {
	p.webTokenMu.Lock()
	defer p.webTokenMu.Unlock()
	if p.webToken != "" && time.Now().Before(p.webExpires) {
		return p.webToken, nil
	}

	totp, ver, err := p.generateTOTP(ctx)
	if err != nil {
		return "", err
	}

	headers := make(http.Header)
	headers.Set("Cookie", p.spDc)
	headers.Set("User-Agent", spotifyDefaultUserAgent)
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
		AccessToken             string `json:"accessToken"`
		AccessTokenExpirationMs int64  `json:"accessTokenExpirationTimestampMs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("spotify: empty access token")
	}

	p.webToken = res.AccessToken
	if res.AccessTokenExpirationMs > 0 {
		p.webExpires = time.UnixMilli(res.AccessTokenExpirationMs)
	} else {
		p.webExpires = time.Now().Add(50 * time.Minute)
	}

	return p.webToken, nil
}

func (p *SpotifyProvider) getAPIToken(ctx context.Context) (string, error) {
	if p.clientID == "" || p.clientSecret == "" {
		return p.getWebToken(ctx)
	}

	p.apiTokenMu.Lock()
	defer p.apiTokenMu.Unlock()
	if p.apiToken != "" && time.Now().Before(p.apiExpires) {
		return p.apiToken, nil
	}

	authBasic := base64.StdEncoding.EncodeToString([]byte(p.clientID + ":" + p.clientSecret))
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

	p.apiToken = res.AccessToken
	p.apiExpires = time.Now().Add(time.Duration(res.ExpiresIn-60) * time.Second)
	return p.apiToken, nil
}

func (p *SpotifyProvider) SearchTrack(ctx context.Context, query string) ([]spotifyTrack, error) {
	token, err := p.getAPIToken(ctx)
	if err != nil {
		return nil, err
	}

	searchURL := fmt.Sprintf("%s/search?q=%s&type=track&limit=10", spotifyBaseURL, url.QueryEscape(query))
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("User-Agent", spotifyDefaultUserAgent)

	resp, err := p.client.Get(ctx, searchURL, headers)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spotify search returned status %d", resp.StatusCode)
	}

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
	token, err := p.getWebToken(ctx)
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("%s%s?format=json&vocalRemoval=false&market=from_token", spotifyLyricsURL, trackID)
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Cookie", p.spDc)
	headers.Set("app-platform", "WebPlayer")
	headers.Set("User-Agent", spotifyDefaultUserAgent)

	resp, err := p.client.Get(ctx, reqURL, headers)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("spotify color-lyrics status %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
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
	serverTime := p.getServerTime(ctx)
	totp := GenerateTOTP(secretBytes, serverTime.Unix(), 6, 30)
	return totp, maxVer, nil
}

func (p *SpotifyProvider) getServerTime(ctx context.Context) time.Time {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://open.spotify.com/", nil)
	if err == nil {
		req.Header.Set("User-Agent", spotifyDefaultUserAgent)
		resp, err := p.client.Do(req)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			dateHeader := resp.Header.Get("Date")
			if dateHeader != "" {
				if t, err := http.ParseTime(dateHeader); err == nil {
					return t
				}
			}
		}
	}
	return time.Now()
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

	p.secretsDict = fallbackSecrets
	p.secretsTime = time.Now()
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
