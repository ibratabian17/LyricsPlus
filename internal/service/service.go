// Package service normalizes, caches, and serves canonical lyrics payloads.
package service

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/orchestrator"
	"lyricsplus/backend/internal/parsers"
	"lyricsplus/backend/internal/similarity"
	"lyricsplus/backend/internal/storage"
)

// Service exposes lyrics retrieval with caching, singleflight dedup and
// fire-and-forget background persistence.
type Service struct {
	Dedup    *orchestrator.Dedup
	Store    *storage.Store
	MemCache *storage.MemoryCache
	Logger   *logger.Logger
}

// RawResult is the raw source payload for /v1/raw/get.
type RawResult struct {
	Source  string
	Raw     string
	TotalMs int64
}

type NotFoundError struct {
	Message        string
	Sources        []string
	SongTitle      string
	SongArtist     string
	SongAlbum      string
	TotalMs        int64
	SourceStatuses map[string]domain.SourceStatus
	Source         string
}

func (e *NotFoundError) Error() string { return e.Message }

func decorateCacheHit(resp *domain.LyricsResponse, q domain.SearchQuery, preferredSources []string, pipeline time.Duration) {
	if resp == nil {
		return
	}
	if resp.Cached == "" || resp.Cached == domain.CacheNone {
		resp.Cached = domain.CacheDatabase
	}
	if resp.ProcessingTime == nil {
		resp.ProcessingTime = &domain.ProcessTiming{}
	}
	if resp.ProcessingTime.TotalElapsedMs == 0 {
		resp.ProcessingTime.TotalElapsedMs = pipeline.Milliseconds()
	}
	if len(resp.ProcessingTime.SourcesStatus) > 0 {
		return
	}
	winner := ""
	if resp.ProcessingTime.WinnerSource != nil {
		winner = *resp.ProcessingTime.WinnerSource
	} else if resp.Metadata.Source != "" {
		winner = resp.Metadata.Source
	}
	winName := providerNameForSource(winner)
	status := make(map[string]domain.SourceStatus)
	for _, src := range orchestrator.SourceOrder(q, preferredSources) {
		status[src] = domain.SourceStatus{Status: "SKIP"}
	}
	if winName != "" {
		status[winName] = domain.SourceStatus{Status: "OK"}
	}
	resp.ProcessingTime.SourcesStatus = status
}

func (s *Service) buildNotFound(q domain.SearchQuery, preferredSources []string, res *orchestrator.Result, elapsed time.Duration) *NotFoundError {
	sources := orchestrator.SourceOrder(q, preferredSources)
	status := make(map[string]domain.SourceStatus, len(sources))
	for _, src := range sources {
		status[src] = domain.SourceStatus{Status: "SKIP"}
	}
	if res != nil {
		for name, out := range res.SourcesStatus {
			ms := out.ElapsedMs
			status[name] = domain.SourceStatus{Status: out.Status, ElapsedMs: &ms}
		}
	}
	return &NotFoundError{
		Message:        "Lyrics not found in sources: " + strings.Join(sources, ", "),
		Sources:        sources,
		SongTitle:      q.Title,
		SongArtist:     q.Artist,
		SongAlbum:      q.Album,
		TotalMs:        elapsed.Milliseconds(),
		SourceStatuses: status,
	}
}

type rawCacheEntry struct {
	Source string `json:"source"`
	Raw    string `json:"raw"`
}

func lyricsCacheKey(q domain.SearchQuery) string { return "lyrics::" + q.ContentKey() }
func rawCacheKey(q domain.SearchQuery) string    { return "raw::" + q.ContentKey() }

