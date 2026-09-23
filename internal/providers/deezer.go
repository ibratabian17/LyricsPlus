package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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

const deezerName = "deezer"

const (
	deezerDefaultAuthURL    = "https://auth.deezer.com/login/renew?jo=p&rto=c&i=c"
	deezerDefaultGraphqlURL = "https://pipe.deezer.com/api"
	deezerDefaultSearchURL  = "https://api.deezer.com/search/track"
)

const deezerDefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"

// deezerAccountCredentials holds the per-account token cache: the JWT plus the
// refresh token returned by the auth API.
type deezerAccountCredentials struct {
	jwt      string
	refToken string
	expires  time.Time
}

// DeezerProvider fetches word-by-word or synchronized lyrics from Deezer
// GraphQL. Authentication uses the current account and a failure rotates to
// the next account, each account keeping its own JWT.
type DeezerProvider struct {
	client     *proxy.Client
	mgm        *AccountManager[config.DeezerAccount]
	authURL    string
	graphqlURL string
	searchURL  string

	mu   sync.RWMutex
	jwts map[int]*deezerAccountCredentials
}

func NewDeezer(client *proxy.Client) *DeezerProvider {
	return NewDeezerWithConfig(client, config.Load().Provider)
}

func NewDeezerWithConfig(client *proxy.Client, cfg config.Provider) *DeezerProvider {
	accounts := cfg.DeezerAccounts
	if len(accounts) == 0 {
		accounts = []config.DeezerAccount{{
			NAMEID:        "DeezerDefault",
			REFRESH_TOKEN: cfg.DeezerRefreshToken,
			ARL:           cfg.DeezerARL,
		}}
	}

	authURL := os.Getenv("DEEZER_AUTH_URL")
	if authURL == "" {
		authURL = deezerDefaultAuthURL
	}
	graphqlURL := os.Getenv("DEEZER_GRAPHQL_URL")
	if graphqlURL == "" {
		graphqlURL = deezerDefaultGraphqlURL
	}
	searchURL := os.Getenv("DEEZER_SEARCH_URL")
	if searchURL == "" {
		searchURL = deezerDefaultSearchURL
	}

	return &DeezerProvider{
		client:     client,
		mgm:        newAccountManager(accounts),
		authURL:    authURL,
		graphqlURL: graphqlURL,
		searchURL:  searchURL,
		jwts:       map[int]*deezerAccountCredentials{},
	}
}

func (p *DeezerProvider) Name() string { return deezerName }

func (p *DeezerProvider) Configured() bool {
	for i := 0; i < p.mgm.Count(); i++ {
		if acc, ok := p.mgm.At(i); ok && acc.IsConfigured() {
			return true
		}
	}
	return false
}

// credentials returns the cached JWT for the given account index, or nil if
// none is cached yet.
func (p *DeezerProvider) cachedCredentials(accountIdx int) (*deezerAccountCredentials, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	creds, ok := p.jwts[accountIdx]
	return creds, ok
}

func (p *DeezerProvider) setCredentials(accountIdx int, creds *deezerAccountCredentials) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jwts[accountIdx] = creds
}

func (p *DeezerProvider) clearCredentials(accountIdx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.jwts, accountIdx)
}

