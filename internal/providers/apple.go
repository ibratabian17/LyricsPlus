// Package providers implements upstream lyric sources behind the Source interface.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
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

// appleAPIError carries the HTTP status of a failed Apple Music request so
// callers can distinguish a 404 (no lyrics) from other failures.
type appleAPIError struct {
	StatusCode int
	Msg        string
}

func (e *appleAPIError) Error() string { return e.Msg }

// isNotFound reports whether err is an Apple Music 404.
func isAppleStatus(err error, status int) bool {
	var apiErr *appleAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

const appleName = "apple"

const (
	appleBaseURL            = "https://amp-api.music.apple.com/v1"
	appleSuggestionsBaseURL = "https://amp-api-edge.music.apple.com/v1"
)

// AppleMusicProvider fetches TTML or syllable lyrics from Apple Music,
// rotating through a list of android/web accounts on 401/429 (503 rate-limits
// reuse the same account).
type AppleMusicProvider struct {
	client *proxy.Client
	mgm    *AccountManager[config.AppleAccount]
	logger *logger.Logger

	tokenMu          sync.Mutex
	storefrontMu     sync.Mutex
	cachedWebToken   string
	cachedStorefront string
}

func NewAppleMusic(client *proxy.Client) *AppleMusicProvider {
	return NewAppleMusicWithConfig(client, config.Load().Provider)
}

func NewAppleMusicWithConfig(client *proxy.Client, cfg config.Provider) *AppleMusicProvider {
	accounts := cfg.AppleAccounts
	if len(accounts) == 0 {
		accounts = []config.AppleAccount{
			{
				NAMEID:             "AppleAndroid",
				AUTH_TYPE:          "android",
				ANDROID_AUTH_TOKEN: cfg.AppleAndroidToken,
				ANDROID_DSID:       cfg.AppleAndroidDsid,
				ANDROID_USER_AGENT: cfg.AppleAndroidUserAgent,
				ANDROID_COOKIE:     cfg.AppleAndroidCookie,
				STOREFRONT:         cfg.AppleStorefront,
			},
			{
				NAMEID:           "AppleWeb",
				AUTH_TYPE:        "web",
				MUSIC_AUTH_TOKEN: cfg.AppleMediaUserToken,
			},
		}
	}
	for i := range accounts {
		if accounts[i].ANDROID_USER_AGENT == "" {
			accounts[i].ANDROID_USER_AGENT = "Music/6.1 Android/16 model/RealmeGT2Pro build/1472 (dt:66)"
		}
		if accounts[i].STOREFRONT == "" {
			accounts[i].STOREFRONT = "in"
		}
	}
	return &AppleMusicProvider{
		client: client,
		mgm:    newAccountManager(accounts),
	}
}

// SetLogger attaches a logger for debug output.
func (p *AppleMusicProvider) SetLogger(lg *logger.Logger) { p.logger = lg }

func (p *AppleMusicProvider) debugf(format string, args ...any) {
	if p.logger == nil {
		return
	}
	p.logger.Debugf("apple: "+format, args...)
}

func (p *AppleMusicProvider) Name() string { return appleName }
func (p *AppleMusicProvider) Configured() bool {
	for i := 0; i < p.mgm.Count(); i++ {
		if acc, ok := p.mgm.At(i); ok && acc.IsConfigured() {
			return true
		}
	}
	return false
}

func (p *AppleMusicProvider) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	storefront, err := p.GetStorefront(ctx)
	if err != nil {
		storefront = "in"
	}

	var bestMatch *appleSong
	if q.ISRC != "" {
		song, err := p.SearchByISRC(ctx, q.ISRC, storefront)
		if err == nil && song != nil {
			bestMatch = song
			p.debugf("ISRC search isrc:%s matched song %q", q.ISRC, song.Attributes.Name)
		} else if err != nil {
			p.debugf("ISRC search isrc:%s failed (err=%v)", q.ISRC, err)
		}
	}

	if bestMatch == nil {
		if q.IDOnly() {
			p.debugf("id-only query, no song resolved")
			return nil, nil
		}
		durationSec := float64(q.Duration) / 1000.0
		bestMatch, _ = p.SearchBestMatch(ctx, q.Title, q.Artist, q.Album, durationSec, q.ISRC, q.PlatformID)
		if bestMatch != nil {
			p.debugf("best match song %q id=%s", bestMatch.Attributes.Name, bestMatch.ID)
		}
	}

	if bestMatch == nil {
		p.debugf("no song matched for %q / %q", q.Title, q.Artist)
		return nil, nil
	}

	if bestMatch.Attributes.HasLyrics != nil && !*bestMatch.Attributes.HasLyrics {
		p.debugf("song %q has no lyrics (hasLyrics=false)", bestMatch.Attributes.Name)
		return nil, nil
	}

	lyricURL := fmt.Sprintf("%s/catalog/%s/songs/%s/syllable-lyrics?l%%5Blyrics%%5D=en-US&extend=ttmlLocalizations&l%%5Bscript%%5D=en-Latn",
		appleBaseURL, storefront, bestMatch.ID)

	resp, err := p.makeAppleMusicRequest(ctx, lyricURL, nil, 0, 0, 0)
	if err != nil {
		if isAppleStatus(err, http.StatusNotFound) {
			p.debugf("lyrics 404 for song %q", bestMatch.Attributes.Name)
			return nil, nil
		}
		p.debugf("lyrics request failed for song %q (err=%v)", bestMatch.Attributes.Name, err)
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("apple music lyrics returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rawResp struct {
		Data []struct {
			Attributes struct {
				TTML              string `json:"ttml"`
				TTMLLocalizations string `json:"ttmlLocalizations"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &rawResp); err != nil {
		return nil, err
	}
	if len(rawResp.Data) == 0 {
		return nil, nil
	}

	ttmlContent := rawResp.Data[0].Attributes.TTML
	if ttmlContent == "" {
		ttmlContent = rawResp.Data[0].Attributes.TTMLLocalizations
	}
	if ttmlContent == "" {
		return nil, nil
	}

	parsed, err := parsers.TTMLToJSON([]byte(ttmlContent))
	if err != nil || parsed == nil || len(parsed.Lyrics) == 0 {
		return nil, nil
	}

	parsed.Metadata.Source = "Apple"
	if len(bestMatch.Attributes.SongwriterNames) > 0 && len(parsed.Metadata.SongWriters) == 0 {
		parsed.Metadata.SongWriters = bestMatch.Attributes.SongwriterNames
	}
	parsed.Cached = domain.CacheNone
	parsed.RawData = ttmlContent

	songTitle := bestMatch.Attributes.Name
	if songTitle == "" {
		songTitle = q.Title
	}
	songArtist := bestMatch.Attributes.ArtistName
	if songArtist == "" {
		songArtist = q.Artist
	}
	songAlbum := bestMatch.Attributes.AlbumName
	if songAlbum == "" {
		songAlbum = q.Album
	}
	songISRC := bestMatch.Attributes.ISRC
	if songISRC == "" {
		songISRC = q.ISRC
	}
	songPlatformID := bestMatch.ID
	if songPlatformID == "" {
		songPlatformID = q.PlatformID
	}

	var durSec *float64
	if bestMatch.Attributes.DurationInMillis > 0 {
		s := float64(bestMatch.Attributes.DurationInMillis) / 1000.0
		durSec = &s
	} else if q.Duration > 0 {
		s := float64(q.Duration) / 1000.0
		durSec = &s
	}

	parsed.ProcessingTime = &domain.ProcessTiming{
		SelectedSongMetadata: &domain.PickedSongMetadata{
			Source:         "Apple",
			Title:          songTitle,
			Artist:         songArtist,
			Album:          songAlbum,
			Duration:       durSec,
			SongISRC:       songISRC,
			SongPlatformID: songPlatformID,
		},
	}

	return parsed, nil
}

type appleSongAttributes struct {
	Name             string   `json:"name"`
	ArtistName       string   `json:"artistName"`
	AlbumName        string   `json:"albumName"`
	DurationInMillis int      `json:"durationInMillis"`
	ISRC             string   `json:"isrc"`
	HasLyrics        *bool    `json:"hasLyrics,omitempty"`
	SongwriterNames  []string `json:"songwriterNames,omitempty"`
	ComposerName     string   `json:"composerName,omitempty"`
	URL              string   `json:"url"`
	Artwork          *struct {
		URL string `json:"url"`
	} `json:"artwork,omitempty"`
}

type appleSong struct {
	ID            string                 `json:"id"`
	Type          string                 `json:"type"`
	Attributes    appleSongAttributes    `json:"attributes"`
	RawAttributes map[string]interface{} `json:"-"`
}

func (s *appleSong) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID         string                 `json:"id"`
		Type       string                 `json:"type"`
		Attributes map[string]interface{} `json:"attributes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.ID = raw.ID
	s.Type = raw.Type
	s.RawAttributes = raw.Attributes

	attrBytes, _ := json.Marshal(raw.Attributes)
	_ = json.Unmarshal(attrBytes, &s.Attributes)
	return nil
}

func (p *AppleMusicProvider) getAuthHeaders(ctx context.Context, accountIdx int) (http.Header, error) {
	acc, ok := p.mgm.At(accountIdx)
	if !ok {
		return nil, fmt.Errorf("apple music: no account available")
	}

	h := make(http.Header)
	if strings.EqualFold(acc.AUTH_TYPE, "android") || acc.AUTH_TYPE == "" {
		h.Set("Authorization", "Bearer "+acc.ANDROID_AUTH_TOKEN)
		h.Set("x-dsid", acc.ANDROID_DSID)
		ua := acc.ANDROID_USER_AGENT
		if ua == "" {
			ua = "Music/6.1 Android/16 model/RealmeGT2Pro build/1472 (dt:66)"
		}
		h.Set("User-Agent", ua)
		if acc.ANDROID_COOKIE != "" {
			h.Set("Cookie", acc.ANDROID_COOKIE)
		}
		return h, nil
	}

	token, err := p.getWebToken(ctx)
	if err != nil {
		return nil, err
	}
	h.Set("Authorization", "Bearer "+token)
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	h.Set("Origin", "https://music.apple.com")
	h.Set("Referer", "https://music.apple.com")
	if acc.MUSIC_AUTH_TOKEN != "" {
		h.Set("media-user-token", acc.MUSIC_AUTH_TOKEN)
	}
	return h, nil
}

// resetCaches clears cached auth state on account rotation so the next attempt
// re-fetches the web token and storefront.
func (p *AppleMusicProvider) resetCaches() {
	p.tokenMu.Lock()
	p.cachedWebToken = ""
	p.tokenMu.Unlock()
	p.storefrontMu.Lock()
	p.cachedStorefront = ""
	p.storefrontMu.Unlock()
}

// makeAppleMusicRequest:
// - 503 (rate limit): retry the SAME account after 500–2000ms jitter.
// - 401/429 (auth): rotate to the NEXT account, resetting cached auth state.
// Both retries are bounded by maxAccountRetries.
func (p *AppleMusicProvider) makeAppleMusicRequest(ctx context.Context, urlstr string, extra http.Header, retries, rateLimitRetries, accountIdx int) (*http.Response, error) {
	headers, err := p.getAuthHeaders(ctx, accountIdx)
	if err != nil {
		return nil, err
	}
	for k, vv := range extra {
		for _, v := range vv {
			headers.Add(k, v)
		}
	}

	resp, err := p.client.Get(ctx, urlstr, headers)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusServiceUnavailable && rateLimitRetries < maxAccountRetries {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = body
		select {
		case <-time.After(time.Duration(500+rand.Intn(1501)) * time.Millisecond):
			return p.makeAppleMusicRequest(ctx, urlstr, extra, retries, rateLimitRetries+1, accountIdx)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests) && retries < maxAccountRetries {
		_ = resp.Body.Close()
		if next, hasNext := p.mgm.Next(accountIdx); hasNext {
			p.resetCaches()
			return p.makeAppleMusicRequest(ctx, urlstr, extra, retries+1, 0, next)
		}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
			return nil, &appleAPIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("Apple Music API returned status %d: %s", resp.StatusCode, string(body))}
		}
		return nil, &appleAPIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("Apple Music API returned status %d: %s", resp.StatusCode, string(body))}
	}

	return resp, nil
}

var (
	reScriptTag = regexp.MustCompile(`<script type="module" crossorigin src="(/assets/index[^"]+\.js)"></script>`)
	reTokenVar  = regexp.MustCompile(`\.headers\.Authorization\s*=\s*` + "`" + `Bearer\s*\$\{([^}]+)\}` + "`")
)

func (p *AppleMusicProvider) getWebToken(ctx context.Context) (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	if p.cachedWebToken != "" {
		return p.cachedWebToken, nil
	}

	// Dynamic scrape from music.apple.com
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://music.apple.com/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch apple music home: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	htmlBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	m := reScriptTag.FindSubmatch(htmlBytes)
	if len(m) < 2 {
		return "", fmt.Errorf("apple music: could not find index script tag")
	}
	scriptPath := string(m[1])
	scriptURL := "https://music.apple.com" + scriptPath

	sReq, err := http.NewRequestWithContext(ctx, http.MethodGet, scriptURL, nil)
	if err != nil {
		return "", err
	}
	sReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	sResp, err := p.client.Do(sReq)
	if err != nil {
		return "", fmt.Errorf("fetch apple music script: %w", err)
	}
	defer func() { _ = sResp.Body.Close() }()

	jsBytes, err := io.ReadAll(sResp.Body)
	if err != nil {
		return "", err
	}

	vm := reTokenVar.FindSubmatch(jsBytes)
	if len(vm) < 2 {
		return "", fmt.Errorf("apple music: could not find authorization token variable")
	}
	tokenVar := regexp.QuoteMeta(string(vm[1]))

	reTokenVal := regexp.MustCompile(tokenVar + `\s*=\s*"([^"]+)"`)
	valM := reTokenVal.FindSubmatch(jsBytes)
	if len(valM) < 2 {
		return "", fmt.Errorf("apple music: could not find authorization token value")
	}

	p.cachedWebToken = string(valM[1])
	return p.cachedWebToken, nil
}

func (p *AppleMusicProvider) GetStorefront(ctx context.Context) (string, error) {
	p.storefrontMu.Lock()
	defer p.storefrontMu.Unlock()
	if p.cachedStorefront != "" {
		return p.cachedStorefront, nil
	}

	// An android account's STOREFRONT is used directly; otherwise it's
	// fetched from the /me/storefront endpoint.
	current, ok := p.mgm.First()
	if ok && (strings.EqualFold(current.AUTH_TYPE, "android") || current.AUTH_TYPE == "") && current.STOREFRONT != "" {
		p.cachedStorefront = current.STOREFRONT
		return p.cachedStorefront, nil
	}

	headers, err := p.getAuthHeaders(ctx, 0)
	if err == nil {
		resp, err := p.client.Get(ctx, "https://api.music.apple.com/v1/me/storefront", headers)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusOK {
				var sfResp struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if json.NewDecoder(resp.Body).Decode(&sfResp) == nil && len(sfResp.Data) > 0 {
					p.cachedStorefront = sfResp.Data[0].ID
					return p.cachedStorefront, nil
				}
			}
		}
	}

	p.cachedStorefront = "us"
	return p.cachedStorefront, nil
}

func (p *AppleMusicProvider) SearchByISRC(ctx context.Context, isrc, storefront string) (*appleSong, error) {
	searchURL := fmt.Sprintf("%s/catalog/%s/songs?filter[isrc]=%s", appleBaseURL, storefront, url.QueryEscape(isrc))

	resp, err := p.makeAppleMusicRequest(ctx, searchURL, nil, 0, 0, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var res struct {
		Data []appleSong `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	if len(res.Data) > 0 {
		return &res.Data[0], nil
	}
	return nil, nil
}

func (p *AppleMusicProvider) SearchSongBySuggestions(ctx context.Context, query, storefront string) ([]appleSong, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	vals := url.Values{}
	vals.Set("art[url]", "f")
	vals.Set("fields[albums]", "artistName,artwork,contentRating,name,playParams,url")
	vals.Set("fields[artists]", "url,name,artwork")
	vals.Set("format[resources]", "map")
	vals.Set("kinds", "topResults")
	vals.Set("l", "en-US")
	vals.Set("limit[results:topResults]", "10")
	vals.Set("omit[resource]", "autos")
	vals.Set("platform", "web")
	vals.Set("term", query)
	vals.Set("types", "songs")
	vals.Set("with", "naturalLanguage")

	sugURL := fmt.Sprintf("%s/catalog/%s/search/suggestions?%s", appleSuggestionsBaseURL, storefront, vals.Encode())

	resp, err := p.makeAppleMusicRequest(ctx, sugURL, nil, 0, 0, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var res struct {
		Resources struct {
			Songs map[string]appleSong `json:"songs"`
		} `json:"resources"`
		Results struct {
			Suggestions []struct {
				Kind    string `json:"kind"`
				Content *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"content"`
			} `json:"suggestions"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	var songs []appleSong
	for _, s := range res.Results.Suggestions {
		if s.Kind == "topResults" && s.Content != nil && s.Content.Type == "songs" {
			if song, ok := res.Resources.Songs[s.Content.ID]; ok {
				songs = append(songs, song)
			}
		}
	}
	return songs, nil
}

func (p *AppleMusicProvider) SearchSong(ctx context.Context, query, storefront string) ([]appleSong, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	searchURL := fmt.Sprintf("%s/catalog/%s/search?types=songs&term=%s", appleBaseURL, storefront, url.QueryEscape(query))

	resp, err := p.makeAppleMusicRequest(ctx, searchURL, nil, 0, 0, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var res struct {
		Results struct {
			Songs struct {
				Data []appleSong `json:"data"`
			} `json:"songs"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return res.Results.Songs.Data, nil
}

func (p *AppleMusicProvider) SearchBestMatch(ctx context.Context, title, artist, album string, durationSec float64, songISRC, songPlatformID string) (*appleSong, error) {
	storefront, err := p.GetStorefront(ctx)
	if err != nil || storefront == "" {
		storefront = "in"
	}

	queries := buildSearchQueries(title, artist, album)
	var candidates []appleSong

	// 1. Suggestion queries in parallel (reversed: simplest first)
	reversed := make([]string, len(queries))
	for i, q := range queries {
		reversed[len(queries)-1-i] = q
	}

	type sugResult struct {
		songs []appleSong
	}
	sugCh := make(chan sugResult, len(reversed))
	var wg sync.WaitGroup
	for _, q := range reversed {
		wg.Add(1)
		go func(query string) {
			defer wg.Done()
			sugSongs, err := p.SearchSongBySuggestions(ctx, query, storefront)
			if err == nil && len(sugSongs) > 0 {
				sugCh <- sugResult{songs: sugSongs}
			}
		}(q)
	}
	wg.Wait()
	close(sugCh)

	for sr := range sugCh {
		candidates = append(candidates, sr.songs...)
	}

	if len(candidates) > 0 {
		if best := matchAppleSongs(candidates, title, artist, album, durationSec); best != nil {
			return best, nil
		}
	}

	// 2. Fallback to standard catalog search sequentially
	for _, q := range queries {
		songs, err := p.SearchSong(ctx, q, storefront)
		if err == nil && len(songs) > 0 {
			candidates = append(candidates, songs...)
			if best := matchAppleSongs(candidates, title, artist, album, durationSec); best != nil {
				return best, nil
			}
		}
	}

	return nil, nil
}

func matchAppleSongs(songs []appleSong, title, artist, album string, durationSec float64) *appleSong {
	simCandidates := make([]similarity.SongCandidate, len(songs))
	for i, s := range songs {
		simCandidates[i] = similarity.SongCandidate{
			Title:      s.Attributes.Name,
			Artist:     s.Attributes.ArtistName,
			Album:      s.Attributes.AlbumName,
			DurationMs: s.Attributes.DurationInMillis,
			Data:       i,
		}
	}

	best := similarity.FindBestSongMatch(simCandidates, title, artist, album, durationSec, "", "")
	if best == nil {
		return nil
	}
	idx := best.Candidate.Data.(int)
	return &songs[idx]
}

func buildSearchQueries(title, artist, album string) []string {
	var queries []string
	seen := make(map[string]bool)
	add := func(q string) {
		q = strings.TrimSpace(q)
		if q != "" && !seen[q] {
			seen[q] = true
			queries = append(queries, q)
		}
	}

	t := strings.TrimSpace(title)
	a := strings.TrimSpace(artist)
	al := strings.TrimSpace(album)

	// Query variants, most specific first: [title, artist, album],
	// [title, artist], artist + ' ' + title, title.
	var full []string
	if t != "" {
		full = append(full, t)
	}
	if a != "" {
		full = append(full, a)
	}
	if al != "" {
		full = append(full, al)
	}
	if len(full) > 0 {
		add(strings.Join(full, " "))
	}

	var ta []string
	if t != "" {
		ta = append(ta, t)
	}
	if a != "" {
		ta = append(ta, a)
	}
	if len(ta) > 0 {
		add(strings.Join(ta, " "))
	}

	if a != "" && t != "" {
		add(a + " " + t)
	}

	if t != "" {
		add(t)
	}

	return queries
}

// NormalizeSong converts an appleSong into domain.SongCatalogItem.
func (p *AppleMusicProvider) NormalizeSong(track appleSong) domain.SongCatalogItem {
	attrs := track.Attributes
	writers := attrs.SongwriterNames
	if len(writers) == 0 && attrs.ComposerName != "" {
		writers = []string{attrs.ComposerName}
	}

	var artURL *string
	if attrs.Artwork != nil && attrs.Artwork.URL != "" {
		u := strings.Replace(attrs.Artwork.URL, "{w}", "300", 1)
		u = strings.Replace(u, "{h}", "300", 1)
		artURL = &u
	}

	var isrc *string
	if attrs.ISRC != "" {
		isrc = &attrs.ISRC
	}

	return domain.SongCatalogItem{
		ID:           map[string]string{"appleMusic": track.ID},
		SourceID:     track.ID,
		Title:        attrs.Name,
		Artist:       attrs.ArtistName,
		Album:        attrs.AlbumName,
		AlbumArtURL:  artURL,
		DurationMs:   int64(attrs.DurationInMillis),
		ISRC:         isrc,
		Songwriters:  writers,
		Availability: []string{"Apple Music"},
		ExternalURLs: map[string]string{"appleMusic": attrs.URL},
	}
}

// SearchCatalog searches songs on Apple Music and normalizes the top 10 results.
func (p *AppleMusicProvider) SearchCatalog(ctx context.Context, query string) ([]domain.SongCatalogItem, error) {
	storefront, err := p.GetStorefront(ctx)
	if err != nil || storefront == "" {
		storefront = "us"
	}
	songs, err := p.SearchSong(ctx, query, storefront)
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

// GetMetadata searches Apple Music across prioritized queries and returns filtered metadata attributes.
func (p *AppleMusicProvider) GetMetadata(ctx context.Context, title, artist, album string, durationSec float64) (map[string]interface{}, error) {
	storefront, err := p.GetStorefront(ctx)
	if err != nil || storefront == "" {
		storefront = "us"
	}

	searchQueries := buildMetadataQueries(title, artist, album)
	var candidates []appleSong

	for _, q := range searchQueries {
		songs, err := p.SearchSong(ctx, q, storefront)
		if err != nil || len(songs) == 0 {
			continue
		}
		candidates = append(candidates, songs...)
		if best := matchAppleSongs(candidates, title, artist, album, durationSec); best != nil {
			meta := make(map[string]interface{}, len(best.RawAttributes))
			for k, v := range best.RawAttributes {
				meta[k] = v
			}
			delete(meta, "isVocalAttenuationAllowed")
			delete(meta, "isMasteredForItunes")
			delete(meta, "url")
			delete(meta, "playParams")
			delete(meta, "discNumber")
			delete(meta, "isAppleDigitalMaster")
			delete(meta, "hasLyrics")
			delete(meta, "audioTraits")
			delete(meta, "hasTimeSyncedLyrics")
			return meta, nil
		}
	}
	return nil, nil
}

func buildMetadataQueries(title, artist, album string) []string {
	var queries []string
	t := strings.TrimSpace(title)
	a := strings.TrimSpace(artist)
	al := strings.TrimSpace(album)

	var parts []string
	if t != "" {
		parts = append(parts, t)
	}
	if a != "" {
		parts = append(parts, a)
	}
	if al != "" {
		parts = append(parts, al)
	}
	if len(parts) > 0 {
		queries = append(queries, strings.Join(parts, " "))
	}

	if t != "" && a != "" {
		queries = append(queries, t+" "+a)
		queries = append(queries, a+" "+t)
	}

	if t != "" {
		queries = append(queries, t)
	}
	return queries
}