// FetchLyrics resolves lyrics for a query, consulting the memory cache and
// SQLite store before racing providers. Cache writes are never blocking.
func (s *Service) FetchLyrics(ctx context.Context, q domain.SearchQuery, preferredSources []string, forceReload bool) (*domain.LyricsResponse, error) {
	start := time.Now()
	if !forceReload {
		if resp, ok := s.fromMemory(ctx, q); ok {
			decorateCacheHit(resp, q, preferredSources, time.Since(start))
			return resp, nil
		}
		if resp, ok := s.fromStore(ctx, q); ok {
			decorateCacheHit(resp, q, preferredSources, time.Since(start))
			return resp, nil
		}
	}

	res, err := s.Dedup.Get(ctx, q, preferredSources)
	if err != nil {
		return nil, err
	}
	if res == nil || res.Resp == nil || len(res.Resp.Lyrics) == 0 {
		return nil, s.buildNotFound(q, preferredSources, res, time.Since(start))
	}

	resp := res.Resp
	switch {
	case strings.EqualFold(res.Source, "apple") || resp.Metadata.Source == "Apple Music":
		resp.Metadata.Source = "Apple"
	case strings.EqualFold(res.Source, "lyricsplus") && !strings.HasPrefix(resp.Metadata.Source, "Lyrics+"):
		resp.Metadata.Source = "Lyrics+"
	case strings.EqualFold(res.Source, "qq"):
		resp.Metadata.Source = "QQ Music"
	case strings.EqualFold(res.Source, "deezer"):
		resp.Metadata.Source = "Deezer"
	case strings.EqualFold(res.Source, "musixmatch") || strings.EqualFold(res.Source, "musixmatch-word"):
		resp.Metadata.Source = "Musixmatch"
	case strings.EqualFold(res.Source, "spotify"):
		if resp.Metadata.Source == "" {
			resp.Metadata.Source = "Spotify"
		}
	}
	now := time.Now()
	resp.ProcessingTime = buildProcessTiming(res, q, now.UnixMilli())
	if resp.KpoeTools == "" {
		resp.KpoeTools = "lyricsplus"
	}
	if resp.Cached == "" {
		resp.Cached = domain.CacheNone
	}

	// Fire-and-forget persistence: never block the HTTP response.
	// Qaple results are synthesized on-the-fly from live sources and are never saved to the SQLite store.
	if s.Store != nil && resp.RawData != "" && res.Source != "qaple" && !strings.Contains(resp.Metadata.Source, "with QQ") {
		winner := res.Source
		go func() {
			ctx2, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			contentJSON, err := json.Marshal(resp)
			if err != nil {
				return
			}
			title := q.Title
			artist := q.Artist
			album := q.Album
			if resp.ProcessingTime != nil && resp.ProcessingTime.SelectedSongMetadata != nil {
				if resp.ProcessingTime.SelectedSongMetadata.Title != "" {
					title = resp.ProcessingTime.SelectedSongMetadata.Title
				}
				if resp.ProcessingTime.SelectedSongMetadata.Artist != "" {
					artist = resp.ProcessingTime.SelectedSongMetadata.Artist
				}
				if resp.ProcessingTime.SelectedSongMetadata.Album != "" {
					album = resp.ProcessingTime.SelectedSongMetadata.Album
				}
			}
			row := &storage.Row{
				Filename:    storage.CanonicalFilename(artist, title, album, q.Duration, q.ISRC, q.PlatformID, extFor(resp.Metadata.Source)),
				ContentJSON: contentJSON,
				ISRC:        q.ISRC,
				PlatformID:  q.PlatformID,
				Source:      winner,
				Title:       title,
				Artist:      artist,
				DurationMS:  q.Duration,
				CreatedAt:   now,
			}
			if err := s.Store.SaveLyrics(ctx2, row); err != nil {
				if s.Logger != nil {
					s.Logger.Errorf("background cache save failed: %v", err)
				}
			}
		}()
	}

	s.cacheResponse(ctx, q, resp, res.Source)
	return resp, nil
}

