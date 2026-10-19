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

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/parsers"
	"lyricsplus/backend/internal/proxy"
	"lyricsplus/backend/internal/similarity"
)

type appleAPIError struct {
	StatusCode int
	Msg        string
}

func (e *appleAPIError) Error() string { return e.Msg }

func isAppleStatus(err error, status int) bool {
	var apiErr *appleAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

const appleName = "apple"

const (
	appleBaseURL            = "https://amp-api.music.apple.com/v1"
	appleSuggestionsBaseURL = "https://amp-api-edge.music.apple.com/v1"
	appleSearchCacheTTL     = 5 * time.Minute
)

type appleSongCacheEntry struct {
	songs   []appleSong
	created time.Time
}

type appleISRCCacheEntry struct {
	song    *appleSong
	created time.Time
}

type AppleMusicProvider struct {
	client *proxy.Client
	mgm    *AccountManager[config.AppleAccount]
	logger *logger.Logger

	tokenMu           sync.Mutex
	storefrontMu      sync.Mutex
	cachedWebToken    string
	webTokenExpires   time.Time
	webTokenFailUntil time.Time
	cachedStorefront  string

	flightGroup      singleflight.Group
	searchCache      *lru.Cache[string, *appleSongCacheEntry]
	suggestionsCache *lru.Cache[string, *appleSongCacheEntry]
	isrcCache        *lru.Cache[string, *appleISRCCacheEntry]
	idCache          *lru.Cache[string, *appleISRCCacheEntry]
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
	var validAccounts []config.AppleAccount
	for i := range accounts {
		if accounts[i].ANDROID_USER_AGENT == "" {
			accounts[i].ANDROID_USER_AGENT = "Music/6.1 Android/16 model/RealmeGT2Pro build/1472 (dt:66)"
		}
		if accounts[i].IsConfigured() {
			validAccounts = append(validAccounts, accounts[i])
		}
	}
	if len(validAccounts) > 0 {
		accounts = validAccounts
	}
	searchCache, _ := lru.New[string, *appleSongCacheEntry](1024)
	suggestionsCache, _ := lru.New[string, *appleSongCacheEntry](1024)
	isrcCache, _ := lru.New[string, *appleISRCCacheEntry](1024)
	idCache, _ := lru.New[string, *appleISRCCacheEntry](1024)
	return &AppleMusicProvider{
		client:           client,
		mgm:              newAccountManager(accounts),
		searchCache:      searchCache,
		suggestionsCache: suggestionsCache,
		isrcCache:        isrcCache,
		idCache:          idCache,
	}
}

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

func cloneAppleLyrics(src *domain.LyricsResponse) *domain.LyricsResponse {
	if src == nil {
		return nil
	}
	cp := *src
	if src.ProcessingTime != nil {
		pt := *src.ProcessingTime
		if src.ProcessingTime.SelectedSongMetadata != nil {
			sm := *src.ProcessingTime.SelectedSongMetadata
			pt.SelectedSongMetadata = &sm
		}
		cp.ProcessingTime = &pt
	}
	return &cp
}

func (p *AppleMusicProvider) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	if !p.Configured() {
		return nil, nil
	}
	sfKey := q.ContentKey()
	ch := p.flightGroup.DoChan(sfKey, func() (any, error) {
		runCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		return p.doFetchLyrics(runCtx, q)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		if resp, ok := res.Val.(*domain.LyricsResponse); ok && resp != nil {
			return cloneAppleLyrics(resp), nil
		}
		return nil, nil
	}
}

func (p *AppleMusicProvider) doFetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	storefront := p.getAccountStorefront(ctx, p.mgm.CurrentIndex())
	if storefront == "" {
		p.debugf("no storefront configured or detected for apple music")
		return nil, nil
	}

	var bestMatch *appleSong
	if q.PlatformID != "" {
		song, err := p.GetSongByID(ctx, q.PlatformID, storefront)
		if err == nil && song != nil {
			bestMatch = song
			p.debugf("platformID lookup %s matched song %q (storefront %s)", q.PlatformID, song.Attributes.Name, storefront)
		} else if err != nil {
			p.debugf("platformID lookup %s failed (err=%v)", q.PlatformID, err)
		}
	}

	if bestMatch == nil && q.ISRC != "" {
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

	var (
		resp       *http.Response
		err        error
		failedIdxs []int
	)

	maxAttempts := p.mgm.Count()
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	currIdx := p.mgm.CurrentIndex()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		accStorefront := p.getAccountStorefront(ctx, currIdx)
		if accStorefront == "" {
			accStorefront = storefront
		}
		if accStorefront == "" {
			continue
		}
		lyricURL := fmt.Sprintf("%s/catalog/%s/songs/%s/syllable-lyrics?l%%5Blyrics%%5D=en-US&extend=ttmlLocalizations&l%%5Bscript%%5D=en-Latn",
			appleBaseURL, accStorefront, bestMatch.ID)

		resp, err = p.makeAppleMusicRequest(ctx, lyricURL, nil, 0, 0, currIdx)
		if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
			for _, badIdx := range failedIdxs {
				p.debugf("account %d silent-failed on syllable-lyrics, marking bad (cooldown)", badIdx)
				p.mgm.MarkBad(badIdx, 30*time.Second)
			}
			break
		}

		if resp != nil {
			_ = resp.Body.Close()
			resp = nil
		}

		if isAppleStatus(err, http.StatusNotFound) {
			failedIdxs = append(failedIdxs, currIdx)
			if nextIdx, hasNext := p.mgm.Next(currIdx); hasNext && attempt+1 < maxAttempts {
				p.debugf("lyrics 404 on account %d for song %q; rotating to account %d", currIdx, bestMatch.Attributes.Name, nextIdx)
				currIdx = nextIdx
				continue
			}
			p.debugf("lyrics 404 for song %q across attempted accounts", bestMatch.Attributes.Name)
			return nil, nil
		}

		if err != nil {
			p.debugf("lyrics request failed for song %q on account %d (err=%v)", bestMatch.Attributes.Name, currIdx, err)
			return nil, err
		}
	}

	if resp == nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("apple music lyrics returned status %d", resp.StatusCode)
	}
	defer func() { _ = resp.Body.Close() }()

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
		ID         string          `json:"id"`
		Type       string          `json:"type"`
		Attributes json.RawMessage `json:"attributes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.ID = raw.ID
	s.Type = raw.Type
	if len(raw.Attributes) > 0 {
		_ = json.Unmarshal(raw.Attributes, &s.Attributes)
		_ = json.Unmarshal(raw.Attributes, &s.RawAttributes)
	}
	return nil
}

func (p *AppleMusicProvider) getAuthHeaders(ctx context.Context, accountIdx int) (http.Header, error) {
	acc, ok := p.mgm.At(accountIdx)
	if !ok {
		return nil, fmt.Errorf("apple music: no account available")
	}

	h := make(http.Header)
	if strings.EqualFold(acc.AUTH_TYPE, "android") || acc.AUTH_TYPE == "" {
		if acc.ANDROID_AUTH_TOKEN == "" {
			return nil, fmt.Errorf("apple music: empty android auth token")
		}
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
		h.Set("music-user-token", acc.MUSIC_AUTH_TOKEN)
		h.Set("media-user-token", acc.MUSIC_AUTH_TOKEN)
	}
	return h, nil
}

func (p *AppleMusicProvider) resetWebToken() {
	p.tokenMu.Lock()
	p.cachedWebToken = ""
	p.webTokenExpires = time.Time{}
	p.tokenMu.Unlock()
}

func (p *AppleMusicProvider) resetCaches() {
	p.resetWebToken()
	p.storefrontMu.Lock()
	p.cachedStorefront = ""
	p.storefrontMu.Unlock()
}

func (p *AppleMusicProvider) makeAppleMusicRequest(ctx context.Context, urlstr string, extra http.Header, retries, rateLimitRetries, accountIdx int) (*http.Response, error) {
	headers, err := p.getAuthHeaders(ctx, accountIdx)
	if err != nil {
		p.mgm.MarkBad(accountIdx, 30*time.Second)
		if retries < maxAccountRetries {
			if next, hasNext := p.mgm.Next(accountIdx); hasNext {
				return p.makeAppleMusicRequest(ctx, urlstr, extra, retries+1, 0, next)
			}
		}
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

	if resp.StatusCode == http.StatusServiceUnavailable && rateLimitRetries < 1 {
		_ = resp.Body.Close()
		delay := time.Duration(150+rand.Intn(200)) * time.Millisecond
		select {
		case <-time.After(delay):
			return p.makeAppleMusicRequest(ctx, urlstr, extra, retries, rateLimitRetries+1, accountIdx)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		_ = resp.Body.Close()
		p.mgm.MarkBad(accountIdx, 15*time.Second)
		if retries < maxAccountRetries {
			if next, hasNext := p.mgm.Next(accountIdx); hasNext {
				return p.makeAppleMusicRequest(ctx, urlstr, extra, retries+1, 0, next)
			}
		}
		return nil, &appleAPIError{StatusCode: resp.StatusCode, Msg: "Apple Music API rate limited (429)"}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if acc, ok := p.mgm.At(accountIdx); ok && strings.EqualFold(acc.AUTH_TYPE, "web") {
			p.resetWebToken()
		}
		p.mgm.MarkBad(accountIdx, 30*time.Second)
		if retries < maxAccountRetries {
			if next, hasNext := p.mgm.Next(accountIdx); hasNext {
				return p.makeAppleMusicRequest(ctx, urlstr, extra, retries+1, 0, next)
			}
		}
		return nil, &appleAPIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("Apple Music API returned status %d: %s", resp.StatusCode, string(body))}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, &appleAPIError{StatusCode: resp.StatusCode, Msg: fmt.Sprintf("Apple Music API returned status %d: %s", resp.StatusCode, string(body))}
	}

	return resp, nil
}

var (
	reScriptTag   = regexp.MustCompile(`<script[^>]+src="(/assets/index[^"]+\.js)"`)
	reTokenVar    = regexp.MustCompile(`\.headers\.Authorization\s*=\s*` + "`" + `Bearer\s*\$\{([^}]+)\}` + "`")
	reDirectJWT   = regexp.MustCompile(`"(eyJ(?:0eXAi|hbGci)[a-zA-Z0-9_\-]+\.[a-zA-Z0-9_\-]+\.[a-zA-Z0-9_\-]+)"`)
	reMultiArtist = regexp.MustCompile(`(?i)\s*(?:&|and|feat\.?|ft\.?|featuring|with|,)\s*`)
)

