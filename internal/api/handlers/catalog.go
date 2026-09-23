// Package handlers contains the HTTP handlers implementing the LyricsPlus API.
package handlers

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/providers"
)

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// Catalog serves /v1/songlist/search and /v1/metadata/get.
type Catalog struct {
	AppleMusic *providers.AppleMusicProvider
	Spotify    *providers.SpotifyProvider
	Musixmatch *providers.MusixmatchProvider
	Logger     *logger.Logger
}

// Search aggregates song catalog results from Apple, Spotify and Musixmatch.
func (h *Catalog) Search(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing required parameter: q (query)"})
		return
	}

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	var results []domain.SongCatalogItem
	agg := func(items []domain.SongCatalogItem, err error) {
		if err != nil {
			h.logf("catalog search %q failed: %v", q, err)
			return
		}
		results = append(results, items...)
	}

	type searchFn struct {
		name string
		call func() ([]domain.SongCatalogItem, error)
	}
	fns := []searchFn{
		{"apple", func() ([]domain.SongCatalogItem, error) { return h.AppleMusic.SearchCatalog(ctx, q) }},
		{"spotify", func() ([]domain.SongCatalogItem, error) { return h.Spotify.SearchCatalog(ctx, q) }},
		{"musixmatch", func() ([]domain.SongCatalogItem, error) { return h.Musixmatch.SearchCatalog(ctx, q) }},
	}
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func(name string, call func() ([]domain.SongCatalogItem, error)) {
			defer wg.Done()
			agg(call())
		}(fn.name, fn.call)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": mergeCatalogResults(results),
		"processingTime": map[string]int64{
			"timeElapsed":   time.Since(start).Milliseconds(),
			"lastProcessed": time.Now().UnixMilli(),
		},
	})
}

// mergeCatalogResults deduplicates catalog items the same way the JS
// songCatalog.service merges: sort by preferred source order (Apple, Spotify,
// Musixmatch), then key on ISRC (falling back to title/artist/album) and union
// the secondary fields into the first occurrence.
func mergeCatalogResults(results []domain.SongCatalogItem) []domain.SongCatalogItem {
	sourceOrder := map[string]int{"Apple Music": 1, "Spotify": 2, "Musixmatch": 3}
	rank := func(item domain.SongCatalogItem) int {
		if len(item.Availability) == 0 {
			return 99
		}
		if r, ok := sourceOrder[item.Availability[0]]; ok {
			return r
		}
		return 99
	}

	sorted := make([]domain.SongCatalogItem, len(results))
	copy(sorted, results)
	sort.SliceStable(sorted, func(i, j int) bool { return rank(sorted[i]) < rank(sorted[j]) })

	seen := make(map[string]int)
	merged := make([]domain.SongCatalogItem, 0, len(sorted))
	for _, song := range sorted {
		key := ""
		if song.ISRC != nil && *song.ISRC != "" {
			key = *song.ISRC
		} else {
			key = song.Title + "-" + song.Artist + "-" + song.Album
		}

		if idx, ok := seen[key]; ok {
			existing := &merged[idx]
			if existing.ID == nil {
				existing.ID = map[string]string{}
			}
			for k, v := range song.ID {
				existing.ID[k] = v
			}
			if existing.ExternalURLs == nil {
				existing.ExternalURLs = map[string]string{}
			}
			for k, v := range song.ExternalURLs {
				existing.ExternalURLs[k] = v
			}
			existing.Songwriters = unionStrings(existing.Songwriters, song.Songwriters)
			existing.Availability = unionStrings(existing.Availability, song.Availability)
			if existing.AlbumArtURL == nil && song.AlbumArtURL != nil {
				existing.AlbumArtURL = song.AlbumArtURL
			}
			if existing.DurationMs == 0 && song.DurationMs != 0 {
				existing.DurationMs = song.DurationMs
			}
		} else {
			seen[key] = len(merged)
			merged = append(merged, song)
		}
	}
	return merged
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	add := func(items []string) {
		for _, s := range items {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	add(a)
	add(b)
	return out
}

// Metadata returns detailed Apple Music metadata for a track.
func (h *Catalog) Metadata(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	artist := strings.TrimSpace(r.URL.Query().Get("artist"))
	if title == "" || artist == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing required parameters: title and artist"})
		return
	}
	album := strings.TrimSpace(r.URL.Query().Get("album"))
	var durationSec float64
	if ds := strings.TrimSpace(r.URL.Query().Get("duration")); ds != "" {
		durationSec, _ = strconv.ParseFloat(ds, 64)
	}

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	metadata, err := h.AppleMusic.GetMetadata(ctx, title, artist, album, durationSec)
	if err != nil {
		h.logf("metadata fetch failed for %q - %q: %v", artist, title, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal Server Error"})
		return
	}
	if metadata == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Could not find metadata"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"metadata": metadata})
}

func (h *Catalog) logf(format string, args ...interface{}) {
	if h.Logger != nil {
		h.Logger.Errorf(format, args...)
	}
}