// FetchRaw returns the raw provider payload for /v1/raw/get.
func (s *Service) FetchRaw(ctx context.Context, q domain.SearchQuery, preferredSources []string, forceReload bool) (*RawResult, error) {
	start := time.Now()
	if !forceReload {
		if s.MemCache != nil {
			if e, ok := s.MemCache.Get(rawCacheKey(q)); ok && e != nil {
				var entry rawCacheEntry
				if json.Unmarshal(e.Body, &entry) == nil && entry.Raw != "" {
					if matchSource(entry.Source, preferredSources) {
						return &RawResult{Source: entry.Source, Raw: entry.Raw}, nil
					}
				}
			}
		}
	}

	res, err := s.Dedup.Get(ctx, q, preferredSources)
	if err != nil {
		return nil, err
	}
	if res == nil || res.Resp == nil || len(res.Resp.Lyrics) == 0 {
		return nil, s.buildNotFound(q, preferredSources, res, time.Since(start))
	}
	if res.Resp.RawData == "" {
		nf := s.buildNotFound(q, preferredSources, res, time.Since(start))
		nf.Message = "Raw data is not available for this result"
		nf.Source = res.Source
		return nil, nf
	}

	raw := &RawResult{
		Source:  res.Source,
		Raw:     res.Resp.RawData,
		TotalMs: res.Elapsed.Milliseconds(),
	}
	if s.MemCache != nil {
		if b, err := json.Marshal(rawCacheEntry{Source: res.Source, Raw: res.Resp.RawData}); err == nil {
			s.MemCache.Set(rawCacheKey(q), &storage.CacheEntry{Body: b, StoredAt: time.Now()})
		}
	}
	return raw, nil
}

func (s *Service) fromMemory(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, bool) {
	if s.MemCache == nil {
		return nil, false
	}
	e, ok := s.MemCache.Get(lyricsCacheKey(q))
	if !ok || e == nil {
		return nil, false
	}
	var resp domain.LyricsResponse
	if json.Unmarshal(e.Body, &resp) != nil || len(resp.Lyrics) == 0 {
		return nil, false
	}
	if resp.Metadata.Source == "Apple Music" {
		resp.Metadata.Source = "Apple"
	} else if strings.EqualFold(resp.Metadata.Source, "lyricsplus") {
		resp.Metadata.Source = "Lyrics+"
	}
	if !matchSource(resp.Metadata.Source, q.Sources) {
		return nil, false
	}
	if resp.ProcessingTime == nil {
		resp.ProcessingTime = &domain.ProcessTiming{}
	}
	if resp.ProcessingTime.WinnerSource == nil {
		winner := providerNameForSource(resp.Metadata.Source)
		resp.ProcessingTime.WinnerSource = &winner
	}
	if resp.ProcessingTime.SelectedSongMetadata == nil {
		resp.ProcessingTime.SelectedSongMetadata = &domain.PickedSongMetadata{
			Source:         providerDisplayName(*resp.ProcessingTime.WinnerSource),
			Title:          q.Title,
			Artist:         q.Artist,
			Album:          q.Album,
			SongISRC:       q.ISRC,
			SongPlatformID: q.PlatformID,
		}
	} else {
		if resp.ProcessingTime.SelectedSongMetadata.Title == "" {
			resp.ProcessingTime.SelectedSongMetadata.Title = q.Title
		}
		if resp.ProcessingTime.SelectedSongMetadata.Artist == "" {
			resp.ProcessingTime.SelectedSongMetadata.Artist = q.Artist
		}
		if resp.ProcessingTime.SelectedSongMetadata.Album == "" {
			resp.ProcessingTime.SelectedSongMetadata.Album = q.Album
		}
		if resp.ProcessingTime.SelectedSongMetadata.SongISRC == "" {
			resp.ProcessingTime.SelectedSongMetadata.SongISRC = q.ISRC
		}
		if resp.ProcessingTime.SelectedSongMetadata.SongPlatformID == "" {
			resp.ProcessingTime.SelectedSongMetadata.SongPlatformID = q.PlatformID
		}
	}
	return &resp, true
}