func (p *AppleMusicProvider) getWebToken(ctx context.Context) (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	now := time.Now()
	if p.cachedWebToken != "" && now.Before(p.webTokenExpires) {
		return p.cachedWebToken, nil
	}
	if now.Before(p.webTokenFailUntil) {
		return "", fmt.Errorf("apple music: web token fetch in cooldown")
	}

	tokCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(tokCtx, http.MethodGet, "https://music.apple.com/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	resp, err := p.client.Do(req)
	if err != nil {
		p.webTokenFailUntil = time.Now().Add(15 * time.Second)
		return "", fmt.Errorf("fetch apple music home: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	htmlBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		p.webTokenFailUntil = time.Now().Add(15 * time.Second)
		return "", err
	}

	m := reScriptTag.FindSubmatch(htmlBytes)
	if len(m) < 2 {
		p.webTokenFailUntil = time.Now().Add(30 * time.Second)
		return "", fmt.Errorf("apple music: could not find index script tag")
	}
	scriptPath := string(m[1])
	scriptURL := "https://music.apple.com" + scriptPath

	sReq, err := http.NewRequestWithContext(tokCtx, http.MethodGet, scriptURL, nil)
	if err != nil {
		return "", err
	}
	sReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	sResp, err := p.client.Do(sReq)
	if err != nil {
		p.webTokenFailUntil = time.Now().Add(15 * time.Second)
		return "", fmt.Errorf("fetch apple music script: %w", err)
	}
	defer func() { _ = sResp.Body.Close() }()

	jsBytes, err := io.ReadAll(sResp.Body)
	if err != nil {
		p.webTokenFailUntil = time.Now().Add(15 * time.Second)
		return "", err
	}

	if jwtM := reDirectJWT.FindSubmatch(jsBytes); len(jwtM) >= 2 {
		p.cachedWebToken = string(jwtM[1])
		p.webTokenExpires = time.Now().Add(2 * time.Hour)
		return p.cachedWebToken, nil
	}

	vm := reTokenVar.FindSubmatch(jsBytes)
	if len(vm) < 2 {
		p.webTokenFailUntil = time.Now().Add(30 * time.Second)
		return "", fmt.Errorf("apple music: could not find authorization token variable")
	}
	tokenVar := regexp.QuoteMeta(string(vm[1]))

	reTokenVal, err := regexp.Compile(tokenVar + `\s*=\s*"([^"]+)"`)
	if err != nil {
		p.webTokenFailUntil = time.Now().Add(30 * time.Second)
		return "", err
	}
	valM := reTokenVal.FindSubmatch(jsBytes)
	if len(valM) < 2 {
		p.webTokenFailUntil = time.Now().Add(30 * time.Second)
		return "", fmt.Errorf("apple music: could not find authorization token value")
	}

	p.cachedWebToken = string(valM[1])
	p.webTokenExpires = time.Now().Add(2 * time.Hour)
	return p.cachedWebToken, nil
}

func (p *AppleMusicProvider) getAccountStorefront(ctx context.Context, accountIdx int) string {
	if acc, ok := p.mgm.At(accountIdx); ok && strings.TrimSpace(acc.STOREFRONT) != "" {
		return strings.TrimSpace(acc.STOREFRONT)
	}

	p.storefrontMu.Lock()
	if p.cachedStorefront != "" {
		sf := p.cachedStorefront
		p.storefrontMu.Unlock()
		return sf
	}
	p.storefrontMu.Unlock()

	headers, err := p.getAuthHeaders(ctx, accountIdx)
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
				if json.NewDecoder(resp.Body).Decode(&sfResp) == nil && len(sfResp.Data) > 0 && sfResp.Data[0].ID != "" {
					sf := strings.TrimSpace(sfResp.Data[0].ID)
					p.storefrontMu.Lock()
					p.cachedStorefront = sf
					p.storefrontMu.Unlock()
					return sf
				}
			}
		}
	}

	for i := 0; i < p.mgm.Count(); i++ {
		if acc, ok := p.mgm.At(i); ok && strings.TrimSpace(acc.STOREFRONT) != "" {
			return strings.TrimSpace(acc.STOREFRONT)
		}
	}

	return ""
}

