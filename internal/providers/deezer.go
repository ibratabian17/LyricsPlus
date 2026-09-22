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

// DeezerProvider fetches word-by-word or synchronized lyrics from Deezer GraphQL.
type DeezerProvider struct {
	client       *proxy.Client
	arl          string
	refreshToken string
	authURL      string
	graphqlURL   string
	searchURL    string

	mu          sync.RWMutex
	cachedJWT   string
	cachedToken string
}

func NewDeezer(client *proxy.Client) *DeezerProvider {
	arl := os.Getenv("DEEZER_ARL")
	refToken := os.Getenv("DEEZER_REFRESH_TOKEN")
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
		client:       client,
		arl:          arl,
		refreshToken: refToken,
		authURL:      authURL,
		graphqlURL:   graphqlURL,
		searchURL:    searchURL,
	}
}

func (p *DeezerProvider) Name() string { return deezerName }

func (p *DeezerProvider) Configured() bool {
	return p.arl != "" || p.refreshToken != ""
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

func (p *DeezerProvider) authenticate(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cachedJWT != "" {
		return p.cachedJWT, nil
	}

	rawToken := p.refreshToken
	isARL := false
	if rawToken == "" {
		rawToken = p.arl
		isARL = true
	}
	if rawToken == "" {
		return "", fmt.Errorf("no deezer credentials available")
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
	headers.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36")
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

	var authResp struct {
		JWT          string `json:"jwt"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &authResp); err != nil {
		return "", fmt.Errorf("parse deezer auth: %w", err)
	}

	if authResp.JWT == "" {
		return "", fmt.Errorf("deezer auth returned empty jwt: %s", string(body))
	}

	p.cachedJWT = authResp.JWT
	if authResp.RefreshToken != "" {
		p.cachedToken = authResp.RefreshToken
	}
	return p.cachedJWT, nil
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

func (p *DeezerProvider) getLyrics(ctx context.Context, trackID string, retryCount int) ([]byte, error) {
	p.mu.RLock()
	jwtToken := p.cachedJWT
	p.mu.RUnlock()

	if jwtToken == "" {
		var err error
		jwtToken, err = p.authenticate(ctx)
		if err != nil {
			return nil, err
		}
	}

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
		if retryCount == 0 {
			p.mu.Lock()
			p.cachedJWT = ""
			p.mu.Unlock()
			return p.getLyrics(ctx, trackID, retryCount+1)
		}
		return nil, fmt.Errorf("deezer graphql error: %s", parsed.Errors[0].Message)
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