func (s *Service) fromStore(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, bool) {
	if s.Store == nil {
		return nil, false
	}
	dbCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var row *storage.Row
	bestPrio := -1
	var candidates []*storage.Row

	// 1. Exact ID match (ISRC / Platform ID) via B-Tree index (<1ms)
	if q.ISRC != "" || q.PlatformID != "" {
		if r, ok := s.Store.GetExact(dbCtx, q.ISRC, q.PlatformID); ok && r != nil {
			src := rowSource(r)
			prio := sourcePriority(src, q.Sources)
			if prio >= 0 {
				row = r
				bestPrio = prio
			}
		} else if q.IDOnly() {
			return nil, false
		}
	}

	// 2. Exact Title + Artist match via idx_lyrics_title_artist B-Tree index (<1ms)
	if (row == nil || bestPrio > 0) && (q.Title != "" && q.Artist != "") {
		if rows, ok := s.Store.GetByTitleArtist(dbCtx, q.Title, q.Artist); ok && len(rows) > 0 {
			candidates = append(candidates, rows...)
			if matched, p := pickBestRow(rows, q); matched != nil {
				if bestPrio == -1 || p < bestPrio {
					row = matched
					bestPrio = p
				}
			}
		}
	}

	// 3. FTS5 full-text search (sub-ms inverted index), Go-side fuzzy scoring.
	// Falls back to LIKE keyword scan only if FTS5 fails (e.g. table not yet populated).
	if (row == nil || bestPrio > 0) && (q.Title != "" || q.Artist != "") {
		if rows, ok := s.Store.GetByFTS5(dbCtx, q.Title, q.Artist); ok && len(rows) > 0 {
			candidates = append(candidates, rows...)
			if matched, p := pickBestRow(rows, q); matched != nil {
				if bestPrio == -1 || p < bestPrio {
					row = matched
					bestPrio = p
				}
			}
		} else {
			keywords := append(storage.ExtractKeywords(q.Title), storage.ExtractKeywords(q.Artist)...)
			if rows, ok2 := s.Store.GetExisting(dbCtx, keywords); ok2 && len(rows) > 0 {
				candidates = append(candidates, rows...)
				if matched, p := pickBestRow(rows, q); matched != nil {
					if bestPrio == -1 || p < bestPrio {
						row = matched
						bestPrio = p
					}
				}
			}
		}
	}
	if row == nil {
		return nil, false
	}

	// If explicit sources were specified and top preference (index 0) was not found in cache,
	// do not return a lower-priority fallback from cache so providers can race live.
	if len(q.Sources) > 0 && bestPrio > 0 {
		return nil, false
	}

	var resp *domain.LyricsResponse
	seen := make(map[int64]struct{}, len(candidates)+1)
	for _, cand := range append([]*storage.Row{row}, candidates...) {
		if cand == nil {
			continue
		}
		if _, dup := seen[cand.ID]; dup {
			continue
		}
		seen[cand.ID] = struct{}{}
		content := cand.ContentJSON
		if len(content) == 0 {
			c, err := s.Store.GetContent(dbCtx, cand.ID)
			if err != nil || len(c) == 0 {
				continue
			}
			content = c
		}
		parsed := parseStoredContent(cand, content)
		if parsed != nil && len(parsed.Lyrics) > 0 {
			resp = parsed
			row = cand
			break
		}
	}
	if resp == nil {
		return nil, false
	}
	winner := row.Source
	if winner == "" {
		if resp.ProcessingTime != nil && resp.ProcessingTime.WinnerSource != nil && *resp.ProcessingTime.WinnerSource != "" {
			winner = *resp.ProcessingTime.WinnerSource
		} else {
			winner = providerNameForSource(resp.Metadata.Source)
		}
	}
	finalPrio := sourcePriority(winner, q.Sources)
	if finalPrio < 0 || (len(q.Sources) > 0 && finalPrio > 0) {
		return nil, false
	}
	resp = parsers.NormalizeV2(resp)
	resp.RawData = string(row.ContentJSON)
	resp.Cached = domain.CacheDatabase
	switch {
	case strings.EqualFold(row.Source, "apple") || resp.Metadata.Source == "Apple Music":
		resp.Metadata.Source = "Apple"
	case strings.EqualFold(row.Source, "lyricsplus") && !strings.HasPrefix(resp.Metadata.Source, "Lyrics+"):
		resp.Metadata.Source = "Lyrics+"
	case strings.EqualFold(row.Source, "qq"):
		resp.Metadata.Source = "QQ Music"
	case strings.EqualFold(row.Source, "deezer"):
		resp.Metadata.Source = "Deezer"
	case strings.EqualFold(row.Source, "musixmatch") || strings.EqualFold(row.Source, "musixmatch-word"):
		resp.Metadata.Source = "Musixmatch"
	case strings.EqualFold(row.Source, "spotify"):
		if resp.Metadata.Source == "" {
			resp.Metadata.Source = "Spotify"
		}
	}
	if resp.ProcessingTime == nil {
		resp.ProcessingTime = &domain.ProcessTiming{}
	}
	if resp.ProcessingTime.WinnerSource == nil {
		resp.ProcessingTime.WinnerSource = &winner
	}
	title := row.Title
	if title == "" {
		title = q.Title
	}
	artist := row.Artist
	if artist == "" {
		artist = q.Artist
	}
	album := q.Album
	isrc := row.ISRC
	if isrc == "" {
		isrc = q.ISRC
	}
	platID := row.PlatformID
	if platID == "" {
		platID = q.PlatformID
	}
	if resp.ProcessingTime.SelectedSongMetadata == nil {
		resp.ProcessingTime.SelectedSongMetadata = &domain.PickedSongMetadata{
			Source:         providerDisplayName(*resp.ProcessingTime.WinnerSource),
			Title:          title,
			Artist:         artist,
			Album:          album,
			SongISRC:       isrc,
			SongPlatformID: platID,
		}
	} else {
		if resp.ProcessingTime.SelectedSongMetadata.Title == "" {
			resp.ProcessingTime.SelectedSongMetadata.Title = title
		}
		if resp.ProcessingTime.SelectedSongMetadata.Artist == "" {
			resp.ProcessingTime.SelectedSongMetadata.Artist = artist
		}
		if resp.ProcessingTime.SelectedSongMetadata.Album == "" {
			resp.ProcessingTime.SelectedSongMetadata.Album = album
		}
		if resp.ProcessingTime.SelectedSongMetadata.SongISRC == "" {
			resp.ProcessingTime.SelectedSongMetadata.SongISRC = isrc
		}
		if resp.ProcessingTime.SelectedSongMetadata.SongPlatformID == "" {
			resp.ProcessingTime.SelectedSongMetadata.SongPlatformID = platID
		}
	}
	return resp, true
}