func (p *AppleMusicProvider) GetStorefront(ctx context.Context) (string, error) {
	sf := p.getAccountStorefront(ctx, p.mgm.CurrentIndex())
	if sf == "" {
		return "", fmt.Errorf("no storefront configured or detected for apple music account")
	}
	return sf, nil
}

func (p *AppleMusicProvider) SearchByISRC(ctx context.Context, isrc, storefront string) (*appleSong, error) {
	cacheKey := storefront + ":" + strings.ToLower(strings.TrimSpace(isrc))
	if p.isrcCache != nil {
		if entry, ok := p.isrcCache.Get(cacheKey); ok && entry != nil {
			if time.Since(entry.created) < appleSearchCacheTTL {
				return entry.song, nil
			}
		}
	}

	searchURL := fmt.Sprintf("%s/catalog/%s/songs?filter[isrc]=%s", appleBaseURL, storefront, url.QueryEscape(isrc))

	resp, err := p.makeAppleMusicRequest(ctx, searchURL, nil, 0, 0, p.mgm.CurrentIndex())
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
	var matched *appleSong
	if len(res.Data) > 0 {
		matched = &res.Data[0]
	}
	if p.isrcCache != nil {
		p.isrcCache.Add(cacheKey, &appleISRCCacheEntry{song: matched, created: time.Now()})
	}
	return matched, nil
}

