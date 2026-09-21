// Package service normalizes, caches, and serves canonical lyrics payloads.
package service

import (
	"context"
	"encoding/json"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/orchestrator"
	"lyricsplus/backend/internal/parsers"
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

type rawCacheEntry struct {
	Source string `json:"source"`
	Raw    string `json:"raw"`
}

func lyricsCacheKey(q domain.SearchQuery) string { return "lyrics::" + q.NormalizeKey() }
func rawCacheKey(q domain.SearchQuery) string    { return "raw::" + q.NormalizeKey() }

// FetchLyrics resolves lyrics for a query, consulting the memory cache and
// SQLite store before racing providers. Cache writes are never blocking.
func (s *Service) FetchLyrics(ctx context.Context, q domain.SearchQuery, preferredSources []string, forceReload bool) (*domain.LyricsResponse, error) {
	if !forceReload {
		if resp, ok := s.fromMemory(ctx, q); ok {
			return resp, nil
		}
		if resp, ok := s.fromStore(ctx, q); ok {
			return resp, nil
		}
	}

	res, err := s.Dedup.Get(ctx, q, preferredSources)
	if err != nil {
		return nil, err
	}
	if res == nil || res.Resp == nil || len(res.Resp.Lyrics) == 0 {
		return nil, nil
	}

	resp := res.Resp
	now := time.Now()
	if resp.ProcessingTime == nil {
		resp.ProcessingTime = buildProcessTiming(res, now.UnixMilli())
	}
	if resp.KpoeTools == "" {
		resp.KpoeTools = "lyricsplus"
	}

	// Fire-and-forget persistence: never block the HTTP response.
	if s.Store != nil && resp.RawData != "" {
		winner := res.Source
		go func() {
			ctx2, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			contentJSON, err := json.Marshal(resp)
			if err != nil {
				return
			}
			row := &storage.Row{
				Filename:    storage.CanonicalFilename(resp.Metadata.Artist, resp.Metadata.Title, resp.Metadata.Album, q.Duration, q.ISRC, q.PlatformID, extFor(resp.Metadata.Source)),
				ContentJSON: contentJSON,
				ISRC:        q.ISRC,
				PlatformID:  q.PlatformID,
				Source:      winner,
				Title:       resp.Metadata.Title,
				Artist:      resp.Metadata.Artist,
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
	if !forceReload {
		if s.MemCache != nil {
			if e, ok := s.MemCache.Get(rawCacheKey(q)); ok && e != nil {
				var entry rawCacheEntry
				if json.Unmarshal(e.Body, &entry) == nil && entry.Raw != "" {
					return &RawResult{Source: entry.Source, Raw: entry.Raw}, nil
				}
			}
		}
	}

	res, err := s.Dedup.Get(ctx, q, preferredSources)
	if err != nil {
		return nil, err
	}
	if res == nil || res.Resp == nil || res.Resp.RawData == "" {
		return nil, nil
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
	if resp.ProcessingTime == nil {
		resp.ProcessingTime = cacheProcessTiming(providerNameForSource(resp.Metadata.Source), time.Now().UnixMilli(), &resp.Metadata)
	}
	return &resp, true
}

func (s *Service) fromStore(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, bool) {
	if s.Store == nil {
		return nil, false
	}
	var row *storage.Row
	var ok bool
	if q.ISRC != "" || q.PlatformID != "" {
		if row, ok = s.Store.GetExact(ctx, q.ISRC, q.PlatformID); !ok {
			return nil, false
		}
	}
	if row == nil {
		keywords := append(storage.ExtractKeywords(q.Title), storage.ExtractKeywords(q.Artist)...)
		if rows, ok2 := s.Store.GetExisting(ctx, keywords); ok2 && len(rows) > 0 {
			row = rows[0]
		}
	}
	if row == nil || len(row.ContentJSON) == 0 {
		return nil, false
	}
	var resp domain.LyricsResponse
	if err := json.Unmarshal(row.ContentJSON, &resp); err != nil || len(resp.Lyrics) == 0 {
		return nil, false
	}
	resp = *parsers.NormalizeV2(&resp)
	resp.RawData = string(row.ContentJSON)
	resp.Cached = domain.CacheDatabase
	if row.Source == "lyricsplus" {
		resp.Metadata.Source = "Lyrics+"
	}
	if resp.ProcessingTime == nil {
		resp.ProcessingTime = cacheProcessTiming(row.Source, time.Now().UnixMilli(), &resp.Metadata)
	}
	return &resp, true
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

func buildProcessTiming(res *orchestrator.Result, lastProcessed int64) *domain.ProcessTiming {
	statuses := make(map[string]domain.SourceStatus, len(res.SourcesStatus))
	for name, out := range res.SourcesStatus {
		ms := out.ElapsedMs
		statuses[name] = domain.SourceStatus{Status: out.Status, ElapsedMs: &ms}
	}
	winner := res.Source
	prio := res.Priority
	return &domain.ProcessTiming{
		LastProcessed:        lastProcessed,
		TotalElapsedMs:       res.Elapsed.Milliseconds(),
		WinnerSource:         &winner,
		SyncPriority:         &prio,
		SourcesStatus:        statuses,
		SelectedSongMetadata: pickedSongMeta(res.Resp),
	}
}

// cacheProcessTiming builds the processingTime for a database cache hit, where
// no race diagnostics exist; only the cached row's identity is known.
func cacheProcessTiming(rowSource string, lastProcessed int64, meta *domain.LyricsMetadata) *domain.ProcessTiming {
	winner := rowSource
	return &domain.ProcessTiming{
		LastProcessed:        lastProcessed,
		WinnerSource:         &winner,
		SelectedSongMetadata: pickedSongMeta(&domain.LyricsResponse{Metadata: *meta}),
	}
}

func pickedSongMeta(resp *domain.LyricsResponse) *domain.PickedSongMetadata {
	if resp == nil {
		return nil
	}
	met := &domain.PickedSongMetadata{
		Source: resp.Metadata.Source,
		Title:  resp.Metadata.Title,
		Artist: resp.Metadata.Artist,
		Album:  resp.Metadata.Album,
	}
	if met.Source == "" {
		met.Source = "Lyrics+"
	}
	return met
}

// providerNameForSource maps a metadata source label back to the racer's
// provider name, so cache hits can report a winner.
func providerNameForSource(source string) string {
	switch source {
	case "Spotify":
		return "spotify"
	case "Apple", "Apple Music":
		return "apple"
	case "QQ Music", "QQ":
		return "qq"
	case "Musixmatch":
		return "musixmatch"
	case "Deezer":
		return "deezer"
	default:
		return "lyricsplus"
	}
}

func extFor(source string) string {
	switch source {
	case "Apple Music", "Apple", "QQ Music", "QQ":
		return "xml"
	}
	return "json"
}