func parseStoredContent(row *storage.Row, content []byte) *domain.LyricsResponse {
	trimmedRaw := strings.TrimSpace(string(content))
	src := row.Source
	if src == "" {
		src = rowSource(row)
	}
	isXML := strings.HasPrefix(trimmedRaw, "<")

	switch {
	case strings.EqualFold(src, "apple"):
		if isXML {
			if p, err := parsers.TTMLToJSON(content); err == nil && p != nil && len(p.Lyrics) > 0 {
				p.Metadata.Source = "Apple"
				return p
			}
		}
		res := parseNormalizedJSON(trimmedRaw)
		if res != nil {
			res.Metadata.Source = "Apple"
		}
		return res
	case strings.EqualFold(src, "qq"):
		if isXML {
			p := parsers.ParseQQQRC(trimmedRaw, parsers.ExactMetadata{
				Title:      row.Title,
				Artist:     row.Artist,
				DurationMs: row.DurationMS,
				PlatformID: row.PlatformID,
			})
			if p != nil && len(p.Lyrics) > 0 {
				return p
			}
		}
		return parseNormalizedJSON(trimmedRaw)
	case strings.EqualFold(src, "deezer"):
		if p, err := parsers.NormalizeDeezerLyrics(content); err == nil && p != nil && len(p.Lyrics) > 0 {
			p.Metadata.Source = "Deezer"
			return p
		}
		res := parseNormalizedJSON(trimmedRaw)
		if res != nil {
			res.Metadata.Source = "Deezer"
		}
		return res
	case strings.EqualFold(src, "musixmatch"):
		if p, err := parsers.ConvertMusixmatchToJSON(content, false); err == nil && p != nil && len(p.Lyrics) > 0 {
			p.Metadata.Source = "Musixmatch"
			return p
		}
		res := parseNormalizedJSON(trimmedRaw)
		if res != nil {
			res.Metadata.Source = "Musixmatch"
		}
		return res
	case strings.EqualFold(src, "spotify"):
		if p, err := parsers.ConvertSpotifyToJSON(content); err == nil && p != nil && len(p.Lyrics) > 0 {
			if p.Metadata.Source == "" {
				p.Metadata.Source = "Spotify"
			}
			return p
		}
		res := parseNormalizedJSON(trimmedRaw)
		if res != nil && res.Metadata.Source == "" {
			res.Metadata.Source = "Spotify"
		}
		return res
	case strings.EqualFold(src, "lyricsplus"), src == "":
		if strings.Contains(trimmedRaw, `"isLineEnding"`) {
			coerced := kpoeToolsRe.ReplaceAllString(trimmedRaw, `"KpoeTools": "$1"`)
			var v1 domain.V1Response
			if json.Unmarshal([]byte(coerced), &v1) == nil && len(v1.Lyrics) > 0 {
				if p := parsers.V1ToV2(&v1); p != nil && len(p.Lyrics) > 0 {
					p.Metadata.Source = "Lyrics+"
					return p
				}
			}
		}
		res := parseNormalizedJSON(trimmedRaw)
		if res != nil && strings.EqualFold(src, "lyricsplus") && !strings.HasPrefix(res.Metadata.Source, "Lyrics+") {
			res.Metadata.Source = "Lyrics+"
		}
		return res
	}

	if isXML {
		if p, err := parsers.TTMLToJSON(content); err == nil && p != nil && len(p.Lyrics) > 0 {
			p.Metadata.Source = "Apple"
			return p
		}
		p := parsers.ParseQQQRC(trimmedRaw, parsers.ExactMetadata{
			Title:      row.Title,
			Artist:     row.Artist,
			DurationMs: row.DurationMS,
			PlatformID: row.PlatformID,
		})
		if p != nil && len(p.Lyrics) > 0 {
			p.Metadata.Source = "QQ Music"
			return p
		}
	}
	if strings.Contains(trimmedRaw, `"syncType"`) && strings.Contains(trimmedRaw, `"lines"`) {
		if p, err := parsers.ConvertSpotifyToJSON(content); err == nil && p != nil && len(p.Lyrics) > 0 {
			return p
		}
	}
	if strings.Contains(trimmedRaw, `"message"`) && (strings.Contains(trimmedRaw, `"subtitle_body"`) || strings.Contains(trimmedRaw, `"richsync_body"`)) {
		if p, err := parsers.ConvertMusixmatchToJSON(content, false); err == nil && p != nil && len(p.Lyrics) > 0 {
			return p
		}
	}
	if strings.Contains(trimmedRaw, `"isLineEnding"`) {
		coerced := kpoeToolsRe.ReplaceAllString(trimmedRaw, `"KpoeTools": "$1"`)
		var v1 domain.V1Response
		if json.Unmarshal([]byte(coerced), &v1) == nil && len(v1.Lyrics) > 0 {
			if p := parsers.V1ToV2(&v1); p != nil && len(p.Lyrics) > 0 {
				return p
			}
		}
	}
	return parseNormalizedJSON(trimmedRaw)
}