func (p *AppleMusicProvider) GetSongByID(ctx context.Context, id, storefront string) (*appleSong, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	cacheKey := storefront + ":" + id
	if p.idCache != nil {
		if entry, ok := p.idCache.Get(cacheKey); ok && entry != nil {
			if time.Since(entry.created) < appleSearchCacheTTL {
				return entry.song, nil
			}
		}
	}

	songURL := fmt.Sprintf("%s/catalog/%s/songs/%s", appleBaseURL, storefront, url.PathEscape(id))

	resp, err := p.makeAppleMusicRequest(ctx, songURL, nil, 0, 0, p.mgm.CurrentIndex())
	if err != nil {
		if isAppleStatus(err, http.StatusNotFound) {
			if p.idCache != nil {
				p.idCache.Add(cacheKey, &appleISRCCacheEntry{song: nil, created: time.Now()})
			}
			return nil, nil
		}
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
	var matched *appleSong
	if len(res.Data) > 0 {
		matched = &res.Data[0]
	}
	if p.idCache != nil {
		p.idCache.Add(cacheKey, &appleISRCCacheEntry{song: matched, created: time.Now()})
	}
	return matched, nil
}

func (p *AppleMusicProvider) SearchSongBySuggestions(ctx context.Context, query, storefront string) ([]appleSong, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	cacheKey := storefront + ":" + strings.ToLower(strings.TrimSpace(query))
	if p.suggestionsCache != nil {
		if entry, ok := p.suggestionsCache.Get(cacheKey); ok && entry != nil {
			if time.Since(entry.created) < appleSearchCacheTTL {
				return entry.songs, nil
			}
		}
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

	resp, err := p.makeAppleMusicRequest(ctx, sugURL, nil, 0, 0, p.mgm.CurrentIndex())
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
	if p.suggestionsCache != nil {
		p.suggestionsCache.Add(cacheKey, &appleSongCacheEntry{songs: songs, created: time.Now()})
	}
	return songs, nil
}

func (p *AppleMusicProvider) SearchSong(ctx context.Context, query, storefront string) ([]appleSong, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	cacheKey := storefront + ":" + strings.ToLower(strings.TrimSpace(query))
	if p.searchCache != nil {
		if entry, ok := p.searchCache.Get(cacheKey); ok && entry != nil {
			if time.Since(entry.created) < appleSearchCacheTTL {
				return entry.songs, nil
			}
		}
	}

	searchURL := fmt.Sprintf("%s/catalog/%s/search?types=songs&term=%s", appleBaseURL, storefront, url.QueryEscape(query))

	resp, err := p.makeAppleMusicRequest(ctx, searchURL, nil, 0, 0, p.mgm.CurrentIndex())
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
	songs := res.Results.Songs.Data
	if p.searchCache != nil {
		p.searchCache.Add(cacheKey, &appleSongCacheEntry{songs: songs, created: time.Now()})
	}
	return songs, nil
}

func (p *AppleMusicProvider) SearchBestMatch(ctx context.Context, title, artist, album string, durationSec float64, songISRC, songPlatformID string) (*appleSong, error) {
	storefront := p.getAccountStorefront(ctx, p.mgm.CurrentIndex())
	if storefront == "" {
		return nil, nil
	}

	queries := buildSearchQueries(title, artist, album)
	if len(queries) == 0 {
		return nil, nil
	}

	var candidates []appleSong
	maxTry := len(queries)
	if maxTry > 2 {
		maxTry = 2
	}
	for i := 0; i < maxTry; i++ {
		if sugSongs, err := p.SearchSongBySuggestions(ctx, queries[i], storefront); err == nil && len(sugSongs) > 0 {
			candidates = append(candidates, sugSongs...)
			if best := matchAppleSongs(candidates, title, artist, album, durationSec); best != nil {
				return best, nil
			}
		}
	}

	for i := 0; i < maxTry; i++ {
		songs, err := p.SearchSong(ctx, queries[i], storefront)
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
	seen := make(map[string]bool, 6)
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

	if a != "" && t != "" {
		add(a + " " + t)
	}

	// Split multi-artists (e.g. "Jung Kook & BTS" -> "Jung Kook")
	parts := reMultiArtist.Split(a, -1)
	if len(parts) > 1 {
		primary := strings.TrimSpace(parts[0])
		if primary != "" && t != "" {
			add(t + " " + primary)
			add(primary + " " + t)
		}
	}

	if t != "" {
		add(t)
	}

	return queries
}

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

func (p *AppleMusicProvider) SearchCatalog(ctx context.Context, query string) ([]domain.SongCatalogItem, error) {
	storefront, err := p.GetStorefront(ctx)
	if err != nil || storefront == "" {
		return nil, err
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

func (p *AppleMusicProvider) GetMetadata(ctx context.Context, title, artist, album string, durationSec float64) (map[string]interface{}, error) {
	storefront, err := p.GetStorefront(ctx)
	if err != nil || storefront == "" {
		return nil, err
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

	if t != "" && a != "" {
		queries = append(queries, t+" "+a)
	}

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
	if len(parts) > 0 && al != "" {
		queries = append(queries, strings.Join(parts, " "))
	}

	if t != "" && a != "" {
		queries = append(queries, a+" "+t)
	}

	if t != "" {
		queries = append(queries, t)
	}
	return queries
}
