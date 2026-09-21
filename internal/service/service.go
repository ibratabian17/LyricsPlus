// Package service normalizes, caches, and serves canonical lyrics payloads.
package service

import (
	"context"
	"encoding/json"
	"strings"
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
	resp.ProcessingTime = buildProcessTiming(res, q, now.UnixMilli())
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
		resp.ProcessingTime = &domain.ProcessTiming{}
	}
	if resp.ProcessingTime.WinnerSource == nil {
		winner := row.Source
		if winner == "" {
			winner = providerNameForSource(resp.Metadata.Source)
		}
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

	return &domain.ProcessTiming{
		LastProcessed:        lastProcessed,
		TotalElapsedMs:       res.Elapsed.Milliseconds(),
		WinnerSource:         &winner,
		SyncPriority:         &prio,
		SourcesStatus:        statuses,
		SelectedSongMetadata: selMeta,
	}
}

func providerDisplayName(source string) string {
	switch strings.ToLower(source) {
	case "spotify":
		return "Spotify"
	case "apple", "apple music":
		return "Apple"
	case "qq", "qq music":
		return "QQ Music"
	case "musixmatch":
		return "Musixmatch"
	case "deezer":
		return "Deezer"
	default:
		return "Lyrics+"
	}
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