func parseNormalizedJSON(trimmedRaw string) *domain.LyricsResponse {
	var resp domain.LyricsResponse
	if json.Unmarshal(coercedForKpoe(trimmedRaw), &resp) == nil && len(resp.Lyrics) > 0 {
		if resp.Metadata.Source == "Apple Music" {
			resp.Metadata.Source = "Apple"
		}
		return &resp
	}
	return nil
}

var kpoeToolsRe = regexp.MustCompile(`"KpoeTools"\s*:\s*(-?[0-9]+(?:\.[0-9]+)?)`)

func coercedForKpoe(trimmedRaw string) []byte {
	return []byte(kpoeToolsRe.ReplaceAllString(trimmedRaw, `"KpoeTools": "$1"`))
}

func (s *Service) cacheResponse(ctx context.Context, q domain.SearchQuery, resp *domain.LyricsResponse, winner string) {
	if s.MemCache == nil {
		return
	}
	if b, err := json.Marshal(resp); err == nil {
		s.MemCache.Set(lyricsCacheKey(q), &storage.CacheEntry{Body: b, StoredAt: time.Now()})
	}
	if resp.RawData != "" {
		if b, err := json.Marshal(rawCacheEntry{Source: winner, Raw: resp.RawData}); err == nil {
			s.MemCache.Set(rawCacheKey(q), &storage.CacheEntry{Body: b, StoredAt: time.Now()})
		}
	}
}