func (p *DeezerProvider) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	if !p.Configured() {
		return nil, nil
	}

	trackID := q.PlatformID
	songTitle := q.Title
	songArtist := q.Artist
	songAlbum := q.Album
	songISRC := q.ISRC

	if trackID == "" {
		tracks, err := p.SearchTrack(ctx, strings.TrimSpace(q.Title+" "+q.Artist), 10)
		if err != nil || len(tracks) == 0 {
			return nil, nil
		}

		candidates := make([]similarity.SongCandidate, len(tracks))
		for i, t := range tracks {
			candidates[i] = similarity.SongCandidate{
				Title:      t.Title,
				Artist:     t.Artist.Name,
				Album:      t.Album.Title,
				DurationMs: t.Duration * 1000,
				ISRC:       t.ISRC,
				PlatformID: strconv.FormatInt(t.ID, 10),
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

	lyricsJSON, err := p.getLyrics(ctx, trackID, 0)
	if err != nil || lyricsJSON == nil {
		return nil, err
	}

	converted, err := parsers.NormalizeDeezerLyrics(lyricsJSON)
	if err != nil || converted == nil || len(converted.Lyrics) == 0 {
		return nil, nil
	}

	converted.Metadata.Source = "Deezer"
	converted.Cached = domain.CacheNone
	converted.RawData = string(lyricsJSON)
	converted.ProcessingTime = &domain.ProcessTiming{
		SelectedSongMetadata: &domain.PickedSongMetadata{
			Source:         "Deezer",
			Title:          songTitle,
			Artist:         songArtist,
			Album:          songAlbum,
			SongISRC:       songISRC,
			SongPlatformID: trackID,
		},
	}
	return converted, nil
}

type deezerTrack struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Link     string `json:"link"`
	Duration int    `json:"duration"`
	ISRC     string `json:"isrc"`
	Artist   struct {
		Name string `json:"name"`
	} `json:"artist"`
	Album struct {
		Title      string `json:"title"`
		CoverXL    string `json:"cover_xl"`
		CoverLarge string `json:"cover_large"`
	} `json:"album"`
}

func (p *DeezerProvider) SearchTrack(ctx context.Context, query string, limit int) ([]deezerTrack, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	reqURL := fmt.Sprintf("%s?q=%s&limit=%d&index=0", p.searchURL, url.QueryEscape(query), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deezer search returned status %d", resp.StatusCode)
	}

	var res struct {
		Data []deezerTrack `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return res.Data, nil
}

// authenticate obtains a JWT for the given account index via the Deezer auth
// API. Refresh-token is preferred; an ARL is sent as an arl=<token> cookie, and
// the response's refresh_token rotation is cached. A failed auth (empty JWT)
// returns an error which the caller uses to rotate to the next account.
func (p *DeezerProvider) authenticate(ctx context.Context, accountIdx int) (string, error) {
	acc, ok := p.mgm.At(accountIdx)
	if !ok {
		return "", fmt.Errorf("deezer: account %d unavailable", accountIdx)
	}

	rawToken := acc.REFRESH_TOKEN
	isARL := false
	if rawToken == "" {
		rawToken = acc.ARL
		isARL = true
	}
	if rawToken == "" {
		return "", fmt.Errorf("deezer: account %q has no credentials", acc.NAMEID)
	}

	cleanToken := strings.TrimSpace(rawToken)
	cleanToken = strings.TrimPrefix(cleanToken, "refresh-token=")

	var cookieString string
	if isARL {
		cookieString = "arl=" + cleanToken
	} else {
		cookieString = "refresh-token=" + cleanToken
	}

	headers := make(http.Header)
	headers.Set("User-Agent", deezerDefaultUserAgent)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "*/*")
	headers.Set("Cookie", cookieString)

	resp, err := p.client.Post(ctx, p.authURL, headers, []byte("{}"))
	if err != nil {
		return "", fmt.Errorf("deezer auth request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deezer auth returned status %d: %s", resp.StatusCode, string(body))
	}

	var authResp struct {
		JWT          string `json:"jwt"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &authResp); err != nil {
		return "", fmt.Errorf("parse deezer auth: %w", err)
	}

	if authResp.JWT == "" {
		return "", fmt.Errorf("deezer auth returned empty jwt for account %q", acc.NAMEID)
	}

	newRefToken := authResp.RefreshToken
	for _, cookie := range resp.Header["Set-Cookie"] {
		if rest, ok := strings.CutPrefix(cookie, "refresh-token="); ok {
			if semicolon := strings.Index(rest, ";"); semicolon >= 0 {
				rest = rest[:semicolon]
			}
			newRefToken = rest
			break
		}
	}
	if newRefToken == "" {
		newRefToken = rawToken
	}

	p.setCredentials(accountIdx, &deezerAccountCredentials{
		jwt:      authResp.JWT,
		refToken: newRefToken,
		expires:  time.Now().Add(50 * time.Minute),
	})
	return authResp.JWT, nil
}

const graphqlQuery = `query GetLyrics($trackId: String!) {
  track(trackId: $trackId) {
    id
    lyrics {
      id
      text
      ...SynchronizedWordByWordLines
      ...SynchronizedLines
      licence
      copyright
      writers
      __typename
    }
    __typename
  }
}

fragment SynchronizedWordByWordLines on Lyrics {
  id
  synchronizedWordByWordLines {
    start
    end
    words {
      start
      end
      word
      __typename
    }
    __typename
  }
  __typename
}

fragment SynchronizedLines on Lyrics {
  id
  synchronizedLines {
    lrcTimestamp
    line
    lineTranslated
    milliseconds
    duration
    __typename
  }
  __typename
}`

// getLyrics fetches lyrics with a valid JWT. retryCount tracks retries across
// accounts: on an auth error (GraphQL token error or a bad JWT) the current
// account's credentials are cleared and the next account is tried (up to
// maxAccountRetries).
func (p *DeezerProvider) getLyrics(ctx context.Context, trackID string, retryCount int) ([]byte, error) {
	accountIdx := 0
	return p.getLyricsWithAccount(ctx, trackID, accountIdx, retryCount)
}

func (p *DeezerProvider) getLyricsWithAccount(ctx context.Context, trackID string, accountIdx, retryCount int) ([]byte, error) {
	jwtToken := ""
	if creds, ok := p.cachedCredentials(accountIdx); ok && creds.jwt != "" && time.Now().Before(creds.expires) {
		jwtToken = creds.jwt
	} else {
		var err error
		jwtToken, err = p.authenticate(ctx, accountIdx)
		if err != nil {
			if next, ok := p.mgm.Next(accountIdx); ok && retryCount < maxAccountRetries {
				return p.getLyricsWithAccount(ctx, trackID, next, retryCount+1)
			}
			return nil, err
		}
	}

	data, err := p.graphQLLyrics(ctx, trackID, jwtToken)
	if err != nil {
		if isDeezerAuthError(err.Error()) {
			p.clearCredentials(accountIdx)
			if next, ok := p.mgm.Next(accountIdx); ok && retryCount < maxAccountRetries {
				return p.getLyricsWithAccount(ctx, trackID, next, retryCount+1)
			}
			return nil, err
		}
		return nil, err
	}
	return data, nil
}

func isDeezerAuthError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "token") || strings.Contains(lower, "unauthorized")
}

func (p *DeezerProvider) graphQLLyrics(ctx context.Context, trackID, jwtToken string) ([]byte, error) {
	payload := map[string]interface{}{
		"operationName": "GetLyrics",
		"variables":     map[string]string{"trackId": trackID},
		"query":         graphqlQuery,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	headers := make(http.Header)
	headers.Set("Accept", "*/*")
	headers.Set("Authorization", "Bearer "+jwtToken)
	headers.Set("Content-Type", "application/json")

	resp, err := p.client.Post(ctx, p.graphqlURL, headers, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &parsed); err == nil && len(parsed.Errors) > 0 {
		msg := parsed.Errors[0].Message
		if strings.Contains(strings.ToLower(msg), "token") || strings.Contains(strings.ToLower(msg), "unauthorized") {
			return nil, fmt.Errorf("deezer graphql auth error: %s", msg)
		}
		return nil, fmt.Errorf("deezer graphql error: %s", msg)
	}

	if len(parsed.Data) == 0 || string(parsed.Data) == "null" {
		return nil, nil
	}

	return parsed.Data, nil
}

// NormalizeSong converts Deezer Track into domain.SongCatalogItem.
func (p *DeezerProvider) NormalizeSong(t deezerTrack) domain.SongCatalogItem {
	artURL := t.Album.CoverXL
	if artURL == "" {
		artURL = t.Album.CoverLarge
	}
	var art *string
	if artURL != "" {
		art = &artURL
	}
	var isrc *string
	if t.ISRC != "" {
		isrc = &t.ISRC
	}

	return domain.SongCatalogItem{
		ID:           map[string]string{"deezer": strconv.FormatInt(t.ID, 10)},
		SourceID:     strconv.FormatInt(t.ID, 10),
		Title:        t.Title,
		Artist:       t.Artist.Name,
		Album:        t.Album.Title,
		AlbumArtURL:  art,
		DurationMs:   int64(t.Duration * 1000),
		ISRC:         isrc,
		Availability: []string{"Deezer"},
		ExternalURLs: map[string]string{"deezer": t.Link},
	}
}