func buildProcessTiming(res *orchestrator.Result, q domain.SearchQuery, lastProcessed int64) *domain.ProcessTiming {
	statuses := make(map[string]domain.SourceStatus, len(res.SourcesStatus))
	for name, out := range res.SourcesStatus {
		ms := out.ElapsedMs
		statuses[name] = domain.SourceStatus{Status: out.Status, ElapsedMs: &ms}
	}
	winner := res.Source
	prio := res.Priority

	var selMeta *domain.PickedSongMetadata
	if res.Resp != nil && res.Resp.ProcessingTime != nil && res.Resp.ProcessingTime.SelectedSongMetadata != nil {
		selMeta = res.Resp.ProcessingTime.SelectedSongMetadata
	} else {
		sourceName := providerDisplayName(winner)
		selMeta = &domain.PickedSongMetadata{
			Source:         sourceName,
			Title:          q.Title,
			Artist:         q.Artist,
			Album:          q.Album,
			SongISRC:       q.ISRC,
			SongPlatformID: q.PlatformID,
		}
	}

	totalMs := res.Elapsed.Milliseconds()
	if res.Pipeline > 0 {
		totalMs = res.Pipeline.Milliseconds()
	}
	return &domain.ProcessTiming{
		LastProcessed:        lastProcessed,
		TotalElapsedMs:       totalMs,
		WinnerSource:         &winner,
		SyncPriority:         &prio,
		SourcesStatus:        statuses,
		SelectedSongMetadata: selMeta,
	}
}

func providerDisplayName(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	switch s {
	case "spotify":
		return "Spotify"
	case "apple", "apple music":
		return "Apple"
	case "qq", "qq music":
		return "QQ Music"
	case "musixmatch":
		return "Musixmatch"
	case "musixmatch-word":
		return "Musixmatch (Word)"
	case "deezer":
		return "Deezer"
	case "qaple":
		return "Qaple"
	case "lyrics+", "lyricsplus":
		return "Lyrics+"
	default:
		if strings.Contains(s, "qaple") || strings.Contains(s, "with qq") {
			return "Qaple"
		}
		return "Lyrics+"
	}
}

// ProviderNameForSource maps a metadata source label back to the racer's
// provider name, so cache hits can report a winner.
func ProviderNameForSource(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	switch s {
	case "spotify":
		return "spotify"
	case "apple", "apple music":
		return "apple"
	case "qq music", "qq", "qqmusic":
		return "qq"
	case "musixmatch":
		return "musixmatch"
	case "musixmatch-word":
		return "musixmatch-word"
	case "deezer":
		return "deezer"
	case "qaple":
		return "qaple"
	case "lyrics+", "lyricsplus":
		return "lyricsplus"
	default:
		if strings.Contains(s, "qaple") || strings.Contains(s, "with qq") {
			return "qaple"
		}
		return "lyricsplus"
	}
}

var providerNameForSource = ProviderNameForSource

// sourcePriority returns the 0-indexed position of candidate in allowed,
// or -1 if candidate is not allowed. If allowed is empty, returns 0.
func sourcePriority(candidate string, allowed []string) int {
	if len(allowed) == 0 {
		return 0
	}
	candNorm := providerNameForSource(candidate)
	for i, a := range allowed {
		aNorm := providerNameForSource(a)
		if aNorm == candNorm {
			return i
		}
		if aNorm == "musixmatch" && candNorm == "musixmatch-word" {
			return i
		}
	}
	return -1
}

func matchSource(candidate string, allowed []string) bool {
	return sourcePriority(candidate, allowed) >= 0
}

func rowSource(r *storage.Row) string {
	if r == nil {
		return ""
	}
	if r.Source != "" {
		return r.Source
	}
	trimmed := strings.TrimSpace(string(r.ContentJSON))
	if strings.HasPrefix(trimmed, "<") {
		if strings.HasSuffix(strings.ToLower(r.Filename), ".ttml") {
			return "apple"
		}
		if strings.HasSuffix(strings.ToLower(r.Filename), ".qrc") {
			return "qq"
		}
	}
	var partial struct {
		Metadata struct {
			Source string `json:"source"`
		} `json:"metadata"`
		ProcessingTime *struct {
			WinnerSource *string `json:"winnerSource"`
		} `json:"processingTime"`
	}
	if json.Unmarshal(r.ContentJSON, &partial) == nil {
		if partial.ProcessingTime != nil && partial.ProcessingTime.WinnerSource != nil && *partial.ProcessingTime.WinnerSource != "" {
			return *partial.ProcessingTime.WinnerSource
		}
		if partial.Metadata.Source != "" {
			return partial.Metadata.Source
		}
	}
	return ""
}

func extFor(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	if strings.HasPrefix(s, "lyrics+") || strings.HasPrefix(s, "lyricsplus") {
		return "json"
	}
	switch s {
	case "apple", "apple music":
		return "ttml"
	case "qq", "qq music", "qqmusic":
		return "qrc"
	default:
		if strings.Contains(s, "apple") && !strings.Contains(s, "lyrics") {
			return "ttml"
		}
		if strings.Contains(s, "qq") && !strings.Contains(s, "lyrics") {
			return "qrc"
		}
		return "json"
	}
}

// pickBestRow evaluates duplicate candidates by duration, album similarity, and source preference.
func pickBestRow(rows []*storage.Row, q domain.SearchQuery) (*storage.Row, int) {
	if len(rows) == 0 {
		return nil, -1
	}

	var valid []*storage.Row
	for _, r := range rows {
		if r == nil {
			continue
		}
		if len(q.Sources) > 0 && sourcePriority(rowSource(r), q.Sources) < 0 {
			continue
		}
		valid = append(valid, r)
	}
	if len(valid) == 0 {
		return nil, -1
	}

	// When explicit sources are requested, evaluate in order of source priority
	if len(q.Sources) > 0 {
		for prio := 0; prio < len(q.Sources); prio++ {
			var prioRows []*storage.Row
			for _, r := range valid {
				if sourcePriority(rowSource(r), q.Sources) == prio {
					prioRows = append(prioRows, r)
				}
			}
			if len(prioRows) == 0 {
				continue
			}
			if matched, _ := pickBestRow(prioRows, domain.SearchQuery{Title: q.Title, Artist: q.Artist, Album: q.Album, Duration: q.Duration, ISRC: q.ISRC, PlatformID: q.PlatformID}); matched != nil {
				return matched, prio
			}
		}
		return nil, -1
	}

	queryDurSec := float64(q.Duration) / 1000.0

	// Rank title queries by song similarity
	if q.Title != "" {
		candidates := make([]similarity.SongCandidate, len(valid))
		for i, r := range valid {
			album := ""
			durMs := r.DurationMS
			if r.Filename != "" {
				pf := storage.ParseFilename(r.Filename)
				if pf.Album != "" {
					album = pf.Album
				}
				if durMs <= 0 && pf.DurationMS > 0 {
					durMs = pf.DurationMS
				}
			}
			candidates[i] = similarity.SongCandidate{
				Title:      r.Title,
				Artist:     r.Artist,
				Album:      album,
				DurationMs: durMs,
				ISRC:       r.ISRC,
				PlatformID: r.PlatformID,
				Data:       r,
			}
		}

		best := similarity.FindBestSongMatch(candidates, q.Title, q.Artist, q.Album, queryDurSec, q.ISRC, q.PlatformID)
		if best != nil {
			row := best.Candidate.Data.(*storage.Row)
			prio := sourcePriority(rowSource(row), q.Sources)
			return row, prio
		}
		return nil, -1
	}

	// Artist-only queries fall back to best source priority.
	var best *storage.Row
	bestPrio := -1
	for _, r := range valid {
		src := rowSource(r)
		prio := sourcePriority(src, q.Sources)
		if prio >= 0 && (bestPrio == -1 || prio < bestPrio) {
			best = r
			bestPrio = prio
			if bestPrio == 0 {
				break
			}
		}
	}
	return best, bestPrio
}
